from __future__ import annotations

from typing import Any, Mapping

from ..core import contract_hash
from ..core.contracts import json_mapping
from .model_common import utc_now
from .model_state import LeaseToken, OperationState
from .store_common import ImmutableRecordError, LeaseLostError, StateVersionConflict, _dump


class BudgetStoreMixin:
    def record_model_usage_once(
        self,
        run_id: str,
        *,
        request_id: str,
        usage: Mapping[str, Any] | None,
        lease_token: LeaseToken,
    ) -> OperationState:
        normalized = json_mapping(usage, field="model_budget.usage")
        usage_hash = contract_hash(normalized)
        if (lease_token.run_id, lease_token.action_id) != (run_id, "__operation__"):
            raise LeaseLostError(f"lease_identity_mismatch:{run_id}:__operation__")
        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(f"lease_lost:{run_id}:__operation__")
            operation = connection.execute(
                "SELECT * FROM operations WHERE run_id=?",
                (run_id,),
            ).fetchone()
            if operation is None:
                raise KeyError(f"operation_not_found:{run_id}")
            state = self._state_from_row(connection, operation)
            if state is None:
                raise RuntimeError(f"operation_state_corrupt:{run_id}")
            existing = connection.execute(
                "SELECT run_id, usage_hash FROM model_budget_usage WHERE request_id=?",
                (request_id,),
            ).fetchone()
            if existing is not None:
                if (str(existing["run_id"]), str(existing["usage_hash"])) != (run_id, usage_hash):
                    raise ImmutableRecordError(f"model_budget_usage_conflict:{request_id}")
                return state
            request = connection.execute(
                "SELECT run_id FROM model_requests WHERE request_id=?",
                (request_id,),
            ).fetchone()
            if request is None or str(request["run_id"]) != run_id:
                raise ImmutableRecordError(f"model_budget_request_mismatch:{request_id}")
            if state.status in {"completed", "failed", "failed_integrity", "cancelled"}:
                raise ValueError(f"operation_terminal:{state.status}")

            input_tokens, output_tokens, total_tokens = state.budget.record_token_usage(
                normalized,
                required=True,
            )
            reason = state.budget.exhaustion_reason()
            if reason:
                state.status = "paused_budget"
                state.budget.pause(reason)
            current_version = int(operation["version"])
            next_version = current_version + 1
            state.state_version = next_version
            state.updated_at = utc_now()
            snapshot = self._snapshot_payload(state, next_version)
            cursor = connection.execute(
                "UPDATE operations SET session_id=?, goal_id=?, workflow_id=?, status=?, "
                "state_json=?, version=?, updated_at=? WHERE run_id=? AND version=?",
                (
                    state.session_id,
                    state.goal.goal_id,
                    state.workflow_id,
                    state.status,
                    _dump(snapshot),
                    next_version,
                    state.updated_at,
                    run_id,
                    current_version,
                ),
            )
            if cursor.rowcount != 1:
                raise StateVersionConflict(f"state_version_conflict:{run_id}:{current_version}")
            connection.execute(
                "INSERT INTO model_budget_usage(request_id, run_id, usage_hash, input_tokens, "
                "output_tokens, total_tokens, usage_missing, usage_json, created_at) "
                "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    request_id,
                    run_id,
                    usage_hash,
                    input_tokens,
                    output_tokens,
                    total_tokens,
                    int(total_tokens is None),
                    _dump(normalized),
                    utc_now(),
                ),
            )
            self._insert_event(
                connection,
                run_id,
                "model_budget_usage_recorded",
                {
                    "request_id": request_id,
                    "usage_hash": usage_hash,
                    "input_tokens": input_tokens,
                    "output_tokens": output_tokens,
                    "total_tokens": total_tokens,
                    "usage_missing": total_tokens is None,
                    "pause_reason": reason,
                    "state_version": next_version,
                    "state_snapshot": snapshot,
                },
            )
            return state

    def enforce_budget(
        self,
        run_id: str,
        *,
        lease_token: LeaseToken,
    ) -> OperationState:
        if (lease_token.run_id, lease_token.action_id) != (run_id, "__operation__"):
            raise LeaseLostError(f"lease_identity_mismatch:{run_id}:__operation__")
        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(f"lease_lost:{run_id}:__operation__")
            row = connection.execute(
                "SELECT * FROM operations WHERE run_id=?",
                (run_id,),
            ).fetchone()
            if row is None:
                raise KeyError(f"operation_not_found:{run_id}")
            state = self._state_from_row(connection, row)
            if state is None:
                raise RuntimeError(f"operation_state_corrupt:{run_id}")
            reason = state.budget.exhaustion_reason()
            if (
                not reason
                or state.status in {"completed", "failed", "failed_integrity", "cancelled"}
                or (state.status == "paused_budget" and state.budget.pause_reason == reason)
            ):
                return state
            current_version = int(row["version"])
            next_version = current_version + 1
            state.status = "paused_budget"
            state.budget.pause(reason)
            state.state_version = next_version
            state.updated_at = utc_now()
            snapshot = self._snapshot_payload(state, next_version)
            cursor = connection.execute(
                "UPDATE operations SET status=?, state_json=?, version=?, updated_at=? "
                "WHERE run_id=? AND version=?",
                (
                    state.status,
                    _dump(snapshot),
                    next_version,
                    state.updated_at,
                    run_id,
                    current_version,
                ),
            )
            if cursor.rowcount != 1:
                raise StateVersionConflict(f"state_version_conflict:{run_id}:{current_version}")
            self._insert_event(
                connection,
                run_id,
                "run_budget_paused",
                {
                    "reason": reason,
                    "budget": state.budget.to_dict(),
                    "state_version": next_version,
                    "state_snapshot": snapshot,
                },
            )
            return state

    def model_budget_usage(self, run_id: str) -> tuple[dict[str, Any], ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM model_budget_usage WHERE run_id=? ORDER BY rowid",
                (run_id,),
            ).fetchall()
        return tuple(
            {
                "request_id": str(row["request_id"]),
                "run_id": str(row["run_id"]),
                "usage_hash": str(row["usage_hash"]),
                "input_tokens": row["input_tokens"],
                "output_tokens": row["output_tokens"],
                "total_tokens": row["total_tokens"],
                "usage_missing": bool(row["usage_missing"]),
                "usage_json": str(row["usage_json"]),
                "created_at": str(row["created_at"]),
            }
            for row in rows
        )
