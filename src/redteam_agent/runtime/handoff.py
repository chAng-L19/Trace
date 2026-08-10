from __future__ import annotations

import hashlib
import hmac
import json
import secrets
from dataclasses import dataclass, replace
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING, Any, Mapping
from uuid import uuid4

from .models import TaskAttempt, utc_now

if TYPE_CHECKING:
    import sqlite3

    from .durable_store import DurableStore


DEFAULT_HANDOFF_TTL_SECONDS = 15 * 60


class _HandoffConsumeConflict(RuntimeError):
    pass


@dataclass(frozen=True)
class HandoffRecord:
    handoff_id: str
    run_id: str
    branch_id: str
    plan_revision: int
    action_id: str
    attempt_id: str
    contract_hash: str
    status: str
    created_at: str
    expires_at: str
    consumed_at: str = ""

    @classmethod
    def from_row(cls, row: Mapping[str, Any]) -> "HandoffRecord":
        return cls(
            handoff_id=str(row["handoff_id"]),
            run_id=str(row["run_id"]),
            branch_id=str(row["branch_id"]),
            plan_revision=int(row["plan_revision"]),
            action_id=str(row["action_id"]),
            attempt_id=str(row["attempt_id"]),
            contract_hash=str(row["contract_hash"]),
            status=str(row["status"]),
            created_at=str(row["created_at"]),
            expires_at=str(row["expires_at"]),
            consumed_at=str(row["consumed_at"]),
        )

    def identity(self) -> dict[str, Any]:
        return {
            "handoff_id": self.handoff_id,
            "run_id": self.run_id,
            "branch_id": self.branch_id,
            "plan_revision": self.plan_revision,
            "action_id": self.action_id,
            "attempt_id": self.attempt_id,
            "contract_hash": self.contract_hash,
        }


def _utc_datetime(value: Any) -> datetime | None:
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _expiry(created_at: str, ttl_seconds: float) -> str:
    created = _utc_datetime(created_at) or datetime.now(timezone.utc)
    ttl = max(1.0, float(ttl_seconds))
    return (created + timedelta(seconds=ttl)).replace(microsecond=0).isoformat()


def _row_expired(row: Mapping[str, Any], *, now: datetime | None = None) -> bool:
    current = now or datetime.now(timezone.utc)
    expires = _utc_datetime(row["expires_at"])
    if expires is None:
        created = str(row["created_at"])
        expires = _utc_datetime(_expiry(created, DEFAULT_HANDOFF_TTL_SECONDS))
    return expires is None or expires <= current


def _serialize_attempt(attempt: TaskAttempt) -> str:
    return json.dumps(attempt.to_dict(), ensure_ascii=False, sort_keys=True, default=str)


def _transition_placeholder(
    connection: "sqlite3.Connection",
    attempt_id: str,
    *,
    expected_status: str,
    next_status: str,
    finished_at: str,
) -> bool:
    row = connection.execute(
        "SELECT attempt_json, status FROM task_attempts WHERE attempt_id=?",
        (attempt_id,),
    ).fetchone()
    if row is None or str(row["status"]) != expected_status:
        return False
    try:
        payload = json.loads(str(row["attempt_json"]))
    except (TypeError, ValueError, json.JSONDecodeError):
        return False
    attempt = TaskAttempt.from_dict(payload)
    if attempt.attempt_id != attempt_id or attempt.status != expected_status:
        return False
    updated = replace(attempt, status=next_status, finished_at=finished_at)
    cursor = connection.execute(
        "UPDATE task_attempts SET status=?, attempt_json=?, finished_at=? "
        "WHERE attempt_id=? AND status=?",
        (next_status, _serialize_attempt(updated), finished_at, attempt_id, expected_status),
    )
    return cursor.rowcount == 1


def _supersede_pending(
    connection: "sqlite3.Connection",
    *,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
    preserve_attempt_id: str = "",
) -> None:
    rows = connection.execute(
        "SELECT handoff_id, attempt_id FROM host_handoffs "
        "WHERE run_id=? AND branch_id=? AND plan_revision=? AND action_id=? "
        "AND status='pending' AND consumed_at=''",
        (run_id, branch_id, plan_revision, action_id),
    ).fetchall()
    if not rows:
        return
    now = utc_now()
    connection.execute(
        "UPDATE host_handoffs SET status='superseded', consumed_at=? "
        "WHERE run_id=? AND branch_id=? AND plan_revision=? AND action_id=? "
        "AND status='pending' AND consumed_at=''",
        (now, run_id, branch_id, plan_revision, action_id),
    )
    for row in rows:
        attempt_id = str(row["attempt_id"])
        if attempt_id and attempt_id != preserve_attempt_id:
            _transition_placeholder(
                connection,
                attempt_id,
                expected_status="waiting_host",
                next_status="superseded",
                finished_at=now,
            )


