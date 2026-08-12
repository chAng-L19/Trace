from __future__ import annotations

from ..core import WorkerResult, WorkerTask
from ..runtime.worker_store import WorkerStore


class CodexHandoffWorker:
    kind = "codex_handoff"

    def __init__(self, *, records: WorkerStore) -> None:
        self.records = records

    def capabilities(self) -> tuple[str, ...]:
        return ("codex.handoff",)

    def execute(self, task: WorkerTask) -> WorkerResult:
        prepared = self.records.prepare(task, worker_kind=self.kind, owner="codex-host")
        if prepared.result is not None:
            return prepared.result
        result = WorkerResult(
            task_id=task.task_id,
            status="waiting_worker",
            output={
                "next_action_spec": dict(task.payload),
                "required_artifacts": list(task.required_artifacts),
            },
            metadata={"worker_kind": self.kind, "replay_protected": True},
        )
        self.records.transition(
            task.task_id,
            expected_statuses=("prepared", "unknown"),
            status="waiting_worker",
            result=result,
            owner="codex-host",
        )
        return result

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        with self.records.store.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM worker_tasks WHERE worker_kind=? AND idempotency_key=?",
                (self.kind, idempotency_key),
            ).fetchall()
        if len(rows) != 1:
            return None
        return self.records._from_row(rows[0]).result

    def cancel(self, task_id: str) -> bool:
        record = self.records.get(task_id)
        if record is None or record.status != "waiting_worker":
            return False
        result = WorkerResult(task_id=task_id, status="cancelled", error="handoff_cancelled")
        self.records.transition(
            task_id,
            expected_statuses=("waiting_worker",),
            status="cancelled",
            result=result,
        )
        return True


__all__ = ["CodexHandoffWorker"]
