from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Event

import pytest

from redteam_agent import AgentService
from redteam_agent.core import WorkerResult, WorkerTask
from redteam_agent.runtime.store_common import LeaseLostError
from test_worker_adapters import FixtureToolPort, _run
from test_worker_plane import _task


def test_live_local_worker_is_not_recovered_by_another_service(tmp_path: Path, monkeypatch) -> None:
    first = AgentService(root=tmp_path / "runtime")
    second = AgentService(root=first.runtime.root)
    run_id = _run(first, "concurrent-local")
    task = _task(run_id, "same-local", "print('executed once')")
    entered, release = Event(), Event()
    ensure = first.workspaces.ensure

    def blocked_ensure(run_id):
        entered.set()
        assert release.wait(5)
        return ensure(run_id)

    monkeypatch.setattr(first.workspaces, "ensure", blocked_ensure)
    try:
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(first.execute_worker, task)
            assert entered.wait(5)
            try:
                with pytest.raises(RuntimeError, match="worker_task_already_active"):
                    second.execute_worker(task)
                assert first.worker_status(run_id, task.task_id).status == "running"
            finally:
                release.set()
            assert future.result(timeout=5).status == "completed"
    finally:
        release.set()
        first.close()
        second.close()


def test_live_mcp_worker_is_not_recovered_by_another_service(tmp_path: Path) -> None:
    entered, release = Event(), Event()

    class BlockingTools(FixtureToolPort):
        def invoke(self, call):
            entered.set()
            assert release.wait(5)
            return super().invoke(call)

    tools = BlockingTools()
    first = AgentService(root=tmp_path / "runtime", tool_port=tools)
    second = AgentService(root=first.runtime.root, tool_port=FixtureToolPort())
    run_id = _run(first, "concurrent-mcp")
    task = WorkerTask(
        task_id="same-mcp", run_id=run_id, capability="mcp.fixture:inspect",
        payload={"tool_name": "fixture:inspect", "arguments": {}},
        idempotency_key="same-mcp", metadata={"worker_kind": "mcp"},
    )
    try:
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(first.execute_worker, task)
            assert entered.wait(5)
            try:
                with pytest.raises(RuntimeError, match="worker_task_already_active"):
                    second.execute_worker(task)
                assert first.worker_status(run_id, task.task_id).status == "running"
            finally:
                release.set()
            assert future.result(timeout=5).status == "completed"
        assert len(tools.calls) == 1
    finally:
        release.set()
        first.close()
        second.close()


@pytest.mark.parametrize("control", ["pause", "cancel"])
def test_controlled_run_replays_worker_but_rejects_new_execution(tmp_path: Path, control: str) -> None:
    with AgentService(root=tmp_path / "runtime") as service:
        run_id = _run(service, "controlled-worker")
        previous_task = _task(run_id, "before-control", "print('before')")
        previous = service.execute_worker(previous_task)
        getattr(service, control)(run_id)
        assert service.execute_worker(previous_task) == previous

        with pytest.raises(ValueError, match="worker_run_not_executable"):
            service.execute_worker(_task(run_id, "after-control", "print('must not execute')"))
        assert len(service.worker_results(run_id)) == 1


def test_cancelled_handoff_replay_records_new_observation_once(tmp_path: Path) -> None:
    with AgentService(root=tmp_path / "runtime") as service:
        run_id = _run(service, "handoff-observation")
        task = WorkerTask(
            task_id="handoff", run_id=run_id, capability="codex.handoff", payload={},
            idempotency_key="handoff", metadata={"worker_kind": "codex_handoff"},
        )
        assert service.execute_worker(task).status == "waiting_worker"
        assert service.cancel_worker(run_id, task.task_id)
        cancelled = service.execute_worker(task)
        assert cancelled.status == "cancelled"
        assert service.execute_worker(task) == cancelled
        assert [item["result"]["status"] for item in service.worker_observations(run_id)] == [
            "waiting_worker", "cancelled",
        ]


