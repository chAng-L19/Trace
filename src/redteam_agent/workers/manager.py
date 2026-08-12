from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from ..core import WorkerPort, WorkerResult, WorkerTask


class WorkerManager:
    def __init__(self, workers: Mapping[str, WorkerPort], *, records: Any | None = None) -> None:
        self.workers = dict(workers)
        self.records = records

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

    def cancel(self, task_id: str, run_id: str = "") -> bool:
        if run_id and self.records is not None:
            record = self.records.get_for_run(task_id, run_id)
            if record is None:
                return False
            worker = self.workers.get(record.worker_kind)
            return worker.cancel(task_id) if worker is not None else False
        return any(worker.cancel(task_id) for worker in self.workers.values())


__all__ = ["WorkerManager"]
