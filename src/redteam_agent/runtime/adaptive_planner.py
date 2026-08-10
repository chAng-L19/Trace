from __future__ import annotations

from dataclasses import replace
from typing import Any, Mapping, Sequence

from .models import GoalContract, WorkflowSpec
from .plan import PlanDelta, PlanRevision
from .workflow_registry import WorkflowRegistry


class AdaptivePlanner:
    def __init__(self, *, max_domains: int = 7, max_hypothesis_branches: int = 4) -> None:
        del max_domains
        self.max_hypothesis_branches = max(1, min(8, int(max_hypothesis_branches)))

    def plan(self, goal: GoalContract, registry: WorkflowRegistry) -> WorkflowSpec:
        return registry.match(goal)

    def compose(self, workflows: tuple[WorkflowSpec, ...]) -> WorkflowSpec:
        if len(workflows) != 1:
            raise ValueError("single_adaptive_workflow_required")
        return workflows[0]

    def expand_hypotheses(
        self,
        workflow: WorkflowSpec,
        *,
        hypothesis_action_id: str,
        hypotheses: Sequence[Mapping[str, Any]],
        max_branches: int | None = None,
    ) -> tuple[WorkflowSpec, tuple[str, ...]]:
        branch_limit = (
            self.max_hypothesis_branches
            if max_branches is None
            else max(1, min(8, int(max_branches)))
        )
        priority_rank = {"critical": 0, "high": 1, "medium": 2, "low": 3}
        selected = tuple(
            sorted(
                (item for item in hypotheses if isinstance(item, Mapping)),
                key=lambda item: (
                    priority_rank.get(str(item.get("priority") or "").casefold(), 4),
                    str(item.get("id") or ""),
                ),
            )[:branch_limit]
        )
        if not selected:
            return workflow, ()
        direct_validations = tuple(
            action
            for action in workflow.actions
            if hypothesis_action_id in action.depends_on and action.expected_artifact == "reproduction_artifact"
        )
        if not direct_validations:
            return workflow, ()

        actions = list(workflow.actions)
        used_action_ids = {action.action_id for action in actions}
        added_ids: list[str] = []
        report_dependencies: list[str] = []
        multiple_validations = len(direct_validations) > 1
        for validation_position, validation in enumerate(direct_validations, start=1):
            validation_index = next(index for index, action in enumerate(actions) if action.action_id == validation.action_id)
            actions[validation_index] = replace(
                validation,
                parameters={**dict(validation.parameters), "hypothesis": dict(selected[0])},
            )
            if len(selected) == 1:
                continue

            branch_ids = {validation.action_id}
            changed = True
            while changed:
                changed = False
                for candidate in workflow.actions:
                    if candidate.expected_artifact == "final_report" or candidate.action_id in branch_ids:
                        continue
                    if any(dependency in branch_ids for dependency in candidate.depends_on):
                        branch_ids.add(candidate.action_id)
                        changed = True
            branch_actions = tuple(action for action in workflow.actions if action.action_id in branch_ids)
            terminal_ids = tuple(
                action.action_id
                for action in branch_actions
                if not any(action.action_id in candidate.depends_on for candidate in branch_actions)
            )
            for branch_index, hypothesis in enumerate(selected[1:], start=2):
                suffix = (
                    f"--h{branch_index}"
                    if not multiple_validations
                    else f"--v{validation_position}-h{branch_index}"
                )
                id_map: dict[str, str] = {}
                for branch_action in branch_actions:
                    candidate = f"{branch_action.action_id}{suffix}"
                    collision_index = 2
                    while candidate in used_action_ids:
                        candidate = f"{branch_action.action_id}{suffix}-{collision_index}"
                        collision_index += 1
                    id_map[branch_action.action_id] = candidate
                    used_action_ids.add(candidate)
                for action in branch_actions:
                    cloned = replace(
                        action,
                        action_id=id_map[action.action_id],
                        name=f"{action.name} [hypothesis {branch_index}]",
                        depends_on=tuple(id_map.get(item, item) for item in action.depends_on),
                        rollback_action=id_map.get(action.rollback_action, action.rollback_action),
                        parameters={**dict(action.parameters), "hypothesis": dict(hypothesis)},
                        attack_tags=tuple(dict.fromkeys((*action.attack_tags, str(hypothesis.get("id") or suffix)))),
                    )
                    actions.append(cloned)
                    added_ids.append(cloned.action_id)
                report_dependencies.extend(id_map[item] for item in terminal_ids)

        if report_dependencies:
            actions = [
                replace(action, depends_on=tuple(dict.fromkeys((*action.depends_on, *report_dependencies))))
                if action.expected_artifact == "final_report"
                else action
                for action in actions
            ]
        if not added_ids and actions == list(workflow.actions):
            return workflow, ()
        expanded = replace(workflow, actions=tuple(actions))
        return expanded, tuple(added_ids)

    def hypothesis_delta(
        self,
        plan: PlanRevision,
        workflow: WorkflowSpec,
        *,
        hypothesis_action_id: str,
        hypotheses: Sequence[Mapping[str, Any]],
        max_branches: int | None = None,
        fact_versions: Mapping[str, int] | None = None,
    ) -> tuple[PlanDelta | None, tuple[str, ...]]:
        expanded, added_ids = self.expand_hypotheses(
            workflow,
            hypothesis_action_id=hypothesis_action_id,
            hypotheses=hypotheses,
            max_branches=max_branches,
        )
        if expanded.actions == plan.actions:
            return None, ()
        base_by_id = {action.action_id: action for action in plan.actions}
        expanded_by_id = {action.action_id: action for action in expanded.actions}
        removed = tuple(action_id for action_id in base_by_id if action_id not in expanded_by_id)
        added = tuple(action for action in expanded.actions if action.action_id not in base_by_id)
        replaced = tuple(
            action
            for action in expanded.actions
            if action.action_id in base_by_id and action != base_by_id[action.action_id]
        )
        delta = PlanDelta(
            plan_id=plan.plan_id,
            run_id=plan.run_id,
            branch_id=plan.branch_id,
            base_revision=plan.revision,
            added_actions=added,
            replaced_actions=replaced,
            removed_action_ids=removed,
            fact_versions=dict(fact_versions or {}),
            reason=f"hypotheses:{hypothesis_action_id}",
        )
        return delta, added_ids
