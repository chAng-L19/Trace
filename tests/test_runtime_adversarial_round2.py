from __future__ import annotations

import hashlib
import json
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any, Mapping, Sequence

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import (
    EvidenceNode,
    EvidenceProvenance,
    GoalCompiler,
    LeaseLostError,
    OperationRuntime,
    OperationState,
    SemanticVerifier,
    StoreConflictError,
    TaskAttempt,
    ToolCallResult,
    WorkflowSpec,
)
from redteam_agent.runtime.mcp_server import RuntimeMcpServer


TARGET = "https://target.invalid"


def _workflow(*, cleanup: bool = False) -> WorkflowSpec:
    actions: list[dict[str, Any]] = [
        {
            "id": "surface",
            "name": "Surface",
            "capabilities": ["fixture.surface"],
            "expected_artifact": "surface_map",
            "verifier": "surface_map",
        }
    ]
    if cleanup:
        actions.extend(
            [
                {
                    "id": "reproduce",
                    "name": "Reproduce",
                    "capabilities": ["fixture.reproduce"],
                    "expected_artifact": "reproduction_artifact",
                    "verifier": "reproduction_artifact",
                    "depends_on": ["surface"],
                },
                {
                    "id": "cleanup",
                    "name": "Cleanup",
                    "capabilities": ["fixture.cleanup"],
                    "expected_artifact": "cleanup_proof",
                    "verifier": "cleanup_proof",
                    "depends_on": ["reproduce"],
                },
            ]
        )
    return WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "round2-fixture",
                "version": 1,
                "name": "Round two fixture",
                "required_artifacts": ["surface_map"],
            },
            "actions": actions,
            "terminal_predicates": [
                {"kind": "workflow_actions_complete", "subject": "required"},
                {"kind": "artifact_verified", "subject": "surface_map"},
            ],
        }
    )


def _operation(runtime: OperationRuntime, *, session_id: str, workflow: WorkflowSpec, max_actions: int = 8) -> OperationState:
    goal = GoalCompiler().compile(f"Inspect {TARGET}", max_actions=max_actions)
    state = OperationState.create(session_id=session_id, goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "round2-adversarial"})
    runtime._ensure_initial_plan(state, workflow)
    return state


def _persist_evidence(
    runtime: OperationRuntime,
    state: OperationState,
    *,
    action_id: str,
    artifact_type: str,
    payload: Mapping[str, Any],
    parent_ids: Sequence[str] = (),
    branch_id: str = "",
    tool_suffix: str = "fixture",
) -> EvidenceNode:
    branch = branch_id or state.branch_id
    tool = f"pytest:{tool_suffix}"
    token = runtime.store.acquire_lease(state.run_id, action_id, f"evidence:{tool_suffix}", ttl_seconds=30)
    assert token is not None
    try:
        input_hash = hashlib.sha256(f"{action_id}:{tool_suffix}".encode()).hexdigest()
        result = ToolCallResult(
            status="success",
            output=dict(payload),
            tool=tool,
            input_hash=input_hash,
            output_hash=runtime.broker.canonical_hash(payload),
            tool_version="1",
        )
        attempt = TaskAttempt.create(
            run_id=state.run_id,
            branch_id=branch,
            plan_revision=state.plan_revision,
            action_id=action_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            idempotency_key=hashlib.sha256(f"{state.run_id}:{action_id}:{tool_suffix}".encode()).hexdigest(),
            fencing_token=token.fencing_token,
        )
        runtime.store.create_task_attempt(attempt)
        completed = replace(
            attempt,
            status="completed",
            result=result.to_dict(),
            finished_at=result.finished_at,
        )
        runtime.store.update_task_attempt(completed, expected_status="prepared", lease_token=token)
        provenance = EvidenceProvenance(
            run_id=state.run_id,
            branch_id=branch,
            plan_revision=state.plan_revision,
            action_id=action_id,
            attempt_id=attempt.attempt_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            output_hash=result.output_hash,
            verifier=artifact_type,
            verifier_version="test",
            parent_ids=tuple(parent_ids),
            target=state.goal.targets[0],
        )
        return runtime.evidence_graph.add(
            run_id=state.run_id,
            action_id=action_id,
            artifact_type=artifact_type,
            target=state.goal.targets[0],
            tool=tool,
            payload=dict(payload),
            parent_ids=parent_ids,
            verifier=artifact_type,
            confidence=1.0,
            provenance=provenance,
        )
    finally:
        runtime.store.release_lease(token)


