from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping

from ..core import WorkerResult, WorkerTask, contract_hash
from .model_common import utc_now
from .store_common import ImmutableRecordError, StoreConflictError, _dump, _load


WORKER_TERMINAL_STATUSES = frozenset({"completed", "failed", "timed_out", "cancelled", "unavailable"})


@dataclass(frozen=True, slots=True)
class WorkerTaskRecord:
    task: WorkerTask
    worker_kind: str
    input_hash: str
    status: str
    result: WorkerResult | None
    owner: str
    created_at: str
    updated_at: str


class WorkerStore:
    def __init__(self, store: Any) -> None:
        self.store = store

    @staticmethod
    def input_hash(task: WorkerTask) -> str:
        return contract_hash(task.to_dict())

    def prepare(self, task: WorkerTask, *, worker_kind: str, owner: str) -> WorkerTaskRecord:
        input_hash = self.input_hash(task)
        now = utc_now()
        serialized = _dump(task.to_dict())
        with self.store.transaction(immediate=True) as connection:
            task_row = connection.execute(
                "SELECT * FROM worker_tasks WHERE task_id=?", (task.task_id,)
            ).fetchone()
            if task_row is not None:
                task_record = self._from_row(task_row)
                if (
                    task_record.task.run_id != task.run_id
                    or task_record.worker_kind != worker_kind
                    or task_record.task.idempotency_key != task.idempotency_key
                ):
                    raise ImmutableRecordError(f"worker_task_identity_conflict:{task.task_id}")
                if task_record.input_hash != input_hash:
                    raise ImmutableRecordError(
                        f"worker_idempotency_conflict:{task.run_id}:{worker_kind}:{task.idempotency_key}"
                    )
                return task_record
            row = connection.execute(
                "SELECT * FROM worker_tasks WHERE run_id=? AND worker_kind=? AND idempotency_key=?",
                (task.run_id, worker_kind, task.idempotency_key),
            ).fetchone()
            if row is not None:
                record = self._from_row(row)
                if record.input_hash != input_hash or record.task.task_id != task.task_id:
                    raise ImmutableRecordError(
                        f"worker_idempotency_conflict:{task.run_id}:{worker_kind}:{task.idempotency_key}"
                    )
                return record
            connection.execute(
                "INSERT INTO worker_tasks(task_id, run_id, worker_kind, capability, idempotency_key, "
                "input_hash, status, task_json, result_json, owner, created_at, updated_at) "
                "VALUES(?, ?, ?, ?, ?, ?, 'prepared', ?, '', ?, ?, ?)",
                (
                    task.task_id,
                    task.run_id,
                    worker_kind,
                    task.capability,
                    task.idempotency_key,
                    input_hash,
                    serialized,
                    owner,
                    now,
                    now,
                ),
            )
        return WorkerTaskRecord(task, worker_kind, input_hash, "prepared", None, owner, now, now)

    def transition(
        self,
        task_id: str,
        *,
        expected_statuses: tuple[str, ...],
        status: str,
        result: WorkerResult | None = None,
        owner: str = "",
    ) -> WorkerTaskRecord:
        statuses = tuple(dict.fromkeys(expected_statuses))
        if not statuses:
            raise ValueError("worker_expected_status_required")
        now = utc_now()
        with self.store.transaction(immediate=True) as connection:
            row = connection.execute("SELECT * FROM worker_tasks WHERE task_id=?", (task_id,)).fetchone()
            if row is None:
                raise KeyError(f"worker_task_not_found:{task_id}")
            current = self._from_row(row)
            if current.status not in statuses:
                raise StoreConflictError(
                    f"worker_transition_conflict:{task_id}:{current.status}:{','.join(statuses)}"
                )
            cursor = connection.execute(
                "UPDATE worker_tasks SET status=?, result_json=?, owner=?, updated_at=? "
                "WHERE task_id=? AND status=?",
                (
                    status,
                    _dump(result.to_dict()) if result is not None else "",
                    owner or current.owner,
                    now,
                    task_id,
                    current.status,
                ),
            )
            if cursor.rowcount != 1:
                raise StoreConflictError(f"worker_transition_race:{task_id}")
            updated = connection.execute("SELECT * FROM worker_tasks WHERE task_id=?", (task_id,)).fetchone()
        assert updated is not None
        return self._from_row(updated)

    def get(self, task_id: str) -> WorkerTaskRecord | None:
        with self.store.connection() as connection:
            row = connection.execute("SELECT * FROM worker_tasks WHERE task_id=?", (task_id,)).fetchone()
        return self._from_row(row) if row is not None else None

    def reconcile(self, run_id: str, worker_kind: str, idempotency_key: str) -> WorkerTaskRecord | None:
        with self.store.connection() as connection:
            row = connection.execute(
                "SELECT * FROM worker_tasks WHERE run_id=? AND worker_kind=? AND idempotency_key=?",
                (run_id, worker_kind, idempotency_key),
            ).fetchone()
        return self._from_row(row) if row is not None else None

    def records(self, run_id: str) -> tuple[WorkerTaskRecord, ...]:
        with self.store.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM worker_tasks WHERE run_id=? ORDER BY created_at, task_id", (run_id,)
            ).fetchall()
        return tuple(self._from_row(row) for row in rows)

    @staticmethod
    def _from_row(row: Mapping[str, Any]) -> WorkerTaskRecord:
        task = WorkerTask.from_dict(_load(row["task_json"], {}))
        payload = _load(row["result_json"], None)
        result = WorkerResult.from_dict(payload) if isinstance(payload, Mapping) else None
        return WorkerTaskRecord(
            task=task,
            worker_kind=str(row["worker_kind"]),
            input_hash=str(row["input_hash"]),
            status=str(row["status"]),
            result=result,
            owner=str(row["owner"]),
            created_at=str(row["created_at"]),
            updated_at=str(row["updated_at"]),
        )


__all__ = ["WORKER_TERMINAL_STATUSES", "WorkerStore", "WorkerTaskRecord"]
