from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Mapping

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.runtime import OperationRuntime, ToolBroker
from redteam_agent.runtime.builtins import local_inspector, report_builder


PLAN_ACTIONS = (
    ("map-surface", "surface_map"),
    ("build-hypotheses", "hypothesis_queue"),
    ("report", "final_report"),
)
ACTION_CASES = tuple(enumerate(item[1] for item in PLAN_ACTIONS))


class SimulatedProcessCrash(RuntimeError):
    pass


def _request(session_id: str, target: Path) -> StartRequest:
    return StartRequest(
        session_id=session_id,
        objective=(
            f"Give me a plan for {target}; do not make changes yet and no need to run tests"
        ),
        targets=(str(target),),
        max_actions=16,
    )


def _advance_to_action(service: AgentService, run_id: str, action_id: str) -> None:
    for _ in range(16):
        state = service.runtime.store.load_operation(run_id)
        assert state is not None
        attempts = service.runtime.store.task_attempts(run_id, action_id=action_id)
        if state.action_status.get(action_id) == "pending" and not attempts:
            view = service.status(run_id)
            if view.next_action == action_id:
                return
        service.run(run_id, max_actions=1)
    raise AssertionError(f"action_boundary_not_reached:{action_id}")


def _run_until_artifact(
    service: AgentService,
    run_id: str,
    artifact_type: str,
):
    for _ in range(8):
        view = service.run(run_id, max_actions=1)
        if any(item.artifact_type == artifact_type for item in view.evidence):
            return view
    raise AssertionError(f"artifact_not_recovered:{artifact_type}")


@pytest.mark.parametrize(("action_index", "artifact_type"), ACTION_CASES)
def test_restart_before_each_action_recovers_exact_boundary(
    tmp_path: Path,
    action_index: int,
    artifact_type: str,
) -> None:
    target = tmp_path / f"before-{action_index}.txt"
    target.write_text(f"before-{action_index}", encoding="utf-8")
    root = tmp_path / f"operations-before-{action_index}"
    service = AgentService(root=root)
    run_id = service.start(_request(f"before-{action_index}", target)).single.run.run_id
    action_id = PLAN_ACTIONS[action_index][0]
    _advance_to_action(service, run_id, action_id)

    restarted = AgentService(root=root)
    observed = _run_until_artifact(restarted, run_id, artifact_type)

    matches = [item for item in observed.evidence if item.artifact_type == artifact_type]
    assert len(matches) == 1
    assert matches[0].run_id == run_id


@pytest.mark.parametrize(("action_index", "artifact_type"), ACTION_CASES)
def test_restart_after_cached_action_result_reconciles_without_duplicate_evidence(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    action_index: int,
    artifact_type: str,
) -> None:
    target = tmp_path / f"after-{action_index}.txt"
    target.write_text(f"after-{action_index}", encoding="utf-8")
    root = tmp_path / f"operations-after-{action_index}"
    service = AgentService(root=root)
    run_id = service.start(_request(f"after-{action_index}", target)).single.run.run_id
    action_id = PLAN_ACTIONS[action_index][0]
    _advance_to_action(service, run_id, action_id)
    original = service.runtime.store.commit_action_outcome
    crashed = False

    def crash_after_result_cache(**arguments: Any) -> int:
        nonlocal crashed
        if arguments["attempt"].action_id == action_id and not crashed:
            crashed = True
            raise SimulatedProcessCrash(f"crash-after:{action_id}")
        return original(**arguments)

    monkeypatch.setattr(service.runtime.store, "commit_action_outcome", crash_after_result_cache)
    for _ in range(8):
        try:
            service.run(run_id, max_actions=1)
        except SimulatedProcessCrash as exc:
            assert str(exc) == f"crash-after:{action_id}"
            break
    else:
        raise AssertionError(f"crash_point_not_reached:{action_id}")

    restarted = AgentService(root=root)
    recovered = _run_until_artifact(restarted, run_id, artifact_type)
    matches = [item for item in recovered.evidence if item.artifact_type == artifact_type]

    assert len(matches) == 1
    assert matches[0].run_id == run_id
    attempts = restarted.runtime.store.task_attempts(run_id, action_id=action_id)
    assert len([item for item in attempts if item.status == "completed"]) == 1
    assert len({item.idempotency_key for item in attempts}) == len(attempts)


