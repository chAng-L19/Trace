from __future__ import annotations

from typing import Any, Mapping, Sequence
from uuid import uuid4

from .durable_store import ImmutableRecordError, StateVersionConflict, StoreConflictError
from .models import EvidenceNode, LeaseToken, OperationState, TerminalDecision, WorkflowSpec
from .operation_result import OperationResult, TERMINAL_FAILURE_STATUSES


class OperationCancellationMixin:

    def cancel(self, run_id: str, *, reason: str = "user_requested") -> OperationResult:
        requested_reason = reason.strip() or "user_requested"
        for _ in range(8):
            state = self.store.load_operation(run_id)
            if state is None:
                raise KeyError(f"operation_not_found:{run_id}")
            workflow = self._workflow_for(state)
            if state.status == "cancelled":
                return self._result(state, workflow, terminal=TerminalDecision(True, False, "cancelled"))
            if state.status in {"completed", "failed", "failed_integrity"}:
                raise ValueError(f"operation_terminal:{state.status}")
            if state.status != "cancelling" or state.cancel_reason != requested_reason:
                expected_version = state.state_version
                state.status = "cancelling"
                state.cancel_reason = requested_reason
                try:
                    self.store.compare_and_swap_operation(
                        state,
                        expected_version=expected_version,
                        event_type="operation_cancelling",
                        event={"reason": requested_reason},
                    )
                except StateVersionConflict:
                    continue
            break
        else:
            raise StoreConflictError(f"cancel_cas_exhausted:{run_id}")

        lease = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:cancel:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(workflow),
        )
        if lease is None:
            current = self.store.load_operation(run_id) or state
            return self._result(current, self._workflow_for(current))
        try:
            current = self.store.load_operation(run_id) or state
            return self._finalize_cancel_locked(current, self._workflow_for(current), lease)
        finally:
            self.store.release_lease(lease)

    @staticmethod
    def _uncovered_reproductions(
        reproductions: Sequence[EvidenceNode],
        cleanup_nodes: Sequence[EvidenceNode],
        evidence: Sequence[EvidenceNode],
    ) -> tuple[EvidenceNode, ...]:
        by_id = {node.evidence_id: node for node in evidence if node.verified}

        def ancestors(node: EvidenceNode) -> set[str]:
            stack = list(node.parent_ids)
            visited: set[str] = set()
            while stack:
                evidence_id = stack.pop()
                if evidence_id in visited:
                    continue
                visited.add(evidence_id)
                parent = by_id.get(evidence_id)
                if parent is not None:
                    stack.extend(parent.parent_ids)
            return visited

        cleanup_ancestry = {
            node.evidence_id: ancestors(node)
            for node in cleanup_nodes
        }
        uncovered: list[EvidenceNode] = []
        for reproduction in reproductions:
            reproduction_provenance = reproduction.provenance
            covered = any(
                reproduction.evidence_id in cleanup_ancestry[cleanup.evidence_id]
                and cleanup.target == reproduction.target
                and cleanup.provenance is not None
                and reproduction_provenance is not None
                and cleanup.provenance.branch_id == reproduction_provenance.branch_id
                and cleanup.provenance.plan_revision >= reproduction_provenance.plan_revision
                for cleanup in cleanup_nodes
            )
            if not covered:
                uncovered.append(reproduction)
        return tuple(uncovered)

    def _finalize_cancel_locked(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        token: LeaseToken,
    ) -> OperationResult:
        if state.status == "cancelled":
            return self._result(state, workflow, terminal=TerminalDecision(True, False, "cancelled"))
        state.status = "cancelling"
        evidence = self.evidence_graph.list(state.run_id)
        reproductions = tuple(node for node in evidence if node.verified and node.artifact_type == "reproduction_artifact")
        cleanup_nodes = tuple(node for node in evidence if node.verified and node.artifact_type == "cleanup_proof")
        uncovered_reproductions = self._uncovered_reproductions(reproductions, cleanup_nodes, evidence)
        cleanup = next((item for item in workflow.actions if item.expected_artifact == "cleanup_proof"), None)

        if not reproductions:
            state.cleanup_status = "not_required"
        elif cleanup_nodes and not uncovered_reproductions:
            state.cleanup_status = "verified"
        elif cleanup is None:
            persist_pending = state.cleanup_status != "unavailable" or bool(state.current_action_id)
            state.cleanup_status = "unavailable"
            state.current_action_id = ""
            if persist_pending:
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="cancel_cleanup_unavailable",
                    event={"reason": "cleanup_action_missing"},
                )
            return self._result(
                state,
                workflow,
                terminal=TerminalDecision(
                    False,
                    False,
                    "cancel_cleanup_unavailable",
                    missing=("cleanup_proof",),
                ),
                next_action="cleanup_required",
            )
        else:
            missing_credentials = self.missing_credential_refs(state)
            if missing_credentials:
                dependency = self._credential_dependency(missing_credentials)
                if state.dependencies.get("credential_rebind") != dependency:
                    state.dependencies["credential_rebind"] = dependency
                    self.store.save_operation(
                        state,
                        expected_version=state.state_version,
                        lease_token=token,
                        event_type="operation_dependency_waiting",
                        event={"dependency": dependency, "phase": "cancel_cleanup"},
                    )
                return self._result(
                    state,
                    workflow,
                    missing_capabilities=("credential_rebind",),
                )
            if "credential_rebind" in state.dependencies:
                state.dependencies.pop("credential_rebind", None)
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="operation_dependency_resolved",
                    event={"dependency_id": "credential_rebind", "phase": "cancel_cleanup"},
                )
            state.action_status[cleanup.action_id] = "pending"
            state.current_action_id = cleanup.action_id
            cleanup_error = ""
            descriptor = self.broker.select(
                cleanup.required_capabilities,
                exclude=self.scheduler.tool_exclusions(state, cleanup.action_id),
            )
            if descriptor is None:
                self.broker.refresh()
                descriptor = self.broker.select(
                    cleanup.required_capabilities,
                    exclude=self.scheduler.tool_exclusions(state, cleanup.action_id),
                )
            if descriptor is None:
                state.cleanup_status = "pending_host"
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="cancel_cleanup_handoff_required",
                    event={"action_id": cleanup.action_id},
                )
                handoff = self._issue_handoff(
                    state,
                    workflow,
                    cleanup,
                    token,
                    allow_budget_exhausted=True,
                )
                return self._result(
                    state,
                    workflow,
                    next_action=cleanup.action_id,
                    missing_capabilities=cleanup.required_capabilities,
                    handoff=handoff,
                )
            try:
                token = self._renew_operation_lease(
                    token, ttl=self._action_timeout(cleanup) + 120.0
                )
                self.executor.execute(
                    state,
                    workflow,
                    cleanup,
                    descriptor,
                    timeout=self._action_timeout(cleanup),
                )
                token = self._renew_operation_lease(token, ttl=self._operation_lease_ttl(workflow))
            except StateVersionConflict:
                state = self.store.load_operation(state.run_id) or state
            except Exception as exc:
                cleanup_error = f"executor_exception:{type(exc).__name__}"
                state = self.store.load_operation(state.run_id) or state
            cleanup_nodes = tuple(
                node
                for node in self.evidence_graph.list(state.run_id)
                if node.verified and node.artifact_type == "cleanup_proof"
            )
            evidence = self.evidence_graph.list(state.run_id)
            reproductions = tuple(
                node
                for node in evidence
                if node.verified and node.artifact_type == "reproduction_artifact"
            )
            cleanup_nodes = tuple(
                node
                for node in evidence
                if node.verified and node.artifact_type == "cleanup_proof"
            )
            uncovered_reproductions = self._uncovered_reproductions(
                reproductions,
                cleanup_nodes,
                evidence,
            )
            state = self.store.load_operation(state.run_id) or state
            state.cleanup_status = "verified" if cleanup_nodes and not uncovered_reproductions else "failed"

            if not cleanup_nodes or uncovered_reproductions:
                state.status = "cancelling"
                state.current_action_id = cleanup.action_id
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="cancel_cleanup_failed",
                    event={
                        "action_id": cleanup.action_id,
                        "reason": cleanup_error or "cleanup_proof_not_verified",
                        "uncovered_reproduction_ids": [
                            node.evidence_id for node in uncovered_reproductions
                        ],
                    },
                )
                return self._result(
                    state,
                    workflow,
                    terminal=TerminalDecision(
                        False,
                        False,
                        "cancel_cleanup_failed",
                        missing=("cleanup_proof",),
                    ),
                    next_action=cleanup.action_id,
                )

        state.status = "cancelled"
        state.current_action_id = ""
        state.failure_reason = "cancelled"
        self.store.save_operation(
            state,
            expected_version=state.state_version,
            lease_token=token,
            event_type="operation_cancelled",
            event={"reason": state.cancel_reason, "cleanup_status": state.cleanup_status},
        )
        return self._result(state, workflow, terminal=TerminalDecision(True, False, "cancelled"))

    def _result(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        *,
        terminal: TerminalDecision | None = None,
        next_action: str = "",
        missing_capabilities: Sequence[str] = (),
        handoff: Mapping[str, Any] | None = None,
    ) -> OperationResult:
        evidence = self.evidence_graph.list(state.run_id, include_unverified=True)
        decision = terminal
        if (
            decision is None
            and state.cancel_reason
            and state.cleanup_status not in {"not_required", "verified"}
        ):
            decision = TerminalDecision(
                False,
                False,
                "cancel_cleanup_pending",
                missing=("cleanup_proof",),
            )
        if decision is None and state.status in TERMINAL_FAILURE_STATUSES:
            decision = TerminalDecision(
                True,
                False,
                state.failure_reason or state.status,
            )
        if decision is None:
            decision = self.terminal_judge.evaluate(
                state=state,
                goal=state.goal,
                workflow=workflow,
                evidence=evidence,
            )
        resolved_action = next_action
        resolved_missing = tuple(missing_capabilities)
        if state.dependencies:
            resolved_action = ""
            resolved_missing = tuple(dict.fromkeys((*resolved_missing, *state.dependencies.keys())))
        elif not resolved_action and not decision.terminal:
            action = next((item for item in workflow.actions if item.action_id == state.current_action_id), None)
            if action is None:
                schedule = self.scheduler.next_action(
                    state,
                    workflow,
                    facts=self.store.facts(state.run_id, branch_id=state.branch_id),
                    attempts=self.store.task_attempts(state.run_id),
                )
                action = schedule.action
                if not resolved_missing:
                    resolved_missing = schedule.missing_capabilities
            if action is not None:
                resolved_action = action.action_id
                if not resolved_missing and self.broker.select(
                    action.required_capabilities,
                    exclude=self.scheduler.tool_exclusions(state, action.action_id),
                ) is None:
                    resolved_missing = action.required_capabilities
        return OperationResult(
            state=state,
            workflow=workflow,
            evidence=evidence,
            terminal=decision,
            next_action=resolved_action,
            missing_capabilities=resolved_missing,
            handoff=dict(handoff or {}),
        )

    def status(self, run_id: str) -> OperationResult:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        base: WorkflowSpec | None = None
        try:
            base = self._base_workflow_for(state)
            goal_error = self._goal_rewrite_integrity_error(state)
            if goal_error:
                raise ValueError(goal_error)
            workflow_error = self._workflow_integrity_error(state, base)
            if workflow_error:
                raise ValueError(workflow_error)
            workflow = self._workflow_for(state)
        except (TypeError, ValueError, ImmutableRecordError) as exc:
            detail = str(exc)
            reason = detail if detail.startswith("prompt_rewrite_") else f"plan_integrity:{detail}"
            view = OperationState.from_dict(state.to_dict())
            view.status = "failed_integrity"
            view.failure_reason = reason
            fallback = base
            if fallback is None:
                fallback = WorkflowSpec(
                    workflow_id=state.workflow_id,
                    version=state.workflow_version,
                    name=state.workflow_id,
                    description="Integrity failure view",
                    match_tags=(),
                    actions=(),
                    terminal_predicates=(),
                    required_artifacts=(),
                )
            return self._result(
                view,
                fallback,
                terminal=TerminalDecision(True, False, reason),
            )
        missing_credentials = self.missing_credential_refs(state)
        if state.status not in {"completed", *TERMINAL_FAILURE_STATUSES} and missing_credentials:
            view = OperationState.from_dict(state.to_dict())
            view.status = "waiting_dependency"
            view.dependencies["credential_rebind"] = self._credential_dependency(missing_credentials)
            return self._result(
                view,
                workflow,
                missing_capabilities=("credential_rebind",),
            )
        result = self._result(state, workflow)
        if state.status == "completed" and not result.terminal.success:
            reason = "terminal_evidence_integrity_failed"
            view = OperationState.from_dict(state.to_dict())
            view.status = "failed_integrity"
            view.failure_reason = reason
            return self._result(
                view,
                workflow,
                terminal=TerminalDecision(True, False, reason),
            )
        return result
