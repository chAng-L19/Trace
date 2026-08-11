from __future__ import annotations

from ..core import (
    Budget,
    Evidence,
    EvidenceProvenance,
    Goal,
    GoalCriterion,
    Run,
    TerminalDecision,
)
from ..runtime.models import (
    EvidenceNode as LegacyEvidenceNode,
    GoalContract as LegacyGoalContract,
    OperationState,
    TerminalDecision as LegacyTerminalDecision,
)


LEGACY_TO_CORE_STATUS = {
    "waiting_goal_input": "waiting_worker",
    "waiting_host": "waiting_worker",
    "waiting_tools": "waiting_worker",
    "waiting_dependency": "waiting_worker",
    "failed_integrity": "failed",
}


def goal_from_runtime(goal: LegacyGoalContract) -> Goal:
    criteria = tuple(
        GoalCriterion(
            criterion_id=item.criterion_id,
            statement=item.statement,
            target=item.target,
            metadata={"workflow_id": item.workflow_id} if item.workflow_id else {},
        )
        for item in goal.success_criteria
    )
    predicates = tuple(
        {
            "kind": item.kind,
            "subject": item.subject,
            "operator": item.operator,
            "value": item.value,
            "description": item.description,
        }
        for item in goal.success_predicates
    )
    return Goal(
        goal_id=goal.goal_id,
        objective=goal.objective,
        targets=tuple(goal.targets),
        criteria=criteria,
        constraints=dict(goal.constraints),
        success_predicates=predicates,
        evidence_standard=goal.evidence_standard,
        max_actions=goal.max_actions,
        max_retries_per_action=goal.max_retries_per_action,
        metadata={
            "workflow_hint": goal.workflow_hint,
            "workflow_hints": list(goal.workflow_hints),
            "starting_context": dict(goal.starting_context),
            "intent_envelope": dict(goal.intent_envelope),
            "stop_conditions": list(goal.stop_conditions),
            "created_at": goal.created_at,
        },
    )


def budget_from_runtime(state: OperationState) -> Budget:
    budget = state.budget
    return Budget(
        action_limit=budget.action_limit,
        token_limit=budget.token_limit,
        time_limit_seconds=budget.time_limit_seconds,
        actions_used=budget.actions_used,
        output_tokens_used=budget.tokens_used,
        token_usage_missing=budget.token_usage_missing,
        started_at=budget.started_at,
        deadline=budget.deadline,
        pause_reason=budget.pause_reason,
        paused_at=budget.paused_at,
    )


def run_from_runtime(state: OperationState) -> Run:
    status = LEGACY_TO_CORE_STATUS.get(state.status, state.status)
    if status not in {
        "created",
        "running",
        "waiting_worker",
        "paused_budget",
        "cancelling",
        "cancelled",
        "completed",
        "failed",
    }:
        status = "failed"
    return Run(
        run_id=state.run_id,
        session_id=state.session_id,
        goal_id=state.goal.goal_id,
        status=status,
        state_version=state.state_version,
        branch_id=state.branch_id,
        current_search_node_id=state.current_action_id,
        budget=budget_from_runtime(state),
        evidence_ids=tuple(state.evidence_ids),
        created_at=state.created_at,
        updated_at=state.updated_at,
        metadata={
            "legacy_status": state.status,
            "workflow_id": state.workflow_id,
            "workflow_version": state.workflow_version,
            "workflow_fingerprint": state.workflow_fingerprint,
            "plan_id": state.plan_id,
            "plan_revision": state.plan_revision,
            "action_status": dict(state.action_status),
            "action_attempts": dict(state.action_attempts),
            "cleanup_status": state.cleanup_status,
            "terminal_reason": state.terminal_reason,
            "failure_reason": state.failure_reason,
            "cancel_reason": state.cancel_reason,
            "credential_refs": list(state.credential_refs),
            "dependencies": {key: dict(value) for key, value in state.dependencies.items()},
        },
    )


def evidence_from_runtime(node: LegacyEvidenceNode) -> Evidence:
    provenance = None
    if node.provenance is not None:
        source = node.provenance
        provenance = EvidenceProvenance(
            run_id=source.run_id,
            branch_id=source.branch_id,
            plan_revision=source.plan_revision,
            action_id=source.action_id,
            attempt_id=source.attempt_id,
            tool=source.tool,
            tool_version=source.tool_version,
            input_hash=source.input_hash,
            output_hash=source.output_hash,
            parent_ids=tuple(source.parent_ids),
            target=source.target,
            metadata={
                "verifier": source.verifier,
                "verifier_version": source.verifier_version,
                "fact_versions": dict(source.fact_versions),
            },
        )
    return Evidence(
        evidence_id=node.evidence_id,
        run_id=node.run_id,
        artifact_type=node.artifact_type,
        target=node.target,
        action_id=node.action_id,
        tool=node.tool,
        payload=node.payload,
        content_hash=node.content_hash,
        parent_ids=tuple(node.parent_ids),
        verifier=node.verifier,
        confidence=node.confidence,
        verified=node.verified,
        trust=node.trust,
        provenance=provenance,
        created_at=node.created_at,
    )


def terminal_from_runtime(decision: LegacyTerminalDecision) -> TerminalDecision:
    return TerminalDecision(
        terminal=decision.terminal,
        success=decision.success,
        reason=decision.reason,
        satisfied=tuple(decision.satisfied),
        missing=tuple(decision.missing),
    )


def runtime_status_from_core(run: Run) -> str:
    if run.status == "waiting_worker":
        legacy = str(run.metadata.get("legacy_status") or "")
        return legacy if legacy in {"waiting_host", "waiting_tools"} else "waiting_host"
    return run.status


def apply_core_run(state: OperationState, run: Run) -> OperationState:
    if (state.run_id, state.session_id, state.goal.goal_id) != (run.run_id, run.session_id, run.goal_id):
        raise ValueError("core_run_identity_mismatch")
    if state.branch_id != run.branch_id:
        raise ValueError("core_run_branch_transition_requires_runtime")
    if state.current_action_id != run.current_search_node_id:
        raise ValueError("core_run_action_transition_requires_runtime")
    if tuple(state.evidence_ids) != tuple(run.evidence_ids):
        raise ValueError("core_run_evidence_transition_requires_runtime")
    current_status = LEGACY_TO_CORE_STATUS.get(state.status, state.status)
    if run.status != current_status:
        if run.status in {"completed", "failed", "cancelled", "cancelling", "waiting_worker"}:
            raise ValueError("core_run_control_transition_requires_runtime")
        if {current_status, run.status} - {"running", "paused_budget"}:
            raise ValueError("core_run_status_transition_invalid")
    state.status = runtime_status_from_core(run)
    state.budget.action_limit = run.budget.action_limit
    state.budget.token_limit = run.budget.token_limit
    state.budget.time_limit_seconds = run.budget.time_limit_seconds
    state.budget.actions_used = run.budget.actions_used
    token_parts = (run.budget.input_tokens_used, run.budget.output_tokens_used)
    state.budget.tokens_used = (
        None if all(item is None for item in token_parts) else sum(item or 0 for item in token_parts)
    )
    state.budget.token_usage_missing = run.budget.token_usage_missing
    state.budget.started_at = run.budget.started_at
    state.budget.deadline = run.budget.deadline
    state.budget.pause_reason = run.budget.pause_reason
    state.budget.paused_at = run.budget.paused_at
    return state