def _expire_row(connection: "sqlite3.Connection", row: Mapping[str, Any]) -> None:
    if str(row["status"]) != "pending":
        return
    now = utc_now()
    cursor = connection.execute(
        "UPDATE host_handoffs SET status='expired', consumed_at=? "
        "WHERE handoff_id=? AND status='pending' AND consumed_at=''",
        (now, str(row["handoff_id"])),
    )
    if cursor.rowcount == 1:
        _transition_placeholder(
            connection,
            str(row["attempt_id"]),
            expected_status="waiting_host",
            next_status="expired",
            finished_at=now,
        )


def ensure_handoff_schema(connection: "sqlite3.Connection") -> None:
    connection.executescript(
        """
        CREATE TABLE IF NOT EXISTS host_handoffs (
            handoff_id TEXT PRIMARY KEY,
            run_id TEXT NOT NULL,
            branch_id TEXT NOT NULL,
            plan_revision INTEGER NOT NULL,
            action_id TEXT NOT NULL,
            attempt_id TEXT NOT NULL,
            token_hash TEXT NOT NULL,
            contract_hash TEXT NOT NULL,
            status TEXT NOT NULL,
            created_at TEXT NOT NULL,
            expires_at TEXT NOT NULL DEFAULT '',
            consumed_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE,
            FOREIGN KEY(attempt_id) REFERENCES task_attempts(attempt_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_handoffs_pending
            ON host_handoffs(run_id, branch_id, plan_revision, action_id, status);
        """
    )
    columns = {
        str(row["name"])
        for row in connection.execute("PRAGMA table_info(host_handoffs)").fetchall()
    }
    if "expires_at" not in columns:
        connection.execute(
            "ALTER TABLE host_handoffs ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''"
        )
    legacy = connection.execute(
        "SELECT handoff_id, created_at FROM host_handoffs WHERE expires_at=''"
    ).fetchall()
    for row in legacy:
        connection.execute(
            "UPDATE host_handoffs SET expires_at=? WHERE handoff_id=? AND expires_at=''",
            (
                _expiry(str(row["created_at"]), DEFAULT_HANDOFF_TTL_SECONDS),
                str(row["handoff_id"]),
            ),
        )


def create_handoff(
    store: "DurableStore",
    *,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
    attempt_id: str,
    contract_hash: str,
    ttl_seconds: float = DEFAULT_HANDOFF_TTL_SECONDS,
) -> tuple[str, str]:
    if not all((run_id, branch_id, action_id, attempt_id, contract_hash)) or plan_revision < 1:
        raise ValueError("handoff_identity_required")
    raw_token = secrets.token_urlsafe(32)
    token_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    handoff_id = f"handoff-{uuid4().hex}"
    now = utc_now()
    expires_at = _expiry(now, ttl_seconds)
    with store.transaction(immediate=True) as connection:
        operation = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
        state = store._state_from_row(connection, operation) if operation else None
        if state is None or (state.branch_id, state.plan_revision) != (branch_id, plan_revision):
            raise ValueError("handoff_operation_revision_mismatch")
        if state.status != "waiting_host" or state.current_action_id != action_id:
            raise ValueError("handoff_operation_not_waiting_for_action")
        attempt = connection.execute(
            "SELECT run_id, branch_id, plan_revision, action_id, status "
            "FROM task_attempts WHERE attempt_id=?",
            (attempt_id,),
        ).fetchone()
        if attempt is None or (
            str(attempt["run_id"]),
            str(attempt["branch_id"]),
            int(attempt["plan_revision"]),
            str(attempt["action_id"]),
        ) != (run_id, branch_id, plan_revision, action_id):
            raise ValueError("handoff_attempt_mismatch")
        if str(attempt["status"]) != "waiting_host":
            raise ValueError("handoff_attempt_not_waiting_host")
        _supersede_pending(
            connection,
            run_id=run_id,
            branch_id=branch_id,
            plan_revision=plan_revision,
            action_id=action_id,
            preserve_attempt_id=attempt_id,
        )
        connection.execute(
            "INSERT INTO host_handoffs("
            "handoff_id, run_id, branch_id, plan_revision, action_id, attempt_id, "
            "token_hash, contract_hash, status, created_at, expires_at, consumed_at"
            ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, '')",
            (
                handoff_id,
                run_id,
                branch_id,
                plan_revision,
                action_id,
                attempt_id,
                token_hash,
                contract_hash,
                now,
                expires_at,
            ),
        )
    return handoff_id, raw_token


