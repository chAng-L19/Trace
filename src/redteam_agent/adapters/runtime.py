from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from ..core import Evidence, Event, Goal, Run, TerminalDecision
from ..core.ports import (
    EventPort,
    StoreConflictError,
    StorePort,
    ToolCall,
    ToolDefinition,
    ToolPort,
    ToolResult,
)
from ..runtime.durable_store import StateVersionConflict
from ..runtime.models import ToolDescriptor as LegacyToolDescriptor
from ..runtime.operation_result import OperationResult
from ..runtime.operation_runtime import OperationRuntime
from .runtime_mapping import (
    apply_core_run,
    evidence_from_runtime,
    goal_from_runtime,
    run_from_runtime,
    terminal_from_runtime,
)


@dataclass(frozen=True, slots=True)
class OperationView:
    run: Run
    goal: Goal
    evidence: tuple[Evidence, ...]
    terminal: TerminalDecision
    next_action: str = ""
    missing_capabilities: tuple[str, ...] = ()
    handoff: Mapping[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "run": self.run.to_dict(),
            "goal": self.goal.to_dict(),
            "evidence": [item.to_dict() for item in self.evidence],
            "terminal": self.terminal.to_dict(),
            "next_action": self.next_action,
            "missing_capabilities": list(self.missing_capabilities),
            "handoff": dict(self.handoff or {}),
        }


def _view(result: OperationResult) -> OperationView:
    return OperationView(
        run=run_from_runtime(result.state),
        goal=goal_from_runtime(result.state.goal),
        evidence=tuple(evidence_from_runtime(item) for item in result.evidence),
        terminal=terminal_from_runtime(result.terminal),
        next_action=result.next_action,
        missing_capabilities=tuple(result.missing_capabilities),
        handoff=dict(result.handoff),
    )


class RuntimeStoreAdapter(StorePort):
    def __init__(self, runtime: OperationRuntime) -> None:
        self.runtime = runtime

    def load_run(self, run_id: str) -> Run | None:
        state = self.runtime.store.load_operation(run_id)
        return run_from_runtime(state) if state is not None else None

    def commit_run(self, run: Run, *, expected_version: int) -> Run:
        state = self.runtime.store.load_operation(run.run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run.run_id}")
        if state.state_version != expected_version or run.state_version != expected_version:
            raise StoreConflictError(
                f"state_version_conflict:{run.run_id}:{expected_version}:{state.state_version}"
            )
        apply_core_run(state, run)
        try:
            self.runtime.store.save_operation(
                state,
                event_type="core_run_committed",
                event={"source": "runtime-store-adapter"},
                expected_version=expected_version,
            )
        except StateVersionConflict as exc:
            raise StoreConflictError(str(exc)) from exc
        return run_from_runtime(state)


class RuntimeEventAdapter(EventPort):
    def __init__(self, runtime: OperationRuntime) -> None:
        self.runtime = runtime

    def append(self, event: Event) -> None:
        self.runtime.store.append_event(event.run_id, event.event_type, dict(event.payload))

    def read(self, run_id: str, *, after_sequence: int = 0, limit: int = 200) -> tuple[Event, ...]:
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


