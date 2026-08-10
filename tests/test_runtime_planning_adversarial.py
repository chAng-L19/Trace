from __future__ import annotations

import math
import sys
from dataclasses import replace
from pathlib import Path

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.adaptive_planner import AdaptivePlanner
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.models import ActionSpec, OperationState, RunBudget, WorkflowSpec
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.plan import PlanDelta, PlanFork, PlanRevision, PlanValidationError
from redteam_agent.runtime.tool_broker import ToolBroker


def _action(
    action_id: str,
    artifact: str,
    *,
    depends_on: tuple[str, ...] = (),
    max_retries: int = 2,
) -> ActionSpec:
    return ActionSpec(
        action_id=action_id,
        name=action_id,
        required_capabilities=(f"fixture.{action_id}",),
        expected_artifact=artifact,
        verifier=artifact,
        depends_on=depends_on,
        max_retries=max_retries,
    )


def _workflow(actions: tuple[ActionSpec, ...]) -> WorkflowSpec:
    return WorkflowSpec(
        workflow_id="planning-adversarial",
        version=1,
        name="Planning adversarial fixture",
        description="fixture",
        match_tags=(),
        actions=actions,
        terminal_predicates=(),
        required_artifacts=tuple(dict.fromkeys(action.expected_artifact for action in actions)),
    )


def test_zero_retry_policy_survives_workflow_and_plan_round_trip() -> None:
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {"id": "zero-retry", "version": 1, "name": "Zero retry"},
            "actions": [
                {
                    "id": "single-attempt",
                    "name": "Single attempt",
                    "capabilities": ["fixture.execute"],
                    "expected_artifact": "reproduction_artifact",
                    "verifier": "reproduction_artifact",
                    "max_retries": 0,
                }
            ],
        }
    )

    assert workflow.actions[0].max_retries == 0
    plan = PlanRevision.from_workflow(run_id="run-zero-retry", workflow=workflow)
    restored = PlanRevision.from_dict(plan.to_dict())
    assert restored == plan
    assert restored.plan_hash == plan.plan_hash


def test_plan_revision_rejects_non_contiguous_parent() -> None:
    workflow = _workflow((_action("inspect", "surface_map"),))

    with pytest.raises(PlanValidationError, match="plan_parent_revision_invalid"):
        PlanRevision(
            plan_id="plan-contiguous",
            run_id="run-contiguous",
            branch_id="main",
            revision=3,
            parent_revision=1,
            actions=workflow.actions,
        )


def test_plan_delta_preserves_fork_origin_across_revisions() -> None:
    workflow = _workflow((_action("inspect", "surface_map"),))
    main = PlanRevision.from_workflow(
        run_id="run-fork",
        workflow=workflow,
        plan_id="plan-fork",
    )
    forked = PlanFork(
        plan_id=main.plan_id,
        run_id=main.run_id,
        source_branch=main.branch_id,
        source_revision=main.revision,
        target_branch="hypothesis-b",
    ).apply(main)
    added = _action("validate", "reproduction_artifact", depends_on=("inspect",))

    revised = PlanDelta(
        plan_id=forked.plan_id,
        run_id=forked.run_id,
        branch_id=forked.branch_id,
        base_revision=forked.revision,
        added_actions=(added,),
    ).apply(forked)

    assert revised.forked_from_branch == main.branch_id
    assert revised.forked_from_revision == main.revision


def test_hypothesis_expansion_caps_override_and_uses_collision_free_branch_ids() -> None:
    workflow = _workflow(
        (
            _action("hypotheses", "hypothesis_queue"),
            _action("validate-a", "reproduction_artifact", depends_on=("hypotheses",)),
            _action("validate-b", "reproduction_artifact", depends_on=("hypotheses",)),
            _action("impact", "impact_proof", depends_on=("validate-a", "validate-b")),
            _action("report", "final_report", depends_on=("impact",)),
        )
    )
    hypotheses = tuple(
        {
            "id": f"hypothesis-{index:02d}",
            "statement": f"statement {index}",
            "priority": "high",
            "evidence_refs": ["surface"],
            "negative_control": "control",
        }
        for index in range(20)
    )

    expanded, added_ids = AdaptivePlanner(max_hypothesis_branches=8).expand_hypotheses(
        workflow,
        hypothesis_action_id="hypotheses",
        hypotheses=hypotheses,
        max_branches=10_000,
    )

    assert len(added_ids) == len(set(added_ids))
    assert len(expanded.actions) == len({action.action_id for action in expanded.actions})
    selected_hypotheses = {
        str(action.parameters.get("hypothesis", {}).get("id"))
        for action in expanded.actions
        if action.expected_artifact == "reproduction_artifact"
    }
    assert selected_hypotheses == {f"hypothesis-{index:02d}" for index in range(8)}
    PlanRevision.from_workflow(run_id="run-expanded", workflow=expanded)


@pytest.mark.parametrize("value", (math.nan, math.inf, -math.inf))
def test_non_finite_budget_numbers_are_rejected_before_runtime_state_changes(value: float) -> None:
    assert ToolBroker._schema_error({"type": "number", "minimum": 0}, value) == "arguments:finite"
    with pytest.raises(ValueError, match="time_limit_seconds_must_be_finite"):
        RunBudget.create(action_limit=4, time_limit_seconds=value)

    budget = RunBudget.create(action_limit=4, time_limit_seconds=30)
    before = budget.to_dict()
    with pytest.raises(ValueError, match="time_limit_seconds_must_be_finite"):
        budget.extend(time_limit_seconds=value)
    assert budget.to_dict() == before
    with pytest.raises(ValueError, match="budget_time_delta_must_be_finite"):
        budget.apply_delta(time_seconds=value)
    assert budget.to_dict() == before

    with pytest.raises(ValueError, match="action_timeout_must_be_finite"):
        WorkflowSpec.from_dict(
            {
                "workflow": {"id": "non-finite-timeout", "version": 1},
                "actions": [
                    {
                        "id": "execute",
                        "capabilities": ["fixture.execute"],
                        "expected_artifact": "reproduction_artifact",
                        "timeout_seconds": value,
                    }
                ],
            }
        )


def test_state_plan_snapshot_cannot_override_immutable_plan_revision(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = _workflow((_action("inspect", "surface_map"),))
    state = OperationState.create(session_id="plan-integrity", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "plan-integrity-test"})
    runtime._ensure_initial_plan(state, workflow)

    forged_action = replace(workflow.actions[0], required_capabilities=("fixture.forged",))
    forged_plan = PlanRevision(
        plan_id=state.plan_id,
        run_id=state.run_id,
        branch_id=state.branch_id,
        revision=state.plan_revision,
        actions=(forged_action,),
        reason="forged-state-snapshot",
    )
    state.plan_snapshot = forged_plan.to_dict()
    runtime.store.save_operation(state, expected_version=state.state_version)

    observed = runtime.status(state.run_id)
    assert observed.state.status == "failed_integrity"
    assert observed.terminal.terminal is True
    assert observed.terminal.success is False
    assert "plan_revision_snapshot_mismatch" in observed.terminal.reason
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None and persisted.status == "running"

    result = runtime.resume(state.run_id, max_actions=1)
    assert result.state.status == "failed_integrity"
    assert result.terminal.terminal is True
    assert result.terminal.success is False
    assert "plan_revision_snapshot_mismatch" in result.terminal.reason