def test_local_cancel_during_artifact_capture_wins_completion(tmp_path: Path, monkeypatch) -> None:
    with AgentService(root=tmp_path / "runtime") as service:
        run_id = _run(service, "cancel-artifact-race")
        task = _task(run_id, "artifact-cancel", "print('finished process')")
        entered, release = Event(), Event()
        put_file = service.runtime.artifacts.put_file

        def blocked_put_file(*args, **kwargs):
            entered.set()
            assert release.wait(5)
            return put_file(*args, **kwargs)

        monkeypatch.setattr(service.runtime.artifacts, "put_file", blocked_put_file)
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(service.execute_worker, task)
            assert entered.wait(5)
            try:
                assert service.cancel_worker(run_id, task.task_id)
            finally:
                release.set()
            assert future.result(timeout=5).status == "cancelled"
        assert not service.cancel_worker(run_id, task.task_id)


def test_local_cancel_reports_accepted_when_worker_commits_cancelled_first(
    tmp_path: Path, monkeypatch,
) -> None:
    with AgentService(root=tmp_path / "runtime") as service:
        run_id = _run(service, "cancel-return-race")
        task = _task(run_id, "cancel-return", "print('unused')")
        worker = service.workers._resolve("local")
        service.worker_records.prepare(task, worker_kind="local", owner="fixture")
        service.worker_records.transition(
            task.task_id, expected_statuses=("prepared",), status="running",
        )
        request_cancel = service.worker_records.request_cancel

        def request_then_finish(task_id: str) -> bool:
            accepted = request_cancel(task_id)
            service.worker_records.transition(
                task_id,
                expected_statuses=("running",),
                status="cancelled",
                result=WorkerResult(task_id=task_id, status="cancelled", error="worker_cancelled"),
            )
            return accepted

        monkeypatch.setattr(service.worker_records, "request_cancel", request_then_finish)
        assert worker.cancel(task.task_id) is True
        assert service.worker_status(run_id, task.task_id).status == "cancelled"


def test_worker_cancel_from_another_service_reaches_running_process(tmp_path: Path) -> None:
    with AgentService(root=tmp_path / "runtime") as first, AgentService(root=tmp_path / "runtime") as second:
        run_id = _run(first, "cross-service-cancel")
        started = Event()
        task = _task(run_id, "cross-cancel", "import time; time.sleep(30)", timeout_seconds=30)
        worker = first.workers._resolve("local")
        original_ensure = first.workspaces.ensure

        def ensure(run_id):
            started.set()
            return original_ensure(run_id)

        first.workspaces.ensure = ensure
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(first.execute_worker, task)
            assert started.wait(5)
            assert second.cancel_worker(run_id, task.task_id)
            try:
                assert future.result(timeout=5).status == "cancelled"
            finally:
                worker.cancel(task.task_id)


def test_worker_execution_lease_renews_and_rejects_stale_commit(tmp_path: Path, monkeypatch) -> None:
    from redteam_agent.runtime import worker_store

    with AgentService(root=tmp_path / "runtime") as service:
        run_id = _run(service, "worker-fencing")
        task = _task(run_id, "fenced", "print('unused')")
        renewed = Event()
        renew = service.runtime.store.renew_lease

        def record_renewal(*args, **kwargs):
            result = renew(*args, **kwargs)
            renewed.set()
            return result

        monkeypatch.setattr(worker_store, "WORKER_LEASE_SECONDS", 0.15)
        monkeypatch.setattr(service.runtime.store, "renew_lease", record_renewal)
        with service.worker_records.execution(task) as token:
            service.worker_records.prepare(task, worker_kind="local", owner="first")
            service.worker_records.transition(task.task_id, expected_statuses=("prepared",), status="running")
            assert renewed.wait(2)
            service.runtime.store.release_lease(token)
            replacement = service.runtime.store.acquire_lease(run_id, token.action_id, "replacement")
            assert replacement is not None
            try:
                with pytest.raises(LeaseLostError, match="worker_lease_lost"):
                    service.worker_records.mark_interrupted_unknown(task.task_id, owner="stale")
                assert service.worker_status(run_id, task.task_id).status == "running"
            finally:
                service.runtime.store.release_lease(replacement)
