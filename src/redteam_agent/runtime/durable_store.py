from __future__ import annotations

import hashlib
import json
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator, Mapping, Sequence

from .model_common import utc_now
from .model_state import LeaseToken, OperationState, ToolCallResult
from .security import secure_directory, secure_file
from .store_common import (
    MAX_HANDOFF_OBSERVATION_BYTES,
    SCHEMA_VERSION,
    ImmutableRecordError,
    LeaseLostError,
    StateVersionConflict,
    StoreConflictError,
    _dump,
    _load,
)
from .store_handoff import HandoffStoreMixin
from .store_records import DurableRecordStoreMixin
from .store_migrations import MigrationReport, SchemaMigrationError, apply_migrations, migration_history
from .model_store import ModelStoreMixin
from .conversation_store import ConversationStoreMixin
from .budget_store import BudgetStoreMixin
from .exploration import ExplorationStoreMixin
from .session_journal import JournalStoreMixin

__all__ = [
    "DurableStore",
    "ImmutableRecordError",
    "LeaseLostError",
    "MAX_HANDOFF_OBSERVATION_BYTES",
    "MigrationReport",
    "SCHEMA_VERSION",
    "SchemaMigrationError",
    "StateVersionConflict",
    "StoreConflictError",
]


BUDGET_DELTA_ACTION_ID = "__agent_service_budget_delta__"


