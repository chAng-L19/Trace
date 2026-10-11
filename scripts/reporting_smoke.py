"""Offline F05/G02 regressions using real SQLite, CAS, EvidenceGraph and gates.

Run: python -B scripts/reporting_smoke.py
No model, network, gate stubs, or persistent test files are used.
"""
from __future__ import annotations

import json
import re
import sys
import tempfile
from copy import deepcopy
from dataclasses import replace
from html.parser import HTMLParser
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.asset_graph import project_asset_attack_graph
from redteam_agent.application.reporting import build_report, render_report_html, render_report_markdown
from redteam_agent.core import Asset, AttackPath, Finding, contract_hash
from redteam_agent.runtime.evidence_gate import EvidenceGate
from redteam_agent.runtime.model_state import EvidenceProvenance, TaskAttempt, ToolCallResult
from redteam_agent.runtime.operation_runtime import OperationRuntime


class HtmlDocument(HTMLParser):
    def __init__(self, source):
        super().__init__(convert_charrefs=True)
        self.tags, self.ids, self.links, self.text, self.attributes = [], set(), [], [], []
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        self.tags.append(tag)
        self.attributes.extend(attrs)
        attributes = dict(attrs)
        if "id" in attributes:
            assert attributes["id"] not in self.ids
            self.ids.add(attributes["id"])
        if "href" in attributes:
            self.links.append(attributes["href"])

    def handle_data(self, data):
        self.text.append(data)


def persist(runtime, state, identity, artifact_type, payload, *, parents=(), branch=None):
    attempt = TaskAttempt.create(
        run_id=state.run_id, branch_id=branch or state.branch_id, plan_revision=state.plan_revision,
        action_id=identity, tool="fixture:probe", tool_version="1",
        input_hash=contract_hash({"target": state.goal.targets[0], "action": identity}), idempotency_key=identity,
    )
    output = {"target": state.goal.targets[0], **payload}
    result = ToolCallResult("success", output, tool=attempt.tool, input_hash=attempt.input_hash,
                            output_hash=EvidenceGate.content_hash(output))
    runtime.store.create_task_attempt(replace(attempt, status="completed", result=result.to_dict()))
    provenance = EvidenceProvenance(
        run_id=state.run_id, branch_id=attempt.branch_id, plan_revision=state.plan_revision,
        action_id=identity, attempt_id=attempt.attempt_id, tool=attempt.tool, tool_version="1",
        input_hash=attempt.input_hash, output_hash=result.output_hash, target=output["target"], parent_ids=parents,
    )
    # add validates the real attempt/payload/parent contract and writes its file.
    # Defer rereading the entire persisted graph until this batch is complete.
    node = runtime.evidence_graph.add(
        run_id=state.run_id, action_id=identity, artifact_type=artifact_type, target=output["target"],
        tool=attempt.tool, payload=output, parent_ids=parents, verifier=artifact_type,
        confidence=0.8, provenance=provenance, trust="tool_verified", persist=False,
    )
    runtime.store.save_evidence(node)
    return node


