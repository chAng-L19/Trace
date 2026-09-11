from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field, replace
from typing import Any, Mapping, Sequence
from uuid import uuid4

from .facts import FactLedger
from .model_state import FactRecord, OperationState, TaskAttempt, ToolDescriptor
from .models import ActionSpec, WorkflowSpec, utc_now
from .tool_broker import ToolBroker


class PlanValidationError(ValueError):
    pass


@dataclass(frozen=True)
class ExecutionOutcome:
    progressed: bool
    reason: str
    event_type: str
    evidence: Any = None
    fact: Any = None
    review: Any = None
    plan: "PlanRevision | None" = None
    added_action_ids: tuple[str, ...] = ()


def _canonical(payload: Any) -> str:
    return json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str)


def _validate_action_graph(actions: Sequence[ActionSpec]) -> None:
    action_ids = [action.action_id for action in actions]
    if any(not action_id for action_id in action_ids):
        raise PlanValidationError("plan_action_id_required")
    if len(action_ids) != len(set(action_ids)):
        raise PlanValidationError("plan_action_id_duplicate")
    known = set(action_ids)
    for action in actions:
        missing = set(action.depends_on) - known
        if missing:
            raise PlanValidationError(f"plan_dependency_missing:{action.action_id}:{','.join(sorted(missing))}")
        if action.action_id in action.depends_on:
            raise PlanValidationError(f"plan_self_dependency:{action.action_id}")
        if action.rollback_action and action.rollback_action not in known:
            raise PlanValidationError(f"plan_rollback_missing:{action.action_id}:{action.rollback_action}")

    visiting: set[str] = set()
    visited: set[str] = set()
    dependencies = {action.action_id: action.depends_on for action in actions}

    def visit(action_id: str) -> None:
        if action_id in visiting:
            raise PlanValidationError(f"plan_dependency_cycle:{action_id}")
        if action_id in visited:
            return
        visiting.add(action_id)
        for dependency in dependencies[action_id]:
            visit(dependency)
        visiting.remove(action_id)
        visited.add(action_id)

    for action_id in action_ids:
        visit(action_id)


