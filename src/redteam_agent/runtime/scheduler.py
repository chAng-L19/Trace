from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping, Sequence

from .facts import FactLedger
from .models import ActionSpec, FactRecord, OperationState, TaskAttempt, ToolDescriptor, WorkflowSpec
from .tool_broker import ToolBroker


TERMINAL_ATTEMPT_STATUSES = {
    "completed",
    "failed",
    "rejected",
    "cancelled",
    "reconciled",
    "superseded",
    "expired",
}
UNCERTAIN_ATTEMPT_STATUSES = {"prepared", "running", "uncertain", "reconciling", "consumed"}


@dataclass(frozen=True)
class ScheduleDecision:
    action: ActionSpec | None
    descriptor: ToolDescriptor | None = None
    reason: str = ""
    missing_capabilities: tuple[str, ...] = ()
    uncertain_attempt: TaskAttempt | None = None


class Scheduler:
    def __init__(self, broker: ToolBroker) -> None:
        self.broker = broker

    @staticmethod
    def tool_exclusions(state: OperationState, action_id: str) -> tuple[str, ...]:
        return tuple(
            dict.fromkeys(
                (*state.action_tools_tried.get(action_id, ()), *state.action_tools_succeeded.get(action_id, ()))
            )
        )

    @staticmethod
    def uncertain_attempt(attempts: Sequence[TaskAttempt]) -> TaskAttempt | None:
        candidates = [item for item in attempts if item.status in UNCERTAIN_ATTEMPT_STATUSES]
        return max(candidates, key=lambda item: (item.started_at, item.attempt_id), default=None)

    @staticmethod
    def attempts_used(state: OperationState) -> int:
        return sum(max(0, int(value)) for value in state.action_attempts.values())

    @staticmethod
    def _dependency_facts_valid(
        state: OperationState,
        workflow: WorkflowSpec,
        action: ActionSpec,
        facts: Sequence[FactRecord],
    ) -> bool:
        if not facts:
            return True
        effective = FactLedger.effective(facts, run_id=state.run_id, branch_id=state.branch_id)
        by_action = {item.action_id: item for item in workflow.actions}
        target = state.goal.targets[0] if state.goal.targets else ""
        for dependency_id in action.depends_on:
            dependency = by_action.get(dependency_id)
            if dependency is None:
                return False
            key = f"artifact:{dependency.expected_artifact}:{target}"
            if key not in effective:
                return False
        return True

    @staticmethod
    def _rank(
        state: OperationState,
        action: ActionSpec,
        descriptor: ToolDescriptor | None,
    ) -> int:
        stage_priority = {
            "reproduction_artifact": 900,
            "impact_proof": 800,
            "coverage_report": 700,
            "cleanup_proof": 600,
            "hypothesis_queue": 300,
            "final_report": 200,
        }
        risk_penalty = {"safe": 0, "passive": 0, "active_low": 25, "active_medium": 75, "active_high": 200}
        strict = str(state.goal.constraints.get("opsec_level") or "").casefold() == "strict"
        score = 10_000 if descriptor is not None else 0
        score += stage_priority.get(action.expected_artifact, 100)
        score += len(action.depends_on) * 25
        score -= state.action_attempts.get(action.action_id, 0) * 100
        score -= risk_penalty.get(action.risk, 100) * (3 if strict else 1)
        return score

    def next_action(
        self,
        state: OperationState,
        workflow: WorkflowSpec,
        *,
        facts: Sequence[FactRecord] = (),
        attempts: Sequence[TaskAttempt] = (),
    ) -> ScheduleDecision:
        scoped_attempts = tuple(
            item
            for item in attempts
            if item.run_id == state.run_id
            and item.branch_id == state.branch_id
            and item.plan_revision <= state.plan_revision
        )
        uncertain = self.uncertain_attempt(scoped_attempts)
        if uncertain is not None:
            action = next((item for item in workflow.actions if item.action_id == uncertain.action_id), None)
            return ScheduleDecision(
                action=action,
                reason="attempt_reconcile_required",
                uncertain_attempt=uncertain,
                missing_capabilities=("attempt_reconcile",),
            )

        ranked: list[tuple[int, int, str, ActionSpec, ToolDescriptor | None]] = []
        for index, action in enumerate(workflow.actions):
            status = state.action_status.get(action.action_id, "pending")
            if status not in {"pending", "running"}:
                continue
            dependency_statuses = [state.action_status.get(item, "pending") for item in action.depends_on]
            if not all(item in {"completed", "skipped"} for item in dependency_statuses):
                continue
            if not self._dependency_facts_valid(state, workflow, action, facts):
                continue
            exclusions = self.tool_exclusions(state, action.action_id)
            descriptor = self.broker.select(action.required_capabilities, exclude=exclusions)
            ranked.append((-self._rank(state, action, descriptor), index, action.action_id, action, descriptor))

        if not ranked:
            return ScheduleDecision(action=None, reason="no_ready_action")
        ranked.sort(key=lambda item: (item[0], item[1], item[2]))
        _, _, _, action, descriptor = ranked[0]
        if descriptor is None:
            return ScheduleDecision(
                action=action,
                reason="capability_missing",
                missing_capabilities=tuple(action.required_capabilities),
            )
        return ScheduleDecision(action=action, descriptor=descriptor, reason="ready")

    @staticmethod
    def ensemble_satisfied(state: OperationState, action: ActionSpec) -> bool:
        return len(state.action_tools_succeeded.get(action.action_id, ())) >= action.min_tool_results


__all__ = ["ScheduleDecision", "Scheduler", "TERMINAL_ATTEMPT_STATUSES", "UNCERTAIN_ATTEMPT_STATUSES"]