def main():
    checks = []
    with tempfile.TemporaryDirectory(prefix="trace-reporting-") as temporary:
        root = Path(temporary)
        fixture = root / "fixture.txt"
        fixture.write_text("needle\n", encoding="utf-8")
        observed = fixture.read_text(encoding="utf-8")
        runtime = OperationRuntime(root=root / "runtime", register_builtins=False)
        first = runtime.start(session_id="reporting", objective="Inspect local fixture", targets=(str(fixture),), model_led=True)
        state = runtime.store.load_operation(first.run_id)
        run_id = state.run_id
        for index in range(1000):
            surface = persist(runtime, state, f"ordinary-{index}", "surface_map",
                              {"assets": [str(fixture)], "measurement": observed.count("needle"), "index": index})
        path = AttackPath("late-path", run_id, "Local measured path", asset_ids=("asset",),
                          finding_ids=("finding",), evidence_ids=(surface.evidence_id,), impact="Read the local fixture")
        path_source = persist(runtime, state, "path-source", "surface_map", {"attack_paths": [path.to_dict()]})
        with AgentService(runtime=runtime, load_external_configuration=False) as service:
            page = project_asset_attack_graph(service, run_id)
            assert len(page["attack_paths"]) == 1 and page["page"]["total"] == 1
            assert not page["truncated"] and page["next_offset"] is None
            assert len(runtime.evidence_graph.list(run_id)) == 1001
            checks.append("1001st_evidence_path_visible_on_default_page")

            asset = Asset("asset", run_id, "file", "Local fixture", target=str(fixture), evidence_ids=(surface.evidence_id,))
            asset_source = persist(runtime, state, "asset-source", "surface_map", asset.to_dict())
            controls = [{"source": "tool_output", "observation_path": "control.matches", "expected": 0,
                         "actual": observed.count("absent-control"), "passed": observed.count("absent-control") == 0}]
            repro = persist(runtime, state, "reproduction", "reproduction_artifact", {
                "reproducible": True, "tests": ["Read fixture.txt and count needle"], "body": observed,
                "side_effects": False, "control": {"matches": observed.count("absent-control")},
                "negative_controls": controls,
            }, parents=(surface.evidence_id,))
            impact = persist(runtime, state, "impact", "impact_proof",
                             {"verified": True, "impact": {"readable_bytes": len(observed.encode())}}, parents=(repro.evidence_id,))
            cleanup = persist(runtime, state, "cleanup", "cleanup_proof",
                              {"verified": True, "checks": ["Read-only test"], "outstanding_changes": []}, parents=(impact.evidence_id,))
            finding = Finding("finding", run_id, "Fixture read observation", "medium", status="verified",
                              description="The controlled probe read the expected fixture content.", target=str(fixture),
                              asset_ids=("asset",), reproduction_evidence_ids=(repro.evidence_id,),
                              impact_evidence_ids=(impact.evidence_id,), negative_control_evidence_ids=(repro.evidence_id,),
                              cleanup_evidence_ids=(cleanup.evidence_id,), metadata={"remediation": "Restrict file access by owner."})
            finding_source = persist(runtime, state, "finding-source", "surface_map", {"findings": [finding.to_dict()]})
            nodes = {node.evidence_id: node for node in runtime.evidence_graph.list(run_id)}
            gate_args = dict(run_id=run_id, branch_id=state.branch_id, target=str(fixture), max_plan_revision=state.plan_revision)
            assert EvidenceGate.validate_finding(finding, nodes, **gate_args).passed
            page = project_asset_attack_graph(service, run_id, limit=1)
            assert page["page"]["items"] == [{"collection": "attack_paths", "id": "late-path"}]
            assert page["page"]["total"] == 3 and page["next_offset"] == 1
            assert page["findings"][0]["finding_id"] == "finding" and page["assets"][0]["asset_id"] == "asset"
            assert {edge["kind"] for edge in page["edges"]} == {"asset_to_path", "finding_to_path", "evidence_to_path"}
            assert set(page["source_evidence_ids"]) == {path_source.evidence_id, asset_source.evidence_id, finding_source.evidence_id}
            second = project_asset_attack_graph(service, run_id, offset=1, limit=1)
            assert second["page"]["items"] == [{"collection": "findings", "id": "finding"}]
            assert second["assets"] and not second["attack_paths"] and second["next_offset"] == 2
            last = project_asset_attack_graph(service, run_id, offset=2, limit=1)
            assert last["page"]["count"] == 1 and not last["truncated"] and last["next_offset"] is None
            beyond = project_asset_attack_graph(service, run_id, offset=99, limit=1)
            assert not beyond["page"]["items"] and not beyond["edges"] and beyond["next_offset"] is None
            checks.append("entity_pages_include_cross_page_dependencies_and_sources")

            forged = replace(finding, finding_id="forged", reproduction_evidence_ids=(surface.evidence_id,))
            assert not EvidenceGate.validate_finding(forged, nodes, **gate_args).passed
            persist(runtime, state, "forged-source", "surface_map", {"findings": [forged.to_dict()]})
            persist(runtime, state, "model-assertion", "surface_map",
                    {"text": "CRITICAL: the model says this is confirmed", "findings": [{"title": "model only", "status": "verified"}]})
            persist(runtime, state, "other-branch", "surface_map", {"attack_paths": [replace(path, path_id="other").to_dict()]}, branch="other")
            bad_ref = replace(path, path_id="missing-ref", evidence_ids=("missing",))
            persist(runtime, state, "missing-ref", "surface_map", bad_ref.to_dict())
            checks.append("real_gate_rejects_wrong_roles_untyped_claims_and_out_of_scope_refs")

            artifact = runtime.artifacts.put_json({"body": observed, "steps": ["Read fixture.txt"]}, run_id=run_id)
            report = build_report(service, run_id)
            assert set(("run", "terminal", "evidence", "artifacts", "report_hash")) <= report.keys()
            assert report["run"]["run"]["run_id"] == run_id and report["goal"]["objective"] == "Inspect local fixture"
            assert [item["finding_id"] for item in report["findings"]] == ["finding"]
            assert [item["path_id"] for item in report["paths"]] == ["late-path"]
            assert report["paths"] == report["attack_paths"]
            assert report["findings"][0]["evidence_validation"]["passed"]
            assert report["findings"][0]["impact"] == [{"readable_bytes": len(observed.encode())}]
            assert next(node for node in report["evidence"]["nodes"] if node["evidence_id"] == repro.evidence_id)["payload"]["body"] == observed
            assert any(item["artifact_ref"] == artifact.artifact_id for item in report["artifacts"])
            assert contract_hash({key: value for key, value in report.items() if key != "report_hash"}) == report["report_hash"]
            markdown = render_report_markdown(report)
            for text in ("Inspect local fixture", "Fixture read observation", "medium", "Read fixture.txt", "needle",
                         "readable_bytes", "Restrict file access by owner.", "负对照", "完整正文", artifact.content_hash,
                         "文字描述、严重性和修复建议仍是记录声明", "CRITICAL: the model says this is confirmed"):
                assert text in markdown, text
            anchors = set(re.findall(r'<a id="([^"]+)"', markdown))
            assert set(re.findall(r'\]\(#([^\)]+)\)', markdown)) <= anchors
            assert "/api/" not in markdown
            # All summary sections are readable headings/fields, not JSON dumps.
            summary = markdown.split("## 证据正文与来源")[0]
            assert "```" not in summary and '"schema_version"' not in summary
            assert "### 1. Fixture read observation" in summary
            assert "Local fixture (asset)" in summary and "Fixture read observation (finding)" in summary
            assert "**目标达成**：\n\n否" in summary
            (root / "report.md").write_text(markdown, encoding="utf-8")
            assert (root / "report.md").read_text(encoding="utf-8") == markdown
            checks.append("standalone_report_payload_artifacts_reproduction_impact_remediation_and_links")

            html = render_report_html(report)
            document = HtmlDocument(html)
            assert {"html", "head", "body", "dl", "dt", "dd", "ol", "li", "pre"} <= set(document.tags)
            assert document.links and all(link.startswith("#") and link[1:] in document.ids for link in document.links)
            assert not {"script", "iframe", "img", "link"} & set(document.tags)
            assert not any(key == "src" or key.startswith("on") for key, _ in document.attributes)
            readable = "\n".join(document.text)
            for text in ("Inspect local fixture", "Fixture read observation", "Read fixture.txt", "readable_bytes",
                         "Restrict file access by owner.", "Local fixture (asset)", "Fixture read observation (finding)",
                         artifact.content_hash, repro.evidence_id, "成功条件", "缺失条件"):
                assert text in readable, text
            assert "<pre>" not in html.split("<h2>证据正文与来源</h2>")[0]
            (root / "report.html").write_text(html, encoding="utf-8")
            assert (root / "report.html").read_text(encoding="utf-8") == html
            hostile = deepcopy(report)
            marker = '<script>alert("fixture")</script><img src=x onerror="fixture"> & ```'
            hostile["goal"]["objective"] = marker
            hostile["findings"][0]["title"] = marker
            hostile["findings"][0]["remediation"] = marker
            hostile["paths"][0]["metadata"] = {marker: marker}
            hostile["assets"][0]["name"] = marker
            hostile["evidence"]["nodes"][0]["payload"] = marker
            hostile["artifacts"][0]["preview"] = marker
            escaped = HtmlDocument(render_report_html(hostile))
            assert not {"script", "img"} & set(escaped.tags)
            assert marker in "\n".join(escaped.text)
            assert all(link[1:] in escaped.ids for link in escaped.links)
            hostile_md = render_report_markdown(hostile)
            assert "<script>" not in hostile_md.split("## 证据正文与来源")[0]
            assert "````\n" + marker + "\n````" in hostile_md
            checks.append("readable_html_escaped_content_internal_anchors_and_no_external_dependencies")

            # Corrupt an actual evidence file: real integrity validation must remove
            # that proof, its descendants and the finding, not just hide its body.
            runtime.evidence_graph._artifact_path(repro).write_text("{}", encoding="utf-8")
            damaged = build_report(service, run_id)
            assert not damaged["findings"]
            assert repro.evidence_id not in {node["evidence_id"] for node in damaged["evidence"]["nodes"]}
            checks.append("corrupt_proof_removes_finding_through_real_integrity_gate")

    with tempfile.TemporaryDirectory(prefix="trace-reporting-empty-") as temporary:
        with AgentService(root=Path(temporary), load_external_configuration=False) as service:
            run_id = service.start({"session_id": "empty", "objective": "Empty fixture", "targets": [temporary]}).single.run.run_id
            empty = build_report(service, run_id)
            assert not empty["findings"] and not empty["paths"] and not empty["evidence"]["nodes"]
            assert not empty["terminal"]["success"]
            assert "没有 eligible evidence" in render_report_markdown(empty)
            assert "没有 eligible evidence" in render_report_html(empty)
            state = service.runtime.store.load_operation(run_id)
            lease = service.runtime.store.acquire_lease(run_id, "__operation__", "report-smoke", ttl_seconds=30)
            try:
                state.status = "failed"
                state.failure_reason = "fixture_execution_failed"
                state.terminal_reason = "fixture_execution_failed"
                service.runtime.store.save_operation(state, expected_version=state.state_version, lease_token=lease)
            finally:
                service.runtime.store.release_lease(lease)
            failed = build_report(service, run_id)
            assert failed["terminal"]["terminal"] and not failed["terminal"]["success"]
            assert "fixture\\_execution\\_failed" in render_report_markdown(failed)
            failed_html = HtmlDocument(render_report_html(failed))
            assert "fixture_execution_failed" in "\n".join(failed_html.text)
            assert not failed["findings"] and not failed["artifacts"]
            checks.append("empty_and_persisted_failed_run_report_without_false_success")

    print(json.dumps({"checks": checks, "passed": len(checks)}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