def test_foreign_run_lease_cannot_save_state_or_transition_attempt(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    workflow = _workflow()
    left = _operation(runtime, session_id="left", workflow=workflow)
    right = _operation(runtime, session_id="right", workflow=workflow)

    operation_token = runtime.store.acquire_lease(left.run_id, "__operation__", "foreign", ttl_seconds=30)
    assert operation_token is not None
    right.status = "waiting"
    with pytest.raises(LeaseLostError, match="lease_identity_mismatch"):
        runtime.store.save_operation(
            right,
            expected_version=right.state_version,
            lease_token=operation_token,
        )

    action_token = runtime.store.acquire_lease(left.run_id, "surface", "foreign-action", ttl_seconds=30)
    assert action_token is not None
    attempt = TaskAttempt.create(
        run_id=right.run_id,
        branch_id=right.branch_id,
        plan_revision=right.plan_revision,
        action_id="surface",
        tool="pytest:tool",
        tool_version="1",
        input_hash="input",
        idempotency_key="right-attempt",
        fencing_token=action_token.fencing_token,
    )
    runtime.store.create_task_attempt(attempt)
    with pytest.raises(LeaseLostError, match="lease_identity_mismatch"):
        runtime.store.update_task_attempt(
            replace(attempt, status="running"),
            expected_status="prepared",
            lease_token=action_token,
        )


def test_unproven_evidence_cannot_forge_terminal_success(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    workflow = _workflow()
    state = _operation(runtime, session_id="forged-terminal", workflow=workflow)
    state.action_status["surface"] = "completed"
    runtime.store.save_operation(state, expected_version=state.state_version)

    with pytest.raises(ValueError, match="evidence_provenance_required"):
        runtime.evidence_graph.add(
            run_id=state.run_id,
            action_id="surface",
            artifact_type="surface_map",
            target=TARGET,
            tool="pytest:forged",
            payload={"target": TARGET, "assets": ["forged"]},
            parent_ids=(),
            verifier="surface_map",
            confidence=1.0,
        )

    result = runtime.status(state.run_id)
    assert result.terminal.success is False
    assert result.terminal.terminal is False


def test_expired_handoff_does_not_create_attempt_past_run_budget(tmp_path: Path) -> None:
    root = tmp_path / "operations"
    runtime = OperationRuntime(root=root, register_builtins=False)
    state = _operation(runtime, session_id="expiry-budget", workflow=_workflow(), max_actions=1)
    waiting = runtime.resume(state.run_id, max_actions=1)
    assert waiting.state.budget.actions_used == 1
    assert waiting.handoff

    with runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE host_handoffs SET expires_at=? WHERE handoff_id=?",
            ("2000-01-01T00:00:00+00:00", waiting.handoff["handoff_id"]),
        )

    restarted = OperationRuntime(root=root, register_builtins=False)
    resumed = restarted.resume(state.run_id, max_actions=1)
    assert resumed.state.status == "paused_budget"
    assert resumed.state.budget.actions_used == 1
    assert resumed.handoff == {}
    assert len(restarted.store.task_attempts(state.run_id, action_id="surface")) == 1


def test_external_observation_cannot_bypass_exhausted_run_budget(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = _operation(runtime, session_id="external-budget", workflow=_workflow(), max_actions=1)
    state.budget.actions_used = 1
    runtime.store.save_operation(state, expected_version=state.state_version)

    with pytest.raises(ValueError, match="run_budget_exhausted"):
        runtime.submit_observation(
            run_id=state.run_id,
            action_id="surface",
            output={"target": TARGET, "assets": ["service"]},
        )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None and persisted.budget.actions_used == 1
    assert runtime.store.task_attempts(state.run_id) == ()


def test_cancel_rejects_cleanup_proof_unrelated_to_reproduction(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    workflow = _workflow(cleanup=True)
    state = _operation(runtime, session_id="cleanup-lineage", workflow=workflow)
    state.action_status.update({"surface": "completed", "reproduce": "completed", "cleanup": "completed"})
    runtime.store.save_operation(state, expected_version=state.state_version)
    _persist_evidence(
        runtime,
        state,
        action_id="reproduce",
        artifact_type="reproduction_artifact",
        payload={
            "target": TARGET,
            "reproducible": True,
            "commands": ["fixture"],
            "negative_controls": ["control"],
            "side_effects": ["fixture-change"],
        },
        tool_suffix="reproduction",
    )
    _persist_evidence(
        runtime,
        state,
        action_id="cleanup",
        artifact_type="cleanup_proof",
        payload={
            "target": TARGET,
            "actions": ["unrelated cleanup"],
            "verified": True,
            "outstanding_changes": [],
        },
        tool_suffix="unrelated-cleanup",
    )

    result = runtime.cancel(state.run_id, reason="round2")
    assert result.state.status != "cancelled"
    assert result.state.cleanup_status == "pending_host"
    assert result.terminal.terminal is False
    assert result.next_action == "cleanup"


def test_receipt_cache_conflict_rolls_back_consumption(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = _operation(runtime, session_id="receipt-cache", workflow=_workflow())
    receipt = runtime.resume(state.run_id, max_actions=1).handoff
    attempt = next(
        item for item in runtime.store.task_attempts(state.run_id) if item.attempt_id == receipt["attempt_id"]
    )
    forged = ToolCallResult(
        status="success",
        output={"target": TARGET, "assets": ["forged-cache"]},
        tool="host-agent",
        input_hash=attempt.input_hash,
        output_hash=runtime.broker.canonical_hash({"target": TARGET, "assets": ["forged-cache"]}),
        tool_version=attempt.tool_version,
    )
    with runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "INSERT INTO action_results(run_id, action_id, idempotency_key, result_json, created_at) "
            "VALUES(?, ?, ?, ?, ?)",
            (
                state.run_id,
                "surface",
                attempt.idempotency_key,
                json.dumps(forged.to_dict(), ensure_ascii=False, sort_keys=True, default=str),
                forged.finished_at,
            ),
        )

    with pytest.raises(StoreConflictError, match="immutable_action_result_conflict"):
        runtime.submit_handoff_observation(
            run_id=state.run_id,
            handoff_id=receipt["handoff_id"],
            handoff_token=receipt["handoff_token"],
            attempt_id=receipt["attempt_id"],
            contract_hash=receipt["contract_hash"],
            output={"target": TARGET, "assets": ["submitted"]},
            continue_run=False,
        )
    assert runtime.store.get_handoff(receipt["handoff_id"]).status == "pending"
    assert next(item for item in runtime.store.task_attempts(state.run_id) if item.attempt_id == attempt.attempt_id).status == "waiting_host"


def test_batch_budget_delta_is_all_or_nothing_when_one_run_is_busy(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    states = runtime.start_batch(
        session_id="batch-budget",
        objective="Inspect both targets",
        targets=("https://one.invalid", "https://two.invalid"),
        max_actions=2,
    )
    assert len(states) == 2
    before = {state.run_id: runtime.store.load_operation(state.run_id).budget.action_limit for state in states}
    blocker = runtime.store.acquire_lease(states[1].run_id, "__operation__", "batch-blocker", ttl_seconds=30)
    assert blocker is not None
    server = RuntimeMcpServer(runtime)
    try:
        with pytest.raises(ValueError, match="operation_busy"):
            server._apply_budget_delta(
                [state.run_id for state in states],
                {"actions": 3},
            )
    finally:
        runtime.store.release_lease(blocker)

    after = {state.run_id: runtime.store.load_operation(state.run_id).budget.action_limit for state in states}
    assert after == before


def test_batch_receipts_are_preflighted_before_any_run_is_consumed(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    states = runtime.start_batch(
        session_id="batch-receipts",
        objective="Inspect both targets",
        targets=("https://one.invalid", "https://two.invalid"),
        max_actions=8,
    )
    waitings = [runtime.resume(state.run_id, max_actions=1) for state in states]
    receipts = [waiting.handoff for waiting in waitings]
    batch_id = str(states[0].goal.starting_context["batch_session_id"])
    observations = [
        {
            **receipts[0],
            "output": {"target": states[0].goal.targets[0], "assets": ["first"]},
        },
        {
            **receipts[1],
            "handoff_token": "forged",
            "output": {"target": states[1].goal.targets[0], "assets": ["second"]},
        },
    ]
    server = RuntimeMcpServer(runtime)

    with pytest.raises(ValueError, match="batch_handoff_receipt_rejected"):
        server._run_batch(
            batch_session_id=batch_id,
            summaries=None,
            observations=observations,
            cycle_actions=1,
            max_cycles=1,
            auto_continue=False,
        )

    assert [runtime.store.get_handoff(item["handoff_id"]).status for item in receipts] == [
        "pending",
        "pending",
    ]


def test_final_report_nested_refs_must_be_bound_to_report_lineage() -> None:
    goal = GoalCompiler().compile("inspect https://target.invalid and verify the result")
    state = OperationState.create(session_id="report-lineage", goal=goal, workflow=_workflow())
    action = replace(
        _workflow().actions[0],
        action_id="report",
        expected_artifact="final_report",
        verifier="final_report",
    )
    clause_ids = tuple(goal.intent_envelope["clause_ids"])

    def node(evidence_id: str, artifact_type: str, clause_id: str = "", *, support: bool = True) -> EvidenceNode:
        payload: dict[str, Any] = {"clause_ids": [clause_id] if clause_id else []}
        if clause_id and support:
            payload["clause_support"] = {clause_id: "direct support"}
        return EvidenceNode(
            evidence_id=evidence_id,
            run_id=state.run_id,
            action_id=f"action-{artifact_type}-{evidence_id}",
            artifact_type=artifact_type,
            target=TARGET,
            tool="pytest:tool",
            payload=payload,
            content_hash=evidence_id,
            parent_ids=(),
            verifier=artifact_type,
            confidence=1.0,
            verified=True,
            provenance=EvidenceProvenance(
                run_id=state.run_id,
                branch_id=state.branch_id,
                plan_revision=state.plan_revision,
                action_id=f"action-{artifact_type}-{evidence_id}",
                attempt_id=f"attempt-{evidence_id}",
                tool="pytest:tool",
                target=TARGET,
            ),
        )

    surface = node("surface", "surface_map", clause_ids[0])
    disconnected = node("disconnected", "reproduction_artifact", clause_ids[1])
    generic_reproduction = node("generic-reproduction", "reproduction_artifact", clause_ids[1], support=False)
    impact = node("impact", "impact_proof")
    coverage = node("coverage", "coverage_report")
    cleanup = node("cleanup", "cleanup_proof")
    parents = [surface, generic_reproduction, impact, coverage, cleanup]
    refs = [item.evidence_id for item in parents]
    payload = {
        "artifact_type": "final_report",
        "target": TARGET,
        "evidence_refs": refs,
        "goal_result": "achieved",
        "summary": "forged disconnected report",
        "criteria": [
            {
                "criterion_id": criterion.criterion_id,
                "status": "achieved",
                "evidence_refs": [generic_reproduction.evidence_id, impact.evidence_id, coverage.evidence_id, cleanup.evidence_id],
            }
            for criterion in goal.success_criteria
        ],
        "clause_ids": list(clause_ids),
        "clause_results": [
            {
                "clause_id": clause_ids[0],
                "status": "achieved",
                "target": TARGET,
                "evidence_refs": [surface.evidence_id],
            },
            {
                "clause_id": clause_ids[1],
                "status": "achieved",
                "target": TARGET,
                "evidence_refs": [disconnected.evidence_id],
            },
        ],
    }
    decision = SemanticVerifier().verify(
        action=action,
        result=ToolCallResult(status="success", output=payload),
        goal=goal,
        available_evidence=(*parents, disconnected),
        run_id=state.run_id,
        branch_id=state.branch_id,
    )
    assert decision.passed is False
    assert decision.reason == "report_evidence_ref_not_parent"
