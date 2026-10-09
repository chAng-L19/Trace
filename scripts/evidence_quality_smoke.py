"""Offline evidence-role, negative-source and recon-cache regression checks."""
from __future__ import annotations

import json
import sys
import tempfile
from copy import deepcopy
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.asset_graph import project_asset_attack_graph
from redteam_agent.core import ExplorationRecord, Finding, ToolDefinition, ToolResult
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.evidence_gate import EvidenceGate
from redteam_agent.runtime.builtins import _artifact_clause_ids, _artifact_clause_support
from redteam_agent.runtime.exploration import ExplorationValidationError, TacticalAttemptRecord
from redteam_agent.runtime.model_contracts import ActionSpec
from redteam_agent.runtime.model_state import EvidenceProvenance, TaskAttempt, ToolCallResult
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.artifact_store import ArtifactIntegrityError
from redteam_agent.application.model_loop import ModelIntegrityError
from redteam_agent.runtime.verifier import SemanticVerifier


class EnumerationTool:
    """A deterministic probe that really evaluates a bounded input set locally."""
    def __init__(self):
        self.calls = 0

    def discover(self):
        return (ToolDefinition(qualified_name="fixture:enumerate", name="enumerate", server="fixture",
            input_schema={"type": "object", "properties": {"target": {"type": "string"}}},
            capabilities=("target_intake",)),)

    def invoke(self, call):
        self.calls += 1
        entries = [f"/missing/{index}" for index in range(32)]
        matches = sum(entry in {"/health"} for entry in entries)
        return ToolResult(call.call_id, "success", call.tool_name,
            output={"target": call.arguments["target"], "requests": len(entries), "matches": matches})

    def reconcile(self, call):
        return None

    def cancel(self, call_id):
        return False


def execute_enumeration(service, run_id, target):
    service.configure_model(FakeModelProvider([
        {"status": "completed", "text": "", "usage": {"total_tokens": 1}, "tool_calls": [
            {"call_id": "enum", "tool_name": "fixture:enumerate", "arguments": {"target": target}}]},
        {"status": "completed", "text": "Enumeration complete", "usage": {"total_tokens": 1},
         "structured_output": {"decision": "waiting_input"}},
    ]))
    service.run(run_id)
    return service.journal.tactical_attempts(run_id)[-1].payload["raw_artifact_ref"]


def persist(runtime, state, artifact_type, payload, *, parents=(), branch=None, identity=None):
    identity = identity or f"{artifact_type}-{len(runtime.store.task_attempts(state.run_id))}"
    attempt = TaskAttempt.create(run_id=state.run_id, branch_id=branch or state.branch_id,
        plan_revision=state.plan_revision, action_id=identity, tool="fixture:probe", tool_version="1",
        input_hash="fixture-input", idempotency_key=identity)
    output = {"target": state.goal.targets[0], **payload}
    result = ToolCallResult("success", output, tool="fixture:probe",
        input_hash="fixture-input", output_hash=EvidenceGate.content_hash(output))
    attempt = replace(attempt, status="completed", result=result.to_dict())
    runtime.store.create_task_attempt(attempt)
    provenance = EvidenceProvenance(run_id=state.run_id, branch_id=attempt.branch_id,
        plan_revision=state.plan_revision, action_id=identity, attempt_id=attempt.attempt_id,
        tool=attempt.tool, tool_version=attempt.tool_version, input_hash=attempt.input_hash,
        output_hash=result.output_hash, target=output["target"], parent_ids=tuple(parents))
    return runtime.evidence_graph.add(run_id=state.run_id, action_id=identity, artifact_type=artifact_type,
        target=output["target"], tool=attempt.tool, payload=output, parent_ids=parents,
        verifier=artifact_type, confidence=0.8, provenance=provenance, trust="tool_verified")


def expect_negative_rejected(service, run_id, record, reason):
    try:
        service.record_exploration(run_id, record)
    except ExplorationValidationError as error:
        assert reason in str(error), error
    else:
        raise AssertionError("invalid verified_negative was accepted")