def rotate_handoff(
    store: "DurableStore",
    *,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
    contract_hash: str,
    ttl_seconds: float = DEFAULT_HANDOFF_TTL_SECONDS,
) -> tuple[str, str, TaskAttempt]:
    """Atomically replace a lost/stale receipt and its placeholder attempt."""

    if not all((run_id, branch_id, action_id, contract_hash)) or plan_revision < 1:
        raise ValueError("handoff_identity_required")
    raw_token = secrets.token_urlsafe(32)
    token_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    handoff_id = f"handoff-{uuid4().hex}"
    placeholder = TaskAttempt.create(
        run_id=run_id,
        branch_id=branch_id,
        plan_revision=plan_revision,
        action_id=action_id,
        tool="host:handoff",
        tool_version="host-receipt-v1",
        input_hash=contract_hash,
        idempotency_key="pending",
    )
    placeholder = replace(
        placeholder,
        idempotency_key=hashlib.sha256(
            f"{run_id}\0{branch_id}\0{plan_revision}\0{action_id}\0{contract_hash}\0{placeholder.attempt_id}".encode(
                "utf-8"
            )
        ).hexdigest(),
        status="waiting_host",
    )
    now = utc_now()
    expires_at = _expiry(now, ttl_seconds)
    with store.transaction(immediate=True) as connection:
        operation = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
        state = store._state_from_row(connection, operation) if operation else None
        if state is None or (state.branch_id, state.plan_revision) != (branch_id, plan_revision):
            raise ValueError("handoff_operation_revision_mismatch")
        if state.status != "waiting_host" or state.current_action_id != action_id:
            raise ValueError("handoff_operation_not_waiting_for_action")
        _supersede_pending(
            connection,
            run_id=run_id,
            branch_id=branch_id,
            plan_revision=plan_revision,
            action_id=action_id,
        )
        serialized = _serialize_attempt(placeholder)
        connection.execute(
            "INSERT INTO task_attempts("
            "attempt_id, run_id, branch_id, plan_revision, action_id, tool, tool_version, "
            "input_hash, idempotency_key, status, fencing_token, attempt_json, started_at, finished_at"
            ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                placeholder.attempt_id,
                placeholder.run_id,
                placeholder.branch_id,
                placeholder.plan_revision,
                placeholder.action_id,
                placeholder.tool,
                placeholder.tool_version,
                placeholder.input_hash,
                placeholder.idempotency_key,
                placeholder.status,
                placeholder.fencing_token,
                serialized,
                placeholder.started_at,
                placeholder.finished_at,
            ),
        )
        connection.execute(
            "INSERT INTO host_handoffs("
            "handoff_id, run_id, branch_id, plan_revision, action_id, attempt_id, "
            "token_hash, contract_hash, status, created_at, expires_at, consumed_at"
            ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, '')",
            (
                handoff_id,
                run_id,
                branch_id,
                plan_revision,
                action_id,
                placeholder.attempt_id,
                token_hash,
                contract_hash,
                now,
                expires_at,
            ),
        )
    return handoff_id, raw_token, placeholder


def get_handoff(store: "DurableStore", handoff_id: str) -> HandoffRecord | None:
    if not handoff_id:
        return None
    with store.transaction(immediate=True) as connection:
        row = connection.execute(
            "SELECT * FROM host_handoffs WHERE handoff_id=?",
            (handoff_id,),
        ).fetchone()
        if row is not None and str(row["status"]) == "pending" and _row_expired(row):
            _expire_row(connection, row)
            row = connection.execute(
                "SELECT * FROM host_handoffs WHERE handoff_id=?",
                (handoff_id,),
            ).fetchone()
    return HandoffRecord.from_row(row) if row is not None else None


def pending_handoff(
    store: "DurableStore",
    *,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
) -> HandoffRecord | None:
    selected = None
    with store.transaction(immediate=True) as connection:
        rows = connection.execute(
            "SELECT * FROM host_handoffs "
            "WHERE run_id=? AND branch_id=? AND plan_revision=? AND action_id=? "
            "AND status='pending' AND consumed_at='' "
            "ORDER BY created_at DESC, handoff_id DESC",
            (run_id, branch_id, int(plan_revision), action_id),
        ).fetchall()
        for row in rows:
            if _row_expired(row):
                _expire_row(connection, row)
                continue
            selected = row
            break
    return HandoffRecord.from_row(selected) if selected is not None else None


