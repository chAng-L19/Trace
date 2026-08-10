from __future__ import annotations

import hashlib
import hmac
import json
from dataclasses import replace
from datetime import datetime, timezone
from typing import Any, Mapping

from .model_common import utc_now
from .model_state import TaskAttempt, ToolCallResult
from .store_common import (
    MAX_HANDOFF_OBSERVATION_BYTES,
    StateVersionConflict,
    StoreConflictError,
    _dump,
    _load,
)


class HandoffStoreMixin:
    def save_session_binding(
        self,
        session_id: str,
        binding: Mapping[str, Any],
        *,
        expected_version: int | None = None,
    ) -> int:
        serialized = _dump(dict(binding))
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT version FROM session_bindings WHERE session_id=?", (session_id,)
            ).fetchone()
            if row is None:
                if expected_version not in (None, 0):
                    raise StateVersionConflict(f"session_binding_version_conflict:{session_id}")
                connection.execute(
                    "INSERT INTO session_bindings VALUES(?, ?, 1, ?, ?)",
                    (session_id, str(binding.get("run_id") or ""), serialized, utc_now()),
                )
                return 1
            current = int(row["version"])
            if expected_version is not None and expected_version != current:
                raise StateVersionConflict(f"session_binding_version_conflict:{session_id}:{expected_version}:{current}")
            next_version = current + 1
            cursor = connection.execute(
                "UPDATE session_bindings SET run_id=?, version=?, binding_json=?, updated_at=? WHERE session_id=? AND version=?",
                (str(binding.get("run_id") or ""), next_version, serialized, utc_now(), session_id, current),
            )
            if cursor.rowcount != 1:
                raise StateVersionConflict(
                    f"session_binding_version_conflict:{session_id}:{current}"
                )
            return next_version

    def session_binding(self, session_id: str) -> Mapping[str, Any] | None:
        with self.connection() as connection:
            row = connection.execute(
                "SELECT version, binding_json FROM session_bindings WHERE session_id=?", (session_id,)
            ).fetchone()
        if row is None:
            return None
        payload = _load(row["binding_json"], {})
        return {**dict(payload or {}), "_version": int(row["version"])}

    def create_handoff(self, **identity: Any) -> tuple[str, str]:
        from .handoff import create_handoff
        return create_handoff(self, **identity)

    def consume_handoff(self, **receipt: Any) -> bool:
        from .handoff import consume_handoff
        return consume_handoff(self, **receipt)

    def validate_handoff(self, **receipt: Any) -> bool:
        from .handoff import validate_handoff
        return validate_handoff(self, **receipt)

    def receive_handoff_observation(
        self,
        *,
        handoff_id: str,
        raw_token: str,
        run_id: str,
        branch_id: str,
        plan_revision: int,
        action_id: str,
        attempt_id: str,
        contract_hash: str,
        output: Any,
        tool: str = "host-agent",
        usage: Mapping[str, Any] | None = None,
    ) -> TaskAttempt | None:
        """Atomically persist a Host result and consume its one-time receipt.

        The cached result and ``consumed`` attempt form a durable inbox.  If the
        process exits before Executor commits Evidence, Resume can reconcile the
        consumed attempt without asking the Host to resend the observation.
        """

        if not raw_token:
            return None
        try:
            output_json = json.dumps(output, ensure_ascii=False, sort_keys=True, default=str)
        except (TypeError, ValueError, OverflowError):
            return None
        if len(output_json.encode("utf-8")) > MAX_HANDOFF_OBSERVATION_BYTES:
            return None
        supplied_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
        identity = (run_id, branch_id, int(plan_revision), action_id, attempt_id, contract_hash)
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM host_handoffs WHERE handoff_id=?",
                (handoff_id,),
            ).fetchone()
            if row is None or str(row["status"]) != "pending" or str(row["consumed_at"]):
                return None
            expires_at = str(row["expires_at"] or "")
            try:
                expires = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
                if expires.tzinfo is None:
                    expires = expires.replace(tzinfo=timezone.utc)
                expired = expires.astimezone(timezone.utc) <= datetime.now(timezone.utc)
            except (TypeError, ValueError):
                expired = True
            if expired:
                now = utc_now()
                connection.execute(
                    "UPDATE host_handoffs SET status='expired', consumed_at=? "
                    "WHERE handoff_id=? AND status='pending'",
                    (now, handoff_id),
                )
                attempt_row = connection.execute(
                    "SELECT attempt_json FROM task_attempts WHERE attempt_id=? AND status='waiting_host'",
                    (attempt_id,),
                ).fetchone()
                if attempt_row is not None:
                    attempt = TaskAttempt.from_dict(_load(attempt_row["attempt_json"], {}))
                    expired_attempt = replace(attempt, status="expired", finished_at=now)
                    connection.execute(
                        "UPDATE task_attempts SET status='expired', attempt_json=?, finished_at=? "
                        "WHERE attempt_id=? AND status='waiting_host'",
                        (_dump(expired_attempt.to_dict()), now, attempt_id),
                    )
                return None
            stored_identity = (
                str(row["run_id"]),
                str(row["branch_id"]),
                int(row["plan_revision"]),
                str(row["action_id"]),
                str(row["attempt_id"]),
                str(row["contract_hash"]),
            )
            if stored_identity != identity or not hmac.compare_digest(str(row["token_hash"]), supplied_hash):
                return None
            operation_row = connection.execute(
                "SELECT * FROM operations WHERE run_id=?",
                (run_id,),
            ).fetchone()
            state = self._state_from_row(connection, operation_row) if operation_row else None
            if state is None or (
                state.branch_id,
                state.plan_revision,
                state.status,
                state.current_action_id,
            ) != (branch_id, int(plan_revision), "waiting_host", action_id):
                return None
            attempt_row = connection.execute(
                "SELECT attempt_json, status FROM task_attempts WHERE attempt_id=?",
                (attempt_id,),
            ).fetchone()
            if attempt_row is None or str(attempt_row["status"]) != "waiting_host":
                return None
            attempt = TaskAttempt.from_dict(_load(attempt_row["attempt_json"], {}))
            if (
                attempt.run_id,
                attempt.branch_id,
                attempt.plan_revision,
                attempt.action_id,
                attempt.status,
            ) != (run_id, branch_id, int(plan_revision), action_id, "waiting_host"):
                return None

            now = utc_now()
            result = ToolCallResult(
                status="success",
                output=output,
                tool=tool or "host-agent",
                started_at=now,
                finished_at=now,
                call_id=f"receipt-{attempt_id}",
                input_hash=attempt.input_hash,
                output_hash=hashlib.sha256(output_json.encode("utf-8")).hexdigest(),
                tool_version=attempt.tool_version,
            )
            serialized_result = _dump(result.to_dict())
            existing_result = connection.execute(
                "SELECT action_id, result_json FROM action_results WHERE run_id=? AND idempotency_key=?",
                (run_id, attempt.idempotency_key),
            ).fetchone()
            if existing_result is not None:
                if (
                    str(existing_result["action_id"]) != action_id
                    or str(existing_result["result_json"]) != serialized_result
                ):
                    raise StoreConflictError(
                        f"immutable_action_result_conflict:{run_id}:{attempt.idempotency_key}"
                    )
            else:
                connection.execute(
                    "INSERT INTO action_results(run_id, action_id, idempotency_key, result_json, created_at) "
                    "VALUES(?, ?, ?, ?, ?)",
                    (run_id, action_id, attempt.idempotency_key, serialized_result, now),
                )
            consumed = replace(
                attempt,
                status="consumed",
                result={"receipt_usage": dict(usage or {})},
                finished_at=now,
            )
            attempt_cursor = connection.execute(
                "UPDATE task_attempts SET status='consumed', attempt_json=?, finished_at=? "
                "WHERE attempt_id=? AND status='waiting_host'",
                (_dump(consumed.to_dict()), now, attempt_id),
            )
            handoff_cursor = connection.execute(
                "UPDATE host_handoffs SET status='consumed', consumed_at=? "
                "WHERE handoff_id=? AND status='pending' AND token_hash=? AND consumed_at=''",
                (now, handoff_id, supplied_hash),
            )
            if attempt_cursor.rowcount != 1 or handoff_cursor.rowcount != 1:
                raise StoreConflictError(f"handoff_receive_conflict:{handoff_id}")
            return consumed

    def get_handoff(self, handoff_id: str):
        from .handoff import get_handoff
        return get_handoff(self, handoff_id)

    def pending_handoff(self, **identity: Any):
        from .handoff import pending_handoff
        return pending_handoff(self, **identity)

    def rotate_handoff(self, **identity: Any):
        from .handoff import rotate_handoff
        return rotate_handoff(self, **identity)

