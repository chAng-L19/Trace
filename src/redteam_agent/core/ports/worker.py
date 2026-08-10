from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, ClassVar, Protocol, runtime_checkable

from ..contracts import (
    contract_version,
    json_mapping,
    json_value,
    optional_positive_float,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class WorkerTask:
    KIND: ClassVar[str] = "worker_task"

    task_id: str
    run_id: str
    capability: str
    payload: Mapping[str, Any]
    idempotency_key: str
    timeout_seconds: float | None = None
    required_artifacts: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "task_id": self.task_id,
                "run_id": self.run_id,
                "capability": self.capability,
                "payload": dict(self.payload),
                "idempotency_key": self.idempotency_key,
                "timeout_seconds": self.timeout_seconds,
                "required_artifacts": list(self.required_artifacts),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "WorkerTask":
        contract_version(payload, kind=cls.KIND)
        return cls(
            task_id=required_text(payload.get("task_id") or payload.get("id"), "worker_task_id"),
            run_id=required_text(payload.get("run_id"), "worker_task_run_id"),
            capability=required_text(payload.get("capability"), "worker_capability"),
            payload=json_mapping(payload.get("payload"), field="worker_task.payload"),
            idempotency_key=required_text(payload.get("idempotency_key"), "worker_idempotency_key"),
            timeout_seconds=optional_positive_float(payload.get("timeout_seconds"), field="worker_timeout"),
            required_artifacts=unique_strings(payload.get("required_artifacts")),
            metadata=json_mapping(payload.get("metadata"), field="worker_task.metadata"),
        )


@dataclass(frozen=True, slots=True)
class WorkerResult:
    KIND: ClassVar[str] = "worker_result"

    task_id: str
    status: str
    output: Any = None
    artifact_refs: tuple[str, ...] = ()
    error: str = ""
    retryable: bool = False
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "task_id": self.task_id,
                "status": self.status,
                "output": json_value(self.output, field="worker_result.output"),
                "artifact_refs": list(self.artifact_refs),
                "error": self.error,
                "retryable": self.retryable,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "WorkerResult":
        contract_version(payload, kind=cls.KIND)
        return cls(
            task_id=required_text(payload.get("task_id"), "worker_result_task_id"),
            status=required_text(payload.get("status"), "worker_result_status"),
            output=json_value(payload.get("output"), field="worker_result.output"),
            artifact_refs=unique_strings(payload.get("artifact_refs")),
            error=str(payload.get("error") or ""),
            retryable=bool(payload.get("retryable", False)),
            metadata=json_mapping(payload.get("metadata"), field="worker_result.metadata"),
        )


@runtime_checkable
class WorkerPort(Protocol):
    def capabilities(self) -> tuple[str, ...]: ...

    def execute(self, task: WorkerTask) -> WorkerResult: ...

    def reconcile(self, idempotency_key: str) -> WorkerResult | None: ...

    def cancel(self, task_id: str) -> bool: ...
