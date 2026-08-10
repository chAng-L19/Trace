from __future__ import annotations

import hashlib
import re
from typing import Any, Mapping, Sequence
from uuid import uuid4

from .durable_store import ImmutableRecordError
from .intent_rewriter import rewrite_objective
from .models import ActionSpec, OperationState, SuccessPredicate, WorkflowSpec
from .plan import PlanRevision
from .security import find_secret_references, is_secret_reference, project_sensitive


class OperationContractMixin:

    @staticmethod
    def _predicate_payloads(
        predicates: Sequence[SuccessPredicate | Mapping[str, Any]],
    ) -> tuple[Any, ...]:
        return tuple(
            item.__dict__ if isinstance(item, SuccessPredicate) else dict(item)
            for item in predicates
            if isinstance(item, (SuccessPredicate, Mapping))
        )

    def _capture_credentials(self, *values: Any) -> None:
        bindings: dict[str, str] = {}
        for value in values:
            _, discovered = project_sensitive(value)
            bindings.update(discovered)
        self._credential_vault.bind_many(bindings)

    def missing_credential_refs(self, state: OperationState) -> tuple[str, ...]:
        return self._credential_vault.missing(state.credential_refs)

    def bind_credentials(self, run_id: str, bindings: Mapping[str, Any]) -> tuple[str, ...]:
        """Bind durable Secret References to process-local tool-channel values."""

        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        required = set(state.credential_refs)
        supplied = {str(reference): value for reference, value in bindings.items()}
        if any(reference not in required for reference in supplied):
            raise ValueError("credential_binding_reference_unknown")
        self._credential_vault.bind_many(supplied)
        return self.missing_credential_refs(state)

    @staticmethod
    def _credential_dependency(missing: Sequence[str]) -> dict[str, Any]:
        return {
            "dependency_id": "credential_rebind",
            "kind": "process-local-secret-binding",
            "status": "pending",
            "required_refs": list(missing),
            "input_channel": "redteam_run.credential_bindings",
            "durable_secret_material": False,
        }

    def _action_timeout(self, action: ActionSpec) -> float:
        return min(action.timeout_seconds, self.action_timeout_cap) if self.action_timeout_cap is not None else action.timeout_seconds

    def _base_workflow_for(self, state: OperationState) -> WorkflowSpec:
        return WorkflowSpec.from_dict(state.workflow_snapshot) if state.workflow_snapshot else self.registry.get(state.workflow_id)

    def _workflow_for(self, state: OperationState) -> WorkflowSpec:
        base = self._base_workflow_for(state)
        return self.executor.workflow_for_plan(base, self.executor.current_plan(state, base))

    @staticmethod
    def _workflow_integrity_error(state: OperationState, workflow: WorkflowSpec) -> str:
        if state.workflow_version != workflow.version:
            return "workflow_version_mismatch"
        if state.workflow_fingerprint and state.workflow_fingerprint != workflow.fingerprint:
            return "workflow_fingerprint_mismatch"
        return ""

    @staticmethod
    def _goal_rewrite_integrity_error(state: OperationState) -> str:
        envelope = state.goal.intent_envelope
        if not isinstance(envelope, Mapping):
            return "prompt_rewrite_missing"
        expected_state_refs = find_secret_references(
            {"session_id": state.session_id, "goal": state.goal.to_dict()}
        )
        if tuple(sorted(state.credential_refs)) != expected_state_refs:
            return "credential_reference_contract_mismatch"
        raw_targets = envelope.get("targets")
        if not isinstance(raw_targets, (list, tuple)):
            return "prompt_rewrite_targets_invalid"
        rewrite_targets = tuple(str(item) for item in raw_targets if str(item))
        target_binding = envelope.get("target_binding")
        if target_binding not in {"compiled", "pending"}:
            return "prompt_rewrite_target_binding_invalid"
        # An initially targetless run remains ``pending`` after the durable
        # provide_target transition because its original PromptRewrite is
        # immutable.  A rewrite compiled with targets must retain the exact
        # GoalContract target set; clearing it is an integrity failure.
        if target_binding == "compiled" and (
            not rewrite_targets or rewrite_targets != state.goal.targets
        ):
            return "prompt_rewrite_target_mismatch"
        if target_binding == "pending" and rewrite_targets:
            return "prompt_rewrite_target_mismatch"
        redaction = envelope.get("input_redaction")
        if not isinstance(redaction, Mapping):
            return "prompt_rewrite_redaction_metadata_missing"
        canonical_sha256 = hashlib.sha256(state.goal.objective.encode("utf-8")).hexdigest()
        canonical_bytes = len(state.goal.objective.encode("utf-8"))
        applied = redaction.get("applied")
        representation = redaction.get("representation")
        original_sha256 = redaction.get("original_sha256")
        original_bytes = redaction.get("original_bytes")
        objective_refs = find_secret_references(state.goal.objective)
        metadata_refs = redaction.get("credential_refs")
        if (
            not isinstance(applied, bool)
            or representation not in {"original-source", "secret-reference-v1"}
            or not isinstance(original_sha256, str)
            or re.fullmatch(r"[0-9a-f]{64}", original_sha256) is None
            or not isinstance(original_bytes, int)
            or isinstance(original_bytes, bool)
            or original_bytes < 0
            or redaction.get("canonical_sha256") != canonical_sha256
            or redaction.get("canonical_bytes") != canonical_bytes
            or not isinstance(metadata_refs, (list, tuple))
            or tuple(sorted(str(item) for item in metadata_refs)) != objective_refs
            or any(not is_secret_reference(item) for item in metadata_refs)
            or (applied and (representation != "secret-reference-v1" or not objective_refs))
            or (not applied and (representation != "original-source" or objective_refs))
            or (not applied and (original_sha256 != canonical_sha256 or original_bytes != canonical_bytes))
        ):
            return "prompt_rewrite_redaction_metadata_invalid"
        try:
            expected = rewrite_objective(
                state.goal.objective,
                targets=rewrite_targets,
                source_representation=str(representation),
                original_source_sha256=original_sha256,
                original_source_bytes=original_bytes,
            )
        except (TypeError, ValueError):
            return "prompt_rewrite_source_invalid"
        expected_envelope = expected.to_dict()
        expected_envelope["fingerprint"] = expected.fingerprint
        expected_envelope["target_binding"] = target_binding
        expected_envelope["input_redaction"] = dict(redaction)
        expected_envelope["credential_refs"] = list(
            find_secret_references(
                {
                    "objective": state.goal.objective,
                    "targets": rewrite_targets,
                    "starting_context": state.goal.starting_context,
                    "constraints": state.goal.constraints,
                    "success_predicates": [predicate.__dict__ for predicate in state.goal.success_predicates],
                }
            )
        )
        if dict(envelope) != expected_envelope:
            return "prompt_rewrite_contract_mismatch"
        return ""

    @staticmethod
    def _budget_values(
        constraints: Mapping[str, Any] | None,
        *,
        token_limit: int | None,
        time_limit_seconds: float | None,
        deadline: str,
    ) -> tuple[int | None, float | None, str]:
        values = dict(constraints or {})
        raw_tokens = token_limit if token_limit is not None else values.get("token_limit", values.get("token_budget"))
        raw_time = time_limit_seconds if time_limit_seconds is not None else values.get(
            "time_limit_seconds", values.get("time_budget_seconds")
        )
        resolved_deadline = deadline or str(values.get("deadline") or values.get("deadline_at") or "")
        try:
            tokens = None if raw_tokens in (None, "") else max(1, int(raw_tokens))
        except (TypeError, ValueError, OverflowError):
            tokens = None
        try:
            seconds = None if raw_time in (None, "") else max(0.1, float(raw_time))
        except (TypeError, ValueError, OverflowError):
            seconds = None
        return tokens, seconds, resolved_deadline

    def _ensure_initial_plan(self, state: OperationState, base_workflow: WorkflowSpec) -> None:
        existing = self.store.plan_revisions(state.run_id, plan_id=state.plan_id, branch_id=state.branch_id)
        if existing:
            first = next((item for item in existing if item.revision == 1), None)
            if first is None or first.actions != base_workflow.actions:
                raise ImmutableRecordError(f"initial_plan_mismatch:{state.run_id}")
            return
        first = PlanRevision.from_workflow(
            run_id=state.run_id, workflow=base_workflow, plan_id=state.plan_id, branch_id=state.branch_id
        )
        try:
            self.store.save_plan_revision(first)
        except ImmutableRecordError:
            existing = self.store.plan_revisions(state.run_id, plan_id=state.plan_id, branch_id=state.branch_id)
            if not any(item.revision == 1 and item.actions == first.actions for item in existing):
                raise

    def _extend_absolute_budget(
        self,
        run_id: str,
        *,
        action_limit: int,
        token_limit: int | None,
        time_limit_seconds: float | None,
        deadline: str,
    ) -> OperationState:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        if not state.budget.extend(
            action_limit=action_limit,
            token_limit=token_limit,
            time_limit_seconds=time_limit_seconds,
            deadline=deadline,
        ):
            return state
        token = self.store.acquire_lease(run_id, "__operation__", f"{self.owner}:budget:{uuid4().hex}", ttl_seconds=30)
        if token is None:
            return self.store.load_operation(run_id) or state
        try:
            current = self.store.load_operation(run_id) or state
            if current.budget.extend(
                action_limit=action_limit,
                token_limit=token_limit,
                time_limit_seconds=time_limit_seconds,
                deadline=deadline,
            ):
                if current.status == "paused_budget" and not current.budget.exhaustion_reason():
                    current.status = "running"
                self.store.save_operation(
                    current,
                    expected_version=current.state_version,
                    lease_token=token,
                    event_type="budget_extended",
                    event={"budget": current.budget.to_dict()},
                )
            return current
        finally:
            self.store.release_lease(token)

