from __future__ import annotations

from uuid import uuid4

from .durable_store import ImmutableRecordError, StateVersionConflict
from .models import LeaseToken, OperationState, TerminalDecision, WorkflowSpec
from .operation_result import OperationResult, TERMINAL_FAILURE_STATUSES


class OperationExecutionMixin:

    @staticmethod
    def _action_descendants(workflow: WorkflowSpec, action_id: str) -> tuple[str, ...]:
        included = {action_id}
        changed = True
        while changed:
            changed = False
            for action in workflow.actions:
                if action.action_id not in included and any(parent in included for parent in action.depends_on):
                    included.add(action.action_id)
                    changed = True
        return tuple(action.action_id for action in workflow.actions if action.action_id in included)

    @staticmethod
    def _terminal_remediation_action(
        state: OperationState,
        workflow: WorkflowSpec,
        terminal: TerminalDecision,
    ) -> tuple[str, str]:
        """Map unmet terminal predicates back to the earliest producer."""

        if any(item in {"evidence_lineage_missing_parent", "evidence_lineage_cycle"} for item in terminal.missing):
            return "", "terminal_evidence_integrity_failed"

        actions = list(workflow.actions)
        by_id = {action.action_id: action for action in actions}
        producer = {action.expected_artifact: action.action_id for action in actions}
        candidates: set[str] = set()

        for missing in terminal.missing:
            if missing.startswith("required_action_evidence:"):
                action_id = missing.split(":", 1)[1]
                if action_id in by_id:
                    candidates.add(action_id)
                continue
            if missing.startswith("action_lineage:"):
                parts = missing.split(":", 2)
                if len(parts) >= 2 and parts[1] in by_id:
                    candidates.add(parts[1])
                continue
            if missing.startswith("required_artifact:"):
                action_id = producer.get(missing.split(":", 1)[1])
                if action_id:
                    candidates.add(action_id)
                continue
            if missing.startswith(("artifact_verified:", "artifact_count:")):
                action_id = producer.get(missing.split(":", 1)[1])
                if action_id:
                    candidates.add(action_id)
                continue
            if missing.startswith("artifact_field:"):
                subject = missing.split(":", 1)[1]
                action_id = producer.get(subject.partition(".")[0])
                if action_id:
                    candidates.add(action_id)
                continue
            if missing.startswith("target_evidence:"):
                if actions:
                    candidates.add(actions[0].action_id)
                continue
            if missing.startswith("workflow_actions_complete"):
                incomplete = next(
                    (
                        action.action_id
                        for action in actions
                        if not action.optional and state.action_status.get(action.action_id) != "completed"
                    ),
                    "",
                )
                if incomplete:
                    candidates.add(incomplete)
                continue
            if missing in {
                "final_report.goal_result:achieved",
                "final_report_lineage_complete",
                "goal_criteria_complete",
                "goal_clause_results_complete",
            }:
                action_id = producer.get("final_report")
                if action_id:
                    candidates.add(action_id)

        if not candidates:
            report = producer.get("final_report")
            if report:
                candidates.add(report)
        if not candidates:
            return "", "terminal_remediation_action_missing"
        order = {action.action_id: index for index, action in enumerate(actions)}
        selected = min(candidates, key=lambda item: order.get(item, len(actions)))
        return selected, "goal_predicates_pending"

    def _schedule_terminal_remediation(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        terminal: TerminalDecision,
        token: LeaseToken,
    ) -> tuple[str, ...] | None:
        action_id, reason = self._terminal_remediation_action(state, workflow, terminal)
        if not action_id:
            state.status = "failed_integrity"
            state.failure_reason = reason
            state.current_action_id = ""
            self.store.save_operation(
                state,
                expected_version=state.state_version,
                lease_token=token,
                event_type="operation_integrity_failed",
                event={"reason": reason, "missing": list(terminal.missing)},
            )
            return None

        reopened = self._action_descendants(workflow, action_id)
        for reopened_id in reopened:
            state.action_status[reopened_id] = "pending"
        state.status = "running"
        state.current_action_id = action_id
        self.store.save_operation(
            state,
            expected_version=state.state_version,
            lease_token=token,
            event_type="terminal_remediation_scheduled",
            event={
                "action_id": action_id,
                "reopened_actions": list(reopened),
                "missing": list(terminal.missing),
            },
        )
        return reopened

    def resume(self, run_id: str, *, max_actions: int | None = None) -> OperationResult:
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        try:
            workflow = self._workflow_for(initial)
        except (TypeError, ValueError, ImmutableRecordError) as exc:
            return self._fail_integrity(initial, self._base_workflow_for(initial), f"plan_integrity:{exc}")
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:resume:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(workflow),
        )
        if token is None:
            current = self.store.load_operation(run_id) or initial
            return self._result(current, self._workflow_for(current))
        try:
            return self._resume_locked(run_id, token=token, max_actions=max_actions)
        finally:
            self.store.release_lease(token)

    def _fail_integrity(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        reason: str,
        token: LeaseToken | None = None,
    ) -> OperationResult:
        state.status = "failed_integrity"
        state.failure_reason = reason
        self.store.save_operation(
            state,
            expected_version=state.state_version,
            lease_token=token,
            event_type="operation_integrity_failed",
            event={"reason": reason},
        )
        return self._result(state, workflow, terminal=TerminalDecision(True, False, reason))

    def _resume_locked(self, run_id: str, *, token: LeaseToken, max_actions: int | None) -> OperationResult:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        base = self._base_workflow_for(state)
        goal_error = self._goal_rewrite_integrity_error(state)
        if goal_error:
            return self._fail_integrity(state, base, goal_error, token)
        workflow_error = self._workflow_integrity_error(state, base)
        if workflow_error:
            return self._fail_integrity(state, base, workflow_error, token)
        try:
            self._ensure_initial_plan(state, base)
            workflow = self._workflow_for(state)
        except (TypeError, ValueError, ImmutableRecordError) as exc:
            return self._fail_integrity(state, base, f"plan_integrity:{exc}", token)

        if state.status == "completed":
            decision = self.terminal_judge.evaluate(
                state=state, goal=state.goal, workflow=workflow, evidence=self.evidence_graph.list(run_id)
            )
            return self._result(state, workflow, terminal=decision) if decision.success else self._fail_integrity(
                state, workflow, "terminal_evidence_integrity_failed", token
            )
        if state.status in TERMINAL_FAILURE_STATUSES:
            return self._result(
                state,
                workflow,
                terminal=TerminalDecision(True, False, state.failure_reason or state.status),
            )
        if state.status == "cancelling":
            return self._finalize_cancel_locked(state, workflow, token)
        if not state.goal.targets:
            if state.status != "waiting_goal_input":
                state.status = "waiting_goal_input"
                state.current_action_id = ""
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="goal_input_required",
                    event={"missing": ["target"]},
                )
            return self._result(state, workflow, next_action="provide_target")

        missing_credentials = self.missing_credential_refs(state)
        if missing_credentials:
            dependency = self._credential_dependency(missing_credentials)
            if state.dependencies.get("credential_rebind") != dependency or state.status != "waiting_dependency":
                state.dependencies["credential_rebind"] = dependency
                state.status = "waiting_dependency"
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="operation_dependency_waiting",
                    event={"dependency": dependency},
                )
            return self._result(
                state,
                workflow,
                missing_capabilities=("credential_rebind",),
            )
        if "credential_rebind" in state.dependencies:
            state.dependencies.pop("credential_rebind", None)
            if state.status == "waiting_dependency":
                state.status = "running"
            self.store.save_operation(
                state,
                expected_version=state.state_version,
                lease_token=token,
                event_type="operation_dependency_resolved",
                event={"dependency_id": "credential_rebind"},
            )

        attempts = self.store.task_attempts(run_id)
        if self._sync_attempt_counters(state, attempts):
            self.store.save_operation(
                state,
                expected_version=state.state_version,
                lease_token=token,
                event_type="attempt_counters_reconciled",
                event={"actions_used": state.budget.actions_used},
            )
        if state.status == "waiting_host":
            action = next((item for item in workflow.actions if item.action_id == state.current_action_id), None)
            if action is not None:
                handoff = self._issue_handoff(state, workflow, action, token)
                return self._result(
                    state,
                    workflow,
                    next_action=action.action_id,
                    missing_capabilities=action.required_capabilities,
                    handoff=handoff,
                )
        if state.status == "paused_budget" and state.budget.pause_reason == "cycle_action_limit":
            state.status = "running"
            state.budget.resume()
            self.store.save_operation(
                state,
                expected_version=state.state_version,
                lease_token=token,
                event_type="operation_resumed",
                event={},
            )

        cycle_limit = max(1, int(max_actions)) if max_actions is not None else state.budget.action_limit
        executed = 0
        while executed < cycle_limit:
            token = self._renew_operation_lease(token, ttl=self._operation_lease_ttl(workflow))
            workflow = self._workflow_for(state)
            evidence = self.evidence_graph.list(run_id)
            terminal = self.terminal_judge.evaluate(state=state, goal=state.goal, workflow=workflow, evidence=evidence)
            if terminal.terminal:
                state.status = "completed"
                state.terminal_reason = terminal.reason
                state.current_action_id = ""
                state.budget.resume()
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="operation_completed",
                    event={"reason": terminal.reason},
                )
                return self._result(state, workflow, terminal=terminal)

            exhaustion = state.budget.exhaustion_reason()
            if exhaustion:
                state.status = "paused_budget"
                state.budget.pause(exhaustion)
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="run_budget_paused",
                    event={"reason": exhaustion, "budget": state.budget.to_dict()},
                )
                return self._result(state, workflow, terminal=terminal)

            facts = self.store.facts(run_id, branch_id=state.branch_id)
            attempts = self.store.task_attempts(run_id)
            decision = self.scheduler.next_action(state, workflow, facts=facts, attempts=attempts)
            if decision.uncertain_attempt is not None:
                timeout = self._action_timeout(decision.action) if decision.action is not None else 60.0
                try:
                    outcome = self.executor.reconcile_attempt(
                        state, workflow, decision.uncertain_attempt, timeout=timeout
                    )
                except StateVersionConflict:
                    current = self.store.load_operation(run_id)
                    if current is not None and current.status == "cancelling":
                        return self._finalize_cancel_locked(current, self._workflow_for(current), token)
                    raise
                executed += 1
                if outcome.event_type == "action_lease_busy":
                    return self._result(state, workflow, next_action=decision.uncertain_attempt.action_id)
                if state.status == "waiting_host":
                    action = decision.action or next(
                        item for item in workflow.actions if item.action_id == state.current_action_id
                    )
                    handoff = self._issue_handoff(state, workflow, action, token)
                    return self._result(
                        state,
                        workflow,
                        next_action=action.action_id,
                        missing_capabilities=("attempt_reconcile", *action.required_capabilities),
                        handoff=handoff,
                    )
                continue

            action = decision.action
            if action is None:
                required = [item for item in workflow.actions if not item.optional]
                if any(state.action_status.get(item.action_id) == "failed" for item in required):
                    state.status = "failed"
                    state.failure_reason = "required_action_failed"
                    self.store.save_operation(
                        state,
                        expected_version=state.state_version,
                        lease_token=token,
                        event_type="operation_failed",
                        event={"reason": state.failure_reason},
                    )
                    return self._result(state, workflow, terminal=TerminalDecision(True, False, state.failure_reason))
                reopened = self._schedule_terminal_remediation(state, workflow, terminal, token)
                if reopened is None:
                    return self._result(
                        state,
                        workflow,
                        terminal=TerminalDecision(True, False, state.failure_reason),
                    )
                continue

            descriptor = decision.descriptor
            if descriptor is None:
                self.broker.refresh()
                descriptor = self.broker.select(
                    action.required_capabilities,
                    exclude=self.scheduler.tool_exclusions(state, action.action_id),
                )
            if descriptor is None:
                if action.tool_strategy == "capability_coverage" and self.scheduler.ensemble_satisfied(state, action):
                    state.action_status[action.action_id] = "completed"
                    state.current_action_id = ""
                    self.store.save_operation(
                        state,
                        expected_version=state.state_version,
                        lease_token=token,
                        event_type="action_ensemble_closed",
                        event={"action_id": action.action_id, "reason": "no_additional_capability_tool"},
                    )
                    continue
                if action.optional:
                    state.action_status[action.action_id] = "skipped"
                    self.store.save_operation(
                        state,
                        expected_version=state.state_version,
                        lease_token=token,
                        event_type="optional_action_skipped",
                        event={"action_id": action.action_id},
                    )
                    continue
                handoff = self._issue_handoff(state, workflow, action, token)
                return self._result(
                    state,
                    workflow,
                    next_action=action.action_id,
                    missing_capabilities=decision.missing_capabilities or action.required_capabilities,
                    handoff=handoff,
                )

            selection = self.broker.explain_selection(
                action.required_capabilities,
                exclude=self.scheduler.tool_exclusions(state, action.action_id),
            )
            self.store.append_event(
                run_id, "tool_selected", {"action_id": action.action_id, "risk": action.risk, **selection}
            )
            token = self._renew_operation_lease(token, ttl=self._action_timeout(action) + 120.0)
            try:
                outcome = self.executor.execute(
                    state,
                    workflow,
                    action,
                    descriptor,
                    timeout=self._action_timeout(action),
                )
            except StateVersionConflict:
                current = self.store.load_operation(run_id)
                if current is not None and current.status == "cancelling":
                    return self._finalize_cancel_locked(current, self._workflow_for(current), token)
                raise
            executed += 1
            token = self._renew_operation_lease(token, ttl=self._operation_lease_ttl(workflow))
            if outcome.event_type == "action_lease_busy":
                return self._result(state, workflow, next_action=action.action_id)
            if state.status == "waiting_host":
                handoff = self._issue_handoff(state, workflow, action, token)
                return self._result(
                    state,
                    workflow,
                    next_action=action.action_id,
                    missing_capabilities=action.required_capabilities,
                    handoff=handoff,
                )

        state.status = "paused_budget"
        state.budget.pause("cycle_action_limit")
        self.store.save_operation(
            state,
            expected_version=state.state_version,
            lease_token=token,
            event_type="cycle_budget_paused",
            event={"executed": executed},
        )
        return self._result(state, self._workflow_for(state))

