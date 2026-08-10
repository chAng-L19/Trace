from __future__ import annotations

import sys
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
CODEX_ROOT = REPO_ROOT / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.operation_runtime import OperationRuntime


def test_plan_only_prompt_contract_completes_without_active_validation(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("fixture input\n", encoding="utf-8")
    runtime = OperationRuntime(root=tmp_path / "plan-operations")
    state = runtime.start(
        session_id="plan-only-session",
        objective=f"先给我针对 {target} 的方案，暂不修改文件，不用执行测试",
        targets=[str(target)],
        max_actions=16,
    )

    completed = runtime.resume(state.run_id, max_actions=16)

    assert completed.state.status == "completed"
    assert completed.terminal.success is True
    assert set(completed.state.action_status) == {"map-surface", "build-hypotheses", "report"}
    assert all(status == "completed" for status in completed.state.action_status.values())
    evidence = runtime.evidence_graph.list(state.run_id)
    assert {node.artifact_type for node in evidence} == {
        "surface_map",
        "hypothesis_queue",
        "final_report",
    }
    report = next(node for node in evidence if node.artifact_type == "final_report")
    assert report.payload["goal_result"] == "achieved"
    assert all(item["status"] == "achieved" for item in report.payload["clause_results"])


def test_operation_runs_from_local_target_through_handoff_to_verified_terminal(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("fixture input\n", encoding="utf-8")
    runtime = OperationRuntime(root=tmp_path / "operations")
    state = runtime.start(
        session_id="end-to-end-session",
        objective=(
            f"Inspect {target}; validate the highest-value path; prove impact; "
            "run a negative control; verify cleanup; write the final report"
        ),
        targets=[str(target)],
        max_actions=32,
    )

    waiting = runtime.resume(state.run_id, max_actions=32)
    waiting_summary = waiting.summary()
    next_spec = waiting_summary["next_action_spec"]

    assert waiting.state.status == "waiting_host"
    assert next_spec["action_id"] == "validate-path"
    assert next_spec["execution_channel"] == "host-agent"
    assert next_spec["handoff"]
    goal_contract = next_spec["goal_contract"]
    assert goal_contract["objective"] == waiting.state.goal.objective
    assert goal_contract["constraints"] == dict(waiting.state.goal.constraints)
    assert goal_contract["success_criteria"]
    assert goal_contract["prompt_rewrite"]["execution_prompt"]
    assert goal_contract["prompt_rewrite"]["clause_contracts"]
    assert goal_contract["prompt_rewrite"]["source_text"] == waiting.state.goal.objective
    assert waiting.state.action_status["map-surface"] == "completed"
    assert waiting.state.action_status["build-hypotheses"] == "completed"

    receipt = next_spec["handoff"]
    clause_contracts = [
        contract
        for contract in waiting.state.goal.intent_envelope["clause_contracts"]
        if "reproduction_artifact" in contract["required_artifacts"]
    ]
    clause_ids = [contract["clause_id"] for contract in clause_contracts]
    clause_support = {
        contract["clause_id"]: {
            "source_text": contract["source_text"],
            "observation": "the replay and matched control cover this requested clause",
        }
        for contract in clause_contracts
    }
    host_output = {
        "artifact_type": "reproduction_artifact",
        "target": str(target),
        "reproducible": True,
        "evidence_refs": list(next_spec["evidence_refs"]),
        "observations": ["fixture behavior reproduced from the recorded input"],
        "impact_observations": ["the validated path consumes the fixture input"],
        "negative_controls": ["an empty fixture does not reproduce the observed behavior"],
        "side_effects": False,
        "cleanup_actions": ["no persistent fixture changes were created"],
        "clause_ids": clause_ids,
        "clause_support": clause_support,
    }

    def independent_validation(arguments: dict[str, object]) -> dict[str, object]:
        requirement = arguments["verification_requirement"]
        assert isinstance(requirement, dict) and requirement["required"] is True
        assertions = arguments["unverified_host_observations"]
        assert isinstance(assertions, list) and assertions
        return {
            **host_output,
            "evidence_refs": list(arguments["evidence_refs"]),
            "observations": ["independent Runtime adapter replayed the fixture path"],
        }

    runtime.broker.register_adapter(
        name="independent-validation-fixture",
        capabilities=("controlled_validation",),
        adapter=independent_validation,
        priority=1,
    )
    completed = runtime.submit_handoff_observation(
        run_id=state.run_id,
        handoff_id=receipt["handoff_id"],
        handoff_token=receipt["handoff_token"],
        attempt_id=receipt["attempt_id"],
        contract_hash=receipt["contract_hash"],
        output=host_output,
        max_actions=32,
    )

    assert completed.state.status == "completed", {
        "current": completed.state.current_action_id,
        "action_status": completed.state.action_status,
        "evidence_types": [node.artifact_type for node in runtime.evidence_graph.list(state.run_id)],
        "attempts": [
            (item.action_id, item.status, item.error)
            for item in runtime.store.task_attempts(state.run_id)
        ],
    }
    assert completed.terminal.terminal is True
    assert completed.terminal.success is True
    assert completed.terminal.reason == "goal_contract_satisfied"
    assert all(
        completed.state.action_status[action_id] == "completed"
        for action_id in (
            "map-surface",
            "build-hypotheses",
            "validate-path",
            "prove-impact",
            "review-coverage",
            "cleanup",
            "report",
        )
    )

    evidence = runtime.evidence_graph.list(state.run_id)
    assert {node.artifact_type for node in evidence} == {
        "surface_map",
        "hypothesis_queue",
        "reproduction_artifact",
        "impact_proof",
        "coverage_report",
        "cleanup_proof",
        "final_report",
    }
    report = next(node for node in evidence if node.artifact_type == "final_report")
    reproduction = next(node for node in evidence if node.artifact_type == "reproduction_artifact")
    assert reproduction.verified is True
    assert reproduction.trust == "runtime_verified"
    assert report.payload["goal_result"] == "achieved"
    assert {item["clause_id"] for item in report.payload["clause_results"]} == set(
        completed.state.goal.intent_envelope["clause_ids"]
    )
    assert all(item["status"] == "achieved" for item in report.payload["clause_results"])
    asserted = [
        node
        for node in runtime.evidence_graph.list(state.run_id, include_unverified=True)
        if node.artifact_type == "host_observation"
    ]
    assert len(asserted) == 1
    assert asserted[0].verified is False
    assert asserted[0].trust == "host_asserted"
    assert asserted[0].evidence_id not in reproduction.parent_ids
