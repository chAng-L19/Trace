from __future__ import annotations

import hashlib
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.mcp_server import RuntimeMcpServer
from redteam_agent.runtime.models import EvidenceProvenance, OperationState, TaskAttempt, ToolCallResult, WorkflowSpec
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.tool_broker import ToolBroker


class _MutationSentinelRuntime:
    def __init__(self) -> None:
        self.submit_calls: list[dict[str, Any]] = []

    def submit_observation(self, **arguments: Any) -> dict[str, Any]:
        self.submit_calls.append(arguments)
        return {"status": "mutated"}


def _rpc_call(server: RuntimeMcpServer, request_id: int, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    response = server.handle(
        {
            "jsonrpc": "2.0",
            "id": request_id,
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        }
    )
    assert isinstance(response, dict)
    return response


def _persist_fixture_evidence(
    runtime: OperationRuntime,
    state: OperationState,
    *,
    action_id: str,
    artifact_type: str,
    payload: dict[str, Any],
    parent_ids: tuple[str, ...] = (),
) -> str:
    tool = f"fixture:{artifact_type}"
    token = runtime.store.acquire_lease(
        state.run_id,
        action_id,
        f"test:{artifact_type}",
        ttl_seconds=30,
    )
    assert token is not None
    try:
        input_hash = runtime.broker.canonical_hash(
            {"run_id": state.run_id, "action_id": action_id, "payload": payload}
        )
        result = ToolCallResult(
            status="success",
            output=payload,
            tool=tool,
            input_hash=input_hash,
            output_hash=runtime.broker.canonical_hash(payload),
            tool_version="1",
        )
        attempt = TaskAttempt.create(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            idempotency_key=hashlib.sha256(
                f"{state.run_id}:{action_id}:{artifact_type}:{len(runtime.store.task_attempts(state.run_id))}".encode()
            ).hexdigest(),
            fencing_token=token.fencing_token,
        )
        runtime.store.create_task_attempt(attempt)
        runtime.store.update_task_attempt(
            replace(attempt, status="completed", result=result.to_dict(), finished_at=result.finished_at),
            expected_status="prepared",
            lease_token=token,
        )
        provenance = EvidenceProvenance(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            attempt_id=attempt.attempt_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            output_hash=result.output_hash,
            verifier=artifact_type,
            parent_ids=parent_ids,
            target=state.goal.targets[0],
        )
        node = runtime.evidence_graph.add(
            run_id=state.run_id,
            action_id=action_id,
            artifact_type=artifact_type,
            target=state.goal.targets[0],
            tool=tool,
            payload=payload,
            parent_ids=parent_ids,
            verifier=artifact_type,
            confidence=1.0,
            provenance=provenance,
        )
        return node.evidence_id
    finally:
        runtime.store.release_lease(token)


def _cancel_fixture(
    tmp_path: Path,
    *,
    include_cleanup: bool,
    broker: ToolBroker | None = None,
) -> tuple[OperationRuntime, OperationState, WorkflowSpec, str]:
    runtime = OperationRuntime(
        root=tmp_path / "operations",
        broker=broker,
        register_builtins=False,
    )
    goal = GoalCompiler().compile("Assess https://target.invalid and preserve rollback evidence")
    actions: list[dict[str, Any]] = [
        {
            "id": "validate",
            "name": "Validate",
            "capabilities": ["fixture.validation"],
            "expected_artifact": "reproduction_artifact",
            "verifier": "reproduction_artifact",
        }
    ]
    if include_cleanup:
        actions.append(
            {
                "id": "cleanup",
                "name": "Cleanup",
                "capabilities": ["fixture.cleanup"],
                "expected_artifact": "cleanup_proof",
                "verifier": "cleanup_proof",
                "depends_on": ["validate"],
                "max_retries": 0,
            }
        )
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "cancel-control-fixture",
                "version": 1,
                "name": "Cancel control fixture",
            },
            "actions": actions,
        }
    )
    state = OperationState.create(session_id="cancel-control", goal=goal, workflow=workflow)
    state.action_status["validate"] = "completed"
    runtime.store.create_operation(state, event={"source": "cancel-control-test"})
    runtime._ensure_initial_plan(state, workflow)
    reproduction_id = _persist_fixture_evidence(
        runtime,
        state,
        action_id="validate",
        artifact_type="reproduction_artifact",
        payload={"steps": ["trigger"], "observed": ["effect"], "verified": True},
    )
    return runtime, state, workflow, reproduction_id


