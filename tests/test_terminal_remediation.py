from __future__ import annotations

import math
import sys
from pathlib import Path

import pytest


REPO_ROOT = Path(__file__).resolve().parents[1]
CODEX_ROOT = REPO_ROOT / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import GoalCompiler, OperationRuntime, SuccessPredicate, TerminalDecision


def test_goal_compiler_rejects_unsupported_terminal_contracts() -> None:
    compiler = GoalCompiler()

    with pytest.raises(ValueError, match="success_predicate_kind_unsupported:unknown"):
        compiler.compile(
            "Inspect TARGET",
            targets=("TARGET",),
            success_predicates=(SuccessPredicate(kind="unknown"),),
        )
    with pytest.raises(ValueError, match="success_predicate_operator_unsupported:matches"):
        compiler.compile(
            "Inspect TARGET",
            targets=("TARGET",),
            success_predicates=(
                SuccessPredicate(kind="artifact_field", subject="final_report.goal_result", operator="matches"),
            ),
        )
    with pytest.raises(ValueError, match="success_predicate_subject_invalid:artifact_field"):
        compiler.compile(
            "Inspect TARGET",
            targets=("TARGET",),
            success_predicates=(SuccessPredicate(kind="artifact_field", subject="final_report"),),
        )
    with pytest.raises(ValueError, match="success_predicate_artifact_unsupported:unknown_artifact"):
        compiler.compile(
            "Inspect TARGET",
            targets=("TARGET",),
            success_predicates=(
                SuccessPredicate(kind="artifact_verified", subject="unknown_artifact"),
            ),
        )
    with pytest.raises(ValueError, match="success_predicate_artifact_unreachable_in_plan:impact_proof"):
        compiler.compile(
            "Only give me a plan for TARGET",
            targets=("TARGET",),
            success_predicates=(SuccessPredicate(kind="artifact_verified", subject="impact_proof"),),
        )
    with pytest.raises(ValueError, match="success_predicate_value_invalid:artifact_count"):
        compiler.compile(
            "Inspect TARGET",
            targets=("TARGET",),
            success_predicates=(
                SuccessPredicate(
                    kind="artifact_count",
                    subject="final_report",
                    operator="gte",
                    value=math.nan,
                ),
            ),
        )


def test_completed_dag_with_missing_evidence_schedules_executable_remediation(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="terminal-remediation",
        objective="Inspect TARGET and report the verified result",
        targets=("TARGET",),
        max_actions=16,
    )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    persisted.action_status = {action_id: "completed" for action_id in persisted.action_status}
    persisted.status = "running"
    persisted.current_action_id = ""
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    result = runtime.resume(state.run_id, max_actions=1)
    summary = result.summary()

    assert result.state.status == "waiting_host"
    assert result.next_action == "map-surface"
    assert summary["next_action_spec"] is not None
    assert summary["next_action_spec"]["action_id"] == "map-surface"
    assert summary["next_action_spec"]["handoff"]
    assert result.state.action_status["map-surface"] == "running"
    assert all(
        status == "pending"
        for action_id, status in result.state.action_status.items()
        if action_id != "map-surface"
    )
    assert any(
        event["event_type"] == "terminal_remediation_scheduled"
        for event in runtime.store.events(state.run_id)
    )


def test_terminal_remediation_reopens_producer_and_descendants_only(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="terminal-remediation-impact",
        objective="Inspect TARGET and report the verified result",
        targets=("TARGET",),
        max_actions=16,
    )
    workflow = runtime._workflow_for(state)
    terminal = TerminalDecision(
        terminal=False,
        success=False,
        reason="goal_predicates_pending",
        missing=("artifact_field:impact_proof.confirmed",),
    )

    action_id, reason = runtime._terminal_remediation_action(state, workflow, terminal)
    reopened = runtime._action_descendants(workflow, action_id)

    assert reason == "goal_predicates_pending"
    assert action_id == "prove-impact"
    assert reopened == ("prove-impact", "review-coverage", "cleanup", "report")
