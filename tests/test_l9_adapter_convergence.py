from __future__ import annotations

from pathlib import Path

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import WorkerResult, WorkerTask
from redteam_agent.workers.manager import WorkerManager


class _ClosableWorker:
    def __init__(self, created: list["_ClosableWorker"]) -> None:
        self.closed = False
        created.append(self)

    def capabilities(self) -> tuple[str, ...]:
        return ("fixture.lazy",)

    def execute(self, task: WorkerTask) -> WorkerResult:
        return WorkerResult(task_id=task.task_id, status="completed", output={"ok": True})

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        return None

    def cancel(self, task_id: str) -> bool:
        return False

    def close(self) -> None:
        self.closed = True


def test_worker_manager_loads_factories_only_on_first_execution() -> None:
    created: list[_ClosableWorker] = []
    manager = WorkerManager(
        factories={"docker": lambda: _ClosableWorker(created)},
        capabilities={"docker": ("fixture.lazy",)},
    )

    assert manager.loaded_kinds == ()
    assert manager.registered_kinds == ("docker",)
    assert manager.capabilities() == ("fixture.lazy",)

    result = manager.execute(
        WorkerTask(
            task_id="lazy-task",
            run_id="run",
            capability="docker.command",
            payload={},
            idempotency_key="lazy-key",
            metadata={"worker_kind": "docker"},
        )
    )

    assert result.status == "completed"
    assert len(created) == 1
    assert manager.loaded_kinds == ("docker",)
    assert manager.restart("docker") is True
    assert created[0].closed is True
    assert manager.loaded_kinds == ()


def test_worker_observation_is_idempotent_and_run_scoped(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = service.start(
        StartRequest(session_id="l9-observation", objective="Run a fixture worker")
    ).single.run.run_id
    task = WorkerTask(
        task_id="observation-task",
        run_id=run_id,
        capability="docker.command",
        payload={"image": "fixture"},
        idempotency_key="observation-key",
        metadata={"worker_kind": "docker"},
    )

    first = service.execute_worker(task)
    second = service.execute_worker(task)

    assert first == second
    observations = service.worker_observations(run_id)
    assert len(observations) == 1
    assert observations[0]["task_id"] == task.task_id
    assert observations[0]["observation_hash"]
    assert observations[0]["result"]["task_id"] == task.task_id


def test_agent_service_defers_low_frequency_workers(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    assert isinstance(service.workers, WorkerManager)
    assert "codex_handoff" not in service.workers.loaded_kinds
    assert "docker" not in service.workers.loaded_kinds
    assert "codex.handoff" in service.workers.capabilities()


def test_agent_service_close_releases_loaded_workers(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = service.start(
        StartRequest(session_id="l9-close", objective="Use docker fixture")
    ).single.run.run_id
    service.execute_worker(
        WorkerTask(
            task_id="close-task",
            run_id=run_id,
            capability="docker.command",
            payload={},
            idempotency_key="close-key",
            metadata={"worker_kind": "docker"},
        )
    )
    assert "docker" in service.workers.loaded_kinds

    service.close()

    assert service.workers.loaded_kinds == ()
