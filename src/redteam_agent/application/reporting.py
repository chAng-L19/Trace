"""Read-only delivery reports over the existing typed evidence contracts."""
from __future__ import annotations

import json
import re
from collections.abc import Mapping
from html import escape
from typing import Any

from ..core import contract_hash
from .asset_graph import _eligible_evidence, _project_typed_graph


def build_report(service: Any, run_id: str) -> dict[str, Any]:
    """Return a complete JSON view, retaining the five original Web report keys.

    Findings pass EvidenceGate.validate_finding via the shared graph extractor.
    Narrative metadata remains declared text, not additional verification.
    Evidence includes full eligible payloads; artifacts retain the existing
    public projection (references/previews, not a binary export bundle).
    """
    view = service.status(run_id)
    nodes = _eligible_evidence(service, run_id)
    graph = _project_typed_graph(run_id, nodes)
    by_id = {node.evidence_id: node for node in nodes}
    findings = []
    for finding in graph["findings"]:
        metadata = finding["metadata"]
        findings.append({
            **finding,
            "evidence_validation": {"passed": True, "reason": "finding_evidence_valid"},
            "reproduction_steps": metadata.get("reproduction_steps", [
                by_id[ref].payload.get("tests", []) for ref in finding["reproduction_evidence_ids"]
            ]),
            "impact": metadata.get("impact", [
                by_id[ref].payload.get("impact", {}) for ref in finding["impact_evidence_ids"]
            ]),
            "remediation": metadata.get("remediation", "未记录修复建议"),
            "narrative_trust": "declared; consult referenced evidence",
        })
    report = {
        "schema_version": 1,
        "run_id": run_id,
        # The old Web report nests AgentRunView here, rather than just Run.
        "run": view.to_dict(),
        "goal": view.goal.to_dict(),
        "terminal": view.terminal.to_dict(),
        "assets": graph["assets"],
        "findings": findings,
        "paths": graph["attack_paths"],
        "attack_paths": graph["attack_paths"],
        "edges": graph["edges"],
        "source_evidence_ids": graph["source_evidence_ids"],
        "entity_sources": graph["entity_sources"],
        "evidence": {
            "nodes": [node.to_dict() for node in nodes],
            "edges": [{"from": parent, "to": node.evidence_id, "kind": "parent"}
                      for node in nodes for parent in node.parent_ids],
            "truncated": False, "next_offset": None,
        },
        "artifacts": [service.runtime.artifacts.project(ref) for ref in service.artifacts(run_id)],
        "notes": [
            "发现的证据角色、执行来源、作用域及负对照已通过 EvidenceGate；文字描述、严重性和修复建议仍是记录声明。",
            "路径为显式 typed 关联记录，不代表整条攻击链已经独立验证；普通输出及模型断言不自动成为发现。",
            "证据正文仅包含当前分支和计划版本内的 eligible evidence；没有已验证发现不代表目标不存在漏洞。",
            "产物列表含引用、哈希和预览；二进制原件未嵌入本报告。",
        ],
    }
    report["report_hash"] = contract_hash(report)
    return report


def _block(value: Any) -> str:
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, indent=2)
    fence = "`" * max(3, 1 + max((len(match) for match in re.findall(r"`+", text)), default=0))
    return f"{fence}\n{text}\n{fence}"


def _anchor(identity: str) -> str:
    return "evidence-" + contract_hash(identity)[:24]


