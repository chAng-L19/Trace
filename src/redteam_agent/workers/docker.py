from __future__ import annotations

from ..core import WorkerResult, WorkerTask
from ..runtime.worker_store import WORKER_TERMINAL_STATUSES, WorkerStore


class DockerWorkerAdapter:
    kind = "docker"

    def __init__(self, *, records: WorkerStore) -> None:
        self.records = records

    def capabilities(self) -> tuple[str, ...]:
        return ()

    def execute(self, task: WorkerTask) -> WorkerResult:
        prepared = self.records.prepare(task, worker_kind=self.kind, owner="docker-adapter")
        if prepared.result is not None and prepared.status in WORKER_TERMINAL_STATUSES:
            return prepared.result
        result = WorkerResult(
            task_id=task.task_id,
            status="unavailable",
            error="docker_worker_adapter_not_configured",
            retryable=True,
            metadata={"worker_kind": self.kind},
        )
        self.records.transition(
            task.task_id,
            expected_statuses=(prepared.status,),
            status="unavailable",
            result=result,
        )
        return result

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        return self.records.reconcile_kind(self.kind, idempotency_key)

    def cancel(self, task_id: str) -> bool:
        return False


__all__ = ["DockerWorkerAdapter"]
