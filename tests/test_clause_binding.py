from __future__ import annotations

import sys
from dataclasses import replace
from pathlib import Path


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import DurableStore, EvidenceGraph, EvidenceNode, EvidenceProvenance, GoalCompiler, OperationState, TerminalJudge, ToolBroker, ToolCallResult, WorkflowRegistry
from redteam_agent.runtime.adaptive_planner import AdaptivePlanner
from redteam_agent.runtime.builtins import report_builder
from redteam_agent.runtime.executor import ActionExecutor
from redteam_agent.runtime.verifier import SemanticVerifier


def _node(
    *,
    evidence_id: str,
    run_id: str,
    branch_id: str,
    target: str,
    artifact_type: str,
    clause_ids: tuple[str, ...] = (),
) -> EvidenceNode:
    return EvidenceNode(
        evidence_id=evidence_id,
        run_id=run_id,
        action_id=f"action-{artifact_type}",
        artifact_type=artifact_type,
        target=target,
        tool="pytest:tool",
        payload={
            "artifact_type": artifact_type,
            "clause_ids": list(clause_ids),
            "clause_support": {
                clause_id: {"artifact_type": artifact_type, "source_text": clause_id}
                for clause_id in clause_ids
            },
        },
        content_hash=evidence_id,
        parent_ids=(),
        verifier=artifact_type,
        confidence=0.9,
        verified=True,
        provenance=EvidenceProvenance(
            run_id=run_id,
            branch_id=branch_id,
            plan_revision=1,
            action_id=f"action-{artifact_type}",
            attempt_id=f"attempt-{artifact_type}",
            tool="pytest:tool",
            target=target,
        ),
    )


def _report_case() -> tuple[object, object, object, tuple[EvidenceNode, ...], dict[str, object]]:
    goal = GoalCompiler().compile("inspect https://target.invalid and verify the result")
    workflow = WorkflowRegistry().match(goal)
    state = OperationState.create(session_id="clause-binding", goal=goal, workflow=workflow)
    target = goal.targets[0]
    clause_ids = tuple(goal.intent_envelope["clause_ids"])
    contracts = {
        str(item["clause_id"]): {str(value) for value in item["required_artifacts"]}
        for item in goal.intent_envelope["clause_contracts"]
    }
    evidence = tuple(
        _node(
            evidence_id=f"evidence-{artifact_type}",
            run_id=state.run_id,
            branch_id=state.branch_id,
            target=target,
            artifact_type=artifact_type,
            clause_ids=tuple(clause_id for clause_id in clause_ids if artifact_type in contracts[clause_id]),
        )
        for artifact_type in (
            "surface_map",
            "reproduction_artifact",
            "impact_proof",
            "coverage_report",
            "cleanup_proof",
        )
    )
    refs = [node.evidence_id for node in evidence]
    criteria = [
        {
            "criterion_id": criterion.criterion_id,
            "status": "achieved",
            "evidence_refs": refs,
        }
        for criterion in goal.success_criteria
    ]
    clauses = [
        {
            "clause_id": clause_id,
            "status": "achieved",
            "target": target,
            "evidence_refs": [node.evidence_id for node in evidence if clause_id in node.payload["clause_ids"]],
        }
        for clause_id in goal.intent_envelope["clause_ids"]
    ]
    payload: dict[str, object] = {
        "artifact_type": "final_report",
        "target": target,
        "evidence_refs": refs,
        "goal_result": "achieved",
        "criteria": criteria,
        "clause_results": clauses,
        "clause_ids": list(clause_ids),
        "summary": "verified report",
    }
    action = next(item for item in workflow.actions if item.expected_artifact == "final_report")
    return goal, state, action, evidence, payload


def _verify_case(goal, state, action, evidence, payload):
    return SemanticVerifier().verify(
        action=action,
        result=ToolCallResult(status="success", output=payload),
        goal=goal,
        available_evidence=evidence,
        run_id=state.run_id,
        branch_id=state.branch_id,
    )


def test_final_report_rejects_missing_clause_results() -> None:
    goal, state, action, evidence, payload = _report_case()
    payload.pop("clause_results")

    decision = _verify_case(goal, state, action, evidence, payload)

    assert decision.passed is False
    assert decision.reason == "report_requires_goal_criteria_and_findings"


