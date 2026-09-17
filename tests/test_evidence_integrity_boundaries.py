from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

import pytest

from redteam_agent.runtime import (
    DurableStore, EvidenceGraph, EvidenceProvenance, GoalCompiler,
    OperationState, TaskAttempt, ToolCallResult, WorkflowRegistry,
)
from redteam_agent.application.asset_graph import project_asset_attack_graph
from redteam_agent.core import Asset, Finding


@pytest.fixture
def graph_factory(tmp_path: Path):
    goal = GoalCompiler().compile("Inspect https://target.invalid")
    workflow = WorkflowRegistry().match(goal)
    state = OperationState.create(session_id="integrity-boundaries", goal=goal, workflow=workflow)
    store = DurableStore(tmp_path / "operations")
    store.create_operation(state, event={"source": "pytest"})
    graph = EvidenceGraph(store, tmp_path / "artifacts")
    action = workflow.actions[0]
    sequence = 0

    def add(payload, parents=()):
        nonlocal sequence
        sequence += 1
        tool = "fixture:tool"
        token = store.acquire_lease(state.run_id, action.action_id, f"test-{sequence}", ttl_seconds=30)
        attempt = TaskAttempt.create(
            run_id=state.run_id, branch_id=state.branch_id, plan_revision=1,
            action_id=action.action_id, tool=tool, tool_version="1",
            input_hash=f"input-{sequence}", idempotency_key=f"attempt-{sequence}",
            fencing_token=token.fencing_token,
        )
        store.create_task_attempt(attempt)
        result = ToolCallResult(status="success", output=payload, tool=tool,
                                input_hash=attempt.input_hash, output_hash=graph.content_hash(payload))
        store.update_task_attempt(replace(attempt, status="completed", result=result.to_dict()),
                                  expected_status="prepared", lease_token=token)
        store.release_lease(token)
        provenance = EvidenceProvenance(
            run_id=state.run_id, branch_id=state.branch_id, plan_revision=1,
            action_id=action.action_id, attempt_id=attempt.attempt_id, tool=tool,
            tool_version="1", input_hash=attempt.input_hash, output_hash=result.output_hash,
            target=goal.targets[0], parent_ids=tuple(parents),
        )
        return graph.add(run_id=state.run_id, action_id=action.action_id,
                         artifact_type=action.expected_artifact, target=goal.targets[0],
                         tool=tool, payload=payload, parent_ids=parents, verifier=action.verifier,
                         confidence=0.9, provenance=provenance)

    return graph, state.run_id, add


def test_repeated_tool_payload_preserves_distinct_attempt_artifacts(graph_factory):
    graph, run_id, add = graph_factory
    first = add({"result": "same"})
    second = add({"result": "same"})
    assert first.evidence_id != second.evidence_id
    assert {node.evidence_id for node in graph.list(run_id)} == {first.evidence_id, second.evidence_id}


@pytest.mark.parametrize("old_path", ["_previous_artifact_path", "_legacy_artifact_path"])
def test_legacy_artifact_is_validated_and_migrated_without_losing_identity(graph_factory, old_path):
    graph, run_id, add = graph_factory
    node = add({"result": "legacy"})
    current = graph._artifact_path(node)
    current.replace(getattr(graph, old_path)(node))
    assert graph.list(run_id) == (node,)
    assert current.is_file()


def test_corrupt_ancestor_excludes_every_descendant(graph_factory):
    graph, run_id, add = graph_factory
    root = add({"result": "root"})
    child = add({"result": "child"}, (root.evidence_id,))
    add({"result": "grandchild"}, (child.evidence_id,))
    graph._artifact_path(root).write_text("{}", encoding="utf-8")
    assert graph.list(run_id) == ()


def test_non_utf8_evidence_artifact_is_excluded_without_breaking_other_evidence(graph_factory):
    graph, run_id, add = graph_factory
    corrupt = add({"result": "corrupt"})
    valid = add({"result": "valid"})
    graph._artifact_path(corrupt).write_bytes(b"\xff\xfe")
    assert graph.list(run_id) == (valid,)


def test_attack_projection_requires_finding_groups_and_target_binding(graph_factory):
    graph, run_id, add = graph_factory
    root = add({"result": "source"})
    complete = Finding(
        "valid", run_id, "Verified finding", "medium", target=root.target,
        reproduction_evidence_ids=(root.evidence_id,), impact_evidence_ids=(root.evidence_id,),
        negative_control_evidence_ids=(root.evidence_id,), cleanup_evidence_ids=(root.evidence_id,),
    )
    incomplete = replace(complete, finding_id="missing-impact", impact_evidence_ids=())
    wrong_target = replace(complete, finding_id="wrong-target", target="https://unrelated.invalid")
    wrong_asset = Asset("wrong-asset", run_id, "host", "unrelated", target="https://unrelated.invalid",
                        evidence_ids=(root.evidence_id,))
    add({"findings": [value.to_dict() for value in (complete, incomplete, wrong_target)],
         "assets": [wrong_asset.to_dict()]}, (root.evidence_id,))
    service = SimpleNamespace(runtime=SimpleNamespace(evidence_graph=graph), status=lambda _: None)
    result = project_asset_attack_graph(service, run_id)
    assert [item["finding_id"] for item in result["findings"]] == ["valid"]
    assert result["assets"] == []