class RuntimeToolAdapter(ToolPort):
    def __init__(self, runtime: OperationRuntime) -> None:
        self.runtime = runtime

    @staticmethod
    def _definition(descriptor: LegacyToolDescriptor) -> ToolDefinition:
        return ToolDefinition(
            qualified_name=descriptor.qualified_name,
            name=descriptor.name,
            server=descriptor.server,
            description=descriptor.description,
            input_schema=dict(descriptor.input_schema),
            capabilities=tuple(descriptor.capabilities),
            version=descriptor.version,
            side_effecting=descriptor.side_effecting,
            supports_reconcile=descriptor.supports_reconcile,
            metadata={
                "source": descriptor.source,
                "healthy": descriptor.healthy,
                "priority": descriptor.priority,
                "schema_hash": descriptor.schema_hash,
            },
        )

    def _legacy_definition(self, qualified_name: str) -> LegacyToolDescriptor:
        descriptor = next(
            (item for item in self.runtime.broker.descriptors() if item.qualified_name == qualified_name),
            None,
        )
        if descriptor is None:
            raise KeyError(f"tool_not_found:{qualified_name}")
        return descriptor

    @staticmethod
    def _result(call: ToolCall, result: Any) -> ToolResult:
        return ToolResult(
            call_id=result.call_id or call.call_id,
            status=result.status,
            tool_name=result.tool or call.tool_name,
            output=result.output,
            error=result.error,
            retryable=result.retryable,
            input_hash=result.input_hash,
            output_hash=result.output_hash,
            started_at=result.started_at,
            finished_at=result.finished_at,
            metadata={"tool_version": result.tool_version},
        )

    def discover(self) -> tuple[ToolDefinition, ...]:
        return tuple(self._definition(item) for item in self.runtime.broker.descriptors())

    def invoke(self, call: ToolCall) -> ToolResult:
        descriptor = self._legacy_definition(call.tool_name)
        result = self.runtime.broker.call(
            descriptor,
            dict(call.arguments),
            timeout=call.timeout_seconds or 60.0,
        )
        return self._result(call, result)

    def reconcile(self, call: ToolCall) -> ToolResult | None:
        if not call.idempotency_key:
            raise ValueError("idempotency_key_required")
        descriptor = self._legacy_definition(call.tool_name)
        result = self.runtime.broker.reconcile(
            descriptor,
            idempotency_key=call.idempotency_key,
            arguments=dict(call.arguments),
            timeout=call.timeout_seconds or 60.0,
        )
        return self._result(call, result) if result is not None else None

    def cancel(self, call_id: str) -> bool:
        del call_id
        return False


class OperationRuntimeAdapter:
    """Core-contract facade over the pre-AgentService runtime."""

    def __init__(self, runtime: OperationRuntime) -> None:
        self.runtime = runtime
        self.store: StorePort = RuntimeStoreAdapter(runtime)
        self.events: EventPort = RuntimeEventAdapter(runtime)
        self.tools: ToolPort = RuntimeToolAdapter(runtime)

    def start(
        self,
        *,
        session_id: str,
        objective: str,
        targets: Sequence[str] | None = None,
        workflow_hint: str = "",
        starting_context: Mapping[str, Any] | None = None,
        constraints: Mapping[str, Any] | None = None,
        success_predicates: Sequence[Mapping[str, Any]] = (),
        max_actions: int = 64,
        max_retries_per_action: int = 2,
    ) -> Run:
        state = self.runtime.start(
            session_id=session_id,
            objective=objective,
            targets=targets,
            workflow_hint=workflow_hint,
            starting_context=starting_context,
            constraints=constraints,
            success_predicates=success_predicates,
            max_actions=max_actions,
            max_retries_per_action=max_retries_per_action,
        )
        return run_from_runtime(state)

    def run(self, run_id: str, *, max_actions: int | None = None) -> OperationView:
        return _view(self.runtime.resume(run_id, max_actions=max_actions))

    def status(self, run_id: str) -> OperationView:
        return _view(self.runtime.status(run_id))

    def cancel(self, run_id: str, *, reason: str = "user_requested") -> OperationView:
        return _view(self.runtime.cancel(run_id, reason=reason))

    def submit_observation(
        self,
        *,
        run_id: str,
        action_id: str,
        output: Any,
        tool: str = "host-agent",
        usage: Mapping[str, Any] | None = None,
        continue_run: bool = True,
        max_actions: int | None = None,
    ) -> OperationView:
        result = self.runtime.submit_observation(
            run_id=run_id,
            action_id=action_id,
            output=output,
            tool=tool,
            usage=usage,
            continue_run=continue_run,
            max_actions=max_actions,
        )
        return _view(result)