def test_final_report_rejects_forged_or_extra_clause_ids() -> None:
    goal, state, action, evidence, payload = _report_case()
    payload["clause_results"] = [
        {**payload["clause_results"][0], "clause_id": "clause-forged"},
        *payload["clause_results"][1:],
    ]

    decision = _verify_case(goal, state, action, evidence, payload)

    assert decision.passed is False
    assert decision.reason == "clause_results_mismatch"


def test_final_report_rejects_cross_target_clause_evidence() -> None:
    goal, state, action, evidence, payload = _report_case()
    wrong_target = "https://other.invalid"
    foreign = _node(
        evidence_id="evidence-other-target",
        run_id=state.run_id,
        branch_id=state.branch_id,
        target=wrong_target,
        artifact_type="reproduction_artifact",
        clause_ids=(goal.intent_envelope["clause_ids"][0],),
    )
    payload["clause_results"][0]["evidence_refs"] = [foreign.evidence_id]

    decision = _verify_case(goal, state, action, (*evidence, foreign), payload)

    assert decision.passed is False
    assert decision.reason == "clause_result_evidence_scope_mismatch"


def test_final_report_rejects_generic_evidence_without_clause_declarations() -> None:
    goal, state, action, evidence, payload = _report_case()
    untagged = tuple(replace(node, payload={"artifact_type": node.artifact_type, "clause_ids": []}) for node in evidence)
    payload["clause_ids"] = []

    decision = _verify_case(goal, state, action, untagged, payload)

    assert decision.passed is False
    assert decision.reason == "clause_result_evidence_unsupported"


def test_final_report_accepts_clause_declaration_from_verified_ancestor() -> None:
    goal, state, action, evidence, payload = _report_case()
    clause_id = goal.intent_envelope["clause_ids"][0]
    source = next(node for node in evidence if clause_id in node.payload["clause_ids"])
    derived = _node(
        evidence_id="evidence-derived-coverage",
        run_id=state.run_id,
        branch_id=state.branch_id,
        target=goal.targets[0],
        artifact_type="coverage_report",
        clause_ids=(clause_id,),
    )
    derived = replace(derived, parent_ids=(source.evidence_id,), provenance=replace(derived.provenance, parent_ids=(source.evidence_id,)))
    clause_result = next(item for item in payload["clause_results"] if item["clause_id"] == clause_id)
    clause_result["evidence_refs"] = [derived.evidence_id]
    payload["evidence_refs"].append(derived.evidence_id)

    decision = _verify_case(goal, state, action, evidence + (derived,), payload)

    assert decision.passed is True


def test_verifier_rejects_unknown_clause_id_on_any_artifact() -> None:
    goal, state, _, _, _ = _report_case()
    workflow = WorkflowRegistry().match(goal)
    action = next(item for item in workflow.actions if item.expected_artifact == "surface_map")

    decision = SemanticVerifier().verify(
        action=action,
        result=ToolCallResult(
            status="success",
            output={
                "artifact_type": "surface_map",
                "target": goal.targets[0],
                "results": ["surface"],
                "clause_ids": ["clause-unknown"],
            },
        ),
        goal=goal,
        available_evidence=(),
        run_id=state.run_id,
        branch_id=state.branch_id,
    )

    assert decision.passed is False
    assert decision.reason == "clause_ids_unknown"


def test_derived_evidence_can_bind_a_new_clause_with_explicit_support() -> None:
    goal, state, _, evidence, _ = _report_case()
    workflow = WorkflowRegistry().match(goal)
    action = next(item for item in workflow.actions if item.expected_artifact == "reproduction_artifact")
    parent = replace(evidence[0], payload={"artifact_type": "surface_map", "clause_ids": []})
    clause_id = next(
        item["clause_id"]
        for item in goal.intent_envelope["clause_contracts"]
        if "reproduction_artifact" in item["required_artifacts"]
    )

    decision = SemanticVerifier().verify(
        action=action,
        result=ToolCallResult(
            status="success",
            output={
                "artifact_type": "reproduction_artifact",
                "target": goal.targets[0],
                "reproducible": True,
                "evidence_refs": [parent.evidence_id],
                "observations": ["the clause-specific behavior was reproduced"],
                "negative_controls": ["the matched control did not reproduce"],
                "side_effects": False,
                "clause_ids": [clause_id],
                "clause_support": {clause_id: "the recorded observation directly validates this clause"},
            },
        ),
        goal=goal,
        available_evidence=(parent,),
        run_id=state.run_id,
        branch_id=state.branch_id,
    )

    assert decision.passed is True