def test_json_rpc_rejects_hidden_observation_tool_before_runtime_dispatch() -> None:
    runtime = _MutationSentinelRuntime()
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]

    response = _rpc_call(
        server,
        1,
        "redteam_submit_observation",
        {"run_id": "run-fixture", "action_id": "action-fixture", "output": {"verified": True}},
    )

    assert response["error"]["code"] == -32602
    assert response["error"]["message"] == "tool_not_found:redteam_submit_observation"
    assert runtime.submit_calls == []


def test_missing_target_is_persisted_then_late_bound_without_recompiling_goal(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    server = RuntimeMcpServer(runtime)
    objective = "Map the exposed surface; produce a reproducible report"

    started = _rpc_call(
        server,
        1,
        "redteam_run",
        {"session_id": "late-target", "objective": objective, "auto_continue": False},
    )["result"]["structuredContent"]

    assert started["status"] == "waiting_goal_input"
    assert started["run_id"]
    waiting = runtime.store.load_operation(started["run_id"])
    assert waiting is not None
    assert waiting.goal.targets == ()
    assert waiting.goal.objective == objective
    assert waiting.goal.intent_envelope["source_sha256"] == hashlib.sha256(objective.encode("utf-8")).hexdigest()
    original_goal_id = waiting.goal.goal_id
    original_objective = waiting.goal.objective
    original_envelope = dict(waiting.goal.intent_envelope)
    original_criteria = waiting.goal.success_criteria
    original_run_id = waiting.run_id

    resumed = _rpc_call(
        server,
        2,
        "redteam_run",
        {
            "run_id": waiting.run_id,
            "targets": ["https://target.invalid"],
            "auto_continue": False,
            "max_actions": 1,
        },
    )["result"]["structuredContent"]

    rebound = runtime.store.load_operation(waiting.run_id)
    assert rebound is not None
    assert resumed["run_id"] == original_run_id
    assert rebound.goal.goal_id == original_goal_id
    assert rebound.goal.targets == ("https://target.invalid",)
    assert rebound.goal.objective == original_objective
    assert dict(rebound.goal.intent_envelope) == original_envelope
    assert rebound.goal.success_criteria == original_criteria
    assert tuple(rebound.goal.intent_envelope["clause_ids"]) == tuple(original_envelope["clause_ids"])
    event_types = [item["event_type"] for item in runtime.store.events(waiting.run_id)]
    assert event_types.count("goal_target_supplied") == 1
    assert "goal_input_required" in event_types


def test_valid_host_receipt_advances_using_execution_outcome_progressed(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "host-progress-fixture",
                "version": 1,
                "name": "Host progress fixture",
                "required_artifacts": ["surface_map"],
            },
            "actions": [
                {
                    "id": "host-surface",
                    "name": "Host surface",
                    "capabilities": ["fixture.host"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
            "terminal_predicates": [
                {"kind": "workflow_actions_complete", "subject": "required"},
                {"kind": "artifact_verified", "subject": "surface_map"},
            ],
        }
    )
    state = OperationState.create(session_id="host-progress", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "host-progress-test"})
    runtime._ensure_initial_plan(state, workflow)

    waiting = runtime.resume(state.run_id, max_actions=1)
    receipt = waiting.handoff
    assert waiting.state.status == "waiting_host"
    assert receipt["action_id"] == "host-surface"

    asserted = runtime.submit_handoff_observation(
        run_id=state.run_id,
        handoff_id=str(receipt["handoff_id"]),
        handoff_token=str(receipt["handoff_token"]),
        attempt_id=str(receipt["attempt_id"]),
        contract_hash=str(receipt["contract_hash"]),
        output={"target": goal.targets[0], "assets": ["fixture-service"]},
        max_actions=1,
    )

    assert asserted.state.status == "waiting_host"
    assert asserted.terminal.terminal is False
    assert asserted.terminal.success is False
    assert asserted.summary()["next_action_spec"]["phase"] == "verify-observation"
    nodes = runtime.evidence_graph.list(state.run_id, include_unverified=True)
    assert [(node.artifact_type, node.trust, node.verified) for node in nodes] == [
        ("host_observation", "host_asserted", False)
    ]
    assert runtime.store.get_handoff(str(receipt["handoff_id"])).status == "consumed"


def test_partial_host_observation_is_asserted_and_rotates_verification_receipt(
    tmp_path: Path,
) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {"id": "host-retry-fixture", "version": 1, "name": "Host retry fixture"},
            "actions": [
                {
                    "id": "host-surface",
                    "name": "Host surface",
                    "capabilities": ["fixture.host"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
        }
    )
    state = OperationState.create(session_id="host-retry", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "host-retry-test"})
    runtime._ensure_initial_plan(state, workflow)
    first = runtime.resume(state.run_id, max_actions=1).handoff

    retried = runtime.submit_handoff_observation(
        run_id=state.run_id,
        handoff_id=str(first["handoff_id"]),
        handoff_token=str(first["handoff_token"]),
        attempt_id=str(first["attempt_id"]),
        contract_hash=str(first["contract_hash"]),
        output={"target": goal.targets[0]},
        max_actions=1,
    )

    assert retried.state.status == "waiting_host"
    assert retried.handoff["handoff_id"] != first["handoff_id"]
    attempts = runtime.store.task_attempts(state.run_id, action_id="host-surface")
    assert {item.status for item in attempts} == {"completed", "waiting_host"}
    assert len({item.idempotency_key for item in attempts}) == 2
    assertions = runtime.evidence_graph.list(state.run_id, include_unverified=True)
    assert len(assertions) == 1
    assert assertions[0].payload == {"target": goal.targets[0]}
    assert assertions[0].trust == "host_asserted"


def test_valid_external_observation_uses_execution_outcome_progressed(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "external-progress-fixture",
                "version": 1,
                "name": "External progress fixture",
                "required_artifacts": ["surface_map"],
            },
            "actions": [
                {
                    "id": "external-surface",
                    "name": "External surface",
                    "capabilities": ["fixture.external"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
            "terminal_predicates": [
                {"kind": "workflow_actions_complete", "subject": "required"},
                {"kind": "artifact_verified", "subject": "surface_map"},
            ],
        }
    )
    state = OperationState.create(session_id="external-progress", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "external-progress-test"})
    runtime._ensure_initial_plan(state, workflow)

    asserted = runtime.submit_observation(
        run_id=state.run_id,
        action_id="external-surface",
        output={"target": goal.targets[0], "assets": ["fixture-service"]},
        continue_run=True,
        max_actions=1,
    )

    assert asserted.state.status == "waiting_host"
    assert asserted.terminal.terminal is False
    assert asserted.terminal.success is False
    assert asserted.summary()["next_action_spec"]["phase"] == "verify-observation"
    nodes = runtime.evidence_graph.list(state.run_id, include_unverified=True)
    assert [(node.artifact_type, node.trust, node.verified) for node in nodes] == [
        ("host_observation", "host_asserted", False)
    ]


def test_cancel_without_cleanup_action_stays_durably_cancelling(tmp_path: Path) -> None:
    runtime, state, _, _ = _cancel_fixture(tmp_path, include_cleanup=False)

    result = runtime.cancel(state.run_id, reason="fixture-stop")
    persisted = runtime.store.load_operation(state.run_id)

    assert result.state.status == "cancelling"
    assert result.state.cleanup_status == "unavailable"
    assert result.terminal.terminal is False
    assert result.terminal.reason == "cancel_cleanup_unavailable"
    assert persisted is not None
    assert persisted.status == "cancelling"
    assert persisted.cleanup_status == "unavailable"
    assert persisted.failure_reason == ""
    before_version = persisted.state_version

    restarted = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    resumed = restarted.resume(state.run_id)
    persisted_again = restarted.store.load_operation(state.run_id)
    assert resumed.state.status == "cancelling"
    assert resumed.terminal.terminal is False
    assert persisted_again is not None and persisted_again.state_version == before_version
    event_types = [item["event_type"] for item in restarted.store.events(state.run_id)]
    assert event_types.count("cancel_cleanup_unavailable") == 1
    assert "operation_cancelled" not in event_types


def test_failed_cancel_cleanup_remains_pending_until_verified_proof(tmp_path: Path) -> None:
    broker = ToolBroker()
    broker.register_adapter(
        name="failing-cleanup",
        capabilities=("fixture.cleanup",),
        adapter=lambda arguments: {
            "evidence_refs": list(arguments.get("evidence_refs", ())),
            "actions": ["attempted rollback"],
            "verified": False,
            "outstanding_changes": ["fixture-change"],
        },
    )
    runtime, state, _, reproduction_id = _cancel_fixture(
        tmp_path,
        include_cleanup=True,
        broker=broker,
    )

    failed = runtime.cancel(state.run_id, reason="fixture-stop")
    persisted = runtime.store.load_operation(state.run_id)

    assert failed.state.status == "cancelling"
    assert failed.state.cleanup_status == "failed"
    assert failed.terminal.terminal is False
    assert failed.terminal.reason == "cancel_cleanup_failed"
    assert failed.next_action == "cleanup"
    assert persisted is not None
    assert persisted.status == "cancelling"
    assert persisted.cleanup_status == "failed"
    assert "operation_cancelled" not in {
        item["event_type"] for item in runtime.store.events(state.run_id)
    }

    _persist_fixture_evidence(
        runtime,
        state,
        action_id="cleanup",
        artifact_type="cleanup_proof",
        payload={
            "evidence_refs": [reproduction_id],
            "actions": ["rollback verified"],
            "verified": True,
            "outstanding_changes": [],
        },
        parent_ids=(reproduction_id,),
    )
    completed = runtime.resume(state.run_id)
    final_state = runtime.store.load_operation(state.run_id)

    assert completed.state.status == "cancelled"
    assert completed.state.cleanup_status == "verified"
    assert completed.terminal.terminal is True
    assert completed.terminal.success is False
    assert final_state is not None and final_state.status == "cancelled"
    assert "operation_cancelled" in {
        item["event_type"] for item in runtime.store.events(state.run_id)
    }


def test_cancel_cleanup_handoff_is_durably_pending_not_cancelled(tmp_path: Path) -> None:
    runtime, state, _, _ = _cancel_fixture(tmp_path, include_cleanup=True)

    result = runtime.cancel(state.run_id, reason="fixture-stop")
    persisted = runtime.store.load_operation(state.run_id)

    assert result.state.status == "waiting_host"
    assert result.state.cleanup_status == "pending_host"
    assert result.terminal.terminal is False
    assert result.next_action == "cleanup"
    assert result.handoff["action_id"] == "cleanup"
    assert persisted is not None
    assert persisted.status == "waiting_host"
    assert persisted.cleanup_status == "pending_host"
    assert persisted.cancel_reason == "fixture-stop"


def test_cancel_cleanup_executor_exception_is_persisted_as_pending_failure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    broker = ToolBroker()
    broker.register_adapter(
        name="raising-cleanup",
        capabilities=("fixture.cleanup",),
        adapter=lambda _: {"verified": True},
    )
    runtime, state, _, _ = _cancel_fixture(
        tmp_path,
        include_cleanup=True,
        broker=broker,
    )

    def raise_executor_error(*_: Any, **__: Any) -> None:
        raise RuntimeError("fixture executor failure")

    monkeypatch.setattr(runtime.executor, "execute", raise_executor_error)
    result = runtime.cancel(state.run_id, reason="fixture-stop")
    persisted = runtime.store.load_operation(state.run_id)

    assert result.state.status == "cancelling"
    assert result.state.cleanup_status == "failed"
    assert result.terminal.terminal is False
    assert persisted is not None and persisted.status == "cancelling"
    failure_events = [
        item for item in runtime.store.events(state.run_id)
        if item["event_type"] == "cancel_cleanup_failed"
    ]
    assert failure_events[-1]["payload"]["reason"] == "executor_exception:RuntimeError"
    assert "fixture executor failure" not in str(failure_events[-1]["payload"])
