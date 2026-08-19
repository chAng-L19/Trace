from __future__ import annotations

import hashlib
from dataclasses import replace
from typing import Any, Mapping
from uuid import uuid4

from .executor_common import ExecutionOutcome
from .models import (
    ActionSpec,
    LeaseToken,
    OperationState,
    TaskAttempt,
    ToolCallResult,
    ToolDescriptor,
    WorkflowSpec,
    utc_now,
)


class ExecutorActionsMixin:
    """Lease-bound execution, reconciliation, and observation ingestion."""

    def _mark_uncertain(
        self,
        *,
        state: OperationState,
        action: ActionSpec,
        attempt: TaskAttempt,
        lease_token: LeaseToken,
        result: ToolCallResult,
        expected_attempt_status: str = "running",
    ) -> ExecutionOutcome:
        expected_version = state.state_version
        state.status = "running"
        state.current_action_id = action.action_id
        state.action_status[action.action_id] = "running"
        uncertain = replace(
            attempt,
            status="uncertain",
            result=result.to_dict(),
            error=result.error or "side_effect_result_uncertain",
        )
        self.store.commit_action_outcome(
            state=state,
            expected_state_version=expected_version,
            attempt=uncertain,
            expected_attempt_status=expected_attempt_status,
            lease_token=lease_token,
            event_type="action_result_uncertain",
            event={
                "action_id": action.action_id,
                "attempt_id": attempt.attempt_id,
                "tool": attempt.tool,
                "reason": uncertain.error,
            },
        )
        return ExecutionOutcome(False, uncertain.error, "action_result_uncertain")

    def _defer_reconcile_to_host(
        self,
        *,
        state: OperationState,
        action: ActionSpec,
        attempt: TaskAttempt,
        lease_token: LeaseToken,
        reason: str,
    ) -> ExecutionOutcome:
        expected_version = state.state_version
        state.status = "waiting_host"
        state.current_action_id = action.action_id
        state.action_status[action.action_id] = "running"
        waiting = replace(
            attempt,
            status="reconciled",
            error=reason or "reconcile_host_required",
            finished_at=utc_now(),
        )
        self.store.commit_action_outcome(
            state=state,
            expected_state_version=expected_version,
            attempt=waiting,
            expected_attempt_status="reconciling",
            lease_token=lease_token,
            event_type="action_reconcile_host_required",
            event={
                "action_id": action.action_id,
                "attempt_id": attempt.attempt_id,
                "tool": attempt.tool,
                "reason": waiting.error,
            },
        )
        return ExecutionOutcome(False, waiting.error, "action_reconcile_host_required")

    def reconcile_attempt(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        attempt: TaskAttempt,
        *,
        timeout: float,
    ) -> ExecutionOutcome:
        action = next((item for item in workflow.actions if item.action_id == attempt.action_id), None)
        if action is None:
            return ExecutionOutcome(False, "attempt_action_missing", "attempt_reconcile_failed")
        lease_token = self.store.acquire_lease(
            state.run_id,
            action.action_id,
            f"{self.owner}:reconcile:{uuid4().hex}",
            ttl_seconds=timeout + 30.0,
        )
        if lease_token is None:
            return ExecutionOutcome(False, "action_lease_busy", "action_lease_busy")
        try:
            prior_status = attempt.status
            claimed = self.store.claim_task_attempt(
                attempt,
                expected_statuses=("prepared", "running", "uncertain", "reconciling", "consumed"),
                lease_token=lease_token,
                next_status="reconciling",
            )
            descriptor = next(
                (
                    item
                    for item in self.broker.descriptors()
                    if item.qualified_name == claimed.tool and item.version == claimed.tool_version
                ),
                None,
            )
            cached = self.store.cached_action_result(
                state.run_id,
                claimed.idempotency_key,
                action_id=action.action_id,
            )
            host_attempt = claimed.tool in {
                "host:handoff",
                "host:agent-observation",
                "host:external-observation",
            }
            if descriptor is None and host_attempt and cached is not None:
                host_name = claimed.tool.removeprefix("host:")
                descriptor = ToolDescriptor(
                    server="host",
                    name=host_name,
                    description="Recovered Host Agent observation.",
                    input_schema={"type": "object"},
                    capabilities=tuple(action.required_capabilities),
                    source="host-receipt",
                    healthy=True,
                    priority=0,
                    version=claimed.tool_version,
                    schema_hash="",
                    side_effecting=True,
                    supports_reconcile=False,
                )
            if descriptor is None:
                return self._defer_reconcile_to_host(
                    state=state,
                    action=action,
                    attempt=claimed,
                    lease_token=lease_token,
                    reason="reconcile_tool_version_unavailable",
                )
            if cached is not None and host_attempt:
                arguments: Mapping[str, Any] = {}
                idempotency_key = claimed.idempotency_key
                input_hash = claimed.input_hash
            else:
                arguments, idempotency_key, input_hash = self.arguments_for(
                    state,
                    workflow,
                    action,
                    descriptor,
                    idempotency_key_override=claimed.idempotency_key,
                )
                if idempotency_key != claimed.idempotency_key or input_hash != claimed.input_hash:
                    return self._defer_reconcile_to_host(
                        state=state,
                        action=action,
                        attempt=claimed,
                        lease_token=lease_token,
                        reason="reconcile_input_identity_changed",
                    )
            result = cached
            if result is not None and result.input_hash and result.input_hash != claimed.input_hash:
                return self._defer_reconcile_to_host(
                    state=state,
                    action=action,
                    attempt=claimed,
                    lease_token=lease_token,
                    reason="cached_result_input_hash_mismatch",
                )
            allow_retry = not descriptor.side_effecting
            if result is None and prior_status == "prepared":
                result = self._with_durable_input_hash(
                    self.broker.call(
                        descriptor,
                        arguments,
                        timeout=timeout,
                        run_id=state.run_id,
                    ), claimed.input_hash
                )
                allow_retry = not descriptor.side_effecting
            elif result is None and descriptor.supports_reconcile:
                result = self._with_durable_input_hash(
                    self.broker.reconcile(
                        descriptor,
                        idempotency_key=claimed.idempotency_key,
                        arguments=arguments,
                    ),
                    claimed.input_hash,
                )
                allow_retry = False
            elif result is None and not descriptor.side_effecting:
                result = self._with_durable_input_hash(
                    self.broker.call(
                        descriptor,
                        arguments,
                        timeout=timeout,
                        run_id=state.run_id,
                    ), claimed.input_hash
                )
            if result is None:
                return self._defer_reconcile_to_host(
                    state=state,
                    action=action,
                    attempt=claimed,
                    lease_token=lease_token,
                    reason="side_effect_probe_required",
                )
            if result.status != "success" and result.retryable and descriptor.side_effecting:
                return self._mark_uncertain(
                    state=state,
                    action=action,
                    attempt=claimed,
                    lease_token=lease_token,
                    result=result,
                    expected_attempt_status="reconciling",
                )
            if result.status == "success":
                self.store.cache_action_result(
                    state.run_id,
                    action.action_id,
                    claimed.idempotency_key,
                    result,
                    lease_token=lease_token,
                )
            return self._finish_attempt(
                state=state,
                workflow=workflow,
                action=action,
                descriptor=descriptor,
                attempt=claimed,
                lease_token=lease_token,
                result=result,
                expected_attempt_status="reconciling",
                allow_retry=allow_retry,
                token_usage=(
                    claimed.result.get("receipt_usage")
                    if host_attempt
                    and isinstance(claimed.result, Mapping)
                    and isinstance(claimed.result.get("receipt_usage"), Mapping)
                    else None
                ),
            )
        finally:
            self.store.release_lease(lease_token)

    def execute(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        action: ActionSpec,
        descriptor: ToolDescriptor,
        *,
        timeout: float,
    ) -> ExecutionOutcome:
        lease_token = self.store.acquire_lease(
            state.run_id,
            action.action_id,
            f"{self.owner}:{uuid4().hex}",
            ttl_seconds=timeout + 30.0,
        )
        if lease_token is None:
            return ExecutionOutcome(False, "action_lease_busy", "action_lease_busy")
        try:
            arguments, idempotency_key, input_hash = self.arguments_for(
                state,
                workflow,
                action,
                descriptor,
                attempt_sequence=state.action_attempts.get(action.action_id, 0) + 1,
            )
            attempt = self._begin_attempt(
                state,
                action,
                descriptor,
                input_hash=input_hash,
                idempotency_key=idempotency_key,
                lease_token=lease_token,
            )
            result = self.store.cached_action_result(
                state.run_id,
                idempotency_key,
                action_id=action.action_id,
            )
            if result is None:
                result = self._with_durable_input_hash(
                    self.broker.call(
                        descriptor,
                        arguments,
                        timeout=timeout,
                        run_id=state.run_id,
                    ), input_hash
                )
                if result.status == "success":
                    self.store.cache_action_result(
                        state.run_id,
                        action.action_id,
                        idempotency_key,
                        result,
                        lease_token=lease_token,
                    )
            elif result.input_hash and result.input_hash != input_hash:
                result = ToolCallResult(
                    status="failed",
                    error="cached_result_input_hash_mismatch",
                    tool=descriptor.qualified_name,
                    input_hash=input_hash,
                    tool_version=descriptor.version,
                )
            if result.status != "success" and result.retryable and descriptor.side_effecting:
                return self._mark_uncertain(
                    state=state,
                    action=action,
                    attempt=attempt,
                    lease_token=lease_token,
                    result=result,
                )
            return self._finish_attempt(
                state=state,
                workflow=workflow,
                action=action,
                descriptor=descriptor,
                attempt=attempt,
                lease_token=lease_token,
                result=result,
            )
        finally:
            self.store.release_lease(lease_token)

    def accept_host_observation(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        action: ActionSpec,
        *,
        output: Any,
        attempt_id: str,
        contract_hash: str,
        usage: Mapping[str, Any] | None = None,
        timeout: float = 120.0,
    ) -> ExecutionOutcome:
        descriptor = ToolDescriptor(
            server="host",
            name="agent-observation",
            description="Host Agent observation accepted through a one-time Runtime receipt.",
            input_schema={"type": "object"},
            capabilities=tuple(action.required_capabilities),
            source="host-receipt",
            healthy=True,
            priority=0,
            version="host-receipt-v1",
            schema_hash=contract_hash,
            side_effecting=True,
            supports_reconcile=False,
        )
        lease_token = self.store.acquire_lease(
            state.run_id,
            action.action_id,
            f"{self.owner}:host:{uuid4().hex}",
            ttl_seconds=timeout,
        )
        if lease_token is None:
            return ExecutionOutcome(False, "action_lease_busy", "action_lease_busy")
        try:
            placeholder = next(
                (item for item in self.store.task_attempts(state.run_id) if item.attempt_id == attempt_id),
                None,
            )
            if placeholder is None or (
                placeholder.run_id,
                placeholder.branch_id,
                placeholder.plan_revision,
                placeholder.action_id,
                placeholder.status,
            ) != (
                state.run_id,
                state.branch_id,
                state.plan_revision,
                action.action_id,
                "consumed",
            ):
                return ExecutionOutcome(False, "host_placeholder_invalid", "host_observation_rejected")
            if placeholder.tool == "host:handoff":
                descriptor = replace(descriptor, name="handoff")
            attempt = self.store.claim_task_attempt(
                placeholder,
                expected_statuses=("consumed",),
                lease_token=lease_token,
                next_status="running",
            )
            if attempt.tool == "host:handoff":
                expected_input_hash = contract_hash
            elif attempt.tool == "host:agent-observation":
                expected_input_hash = hashlib.sha256(contract_hash.encode("utf-8")).hexdigest()
            else:
                waiting = replace(attempt, status="rejected", error="host_placeholder_tool_invalid", finished_at=utc_now())
                self.store.update_task_attempt(waiting, expected_status="running", lease_token=lease_token)
                return ExecutionOutcome(False, waiting.error, "host_observation_rejected")
            if attempt.input_hash != expected_input_hash:
                waiting = replace(attempt, status="rejected", error="host_contract_hash_mismatch", finished_at=utc_now())
                self.store.update_task_attempt(waiting, expected_status="running", lease_token=lease_token)
                return ExecutionOutcome(False, waiting.error, "host_observation_rejected")
            state.status = "running"
            state.current_action_id = action.action_id
            state.action_status[action.action_id] = "running"
            result = self.store.cached_action_result(
                state.run_id,
                attempt.idempotency_key,
                action_id=action.action_id,
            )
            if result is None:
                durable_output = self.credential_projector(output)
                result = ToolCallResult(
                    status="success",
                    output=durable_output,
                    tool=descriptor.qualified_name,
                    started_at=utc_now(),
                    call_id=f"receipt-{attempt_id}",
                    input_hash=expected_input_hash,
                    output_hash=self.broker.canonical_hash(durable_output),
                    tool_version=descriptor.version,
                )
                self.store.cache_action_result(
                    state.run_id,
                    action.action_id,
                    attempt.idempotency_key,
                    result,
                    lease_token=lease_token,
                )
            persisted_usage = (
                attempt.result.get("receipt_usage")
                if isinstance(attempt.result, Mapping)
                and isinstance(attempt.result.get("receipt_usage"), Mapping)
                else None
            )
            return self._finish_attempt(
                state=state,
                workflow=workflow,
                action=action,
                descriptor=descriptor,
                attempt=attempt,
                lease_token=lease_token,
                result=result,
                expected_attempt_status="running",
                token_usage=usage or persisted_usage,
            )
        finally:
            self.store.release_lease(lease_token)

    def accept_external_observation(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        action: ActionSpec,
        *,
        output: Any,
        tool: str = "host-agent",
        usage: Mapping[str, Any] | None = None,
        idempotency_key: str = "",
        timeout: float = 120.0,
    ) -> ExecutionOutcome:
        """Compatibility path for callers that predate one-time handoff receipts."""

        descriptor = ToolDescriptor(
            server="host",
            name="external-observation",
            description=f"External observation supplied by {tool or 'host-agent'}.",
            input_schema={"type": "object"},
            capabilities=tuple(action.required_capabilities),
            source="host-receipt",
            healthy=True,
            priority=0,
            version="host-observation-v1",
            schema_hash="",
            side_effecting=False,
            supports_reconcile=False,
        )
        lease_token = self.store.acquire_lease(
            state.run_id,
            action.action_id,
            f"{self.owner}:external:{uuid4().hex}",
            ttl_seconds=timeout,
        )
        if lease_token is None:
            return ExecutionOutcome(False, "action_lease_busy", "action_lease_busy")
        try:
            durable_output = self.credential_projector(output)
            output_hash = self.broker.canonical_hash(durable_output)
            input_hash = self.broker.canonical_hash(
                {
                    "run_id": state.run_id,
                    "branch_id": state.branch_id,
                    "plan_revision": state.plan_revision,
                    "action_id": action.action_id,
                    "output_hash": output_hash,
                }
            )
            client_key = idempotency_key.strip()
            resolved_idempotency_key = hashlib.sha256(
                (
                    f"external-client\0{state.run_id}\0{state.branch_id}\0{state.plan_revision}\0"
                    f"{action.action_id}\0{client_key}"
                    if client_key
                    else (
                        f"external\0{state.run_id}\0{state.branch_id}\0{state.plan_revision}\0"
                        f"{action.action_id}\0{input_hash}"
                    )
                ).encode("utf-8")
            ).hexdigest()
            existing = next(
                (
                    item
                    for item in self.store.task_attempts(
                        state.run_id,
                        action_id=action.action_id,
                    )
                    if item.branch_id == state.branch_id
                    and item.plan_revision == state.plan_revision
                    and item.idempotency_key == resolved_idempotency_key
                ),
                None,
            )
            if existing is not None:
                if existing.input_hash != input_hash:
                    raise ValueError(
                        f"external_observation_idempotency_conflict:{action.action_id}"
                    )
                if existing.status in {
                    "prepared",
                    "running",
                    "uncertain",
                    "reconciling",
                    "consumed",
                }:
                    return self.reconcile_attempt(
                        state,
                        workflow,
                        existing,
                        timeout=timeout,
                    )
                return ExecutionOutcome(
                    False,
                    "observation_already_recorded",
                    "observation_idempotent_replay",
                )
            if state.action_status.get(action.action_id) in {"completed", "skipped"}:
                return ExecutionOutcome(
                    False,
                    f"action_already_terminal:{action.action_id}",
                    "action_already_terminal",
                )
            attempt = self._begin_attempt(
                state,
                action,
                descriptor,
                input_hash=input_hash,
                idempotency_key=resolved_idempotency_key,
                lease_token=lease_token,
            )
            result = ToolCallResult(
                status="success",
                output=durable_output,
                tool=tool or descriptor.qualified_name,
                started_at=utc_now(),
                call_id=f"external-{attempt.attempt_id}",
                input_hash=input_hash,
                output_hash=output_hash,
                tool_version=descriptor.version,
            )
            self.store.cache_action_result(
                state.run_id,
                action.action_id,
                resolved_idempotency_key,
                result,
                lease_token=lease_token,
            )
            return self._finish_attempt(
                state=state,
                workflow=workflow,
                action=action,
                descriptor=descriptor,
                attempt=attempt,
                lease_token=lease_token,
                result=result,
                token_usage=usage,
            )
        finally:
            self.store.release_lease(lease_token)


__all__ = ["ExecutorActionsMixin"]
