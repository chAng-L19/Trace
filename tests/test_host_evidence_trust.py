from __future__ import annotations

import sys
from pathlib import Path
from typing import Any, Mapping

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.operation_runtime import OperationRuntime


def _host_claim(next_spec: Mapping[str, Any], target: str) -> dict[str, Any]:
    contracts = next_spec["goal_contract"]["prompt_rewrite"]["clause_contracts"]
    eligible = [
        item
        for item in contracts
        if "reproduction_artifact" in item["required_artifacts"]
    ]
    return {
        "artifact_type": "reproduction_artifact",
        "target": target,
        "reproducible": True,
        "evidence_refs": list(next_spec["evidence_refs"]),
        "results": ["claimed success; no tool was executed"],
        "impact_observations": ["claimed impact"],
        "negative_controls": ["claimed control"],
        "side_effects": False,
        "cleanup_actions": ["claimed cleanup"],
        "clause_ids": [item["clause_id"] for item in eligible],
        "clause_support": {
            item["clause_id"]: {
                "source_text": item["source_text"],
                "observation": "self-reported coverage",
            }
            for item in eligible
        },
    }


def _waiting_validation(tmp_path: Path) -> tuple[OperationRuntime, str, str, dict[str, Any]]:
    target = tmp_path / "target.txt"
    target.write_text("fixture\n", encoding="utf-8")
    runtime = OperationRuntime(root=tmp_path / "operations")
    state = runtime.start(
        session_id="host-trust-audit",
        objective=f"Validate {target} and report impact",
        targets=[str(target)],
        max_actions=64,
    )
    waiting = runtime.resume(state.run_id, max_actions=64)
    next_spec = waiting.summary()["next_action_spec"]
    assert next_spec["action_id"] == "validate-path"
    return runtime, state.run_id, str(target), next_spec


def _submit_claim(
    runtime: OperationRuntime,
    run_id: str,
    next_spec: Mapping[str, Any],
    claim: Mapping[str, Any],
):
    receipt = next_spec["handoff"]
    return runtime.submit_handoff_observation(
        run_id=run_id,
        handoff_id=receipt["handoff_id"],
        handoff_token=receipt["handoff_token"],
        attempt_id=receipt["attempt_id"],
        contract_hash=receipt["contract_hash"],
        output=dict(claim),
        max_actions=64,
    )


def test_host_receipt_self_report_stays_unverified_and_requests_independent_check(
    tmp_path: Path,
) -> None:
    runtime, run_id, target, next_spec = _waiting_validation(tmp_path)
    first_receipt = next_spec["handoff"]["handoff_id"]

    result = _submit_claim(runtime, run_id, next_spec, _host_claim(next_spec, target))
    summary = result.summary()

    assert result.state.status == "waiting_host"
    assert result.terminal.terminal is False
    assert result.terminal.success is False
    verify = summary["next_action_spec"]
    assert verify["action_id"] == "validate-path"
    assert verify["phase"] == "verify-observation"
    assert verify["verification_requirement"]["required"] is True
    assert verify["feedback_gate"]["predicate"] == "independent_tool_evidence"
    assert verify["handoff"]["handoff_id"] != first_receipt
    assert verify["host_assertion_refs"]

    assert not runtime.evidence_graph.by_type(run_id, "reproduction_artifact")
    assert not runtime.evidence_graph.by_type(run_id, "impact_proof")
    all_nodes = runtime.evidence_graph.list(run_id, include_unverified=True)
    assertions = [node for node in all_nodes if node.artifact_type == "host_observation"]
    assert len(assertions) == 1
    assert assertions[0].verified is False
    assert assertions[0].trust == "host_asserted"
    assert assertions[0].payload["results"] == ["claimed success; no tool was executed"]
    assert "host_observation_asserted" in {
        item["event_type"] for item in runtime.store.events(run_id)
    }
    with pytest.raises(ValueError, match="evidence_trust_invalid"):
        runtime.evidence_graph.add(
            run_id=run_id,
            action_id=assertions[0].action_id,
            artifact_type="reproduction_artifact",
            target=target,
            tool=assertions[0].tool,
            payload=assertions[0].payload,
            parent_ids=assertions[0].parent_ids,
            verifier="reproduction_artifact",
            confidence=1.0,
            provenance=assertions[0].provenance,
            verified=True,
            trust="runtime_verified",
        )


def test_derived_tool_cannot_promote_a_host_assertion_to_verified_evidence(
    tmp_path: Path,
) -> None:
    runtime, run_id, target, next_spec = _waiting_validation(tmp_path)
    claim = _host_claim(next_spec, target)
    replay_without_unexecuted_markers = {
        **claim,
        "results": ["copied observation"],
        "impact_observations": ["copied impact"],
        "negative_controls": ["copied control"],
        "clause_support": {
            clause_id: {
                **dict(support),
                "observation": "copied coverage",
            }
            for clause_id, support in claim["clause_support"].items()
        },
    }

    def copy_assertion(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
        assertions = arguments["unverified_host_observations"]
        assertion_id = assertions[-1]["evidence_id"]
        return {**replay_without_unexecuted_markers, "evidence_refs": [assertion_id]}

    runtime.broker.register_adapter(
        name="assertion-copy-fixture",
        capabilities=("controlled_validation",),
        adapter=copy_assertion,
        priority=1,
    )
    result = _submit_claim(runtime, run_id, next_spec, claim)

    assert result.state.status == "waiting_host"
    assert result.terminal.success is False
    assert not runtime.evidence_graph.by_type(run_id, "reproduction_artifact")
    attempts = runtime.store.task_attempts(run_id, action_id="validate-path")
    assert any(item.error == "unknown_evidence_reference" for item in attempts)


def test_independent_tool_never_receives_host_payload_and_rejects_claim_replay(
    tmp_path: Path,
) -> None:
    runtime, run_id, target, next_spec = _waiting_validation(tmp_path)
    claim = _host_claim(next_spec, target)
    observed_arguments: dict[str, Any] = {}

    def replay_claim(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
        observed_arguments.update(arguments)
        return {**claim, "evidence_refs": list(next_spec["evidence_refs"])}

    runtime.broker.register_adapter(
        name="claim-replay-fixture",
        capabilities=("controlled_validation",),
        adapter=replay_claim,
        priority=1,
    )
    result = _submit_claim(runtime, run_id, next_spec, claim)

    assertions = observed_arguments["unverified_host_observations"]
    assert assertions and set(assertions[0]).isdisjoint({"payload", "payload_json", "results"})
    assert "claimed success" not in str(observed_arguments)
    assert result.state.status == "waiting_host"
    assert not runtime.evidence_graph.by_type(run_id, "reproduction_artifact")
    attempts = runtime.store.task_attempts(run_id, action_id="validate-path")
    assert any(item.error == "unexecuted_claim_rejected" for item in attempts)
