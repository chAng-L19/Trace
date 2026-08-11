from __future__ import annotations

import hashlib
import json

from .model_common import utc_now
from .model_state import LeaseToken, OperationState, ToolCallResult
from .store_common import (
    ImmutableRecordError,
    LeaseLostError,
    StateVersionConflict,
    _dump,
    _load,
)


BUDGET_DELTA_ACTION_ID = "__agent_service_budget_delta__"


class ServiceStoreMixin:
    def apply_budget_delta_once(
        self,
        run_id: str,
        *,
        actions: int,
        tokens: int,
        time_seconds: float,
        deadline: str,
        acknowledge_missing_usage: bool,
        idempotency_key: str,
        lease_token: LeaseToken,
    ) -> OperationState:
        client_key = idempotency_key.strip()
        if not client_key:
            raise ValueError("budget_delta_idempotency_key_required")
        if (lease_token.run_id, lease_token.action_id) != (run_id, "__operation__"):
            raise LeaseLostError(f"lease_identity_mismatch:{run_id}:__operation__")
        request = {
            "actions": int(actions),
            "tokens": int(tokens),
            "time_seconds": float(time_seconds),
            "deadline": str(deadline or ""),
            "acknowledge_missing_usage": bool(acknowledge_missing_usage),
        }
        canonical = json.dumps(
            request,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        request_hash = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
        durable_key = hashlib.sha256(
            f"agent-service-budget\0{run_id}\0{client_key}".encode("utf-8")
        ).hexdigest()

        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(
                    f"lease_lost:{run_id}:__operation__:{lease_token.fencing_token}"
                )
            row = connection.execute(
                "SELECT * FROM operations WHERE run_id=?",
                (run_id,),
            ).fetchone()
            if row is None:
                raise KeyError(f"operation_not_found:{run_id}")
            state = self._state_from_row(connection, row)
            if state is None:
                raise RuntimeError(f"operation_state_corrupt:{run_id}")

            existing = connection.execute(
                "SELECT action_id, result_json FROM action_results "
                "WHERE run_id=? AND idempotency_key=?",
                (run_id, durable_key),
            ).fetchone()
            if existing is not None:
                payload = _load(existing["result_json"], {})
                result = ToolCallResult.from_dict(payload) if isinstance(payload, dict) else None
                if (
                    str(existing["action_id"]) != BUDGET_DELTA_ACTION_ID
                    or result is None
                    or result.input_hash != request_hash
                ):
                    raise ImmutableRecordError(
                        f"budget_delta_idempotency_conflict:{run_id}:{durable_key}"
                    )
                return state
            if state.status in {"completed", "failed", "failed_integrity", "cancelled"}:
                raise ValueError(f"operation_terminal:{state.status}")

            changed = state.budget.apply_delta(**request)
            if state.status == "paused_budget" and not state.budget.exhaustion_reason():
                state.status = "running"
            current_version = int(row["version"])
            if changed:
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
                    raise StateVersionConflict(
                        f"state_version_conflict:{run_id}:{current_version}"
                    )
            else:
                snapshot = self._snapshot_payload(state, state.state_version)

            output = {
                "kind": "budget_delta",
                "changed": changed,
                "state_version": state.state_version,
                "request_hash": request_hash,
            }
            output_hash = hashlib.sha256(
                json.dumps(output, sort_keys=True, separators=(",", ":")).encode("utf-8")
            ).hexdigest()
            result = ToolCallResult(
                status="success",
                output=output,
                tool="agent-service:budget-delta",
                call_id=f"budget-delta-{durable_key[:24]}",
                input_hash=request_hash,
                output_hash=output_hash,
                tool_version="agent-service-v1",
            )
            connection.execute(
                "INSERT INTO action_results(run_id, action_id, idempotency_key, result_json, created_at) "
                "VALUES(?, ?, ?, ?, ?)",
                (
                    run_id,
                    BUDGET_DELTA_ACTION_ID,
                    durable_key,
                    _dump(result.to_dict()),
                    utc_now(),
                ),
            )
            self._insert_event(
                connection,
                run_id,
                "budget_delta_applied",
                {
                    **request,
                    "idempotency_hash": durable_key,
                    "changed": changed,
                    "state_version": state.state_version,
                    "state_snapshot": snapshot,
                },
            )
            return state
