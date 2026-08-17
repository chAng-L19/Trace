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
from .operation_result import (
    ARTIFACT_PHASES,
    MAX_INLINE_EVIDENCE_BYTES,
    TERMINAL_FAILURE_STATUSES,
    OperationResult,
    _goal_contract_payload,
)
from .review import ReviewEngine
from .scheduler import Scheduler
from .security import CredentialVault
from .terminal_judge import TerminalJudge
from .tool_broker import ToolBroker
from .verifier import SemanticVerifier
from .workflow_registry import WorkflowRegistry
from .exploration_records import TacticalAttemptRecord


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
        if register_builtins:
            register_builtin_tools(self.broker)
        self.registry = registry or WorkflowRegistry()
        self.compiler = compiler or GoalCompiler()
        self.verifier = verifier or SemanticVerifier()
        self.terminal_judge = terminal_judge or TerminalJudge()
        self.planner = planner or AdaptivePlanner()
        self.review_engine = ReviewEngine()
        self.scheduler = Scheduler(self.broker)
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


__all__ = ["OperationResult", "OperationRuntime"]
