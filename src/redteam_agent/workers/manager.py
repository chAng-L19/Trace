from __future__ import annotations

from collections.abc import Callable, Mapping
import threading
from typing import Any

from ..core import WorkerPort, WorkerResult, WorkerTask
from ..runtime.worker_store import WORKER_TERMINAL_STATUSES, WorkerStore


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
        result = WorkerResult(task_id=task.task_id, status="waiting_worker", output={"next_action_spec": dict(task.payload), "required_artifacts": list(task.required_artifacts)}, metadata={"worker_kind": self.kind, "replay_protected": True})
        self.records.transition(task.task_id, expected_statuses=("prepared", "unknown"), status="waiting_worker", result=result, owner="codex-host")
        return result

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        return self.records.reconcile_kind(self.kind, idempotency_key)

    def cancel(self, task_id: str) -> bool:
        record = self.records.get(task_id)
        if record is None or record.status != "waiting_worker":
            return False
        self.records.transition(task_id, expected_statuses=("waiting_worker",), status="cancelled", result=WorkerResult(task_id=task_id, status="cancelled", error="handoff_cancelled"))
        return True


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
        result = WorkerResult(task_id=task.task_id, status="unavailable", error="docker_worker_adapter_not_configured", retryable=True, metadata={"worker_kind": self.kind})
        self.records.transition(task.task_id, expected_statuses=(prepared.status,), status="unavailable", result=result)
        return result

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        return self.records.reconcile_kind(self.kind, idempotency_key)

    def cancel(self, task_id: str) -> bool:
        return False


class WorkerManager:
    _ALIASES = {"codex": "codex_handoff", "local": "local", "mcp": "mcp", "docker": "docker"}

    def __init__(
        self,
        workers: Mapping[str, WorkerPort | Callable[[], WorkerPort]] | None = None,
        *,
        factories: Mapping[str, Callable[[], WorkerPort]] | None = None,
        capabilities: Mapping[str, tuple[str, ...]] | None = None,
        records: Any | None = None,
    ) -> None:
        self.workers: dict[str, WorkerPort] = {}
        self._lock = threading.RLock()
        self._factories: dict[str, Callable[[], WorkerPort]] = {
            self._canonical_kind(str(kind)): factory
            for kind, factory in (factories or {}).items()
        }
        self._capability_map = {
            self._canonical_kind(str(kind)): tuple(str(item) for item in values)
            for kind, values in (capabilities or {}).items()
        }
        for kind, worker in (workers or {}).items():
            canonical = self._canonical_kind(str(kind))
            # A WorkerPort is identified by execute; callable factories are lazy.
            if hasattr(worker, "execute"):
                self.workers[canonical] = worker  # type: ignore[assignment]
            elif callable(worker):
                self._factories[canonical] = worker
            else:
                raise TypeError(f"worker_registration_invalid:{kind}")
        self.records = records

    @property
    def loaded_kinds(self) -> tuple[str, ...]:
        with self._lock:
            return tuple(sorted(self.workers))

    @property
    def registered_kinds(self) -> tuple[str, ...]:
        with self._lock:
            return tuple(sorted(set(self.workers) | set(self._factories)))

    def _canonical_kind(self, kind: str) -> str:
        return self._ALIASES.get(kind, kind)

    def _resolve(self, kind: str) -> WorkerPort | None:
        canonical = self._canonical_kind(kind)
        with self._lock:
            worker = self.workers.get(canonical)
            if worker is not None:
                return worker
            factory = self._factories.get(canonical)
            if factory is None:
                return None
            worker = factory()
            self.workers[canonical] = worker
            return worker

    def capabilities(self) -> tuple[str, ...]:
        with self._lock:
            offered = set(self._capability_map.values())
            loaded = tuple(self.workers.values())
        for worker in loaded:
            offered.add(tuple(worker.capabilities()))
        return tuple(sorted({item for group in offered for item in group}))

    def execute(self, task: WorkerTask) -> WorkerResult:
        kind = str(task.metadata.get("worker_kind") or task.capability.partition(".")[0]).strip()
        worker = self._resolve(kind)
        if worker is None:
            return WorkerResult(
                task_id=task.task_id,
                status="unavailable",
                error=f"worker_capability_unavailable:{task.capability}",
                retryable=True,
            )
        return worker.execute(task)

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        kinds: tuple[str, ...] = ()
        finder = getattr(self.records, "kinds_for_idempotency", None)
        if callable(finder):
            kinds = tuple(finder(idempotency_key))
        candidates = kinds or self.registered_kinds
        matches = tuple(
            result
            for kind in candidates
            if (worker := self._resolve(kind)) is not None
            if (result := worker.reconcile(idempotency_key)) is not None
        )
        return matches[0] if len(matches) == 1 else None

    def cancel(self, task_id: str, run_id: str = "") -> bool:
        if run_id and self.records is not None:
            record = self.records.get_for_run(task_id, run_id)
            if record is None:
                return False
            worker = self._resolve(record.worker_kind)
            return worker.cancel(task_id) if worker is not None else False
        return any(
            worker.cancel(task_id)
            for kind in self.registered_kinds
            if (worker := self._resolve(kind)) is not None
        )

    def restart(self, kind: str) -> bool:
        canonical = self._canonical_kind(kind)
        with self._lock:
            worker = self.workers.pop(canonical, None)
        if worker is None:
            with self._lock:
                return canonical in self._factories
        close = getattr(worker, "close", None)
        if callable(close):
            close()
        return True

    def close(self) -> None:
        with self._lock:
            workers = tuple(self.workers.values())
            self.workers.clear()
        for worker in workers:
            close = getattr(worker, "close", None)
            if callable(close):
                close()


__all__ = ["CodexHandoffWorker", "DockerWorkerAdapter", "WorkerManager"]
