from __future__ import annotations

import hashlib
import json
import sys
from dataclasses import replace
from pathlib import Path

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.intent_rewriter import rewrite_objective
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.models import OperationState, WorkflowSpec
from redteam_agent.runtime.operation_runtime import OperationRuntime


def test_clause_contracts_index_actions_deliverables_constraints_and_targets() -> None:
    target = r"E:\fixtures\sample.bin"
    objective = (
        f"检查 {target} 并输出报告；"
        "不要只给方案，继续执行修复并验证；"
        "保留回滚副本"
    )

    rewrite = rewrite_objective(objective, targets=(target,))

    assert rewrite.source_text == objective
    assert rewrite.action_kind == "execute"
    assert len(rewrite.clause_contracts) == len(rewrite.clauses)
    assert [item["clause_id"] for item in rewrite.clause_contracts] == list(rewrite.clause_ids)
    assert rewrite.clause_contracts[0]["targets"] == [target]
    assert {"inspect", "report"}.issubset(rewrite.clause_contracts[0]["actions"])
    assert "report" in rewrite.clause_contracts[0]["deliverables"]
    assert {"execute", "validate", "repair"}.issubset(rewrite.clause_contracts[1]["actions"])
    assert rewrite.clause_contracts[1]["constraints"] == [rewrite.clauses[1]]
    assert "rollback" in rewrite.clause_contracts[2]["actions"]
    assert all(item["source_text"] in rewrite.execution_prompt for item in rewrite.clause_contracts)


def test_plan_only_detection_scopes_negation_to_the_execution_occurrence() -> None:
    execute = rewrite_objective("不要只给方案，继续执行修复并验证")
    plan = rewrite_objective("只给方案，不要执行或修改文件")

    assert execute.action_kind == "execute"
    assert execute.execution_required is True
    assert plan.action_kind == "plan"
    assert plan.execution_required is False


@pytest.mark.parametrize(
    "objective",
    (
        "不要只给方案，直接做",
        "别只给方案，动手搞定",
        "do not only give me a plan, just do it",
        "don't give me a plan first; go ahead and make the changes",
        "I don't want you to only give me a plan; start working",
        "do not only give me a plan first",
        "do not merely give me a plan first",
    ),
)
def test_negated_plan_only_phrase_cannot_downgrade_an_execution_request(objective: str) -> None:
    rewrite = rewrite_objective(objective)

    assert rewrite.action_kind == "execute"
    assert rewrite.execution_required is True


@pytest.mark.parametrize(
    "objective",
    (
        "不要只给方案，直接做",
        "别只给方案，动手搞定",
        "do not only give me a plan, just do it",
        "don't give me a plan first; go ahead and make the changes",
        "I don't want you to only give me a plan; start working",
    ),
)
def test_explicit_positive_action_after_a_negated_plan_phrase_is_preserved(objective: str) -> None:
    rewrite = rewrite_objective(objective)

    assert {"execute", "implement"}.intersection(rewrite.verbs)


@pytest.mark.parametrize(
    "objective",
    (
        "先给我方案，暂不修改文件，不用执行测试",
        "方案即可，先别改代码，也不必运行验证",
        "Give me a plan first; do not make changes yet and no need to run tests",
        "Plan only; avoid modifying files or executing tests",
    ),
)
def test_plan_only_rewrite_does_not_turn_negated_actions_into_execution_contracts(objective: str) -> None:
    rewrite = rewrite_objective(objective)

    assert rewrite.action_kind == "plan"
    assert rewrite.execution_required is False
    assert not {"transform", "execute", "validate"}.intersection(rewrite.verbs)
    assert all(
        "reproduction_artifact" not in contract["required_artifacts"]
        for contract in rewrite.clause_contracts
    )
    assert all(contract["required_artifacts"] for contract in rewrite.clause_contracts)


@pytest.mark.parametrize(
    "target",
    (
        r"E:\pytest-rewrite-current\test-report.bin",
        "https://target.invalid/modify-and-test/report",
    ),
)
def test_explicit_target_literals_cannot_inject_actions_into_plan_only_rewrite(target: str) -> None:
    objective = f"先给我针对 {target} 的方案，暂不修改文件，不用执行测试"

    rewrite = rewrite_objective(objective, targets=(target,))

    assert rewrite.action_kind == "plan"
    assert rewrite.execution_required is False
    assert rewrite.verbs == ()
    assert rewrite.deliverables == ("plan",)
    assert all(contract["required_artifacts"] == ["hypothesis_queue"] for contract in rewrite.clause_contracts)


