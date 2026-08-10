from __future__ import annotations

import sys
import time
from dataclasses import replace
from pathlib import Path

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import (
    DurableStore,
    EvidenceGraph,
    EvidenceProvenance,
    GoalCompiler,
    OperationState,
    TaskAttempt,
    ToolCallResult,
    WorkflowRegistry,
)


def test_evidence_graph_preserves_duplicate_identity_and_tool_provenance(tmp_path: Path) -> None:
    goal = GoalCompiler().compile("Validate SQL injection on https://target.invalid")
    workflow = WorkflowRegistry().match(goal)
    state = OperationState.create(session_id="evidence-test", goal=goal, workflow=workflow)
    store = DurableStore(tmp_path / "operations")
    store.create_operation(state, event={"source": "pytest"})
    graph = EvidenceGraph(store, tmp_path / "artifacts")
    action = workflow.actions[0]

    def provenance_for(tool: str, payload: dict[str, object], sequence: int, parent_ids: tuple[str, ...] = ()) -> EvidenceProvenance:
        token = store.acquire_lease(state.run_id, action.action_id, f"pytest:{sequence}", ttl_seconds=30)
        assert token is not None
        input_hash = f"input-{sequence}"
        output_hash = EvidenceGraph.content_hash(payload)
        attempt = TaskAttempt.create(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action.action_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            idempotency_key=f"evidence-{sequence}",
            fencing_token=token.fencing_token,
        )
        store.create_task_attempt(attempt)
        result = ToolCallResult(
            status="success",
            output=payload,
            tool=tool,
            input_hash=input_hash,
            output_hash=output_hash,
            tool_version="1",
        )
        store.update_task_attempt(
            replace(attempt, status="completed", result=result.to_dict(), finished_at=result.finished_at),
            expected_status="prepared",
            lease_token=token,
        )
        store.release_lease(token)
        return EvidenceProvenance(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action.action_id,
            attempt_id=attempt.attempt_id,
            tool=tool,
            tool_version="1",
            input_hash=input_hash,
            output_hash=output_hash,
            verifier=action.verifier,
            parent_ids=parent_ids,
            target=goal.targets[0],
        )

    first_payload = {"status": "verified"}
    first_provenance = provenance_for("pytest:adapter", first_payload, 1)

    first = graph.add(
        run_id=state.run_id,
        action_id=action.action_id,
        artifact_type=action.expected_artifact,
        target=goal.targets[0],
        tool="pytest:adapter",
        payload=first_payload,
        parent_ids=(),
        verifier=action.verifier,
        confidence=0.9,
        provenance=first_provenance,
    )
    time.sleep(1.1)
    duplicate = graph.add(
        run_id=state.run_id,
        action_id=action.action_id,
        artifact_type=action.expected_artifact,
        target=goal.targets[0],
        tool="pytest:adapter",
        payload=first_payload,
        parent_ids=(),
        verifier=action.verifier,
        confidence=0.9,
        provenance=first_provenance,
    )

    assert duplicate == first
    assert graph.list(state.run_id) == (first,)

    alternate_payload = {"status": "verified"}
    alternate_provenance = provenance_for("pytest:alternate", alternate_payload, 2)
    alternate_tool = graph.add(
        run_id=state.run_id,
        action_id=action.action_id,
        artifact_type=action.expected_artifact,
        target=goal.targets[0],
        tool="pytest:alternate",
        payload=alternate_payload,
        parent_ids=(),
        verifier=action.verifier,
        confidence=0.9,
        provenance=alternate_provenance,
    )

    assert alternate_tool.tool == "pytest:alternate"
    assert alternate_tool.evidence_id != first.evidence_id
    assert {node.evidence_id for node in graph.list(state.run_id)} == {
        first.evidence_id,
        alternate_tool.evidence_id,
    }

    missing_payload = {"status": "new"}
    missing_provenance = provenance_for(
        "pytest:adapter",
        missing_payload,
        3,
        ("missing-parent",),
    )
    with pytest.raises(ValueError, match="evidence_parent_missing"):
        graph.add(
            run_id=state.run_id,
            action_id=action.action_id,
            artifact_type=action.expected_artifact,
            target=goal.targets[0],
            tool="pytest:adapter",
            payload=missing_payload,
            parent_ids=("missing-parent",),
            verifier=action.verifier,
            confidence=1.0,
            provenance=missing_provenance,
        )
