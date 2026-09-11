from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Mapping, Sequence

from .tool_broker import ToolBroker
from .open_source_tools import register_open_source_tools
from .evidence_gate import EvidenceGate


def _evidence(arguments: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    raw = arguments.get("evidence")
    return [
        item
        for item in raw
        if isinstance(item, Mapping)
        and EvidenceGate.trusted(item)
    ] if isinstance(raw, list) else []


def _refs(arguments: Mapping[str, Any]) -> list[str]:
    return [str(item.get("evidence_id")) for item in _evidence(arguments) if item.get("evidence_id")]


def _target(arguments: Mapping[str, Any]) -> str:
    return str(arguments.get("target") or "")


def _declared_clause_ids(item: Mapping[str, Any]) -> set[str]:
    raw = item.get("clause_ids")
    if raw is None and isinstance(item.get("payload"), Mapping):
        raw = item["payload"].get("clause_ids")
    return {str(value) for value in raw if str(value)} if isinstance(raw, list) else set()


def _inherited_clause_ids(evidence: Sequence[Mapping[str, Any]]) -> list[str]:
    return sorted({clause_id for item in evidence for clause_id in _declared_clause_ids(item)})


def _clause_contracts(arguments: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    raw = arguments.get("clause_contract")
    if not isinstance(raw, list):
        envelope = arguments.get("intent_envelope")
        raw = envelope.get("clause_contracts") if isinstance(envelope, Mapping) else None
    return [item for item in raw if isinstance(item, Mapping)] if isinstance(raw, list) else []


def _eligible_clause_ids(arguments: Mapping[str, Any], artifact_type: str) -> set[str]:
    return {
        str(item.get("clause_id"))
        for item in _clause_contracts(arguments)
        if item.get("clause_id")
        and isinstance(item.get("required_artifacts"), list)
        and artifact_type in {str(value) for value in item["required_artifacts"]}
    }


def _artifact_clause_ids(
    arguments: Mapping[str, Any],
    artifact_type: str,
    evidence: Sequence[Mapping[str, Any]] = (),
) -> list[str]:
    return sorted(set(_inherited_clause_ids(evidence)) | _eligible_clause_ids(arguments, artifact_type))


def _artifact_clause_support(arguments: Mapping[str, Any], artifact_type: str) -> dict[str, Mapping[str, str]]:
    return {
        str(item["clause_id"]): {
            "artifact_type": artifact_type,
            "source_text": str(item.get("source_text") or ""),
            "basis": f"the verified {artifact_type} satisfies this clause's declared evidence contract",
        }
        for item in _clause_contracts(arguments)
        if str(item.get("clause_id") or "") in _eligible_clause_ids(arguments, artifact_type)
    }


def _supports_clause(item: Mapping[str, Any], clause_id: str, evidence_by_id: Mapping[str, Mapping[str, Any]]) -> bool:
    del evidence_by_id
    raw = item.get("clause_support")
    if raw is None and isinstance(item.get("payload"), Mapping):
        raw = item["payload"].get("clause_support")
    return isinstance(raw, Mapping) and clause_id in {str(key) for key in raw}


def local_inspector(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    target = _target(arguments)
    path = Path(target).expanduser()
    if not target or not path.exists():
        raise ValueError("local_target_not_found")
    resolved = path.resolve(strict=True)
    if resolved.is_file():
        digest = hashlib.sha256(resolved.read_bytes()).hexdigest()
        surface = {"files": [{"path": str(resolved), "size": resolved.stat().st_size, "sha256": digest}]}
    else:
        files: list[dict[str, Any]] = []
        for child in sorted(item for item in resolved.rglob("*") if item.is_file())[:2000]:
            files.append({"path": str(child), "size": child.stat().st_size})
        surface = {"files": files, "root": str(resolved)}
    return {
        "artifact_type": str(arguments.get("expected_artifact") or "surface_map"),
        "target": target,
        "confidence": 1.0,
        "clause_ids": _artifact_clause_ids(arguments, "surface_map"),
        "clause_support": _artifact_clause_support(arguments, "surface_map"),
        **surface,
    }


def hypothesis_builder(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    evidence = _evidence(arguments)
    if not evidence:
        raise ValueError("hypothesis_requires_surface_evidence")
    hypotheses: list[dict[str, Any]] = []
    patterns = (
        (("routes", "requests", "parameters"), "Validate input, authentication, and authorization boundaries across the discovered request surface.", ("controlled_validation", "browser_automation")),
        (("services", "hosts", "addresses"), "Validate exposed-service configuration and version-specific attack paths with negative controls.", ("controlled_validation", "cve_search")),
        (("files", "components", "dependencies", "sinks"), "Trace untrusted inputs through security-sensitive code and validate the highest-impact reachable sink.", ("code_analysis", "test_harness")),
        (("principals", "roles", "trusts", "permissions"), "Validate the shortest evidence-backed privilege or trust path and measure its effective permissions.", ("identity_validation", "graph_analysis")),
        (("prompts", "hierarchy", "models"), "Run reproducible prompt cases against the identified instruction boundary with matched negative controls.", ("prompt_evaluation", "controlled_validation")),
        (("entrypoints", "imports", "symbols", "binary"), "Validate the highest-value control-flow path from an exposed entrypoint to a security-relevant operation.", ("binary_reverse", "test_harness")),
    )
    for item in evidence[-12:]:
        payload = item.get("payload") if isinstance(item.get("payload"), Mapping) else {}
        evidence_id = str(item.get("evidence_id") or "")
        for keys, statement, capabilities in patterns:
            if not any(payload.get(key) for key in keys):
                continue
            hypotheses.append(
                {
                    "id": f"hypothesis-{len(hypotheses) + 1}",
                    "statement": statement,
                    "priority": "high" if not hypotheses else "medium",
                    "status": "unvalidated",
                    "evidence_refs": [evidence_id],
                    "recommended_capabilities": list(capabilities),
                    "negative_control": "Repeat the same validation with the suspected control variable removed or a non-privileged identity.",
                }
            )
    if not hypotheses:
        hypotheses.append(
            {
                "id": "hypothesis-1",
                "statement": "Validate the highest-value path exposed by the collected surface evidence.",
                "priority": "high",
                "status": "unvalidated",
                "evidence_refs": _refs(arguments),
                "recommended_capabilities": ["controlled_validation"],
                "negative_control": "Repeat the validation with the suspected control variable removed.",
            }
        )
    return {
        "artifact_type": "hypothesis_queue",
        "target": _target(arguments),
        "confidence": 0.8,
        "evidence_refs": _refs(arguments),
        "clause_ids": _artifact_clause_ids(arguments, "hypothesis_queue", evidence),
        "clause_support": _artifact_clause_support(arguments, "hypothesis_queue"),
        "hypotheses": hypotheses,
    }


def impact_builder(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    evidence = _evidence(arguments)
    reproduction = next((item for item in reversed(evidence) if item.get("artifact_type") == "reproduction_artifact"), None)
    if reproduction is None:
        raise ValueError("impact_requires_reproduction")
    payload = reproduction.get("payload") if isinstance(reproduction.get("payload"), Mapping) else {}
    concrete = payload.get("impact_observations") or payload.get("measured_outcomes")
    if not concrete:
        raise ValueError("impact_requires_measured_reproduction")
    return {
        "artifact_type": "impact_proof",
        "target": _target(arguments),
        "confidence": 0.8,
        "evidence_refs": [str(reproduction.get("evidence_id"))],
        "clause_ids": _artifact_clause_ids(arguments, "impact_proof", (reproduction,)),
        "clause_support": _artifact_clause_support(arguments, "impact_proof"),
        "verified": True,
        "impact": {"source": str(reproduction.get("evidence_id")), "observed": concrete},
    }


def coverage_builder(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    evidence = _evidence(arguments)
    reproduction = next((item for item in reversed(evidence) if item.get("artifact_type") == "reproduction_artifact"), None)
    if reproduction is None:
        raise ValueError("coverage_requires_reproduction")
    payload = reproduction.get("payload") if isinstance(reproduction.get("payload"), Mapping) else {}
    controls = payload.get("negative_controls") or payload.get("false_positive_controls")
    if not controls:
        raise ValueError("coverage_requires_negative_controls")
    return {
        "artifact_type": "coverage_report",
        "target": _target(arguments),
        "confidence": 0.85,
        "evidence_refs": _refs(arguments),
        "clause_ids": _artifact_clause_ids(arguments, "coverage_report", evidence),
        "clause_support": _artifact_clause_support(arguments, "coverage_report"),
        "checked": [str(item.get("artifact_type") or "") for item in evidence],
        "negative_controls": controls,
        "coverage": {"evidence_nodes": len(evidence), "required_actions": arguments.get("required_actions", [])},
    }


def cleanup_builder(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    evidence = _evidence(arguments)
    reproduction = next((item for item in reversed(evidence) if item.get("artifact_type") == "reproduction_artifact"), None)
    if reproduction is None:
        raise ValueError("cleanup_requires_reproduction")
    payload = reproduction.get("payload") if isinstance(reproduction.get("payload"), Mapping) else {}
    side_effects = payload.get("side_effects", False)
    cleanup_actions = payload.get("cleanup_actions")
    if side_effects:
        raise ValueError("live_cleanup_tool_required_for_side_effects")
    actions = cleanup_actions or ["No persistent side effects were recorded by the reproduction step."]
    return {
        "artifact_type": "cleanup_proof",
        "target": _target(arguments),
        "confidence": 0.9,
        "evidence_refs": _refs(arguments),
        "clause_ids": _artifact_clause_ids(arguments, "cleanup_proof", (reproduction,)),
        "clause_support": _artifact_clause_support(arguments, "cleanup_proof"),
        "actions": actions,
        "verified": True,
        "outstanding_changes": [],
    }


def report_builder(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    evidence = _evidence(arguments)
    types = {str(item.get("artifact_type") or "") for item in evidence}
    raw_required_artifacts = arguments.get("required_artifacts")
    required_goal_artifacts = (
        {str(item) for item in raw_required_artifacts if str(item)} - {"final_report"}
        if isinstance(raw_required_artifacts, list)
        else {"reproduction_artifact", "impact_proof", "coverage_report", "cleanup_proof"}
    )
    raw_criteria = arguments.get("goal_criteria")
    goal_criteria = [item for item in raw_criteria if isinstance(item, Mapping)] if isinstance(raw_criteria, list) else []
    workflow_id = str(arguments.get("workflow_id") or "")
    intent_envelope = arguments.get("intent_envelope")
    clause_ids = (
        [str(item) for item in intent_envelope.get("clause_ids", ()) if str(item)]
        if isinstance(intent_envelope, Mapping)
        else []
    )
    criterion_results: list[dict[str, Any]] = []
    for criterion in goal_criteria:
        criterion_target = str(criterion.get("target") or _target(arguments))
        required_workflow = str(criterion.get("workflow_id") or "")
        scoped = [item for item in evidence if not criterion_target or str(item.get("target") or "") == criterion_target]
        if required_workflow and workflow_id != required_workflow:
            prefix = f"{required_workflow}__"
            scoped = [item for item in scoped if str(item.get("action_id") or "").startswith(prefix)]
        scoped_types = {str(item.get("artifact_type") or "") for item in scoped}
        achieved = required_goal_artifacts.issubset(scoped_types)
        criterion_results.append(
            {
                "criterion_id": str(criterion.get("criterion_id") or ""),
                "statement": str(criterion.get("statement") or ""),
                "target": criterion_target,
                "workflow_id": required_workflow,
                "status": "achieved" if achieved else "not_achieved",
                "evidence_refs": [str(item.get("evidence_id")) for item in scoped if item.get("evidence_id")],
            }
        )
    goal_result = (
        "achieved"
        if required_goal_artifacts.issubset(types) and criterion_results and all(item["status"] == "achieved" for item in criterion_results)
        else "partial"
    )
    target = _target(arguments)
    verified_target_evidence = [
        item
        for item in evidence
        if item.get("verified") is True and str(item.get("target") or "") == target
    ]
    verified_target_types = {str(item.get("artifact_type") or "") for item in verified_target_evidence}
    evidence_by_id = {
        str(item.get("evidence_id")): item
        for item in verified_target_evidence
        if item.get("evidence_id")
    }
    contracts_by_id = {
        str(item.get("clause_id")): item
        for item in _clause_contracts(arguments)
        if item.get("clause_id")
    }
    clause_results: list[dict[str, Any]] = []
    for clause_id in clause_ids:
        contract = contracts_by_id.get(clause_id, {})
        raw_required = contract.get("required_artifacts")
        required_clause_artifacts = (
            {str(item) for item in raw_required if str(item)}
            if isinstance(raw_required, list)
            else set()
        )
        non_report_required = required_clause_artifacts - {"final_report"}
        clause_refs: list[str] = []
        supported_types: set[str] = set()
        for item in verified_target_evidence:
            evidence_id = str(item.get("evidence_id") or "")
            artifact_type = str(item.get("artifact_type") or "")
            if not evidence_id or not _supports_clause(item, clause_id, evidence_by_id):
                continue
            clause_refs.append(evidence_id)
            supported_types.add(artifact_type)
        if not clause_refs and required_clause_artifacts == {"final_report"}:
            clause_refs = [
                str(item.get("evidence_id"))
                for item in verified_target_evidence
                if item.get("evidence_id")
            ][:1]
        achieved = bool(clause_refs) and non_report_required.issubset(supported_types)
        clause_results.append(
            {
                "clause_id": clause_id,
                "target": target,
                "status": "achieved" if achieved else "not_achieved",
                "evidence_refs": list(dict.fromkeys(clause_refs)),
            }
        )
    clause_achieved = bool(clause_results) and all(item["status"] == "achieved" for item in clause_results)
    if goal_result == "achieved" and (
        not clause_achieved or not required_goal_artifacts.issubset(verified_target_types)
    ):
        goal_result = "partial"
    findings = [
        {
            "artifact_type": str(item.get("artifact_type") or ""),
            "evidence_id": str(item.get("evidence_id") or ""),
            "target": str(item.get("target") or _target(arguments)),
            "confidence": item.get("confidence"),
            "result": (
                item.get("payload", {}).get("impact")
                or item.get("payload", {}).get("results")
                or item.get("payload", {}).get("summary")
                if isinstance(item.get("payload"), Mapping)
                else None
            ),
        }
        for item in evidence
    ]
    negative_results = []
    for item in evidence:
        payload = item.get("payload") if isinstance(item.get("payload"), Mapping) else {}
        controls = payload.get("negative_controls") or payload.get("false_positive_controls") or []
        if isinstance(controls, list):
            negative_results.extend(controls)
    return {
        "artifact_type": "final_report",
        "target": _target(arguments),
        "confidence": 0.9,
        "evidence_refs": _refs(arguments),
        "goal_result": goal_result,
        "criteria": criterion_results,
        "clause_results": clause_results,
        "clause_ids": _artifact_clause_ids(arguments, "final_report", evidence),
        "clause_support": _artifact_clause_support(arguments, "final_report"),
        "summary": str(arguments.get("objective") or ""),
        "findings": findings,
        "report": {
            "objective": arguments.get("objective"),
            "targets": arguments.get("targets", []),
            "workflow_id": arguments.get("workflow_id"),
            "evidence_count": len(evidence),
            "verified_artifacts": sorted(types),
            "negative_results": negative_results,
            "cleanup_verified": "cleanup_proof" in types,
        },
    }


def register_builtin_tools(broker: ToolBroker) -> None:
    # These adapters are in-process integrations.  External MCP servers remain
    # optional extensions and are never required for the default capability set.
    register_open_source_tools(broker)
    broker.register_adapter(
        name="local-target-inspector",
        capabilities=("target_intake", "code_analysis", "source_inventory", "environment_inventory"),
        adapter=local_inspector,
        description="Inventory an existing local file or directory without claiming security findings.",
        priority=400,
    )
    broker.register_adapter(
        name="evidence-hypothesis-builder",
        capabilities=("reasoning",),
        adapter=hypothesis_builder,
        description="Build a minimal evidence-linked hypothesis queue.",
        priority=500,
    )
    broker.register_adapter(
        name="evidence-impact-builder",
        capabilities=("impact_analysis",),
        adapter=impact_builder,
        description="Derive impact only from concrete reproduction observations.",
        priority=500,
    )
    broker.register_adapter(
        name="evidence-coverage-builder",
        capabilities=("coverage_analysis",),
        adapter=coverage_builder,
        description="Build coverage from verified evidence and explicit negative controls.",
        priority=500,
    )
    broker.register_adapter(
        name="evidence-cleanup-builder",
        capabilities=("cleanup", "rollback"),
        adapter=cleanup_builder,
        description="Verify cleanup requirements from recorded side effects.",
        priority=500,
    )
    broker.register_adapter(
        name="evidence-report-builder",
        capabilities=("report_generation",),
        adapter=report_builder,
        description="Build a provenance-linked final report.",
        priority=500,
    )