def _counting_broker(counter: list[int]) -> ToolBroker:
    broker = ToolBroker()

    def counting_inspector(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
        counter[0] += 1
        return local_inspector(arguments)

    broker.register_adapter(
        name="phase2-counting-inspector",
        capabilities=("target_intake", "code_analysis"),
        adapter=counting_inspector,
        priority=0,
    )
    return broker


def test_concurrent_recovery_has_one_submitter_and_one_tool_side_effect(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    target = tmp_path / "concurrent.txt"
    target.write_text("concurrent", encoding="utf-8")
    root = tmp_path / "operations"
    counter = [0]
    runtime = OperationRuntime(root=root, broker=_counting_broker(counter))
    service = AgentService(runtime=runtime)
    run_id = service.start(_request("concurrent-recovery", target)).single.run.run_id
    original = runtime.store.commit_action_outcome

    def crash_once(**arguments: Any) -> int:
        monkeypatch.setattr(runtime.store, "commit_action_outcome", original)
        raise SimulatedProcessCrash("crash-after-side-effect")

    monkeypatch.setattr(runtime.store, "commit_action_outcome", crash_once)
    with pytest.raises(SimulatedProcessCrash, match="crash-after-side-effect"):
        service.run(run_id, max_actions=1)
    assert counter == [1]

    services = tuple(
        AgentService(
            runtime=OperationRuntime(root=root, broker=_counting_broker(counter))
        )
        for _ in range(2)
    )
    barrier = threading.Barrier(2)

    def recover(candidate: AgentService) -> str:
        barrier.wait(timeout=5)
        return candidate.run(run_id, max_actions=1).run.status

    with ThreadPoolExecutor(max_workers=2) as pool:
        statuses = tuple(pool.map(recover, services))

    final = services[0].status(run_id)
    surface = [item for item in final.evidence if item.artifact_type == "surface_map"]
    assert set(statuses) <= {"running", "paused_budget"}
    assert counter == [1]
    assert len(surface) == 1
    attempts = services[0].runtime.store.task_attempts(run_id, action_id="map-surface")
    assert len(attempts) == 1
    assert attempts[0].status == "completed"


def test_cancel_and_completion_race_has_one_authoritative_terminal(
    tmp_path: Path,
) -> None:
    target = tmp_path / "cancel-race.txt"
    target.write_text("cancel-race", encoding="utf-8")
    root = tmp_path / "operations"
    entered = threading.Event()
    release = threading.Event()
    broker = ToolBroker()

    def blocking_report(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
        entered.set()
        if not release.wait(timeout=10):
            raise TimeoutError("release_not_received")
        return report_builder(arguments)

    broker.register_adapter(
        name="phase2-blocking-report",
        capabilities=("report_generation",),
        adapter=blocking_report,
        priority=0,
    )
    runner = AgentService(runtime=OperationRuntime(root=root, broker=broker))
    run_id = runner.start(_request("cancel-complete-race", target)).single.run.run_id
    _advance_to_action(runner, run_id, "report")
    canceller = AgentService(root=root)

    def finish_report() -> Any:
        for _ in range(8):
            view = runner.run(run_id, max_actions=1)
            if view.run.status in {"completed", "cancelled", "failed"}:
                return view
        raise AssertionError("report_did_not_reach_terminal_state")

    with ThreadPoolExecutor(max_workers=1) as pool:
        future = pool.submit(finish_report)
        assert entered.wait(timeout=5)
        cancelling = canceller.cancel(run_id, reason="phase2-race")
        assert cancelling.run.status in {"cancelling", "cancelled"}
        release.set()
        raced = future.result(timeout=15)

    final = canceller.status(run_id)
    terminal_events = [
        event.event_type
        for event in canceller.events(run_id)
        if event.event_type in {"operation_completed", "operation_cancelled"}
    ]
    assert raced.run.status == "cancelled"
    assert final.run.status == "cancelled"
    assert final.terminal.terminal is True
    assert final.terminal.success is False
    assert terminal_events == ["operation_cancelled"]
    assert not [item for item in final.evidence if item.artifact_type == "final_report"]
