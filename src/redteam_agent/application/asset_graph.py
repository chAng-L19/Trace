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
    """Page typed entities, including their referenced assets/findings as context.

    ``page.items`` identifies the primary entities counted against limit; context
    objects may repeat across pages and do not consume the limit. Offsets refer
    to entities (paths, findings, assets), never raw evidence.
    """

    service.status(run_id)
    bounded = max(1, min(10000, int(limit)))
    start = max(0, int(offset))
    graph = _project_typed_graph(run_id, _eligible_evidence(service, run_id))
    collections = (("attack_paths", "path_id"), ("findings", "finding_id"), ("assets", "asset_id"))
    entities = [(key, item[id_key]) for key, id_key in collections for item in graph[key]]
    selected = entities[start : start + bounded]
    included = set(selected)
    for path in graph["attack_paths"]:
        if ("attack_paths", path["path_id"]) in included:
            included.update(("findings", identity) for identity in path["finding_ids"])
            included.update(("assets", identity) for identity in path["asset_ids"])
    for finding in graph["findings"]:
        if ("findings", finding["finding_id"]) in included:
            included.update(("assets", identity) for identity in finding["asset_ids"])
    for key, id_key in collections:
        graph[key] = [item for item in graph[key] if (key, item[id_key]) in included]
    graph["materialized"] = any(graph[key] for key, _ in collections)
    path_ids = {item["path_id"] for item in graph["attack_paths"]}
    graph["edges"] = [edge for edge in graph["edges"] if edge["to"] in path_ids]
    graph["entity_sources"] = {
        key: {identity: refs for identity, refs in sources.items() if (key, identity) in included}
        for key, sources in graph["entity_sources"].items()
    }
    graph["source_evidence_ids"] = list(dict.fromkeys(
        ref for sources in graph["entity_sources"].values() for refs in sources.values() for ref in refs
    ))
    more = start + len(selected) < len(entities)
    graph.update(truncated=more, next_offset=start + len(selected) if more else None)
    graph["page"] = {
        "offset": start, "limit": bounded, "total": len(entities), "count": len(selected),
        "has_more": more, "next_offset": graph["next_offset"],
        "items": [{"collection": key, "id": identity} for key, identity in selected],
    }
    return graph


def _eligible_evidence(service: Any, run_id: str) -> tuple[Any, ...]:
    state = service.runtime.store.load_operation(run_id)
    return EvidenceGate.eligible_evidence(
        service.runtime.evidence_graph.list(run_id), run_id=run_id,
        branch_id=state.branch_id, max_plan_revision=state.plan_revision,
    )


def _project_typed_graph(run_id: str, nodes: Sequence[Any]) -> dict[str, Any]:
    """Extract once from the complete eligible set; shared by pages and reports."""
    trusted = {node.evidence_id: node for node in nodes}
    assets: dict[str, Asset] = {}
    findings: dict[str, Finding] = {}
    paths: dict[str, AttackPath] = {}
    source_ids: list[str] = []
    entity_sources: dict[str, dict[str, list[str]]] = {"assets": {}, "findings": {}, "attack_paths": {}}

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
            entity_sources["assets"][value.asset_id] = [node.evidence_id]
            typed = True
        for value in typed_items(node, "findings", Finding):
            findings[value.finding_id] = value
            entity_sources["findings"][value.finding_id] = [node.evidence_id]
            typed = True
        for value in typed_items(node, "attack_paths", AttackPath):
            paths[value.path_id] = value
            entity_sources["attack_paths"][value.path_id] = [node.evidence_id]
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
    return {
        "schema_version": 1,
        "run_id": run_id,
        "materialized": bool(assets or findings or paths),
        "assets": [item.to_dict() for item in assets.values()],
        "findings": [item.to_dict() for item in findings.values()],
        "attack_paths": [item.to_dict() for item in paths.values()],
        "edges": edges,
        "source_evidence_ids": list(dict.fromkeys(source_ids)),
        "entity_sources": entity_sources,
        "truncated": False,
        "next_offset": None,
        "note": "仅展示带 Core contract version 的显式资产、发现和攻击路径；普通工具输出不会被推断为攻击路径。",
    }
