from __future__ import annotations

from dataclasses import replace
from pathlib import Path

import pytest

from redteam_agent import AgentService
from redteam_agent.adapters import RuntimeEventAdapter, RuntimeStoreAdapter, RuntimeToolAdapter
from redteam_agent.core import Event, Run, TerminalDecision, ToolCall
from redteam_agent.core.ports import EventPort, StoreConflictError, StorePort, ToolPort
from redteam_agent.runtime.operation_runtime import OperationRuntime


def _service(tmp_path: Path) -> tuple[AgentService, Path]:
    target = tmp_path / "target.txt"
    target.write_text("phase1 adapter fixture\n", encoding="utf-8")
    runtime = OperationRuntime(root=tmp_path / "runtime")
    return AgentService(runtime=runtime), target


def test_agent_service_projects_plan_only_run_to_core_contracts(tmp_path: Path) -> None:
    service, target = _service(tmp_path)
    run = service.start(
        {
            "session_id": "phase1-plan",
            "objective": f"Give me a plan for {target}; do not make changes yet and no need to run tests",
            "targets": (str(target),),
            "max_actions": 16,
        }
    ).single.run
    store = RuntimeStoreAdapter(service.runtime)
    events = RuntimeEventAdapter(service.runtime)
    tools = RuntimeToolAdapter(service.runtime)

    assert isinstance(run, Run)
    assert run.status == "running"
    assert isinstance(store, StorePort)
    assert isinstance(events, EventPort)
    assert isinstance(tools, ToolPort)

    completed = service.run(run.run_id, max_actions=16)

    assert completed.run.status == "completed"
    assert completed.goal.goal_id == run.goal_id
    assert isinstance(completed.terminal, TerminalDecision)
    assert completed.terminal.terminal is True
    assert completed.terminal.success is True
    assert {item.artifact_type for item in completed.evidence} == {
        "surface_map",
        "hypothesis_queue",
        "final_report",
    }
    assert all(item.provenance is not None for item in completed.evidence)


def test_agent_service_preserves_host_waiting_state_projection(tmp_path: Path) -> None:
    service, target = _service(tmp_path)
    run = service.start(
        {
            "session_id": "phase1-waiting",
            "objective": (
                f"Inspect {target}; validate the highest-value path; prove impact; "
                "run a negative control; verify cleanup; write the final report"
            ),
            "targets": (str(target),),
            "max_actions": 32,
        }
    ).single.run

    waiting = service.run(run.run_id, max_actions=32)

    assert waiting.run.status == "waiting_worker"
    assert waiting.run.metadata["legacy_status"] == "waiting_host"
    assert waiting.next_action == "validate-path"
    assert waiting.handoff


def test_runtime_store_adapter_preserves_cas_semantics(tmp_path: Path) -> None:
    service, target = _service(tmp_path)
    run = service.start(
        {
            "session_id": "phase1-store",
            "objective": f"Give me a plan for {target}; do not make changes yet and no need to run tests",
            "targets": (str(target),),
        }
    ).single.run
    store = RuntimeStoreAdapter(service.runtime)
    paused = replace(
        run,
        status="paused_budget",
        budget=replace(run.budget, pause_reason="phase1-test"),
    )

    committed = store.commit_run(paused, expected_version=run.state_version)

    assert committed.state_version == run.state_version + 1
    assert committed.status == "paused_budget"
    assert committed.budget.pause_reason == "phase1-test"
    legacy = service.runtime.store.load_operation(run.run_id)
    assert legacy is not None
    assert legacy.status == "paused_budget"
    with pytest.raises(StoreConflictError, match="state_version_conflict"):
        store.commit_run(paused, expected_version=run.state_version)


def test_runtime_store_adapter_cannot_bypass_runtime_control_transitions(tmp_path: Path) -> None:
    service, target = _service(tmp_path)
    run = service.start(
        {
            "session_id": "phase1-store-invariants",
            "objective": f"Give me a plan for {target}; do not make changes yet and no need to run tests",
            "targets": (str(target),),
        }
    ).single.run
    store = RuntimeStoreAdapter(service.runtime)

    with pytest.raises(ValueError, match="control_transition_requires_runtime"):
        store.commit_run(replace(run, status="completed"), expected_version=run.state_version)
    with pytest.raises(ValueError, match="action_transition_requires_runtime"):
        store.commit_run(
            replace(run, current_search_node_id="forged-action"),
            expected_version=run.state_version,
        )
    with pytest.raises(ValueError, match="evidence_transition_requires_runtime"):
        store.commit_run(
            replace(run, evidence_ids=("forged-evidence",)),
            expected_version=run.state_version,
        )


def test_runtime_event_and_tool_ports_bridge_existing_components(tmp_path: Path) -> None:
    service, target = _service(tmp_path)
    run = service.start(
        {
            "session_id": "phase1-ports",
            "objective": f"Give me a plan for {target}; do not make changes yet and no need to run tests",
            "targets": (str(target),),
        }
    ).single.run
    events = RuntimeEventAdapter(service.runtime)
    tools = RuntimeToolAdapter(service.runtime)
    before = events.read(run.run_id)
    events.append(Event(run_id=run.run_id, event_type="phase1_probe", payload={"ok": True}))
    after = events.read(run.run_id, after_sequence=before[-1].sequence)

    assert [item.event_type for item in after] == ["phase1_probe"]
    definitions = {item.qualified_name: item for item in tools.discover()}
    assert "builtin:local-target-inspector" in definitions

    result = tools.invoke(
        ToolCall(
            call_id="phase1-call",
            run_id=run.run_id,
            tool_name="builtin:local-target-inspector",
            arguments={"target": str(target), "expected_artifact": "surface_map"},
            timeout_seconds=10.0,
        )
    )

    assert result.status == "success"
    assert result.call_id
    assert result.tool_name == "builtin:local-target-inspector"
    assert result.output["artifact_type"] == "surface_map"