@dataclass(frozen=True)
class PlanRevision:
    plan_id: str
    run_id: str
    branch_id: str
    revision: int
    actions: tuple[ActionSpec, ...]
    parent_revision: int = 0
    forked_from_branch: str = ""
    forked_from_revision: int = 0
    fact_versions: Mapping[str, int] = field(default_factory=dict)
    reason: str = ""
    created_at: str = field(default_factory=utc_now)
    plan_hash: str = ""

    def __post_init__(self) -> None:
        if not self.plan_id or not self.run_id or not self.branch_id:
            raise PlanValidationError("plan_identity_required")
        if self.revision < 1:
            raise PlanValidationError("plan_revision_invalid")
        expected_parent = 0 if self.revision == 1 else self.revision - 1
        if self.parent_revision != expected_parent:
            raise PlanValidationError("plan_parent_revision_invalid")
        if self.forked_from_revision < 0:
            raise PlanValidationError("plan_fork_revision_invalid")
        if bool(self.forked_from_branch) != bool(self.forked_from_revision):
            raise PlanValidationError("plan_fork_lineage_incomplete")
        if self.forked_from_branch == self.branch_id:
            raise PlanValidationError("plan_fork_branch_cycle")
        _validate_action_graph(self.actions)
        calculated = self.fingerprint
        if self.plan_hash and self.plan_hash != calculated:
            raise PlanValidationError("plan_hash_mismatch")
        object.__setattr__(self, "plan_hash", calculated)

    @property
    def fingerprint(self) -> str:
        payload = {
            "plan_id": self.plan_id,
            "run_id": self.run_id,
            "branch_id": self.branch_id,
            "revision": self.revision,
            "parent_revision": self.parent_revision,
            "forked_from_branch": self.forked_from_branch,
            "forked_from_revision": self.forked_from_revision,
            "fact_versions": dict(self.fact_versions),
            "actions": [asdict(action) for action in self.actions],
        }
        return hashlib.sha256(_canonical(payload).encode("utf-8")).hexdigest()

    @classmethod
    def from_workflow(
        cls,
        *,
        run_id: str,
        workflow: WorkflowSpec,
        plan_id: str = "",
        branch_id: str = "main",
    ) -> "PlanRevision":
        resolved_id = plan_id or f"plan-{workflow.fingerprint[:32]}"
        return cls(
            plan_id=resolved_id,
            run_id=run_id,
            branch_id=branch_id,
            revision=1,
            actions=workflow.actions,
            reason=f"workflow:{workflow.workflow_id}@{workflow.version}",
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "PlanRevision":
        raw_actions = payload.get("actions")
        if not isinstance(raw_actions, (list, tuple)):
            workflow = payload.get("workflow")
            raw_actions = workflow.get("actions", ()) if isinstance(workflow, Mapping) else ()
        return cls(
            plan_id=str(payload.get("plan_id") or ""),
            run_id=str(payload.get("run_id") or ""),
            branch_id=str(payload.get("branch_id") or "main"),
            revision=max(1, int(payload.get("revision") or 1)),
            actions=tuple(
                item if isinstance(item, ActionSpec) else ActionSpec.from_dict(item)
                for item in raw_actions
                if isinstance(item, (ActionSpec, Mapping))
            ),
            parent_revision=max(0, int(payload.get("parent_revision") or 0)),
            forked_from_branch=str(payload.get("forked_from_branch") or ""),
            forked_from_revision=max(0, int(payload.get("forked_from_revision") or 0)),
            fact_versions={str(key): max(0, int(value)) for key, value in dict(payload.get("fact_versions") or {}).items()},
            reason=str(payload.get("reason") or ""),
            created_at=str(payload.get("created_at") or utc_now()),
            plan_hash=str(payload.get("plan_hash") or ""),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "plan_id": self.plan_id,
            "run_id": self.run_id,
            "branch_id": self.branch_id,
            "revision": self.revision,
            "actions": [asdict(action) for action in self.actions],
            "parent_revision": self.parent_revision,
            "forked_from_branch": self.forked_from_branch,
            "forked_from_revision": self.forked_from_revision,
            "fact_versions": dict(self.fact_versions),
            "reason": self.reason,
            "created_at": self.created_at,
            "plan_hash": self.plan_hash,
        }


@dataclass(frozen=True)
class PlanDelta:
    plan_id: str
    run_id: str
    branch_id: str
    base_revision: int
    added_actions: tuple[ActionSpec, ...] = ()
    replaced_actions: tuple[ActionSpec, ...] = ()
    removed_action_ids: tuple[str, ...] = ()
    fact_versions: Mapping[str, int] = field(default_factory=dict)
    reason: str = ""
    delta_id: str = field(default_factory=lambda: f"delta-{uuid4().hex}")
    created_at: str = field(default_factory=utc_now)

    def __post_init__(self) -> None:
        if not self.plan_id or not self.run_id or not self.branch_id or self.base_revision < 1:
            raise PlanValidationError("plan_delta_identity_invalid")
        added = [action.action_id for action in self.added_actions]
        replaced = [action.action_id for action in self.replaced_actions]
        removed = list(self.removed_action_ids)
        if len(added) != len(set(added)) or len(replaced) != len(set(replaced)) or len(removed) != len(set(removed)):
            raise PlanValidationError("plan_delta_duplicate_action")
        if set(added) & (set(replaced) | set(removed)) or set(replaced) & set(removed):
            raise PlanValidationError("plan_delta_conflicting_action")

    def apply(self, base: PlanRevision) -> PlanRevision:
        if (base.plan_id, base.run_id, base.branch_id, base.revision) != (
            self.plan_id,
            self.run_id,
            self.branch_id,
            self.base_revision,
        ):
            raise PlanValidationError("plan_delta_base_mismatch")
        actions = {action.action_id: action for action in base.actions}
        for action_id in self.removed_action_ids:
            if action_id not in actions:
                raise PlanValidationError(f"plan_delta_remove_missing:{action_id}")
            actions.pop(action_id)
        for action in self.replaced_actions:
            if action.action_id not in actions:
                raise PlanValidationError(f"plan_delta_replace_missing:{action.action_id}")
            actions[action.action_id] = action
        for action in self.added_actions:
            if action.action_id in actions:
                raise PlanValidationError(f"plan_delta_add_exists:{action.action_id}")
            actions[action.action_id] = action
        return PlanRevision(
            plan_id=base.plan_id,
            run_id=base.run_id,
            branch_id=base.branch_id,
            revision=base.revision + 1,
            parent_revision=base.revision,
            forked_from_branch=base.forked_from_branch,
            forked_from_revision=base.forked_from_revision,
            actions=tuple(actions.values()),
            fact_versions={**dict(base.fact_versions), **dict(self.fact_versions)},
            reason=self.reason or self.delta_id,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "plan_id": self.plan_id,
            "run_id": self.run_id,
            "branch_id": self.branch_id,
            "base_revision": self.base_revision,
            "added_actions": [asdict(action) for action in self.added_actions],
            "replaced_actions": [asdict(action) for action in self.replaced_actions],
            "removed_action_ids": list(self.removed_action_ids),
            "fact_versions": dict(self.fact_versions),
            "reason": self.reason,
            "delta_id": self.delta_id,
            "created_at": self.created_at,
        }


@dataclass(frozen=True)
class PlanFork:
    plan_id: str
    run_id: str
    source_branch: str
    source_revision: int
    target_branch: str
    reason: str = ""
    fork_id: str = field(default_factory=lambda: f"fork-{uuid4().hex}")
    created_at: str = field(default_factory=utc_now)

    def apply(self, base: PlanRevision) -> PlanRevision:
        if (base.plan_id, base.run_id, base.branch_id, base.revision) != (
            self.plan_id,
            self.run_id,
            self.source_branch,
            self.source_revision,
        ):
            raise PlanValidationError("plan_fork_source_mismatch")
        if not self.target_branch or self.target_branch == self.source_branch:
            raise PlanValidationError("plan_fork_target_invalid")
        return PlanRevision(
            plan_id=base.plan_id,
            run_id=base.run_id,
            branch_id=self.target_branch,
            revision=1,
            actions=tuple(replace(action) for action in base.actions),
            forked_from_branch=base.branch_id,
            forked_from_revision=base.revision,
            fact_versions=dict(base.fact_versions),
            reason=self.reason or self.fork_id,
        )


Fork = PlanFork


TERMINAL_ATTEMPT_STATUSES = frozenset(
    {"completed", "failed", "rejected", "cancelled", "reconciled", "superseded", "expired"}
)
UNCERTAIN_ATTEMPT_STATUSES = frozenset({"prepared", "running", "uncertain", "reconciling", "consumed"})


@dataclass(frozen=True)
class NextActionProposal:
    action: ActionSpec | None
    descriptor: ToolDescriptor | None = None
    reason: str = ""
    missing_capabilities: tuple[str, ...] = ()
    uncertain_attempt: TaskAttempt | None = None


class NextActionPolicy:
    """Choose the next ready action without creating a second scheduler state."""

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
        return max(
            (item for item in attempts if item.status in UNCERTAIN_ATTEMPT_STATUSES),
            key=lambda item: (item.started_at, item.attempt_id),
            default=None,
        )

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
            if dependency is None or f"artifact:{dependency.expected_artifact}:{target}" not in effective:
                return False
        return True

    @staticmethod
    def _rank(state: OperationState, action: ActionSpec, descriptor: ToolDescriptor | None) -> int:
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
        score += stage_priority.get(action.expected_artifact, 100) + len(action.depends_on) * 25
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
    ) -> NextActionProposal:
        scoped = tuple(
            item
            for item in attempts
            if item.run_id == state.run_id
            and item.branch_id == state.branch_id
            and item.plan_revision <= state.plan_revision
        )
        uncertain = self.uncertain_attempt(scoped)
        if uncertain is not None:
            action = next((item for item in workflow.actions if item.action_id == uncertain.action_id), None)
            return NextActionProposal(
                action,
                reason="attempt_reconcile_required",
                uncertain_attempt=uncertain,
                missing_capabilities=("attempt_reconcile",),
            )
        ranked: list[tuple[int, int, str, ActionSpec, ToolDescriptor | None]] = []
        for index, action in enumerate(workflow.actions):
            if state.action_status.get(action.action_id, "pending") not in {"pending", "running"}:
                continue
            if not all(state.action_status.get(item, "pending") in {"completed", "skipped"} for item in action.depends_on):
                continue
            if not self._dependency_facts_valid(state, workflow, action, facts):
                continue
            if action.tool_strategy == "capability_coverage" and self.ensemble_satisfied(state, action):
                ranked.append((-self._rank(state, action, None), index, action.action_id, action, None))
                continue
            descriptor = self.broker.select(action.required_capabilities, exclude=self.tool_exclusions(state, action.action_id))
            ranked.append((-self._rank(state, action, descriptor), index, action.action_id, action, descriptor))
        if not ranked:
            return NextActionProposal(None, reason="no_ready_action")
        ranked.sort(key=lambda item: (item[0], item[1], item[2]))
        _, _, _, action, descriptor = ranked[0]
        if descriptor is None:
            return NextActionProposal(action, reason="capability_missing", missing_capabilities=tuple(action.required_capabilities))
        return NextActionProposal(action, descriptor=descriptor, reason="ready")

    @staticmethod
    def ensemble_satisfied(state: OperationState, action: ActionSpec) -> bool:
        return len(state.action_tools_succeeded.get(action.action_id, ())) >= action.min_tool_results


ScheduleDecision = NextActionProposal
Scheduler = NextActionPolicy


__all__ = [
    "Fork",
    "ExecutionOutcome",
    "PlanDelta",
    "PlanFork",
    "PlanRevision",
    "PlanValidationError",
    "NextActionPolicy",
    "NextActionProposal",
    "ScheduleDecision",
    "Scheduler",
    "TERMINAL_ATTEMPT_STATUSES",
    "UNCERTAIN_ATTEMPT_STATUSES",
]
