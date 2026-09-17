from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any

from ..core import Asset, AttackPath, Finding
from ..runtime.evidence_gate import EvidenceGate


def project_asset_attack_graph(
    service: Any,
    run_id: str,
    *,
    limit: int = 1000,
    offset: int = 0,
) -> dict[str, Any]:
    """Project typed graph objects from the trusted evidence view only."""

    service.status(run_id)
    bounded = max(1, min(10000, int(limit)))
    start = max(0, int(offset))
    all_nodes = service.runtime.evidence_graph.list(run_id)
    nodes = all_nodes[start : start + bounded]
    trusted = {node.evidence_id: node for node in all_nodes}
    assets: dict[str, Asset] = {}
    findings: dict[str, Finding] = {}
    paths: dict[str, AttackPath] = {}
    source_ids: list[str] = []

    def typed_items(source: Any, key: str, factory: Any) -> list[Any]:
        payload = source.payload
        if isinstance(payload, Mapping) and payload.get("kind") == factory.KIND:
            candidates = [payload]
        elif isinstance(payload, Mapping):
            raw = payload.get(key, ())
            candidates = list(raw) if isinstance(raw, Sequence) and not isinstance(raw, (str, bytes)) else []
        else:
            candidates = []
        result = []
        for item in candidates:
            if not isinstance(item, Mapping):
                continue
            try:
                value = factory.from_dict(item)
            except (TypeError, ValueError):
                continue
            if factory is Asset:
                references = value.evidence_ids
            elif factory is Finding:
                references = tuple(
                    ref
                    for field in (
                        "reproduction_evidence_ids",
                        "impact_evidence_ids",
                        "negative_control_evidence_ids",
                        "cleanup_evidence_ids",
                    )
                    for ref in getattr(value, field)
                )
            else:
                references = value.evidence_ids
            if value.run_id != run_id or not references or not source.provenance:
                continue
            target = str(getattr(value, "target", "") or source.target)
            if target != source.target or not all(
                reference in trusted and EvidenceGate.same_scope(
                    trusted[reference], run_id=run_id, branch_id=source.provenance.branch_id,
                    target=target, max_plan_revision=source.provenance.plan_revision,
                )
                for reference in references
            ):
                continue
            if factory is Finding and not EvidenceGate.validate_finding(
                value, trusted, run_id=run_id, branch_id=source.provenance.branch_id,
                target=target, max_plan_revision=source.provenance.plan_revision,
            ).passed:
                continue
            result.append(value)
        return result

    for node in nodes:
        typed = False
        for value in typed_items(node, "assets", Asset):
            assets[value.asset_id] = value
            typed = True
        for value in typed_items(node, "findings", Finding):
            findings[value.finding_id] = value
            typed = True
        for value in typed_items(node, "attack_paths", AttackPath):
            paths[value.path_id] = value
            typed = True
        if typed:
            source_ids.append(node.evidence_id)

    edges: list[dict[str, str]] = []
    for path in paths.values():
        edges.extend(
            {"from": asset_id, "to": path.path_id, "kind": "asset_to_path"}
            for asset_id in path.asset_ids
            if asset_id in assets
        )
        edges.extend(
            {"from": finding_id, "to": path.path_id, "kind": "finding_to_path"}
            for finding_id in path.finding_ids
            if finding_id in findings
        )
        edges.extend(
            {"from": evidence_id, "to": path.path_id, "kind": "evidence_to_path"}
            for evidence_id in path.evidence_ids
        )
    truncated = start + bounded < len(all_nodes)
    return {
        "schema_version": 1,
        "run_id": run_id,
        "materialized": bool(assets or findings or paths),
        "assets": [item.to_dict() for item in assets.values()],
        "findings": [item.to_dict() for item in findings.values()],
        "attack_paths": [item.to_dict() for item in paths.values()],
        "edges": edges,
        "source_evidence_ids": list(dict.fromkeys(source_ids)),
        "truncated": truncated,
        "next_offset": start + len(nodes) if truncated else None,
        "note": "仅展示带 Core contract version 的显式资产、发现和攻击路径；普通工具输出不会被推断为攻击路径。",
    }
