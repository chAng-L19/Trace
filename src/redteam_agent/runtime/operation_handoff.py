from __future__ import annotations

import hashlib
from dataclasses import replace
from typing import Any, Mapping
from uuid import uuid4

from ..core import ModelRequest, ModelResponse, ToolResult, contract_hash as core_contract_hash
from .artifact_store import ArtifactIntegrityError
from .models import ActionSpec, LeaseToken, OperationState, TaskAttempt, WorkflowSpec
from .session_journal import SessionJournal
from .terminal_judge import OperationResult, _goal_contract_payload
from .verifier import SemanticVerifier


class OperationHandoffMixin:

    def _validated_model_observation(
        self,
        *,
        state: OperationState,
        action: ActionSpec,
        request_id: str,
        call_ids: tuple[str, ...],
        handoff_id: str,
    ) -> tuple[Any, Mapping[str, Any]]:
        journal = SessionJournal(self.store)
        request_record = next(
            (item for item in journal.model_requests(state.run_id) if item.request_id == request_id),
            None,
        )
        response_record = next(
            (item for item in journal.model_responses(state.run_id) if item.request_id == request_id),
            None,
        )
        if request_record is None or response_record is None:
            raise ValueError("model_observation_turn_missing")
        request = ModelRequest.from_dict(request_record.request)
        response = ModelResponse.from_dict(response_record.response)
        metadata = request.metadata
        if (
            request.run_id != state.run_id
            or str(metadata.get("action_id") or "") != action.action_id
            or str(metadata.get("branch_id") or "") != state.branch_id
            or int(metadata.get("plan_revision") or 0) != state.plan_revision
            or str(metadata.get("handoff_id") or "") != handoff_id
        ):
            raise ValueError("model_observation_turn_scope_mismatch")
        prompt_projection = {
            "messages": [dict(item) for item in request.messages],
            "tools": [dict(item) for item in request.tools],
            "response_schema": dict(request.response_schema),
            "model": request.model,
            "allow_parallel_tools": request.allow_parallel_tools,
            **({"continuation": dict(request.continuation)} if request.continuation else {}),
        }
        response_projection = response.to_dict()
        response_projection.pop("response_hash", None)
        if (
            core_contract_hash(prompt_projection) != request_record.prompt_hash
            or core_contract_hash(response_projection) != response_record.response_hash
            or response_record.status not in {"completed", "success"}
        ):
            raise ValueError("model_observation_turn_integrity_mismatch")
        if state.model_led and response.structured_output.get("commit_lifecycle_gate") is not True:
            raise ValueError("model_lifecycle_gate_commit_required")
        response_calls = {
            str(item.get("call_id") or item.get("id") or f"call-{index}")
            for index, item in enumerate(response.tool_calls)
            if isinstance(item, Mapping)
        }
        requested_calls = tuple(
            dict.fromkeys(str(item).strip() for item in call_ids if str(item).strip())
        )
        if not requested_calls or not set(requested_calls).issubset(response_calls):
            raise ValueError("model_observation_call_scope_mismatch")
        records = {
            item.call_id: item
            for item in journal.model_observations(state.run_id)
            if item.request_id == request_id and item.action_id == action.action_id
        }
        results: list[ToolResult] = []
        observation_ids: list[str] = []
        for call_id in requested_calls:
            record = records.get(call_id)
            if record is None or record.status != "success":
                raise ValueError(f"model_observation_result_invalid:{call_id}")
            raw = record.observation.get("tool_result")
            if not isinstance(raw, Mapping):
                artifact = record.observation.get("tool_result_artifact")
                artifact_id = (
                    str(artifact.get("artifact_ref") or "")
                    if isinstance(artifact, Mapping)
                    else ""
                )
                try:
                    raw = self.artifacts.read_json(artifact_id, run_id=state.run_id)
                except (ArtifactIntegrityError, KeyError, ValueError) as exc:
                    raise ValueError("model_observation_artifact_invalid") from exc
            if not isinstance(raw, Mapping):
                raise ValueError("model_observation_result_missing")
            result = ToolResult.from_dict(raw)
            output_hash = core_contract_hash(
                {
                    "call_id": result.call_id,
                    "status": result.status,
                    "tool_name": result.tool_name,
                    "output": result.output,
                    "error": result.error,
                    "retryable": result.retryable,
                }
            )
            if (
                result.call_id != record.call_id
                or result.tool_name != record.tool_name
                or result.input_hash != record.input_hash
                or result.output_hash != record.output_hash
                or output_hash != record.output_hash
            ):
                raise ValueError("model_observation_result_integrity_mismatch")
            results.append(result)
            observation_ids.append(record.observation_id)
        output: Any = (
            results[0].output
            if len(results) == 1
            else {"tool_results": [item.to_dict() for item in results]}
        )
        target = state.goal.targets[0] if state.goal.targets else ""
        if isinstance(output, Mapping):
            output = {**dict(output), "target": str(output.get("target") or target)}
        else:
            output = {"results": output, "target": target}
        tool_names = {item.tool_name for item in results}
        source = {
            "request_id": request_id,
            "call_ids": requested_calls,
            "observation_ids": tuple(observation_ids),
            "tool_names": tuple(item.tool_name for item in results),
            "tool_version": ",".join(
                str(item.metadata.get("tool_version") or "unknown") for item in results
            ),
            "side_effecting": any(
                bool(item.get("side_effecting", True))
                for item in request.tools
                if str(item.get("name") or "") in tool_names
            ),
            "input_hash": core_contract_hash([item.input_hash for item in results]),
            "output_hash": core_contract_hash([item.output_hash for item in results]),
        }
        return output, source

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
        if state.model_led:
            contract["action"] = {
                "name": action.name,
                "role": "lifecycle_quality_gate",
                "capability_hints": list(action.required_capabilities),
                "evidence_guidance": dict(action.parameters),
                "commit_required": True,
                "tactics": "model_authored_search_graph",
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
            if raw_token and self.store.validate_handoff(raw_token=raw_token, **pending.identity()):
                if state.status == "running":
                    state.status = "waiting_host"
                    state.current_action_id = action.action_id
                    self.store.save_operation(
                        state, expected_version=state.state_version, lease_token=token,
                        event_type="host_handoff_resumed", event={"action_id": action.action_id},
                    )
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
        new_placeholder = placeholder is None
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
        if new_placeholder or (state.status, state.current_action_id, state.action_status.get(action.action_id)) != (
            "waiting_host", action.action_id, "running"
        ):
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

    def submit_model_observation(
        self,
        *,
        run_id: str,
        request_id: str,
        call_ids: tuple[str, ...],
        handoff_id: str,
        handoff_token: str,
        attempt_id: str,
        contract_hash: str,
        continue_run: bool = True,
        max_actions: int | None = None,
    ) -> OperationResult:
        """Promote only durable, scope-bound ToolPort results through Runtime verification."""

        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        lease = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:model-observation:{uuid4().hex}",
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
            output, source = self._validated_model_observation(
                state=state,
                action=action,
                request_id=request_id,
                call_ids=call_ids,
                handoff_id=handoff_id,
            )
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
                tool="model-loop:" + ",".join(source["tool_names"]),
                usage={"_accounted_request_id": request_id},
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
                usage={"_accounted_request_id": request_id},
                timeout=self._action_timeout(action) + 30.0,
                trusted_model_observation=source,
            )
            if not outcome.progressed and outcome.event_type not in {
                "action_lease_busy",
                "action_host_handoff_required",
                "action_verification_failed",
            }:
                raise ValueError(f"model_observation_rejected:{outcome.reason}")
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
