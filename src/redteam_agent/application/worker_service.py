from __future__ import annotations

from typing import Any, Mapping

from ..core import WorkerResult, WorkerTask, contract_hash
from ..runtime.worker_store import WORKER_BLOCKED_RUN_STATUSES
from ..runtime.durable_store import StoreConflictError
from ..workers import WorkerManager
from .bounded_output import BoundedOutput
from .service_execution import service_write


class WorkerServiceMixin:
    """Run-scoped worker execution, observations and explicit result settlement."""

    @service_write
    def execute_worker(self, task: WorkerTask | Mapping[str, Any]) -> WorkerResult:
        resolved = task if isinstance(task, WorkerTask) else WorkerTask.from_dict(task)
        with self._model_condition:
            self._ensure_open()
            self._active_runs[resolved.run_id] = self._active_runs.get(resolved.run_id, 0) + 1
        try:
            return self._execute_worker(resolved)
        finally:
            with self._model_condition:
                self._active_runs[resolved.run_id] -= 1
                if not self._active_runs[resolved.run_id]:
                    del self._active_runs[resolved.run_id]
                self._model_condition.notify_all()

    def _execute_worker(self, resolved: WorkerTask) -> WorkerResult:
        state = self.runtime.store.load_operation(resolved.run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{resolved.run_id}")
        if state.status in WORKER_BLOCKED_RUN_STATUSES:
            existing = self.worker_records.get_for_run(resolved.task_id, resolved.run_id)
            if existing is None or existing.result is None:
                raise ValueError(f"worker_run_not_executable:{resolved.run_id}:{state.status}")
        for artifact_id in resolved.required_artifacts:
            try:
                self.runtime.artifacts.verify(artifact_id, run_id=resolved.run_id)
            except KeyError:
                raise ValueError(f"worker_required_artifact_missing:{artifact_id}")
        result = self.workers.execute(resolved)
        for artifact_id in result.artifact_refs:
            self.runtime.artifacts.verify(artifact_id, run_id=resolved.run_id)
        self._record_worker_observation(resolved, result)
        return result

    def _record_worker_observation(self, task: WorkerTask, result: WorkerResult) -> None:
        self.worker_records.record_observation(task.run_id, self._worker_observation(task, result))

    @staticmethod
    def _worker_observation(task: WorkerTask, result: WorkerResult) -> Mapping[str, Any]:
        """Persist one bounded worker result projection for later model turns."""
        bounded = BoundedOutput.capture_json(result.output)
        try:
            output_projection = bounded.preview()
        finally:
            bounded.discard()
        result_projection = {
            **result.to_dict(),
            "output": output_projection,
            "metadata": {
                **dict(result.metadata),
                "complete_output_artifacts": list(result.artifact_refs),
            },
        }
        payload = {
            "task_id": task.task_id,
            "worker_kind": str(task.metadata.get("worker_kind") or task.capability.partition(".")[0]),
            "action_id": str(task.metadata.get("action_id") or ""),
            "idempotency_key": task.idempotency_key,
            "result": result_projection,
        }
        observation_hash = contract_hash(payload)
        observation_id = contract_hash({"task_id": task.task_id, "status": result.status})
        return {**payload, "observation_hash": observation_hash, "observation_id": observation_id}

    def worker_observations(self, run_id: str) -> tuple[Mapping[str, Any], ...]:
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return tuple(
            dict(item["payload"])
            for item in self.runtime.store.events(run_id)
            if item["event_type"] == "worker_observation_recorded"
        )
    def worker_status(self, run_id: str, task_id: str):
        record = self.worker_records.get_for_run(task_id, run_id)
        if record is None:
            raise KeyError(f"worker_task_not_found:{run_id}:{task_id}")
        return record

    def worker_results(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.worker_records.records(run_id)

    @service_write
    def cancel_worker(self, run_id: str, task_id: str) -> bool:
        return self._cancel_worker(run_id, task_id)

    def _cancel_worker(self, run_id: str, task_id: str) -> bool:
        if self.worker_records.get_for_run(task_id, run_id) is None:
            raise KeyError(f"worker_task_not_found:{run_id}:{task_id}")
        if isinstance(self.workers, WorkerManager):
            accepted = self.workers.cancel(task_id, run_id)
        else:
            accepted = self.workers.cancel(task_id)
        record = self.worker_status(run_id, task_id)
        if accepted and record.status in {"prepared", "waiting_worker"}:
            try:
                record = self.worker_records.transition(
                    task_id, expected_statuses=(record.status,), status="cancelled",
                    result=WorkerResult(task_id, "cancelled", error="worker_cancelled"),
                )
            except StoreConflictError:
                record = self.worker_status(run_id, task_id)
        if accepted and record.result is not None and record.status == "cancelled":
            self._record_worker_observation(record.task, record.result)
        return accepted

    def _cancel_run_workers(self, run_id: str, *, include_waiting: bool = True) -> None:
        statuses = ("prepared", "running", "waiting_worker") if include_waiting else ("prepared", "running")
        tasks = self.worker_records.request_cancel_run(run_id, statuses=statuses)
        errors = []
        for task_id in tasks:
            try:
                self._cancel_worker(run_id, task_id)
            except Exception as exc:
                errors.append(exc)
        if errors:
            raise ExceptionGroup("worker_cancel_failed", errors)

    @service_write
    def settle_worker(self, run_id: str, submission: Mapping[str, Any]) -> WorkerResult:
        """Settle an interrupted worker without repeating unknown side effects."""
        return self._settle_worker_result(run_id, submission, expected_status="unknown")

    @service_write
    def submit_worker_result(self, run_id: str, submission: Mapping[str, Any]) -> WorkerResult:
        """Accept a Codex result bound to the persisted task and handoff receipt.

        Artifacts must already exist in this run's CAS, with matching task_id and
        worker_kind metadata. Supply their SHA-256 hashes and contract_hash(output).
        Legacy handoffs lacking handoff_id use contract_hash(waiting_result.to_dict()).
        This records an observation, without promoting host claims to evidence.
        """
        return self._settle_worker_result(run_id, submission, expected_status="waiting_worker")

    def _settle_worker_result(
        self, run_id: str, submission: Mapping[str, Any], *, expected_status: str,
    ) -> WorkerResult:
        required = {"task_id", "worker_kind", "idempotency_key", "input_hash", "result", "reason"}
        allowed = required | {"handoff_id", "output_hash", "artifact_hashes"}
        if not isinstance(submission, Mapping) or not required.issubset(submission) or set(submission) - allowed:
            raise ValueError("worker_submission_fields_invalid")
        record = self.worker_status(run_id, str(submission["task_id"]))
        if any(not isinstance(submission[key], str) or not submission[key].strip()
               for key in required - {"result"}):
            raise ValueError("worker_submission_identity_required")
        if (submission["worker_kind"] != record.worker_kind
                or submission["idempotency_key"] != record.task.idempotency_key
                or submission["input_hash"] != record.input_hash):
            raise ValueError("worker_result_identity_conflict")
        if not isinstance(submission["result"], Mapping):
            raise ValueError("worker_result_mapping_required")
        result = WorkerResult.from_dict(submission["result"])
        if result.task_id != record.task.task_id:
            raise ValueError("worker_result_task_mismatch")
        if expected_status == "unknown" and record.worker_kind not in {"local", "mcp"}:
            raise ValueError("worker_unknown_kind_invalid")
        output_hash = submission.get("output_hash", "")
        artifact_hashes = submission.get("artifact_hashes", {})
        if not isinstance(artifact_hashes, Mapping) or set(artifact_hashes) != set(result.artifact_refs):
            raise ValueError("worker_result_artifact_hashes_required")
        if len(result.artifact_refs) > 100:
            raise ValueError("worker_result_artifacts_too_many")
        if result.status == "completed" and (not result.artifact_refs or not output_hash):
            raise ValueError("worker_completed_evidence_required")
        if output_hash and output_hash != contract_hash(result.output):
            raise ValueError("worker_result_output_hash_mismatch")
        workspace = self.workspaces.ensure(run_id)
        # Only existing CAS references are accepted; submitted paths are never imported.
        for metadata in (record.task.metadata, result.metadata):
            if metadata.get("workspace_key", workspace.workspace_key) != workspace.workspace_key:
                raise ValueError("worker_result_workspace_mismatch")
            if metadata.get("worker_kind", record.worker_kind) not in {record.worker_kind, "codex" if record.worker_kind == "codex_handoff" else record.worker_kind}:
                raise ValueError("worker_result_kind_mismatch")
        if result.status == "completed":
            self.workspaces.resolve(workspace, str(record.task.payload.get("cwd") or "."))
        for artifact_id in result.artifact_refs:
            ref = self.runtime.artifacts.get_ref(artifact_id, run_id=run_id)
            if ref is None:
                raise ValueError(f"worker_result_artifact_missing:{artifact_id}")
            if (artifact_hashes[artifact_id] != ref.content_hash
                    or ref.metadata.get("task_id") != record.task.task_id
                    or ref.metadata.get("worker_kind") != record.worker_kind
                    or ref.metadata.get("workspace_key", workspace.workspace_key) != workspace.workspace_key
                    or ref.metadata.get("provider_private")):
                raise ValueError(f"worker_result_artifact_identity_mismatch:{artifact_id}")
            self.runtime.artifacts.verify(artifact_id, run_id=run_id)
        if result.status == "completed":
            for artifact_id in record.task.required_artifacts:
                self.runtime.artifacts.verify(artifact_id, run_id=run_id)
        return self.worker_records.settle_result(
            record.task, worker_kind=str(submission["worker_kind"]),
            input_hash=str(submission["input_hash"]), result=result,
            expected_status=expected_status, reason=str(submission["reason"]),
            handoff_id=str(submission.get("handoff_id") or ""),
            output_hash=str(output_hash), artifact_hashes=artifact_hashes,
            observation=self._worker_observation,
        )
