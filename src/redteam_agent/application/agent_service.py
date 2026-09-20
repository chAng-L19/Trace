from __future__ import annotations

import json
import sqlite3
import threading
from pathlib import Path
from typing import Any, Mapping, Sequence
from uuid import uuid4

from ..adapters.runtime import (
    LEGACY_TO_CORE_STATUS,
    evidence_from_runtime,
    goal_from_runtime,
    run_from_runtime,
    terminal_from_runtime,
    RuntimeToolAdapter,
)
from ..core import Event, ModelPort, ToolPort, WorkerPort, WorkerResult, WorkerTask, contract_hash
from ..runtime.durable_store import StateVersionConflict, StoreConflictError
from ..runtime.worker_store import WORKER_BLOCKED_RUN_STATUSES, WorkerStore
from ..runtime.terminal_judge import OperationResult
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
    validate_run_transition,
)
from .bounded_output import BoundedOutput
from .agent_tools import AgentToolAdapter
from .asset_graph import project_asset_attack_graph
from .model_loop import AgentLoop, ModelInterruptedError
from .context import ContextSelection, ContextSelector, ConversationLedger, TraceableCompactor
from .resources import ResourceIndex, ResourceResolver, ResourceSelection
from .transparency import TransparencyProjector
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
        self.transparency = TransparencyProjector(self)
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
        self._model_lock = threading.RLock()
        self._model_condition = threading.Condition(self._model_lock)
        self._active_model_loops: dict[str, dict[AgentLoop, int]] = {}
        resolved_tool_port.delegate = AgentToolAdapter(self, resolved_tool_port.delegate)
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
        with self._model_lock:
            return self.agent_loop

    def configure_model(
        self,
        model_port: ModelPort | None,
        *,
        model_name: str = "",
        streaming: bool = False,
        max_retries: int | None = None,
        max_turns: int | None = None,
    ) -> None:
        """Replace the provider used by future turns through the canonical service."""
        with self._model_lock:
            previous = self.agent_loop
            resolved_retries = previous.max_retries if max_retries is None and previous is not None else (2 if max_retries is None else max_retries)
            resolved_turns = previous.max_turns if max_turns is None and previous is not None else (8 if max_turns is None else max_turns)
            self.agent_loop = (
                AgentLoop(
                    service=self,
                    model=model_port,
                    tools=self.tools,
                    model_name=model_name,
                    streaming=streaming,
                    max_retries=resolved_retries,
                    max_turns=resolved_turns,
                )
                if model_port is not None
                else None
            )

    def _interrupt_model_loops(self, run_id: str, *, cancel: bool = False) -> None:
        with self._model_lock:
            loops = set(self._active_model_loops.get(run_id, ()))
            if self.agent_loop is not None:
                loops.add(self.agent_loop)
        for loop in loops:
            (loop.cancel if cancel else loop.interrupt)(run_id)

    def control_write(self, operation: Any, *args: Any, **kwargs: Any) -> Any:
        """Single application boundary for adapter-owned durable settings."""
        if not callable(operation):
            raise TypeError("control_operation_required")
        return operation(*args, **kwargs)

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
        states = self.runtime.start_batch(**resolved.runtime_arguments(), model_led=self.agent_loop is not None)
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
        with self._model_lock:
            loop = self.agent_loop
            if loop is not None:
                active = self._active_model_loops.setdefault(run_id, {})
                active[loop] = active.get(loop, 0) + 1
        if loop is None:
            return self._resume_runtime(run_id, max_actions=max_actions)
        try:
            return self._run_model_loop(loop, run_id, max_actions=max_actions)
        finally:
            with self._model_condition:
                active = self._active_model_loops[run_id]
                active[loop] -= 1
                if not active[loop]:
                    del active[loop]
                if not active:
                    del self._active_model_loops[run_id]
                self._model_condition.notify_all()

    def _run_model_loop(
        self,
        loop: AgentLoop,
        run_id: str,
        *,
        max_actions: int | None,
    ) -> AgentRunView:
        ttl = 30.0
        token = self.runtime.store.acquire_lease(
            run_id,
            "__model_loop__",
            f"agent-service:model-loop:{uuid4().hex}",
            ttl_seconds=ttl,
        )
        if token is None:
            return self.status(run_id)
        stopped = threading.Event()

        def renew() -> None:
            while not stopped.wait(ttl / 3):
                try:
                    if self.runtime.store.renew_lease(token, ttl_seconds=ttl) is not None:
                        continue
                except Exception:
                    pass
                loop.interrupt(run_id)
                return

        heartbeat = threading.Thread(target=renew, name="model-loop-lease", daemon=True)
        try:
            heartbeat.start()
            try:
                result = loop.run(run_id, max_actions=max_actions)
            except ModelInterruptedError:
                result = self.status(run_id)
                if result.run.status not in {"paused_budget", "cancelling", "cancelled"}:
                    raise
        finally:
            stopped.set()
            heartbeat.join(timeout=5)
            self.runtime.store.release_lease(token)
        state = self.runtime.store.load_operation(run_id)
        if state is not None and state.status == "cancelling":
            return self._view(
                self.runtime.cancel(run_id, reason=state.cancel_reason or "cancel_requested")
            )
        return result

    def _resume_runtime(
        self,
        run_id: str,
        *,
        max_actions: int | None = None,
        model_led: bool = False,
    ) -> AgentRunView:
        before = self.status(run_id)
        try:
            view = self._view(self.runtime.resume(run_id, max_actions=max_actions, model_led=model_led))
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
                    action_id=resolved.action_id,
                    idempotency_key=resolved.idempotency_key,
                    **arguments,
                )
            view = self._view(result)
        except (StateVersionConflict, StoreConflictError) as exc:
            view = self._settle_control_conflict(run_id, exc)
        return self._validate_result(before.run.status, view)

    def status(self, run_id: str) -> AgentRunView:
        return self._view(self.runtime.status(run_id))

    def list_runs(
        self,
        *,
        limit: int = 100,
        offset: int = 0,
        status: str = "",
    ) -> tuple[AgentRunView, ...]:
        """Return bounded run projections for CLI/Web consumers."""

        normalized_status = str(status or "").strip()
        runtime_statuses = tuple(
            dict.fromkeys(
                (
                    normalized_status,
                    *(
                        legacy
                        for legacy, canonical in LEGACY_TO_CORE_STATUS.items()
                        if canonical == normalized_status
                    ),
                )
            )
        ) if normalized_status else ()
        return tuple(
            self.status(state.run_id)
            for state in self.runtime.store.operations(
                limit=limit,
                offset=offset,
                statuses=runtime_statuses,
            )
        )

    def summary(self, run_id: str) -> dict[str, Any]:
        self.status(run_id)
        return self.runtime.status(run_id).summary()

    def bind_credentials(self, run_id: str, bindings: Mapping[str, str]) -> None:
        self.runtime.bind_credentials(run_id, bindings)

    def apply_budget_delta(self, run_id: str, **delta: Any) -> AgentRunView:
        before = self.status(run_id)
        view = self._view(self.runtime.apply_budget_delta(run_id, **delta))
        return self._validate_result(before.run.status, view)

    def apply_budget_delta_once(self, run_id: str, *, idempotency_key: str, **delta: Any) -> AgentRunView:
        before = self.status(run_id)
        view = self._view(self.runtime.apply_budget_delta_once(run_id, idempotency_key=idempotency_key, **delta))
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
        try:
            view = self._view(self.runtime.cancel(run_id, reason=reason))
        except (StateVersionConflict, StoreConflictError) as exc:
            view = self._settle_control_conflict(run_id, exc)
        self._interrupt_model_loops(run_id, cancel=True)
        if view.run.status == "cancelling":
            with self._model_condition:
                self._model_condition.wait_for(
                    lambda: run_id not in self._active_model_loops,
                    timeout=5,
                )
            state = self.runtime.store.load_operation(run_id)
            if state is not None and state.status == "cancelling":
                view = self._view(
                    self.runtime.cancel(run_id, reason=state.cancel_reason or reason)
                )
            else:
                view = self.status(run_id)
        return self._validate_result(before.run.status, view)

    def pause(self, run_id: str, reason: str = "user_requested") -> AgentRunView:
        before = self.status(run_id)
        view = self._view(self.runtime.pause_run(run_id, reason=reason))
        if before.run.status != "paused_budget":
            self._interrupt_model_loops(run_id)
        return self._validate_result(before.run.status, view)

    def resume(
        self,
        run_id: str,
        budget_delta: BudgetDelta | Mapping[str, Any] | None = None,
        *,
        max_actions: int | None = None,
        execute: bool = True,
    ) -> AgentRunView:
        delta = BudgetDelta.from_value(budget_delta)
        before = self.status(run_id)
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
        self.runtime.resume_control(run_id)
        loop = self.model_loop
        if loop is not None:
            loop.resume(run_id)
        if execute:
            return self.run(run_id, max_actions=max_actions)
        view = self.status(run_id)
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

    def tool_catalog_snapshot(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.tools.snapshot(run_id)

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

    def inspect_session(self, run_id: str, *, event_limit: int = 1000):
        """Return a bounded, read-only operator view of one run."""
        return self.transparency.inspect(run_id, event_limit=event_limit)

    def export_transparency(self, run_id: str, *, event_limit: int = 10000):
        """Export model, tool, context, artifact and evidence lineage metadata."""
        return self.transparency.export(run_id, event_limit=event_limit)

    def explain_tool_visibility(self, run_id: str, *, tool_name: str = ""):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.tools.explain(run_id, tool_name=tool_name)

    def context_usage(self, run_id: str):
        report = self.inspect_session(run_id, event_limit=1)
        return dict(report["context"])

    def evidence_lineage(
        self,
        run_id: str,
        evidence_id: str,
        *,
        direction: str = "both",
        include_payload: bool = True,
    ):
        return self.runtime.evidence_graph.lineage(
            run_id,
            evidence_id,
            direction=direction,
            include_payload=include_payload,
        )

    def asset_attack_graph(
        self,
        run_id: str,
        *,
        limit: int = 1000,
        offset: int = 0,
    ) -> dict[str, Any]:
        return project_asset_attack_graph(self, run_id, limit=limit, offset=offset)

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
        return self.resources.index(roots or (self.runtime.root,))

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
        requested_values = (requested,) if isinstance(requested, str) else requested
        disabled_values = (disabled,) if isinstance(disabled, str) else disabled
        if isinstance(requested_values, bytes) or not isinstance(requested_values, Sequence):
            requested_values = ()
        if isinstance(disabled_values, bytes) or not isinstance(disabled_values, Sequence):
            disabled_values = ()
        configured_requested: list[str] = []
        configured_disabled: list[str] = []
        configured_budget = max(1, int(token_budget))
        try:
            with self.runtime.store.connection() as connection:
                rows = connection.execute("SELECT skill_id,enabled,config_json FROM trace_skills").fetchall()
                for row in rows:
                    skill_id = str(row["skill_id"])
                    if not bool(row["enabled"]):
                        configured_disabled.append(skill_id)
                        continue
                    configured_requested.append(skill_id)
                    try:
                        config = json.loads(str(row["config_json"] or "{}"))
                    except (TypeError, ValueError, json.JSONDecodeError):
                        config = {}
                    if not isinstance(config, Mapping):
                        continue
                    for key, target in (("requested", configured_requested), ("resources", configured_requested), ("disabled", configured_disabled), ("exclude", configured_disabled)):
                        values = config.get(key, ())
                        if isinstance(values, Sequence) and not isinstance(values, (str, bytes)):
                            target.extend(str(item) for item in values if str(item).strip())
                    try:
                        configured_budget = min(configured_budget, max(1, int(config.get("token_budget", configured_budget))))
                    except (TypeError, ValueError, OverflowError):
                        pass
        except sqlite3.OperationalError as exc:
            if "no such table" not in str(exc).casefold():
                raise RuntimeError("skill_state_unavailable") from exc
            configured_disabled = []
        return self.resources.select(
            self.resource_index(run_id),
            requested=tuple(dict.fromkeys((*map(str, requested_values), *configured_requested))),
            disabled=tuple(dict.fromkeys((*map(str, disabled_values), *configured_disabled))),
            token_budget=configured_budget,
        )

    def compact_context(self, run_id: str, message_ids: tuple[str, ...] = ()):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return self.context_compactor.compact(run_id, message_ids)

    def execute_worker(self, task: WorkerTask | Mapping[str, Any]) -> WorkerResult:
        resolved = task if isinstance(task, WorkerTask) else WorkerTask.from_dict(task)
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
        self.worker_records.record_observation(
            task.run_id,
            {**payload, "observation_hash": observation_hash, "observation_id": observation_id},
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
        if ref is None or ref.metadata.get("provider_private"):
            raise KeyError(f"artifact_not_found:{artifact_id}")
        return ref

    def read_artifact(self, run_id: str, artifact_id: str, *, offset: int = 0, limit: int | None = None) -> bytes:
        return self.runtime.artifacts.read(self.artifact(run_id, artifact_id).artifact_id, run_id=run_id, offset=offset, limit=limit)

    def artifacts(self, run_id: str):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return tuple(ref for ref in self.runtime.artifacts.refs(run_id) if not ref.metadata.get("provider_private"))

    def search_artifacts(self, run_id: str, query: str, *, limit: int = 20):
        if self.runtime.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return tuple(ref for ref in self.runtime.artifacts.search(run_id, query, limit=limit) if not ref.metadata.get("provider_private"))

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
