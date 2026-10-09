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
        with self.records.execution(task, on_lease_lost=lambda: self.cancel(task.task_id)):
            return self._execute(task)

    def _execute(self, task: WorkerTask) -> WorkerResult:
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
            metadata={**dict(task.metadata), "worker_kind": self.kind},
        )
        definition = None
        invoking = False
        try:
            definition = next((item for item in self.tools.discover() if item.qualified_name == tool_name), None)
            tool_result = self.tools.reconcile(call)
            if self.records.cancel_requested(task.task_id):
                cancelled = self.records.transition(
                    task.task_id, expected_statuses=("running",), status="cancelled",
                    result=WorkerResult(task.task_id, "cancelled", error="worker_cancelled"),
                )
                assert cancelled.result is not None
                return cancelled.result
            if tool_result is None and recovering:
                unknown = self.records.mark_interrupted_unknown(task.task_id, owner="mcp-worker")
                return unknown.result  # type: ignore[return-value]
            if tool_result is None:
                invoking = True
                tool_result = self.tools.invoke(call)
        except Exception as exc:
            uncertain = bool(definition is not None and definition.side_effecting and (invoking or recovering))
            result = WorkerResult(
                task_id=task.task_id,
                status="unknown" if uncertain else "failed",
                error=f"mcp_worker_error:{safe_error_text(exc)}",
                retryable=True,
            )
        else:
            if tool_result.call_id != call.call_id or tool_result.tool_name != call.tool_name:
                result = WorkerResult(
                    task_id=task.task_id,
                    status="unknown" if definition is not None and definition.side_effecting else "failed",
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
                            metadata={"task_id": task.task_id, "worker_kind": self.kind, "tool_name": tool_name},
                            parents=task.required_artifacts,
                        )
                    finally:
                        bounded.discard()
                except Exception as exc:
                    result = WorkerResult(
                        task_id=task.task_id,
                        status="unknown" if definition is not None and definition.side_effecting else "failed",
                        error=f"mcp_worker_artifact_error:{safe_error_text(exc)}",
                        retryable=True,
                    )
                else:
                    uncertain = (tool_result.status != "success" and tool_result.retryable
                                 and definition is not None and definition.side_effecting)
                    status = "completed" if tool_result.status == "success" else "unknown" if uncertain else "failed"
                    capture_refs = []
                    if isinstance(tool_result.output, dict):
                        for reference in tool_result.output.get("artifact_refs", ()):
                            ref = self.artifacts.get_ref(str(reference), run_id=task.run_id)
                            if (ref is not None and not ref.metadata.get("provider_private")
                                    and ref.metadata.get("task_id") == task.task_id
                                    and ref.metadata.get("worker_kind") == self.kind):
                                capture_refs.append(ref.artifact_id)
                    result = WorkerResult(
                        task_id=task.task_id,
                        status=status,
                        output=self.artifacts.project(artifact),
                        artifact_refs=tuple(dict.fromkeys((artifact.artifact_id, *capture_refs))),
                        error=tool_result.error,
                        retryable=tool_result.retryable,
                        metadata={"worker_kind": self.kind, "tool_name": tool_name,
                                  "outcome_unknown": uncertain},
                    )
        saved = self.records.transition(
            task.task_id,
            expected_statuses=("running",),
            status=result.status,
            result=result,
        )
        assert saved.result is not None
        return saved.result

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        return self.records.reconcile_kind(self.kind, idempotency_key)

    def cancel(self, task_id: str) -> bool:
        if not self.records.request_cancel(task_id):
            return False
        self.tools.cancel(task_id)
        return True

    def close(self) -> None:
        close = getattr(self.tools, "close", None)
        if callable(close):
            close()


__all__ = ["McpWorker"]
