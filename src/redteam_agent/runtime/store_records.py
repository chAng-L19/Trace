from __future__ import annotations

import sqlite3
from dataclasses import replace
from typing import Any, Mapping, Sequence

from .model_common import utc_now
from .evidence_gate import EvidenceGate
from .model_state import (
    EvidenceNode,
    FactRecord,
    LeaseToken,
    OperationState,
    ReviewRecord,
    TaskAttempt,
    ToolCallResult,
)
from .plan import PlanRevision
from .store_common import (
    ImmutableRecordError,
    LeaseLostError,
    StoreConflictError,
    _dump,
    _load,
)


class DurableRecordStoreMixin:
    def cache_action_result(
        self,
        run_id: str,
        action_id: str,
        idempotency_key: str,
        result: ToolCallResult,
        *,
        lease_token: LeaseToken | None = None,
    ) -> None:
        if lease_token is None:
            raise LeaseLostError(f"lease_required:{run_id}:{action_id}")
        serialized = _dump(result.to_dict())
        with self.transaction(immediate=True) as connection:
            if (lease_token.run_id, lease_token.action_id) != (run_id, action_id):
                raise LeaseLostError(f"lease_identity_mismatch:{run_id}:{action_id}")
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(f"lease_lost:{run_id}:{action_id}")
            existing = connection.execute(
                "SELECT action_id, result_json FROM action_results WHERE run_id=? AND idempotency_key=?",
                (run_id, idempotency_key),
            ).fetchone()
            if existing is not None:
                if str(existing["action_id"]) != action_id or str(existing["result_json"]) != serialized:
                    raise ImmutableRecordError(
                        f"immutable_action_result_conflict:{run_id}:{idempotency_key}"
                    )
                return
            connection.execute(
                "INSERT INTO action_results(run_id, action_id, idempotency_key, result_json, created_at) "
                "VALUES(?, ?, ?, ?, ?)",
                (run_id, action_id, idempotency_key, serialized, utc_now()),
            )

    def cached_action_result(
        self,
        run_id: str,
        idempotency_key: str,
        *,
        action_id: str = "",
    ) -> ToolCallResult | None:
        with self.connection() as connection:
            row = connection.execute(
                "SELECT action_id, result_json FROM action_results WHERE run_id=? AND idempotency_key=?",
                (run_id, idempotency_key),
            ).fetchone()
        if row is not None and action_id and str(row["action_id"]) != action_id:
            raise ImmutableRecordError(
                f"action_result_scope_mismatch:{run_id}:{idempotency_key}:{action_id}"
            )
        payload = _load(row["result_json"], None) if row else None
        return ToolCallResult.from_dict(payload) if isinstance(payload, Mapping) else None

    def commit_action_outcome(
        self,
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
        from .atomic_commit import commit_action_outcome

        return commit_action_outcome(
            self,
            state=state,
            expected_state_version=expected_state_version,
            attempt=attempt,
            expected_attempt_status=expected_attempt_status,
            lease_token=lease_token,
            evidence=evidence,
            facts=facts,
            reviews=reviews,
            plans=plans,
            event_type=event_type,
            event=event,
        )

    @staticmethod
    def _immutable_insert(
        connection: sqlite3.Connection,
        *,
        table: str,
        key_column: str,
        key: str,
        json_column: str,
        serialized: str,
        sql: str,
        values: Sequence[Any],
    ) -> None:
        try:
            connection.execute(sql, tuple(values))
        except sqlite3.IntegrityError as exc:
            row = connection.execute(
                f"SELECT {json_column} FROM {table} WHERE {key_column}=?", (key,)
            ).fetchone()
            if row is None or str(row[json_column]) != serialized:
                raise ImmutableRecordError(f"immutable_record_conflict:{table}:{key}") from exc

    def save_evidence(self, node: EvidenceNode) -> None:
        provenance = node.provenance
        if provenance is None:
            raise ValueError("evidence_provenance_required")
        if not EvidenceGate.valid_trust(node):
            raise ValueError("evidence_trust_invalid")
        serialized = _dump(node.to_dict())
        with self.transaction(immediate=True) as connection:
            attempt_row = connection.execute(
                "SELECT attempt_json, status FROM task_attempts WHERE attempt_id=?",
                (provenance.attempt_id,),
            ).fetchone()
            attempt = (
                TaskAttempt.from_dict(_load(attempt_row["attempt_json"], {}))
                if attempt_row is not None
                else None
            )
            if (
                attempt is None
                or str(attempt_row["status"]) not in {"completed", "succeeded"}
                or not EvidenceGate.valid_node_identity(
                    provenance,
                    run_id=node.run_id,
                    action_id=node.action_id,
                    target=node.target,
                    tool=node.tool,
                    attempt=attempt,
                    final_required=True,
                )
            ):
                raise ValueError("evidence_attempt_identity_mismatch")
            self._immutable_insert(
                connection, table="evidence_nodes", key_column="evidence_id", key=node.evidence_id,
                json_column="node_json", serialized=serialized,
                sql="INSERT INTO evidence_nodes VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                values=(
                    node.evidence_id, node.run_id, node.action_id, node.artifact_type,
                    node.tool, node.content_hash, serialized, node.created_at,
                ),
            )

    def evidence(self, run_id: str) -> tuple[EvidenceNode, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT content_hash, node_json FROM evidence_nodes WHERE run_id=? ORDER BY created_at, evidence_id",
                (run_id,),
            ).fetchall()
        nodes: list[EvidenceNode] = []
        for row in rows:
            payload = _load(row["node_json"], None)
            try:
                node = EvidenceNode.from_dict(payload) if isinstance(payload, Mapping) else None
            except (TypeError, ValueError, OverflowError):
                node = None
            if node is not None and node.content_hash == str(row["content_hash"]):
                nodes.append(node)
        return tuple(nodes)

    def save_plan_revision(self, plan: PlanRevision) -> None:
        serialized = _dump(plan.to_dict())
        compound_key = f"{plan.run_id}\0{plan.plan_id}\0{plan.branch_id}\0{plan.revision}"
        with self.transaction(immediate=True) as connection:
            if plan.revision > 1:
                parent = connection.execute(
                    "SELECT 1 FROM plan_revisions WHERE run_id=? AND plan_id=? AND branch_id=? AND revision=?",
                    (plan.run_id, plan.plan_id, plan.branch_id, plan.parent_revision),
                ).fetchone()
                if parent is None:
                    raise ImmutableRecordError(f"plan_parent_missing:{plan.plan_id}:{plan.branch_id}:{plan.parent_revision}")
            try:
                connection.execute(
                    "INSERT INTO plan_revisions VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        plan.run_id, plan.plan_id, plan.branch_id, plan.revision, plan.parent_revision,
                        plan.plan_hash, serialized, plan.created_at,
                    ),
                )
            except sqlite3.IntegrityError as exc:
                row = connection.execute(
                    "SELECT plan_json FROM plan_revisions WHERE run_id=? AND plan_id=? AND branch_id=? AND revision=?",
                    (plan.run_id, plan.plan_id, plan.branch_id, plan.revision),
                ).fetchone()
                if row is None or str(row["plan_json"]) != serialized:
                    raise ImmutableRecordError(f"immutable_plan_conflict:{compound_key}") from exc

    def plan_revisions(self, run_id: str, *, plan_id: str = "", branch_id: str = "") -> tuple[PlanRevision, ...]:
        query, parameters = "SELECT * FROM plan_revisions WHERE run_id=?", [run_id]
        if plan_id:
            query, parameters = query + " AND plan_id=?", [*parameters, plan_id]
        if branch_id:
            query, parameters = query + " AND branch_id=?", [*parameters, branch_id]
        query += " ORDER BY branch_id, revision"
        with self.connection() as connection:
            rows = connection.execute(query, tuple(parameters)).fetchall()
        plans: list[PlanRevision] = []
        for row in rows:
            try:
                plan = PlanRevision.from_dict(_load(row["plan_json"], {}))
            except (TypeError, ValueError, OverflowError) as exc:
                raise ImmutableRecordError("plan_record_invalid") from exc
            if (
                plan.run_id,
                plan.plan_id,
                plan.branch_id,
                plan.revision,
                plan.parent_revision,
                plan.plan_hash,
            ) != (
                str(row["run_id"]),
                str(row["plan_id"]),
                str(row["branch_id"]),
                int(row["revision"]),
                int(row["parent_revision"]),
                str(row["plan_hash"]),
            ):
                raise ImmutableRecordError("plan_record_column_mismatch")
            plans.append(plan)
        return tuple(plans)

    def create_task_attempt(self, attempt: TaskAttempt) -> None:
        serialized = _dump(attempt.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection, table="task_attempts", key_column="attempt_id", key=attempt.attempt_id,
                json_column="attempt_json", serialized=serialized,
                sql="INSERT INTO task_attempts VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                values=(
                    attempt.attempt_id, attempt.run_id, attempt.branch_id, attempt.plan_revision,
                    attempt.action_id, attempt.tool, attempt.tool_version, attempt.input_hash,
                    attempt.idempotency_key, attempt.status, attempt.fencing_token, serialized,
                    attempt.started_at, attempt.finished_at,
                ),
            )

    def update_task_attempt(
        self,
        attempt: TaskAttempt,
        *,
        expected_status: str,
        lease_token: LeaseToken | None = None,
    ) -> None:
        serialized = _dump(attempt.to_dict())
        with self.transaction(immediate=True) as connection:
            if lease_token is not None:
                if (lease_token.run_id, lease_token.action_id) != (attempt.run_id, attempt.action_id):
                    raise LeaseLostError(
                        f"lease_identity_mismatch:{attempt.run_id}:{attempt.action_id}"
                    )
                if not self._assert_lease(connection, lease_token):
                    raise LeaseLostError(f"lease_lost:{attempt.run_id}:{attempt.action_id}")
            cursor = connection.execute(
                "UPDATE task_attempts SET status=?, attempt_json=?, finished_at=? "
                "WHERE attempt_id=? AND status=? AND fencing_token=?",
                (
                    attempt.status, serialized, attempt.finished_at, attempt.attempt_id,
                    expected_status, attempt.fencing_token,
                ),
            )
            if cursor.rowcount != 1:
                raise StoreConflictError(f"attempt_transition_conflict:{attempt.attempt_id}:{expected_status}")

    def claim_task_attempt(
        self,
        attempt: TaskAttempt,
        *,
        expected_statuses: Sequence[str],
        lease_token: LeaseToken,
        next_status: str = "reconciling",
    ) -> TaskAttempt:
        """Re-fence an interrupted attempt under the current action lease.

        The transition is a single CAS over both the prior status and prior
        fencing token.  A late writer holding an older token therefore cannot
        commit after recovery has claimed the attempt.
        """

        statuses = tuple(dict.fromkeys(str(item) for item in expected_statuses if str(item)))
        if not statuses:
            raise ValueError("attempt_claim_expected_status_required")
        if (attempt.run_id, attempt.action_id) != (lease_token.run_id, lease_token.action_id):
            raise LeaseLostError(f"lease_identity_mismatch:{attempt.run_id}:{attempt.action_id}")
        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(f"lease_lost:{attempt.run_id}:{attempt.action_id}")
            row = connection.execute(
                "SELECT attempt_json, status, fencing_token FROM task_attempts WHERE attempt_id=?",
                (attempt.attempt_id,),
            ).fetchone()
            if row is None:
                raise StoreConflictError(f"attempt_not_found:{attempt.attempt_id}")
            stored = TaskAttempt.from_dict(_load(row["attempt_json"], {}))
            if (
                stored.run_id,
                stored.branch_id,
                stored.plan_revision,
                stored.action_id,
            ) != (
                attempt.run_id,
                attempt.branch_id,
                attempt.plan_revision,
                attempt.action_id,
            ):
                raise StoreConflictError(f"attempt_identity_conflict:{attempt.attempt_id}")
            prior_status = str(row["status"])
            prior_fence = int(row["fencing_token"])
            if prior_status not in statuses:
                raise StoreConflictError(
                    f"attempt_claim_status_conflict:{attempt.attempt_id}:{prior_status}"
                )
            claimed = replace(
                stored,
                status=next_status,
                fencing_token=lease_token.fencing_token,
                finished_at="",
            )
            cursor = connection.execute(
                "UPDATE task_attempts SET status=?, fencing_token=?, attempt_json=?, finished_at='' "
                "WHERE attempt_id=? AND status=? AND fencing_token=?",
                (
                    claimed.status,
                    claimed.fencing_token,
                    _dump(claimed.to_dict()),
                    claimed.attempt_id,
                    prior_status,
                    prior_fence,
                ),
            )
            if cursor.rowcount != 1:
                raise StoreConflictError(f"attempt_claim_conflict:{attempt.attempt_id}")
            return claimed

    def task_attempts(self, run_id: str, *, action_id: str = "") -> tuple[TaskAttempt, ...]:
        query, parameters = "SELECT attempt_json FROM task_attempts WHERE run_id=?", [run_id]
        if action_id:
            query, parameters = query + " AND action_id=?", [*parameters, action_id]
        query += " ORDER BY started_at, attempt_id"
        with self.connection() as connection:
            rows = connection.execute(query, tuple(parameters)).fetchall()
        return tuple(TaskAttempt.from_dict(_load(row["attempt_json"], {})) for row in rows)

    def save_fact(self, fact: FactRecord) -> None:
        serialized = _dump(fact.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection, table="facts", key_column="fact_id", key=fact.fact_id,
                json_column="fact_json", serialized=serialized,
                sql="INSERT INTO facts VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                values=(
                    fact.fact_id, fact.run_id, fact.branch_id, fact.key, fact.version,
                    1 if fact.valid else 0, serialized, fact.created_at,
                ),
            )

    def facts(self, run_id: str, *, branch_id: str = "") -> tuple[FactRecord, ...]:
        query, parameters = "SELECT fact_json FROM facts WHERE run_id=?", [run_id]
        if branch_id:
            query, parameters = query + " AND branch_id=?", [*parameters, branch_id]
        query += " ORDER BY branch_id, fact_key, version"
        with self.connection() as connection:
            rows = connection.execute(query, tuple(parameters)).fetchall()
        return tuple(FactRecord.from_dict(_load(row["fact_json"], {})) for row in rows)

    def save_review(self, review: ReviewRecord) -> None:
        serialized = _dump(review.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection, table="reviews", key_column="review_id", key=review.review_id,
                json_column="review_json", serialized=serialized,
                sql="INSERT INTO reviews VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)",
                values=(
                    review.review_id, review.run_id, review.branch_id, review.plan_revision,
                    review.scope, review.subject_id, review.decision, serialized, review.created_at,
                ),
            )

    def reviews(self, run_id: str, *, branch_id: str = "") -> tuple[ReviewRecord, ...]:
        query, parameters = "SELECT review_json FROM reviews WHERE run_id=?", [run_id]
        if branch_id:
            query, parameters = query + " AND branch_id=?", [*parameters, branch_id]
        query += " ORDER BY plan_revision, created_at, review_id"
        with self.connection() as connection:
            rows = connection.execute(query, tuple(parameters)).fetchall()
        return tuple(ReviewRecord.from_dict(_load(row["review_json"], {})) for row in rows)
