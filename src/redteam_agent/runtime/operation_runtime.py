from __future__ import annotations

import threading
from pathlib import Path
from uuid import uuid4

from .adaptive_planner import AdaptivePlanner
from .artifact_store import ArtifactStore
from .builtins import register_builtin_tools
from .durable_store import DurableStore
from .evidence_graph import EvidenceGraph
from .executor import ActionExecutor
from .goal_compiler import GoalCompiler
from .operation_cancellation import OperationCancellationMixin
from .operation_contract import OperationContractMixin
from .operation_execution import OperationExecutionMixin
from .operation_handoff import OperationHandoffMixin
from .operation_lifecycle import OperationLifecycleMixin
from .terminal_judge import (
    ARTIFACT_PHASES,
    MAX_INLINE_EVIDENCE_BYTES,
    TERMINAL_FAILURE_STATUSES,
    OperationResult,
    _goal_contract_payload,
)
from .evidence_gate import ReviewEngine
from .plan import NextActionPolicy
from .security import CredentialVault
from .terminal_judge import TerminalJudge
from .tool_broker import ToolBroker
from .verifier import SemanticVerifier
from .workflow_registry import WorkflowRegistry
from .exploration import TacticalAttemptRecord


OPERATOR_PAUSE_REASONS = frozenset({"user_requested", "operator_pause"})
BUDGET_PAUSE_REASONS = frozenset(
    {
        "action_limit_exhausted",
        "token_limit_exhausted",
        "token_usage_unknown",
        "time_limit_exhausted",
        "cycle_action_limit",
    }
)