def main():
    checks = []
    with tempfile.TemporaryDirectory(prefix="trace-evidence-quality-") as directory:
        runtime = OperationRuntime(root=Path(directory), register_builtins=False)
        first = runtime.start(session_id="quality", objective="Inspect local fixture", targets=("fixture-a", "fixture-b"), model_led=True)
        state = runtime.store.load_operation(first.run_id)
        control = {"source": "tool_output", "observation_path": "control.matches",
                   "expected": 0, "actual": 0, "passed": True}
        surface = persist(runtime, state, "surface_map", {"assets": ["fixture-a"], "control": {"matches": 0}})
        reproduction = persist(runtime, state, "reproduction_artifact", {
            "reproducible": True, "tests": ["bounded enumeration"], "side_effects": False,
            "control": {"matches": 0}, "negative_controls": [control], "evidence_refs": [surface.evidence_id],
        }, parents=(surface.evidence_id,))
        impact = persist(runtime, state, "impact_proof", {"verified": True, "impact": {"observed": "denied"}}, parents=(reproduction.evidence_id,))
        cleanup = persist(runtime, state, "cleanup_proof", {"verified": True, "checks": ["read-only"], "outstanding_changes": []}, parents=(impact.evidence_id,))
        nodes = {item.evidence_id: item for item in runtime.evidence_graph.list(first.run_id)}
        finding = Finding("finding", first.run_id, "Observed path", "medium", status="verified", target="fixture-a",
            reproduction_evidence_ids=(reproduction.evidence_id,), impact_evidence_ids=(impact.evidence_id,),
            negative_control_evidence_ids=(reproduction.evidence_id,), cleanup_evidence_ids=(cleanup.evidence_id,))
        gate_args = {"run_id": first.run_id, "branch_id": state.branch_id, "target": "fixture-a", "max_plan_revision": state.plan_revision}
        assert EvidenceGate.validate_finding(finding, nodes, **gate_args).passed
        forged = replace(finding, reproduction_evidence_ids=(surface.evidence_id,), impact_evidence_ids=(surface.evidence_id,),
            negative_control_evidence_ids=(surface.evidence_id,), cleanup_evidence_ids=(surface.evidence_id,))
        assert EvidenceGate.validate_finding(forged, nodes, **gate_args).reason == "finding_evidence_role_mismatch"
        for changes in ({"run_id": "other"}, {"branch_id": "other"}, {"target": "fixture-b"}):
            assert not EvidenceGate.validate_finding(finding, nodes, **{**gate_args, **changes}).passed
        checks.append("finding_roles_and_scope")
        source = persist(runtime, state, "surface_map", {"assets": ["fixture-a"], "findings": [forged.to_dict()]})
        with AgentService(runtime=runtime, load_external_configuration=False) as service:
            assert not project_asset_attack_graph(service, first.run_id)["findings"]
            persist(runtime, state, "surface_map", {"assets": ["fixture-a"], "findings": [finding.to_dict()]})
            assert len(project_asset_attack_graph(service, first.run_id)["findings"]) == 1
            checks.append("asset_graph_filters_forged_finding_and_keeps_valid")
            baseline = {"record_id": "negative", "hypothesis_id": "hypothesis", "kind": "verified_negative", "status": "closed",
                "statement": "Exact tested set was negative", "target": "fixture-a", "tested_domain": {"entries": 32},
                "observations": {"matches": 0}, "coverage": {"entries": 32}, "confidence": 0.8,
                "reopen_triggers": ["capability:schema_discovery"], "metadata": {"observation_paths": {"matches": "control.matches"}}}
            arbitrary = runtime.artifacts.put_json({"requests": 32, "matches": 0}, run_id=first.run_id)
            expect_negative_rejected(service, first.run_id, {**baseline, "artifact_refs": [arbitrary.artifact_id]}, "execution_source_required")
            expect_negative_rejected(service, first.run_id, {**baseline, "target": "fixture-b", "evidence_refs": [surface.evidence_id]}, "scope_mismatch")
            other_branch = persist(runtime, state, "surface_map", {"assets": ["fixture-a"]}, branch="other")
            expect_negative_rejected(service, first.run_id, {**baseline, "evidence_refs": [other_branch.evidence_id]}, "scope_mismatch")
            service.record_exploration(first.run_id, {**baseline, "evidence_refs": [surface.evidence_id]})
            expect_negative_rejected(service, first.run_id, {**baseline, "record_id": "false-measurement",
                "observations": {"matches": 9}, "evidence_refs": [surface.evidence_id]}, "observation_unproven")
            assert service.exploration.current(first.run_id)[0].status == "closed"
            legacy = ExplorationRecord.from_dict({**baseline, "run_id": first.run_id,
                "record_id": "legacy-negative", "hypothesis_id": "legacy-hypothesis", "artifact_refs": [arbitrary.artifact_id]})
            runtime.store.save_exploration_record(legacy)
            projected = {item.hypothesis_id: item for item in service.exploration.current(first.run_id)}
            assert projected["legacy-hypothesis"].status == "suspended"
            assert next(item for item in runtime.store.exploration_records(first.run_id) if item.record_id == legacy.record_id).to_dict() == legacy.to_dict()
            checks.append("verified_negative_evidence_scope_and_arbitrary_blob")
            checks.append("legacy_unproven_closed_verdict_is_suspended_without_rewriting")
            first_digest = service.recon_digest(first.run_id)
            assert service.recon_digest(first.run_id).digest_id == first_digest.digest_id
            runtime.pause_run(first.run_id)
            paused = service.recon_digest(first.run_id)
            assert paused.digest_id != first_digest.digest_id and paused.digest["run_status"] == "paused_budget"
            persist(runtime, state, "surface_map", {"assets": ["new-observation"]})
            changed = service.recon_digest(first.run_id)
            assert changed.digest_id != paused.digest_id and len(changed.digest["confirmed_observations"]) > len(paused.digest["confirmed_observations"])
            runtime.store.save_tactical_attempt(TacticalAttemptRecord("new-attempt", first.run_id, "request", "call", "discovery",
                "fingerprint", "success", {"tool": "fixture:probe"}, "2026-10-08T00:00:00Z"))
            attempted = service.recon_digest(first.run_id)
            assert attempted.digest_id != changed.digest_id and attempted.digest["attempted_actions"][-1]["attempt_id"] == "new-attempt"
            checks.append("recon_cache_tracks_state_evidence_and_attempts")
            leaf = service.journal.leaf_id(first.run_id)
            service.fork_session(first.run_id, leaf, "quality-branch")
            service.checkout_session(first.run_id, "quality-branch")
            branch_digest = service.recon_digest(first.run_id)
            assert branch_digest.digest_id != attempted.digest_id
            checks.append("recon_cache_tracks_session_branch")

        verifier = SemanticVerifier()
        action = ActionSpec("repro", "Reproduce", (), "reproduction_artifact", "reproduction_artifact")
        output = {"target": "fixture-a", "artifact_type": "reproduction_artifact", "evidence_refs": [surface.evidence_id],
                  "reproducible": True, "tests": ["bounded enumeration"], "side_effects": False,
                  "control": {"matches": 0}, "negative_controls": [control]}
        def verify(body, **result_values):
            return verifier.verify(action=action, result=ToolCallResult("success", body,
                tool="fixture:probe", input_hash="input", output_hash="output", **result_values),
                goal=state.goal, available_evidence=tuple(nodes.values()), run_id=state.run_id, branch_id=state.branch_id)
        assert verify(output).passed
        bad = deepcopy(output)
        bad["negative_controls"] = ["todo"]
        assert verify(bad).reason == "negative_control_execution_unproven"
        for altered in ({"actual": 1}, {"observation_path": "missing.value"}, {"passed": False}, {"actual": False, "expected": False}):
            bad = deepcopy(output)
            bad["negative_controls"] = [{**control, **altered}]
            assert not verify(bad).passed
        linked = {**control, "evidence_refs": [surface.evidence_id]}
        linked.pop("source")
        assert verify({**output, "negative_controls": [linked]}).passed
        bad = deepcopy(output)
        bad["negative_controls"] = [{**linked, "evidence_refs": [impact.evidence_id]}]
        assert not verify(bad).passed
        checks.append("negative_controls_bind_measurements_and_source")
        coverage_action = replace(action, expected_artifact="coverage_report", verifier="coverage_report")
        coverage = {"target": "fixture-a", "artifact_type": "coverage_report", "evidence_refs": [reproduction.evidence_id],
                    "checked": ["bounded enumeration"], "negative_controls": [control]}
        assert verifier.verify(action=coverage_action, result=ToolCallResult("success", coverage, tool="builtin:coverage", input_hash="i", output_hash="o"),
            goal=state.goal, available_evidence=tuple(nodes.values()), run_id=state.run_id, branch_id=state.branch_id).passed
        checks.append("coverage_inherits_executed_reproduction_control")

    with tempfile.TemporaryDirectory(prefix="trace-negative-tool-source-") as directory:
        tool = EnumerationTool()
        provider = FakeModelProvider([])
        runtime = OperationRuntime(root=Path(directory), register_builtins=False)
        created = runtime.start(session_id="source", objective="Inspect fixture", targets=("fixture-a", "fixture-b"), model_led=True)
        with AgentService(runtime=runtime, tool_port=tool, model_port=provider,
                          load_external_configuration=False) as service:
            run_id = created.run_id
            artifact_id = execute_enumeration(service, run_id, "fixture-a")
            assert tool.calls == 1
            record = {"record_id": "negative-artifact", "hypothesis_id": "hidden-route", "kind": "verified_negative", "status": "closed",
                "statement": "Exact tested entries had no match", "target": "fixture-a", "artifact_refs": [artifact_id],
                "tested_domain": {"entries": 32}, "observations": {"matches": 0}, "coverage": {"entries": 32},
                "confidence": 0.8, "reopen_triggers": ["capability:schema_discovery"]}
            service.record_exploration(run_id, record)
            expect_negative_rejected(service, run_id, {**record, "record_id": "false-artifact-measurement",
                "observations": {"matches": 9}}, "observation_unproven")
            assert service.exploration.current(run_id)[-1].status == "closed"
            assert not service.status(run_id).terminal.success
            good_digest = service.recon_digest(run_id)
            with patch.object(runtime.artifacts, "read_json", side_effect=ArtifactIntegrityError("corrupt fixture")):
                assert service.exploration.current(run_id)[-1].status == "suspended"
                invalid_digest = service.recon_digest(run_id)
                assert invalid_digest.digest_id != good_digest.digest_id
                assert invalid_digest.digest["unverified_hypotheses"][-1]["status"] == "suspended"
            assert service.exploration.current(run_id)[-1].status == "closed"
            checks.append("source_loss_suspends_navigation_and_invalidates_digest")
            expect_negative_rejected(service, run_id, {**record, "record_id": "wrong-target", "target": "fixture-b"}, "execution_source_required")
            attempts = service.journal.tactical_attempts(run_id)
            observations = service.journal.model_observations(run_id)
            for source, method, rows in (
                (service.journal, "tactical_attempts", tuple(replace(item, status="failed") for item in attempts)),
                (service.journal, "model_observations", tuple(replace(item, output_hash="wrong") for item in observations)),
                (service.journal, "model_observations", tuple(replace(item, status="failed") for item in observations)),
            ):
                with patch.object(source, method, return_value=rows):
                    expect_negative_rejected(service, run_id, {**record, "record_id": "bad-observation"}, "execution_source_required")
            checks.append("artifact_source_requires_successful_hash_bound_observation")
            checks.append("actual_model_tool_artifact_closes_scoped_hypothesis_only")
            current_state = runtime.store.load_operation(run_id)
            assert artifact_id in service.exploration._executed_artifacts(replace(current_state, plan_revision=current_state.plan_revision + 1), "fixture-a")
            checks.append("same_branch_historical_execution_is_preserved")
            active_state = runtime.store.load_operation(run_id)
            lease = runtime.store.acquire_lease(run_id, "__operation__", "source-smoke", ttl_seconds=30)
            try:
                active_state.branch_id = "other-plan"
                runtime.store.save_operation(active_state, expected_version=active_state.state_version, lease_token=lease)
                expect_negative_rejected(service, run_id, {**record, "record_id": "wrong-branch"}, "execution_source_required")
                active_state.branch_id = "main"
                runtime.store.save_operation(active_state, expected_version=active_state.state_version, lease_token=lease)
            finally:
                runtime.store.release_lease(lease)
            checks.append("artifact_source_cannot_cross_target_or_plan_branch")
            leaf = service.journal.leaf_id(run_id)
            service.fork_session(run_id, leaf, "source-branch")
            service.checkout_session(run_id, "source-branch")
            # An ancestor execution remains visible and is a valid source on a descendant session branch.
            assert artifact_id in service.exploration._executed_artifacts(service.runtime.store.load_operation(run_id), "fixture-a")
            checks.append("descendant_session_preserves_ancestor_execution")

    with tempfile.TemporaryDirectory(prefix="trace-no-model-evidence-") as directory:
        path = Path(directory) / "fixture.txt"
        path.write_bytes(b"needle")
        runtime = OperationRuntime(root=Path(directory) / "runtime")
        def validate(arguments):
            observed = Path(arguments["target"]).read_bytes()
            return {"artifact_type": "reproduction_artifact", "target": arguments["target"],
                "evidence_refs": [item["evidence_id"] for item in arguments["evidence"]],
                "reproducible": True, "tests": [{"matches": observed.count(b"needle")}],
                "impact_observations": {"matches": observed.count(b"needle")}, "side_effects": False,
                "control": {"matches": observed.count(b"absent-control")}, "negative_controls": [control],
                "clause_ids": _artifact_clause_ids(arguments, "reproduction_artifact", arguments["evidence"]),
                "clause_support": _artifact_clause_support(arguments, "reproduction_artifact")}
        runtime.broker.register_adapter(name="fixture-validation", capabilities=("controlled_validation",),
            adapter=validate, priority=1)
        result = runtime.start(session_id="no-model", objective="Validate the local fixture", targets=(str(path),))
        result = runtime.resume(result.run_id)
        assert result.terminal.success, result.summary()
        assert {node.artifact_type for node in result.evidence}.issuperset({"reproduction_artifact", "impact_proof", "coverage_report", "cleanup_proof", "final_report"})
        checks.append("no_model_builtin_lifecycle_keeps_measured_controls")

    for arguments in ('{"broken":', [1, 2], "null"):
        with tempfile.TemporaryDirectory(prefix="trace-invalid-arguments-") as directory:
            provider = FakeModelProvider([
                {"status": "completed", "text": "", "usage": {"total_tokens": 9}, "tool_calls": [
                    {"call_id": "invalid", "tool_name": "fixture:enumerate", "arguments": arguments}]},
                {"status": "completed", "text": "Stopped for review", "usage": {"total_tokens": 1},
                 "structured_output": {"decision": "waiting_input"}},
            ])
            with AgentService(root=Path(directory), model_port=provider, load_external_configuration=False) as service:
                run_id = service.start({"session_id": "invalid-args", "objective": "Execute the local fixture",
                    "targets": [directory], "token_limit": 100}).single.run.run_id
                with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
                    try:
                        service.run(run_id)
                    except ModelIntegrityError as error:
                        assert str(error) == "model_tool_arguments_invalid"
                    else:
                        if arguments == "null":
                            # Existing null argument normalization remains an empty object.
                            continue
                        raise AssertionError("malformed tool arguments were accepted")
                    execute.assert_not_called()
                state = service.runtime.store.load_operation(run_id)
                assert state.budget.tokens_used == 9
                stored = service.journal.model_responses(run_id)[0]
                assert stored.status == "integrity_error" and stored.usage["total_tokens"] == 9
                assert stored.response["metadata"]["protocol_error"] == "model_tool_arguments_invalid"
                service.configure_model(provider)
                assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
                service.resume(run_id)
                assert len(provider.requests) == 2
    checks.append("malformed_arguments_retain_usage_and_never_execute_or_replay")

    print(json.dumps({"checks": checks, "passed": len(checks)}, indent=2))


if __name__ == "__main__":
    main()