def _report_content(report: Mapping[str, Any]):
    """Share editorial content between Markdown and HTML, not their markup."""
    goal, terminal = report["goal"], report["terminal"]
    yield "heading", 1, "Trace 安全研究报告"
    yield "field", "运行 ID", report["run_id"]
    yield "heading", 2, "目标与终态"
    for label, value in (
        ("研究目标", goal["objective"]), ("目标范围", goal["targets"]),
        ("运行状态", report["run"]["run"]["status"]), ("是否终态", terminal["terminal"]),
        ("目标达成", terminal["success"]), ("裁决原因", terminal["reason"]),
        ("已满足条件", terminal["satisfied"]), ("缺失条件", terminal["missing"]),
        ("成功条件", [{"条件": criterion["statement"], "目标": criterion["target"],
                       "所需证据": criterion["required_artifacts"]} for criterion in goal["criteria"]]),
        ("成功判据", goal["success_predicates"]), ("任务约束", goal["constraints"]),
    ):
        yield "field", label, value
    yield "references", "裁决证据", terminal["evidence_refs"]
    yield "text", "", (f"本报告含 {len(report['findings'])} 个通过门禁的发现、"
                         f"{len(report['paths'])} 条路径、{len(report['evidence']['nodes'])} 条合格证据。")
    yield "heading", 2, "证据与声明边界"
    for note in report["notes"]:
        yield "text", "", note
    yield "heading", 2, "发现"
    if not report["findings"]:
        yield "text", "", "本次运行没有通过证据门禁的 typed 发现。请结合终态及失败原因阅读；不能据此宣称测试成功或目标安全。"
    for index, finding in enumerate(report["findings"], 1):
        yield "heading", 3, f"{index}. {finding['title']}"
        for label, key in (("发现 ID", "finding_id"), ("严重性（声明）", "severity"),
                           ("记录状态", "status"), ("目标", "target"), ("概述", "description"),
                           ("复现步骤（对照证据核验）", "reproduction_steps"),
                           ("影响（对照证据核验）", "impact"), ("修复建议（非已验证修复）", "remediation")):
            yield "field", label, finding[key]
        yield "field", "证据门禁", "通过：finding_evidence_valid"
        for key, label in (("reproduction_evidence_ids", "复现证据"), ("impact_evidence_ids", "影响证据"),
                           ("negative_control_evidence_ids", "负对照 / 反证核验"), ("cleanup_evidence_ids", "清理证据")):
            yield "references", label, finding[key]
        yield "references", "发现记录来源", report["entity_sources"]["findings"][finding["finding_id"]]
        if finding["metadata"]:
            yield "field", "补充说明（声明）", finding["metadata"]

    asset_names = {item["asset_id"]: item["name"] for item in report["assets"]}
    finding_names = {item["finding_id"]: item["title"] for item in report["findings"]}
    yield "heading", 2, "攻击路径"
    if not report["paths"]:
        yield "text", "", "未记录符合契约的攻击路径。"
    for index, path in enumerate(report["paths"], 1):
        yield "heading", 3, f"{index}. {path['title']}"
        yield "field", "路径 ID", path["path_id"]
        yield "field", "记录状态（非独立攻击链验证）", path["status"]
        yield "field", "涉及资产", [f"{asset_names.get(identity, '未纳入合格资产')} ({identity})" for identity in path["asset_ids"]]
        yield "field", "关联发现", [f"{finding_names.get(identity, '未纳入已验证发现')} ({identity})" for identity in path["finding_ids"]]
        yield "field", "影响（声明）", path["impact"]
        if path["relationship_ids"]:
            yield "field", "关系记录 ID", path["relationship_ids"]
        if path["metadata"]:
            yield "field", "路径说明（声明）", path["metadata"]
        yield "references", "支持证据", path["evidence_ids"]
        yield "references", "路径记录来源", report["entity_sources"]["attack_paths"][path["path_id"]]
    yield "heading", 2, "资产"
    for asset in report["assets"]:
        yield "heading", 3, asset["name"]
        for label, key in (("资产 ID", "asset_id"), ("类型", "asset_type"), ("目标", "target"), ("记录状态", "status")):
            yield "field", label, asset[key]
        if asset["attributes"]:
            yield "field", "资产属性", asset["attributes"]
        yield "references", "资产证据", asset["evidence_ids"]
        yield "references", "资产记录来源", report["entity_sources"]["assets"][asset["asset_id"]]
    if not report["assets"]:
        yield "text", "", "未记录符合契约的资产。"

    yield "heading", 2, "证据正文与来源"
    for index, node in enumerate(report["evidence"]["nodes"], 1):
        yield "anchor", "", node["evidence_id"]
        yield "heading", 3, f"证据 {index}：{node['artifact_type']}"
        for label, key in (("证据 ID", "evidence_id"), ("目标", "target"), ("工具", "tool"),
                           ("信任级别", "trust"), ("已验证", "verified"), ("内容哈希", "content_hash"),
                           ("执行来源", "provenance")):
            yield "field", label, node[key]
        yield "payload", "完整正文", node["payload"]
        yield "references", "父证据", node["parent_ids"]
    if not report["evidence"]["nodes"]:
        yield "text", "", "没有 eligible evidence。"
    yield "heading", 2, "产物"
    for index, artifact in enumerate(report["artifacts"], 1):
        yield "heading", 3, f"产物 {index}：{artifact['artifact_type']}"
        for label, key in (("产物引用", "artifact_ref"), ("内容哈希", "content_hash"),
                           ("字节数", "byte_count"), ("媒体类型", "media_type")):
            yield "field", label, artifact[key]
        if artifact["metadata"]:
            yield "field", "产物说明", artifact["metadata"]
        yield "payload", "预览（原件未嵌入）", artifact["preview"]
    if not report["artifacts"]:
        yield "text", "", "未记录产物。"
    yield "field", "报告摘要哈希", report["report_hash"]


