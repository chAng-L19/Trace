from __future__ import annotations

from collections.abc import Mapping

from ..core import WorkerPort, WorkerResult, WorkerTask


class WorkerManager:
    def __init__(self, workers: Mapping[str, WorkerPort]) -> None:
        self.workers = dict(workers)

    def capabilities(self) -> tuple[str, ...]:
        return tuple(
            sorted({capability for worker in self.workers.values() for capability in worker.capabilities()})
        )

    def execute(self, task: WorkerTask) -> WorkerResult:
        kind = str(task.metadata.get("worker_kind") or task.capability.partition(".")[0]).strip()
        aliases = {"codex": "codex_handoff", "local": "local", "mcp": "mcp", "docker": "docker"}
        worker = self.workers.get(aliases.get(kind, kind))
        if worker is None:
            return WorkerResult(
                task_id=task.task_id,
                status="unavailable",
                error=f"worker_capability_unavailable:{task.capability}",
                retryable=True,
            )
        return worker.execute(task)

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        matches = tuple(
            result
            for worker in self.workers.values()
            if (result := worker.reconcile(idempotency_key)) is not None
        )
        return matches[0] if len(matches) == 1 else None

    def cancel(self, task_id: str) -> bool:
        return any(worker.cancel(task_id) for worker in self.workers.values())


__all__ = ["WorkerManager"]