def test_clause_split_does_not_treat_delimiters_inside_explicit_target_as_user_syntax() -> None:
    target = "https://target.invalid/rewrite;then,test"
    objective = f"Inspect {target}; write the report"

    rewrite = rewrite_objective(objective, targets=(target,))

    assert rewrite.clauses == (f"Inspect {target}", "write the report")
    assert rewrite.clause_contracts[0]["targets"] == [target]
    assert rewrite.clause_contracts[0]["actions"] == ["inspect"]
    assert rewrite.clause_contracts[1]["actions"] == ["report"]


def test_control_rewrite_preserves_source_with_explicit_control_semantics() -> None:
    objective = "/redteam off"

    rewrite = rewrite_objective(
        objective,
        action_kind_override="control",
        execution_required_override=False,
    )

    assert rewrite.source_text == objective
    assert rewrite.action_kind == "control"
    assert rewrite.execution_required is False
    assert "Mode: control; execution_required=false" in rewrite.execution_prompt


def test_rewrite_rejects_non_control_classification_overrides() -> None:
    with pytest.raises(ValueError, match="action_kind_override_invalid"):
        rewrite_objective("inspect TARGET", action_kind_override="plan")
    with pytest.raises(ValueError, match="execution_required_override_invalid"):
        rewrite_objective("inspect TARGET", execution_required_override=False)


def test_runtime_rejects_a_persisted_goal_with_a_downgraded_rewrite(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations")
    state = runtime.start(
        session_id="rewrite-integrity",
        objective="Inspect https://target.invalid; validate it; write the report",
    )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    downgraded = dict(persisted.goal.intent_envelope)
    downgraded.pop("execution_prompt")
    persisted.goal = replace(persisted.goal, intent_envelope=downgraded)
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    status = runtime.status(state.run_id)
    assert status.state.status == "failed_integrity"
    assert status.terminal.reason == "prompt_rewrite_contract_mismatch"

    resumed = runtime.resume(state.run_id)
    assert resumed.state.status == "failed_integrity"
    assert resumed.terminal.reason == "prompt_rewrite_contract_mismatch"
    stored = runtime.store.load_operation(state.run_id)
    assert stored is not None and stored.status == "failed_integrity"


def test_runtime_rejects_removing_targets_from_a_compiled_rewrite(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations")
    state = runtime.start(
        session_id="rewrite-target-integrity",
        objective="Inspect https://target.invalid; validate it; write the report",
    )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    downgraded = rewrite_objective(persisted.goal.objective, targets=()).to_dict()
    rewrite = rewrite_objective(persisted.goal.objective, targets=())
    downgraded["fingerprint"] = rewrite.fingerprint
    downgraded["target_binding"] = "compiled"
    persisted.goal = replace(persisted.goal, intent_envelope=downgraded)
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    result = runtime.resume(state.run_id)

    assert result.state.status == "failed_integrity"
    assert result.terminal.reason == "prompt_rewrite_target_mismatch"


def test_external_observation_cannot_persist_evidence_after_rewrite_tampering(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "rewrite-observation-integrity",
                "version": 1,
                "name": "Rewrite observation integrity",
                "required_artifacts": ["surface_map"],
            },
            "actions": [
                {
                    "id": "host-surface",
                    "name": "Host surface",
                    "capabilities": ["fixture.host"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
            "terminal_predicates": [],
        }
    )
    state = OperationState.create(session_id="rewrite-observation", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "test"})
    runtime._ensure_initial_plan(state, workflow)
    runtime.resume(state.run_id, max_actions=1)

    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    downgraded = dict(persisted.goal.intent_envelope)
    downgraded.pop("execution_prompt")
    persisted.goal = replace(persisted.goal, intent_envelope=downgraded)
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    with pytest.raises(ValueError, match="prompt_rewrite_contract_mismatch"):
        runtime.submit_observation(
            run_id=state.run_id,
            action_id="host-surface",
            output={"target": goal.targets[0], "assets": ["fixture-service"]},
            continue_run=False,
        )

    assert runtime.store.evidence(state.run_id) == ()
