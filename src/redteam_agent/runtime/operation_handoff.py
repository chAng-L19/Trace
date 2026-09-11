from __future__ import annotations

import hashlib
from dataclasses import replace
from typing import Any, Mapping
from uuid import uuid4

from .models import ActionSpec, LeaseToken, OperationState, TaskAttempt, WorkflowSpec
from .terminal_judge import OperationResult, _goal_contract_payload
from .verifier import SemanticVerifier


class OperationHandoffMixin:

    def _handoff_contract(
        self, state: OperationState, workflow: WorkflowSpec, action: ActionSpec
    ) -> tuple[str, dict[str, Any]]:
        evidence = self.executor.evidence_for_action(state, workflow, action)
        host_assertions = self.executor.host_assertions_for_action(state, action)
        contract = {
            "run_id": state.run_id,
            "branch_id": state.branch_id,
            "plan_revision": state.plan_revision,
            "action_id": action.action_id,
            "target": state.goal.targets[0] if state.goal.targets else "",
            "goal_contract": _goal_contract_payload(state.goal),
            "action": {
                "name": action.name,
                "required_capabilities": list(action.required_capabilities),
                "parameters": dict(action.parameters),
                "risk": action.risk,
                "timeout_seconds": action.timeout_seconds,
                "tool_strategy": action.tool_strategy,
                "min_tool_results": action.min_tool_results,
                "max_tool_results": action.max_tool_results,
            },
            "expected_artifact": action.expected_artifact,
            "verifier": action.verifier,
            "output_contract": SemanticVerifier.output_contract(action.verifier),
            "evidence_refs": [node.evidence_id for node in evidence],
            "host_assertion_refs": [node.evidence_id for node in host_assertions],
            "verification_requirement": (
                {
                    "required": True,
                    "mode": "independent_tool_execution",
                    "rule": "Host assertions are unverified leads. Re-run the action with an independent Runtime/MCP tool; do not cite assertion IDs as evidence parents.",
                }
                if host_assertions
                else {"required": False}
            ),
        }
        return self.broker.canonical_hash(contract), contract

    def _issue_handoff(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        action: ActionSpec,
        token: LeaseToken,
        *,
        allow_budget_exhausted: bool = False,
    ) -> dict[str, Any]:
        contract_hash, _ = self._handoff_contract(state, workflow, action)
        pending = self.store.pending_handoff(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action.action_id,
        )
        if pending is not None and pending.contract_hash == contract_hash:
            with self._handoff_token_lock:
                raw_token = self._handoff_tokens.get(pending.handoff_id, "")
            if raw_token:
                return {
                    **pending.identity(),
                    "handoff_token": raw_token,
                }

        expected_input_hash = hashlib.sha256(contract_hash.encode("utf-8")).hexdigest()
        attempts = self.store.task_attempts(state.run_id, action_id=action.action_id)
        placeholder = next(
            (
                item
                for item in reversed(attempts)
                if item.branch_id == state.branch_id
                and item.plan_revision == state.plan_revision
                and item.status == "waiting_host"
                and item.tool == "host:agent-observation"
                and item.input_hash == expected_input_hash
            ),
            None,
        )
        if placeholder is None:
            exhaustion = state.budget.exhaustion_reason()
            if exhaustion and not allow_budget_exhausted:
                state.status = "paused_budget"
                state.current_action_id = action.action_id
                state.action_status[action.action_id] = "pending"
                state.budget.pause(exhaustion)
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="run_budget_paused",
                    event={
                        "reason": exhaustion,
                        "budget": state.budget.to_dict(),
                        "blocked_action": action.action_id,
                    },
                )
                return {}
            attempt_sequence = 1 + sum(
                1
                for item in attempts
                if item.branch_id == state.branch_id
                and item.plan_revision == state.plan_revision
            )
            idempotency_key = hashlib.sha256(
                f"handoff\0{state.run_id}\0{state.branch_id}\0{state.plan_revision}\0{action.action_id}\0{contract_hash}\0{attempt_sequence}".encode(
                    "utf-8"
                )
            ).hexdigest()
            placeholder = replace(
                TaskAttempt.create(
                    run_id=state.run_id,
                    branch_id=state.branch_id,
                    plan_revision=state.plan_revision,
                    action_id=action.action_id,
                    tool="host:agent-observation",
                    tool_version="host-receipt-v1",
                    input_hash=expected_input_hash,
                    idempotency_key=idempotency_key,
                ),
                status="waiting_host",
            )
            self.store.create_task_attempt(placeholder)
            state.action_attempts[action.action_id] = state.action_attempts.get(action.action_id, 0) + 1
            state.budget.record_action()
        state.status = "waiting_host"
        state.current_action_id = action.action_id
        state.action_status[action.action_id] = "running"
        self.store.save_operation(
            state,
            expected_version=state.state_version,
            lease_token=token,
            event_type="host_handoff_ready",
            event={"action_id": action.action_id, "attempt_id": placeholder.attempt_id, "contract_hash": contract_hash},
        )
        handoff_id, raw_token = self.store.create_handoff(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action.action_id,
            attempt_id=placeholder.attempt_id,
            contract_hash=contract_hash,
        )
        with self._handoff_token_lock:
            if pending is not None:
                self._handoff_tokens.pop(pending.handoff_id, None)
            self._handoff_tokens[handoff_id] = raw_token
        return {
            "handoff_id": handoff_id,
            "handoff_token": raw_token,
            "attempt_id": placeholder.attempt_id,
            "contract_hash": contract_hash,
            "run_id": state.run_id,
            "branch_id": state.branch_id,
            "plan_revision": state.plan_revision,
            "action_id": action.action_id,
        }

    def _action_for_observation(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        action_id: str,
    ) -> ActionSpec:
        if state.status in {"completed", "failed", "failed_integrity", "cancelled"}:
            raise ValueError(f"operation_terminal:{state.status}")
        goal_error = self._goal_rewrite_integrity_error(state)
        if goal_error:
            raise ValueError(goal_error)
        error = self._workflow_integrity_error(state, self._base_workflow_for(state))
        if error:
            raise ValueError(error)
        action = next((item for item in workflow.actions if item.action_id == action_id), None)
        if action is None:
            raise KeyError(f"action_not_found:{action_id}")
        dependencies = [state.action_status.get(item, "pending") for item in action.depends_on]
        if not all(item in {"completed", "skipped"} for item in dependencies):
            raise ValueError(f"action_dependencies_incomplete:{action_id}")
        return action

    def submit_handoff_observation(
        self,
        *,
        run_id: str,
        handoff_id: str,
        handoff_token: str,
        attempt_id: str,
        contract_hash: str,
        output: Any,
        tool: str = "host-agent",
        usage: Mapping[str, Any] | None = None,
        continue_run: bool = True,
        max_actions: int | None = None,
    ) -> OperationResult:
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        lease = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:handoff:{uuid4().hex}",
            ttl_seconds=120.0,
        )
        if lease is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.load_operation(run_id) or initial
            workflow = self._workflow_for(state)
            record = self.store.get_handoff(handoff_id)
            if record is None or record.status != "pending":
                raise ValueError("handoff_receipt_not_pending")
            action = self._action_for_observation(state, workflow, record.action_id)
            expected_hash, _ = self._handoff_contract(state, workflow, action)
            if expected_hash != contract_hash or record.contract_hash != contract_hash:
                raise ValueError("handoff_contract_mismatch")
            consumed = self.store.receive_handoff_observation(
                handoff_id=handoff_id,
                raw_token=handoff_token,
                run_id=run_id,
                branch_id=state.branch_id,
                plan_revision=state.plan_revision,
                action_id=action.action_id,
                attempt_id=attempt_id,
                contract_hash=contract_hash,
                output=output,
                tool=tool,
                usage=usage,
            )
            if consumed is None:
                raise ValueError("handoff_receipt_rejected")
            with self._handoff_token_lock:
                self._handoff_tokens.pop(handoff_id, None)
            outcome = self.executor.accept_host_observation(
                state,
                workflow,
                action,
                output=output,
                attempt_id=attempt_id,
                contract_hash=contract_hash,
                usage=usage,
                timeout=self._action_timeout(action) + 30.0,
            )
            if not outcome.progressed and outcome.event_type not in {
                "action_lease_busy",
                "action_host_handoff_required",
                "action_verification_failed",
            }:
                raise ValueError(f"host_observation_rejected:{outcome.reason}")
        finally:
            self.store.release_lease(lease)
        current = self.store.load_operation(run_id)
        if current is not None and current.cancel_reason:
            return self.cancel(run_id, reason=current.cancel_reason)
        return self.resume(run_id, max_actions=max_actions) if continue_run else self.status(run_id)

    def validate_handoff_observation(
        self,
        *,
        run_id: str,
        handoff_id: str,
        handoff_token: str,
        attempt_id: str,
        contract_hash: str,
        action_id: str = "",
    ) -> bool:
        """Validate a receipt without consuming it or advancing its operation."""

        state = self.store.load_operation(run_id)
        if state is None:
            return False
        try:
            workflow = self._workflow_for(state)
            record = self.store.get_handoff(handoff_id)
            if record is None or record.status != "pending":
                return False
            if action_id and action_id != record.action_id:
                return False
            action = self._action_for_observation(state, workflow, record.action_id)
            expected_hash, _ = self._handoff_contract(state, workflow, action)
        except (KeyError, TypeError, ValueError):
            return False
        if expected_hash != contract_hash or record.contract_hash != contract_hash:
            return False
        return self.store.validate_handoff(
            handoff_id=handoff_id,
            raw_token=handoff_token,
            run_id=run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=record.action_id,
            attempt_id=attempt_id,
            contract_hash=contract_hash,
        )

    def submit_observation(
        self,
        *,
        run_id: str,
        action_id: str,
        output: Any,
        tool: str = "host-agent",
        usage: Mapping[str, Any] | None = None,
        idempotency_key: str = "",
        continue_run: bool = True,
        max_actions: int | None = None,
    ) -> OperationResult:
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        initial_workflow = self._workflow_for(initial)
        lease = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:external:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(initial_workflow),
        )
        if lease is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.load_operation(run_id) or initial
            workflow = self._workflow_for(state)
            exhaustion = state.budget.exhaustion_reason()
            if exhaustion:
                raise ValueError(f"run_budget_exhausted:{exhaustion}")
            action = self._action_for_observation(state, workflow, action_id)
            if (
                state.action_status.get(action.action_id) in {"completed", "skipped"}
                and not idempotency_key.strip()
            ):
                raise ValueError(f"action_already_terminal:{action.action_id}")
            outcome = self.executor.accept_external_observation(
                state,
                workflow,
                action,
                output=output,
                tool=tool,
                usage=usage,
                idempotency_key=idempotency_key,
                timeout=self._action_timeout(action) + 30.0,
            )
            if not outcome.progressed and outcome.event_type not in {
                "action_lease_busy",
                "observation_idempotent_replay",
            }:
                raise ValueError(f"observation_verification_failed:{outcome.reason}")
        finally:
            self.store.release_lease(lease)
        return self.resume(run_id, max_actions=max_actions) if continue_run else self.status(run_id)
