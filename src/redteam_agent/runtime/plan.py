from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field, replace
from typing import Any, Mapping, Sequence
from uuid import uuid4

from .models import ActionSpec, WorkflowSpec, utc_now


class PlanValidationError(ValueError):
    pass


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


__all__ = [
    "Fork",
    "PlanDelta",
    "PlanFork",
    "PlanRevision",
    "PlanValidationError",
]