class ServiceStoreMixin:
    """Durable budget-delta writes share the store transaction and lease."""

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
        request = {"actions": int(actions), "tokens": int(tokens), "time_seconds": float(time_seconds), "deadline": str(deadline or ""), "acknowledge_missing_usage": bool(acknowledge_missing_usage)}
        request_hash = hashlib.sha256(json.dumps(request, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
        durable_key = hashlib.sha256(f"agent-service-budget\0{run_id}\0{client_key}".encode("utf-8")).hexdigest()
        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, lease_token):
                raise LeaseLostError(f"lease_lost:{run_id}:__operation__:{lease_token.fencing_token}")
            row = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
            if row is None:
                raise KeyError(f"operation_not_found:{run_id}")
            state = self._state_from_row(connection, row)
            if state is None:
                raise RuntimeError(f"operation_state_corrupt:{run_id}")
            existing = connection.execute("SELECT action_id, result_json FROM action_results WHERE run_id=? AND idempotency_key=?", (run_id, durable_key)).fetchone()
            if existing is not None:
                result = ToolCallResult.from_dict(_load(existing["result_json"], {}))
                if str(existing["action_id"]) != BUDGET_DELTA_ACTION_ID or result.input_hash != request_hash:
                    raise ImmutableRecordError(f"budget_delta_idempotency_conflict:{run_id}:{durable_key}")
                return state
            if state.status in {"completed", "failed", "failed_integrity", "cancelled"}:
                raise ValueError(f"operation_terminal:{state.status}")
            changed = state.budget.apply_delta(**request)
            if state.status == "paused_budget" and not state.budget.exhaustion_reason():
                state.status = "running"
            current_version = int(row["version"])
            if changed:
                next_version = current_version + 1
                state.state_version, state.updated_at = next_version, utc_now()
                snapshot = self._snapshot_payload(state, next_version)
                cursor = connection.execute("UPDATE operations SET session_id=?, goal_id=?, workflow_id=?, status=?, state_json=?, version=?, updated_at=? WHERE run_id=? AND version=?", (state.session_id, state.goal.goal_id, state.workflow_id, state.status, _dump(snapshot), next_version, state.updated_at, run_id, current_version))
                if cursor.rowcount != 1:
                    raise StateVersionConflict(f"state_version_conflict:{run_id}:{current_version}")
            else:
                snapshot = self._snapshot_payload(state, state.state_version)
            output = {"kind": "budget_delta", "changed": changed, "state_version": state.state_version, "request_hash": request_hash}
            output_hash = hashlib.sha256(json.dumps(output, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
            result = ToolCallResult(status="success", output=output, tool="agent-service:budget-delta", call_id=f"budget-delta-{durable_key[:24]}", input_hash=request_hash, output_hash=output_hash, tool_version="agent-service-v1")
            connection.execute("INSERT INTO action_results(run_id, action_id, idempotency_key, result_json, created_at) VALUES(?, ?, ?, ?, ?)", (run_id, BUDGET_DELTA_ACTION_ID, durable_key, _dump(result.to_dict()), utc_now()))
            self._insert_event(connection, run_id, "budget_delta_applied", {**request, "idempotency_hash": durable_key, "changed": changed, "state_version": state.state_version, "state_snapshot": snapshot})
            return state


class StoreSchemaMixin:
    @staticmethod
    def _columns(connection: sqlite3.Connection, table: str) -> set[str]:
        return {str(row["name"]) for row in connection.execute(f"PRAGMA table_info({table})").fetchall()}

    def _initialize(self) -> None:
        with self.transaction(immediate=True) as connection:
            self._migration_report = apply_migrations(self, connection)

    @property
    def migration_report(self) -> MigrationReport:
        return self._migration_report

    def schema_version(self) -> int:
        with self.connection() as connection:
            return int(connection.execute("PRAGMA user_version").fetchone()[0])

    def migration_history(self) -> tuple[tuple[int, str], ...]:
        with self.connection() as connection:
            return migration_history(connection)

    def _migrate_evidence_table(self, connection: sqlite3.Connection) -> None:
        row = connection.execute("SELECT sql FROM sqlite_master WHERE type='table' AND name='evidence_nodes'").fetchone()
        table_sql = str(row["sql"] or "") if row else ""
        if "unique(run_id,action_id,artifact_type,tool,content_hash)" not in "".join(table_sql.casefold().split()) and "tool" in self._columns(connection, "evidence_nodes"):
            return
        legacy = "evidence_nodes_schema2"
        connection.execute(f"DROP TABLE IF EXISTS {legacy}")
        connection.execute(f"ALTER TABLE evidence_nodes RENAME TO {legacy}")
        connection.execute("""
            CREATE TABLE evidence_nodes (
                evidence_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, action_id TEXT NOT NULL,
                artifact_type TEXT NOT NULL, tool TEXT NOT NULL, content_hash TEXT NOT NULL,
                node_json TEXT NOT NULL, created_at TEXT NOT NULL,
                FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
            )
            """)
        columns = self._columns(connection, legacy)
        for legacy_row in connection.execute(f"SELECT * FROM {legacy}").fetchall():
            payload = _load(legacy_row["node_json"], {})
            tool = str(legacy_row["tool"]) if "tool" in columns else str((payload or {}).get("tool") or "legacy")
            connection.execute("INSERT OR IGNORE INTO evidence_nodes VALUES(?, ?, ?, ?, ?, ?, ?, ?)", (legacy_row["evidence_id"], legacy_row["run_id"], legacy_row["action_id"], legacy_row["artifact_type"], tool, legacy_row["content_hash"], legacy_row["node_json"], legacy_row["created_at"]))
        connection.execute(f"DROP TABLE {legacy}")


class DurableStore(
    HandoffStoreMixin,
    DurableRecordStoreMixin,
    ServiceStoreMixin,
    ModelStoreMixin,
    ConversationStoreMixin,
    BudgetStoreMixin,
    ExplorationStoreMixin,
    JournalStoreMixin,
    StoreSchemaMixin,
):
    def __init__(self, root: Path) -> None:
        self.root = root
        secure_directory(self.root)
        self.path = self.root / "runtime.sqlite3"
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30.0)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA busy_timeout=30000")
        secure_file(self.path)
        secure_file(self.path.with_name(f"{self.path.name}-wal"))
        secure_file(self.path.with_name(f"{self.path.name}-shm"))
        return connection

    @contextmanager
    def connection(self) -> Iterator[sqlite3.Connection]:
        connection = self._connect()
        try:
            yield connection
        finally:
            connection.close()

    @contextmanager
    def transaction(self, *, immediate: bool = False) -> Iterator[sqlite3.Connection]:
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE" if immediate else "BEGIN")
            yield connection
            connection.commit()
        except BaseException:
            connection.rollback()
            raise
        finally:
            connection.close()

    @staticmethod
    def _snapshot_payload(state: OperationState, version: int) -> dict[str, Any]:
        payload = state.to_dict()
        payload["state_version"] = version
        return payload

    def _insert_event(
        self,
        connection: sqlite3.Connection,
        run_id: str,
        event_type: str,
        payload: Mapping[str, Any],
    ) -> None:
        created_at = utc_now()
        cursor = connection.execute(
            "INSERT INTO operation_events(run_id, event_type, payload_json, created_at) VALUES(?, ?, ?, ?)",
            (run_id, event_type, _dump(dict(payload)), created_at),
        )
        self._insert_journal_entry(
            connection,
            run_id=run_id,
            entry_type=f"event:{event_type}",
            raw_table="operation_events",
            raw_id=str(cursor.lastrowid),
            raw_json=_dump({"event_type": event_type, "payload": dict(payload)}),
            created_at=created_at,
        )

    def _recover_snapshot(self, connection: sqlite3.Connection, run_id: str) -> Mapping[str, Any] | None:
        rows = connection.execute(
            "SELECT payload_json FROM operation_events WHERE run_id=? ORDER BY event_id DESC",
            (run_id,),
        ).fetchall()
        for row in rows:
            payload = _load(row["payload_json"], {})
            snapshot = payload.get("state_snapshot") if isinstance(payload, Mapping) else None
            if isinstance(snapshot, Mapping) and str(snapshot.get("run_id") or "") == run_id:
                return snapshot
        return None

    def _state_from_row(self, connection: sqlite3.Connection, row: sqlite3.Row) -> OperationState | None:
        payload = _load(row["state_json"], None)
        if not isinstance(payload, Mapping) or str(payload.get("run_id") or "") != str(row["run_id"]):
            payload = self._recover_snapshot(connection, str(row["run_id"]))
        if not isinstance(payload, Mapping):
            return None
        try:
            state = OperationState.from_dict(payload)
        except (TypeError, ValueError, OverflowError):
            return None
        state.state_version = max(1, int(row["version"]))
        return state

    def create_operation(self, state: OperationState, *, event: Mapping[str, Any]) -> OperationState:
        old_version = state.state_version
        old_updated = state.updated_at
        state.state_version = 1
        state.updated_at = utc_now()
        snapshot = self._snapshot_payload(state, 1)
        try:
            with self.transaction(immediate=True) as connection:
                cursor = connection.execute(
                    "INSERT INTO operations(run_id, session_id, goal_id, workflow_id, status, state_json, version, created_at, updated_at) "
                    "VALUES(?, ?, ?, ?, ?, ?, 1, ?, ?) ON CONFLICT(run_id) DO NOTHING",
                    (
                        state.run_id, state.session_id, state.goal.goal_id, state.workflow_id, state.status,
                        _dump(snapshot), state.created_at, state.updated_at,
                    ),
                )
                if cursor.rowcount:
                    self._insert_event(
                        connection, state.run_id, "operation_started",
                        {**dict(event), "state_version": 1, "state_snapshot": snapshot},
                    )
                    return state
                row = connection.execute("SELECT * FROM operations WHERE run_id=?", (state.run_id,)).fetchone()
                existing = self._state_from_row(connection, row) if row else None
        except BaseException:
            state.state_version, state.updated_at = old_version, old_updated
            raise
        state.state_version, state.updated_at = old_version, old_updated
        if existing is None:
            raise RuntimeError(f"operation_state_corrupt:{state.run_id}")
        return existing

    def save_operation(
        self,
        state: OperationState,
        *,
        event_type: str = "state_saved",
        event: Mapping[str, Any] | None = None,
        expected_version: int | None = None,
        lease_token: LeaseToken | None = None,
    ) -> None:
        old_version, old_updated = state.state_version, state.updated_at
        try:
            with self.transaction(immediate=True) as connection:
                row = connection.execute("SELECT version FROM operations WHERE run_id=?", (state.run_id,)).fetchone()
                if row is None:
                    raise KeyError(f"operation_not_found:{state.run_id}")
                if lease_token is not None:
                    if lease_token.run_id != state.run_id:
                        raise LeaseLostError(
                            f"lease_identity_mismatch:{state.run_id}:{lease_token.run_id}:{lease_token.action_id}"
                        )
                    if not self._assert_lease(connection, lease_token):
                        raise LeaseLostError(
                            f"lease_lost:{state.run_id}:{lease_token.action_id}:{lease_token.fencing_token}"
                        )
                current_version = int(row["version"])
                expected = expected_version if expected_version is not None else state.state_version
                if expected <= 0:
                    expected = current_version
                if expected != current_version:
                    raise StateVersionConflict(f"state_version_conflict:{state.run_id}:{expected}:{current_version}")
                next_version = current_version + 1
                state.state_version = next_version
                state.updated_at = utc_now()
                snapshot = self._snapshot_payload(state, next_version)
                cursor = connection.execute(
                    "UPDATE operations SET session_id=?, goal_id=?, workflow_id=?, status=?, state_json=?, "
                    "version=?, updated_at=? WHERE run_id=? AND version=?",
                    (
                        state.session_id, state.goal.goal_id, state.workflow_id, state.status, _dump(snapshot),
                        next_version, state.updated_at, state.run_id, current_version,
                    ),
                )
                if cursor.rowcount != 1:
                    raise StateVersionConflict(f"state_version_conflict:{state.run_id}:{current_version}")
                self._insert_event(
                    connection, state.run_id, event_type,
                    {**dict(event or {}), "state_version": next_version, "state_snapshot": snapshot},
                )
        except BaseException:
            state.state_version, state.updated_at = old_version, old_updated
            raise

    def compare_and_swap_operation(
        self,
        state: OperationState,
        *,
        expected_version: int,
        lease_token: LeaseToken | None = None,
        event_type: str = "state_saved",
        event: Mapping[str, Any] | None = None,
    ) -> int:
        self.save_operation(
            state, event_type=event_type, event=event,
            expected_version=expected_version, lease_token=lease_token,
        )
        return state.state_version

    def apply_budget_delta_batch(
        self,
        run_ids: Sequence[str],
        *,
        lease_tokens: Mapping[str, LeaseToken],
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
        acknowledge_missing_usage: bool = False,
    ) -> tuple[OperationState, ...]:
        """Apply one budget delta to every run in a single SQLite transaction."""

        ordered = tuple(dict.fromkeys(str(run_id) for run_id in run_ids if str(run_id)))
        if not ordered:
            return ()
        states: list[OperationState] = []
        with self.transaction(immediate=True) as connection:
            rows: dict[str, sqlite3.Row] = {}
            for run_id in ordered:
                lease = lease_tokens.get(run_id)
                if (
                    lease is None
                    or lease.run_id != run_id
                    or lease.action_id != "__operation__"
                    or not self._assert_lease(connection, lease)
                ):
                    raise LeaseLostError(f"lease_identity_mismatch:{run_id}:__operation__")
                row = connection.execute(
                    "SELECT * FROM operations WHERE run_id=?",
                    (run_id,),
                ).fetchone()
                if row is None:
                    raise KeyError(f"operation_not_found:{run_id}")
                rows[run_id] = row

            for run_id in ordered:
                row = rows[run_id]
                state = self._state_from_row(connection, row)
                if state is None:
                    raise RuntimeError(f"operation_state_corrupt:{run_id}")
                changed = state.budget.apply_delta(
                    actions=actions,
                    tokens=tokens,
                    time_seconds=time_seconds,
                    deadline=deadline,
                    acknowledge_missing_usage=acknowledge_missing_usage,
                )
                if not changed:
                    states.append(state)
                    continue
                if state.status == "paused_budget" and not state.budget.exhaustion_reason():
                    state.status = "running"
                current_version = int(row["version"])
                next_version = current_version + 1
                state.state_version = next_version
                state.updated_at = utc_now()
                snapshot = self._snapshot_payload(state, next_version)
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
                        run_id,
                        current_version,
                    ),
                )
                if cursor.rowcount != 1:
                    raise StateVersionConflict(
                        f"state_version_conflict:{run_id}:{current_version}"
                    )
                self._insert_event(
                    connection,
                    run_id,
                    "budget_delta_applied",
                    {
                        "actions": actions,
                        "tokens": tokens,
                        "time_seconds": time_seconds,
                        "deadline": deadline,
                        "acknowledge_missing_usage": acknowledge_missing_usage,
                        "state_version": next_version,
                        "state_snapshot": snapshot,
                    },
                )
                states.append(state)
        return tuple(states)

    def load_operation(self, run_id: str) -> OperationState | None:
        with self.connection() as connection:
            row = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
            return self._state_from_row(connection, row) if row else None

    def recover_operation(self, run_id: str) -> OperationState | None:
        with self.connection() as connection:
            row = connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone()
            if row is None:
                return None
            snapshot = self._recover_snapshot(connection, run_id)
            if not isinstance(snapshot, Mapping):
                return None
            state = OperationState.from_dict(snapshot)
            state.state_version = max(1, int(row["version"]))
            return state

    def latest_operation(self, session_id: str, *, include_terminal: bool = True) -> OperationState | None:
        query = "SELECT * FROM operations WHERE session_id=?"
        if not include_terminal:
            query += " AND status NOT IN ('completed', 'failed', 'cancelled')"
        query += " ORDER BY updated_at DESC, version DESC LIMIT 1"
        with self.connection() as connection:
            row = connection.execute(query, (session_id,)).fetchone()
            return self._state_from_row(connection, row) if row else None

    def operations_for_batch(self, batch_session_id: str) -> tuple[OperationState, ...]:
        batch_id = batch_session_id.strip()
        if not batch_id:
            return ()
        escaped = batch_id.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM operations WHERE session_id LIKE ? ESCAPE '\\' ORDER BY created_at, session_id",
                (f"{escaped}:%",),
            ).fetchall()
            states = [self._state_from_row(connection, row) for row in rows]
        valid = [
            state for state in states
            if state is not None and state.goal.starting_context.get("batch_session_id") == batch_id
        ]
        valid.sort(key=lambda item: int(item.goal.starting_context.get("batch_index") or 0))
        return tuple(valid)

    def append_event(
        self,
        run_id: str,
        event_type: str,
        payload: Mapping[str, Any] | None = None,
        *,
        state: OperationState | None = None,
    ) -> None:
        event = dict(payload or {})
        if state is not None:
            event.update({"state_version": state.state_version, "state_snapshot": self._snapshot_payload(state, state.state_version)})
        with self.transaction(immediate=True) as connection:
            self._insert_event(connection, run_id, event_type, event)

    def append_event_once(
        self,
        run_id: str,
        event_type: str,
        payload: Mapping[str, Any],
        *,
        identity_field: str,
        fingerprint_field: str = "",
    ) -> bool:
        """Append an event once for an immutable identity/fingerprint pair."""
        event = dict(payload)
        identity = str(event.get(identity_field) or "")
        if not identity:
            raise ValueError(f"event_identity_required:{identity_field}")
        fingerprint = str(event.get(fingerprint_field) or "") if fingerprint_field else ""
        with self.transaction(immediate=True) as connection:
            rows = connection.execute(
                "SELECT payload_json FROM operation_events WHERE run_id=? AND event_type=?",
                (run_id, event_type),
            ).fetchall()
            for row in rows:
                existing = _load(row["payload_json"], {})
                if not isinstance(existing, Mapping) or str(existing.get(identity_field) or "") != identity:
                    continue
                if fingerprint_field and str(existing.get(fingerprint_field) or "") != fingerprint:
                    raise ImmutableRecordError(f"event_identity_conflict:{event_type}:{identity}")
                return False
            self._insert_event(connection, run_id, event_type, event)
        return True

    def events(self, run_id: str, *, after_event_id: int = 0, limit: int = 200) -> tuple[dict[str, Any], ...]:
        bounded = max(1, min(1000, int(limit)))
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT event_id, event_type, payload_json, created_at FROM operation_events "
                "WHERE run_id=? AND event_id>? ORDER BY event_id LIMIT ?",
                (run_id, max(0, int(after_event_id)), bounded),
            ).fetchall()
        return tuple(
            {
                "event_id": int(row["event_id"]), "event_type": str(row["event_type"]),
                "payload": _load(row["payload_json"], {}), "created_at": str(row["created_at"]),
            }
            for row in rows
        )

    def acquire_lease(
        self,
        run_id: str,
        action_id: str,
        owner: str,
        *,
        ttl_seconds: float = 120.0,
    ) -> LeaseToken | None:
        now = time.time()
        expires_at = now + max(1.0, float(ttl_seconds))
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT owner, expires_at FROM action_leases WHERE run_id=? AND action_id=?",
                (run_id, action_id),
            ).fetchone()
            if row is not None and float(row["expires_at"]) > now and str(row["owner"]) != owner:
                return None
            connection.execute("DELETE FROM action_leases WHERE run_id=? AND action_id=?", (run_id, action_id))
            generation_row = connection.execute(
                "SELECT generation FROM lease_generations WHERE run_id=? AND action_id=?",
                (run_id, action_id),
            ).fetchone()
            generation = (int(generation_row["generation"]) if generation_row else 0) + 1
            connection.execute(
                "INSERT INTO lease_generations(run_id, action_id, generation) VALUES(?, ?, ?) "
                "ON CONFLICT(run_id, action_id) DO UPDATE SET generation=excluded.generation",
                (run_id, action_id, generation),
            )
            connection.execute(
                "INSERT INTO action_leases(run_id, action_id, owner, fencing_token, expires_at) VALUES(?, ?, ?, ?, ?)",
                (run_id, action_id, owner, generation, expires_at),
            )
        return LeaseToken(run_id, action_id, owner, generation, expires_at)

    @staticmethod
    def _assert_lease(connection: sqlite3.Connection, token: LeaseToken) -> bool:
        row = connection.execute(
            "SELECT owner, fencing_token, expires_at FROM action_leases WHERE run_id=? AND action_id=?",
            (token.run_id, token.action_id),
        ).fetchone()
        return bool(
            row is not None
            and str(row["owner"]) == token.owner
            and int(row["fencing_token"]) == token.fencing_token
            and float(row["expires_at"]) > time.time()
        )

    def assert_lease(self, token: LeaseToken) -> bool:
        with self.connection() as connection:
            return self._assert_lease(connection, token)

    def renew_lease(self, token: LeaseToken, *, ttl_seconds: float = 120.0) -> LeaseToken | None:
        expires_at = time.time() + max(1.0, float(ttl_seconds))
        with self.transaction(immediate=True) as connection:
            if not self._assert_lease(connection, token):
                return None
            connection.execute(
                "UPDATE action_leases SET expires_at=? WHERE run_id=? AND action_id=? AND owner=? AND fencing_token=?",
                (expires_at, token.run_id, token.action_id, token.owner, token.fencing_token),
            )
        return LeaseToken(token.run_id, token.action_id, token.owner, token.fencing_token, expires_at)

    def release_lease(
        self,
        run_id: str | LeaseToken,
        action_id: str = "",
        owner: str = "",
        *,
        fencing_token: int | None = None,
    ) -> bool:
        if isinstance(run_id, LeaseToken):
            token = run_id
            run_id, action_id, owner, fencing_token = token.run_id, token.action_id, token.owner, token.fencing_token
        with self.transaction(immediate=True) as connection:
            if fencing_token is None:
                cursor = connection.execute(
                    "DELETE FROM action_leases WHERE run_id=? AND action_id=? AND owner=?",
                    (run_id, action_id, owner),
                )
            else:
                cursor = connection.execute(
                    "DELETE FROM action_leases WHERE run_id=? AND action_id=? AND owner=? AND fencing_token=?",
                    (run_id, action_id, owner, int(fencing_token)),
                )
            return cursor.rowcount == 1
