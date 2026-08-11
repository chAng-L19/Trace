from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from typing import Any, Mapping, Sequence
from uuid import uuid4

from .durable_store import StoreConflictError
from .models import LeaseToken, OperationState, RunBudget, SuccessPredicate, TaskAttempt, WorkflowSpec
from .operation_result import OperationResult
from .plan import PlanRevision
from .security import find_secret_references, project_sensitive


class OperationLifecycleMixin:

    def start(
        self,
        *,
        session_id: str,
        objective: str,
        targets: Sequence[str] | None = None,
        workflow_hint: str = "",
        starting_context: Mapping[str, Any] | None = None,
        constraints: Mapping[str, Any] | None = None,
        success_predicates: Sequence[SuccessPredicate | Mapping[str, Any]] = (),
        max_actions: int = 64,
        max_retries_per_action: int = 2,
        token_limit: int | None = None,
        time_limit_seconds: float | None = None,
        deadline: str = "",
    ) -> OperationState:
        predicate_payloads = self._predicate_payloads(success_predicates)
        self._capture_credentials(
            session_id,
            objective,
            tuple(targets or ()),
            workflow_hint,
            dict(starting_context or {}),
            dict(constraints or {}),
            predicate_payloads,
        )
        durable_session_id, _ = project_sensitive(session_id)
        goal = self.compiler.compile(
            objective,
            targets=targets,
            workflow_hint=workflow_hint,
            starting_context=starting_context,
            constraints=constraints,
            success_predicates=success_predicates,
            max_actions=max_actions,
            max_retries_per_action=max_retries_per_action,
        )
        base_workflow = self.planner.plan(goal, self.registry)
        identity = json.dumps(
            {
                "session_id": session_id,
                "objective": goal.objective,
                "targets": goal.targets,
                "workflow_id": base_workflow.workflow_id,
                "workflow_version": base_workflow.version,
                "workflow_fingerprint": base_workflow.fingerprint,
                "intent_fingerprint": str(goal.intent_envelope.get("fingerprint") or ""),
                "original_source_sha256": str(
                    dict(goal.intent_envelope.get("input_redaction") or {}).get("original_sha256") or ""
                ),
                "original_source_bytes": int(
                    dict(goal.intent_envelope.get("input_redaction") or {}).get("original_bytes") or 0
                ),
                "starting_context": goal.starting_context,
                "constraints": goal.constraints,
                "success_predicates": [predicate.__dict__ for predicate in goal.success_predicates],
                "success_criteria": [criterion.__dict__ for criterion in goal.success_criteria],
            },
            ensure_ascii=False,
            sort_keys=True,
            default=str,
        )
        digest = hashlib.sha256(identity.encode("utf-8")).hexdigest()
        goal = replace(goal, goal_id=f"goal-{digest[:32]}")
        state = OperationState.create(session_id=str(durable_session_id), goal=goal, workflow=base_workflow)
        state.run_id = f"run-{digest[:32]}"
        state.credential_refs = list(
            find_secret_references({"session_id": durable_session_id, "goal": goal.to_dict()})
        )
        first = PlanRevision.from_workflow(
            run_id=state.run_id,
            workflow=base_workflow,
            plan_id=state.plan_id,
            branch_id=state.branch_id,
        )
        state.plan_snapshot = first.to_dict()
        tokens, seconds, resolved_deadline = self._budget_values(
            constraints,
            token_limit=token_limit,
            time_limit_seconds=time_limit_seconds,
            deadline=deadline,
        )
        state.budget = RunBudget.create(
            action_limit=max_actions,
            token_limit=tokens,
            time_limit_seconds=seconds,
            started_at=state.created_at,
            deadline=resolved_deadline,
        )
        created = self.store.create_operation(
            state,
            event={"workflow_id": base_workflow.workflow_id, "objective": goal.objective, "targets": list(goal.targets)},
        )
        self._ensure_initial_plan(created, base_workflow)
        return self._extend_absolute_budget(
            created.run_id,
            action_limit=max_actions,
            token_limit=tokens,
            time_limit_seconds=seconds,
            deadline=resolved_deadline,
        )

    def start_batch(self, **arguments: Any) -> tuple[OperationState, ...]:
        objective = str(arguments["objective"])
        starting_context = arguments.get("starting_context")
        raw_targets = arguments.get("targets")
        resolved_targets = tuple(
            dict.fromkeys(
                str(item).strip()
                for item in (
                    raw_targets
                    or self.compiler.extract_targets(objective)
                    or self.compiler.extract_context_targets(starting_context)
                )
                if str(item).strip()
            )
        )
        if len(resolved_targets) <= 1:
            return (self.start_or_resume(**{**arguments, "targets": resolved_targets}),)
        batch_identity = json.dumps(
            {
                "session_id": arguments["session_id"],
                "objective": objective,
                "targets": resolved_targets,
                "workflow_hint": arguments.get("workflow_hint", ""),
                "starting_context": dict(starting_context or {}),
                "constraints": dict(arguments.get("constraints") or {}),
            },
            ensure_ascii=False,
            sort_keys=True,
            default=str,
        )
        batch_session_id = f"batch-{hashlib.sha256(batch_identity.encode('utf-8')).hexdigest()[:32]}"
        states: list[OperationState] = []
        for index, target in enumerate(resolved_targets, start=1):
            hint = str(arguments.get("workflow_hint") or "") or ",".join(
                self.compiler.workflow_hints_for_target(objective, target)
            )
            states.append(
                self.start_or_resume(
                    **{
                        **arguments,
                        "session_id": f"{batch_session_id}:{index}",
                        "targets": (target,),
                        "workflow_hint": hint,
                        "starting_context": {
                            **dict(starting_context or {}),
                            "batch_session_id": batch_session_id,
                            "parent_session_id": arguments["session_id"],
                            "batch_index": index,
                            "batch_size": len(resolved_targets),
                        },
                    }
                )
            )
        return tuple(states)

    def start_or_resume(self, **arguments: Any) -> OperationState:
        return self.start(**arguments)

    def provide_target(self, run_id: str, *, targets: Sequence[str]) -> OperationState:
        """Late-bind the sole missing target without recompiling the goal.

        The original objective, rewrite fingerprint, clause IDs, criteria, and
        run identity are immutable.  A waiting run accepts exactly one target
        because one OperationState executes one target; callers that need a
        batch must start a batch explicitly.
        """

        self._capture_credentials(tuple(targets))
        projected_targets, _ = project_sensitive(tuple(targets))
        resolved_targets = tuple(
            dict.fromkeys(str(item).strip() for item in projected_targets if str(item).strip())
        )
        if len(resolved_targets) != 1:
            raise ValueError("waiting_goal_input_requires_exactly_one_target")
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        if initial.goal.targets:
            if initial.goal.targets == resolved_targets:
                return initial
            raise ValueError("goal_target_already_bound")
        if initial.status != "waiting_goal_input":
            raise ValueError(f"goal_input_not_pending:{initial.status}")
        workflow = self._workflow_for(initial)
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:goal-input:{uuid4().hex}",
            ttl_seconds=self._operation_lease_ttl(workflow),
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.load_operation(run_id) or initial
            if state.goal.targets:
                if state.goal.targets == resolved_targets:
                    return state
                raise ValueError("goal_target_already_bound")
            if state.status != "waiting_goal_input":
                raise ValueError(f"goal_input_not_pending:{state.status}")
            state.goal = replace(state.goal, targets=resolved_targets)
            state.credential_refs = list(
                dict.fromkeys((*state.credential_refs, *find_secret_references(resolved_targets)))
            )
            state.status = "running"
            state.current_action_id = ""
            self.store.save_operation(
                state,
                expected_version=state.state_version,
                lease_token=token,
                event_type="goal_target_supplied",
                event={"target": resolved_targets[0]},
            )
            return state
        finally:
            self.store.release_lease(token)

    def apply_budget_delta(
        self,
        run_id: str,
        *,
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
        acknowledge_missing_usage: bool = False,
    ) -> OperationResult:
        if actions < 0 or tokens < 0 or time_seconds < 0:
            raise ValueError("budget_delta_must_be_nonnegative")
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        token = self.store.acquire_lease(
            run_id, "__operation__", f"{self.owner}:budget-delta:{uuid4().hex}", ttl_seconds=30
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.load_operation(run_id) or initial
            if state.budget.apply_delta(
                actions=actions,
                tokens=tokens,
                time_seconds=time_seconds,
                deadline=deadline,
                acknowledge_missing_usage=acknowledge_missing_usage,
            ):
                if state.status == "paused_budget" and not state.budget.exhaustion_reason():
                    state.status = "running"
                self.store.save_operation(
                    state,
                    expected_version=state.state_version,
                    lease_token=token,
                    event_type="budget_delta_applied",
                    event={
                        "actions": actions,
                        "tokens": tokens,
                        "time_seconds": time_seconds,
                        "deadline": deadline,
                        "acknowledge_missing_usage": acknowledge_missing_usage,
                    },
                )
            return self._result(state, self._workflow_for(state))
        finally:
            self.store.release_lease(token)

    def apply_budget_delta_once(
        self,
        run_id: str,
        *,
        idempotency_key: str,
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
        acknowledge_missing_usage: bool = False,
    ) -> OperationResult:
        if actions < 0 or tokens < 0 or time_seconds < 0:
            raise ValueError("budget_delta_must_be_nonnegative")
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:budget-delta-once:{uuid4().hex}",
            ttl_seconds=30,
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.apply_budget_delta_once(
                run_id,
                actions=actions,
                tokens=tokens,
                time_seconds=time_seconds,
                deadline=deadline,
                acknowledge_missing_usage=acknowledge_missing_usage,
                idempotency_key=idempotency_key,
                lease_token=token,
            )
            return self._result(state, self._workflow_for(state))
        finally:
            self.store.release_lease(token)

    def apply_budget_delta_batch(
        self,
        run_ids: Sequence[str],
        *,
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
        acknowledge_missing_usage: bool = False,
    ) -> tuple[OperationState, ...]:
        """Acquire every operation fence before atomically changing a batch."""

        if actions < 0 or tokens < 0 or time_seconds < 0:
            raise ValueError("budget_delta_must_be_nonnegative")
        ordered = tuple(dict.fromkeys(str(run_id) for run_id in run_ids if str(run_id)))
        if not ordered:
            return ()
        leases: dict[str, LeaseToken] = {}
        try:
            for run_id in sorted(ordered):
                lease = self.store.acquire_lease(
                    run_id,
                    "__operation__",
                    f"{self.owner}:batch-budget:{uuid4().hex}",
                    ttl_seconds=30,
                )
                if lease is None:
                    raise ValueError(f"operation_busy:{run_id}")
                leases[run_id] = lease
            return self.store.apply_budget_delta_batch(
                ordered,
                lease_tokens=leases,
                actions=actions,
                tokens=tokens,
                time_seconds=time_seconds,
                deadline=deadline,
                acknowledge_missing_usage=acknowledge_missing_usage,
            )
        finally:
            for lease in leases.values():
                self.store.release_lease(lease)

    def record_model_usage(
        self,
        run_id: str,
        *,
        request_id: str,
        usage: Mapping[str, Any] | None,
    ) -> OperationResult:
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:model-usage:{uuid4().hex}",
            ttl_seconds=30,
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.record_model_usage_once(
                run_id,
                request_id=request_id,
                usage=usage,
                lease_token=token,
            )
            return self._result(state, self._workflow_for(state))
        finally:
            self.store.release_lease(token)

    def enforce_budget(self, run_id: str) -> OperationResult:
        initial = self.store.load_operation(run_id)
        if initial is None:
            raise KeyError(f"operation_not_found:{run_id}")
        token = self.store.acquire_lease(
            run_id,
            "__operation__",
            f"{self.owner}:budget-enforce:{uuid4().hex}",
            ttl_seconds=30,
        )
        if token is None:
            raise ValueError(f"operation_busy:{run_id}")
        try:
            state = self.store.enforce_budget(run_id, lease_token=token)
            return self._result(state, self._workflow_for(state))
        finally:
            self.store.release_lease(token)

    def _operation_lease_ttl(self, workflow: WorkflowSpec) -> float:
        return max((self._action_timeout(action) for action in workflow.actions), default=60.0) + 120.0

    def _renew_operation_lease(self, token: LeaseToken, *, ttl: float) -> LeaseToken:
        renewed = self.store.renew_lease(token, ttl_seconds=ttl)
        if renewed is None:
            raise StoreConflictError(f"operation_lease_lost:{token.run_id}:{token.fencing_token}")
        return renewed

    def _sync_attempt_counters(self, state: OperationState, attempts: Sequence[TaskAttempt]) -> bool:
        counts: dict[str, int] = {}
        for attempt in attempts:
            if attempt.branch_id == state.branch_id:
                counts[attempt.action_id] = counts.get(attempt.action_id, 0) + 1
        changed = False
        for action_id, count in counts.items():
            if state.action_attempts.get(action_id, 0) < count:
                state.action_attempts[action_id] = count
                changed = True
        # The budget is run-wide, while action counters are branch-local.  Count
        # every durable attempt so a fork cannot reset the global action budget.
        used = max(sum(state.action_attempts.values()), len(attempts))
        if state.budget.actions_used < used:
            state.budget.actions_used = used
            changed = True
        return changed