def test_derived_evidence_rejects_a_new_clause_without_explicit_support() -> None:
    goal, state, _, evidence, _ = _report_case()
    workflow = WorkflowRegistry().match(goal)
    action = next(item for item in workflow.actions if item.expected_artifact == "reproduction_artifact")
    parent = replace(evidence[0], payload={"artifact_type": "surface_map", "clause_ids": []})
    clause_id = next(
        item["clause_id"]
        for item in goal.intent_envelope["clause_contracts"]
        if "reproduction_artifact" in item["required_artifacts"]
    )

    decision = SemanticVerifier().verify(
        action=action,
        result=ToolCallResult(
            status="success",
            output={
                "artifact_type": "reproduction_artifact",
                "target": goal.targets[0],
                "reproducible": True,
                "evidence_refs": [parent.evidence_id],
                "observations": ["generic observation"],
                "negative_controls": ["matched control"],
                "side_effects": False,
                "clause_ids": [clause_id],
            },
        ),
        goal=goal,
        available_evidence=(parent,),
        run_id=state.run_id,
        branch_id=state.branch_id,
    )

    assert decision.passed is False
    assert decision.reason == "derived_clause_support_required"


def test_complete_clause_binding_passes_and_terminal_rejects_generic_artifacts_without_it() -> None:
    goal, state, action, evidence, payload = _report_case()
    decision = _verify_case(goal, state, action, evidence, payload)
    report = _node(
        evidence_id="evidence-final-report",
        run_id=state.run_id,
        branch_id=state.branch_id,
        target=goal.targets[0],
        artifact_type="final_report",
    )
    report = replace(report, payload=payload, parent_ids=tuple(node.evidence_id for node in evidence))

    assert decision.passed is True
    assert TerminalJudge._goal_clauses_complete((report,), goal, evidence + (report,), state) is True
    assert TerminalJudge._goal_clauses_complete((), goal, evidence, state) is False


def test_builtin_report_binds_each_clause_only_to_verified_existing_evidence() -> None:
    goal, state, _, evidence, _ = _report_case()
    arguments = {
        "objective": goal.objective,
        "target": goal.targets[0],
        "targets": list(goal.targets),
        "goal_criteria": [criterion.__dict__ for criterion in goal.success_criteria],
        "intent_envelope": goal.intent_envelope,
        "evidence": [
            {
                "evidence_id": node.evidence_id,
                "artifact_type": node.artifact_type,
                "target": node.target,
                "verified": True,
                "payload": node.payload,
                "parent_ids": list(node.parent_ids),
            }
            for node in evidence
        ],
    }

    report = report_builder(arguments)

    assert {item["clause_id"] for item in report["clause_results"]} == set(goal.intent_envelope["clause_ids"])
    assert all(item["status"] == "achieved" for item in report["clause_results"]), report["clause_results"]
    for item in report["clause_results"]:
        expected = {
            node.evidence_id
            for node in evidence
            if item["clause_id"] in node.payload["clause_ids"]
        }
        assert set(item["evidence_refs"]) == expected


def test_executor_exposes_lossless_clause_contract_to_the_tool(tmp_path: Path) -> None:
    goal, state, _, _, _ = _report_case()
    workflow = WorkflowRegistry().match(goal)
    store = DurableStore(tmp_path / "operations")
    store.create_operation(state, event={"source": "pytest"})
    graph = EvidenceGraph(store, tmp_path / "artifacts")
    broker = ToolBroker()
    descriptor = broker.register_adapter(
        name="clause-contract-check",
        capabilities=("report_generation",),
        adapter=lambda arguments: arguments,
    )
    executor = ActionExecutor(
        store=store,
        evidence_graph=graph,
        broker=broker,
        verifier=SemanticVerifier(),
        planner=AdaptivePlanner(),
    )
    action = next(item for item in workflow.actions if item.expected_artifact == "final_report")

    arguments, _, _ = executor.arguments_for(state, workflow, action, descriptor)

    assert arguments["intent_envelope"] == goal.intent_envelope
    assert arguments["clause_ids"] == list(goal.intent_envelope["clause_ids"])
    assert [item["clause_id"] for item in arguments["clause_contract"]] == list(goal.intent_envelope["clause_ids"])
