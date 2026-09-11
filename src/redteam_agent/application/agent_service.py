from __future__ import annotations

from pathlib import Path
from typing import Any, Mapping, Sequence

from ..adapters.runtime_mapping import (
    evidence_from_runtime,
    goal_from_runtime,
    run_from_runtime,
    terminal_from_runtime,
)
from ..adapters.runtime import RuntimeToolAdapter
from ..core import Event, ModelPort, ToolPort, WorkerPort, WorkerResult, WorkerTask, contract_hash
from ..runtime.durable_store import StateVersionConflict, StoreConflictError
from ..runtime.worker_store import WorkerStore
from ..runtime.operation_result import OperationResult
from ..runtime.operation_runtime import OperationRuntime
from ..runtime.exploration import ExplorationLedger
from ..runtime.session_journal import SessionJournal
from ..runtime.tool_registry import ToolRegistry
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
from .bounded_output import BoundedOutput
from .lifecycle import validate_run_transition
from .model_loop import AgentLoop
from .context import ContextSelection, ContextSelector, ConversationLedger, TraceableCompactor
from .resources import ResourceIndex, ResourceResolver, ResourceSelection


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
        self.resources = ResourceResolver()
        resolved_tool_port = ToolRegistry(
            tool_port or RuntimeToolAdapter(self.runtime),
            store=self.runtime.store,
        )
        self.tools = resolved_tool_port
        self.journal = SessionJournal(self.runtime.store)
        self.exploration = ExplorationLedger(
            self.runtime.store,
            self.runtime.artifacts,
            self.runtime.evidence_graph,
            journal=self.journal,
        )
        self.conversation = ConversationLedger(
            self.runtime.store,
            self.runtime.artifacts,
            journal=self.journal,
        )
        self.context_compactor = TraceableCompactor(self.runtime.store, journal=self.journal)
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
            }
            workers["mcp"] = McpWorker(
                tools=resolved_tool_port,
                artifacts=self.runtime.artifacts,
                records=self.worker_records,
            )
            self.workers = WorkerManager(
                workers,
                factories={
                    "codex_handoff": lambda: CodexHandoffWorker(records=self.worker_records),
                    "docker": lambda: DockerWorkerAdapter(records=self.worker_records),
                },
                capabilities={"codex_handoff": ("codex.handoff",)},
                records=self.worker_records,
            )
        self.agent_loop = (
            AgentLoop(
                service=self,
                model=model_port,
                tools=resolved_tool_port,
                model_name=model_name,
                streaming=model_streaming,
                max_retries=model_max_retries,
                max_turns=model_max_turns,
            )
            if model_port is not None
            else None
        )

    @property
    def model_loop(self) -> AgentLoop | None:
        return self.agent_loop

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

    def summary(self, run_id: str) -> dict[str, Any]:
        self.status(run_id)
        return self.runtime.status(run_id).summary()

    def bind_credentials(self, run_id: str, bindings: Mapping[str, str]) -> None:
        self.runtime.bind_credentials(run_id, bindings)

    def apply_budget_delta(self, run_id: str, **delta: Any) -> AgentRunView:
        before = self.status(run_id)
        view = self._view(self.runtime.apply_budget_delta(run_id, **delta))
        return self._validate_result(before.run.status, view)

    def apply_budget_delta_batch(self, run_ids: list[str], **delta: Any) -> None:
        self.runtime.apply_budget_delta_batch(run_ids, **delta)

    def provide_target(self, run_id: str, targets: Sequence[str]) -> AgentRunView:
        before = self.status(run_id)
        self.runtime.provide_target(run_id, targets=targets)
        view = self.status(run_id)
        return self._validate_result(before.run.status, view)

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

    def tool_catalog(self, run_id: str, *, capabilities: tuple[str, ...] = (), profile: str = ""):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.tools.catalog(run_id, capabilities=capabilities, profile=profile)

    def expand_tools(self, run_id: str, selectors: tuple[str, ...] = ()):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.tools.expand(run_id, selectors)

    def refresh_tools(self, *, force: bool = False):
        return self.tools.refresh(force=force)

    def session_entries(self, run_id: str):
        return self.journal.entries(run_id)

    def session_tree(self, run_id: str):
        return self.journal.tree(run_id)

    def export_session(self, run_id: str):
        return self.journal.export(run_id)

    def replay_session(self, run_id: str, leaf_id: str | None = None):
        return self.journal.replay(run_id, leaf_id)

    def branch_session(
        self,
        run_id: str,
        from_entry_id: str,
        *,
        expected_leaf_id: str | None = None,
    ):
        return self.journal.branch(
            run_id,
            from_entry_id,
            expected_leaf_id=expected_leaf_id,
        )

    def fork_session(self, run_id: str, from_entry_id: str, branch_id: str):
        return self.journal.fork(run_id, from_entry_id, branch_id)

    def checkout_session(self, run_id: str, branch_id: str):
        return self.journal.checkout(run_id, branch_id)

    def select_context(self, run_id: str, *, max_messages: int = 32) -> ContextSelection:
        return self.context_selector.select(
            self.status(run_id),
            max_messages=max_messages,
            turn_boundary=True,
        )

    def resource_index(self, run_id: str) -> ResourceIndex:
        view = self.status(run_id)
        roots = view.goal.constraints.get("resource_roots", ())
        if isinstance(roots, (str, bytes)) or not isinstance(roots, Sequence):
            roots = ()
        return self.resources.index(roots)

    def resource_selection(
        self,
        run_id: str,
        *,
        token_budget: int = 4096,
    ) -> ResourceSelection:
        view = self.status(run_id)
        constraints = view.goal.constraints
        requested = constraints.get("resources", ())
        disabled = constraints.get("disabled_resources", ())
        requested_values = () if isinstance(requested, (str, bytes)) else requested
        disabled_values = () if isinstance(disabled, (str, bytes)) else disabled
        if not isinstance(requested_values, Sequence):
            requested_values = ()
        if not isinstance(disabled_values, Sequence):
            disabled_values = ()
        return self.resources.select(
            self.resource_index(run_id),
            requested=tuple(str(item) for item in requested_values),
            disabled=tuple(str(item) for item in disabled_values),
            token_budget=token_budget,
        )

    def compact_context(self, run_id: str, message_ids: tuple[str, ...] = ()):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.context_compactor.compact(run_id, message_ids)

    def execute_worker(self, task: WorkerTask | Mapping[str, Any]) -> WorkerResult:
        resolved = task if isinstance(task, WorkerTask) else WorkerTask.from_dict(task)
        if self.runtime.store.load_operation(resolved.run_id) is None:
            raise KeyError(f"operation_not_found:{resolved.run_id}")
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
        self.runtime.store.append_event_once(
            task.run_id,
            "worker_observation_recorded",
            {**payload, "observation_hash": observation_hash},
            identity_field="task_id",
            fingerprint_field="observation_hash",
        )

    def worker_observations(self, run_id: str) -> tuple[Mapping[str, Any], ...]:
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return tuple(
            dict(item["payload"])
            for item in self.runtime.store.events(run_id)
            if item["event_type"] == "worker_observation_recorded"
        )

    def close(self) -> None:
        close = getattr(self.workers, "close", None)
        if callable(close):
            close()
        self.runtime.broker.close()

    def __enter__(self) -> "AgentService":
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.close()

    def worker_status(self, run_id: str, task_id: str):
        record = self.worker_records.get_for_run(task_id, run_id)
        if record is None:
            raise KeyError(f"worker_task_not_found:{run_id}:{task_id}")
        return record

    def worker_results(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.worker_records.records(run_id)

    def cancel_worker(self, run_id: str, task_id: str) -> bool:
        if self.worker_records.get_for_run(task_id, run_id) is None:
            raise KeyError(f"worker_task_not_found:{run_id}:{task_id}")
        if isinstance(self.workers, WorkerManager):
            return self.workers.cancel(task_id, run_id)
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

    def record_exploration(self, run_id: str, record: Mapping[str, Any]):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        payload = {**dict(record), "run_id": run_id}
        return self.exploration.record(payload)

    def exploration_records(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.journal.exploration_records(run_id)

    def exploration_state(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.exploration.projection(run_id)

    def recon_digest(self, run_id: str, *, source_message_ids: tuple[str, ...] = ()):
        return self.exploration.build_recon_digest(
            run_id,
            source_message_ids=source_message_ids,
        )

    def recon_digests(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.journal.recon_digests(run_id)
