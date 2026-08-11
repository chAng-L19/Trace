from __future__ import annotations

from dataclasses import replace
from pathlib import Path

import pytest

from redteam_agent import AgentService, BudgetDelta, Observation, StartRequest
from redteam_agent.application import ALLOWED_RUN_TRANSITIONS, CANONICAL_RUN_STATUSES
from redteam_agent.core import RUN_STATUSES
from redteam_agent.runtime import OperationRuntime
from redteam_agent.runtime.store_common import ImmutableRecordError


def _plan_request(session_id: str, target: Path) -> StartRequest:
    return StartRequest(
        session_id=session_id,
        objective=(
            f"Give me a plan for {target}; do not make changes yet and no need to run tests"
        ),
        targets=(str(target),),
        max_actions=16,
    )


def test_agent_service_exposes_the_canonical_lifecycle(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("phase-2", encoding="utf-8")
    service = AgentService(root=tmp_path / "operations")

    started = service.start(_plan_request("phase2-service", target))
    completed = service.run(started.single.run.run_id)

    assert CANONICAL_RUN_STATUSES == RUN_STATUSES
    assert set(ALLOWED_RUN_TRANSITIONS) == set(RUN_STATUSES)
    assert started.single.run.status == "running"
    assert completed.run.status == "completed"
    assert completed.terminal.terminal is True
    assert completed.terminal.success is True
    assert {item.artifact_type for item in completed.evidence} == {
        "surface_map",
        "hypothesis_queue",
        "final_report",
    }


def test_internal_waiting_states_project_to_waiting_worker(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "operations")
    started = service.start(
        StartRequest(
            session_id="phase2-waiting-goal",
            objective="Assess the supplied target and produce an evidence-linked report",
        )
    )

    waiting = service.run(started.single.run.run_id, max_actions=1)

    assert waiting.run.status == "waiting_worker"
    assert waiting.run.metadata["legacy_status"] == "waiting_goal_input"
    assert waiting.next_action == "provide_target"


def test_budget_delta_with_same_idempotency_key_applies_once(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "operations")
    started = service.start(
        StartRequest(
            session_id="phase2-budget",
            objective="Assess the supplied target and produce an evidence-linked report",
        )
    ).single
    delta = BudgetDelta(actions=4, tokens=1000, idempotency_key="budget-command-1")

    first = service.run(started.run.run_id, delta, max_actions=1)
    second = service.run(started.run.run_id, delta, max_actions=1)

    assert first.run.budget.action_limit == 68
    assert second.run.budget.action_limit == 68
    assert second.run.budget.token_limit == 1000
    events = [
        event
        for event in service.events(started.run.run_id)
        if event.event_type == "budget_delta_applied"
    ]
    assert len(events) == 1
    assert events[0].payload["idempotency_hash"]

    with pytest.raises(ImmutableRecordError, match="budget_delta_idempotency_conflict"):
        service.run(
            started.run.run_id,
            BudgetDelta(actions=5, idempotency_key="budget-command-1"),
            max_actions=1,
        )


def test_terminal_run_rejects_new_budget_delta(tmp_path: Path) -> None:
    target = tmp_path / "terminal-budget.txt"
    target.write_text("terminal", encoding="utf-8")
    service = AgentService(root=tmp_path / "operations")
    started = service.start(_plan_request("terminal-budget", target)).single
    completed = service.run(started.run.run_id)

    assert completed.run.status == "completed"
    with pytest.raises(ValueError, match="operation_terminal:completed"):
        service.run(
            started.run.run_id,
            BudgetDelta(actions=1, idempotency_key="late-budget"),
        )


def test_observation_idempotency_prevents_duplicate_assertions(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    service = AgentService(runtime=runtime)
    started = service.start(
        StartRequest(
            session_id="phase2-observation",
            objective="Inspect https://target.invalid",
            targets=("https://target.invalid",),
        )
    ).single
    waiting = service.run(started.run.run_id, max_actions=1)
    observation = Observation(
        action_id=waiting.next_action,
        output={"target": "https://target.invalid", "assets": ["fixture-api"]},
        idempotency_key="observation-command-1",
        continue_run=False,
    )

    first = service.submit_observation(started.run.run_id, observation)
    replayed = service.submit_observation(started.run.run_id, observation)

    assert first.run.status == "running"
    assert replayed.run.status == "running"
    assertions = [item for item in replayed.evidence if item.artifact_type == "host_observation"]
    assert len(assertions) == 1
    assert assertions[0].trust == "host_asserted"

    with pytest.raises(ValueError, match="external_observation_idempotency_conflict"):
        service.submit_observation(
            started.run.run_id,
            Observation(
                action_id=waiting.next_action,
                output={"target": "https://target.invalid", "assets": ["different-api"]},
                idempotency_key="observation-command-1",
                continue_run=False,
            ),
        )


def test_events_resume_after_sequence_without_replaying_prior_events(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("events", encoding="utf-8")
    service = AgentService(root=tmp_path / "operations")
    started = service.start(_plan_request("phase2-events", target)).single

    before = service.events(started.run.run_id)
    service.run(started.run.run_id, max_actions=1)
    after = service.events(started.run.run_id, after_sequence=before[-1].sequence)

    assert after
    assert min(item.sequence for item in after) > before[-1].sequence
    assert tuple(item.sequence for item in after) == tuple(
        sorted(item.sequence for item in after)
    )


def test_multi_target_start_creates_isolated_runs_and_evidence(tmp_path: Path) -> None:
    left = tmp_path / "left.txt"
    right = tmp_path / "right.txt"
    left.write_text("left", encoding="utf-8")
    right.write_text("right", encoding="utf-8")
    service = AgentService(root=tmp_path / "operations")

    started = service.start(
        StartRequest(
            session_id="phase2-batch",
            objective="Give me a plan for both local targets; do not modify them or run tests",
            targets=(str(left), str(right)),
            max_actions=16,
        )
    )

    assert started.batch_id
    assert len(started.runs) == 2
    assert len(set(started.run_ids)) == 2
    completed = tuple(service.run(run_id) for run_id in started.run_ids)
    assert {view.run.status for view in completed} == {"completed"}
    assert {view.goal.targets for view in completed} == {(str(left),), (str(right),)}

    evidence_ids: list[set[str]] = []
    fact_ids: list[set[str]] = []
    for view in completed:
        target = view.goal.targets[0]
        assert all(item.run_id == view.run.run_id for item in view.evidence)
        assert all(item.target == target for item in view.evidence)
        evidence_ids.append({item.evidence_id for item in view.evidence})
        facts = service.runtime.store.facts(view.run.run_id)
        assert facts
        assert all(item.run_id == view.run.run_id for item in facts)
        fact_ids.append({item.fact_id for item in facts})
    assert evidence_ids[0].isdisjoint(evidence_ids[1])
    assert fact_ids[0].isdisjoint(fact_ids[1])


def test_runs_do_not_share_credential_references(tmp_path: Path) -> None:
    left = tmp_path / "credential-left.txt"
    right = tmp_path / "credential-right.txt"
    left.write_text("left", encoding="utf-8")
    right.write_text("right", encoding="utf-8")
    service = AgentService(root=tmp_path / "operations")

    left_run = service.start(
        replace(
            _plan_request("credential-left", left),
            starting_context={"api_key": "left-phase2-secret"},
        )
    ).single
    right_run = service.start(
        replace(
            _plan_request("credential-right", right),
            starting_context={"api_key": "right-phase2-secret"},
        )
    ).single
    left_state = service.runtime.store.load_operation(left_run.run.run_id)
    right_state = service.runtime.store.load_operation(right_run.run.run_id)

    assert left_state is not None and right_state is not None
    assert left_state.credential_refs
    assert right_state.credential_refs
    assert set(left_state.credential_refs).isdisjoint(right_state.credential_refs)
