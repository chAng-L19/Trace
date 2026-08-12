from __future__ import annotations

from pathlib import Path
from typing import Any, Mapping

from ..adapters.runtime_mapping import (
    evidence_from_runtime,
    goal_from_runtime,
    run_from_runtime,
    terminal_from_runtime,
)
from ..core import Event, ModelPort, ToolPort, WorkerPort, WorkerResult, WorkerTask
from ..runtime.durable_store import StateVersionConflict, StoreConflictError
from ..runtime.worker_store import WorkerStore
from ..runtime.operation_result import OperationResult
from ..runtime.operation_runtime import OperationRuntime
from ..workers import (
    CodexHandoffWorker,
    DockerWorkerAdapter,
    LocalWorker,
    McpWorker,
    WorkerManager,
    WorkspaceManager,
)
from .contracts import (
    AgentRunView,
    AgentStartResult,
    BudgetDelta,
    Observation,
    StartRequest,
)
from .lifecycle import validate_run_transition
from .model_loop import ModelLoop
from .context import ContextSelection, ContextSelector, ConversationLedger, TraceableCompactor


class AgentService:
    """The canonical application entry point for durable agent operations."""

    def __init__(
        self,
        *,
        root: Path | None = None,
        runtime: OperationRuntime | None = None,
        model_port: ModelPort | None = None,
        tool_port: ToolPort | None = None,
        worker_port: WorkerPort | None = None,
        model_name: str = "",
        model_streaming: bool = False,
        model_max_retries: int = 2,
        model_max_turns: int = 8,
    ) -> None:
        if runtime is None and root is None:
            raise ValueError("agent_service_root_required")
        if runtime is not None and root is not None and runtime.root.resolve() != root.resolve():
            raise ValueError("agent_service_runtime_root_mismatch")
        if runtime is not None:
            self.runtime = runtime
        else:
            assert root is not None
            self.runtime = OperationRuntime(root=root)
        self.conversation = ConversationLedger(self.runtime.store, self.runtime.artifacts)
        self.context_compactor = TraceableCompactor(self.runtime.store)
        self.context_selector = ContextSelector(self, self.conversation, self.context_compactor)
        self.worker_records = WorkerStore(self.runtime.store)
        self.workspaces = WorkspaceManager(self.runtime.root, self.runtime.store)
        if worker_port is not None:
            self.workers = worker_port
        else:
            workers: dict[str, WorkerPort] = {
                "local": LocalWorker(
                    workspaces=self.workspaces,
                    artifacts=self.runtime.artifacts,
                    records=self.worker_records,
                ),
                "codex_handoff": CodexHandoffWorker(records=self.worker_records),
                "docker": DockerWorkerAdapter(records=self.worker_records),
            }
            if tool_port is not None:
                workers["mcp"] = McpWorker(
                    tools=tool_port,
                    artifacts=self.runtime.artifacts,
                    records=self.worker_records,
                )
            self.workers = WorkerManager(workers)
        self.model_loop = (
            ModelLoop(
                service=self,
                model=model_port,
                tools=tool_port,
                model_name=model_name,
                streaming=model_streaming,
                max_retries=model_max_retries,
                max_turns=model_max_turns,
            )
            if model_port is not None
            else None
        )

    @staticmethod
    def _view(result: OperationResult) -> AgentRunView:
        return AgentRunView(
            run=run_from_runtime(result.state),
            goal=goal_from_runtime(result.state.goal),
            evidence=tuple(evidence_from_runtime(item) for item in result.evidence),
            terminal=terminal_from_runtime(result.terminal),
            next_action=result.next_action,
            missing_capabilities=tuple(result.missing_capabilities),
            handoff=dict(result.handoff),
        )

    def _settle_control_conflict(self, run_id: str, error: BaseException) -> AgentRunView:
        text = str(error)
        if not (
            text.startswith("operation_cancel_requested:")
            or text.startswith("state_version_conflict:")
        ):
            raise error
        state = self.runtime.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        if state.cancel_reason or state.status in {"cancelling", "cancelled"}:
            try:
                return self._view(
                    self.runtime.cancel(run_id, reason=state.cancel_reason or "cancel_requested")
                )
            except ValueError as exc:
                if not str(exc).startswith("operation_terminal:"):
                    raise
        return self._view(self.runtime.status(run_id))

    @staticmethod
    def _validate_result(previous: str, view: AgentRunView) -> AgentRunView:
        validate_run_transition(previous, view.run.status)
        if view.run.status in {"completed", "failed", "cancelled"} and not view.terminal.terminal:
            raise ValueError(f"terminal_decision_missing:{view.run.status}")
        if view.run.status == "completed" and not view.terminal.success:
            raise ValueError("completed_run_requires_successful_terminal_decision")
        return view

    def start(self, request: StartRequest | Mapping[str, Any]) -> AgentStartResult:
        resolved = StartRequest.from_value(request)
        states = self.runtime.start_batch(**resolved.runtime_arguments())
        views = tuple(
            self._validate_result("created", self._view(self.runtime.status(state.run_id)))
            for state in states
        )
        batch_ids = {
            str(state.goal.starting_context.get("batch_session_id") or "")
            for state in states
            if state.goal.starting_context.get("batch_session_id")
        }
        if len(batch_ids) > 1:
            raise ValueError("batch_identity_mismatch")
        for view in views:
            self.conversation.record_start(view, resolved)
        return AgentStartResult(runs=views, batch_id=next(iter(batch_ids), ""))

    def run(
        self,
        run_id: str,
        budget_delta: BudgetDelta | Mapping[str, Any] | None = None,
        *,
        max_actions: int | None = None,
    ) -> AgentRunView:
        before = self.status(run_id)
        delta = BudgetDelta.from_value(budget_delta)
        if delta.changes_budget and before.run.status in {"completed", "failed", "cancelled"}:
            raise ValueError(f"operation_terminal:{before.run.status}")
        if delta.changes_budget:
            arguments = {
                "actions": delta.actions,
                "tokens": delta.tokens,
                "time_seconds": delta.time_seconds,
                "deadline": delta.deadline,
                "acknowledge_missing_usage": delta.acknowledge_missing_usage,
            }
            if delta.idempotency_key:
                self.runtime.apply_budget_delta_once(
                    run_id,
                    idempotency_key=delta.idempotency_key,
                    **arguments,
                )
            else:
                self.runtime.apply_budget_delta(run_id, **arguments)
        if self.model_loop is not None:
            return self.model_loop.run(run_id, max_actions=max_actions)
        return self._resume_runtime(run_id, max_actions=max_actions)

    def _resume_runtime(self, run_id: str, *, max_actions: int | None = None) -> AgentRunView:
        before = self.status(run_id)
        try:
            view = self._view(self.runtime.resume(run_id, max_actions=max_actions))
        except (StateVersionConflict, StoreConflictError) as exc:
            view = self._settle_control_conflict(run_id, exc)
        return self._validate_result(before.run.status, view)

    def _enforce_runtime_budget(self, run_id: str) -> AgentRunView:
        before = self.status(run_id)
        view = self._view(self.runtime.enforce_budget(run_id))
        return self._validate_result(before.run.status, view)

    def _record_model_usage(
        self,
        run_id: str,
        request_id: str,
        usage: Mapping[str, Any],
    ) -> AgentRunView:
        before = self.status(run_id)
        view = self._view(
            self.runtime.record_model_usage(
                run_id,
                request_id=request_id,
                usage=usage,
            )
        )
        return self._validate_result(before.run.status, view)

    def submit_observation(
        self,
        run_id: str,
        observation: Observation | Mapping[str, Any],
    ) -> AgentRunView:
        return self._submit_runtime_observation(run_id, Observation.from_value(observation))

    def _submit_runtime_observation(
        self,
        run_id: str,
        resolved: Observation,
    ) -> AgentRunView:
        before = self.status(run_id)
        arguments = {
            "run_id": run_id,
            "action_id": resolved.action_id,
            "output": resolved.output,
            "tool": resolved.tool,
            "usage": dict(resolved.usage),
            "continue_run": resolved.continue_run,
            "max_actions": resolved.max_actions,
        }
        try:
            if resolved.has_handoff_receipt:
                result = self.runtime.submit_handoff_observation(
                    handoff_id=resolved.handoff_id,
                    handoff_token=resolved.handoff_token,
                    attempt_id=resolved.attempt_id,
                    contract_hash=resolved.contract_hash,
                    **arguments,
                )
            else:
                result = self.runtime.submit_observation(
                    idempotency_key=resolved.idempotency_key,
                    **arguments,
                )
            view = self._view(result)
        except (StateVersionConflict, StoreConflictError) as exc:
            view = self._settle_control_conflict(run_id, exc)
        return self._validate_result(before.run.status, view)

    def status(self, run_id: str) -> AgentRunView:
        return self._view(self.runtime.status(run_id))

    def cancel(self, run_id: str, reason: str = "user_requested") -> AgentRunView:
        before = self.status(run_id)
        if self.model_loop is not None:
            self.model_loop.cancel(run_id)
        try:
            view = self._view(self.runtime.cancel(run_id, reason=reason))
        except (StateVersionConflict, StoreConflictError) as exc:
            view = self._settle_control_conflict(run_id, exc)
        return self._validate_result(before.run.status, view)

    def events(
        self,
        run_id: str,
        after_sequence: int = 0,
        *,
        limit: int = 200,
    ) -> tuple[Event, ...]:
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return tuple(
            Event(
                run_id=run_id,
                event_type=str(item["event_type"]),
                payload=dict(item["payload"]),
                sequence=int(item["event_id"]),
                created_at=str(item["created_at"]),
            )
            for item in self.runtime.store.events(
                run_id,
                after_event_id=after_sequence,
                limit=limit,
            )
        )

    def transcript(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.conversation.messages(run_id)

    def select_context(self, run_id: str, *, max_messages: int = 32) -> ContextSelection:
        return self.context_selector.select(self.status(run_id), max_messages=max_messages)

    def compact_context(self, run_id: str, message_ids: tuple[str, ...] = ()):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.context_compactor.compact(run_id, message_ids)

    def execute_worker(self, task: WorkerTask | Mapping[str, Any]) -> WorkerResult:
        resolved = task if isinstance(task, WorkerTask) else WorkerTask.from_dict(task)
        if self.runtime.store.load_operation(resolved.run_id) is None:
            raise KeyError(f"operation_not_found:{resolved.run_id}")
        for artifact_id in resolved.required_artifacts:
            if self.runtime.artifacts.get_ref(artifact_id, run_id=resolved.run_id) is None:
                raise ValueError(f"worker_required_artifact_missing:{artifact_id}")
        return self.workers.execute(resolved)

    def worker_status(self, task_id: str):
        return self.worker_records.get(task_id)

    def worker_results(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.worker_records.records(run_id)

    def cancel_worker(self, task_id: str) -> bool:
        return self.workers.cancel(task_id)

    def artifact(self, run_id: str, artifact_id: str):
        ref = self.runtime.artifacts.get_ref(artifact_id, run_id=run_id)
        if ref is None:
            raise KeyError(f"artifact_not_found:{artifact_id}")
        return ref

    def read_artifact(self, run_id: str, artifact_id: str) -> bytes:
        return self.runtime.artifacts.read(artifact_id, run_id=run_id)

    def artifacts(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.runtime.artifacts.refs(run_id)

    def search_artifacts(self, run_id: str, query: str, *, limit: int = 20):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.runtime.artifacts.search(run_id, query, limit=limit)
