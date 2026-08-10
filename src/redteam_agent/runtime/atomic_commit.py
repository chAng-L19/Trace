from __future__ import annotations

import hashlib
import json
import sqlite3
from typing import Any, Mapping, Sequence, TYPE_CHECKING

from .models import EvidenceNode, FactRecord, LeaseToken, OperationState, ReviewRecord, TaskAttempt, utc_now
from .evidence_trust import is_trusted_evidence, valid_evidence_trust
from .plan import PlanRevision

if TYPE_CHECKING:
    from .durable_store import DurableStore


def commit_action_outcome(
    store: "DurableStore",
    *,
    state: OperationState,
    expected_state_version: int,
    attempt: TaskAttempt,
    expected_attempt_status: str,
    lease_token: LeaseToken,
    evidence: Sequence[EvidenceNode] = (),
    facts: Sequence[FactRecord] = (),
    reviews: Sequence[ReviewRecord] = (),
    plans: Sequence[PlanRevision] = (),
    event_type: str = "action_outcome_committed",
    event: Mapping[str, Any] | None = None,
) -> int:
    from .durable_store import LeaseLostError, StateVersionConflict, StoreConflictError, _dump, _load

    if (state.run_id, state.branch_id) != (attempt.run_id, attempt.branch_id):
        raise ValueError("attempt_state_identity_mismatch")
    if state.plan_revision not in {attempt.plan_revision, attempt.plan_revision + 1}:
        raise ValueError("attempt_state_plan_revision_mismatch")
    if (attempt.run_id, attempt.action_id, attempt.fencing_token) != (
        lease_token.run_id,
        lease_token.action_id,
        lease_token.fencing_token,
    ):
        raise ValueError("attempt_lease_identity_mismatch")
    if evidence and attempt.status not in {"completed", "succeeded"}:
        raise ValueError("evidence_requires_completed_attempt")
    for node in evidence:
        provenance = node.provenance
        digest = hashlib.sha256(
            json.dumps(
                node.payload,
                ensure_ascii=False,
                sort_keys=True,
                default=str,
            ).encode("utf-8")
        ).hexdigest()
        if (
            node.run_id != attempt.run_id
            or node.action_id != attempt.action_id
            or not valid_evidence_trust(node)
            or node.content_hash != digest
            or node.target not in state.goal.targets
            or len(set(node.parent_ids)) != len(node.parent_ids)
            or provenance is None
            or (
                provenance.run_id,
                provenance.branch_id,
                provenance.plan_revision,
                provenance.action_id,
                provenance.attempt_id,
            )
            != (
                attempt.run_id,
                attempt.branch_id,
                attempt.plan_revision,
                attempt.action_id,
                attempt.attempt_id,
            )
            or provenance.tool != attempt.tool
            or provenance.tool_version != attempt.tool_version
            or provenance.input_hash != attempt.input_hash
            or provenance.target != node.target
            or tuple(provenance.parent_ids) != tuple(node.parent_ids)
        ):
            raise ValueError("evidence_attempt_identity_mismatch")
    if any(
        item.run_id != attempt.run_id
        or item.branch_id != attempt.branch_id
        or item.plan_revision != attempt.plan_revision
        for item in (*facts, *reviews)
    ):
        raise ValueError("derived_record_identity_mismatch")
    if any(
        item.run_id != state.run_id
        or item.branch_id != state.branch_id
        or item.plan_id != state.plan_id
        or item.revision != state.plan_revision
        for item in plans
    ):
        raise ValueError("plan_state_identity_mismatch")

    old_version, old_updated = state.state_version, state.updated_at
    try:
        with store.transaction(immediate=True) as connection:
            if not store._assert_lease(connection, lease_token):
                raise LeaseLostError(
                    f"lease_lost:{lease_token.run_id}:{lease_token.action_id}:{lease_token.fencing_token}"
                )
            operation = connection.execute(
                "SELECT version, status FROM operations WHERE run_id=?", (state.run_id,)
            ).fetchone()
            current_version = int(operation["version"]) if operation else -1
            current_status = str(operation["status"]) if operation else ""
            if current_status in {"cancelling", "cancelled"} and state.status not in {"cancelling", "cancelled"}:
                raise StoreConflictError(f"operation_cancel_requested:{state.run_id}:{current_status}")
            if current_version != expected_state_version:
                raise StateVersionConflict(
                    f"state_version_conflict:{state.run_id}:{expected_state_version}:{current_version}"
                )

            pending_nodes = {node.evidence_id: node for node in evidence}
            for node in evidence:
                for parent_id in node.parent_ids:
                    parent = pending_nodes.get(parent_id)
                    if parent is None:
                        parent_row = connection.execute(
                            "SELECT node_json FROM evidence_nodes WHERE evidence_id=? AND run_id=?",
                            (parent_id, attempt.run_id),
                        ).fetchone()
                        parent = (
                            EvidenceNode.from_dict(_load(parent_row["node_json"], {}))
                            if parent_row is not None
                            else None
                        )
                    if (
                        parent is None
                        or not is_trusted_evidence(parent)
                        or parent.run_id != attempt.run_id
                        or parent.target != node.target
                        or parent.provenance is None
                        or parent.provenance.branch_id != attempt.branch_id
                        or parent.provenance.plan_revision > attempt.plan_revision
                    ):
                        raise ValueError("evidence_parent_scope_mismatch")

            serialized_attempt = _dump(attempt.to_dict())
            cursor = connection.execute(
                "UPDATE task_attempts SET status=?, attempt_json=?, finished_at=? "
                "WHERE attempt_id=? AND run_id=? AND action_id=? AND status=? AND fencing_token=?",
                (
                    attempt.status,
                    serialized_attempt,
                    attempt.finished_at,
                    attempt.attempt_id,
                    attempt.run_id,
                    attempt.action_id,
                    expected_attempt_status,
                    attempt.fencing_token,
                ),
            )
            if cursor.rowcount != 1:
                raise StoreConflictError(
                    f"attempt_transition_conflict:{attempt.attempt_id}:{expected_attempt_status}"
                )

            for node in evidence:
                serialized = _dump(node.to_dict())
                store._immutable_insert(
                    connection,
                    table="evidence_nodes",
                    key_column="evidence_id",
                    key=node.evidence_id,
                    json_column="node_json",
                    serialized=serialized,
                    sql="INSERT INTO evidence_nodes VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                    values=(
                        node.evidence_id,
                        node.run_id,
                        node.action_id,
                        node.artifact_type,
                        node.tool,
                        node.content_hash,
                        serialized,
                        node.created_at,
                    ),
                )
            for fact in facts:
                serialized = _dump(fact.to_dict())
                store._immutable_insert(
                    connection,
                    table="facts",
                    key_column="fact_id",
                    key=fact.fact_id,
                    json_column="fact_json",
                    serialized=serialized,
                    sql="INSERT INTO facts VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                    values=(
                        fact.fact_id,
                        fact.run_id,
                        fact.branch_id,
                        fact.key,
                        fact.version,
                        1 if fact.valid else 0,
                        serialized,
                        fact.created_at,
                    ),
                )
            for review in reviews:
                serialized = _dump(review.to_dict())
                store._immutable_insert(
                    connection,
                    table="reviews",
                    key_column="review_id",
                    key=review.review_id,
                    json_column="review_json",
                    serialized=serialized,
                    sql="INSERT INTO reviews VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    values=(
                        review.review_id,
                        review.run_id,
                        review.branch_id,
                        review.plan_revision,
                        review.scope,
                        review.subject_id,
                        review.decision,
                        serialized,
                        review.created_at,
                    ),
                )
            for plan in plans:
                if plan.revision > 1:
                    parent = connection.execute(
                        "SELECT 1 FROM plan_revisions WHERE run_id=? AND plan_id=? AND branch_id=? AND revision=?",
                        (plan.run_id, plan.plan_id, plan.branch_id, plan.parent_revision),
                    ).fetchone()
                    if parent is None:
                        raise StoreConflictError(
                            f"plan_parent_missing:{plan.plan_id}:{plan.branch_id}:{plan.parent_revision}"
                        )
                serialized = _dump(plan.to_dict())
                try:
                    connection.execute(
                        "INSERT INTO plan_revisions VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                        plan.run_id,
                        plan.plan_id,
                        plan.branch_id,
                        plan.revision,
                        plan.parent_revision,
                        plan.plan_hash,
                        serialized,
                        plan.created_at,
                        ),
                    )
                except sqlite3.IntegrityError as exc:
                    existing = connection.execute(
                        "SELECT plan_json FROM plan_revisions "
                        "WHERE run_id=? AND plan_id=? AND branch_id=? AND revision=?",
                        (plan.run_id, plan.plan_id, plan.branch_id, plan.revision),
                    ).fetchone()
                    if existing is None or str(existing["plan_json"]) != serialized:
                        raise StoreConflictError(
                            f"immutable_plan_conflict:{plan.plan_id}:{plan.branch_id}:{plan.revision}"
                        ) from exc

            next_version = current_version + 1
            state.state_version = next_version
            state.updated_at = utc_now()
            snapshot = store._snapshot_payload(state, next_version)
            cursor = connection.execute(
                "UPDATE operations SET session_id=?, goal_id=?, workflow_id=?, status=?, state_json=?, "
                "version=?, updated_at=? WHERE run_id=? AND version=?",
                (
                    state.session_id,
                    state.goal.goal_id,
                    state.workflow_id,
                    state.status,
                    _dump(snapshot),
                    next_version,
                    state.updated_at,
                    state.run_id,
                    current_version,
                ),
            )
            if cursor.rowcount != 1:
                raise StateVersionConflict(f"state_version_conflict:{state.run_id}:{current_version}")
            store._insert_event(
                connection,
                state.run_id,
                event_type,
                {
                    **dict(event or {}),
                    "attempt_id": attempt.attempt_id,
                    "evidence_ids": [node.evidence_id for node in evidence],
                    "fact_ids": [fact.fact_id for fact in facts],
                    "review_ids": [review.review_id for review in reviews],
                    "plan_revisions": [plan.revision for plan in plans],
                    "state_version": next_version,
                    "state_snapshot": snapshot,
                },
            )
            return next_version
    except BaseException:
        state.state_version, state.updated_at = old_version, old_updated
        raise


__all__ = ["commit_action_outcome"]
