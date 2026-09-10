from __future__ import annotations

from ..application.bounded_output import BoundedOutput
from ..core import ToolCall, ToolPort, WorkerResult, WorkerTask, contract_hash
from ..runtime.artifact_store import ArtifactStore
from ..runtime.worker_store import WORKER_TERMINAL_STATUSES, WorkerStore
from ..runtime.security import safe_error_text


class McpWorker:
    kind = "mcp"

    def __init__(self, *, tools: ToolPort, artifacts: ArtifactStore, records: WorkerStore) -> None:
        self.tools = tools
        self.artifacts = artifacts
        self.records = records

    def capabilities(self) -> tuple[str, ...]:
        return tuple(f"mcp.{item.name}" for item in self.tools.discover())

    def execute(self, task: WorkerTask) -> WorkerResult:
        prepared = self.records.prepare(task, worker_kind=self.kind, owner="mcp-worker")
        if prepared.result is not None and prepared.status in WORKER_TERMINAL_STATUSES:
            return prepared.result
        recovering = prepared.status == "running"
        if prepared.status == "unknown":
            return prepared.result or WorkerResult(
                task_id=task.task_id,
                status="unknown",
                error="worker_interrupted_requires_reconcile",
                retryable=True,
            )
        if prepared.status not in {"prepared", "running"}:
            raise RuntimeError(f"worker_task_already_active:{task.task_id}:{prepared.status}")
        if not recovering:
            self.records.transition(
                task.task_id,
                expected_statuses=(prepared.status,),
                status="running",
            )
        tool_name = str(task.payload.get("tool_name") or task.capability.removeprefix("mcp.")).strip()
        arguments = task.payload.get("arguments")
        if not isinstance(arguments, dict):
            arguments = dict(task.payload)
            arguments.pop("tool_name", None)
        call = ToolCall(
            call_id=task.task_id,
            run_id=task.run_id,
            tool_name=tool_name,
            arguments=arguments,
            idempotency_key=task.idempotency_key,
            timeout_seconds=task.timeout_seconds,
            metadata={"worker_kind": self.kind},
        )
        try:
            tool_result = self.tools.reconcile(call)
            if tool_result is None and recovering:
                unknown = self.records.mark_interrupted_unknown(task.task_id, owner="mcp-worker")
                return unknown.result  # type: ignore[return-value]
            if tool_result is None:
                tool_result = self.tools.invoke(call)
        except Exception as exc:
            result = WorkerResult(
                task_id=task.task_id,
                status="failed",
                error=f"mcp_worker_error:{safe_error_text(exc)}",
                retryable=True,
            )
        else:
            if tool_result.call_id != call.call_id or tool_result.tool_name != call.tool_name:
                result = WorkerResult(
                    task_id=task.task_id,
                    status="failed",
                    error="mcp_worker_result_identity_mismatch",
                )
            else:
                try:
                    bounded = BoundedOutput.capture_json(tool_result.to_dict())
                    try:
                        bounded.close()
                        artifact = self.artifacts.put_file(
                            bounded.path,
                            run_id=task.run_id,
                            artifact_type="mcp_tool_result",
                            media_type="application/json",
                            preview={
                                "status": tool_result.status,
                                "tool_name": tool_result.tool_name,
                                "output_hash": contract_hash(tool_result.output),
                                **bounded.preview(),
                            },
                            metadata={"task_id": task.task_id, "tool_name": tool_name},
                            parents=task.required_artifacts,
                        )
                    finally:
                        bounded.discard()
                except Exception as exc:
                    result = WorkerResult(
                        task_id=task.task_id,
                        status="failed",
                        error=f"mcp_worker_artifact_error:{safe_error_text(exc)}",
                        retryable=True,
                    )
                else:
                    status = "completed" if tool_result.status == "success" else "failed"
                    result = WorkerResult(
                        task_id=task.task_id,
                        status=status,
                        output=self.artifacts.project(artifact),
                        artifact_refs=(artifact.artifact_id,),
                        error=tool_result.error,
                        retryable=tool_result.retryable,
                        metadata={"worker_kind": self.kind, "tool_name": tool_name},
                    )
        self.records.transition(
            task.task_id,
            expected_statuses=("running",),
            status=result.status,
            result=result,
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
        record = self.records._from_row(rows[0])
        return record.result if record.status in WORKER_TERMINAL_STATUSES else None

    def cancel(self, task_id: str) -> bool:
        return self.tools.cancel(task_id)


__all__ = ["McpWorker"]