def _plain(value: Any) -> str:
    if isinstance(value, bool):
        return "是" if value else "否"
    return "未记录" if value is None or value == "" else str(value)


def _md_text(value: Any) -> str:
    return re.sub(r"([\\`*_\[\]<>#])", r"\\\1", _plain(value))


def _md_value(value: Any) -> str:
    if isinstance(value, Mapping):
        return "\n".join(f"- **{_md_text(key)}**：\n  " + _md_value(item).replace("\n", "\n  ")
                         for key, item in value.items()) or "未记录"
    if isinstance(value, (list, tuple)):
        return "\n".join(f"{index}. " + _md_value(item).replace("\n", "\n   ")
                         for index, item in enumerate(value, 1)) or "未记录"
    return _md_text(value)


def render_report_markdown(report: Mapping[str, Any]) -> str:
    """Return standalone Markdown with readable fields and internal evidence links."""
    known = {node["evidence_id"] for node in report["evidence"]["nodes"]}
    lines = []
    for kind, label, value in _report_content(report):
        if kind == "heading":
            lines.append(f"{'#' * label} {_md_text(value).replace(chr(10), ' ')}")
        elif kind == "anchor":
            lines.append(f'<a id="{_anchor(value)}"></a>')
        elif kind == "references":
            links = [f"[{_md_text(ref)}](#{_anchor(ref)})" if ref in known else
                     f"{_md_text(ref)}（未纳入当前合格证据）" for ref in value]
            lines.append(f"**{label}**：" + (", ".join(links) or "未记录"))
        elif kind == "payload":
            lines.extend([f"**{label}**", _block(value)])
        elif kind == "field":
            lines.append(f"**{label}**：\n\n{_md_value(value)}")
        else:
            lines.append(_md_text(value))
    return "\n\n".join(lines) + "\n"


def _html_value(value: Any) -> str:
    if isinstance(value, Mapping):
        return "<dl>" + "".join(f"<dt>{escape(str(key))}</dt><dd>{_html_value(item)}</dd>"
                                 for key, item in value.items()) + "</dl>" if value else "未记录"
    if isinstance(value, (list, tuple)):
        return "<ol>" + "".join(f"<li>{_html_value(item)}</li>" for item in value) + "</ol>" if value else "未记录"
    return escape(_plain(value)).replace("\n", "<br>")


def render_report_html(report: Mapping[str, Any]) -> str:
    """Return a standalone escaped HTML document; no scripts or remote assets."""
    known = {node["evidence_id"] for node in report["evidence"]["nodes"]}
    lines = ['<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">',
             '<meta name="viewport" content="width=device-width, initial-scale=1">',
             '<title>Trace 安全研究报告</title>',
             '<style>body{max-width:960px;margin:40px auto;padding:0 24px;font:16px/1.65 system-ui,sans-serif;color:#222}'
             'h2{border-bottom:1px solid #ccc;padding-bottom:8px;margin-top:40px}dt{font-weight:600}'
             'dd{margin:0 0 12px 20px}pre{white-space:pre-wrap;overflow-wrap:anywhere;padding:16px;background:#f5f5f5}'
             'a,dd{overflow-wrap:anywhere}li{margin:4px 0}</style></head><body><main>']
    for kind, label, value in _report_content(report):
        if kind == "heading":
            lines.append(f"<h{label}>{escape(str(value))}</h{label}>")
        elif kind == "anchor":
            lines.append(f'<a id="{_anchor(value)}"></a>')
        elif kind == "references":
            links = [f'<a href="#{_anchor(ref)}">{escape(ref)}</a>' if ref in known else
                     f"{escape(ref)}（未纳入当前合格证据）" for ref in value]
            lines.append(f"<dl><dt>{escape(label)}</dt><dd>{', '.join(links) or '未记录'}</dd></dl>")
        elif kind == "payload":
            text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, indent=2)
            lines.append(f"<p><strong>{escape(label)}</strong></p><pre>{escape(text)}</pre>")
        elif kind == "field":
            lines.append(f"<dl><dt>{escape(label)}</dt><dd>{_html_value(value)}</dd></dl>")
        else:
            lines.append(f"<p>{escape(str(value))}</p>")
    return "\n".join([*lines, "</main></body></html>"]) + "\n"