class OperationRuntime(
    OperationContractMixin,
    OperationLifecycleMixin,
    OperationExecutionMixin,
    OperationHandoffMixin,
    OperationCancellationMixin,
):
    """Durable facade composed from focused operation-runtime responsibilities."""

    def __init__(
        self,
        *,
        root: Path,
        broker: ToolBroker | None = None,
        registry: WorkflowRegistry | None = None,
        compiler: GoalCompiler | None = None,
        verifier: SemanticVerifier | None = None,
        terminal_judge: TerminalJudge | None = None,
        register_builtins: bool = True,
        action_timeout_cap: float | None = None,
        planner: AdaptivePlanner | None = None,
    ) -> None:
        self.root = root
        self.store = DurableStore(root)
        self.evidence_graph = EvidenceGraph(self.store, root / "artifacts")
        self.artifacts = ArtifactStore(root / "artifact-store", self.store)
        self.broker = broker or ToolBroker()
        self.broker.bind_workspace_root(root / "workspaces")
        if register_builtins:
            register_builtin_tools(self.broker)
        self.registry = registry or WorkflowRegistry()
        self.compiler = compiler or GoalCompiler()
        self.verifier = verifier or SemanticVerifier()
        self.terminal_judge = terminal_judge or TerminalJudge()
        self.planner = planner or AdaptivePlanner()
        self.review_engine = ReviewEngine()
        self.scheduler = NextActionPolicy(self.broker)
        self.action_timeout_cap = max(0.1, float(action_timeout_cap)) if action_timeout_cap is not None else None
        self.owner = f"runtime-{uuid4().hex}"
        self._credential_vault = CredentialVault()
        # Receipt secrets are deliberately process-local. Durable storage only
        # contains their hashes, so a restart rotates a receipt while repeated
        # resume calls from this runtime keep the valid receipt stable.
        self._handoff_tokens: dict[str, str] = {}
        self._handoff_token_lock = threading.RLock()
        self.executor = ActionExecutor(
            store=self.store,
            evidence_graph=self.evidence_graph,
            broker=self.broker,
            verifier=self.verifier,
            planner=self.planner,
            review_engine=self.review_engine,
            owner=self.owner,
            credential_resolver=self._credential_vault.resolve,
            credential_projector=self._credential_vault.project,
        )

    def _close_run_resources(self, run_id: str) -> None:
        cleanup = self.broker.close_run(run_id)
        if cleanup:
            self.store.append_event(
                run_id,
                "mcp_run_resources_closed",
                {"servers": list(cleanup)},
            )

    def pause_run(self, run_id: str, *, reason: str = "user_requested") -> OperationResult:
        """Persist an operator pause without introducing a second state machine."""

        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        workflow = self._workflow_for(state)
        normalized_reason = reason.strip() or "user_requested"
        if state.status in {"completed", "failed", "failed_integrity", "cancelling", "cancelled"}:
            raise ValueError(f"operation_terminal:{state.status}")
        if state.status == "paused_budget":
            # Never overwrite a budget or missing-usage gate with an operator
            # reason: doing so would let a normal resume bypass enforcement.
            return self._result(state, workflow)
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:pause:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(workflow),
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            current = self.store.load_operation(run_id)
            if current is None:
                raise KeyError(f"operation_not_found:{run_id}")
            if current.status in {"completed", "failed", "failed_integrity", "cancelling", "cancelled"}:
                raise ValueError(f"operation_terminal:{current.status}")
            current.status = "paused_budget"
            # If the budget is already exhausted, preserve its machine reason
            # even when an operator pause arrives concurrently.
            current.budget.pause(current.budget.exhaustion_reason() or normalized_reason)
            self.store.save_operation(
                current,
                expected_version=current.state_version,
                lease_token=token,
                event_type="run_paused",
                event={"reason": current.budget.pause_reason},
            )
            return self._result(current, self._workflow_for(current))
        finally:
            self.store.release_lease(token)

    def resume_control(self, run_id: str) -> OperationResult:
        """Clear an operator pause; budget exhaustion remains enforced by resume()."""

        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        workflow = self._workflow_for(state)
        if (
            state.status != "paused_budget"
            or state.budget.pause_reason in BUDGET_PAUSE_REASONS
            or bool(state.budget.exhaustion_reason())
        ):
            return self._result(state, workflow)
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:resume-control:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(workflow),
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            current = self.store.load_operation(run_id)
            if current is None:
                raise KeyError(f"operation_not_found:{run_id}")
            if (
                current.status == "paused_budget"
                and current.budget.pause_reason not in BUDGET_PAUSE_REASONS
                and not current.budget.exhaustion_reason()
            ):
                current.status = "running"
                current.budget.resume()
                self.store.save_operation(
                    current,
                    expected_version=current.state_version,
                    lease_token=token,
                    event_type="run_resumed",
                    event={},
                )
            return self._result(current, self._workflow_for(current))
        finally:
            self.store.release_lease(token)

    def record_tactical_attempt(
        self,
        record: TacticalAttemptRecord,
    ) -> TacticalAttemptRecord:
        """Persist one model-selected tool action and account for it once."""

        initial = self.store.load_operation(record.run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{record.run_id}")
        token = self.store.acquire_lease(
            record.run_id,
            "__operation__",
            f"{self.owner}:tactical-attempt:{record.attempt_id}",
            ttl_seconds=30.0,
        )
        if token is None:
            raise ValueError(f"operation_busy:{record.run_id}")
        try:
            saved, created = self.store.save_tactical_attempt(record)
            state = self.store.load_operation(record.run_id) or initial
            total_attempts = len(self.store.task_attempts(record.run_id)) + len(
                self.store.tactical_attempts(record.run_id)
            )
            if state.budget.actions_used < total_attempts:
                state.budget.actions_used = total_attempts
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="tactical_attempt_recorded" if created else "tactical_attempt_reconciled",
                    event={
                        "attempt_id": saved.attempt_id,
                        "request_id": saved.request_id,
                        "call_id": saved.call_id,
                        "lifecycle_action_id": saved.lifecycle_action_id,
                        "action_fingerprint": saved.action_fingerprint,
                        "actions_used": total_attempts,
                    },
                )
            return saved
        finally:
            self.store.release_lease(token)


__all__ = ["BUDGET_PAUSE_REASONS", "OPERATOR_PAUSE_REASONS", "OperationResult", "OperationRuntime"]