def consume_handoff(
    store: "DurableStore",
    *,
    handoff_id: str,
    raw_token: str,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
    attempt_id: str,
    contract_hash: str,
) -> bool:
    if not raw_token:
        return False
    supplied_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    try:
        with store.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM host_handoffs WHERE handoff_id=?",
                (handoff_id,),
            ).fetchone()
            if row is None or str(row["status"]) != "pending" or str(row["consumed_at"]):
                return False
            if _row_expired(row):
                _expire_row(connection, row)
                return False
            stored_identity = (
                str(row["run_id"]),
                str(row["branch_id"]),
                int(row["plan_revision"]),
                str(row["action_id"]),
                str(row["attempt_id"]),
                str(row["contract_hash"]),
            )
            if stored_identity != (run_id, branch_id, plan_revision, action_id, attempt_id, contract_hash):
                return False
            if not hmac.compare_digest(str(row["token_hash"]), supplied_hash):
                return False
            operation = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
            state = store._state_from_row(connection, operation) if operation else None
            if state is None or (state.branch_id, state.plan_revision) != (branch_id, plan_revision):
                return False
            if state.status != "waiting_host" or state.current_action_id != action_id:
                return False
            attempt_row = connection.execute(
                "SELECT attempt_json, status FROM task_attempts WHERE attempt_id=?",
                (attempt_id,),
            ).fetchone()
            if attempt_row is None or str(attempt_row["status"]) != "waiting_host":
                return False
            try:
                attempt_payload = json.loads(str(attempt_row["attempt_json"]))
            except (TypeError, ValueError, json.JSONDecodeError):
                return False
            placeholder = TaskAttempt.from_dict(attempt_payload)
            if (
                placeholder.run_id,
                placeholder.branch_id,
                placeholder.plan_revision,
                placeholder.action_id,
                placeholder.status,
            ) != (run_id, branch_id, plan_revision, action_id, "waiting_host"):
                return False
            consumed_at = utc_now()
            cursor = connection.execute(
                "UPDATE host_handoffs SET status='consumed', consumed_at=? "
                "WHERE handoff_id=? AND status='pending' AND token_hash=? AND consumed_at=''",
                (consumed_at, handoff_id, supplied_hash),
            )
            if cursor.rowcount != 1:
                raise _HandoffConsumeConflict
            if not _transition_placeholder(
                connection,
                attempt_id,
                expected_status="waiting_host",
                next_status="consumed",
                finished_at=consumed_at,
            ):
                raise _HandoffConsumeConflict
    except _HandoffConsumeConflict:
        return False
    return True


def validate_handoff(
    store: "DurableStore",
    *,
    handoff_id: str,
    raw_token: str,
    run_id: str,
    branch_id: str,
    plan_revision: int,
    action_id: str,
    attempt_id: str,
    contract_hash: str,
) -> bool:
    """Read-only receipt validation used to preflight an entire batch."""

    if not raw_token:
        return False
    supplied_hash = hashlib.sha256(raw_token.encode("utf-8")).hexdigest()
    with store.connection() as connection:
        row = connection.execute(
            "SELECT * FROM host_handoffs WHERE handoff_id=?",
            (handoff_id,),
        ).fetchone()
        if (
            row is None
            or str(row["status"]) != "pending"
            or str(row["consumed_at"])
            or _row_expired(row)
        ):
            return False
        stored_identity = (
            str(row["run_id"]),
            str(row["branch_id"]),
            int(row["plan_revision"]),
            str(row["action_id"]),
            str(row["attempt_id"]),
            str(row["contract_hash"]),
        )
        if stored_identity != (
            run_id,
            branch_id,
            int(plan_revision),
            action_id,
            attempt_id,
            contract_hash,
        ):
            return False
        if not hmac.compare_digest(str(row["token_hash"]), supplied_hash):
            return False
        operation = connection.execute(
            "SELECT * FROM operations WHERE run_id=?",
            (run_id,),
        ).fetchone()
        state = store._state_from_row(connection, operation) if operation else None
        if state is None or (
            state.branch_id,
            state.plan_revision,
            state.status,
            state.current_action_id,
        ) != (branch_id, int(plan_revision), "waiting_host", action_id):
            return False
        attempt_row = connection.execute(
            "SELECT attempt_json, status FROM task_attempts WHERE attempt_id=?",
            (attempt_id,),
        ).fetchone()
        if attempt_row is None or str(attempt_row["status"]) != "waiting_host":
            return False
        try:
            attempt_payload = json.loads(str(attempt_row["attempt_json"]))
        except (TypeError, ValueError, json.JSONDecodeError):
            return False
        placeholder = TaskAttempt.from_dict(attempt_payload)
        return (
            placeholder.run_id,
            placeholder.branch_id,
            placeholder.plan_revision,
            placeholder.action_id,
            placeholder.status,
        ) == (run_id, branch_id, int(plan_revision), action_id, "waiting_host")


__all__ = [
    "DEFAULT_HANDOFF_TTL_SECONDS",
    "HandoffRecord",
    "consume_handoff",
    "create_handoff",
    "ensure_handoff_schema",
    "get_handoff",
    "pending_handoff",
    "rotate_handoff",
    "validate_handoff",
]
