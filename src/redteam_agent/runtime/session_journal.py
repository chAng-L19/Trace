from __future__ import annotations

import hashlib
import re
import sqlite3
from dataclasses import dataclass, field
from typing import Any, Callable, Mapping

from ..core.contracts import json_mapping, required_text
from .model_common import utc_now
from .store_common import ImmutableRecordError, StoreConflictError, _dump, _load


_RAW_SOURCES = {
    "conversation_messages": ("message_id", "message_json"),
    "context_summaries": ("summary_id", "summary_json"),
    "model_requests": ("request_id", "request_json"),
    "model_responses": ("request_id", "response_json"),
    "model_observations": ("observation_id", "observation_json"),
    "exploration_records": ("record_id", "record_json"),
    "recon_digests": ("digest_id", "digest_json"),
    "tactical_attempts": ("attempt_id", "attempt_json"),
}
_BRANCH_ID = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$")


@dataclass(frozen=True, slots=True)
class ModelRequestRecord:
    request_id: str
    run_id: str
    prompt_hash: str
    provider: str
    model: str
    capabilities: Mapping[str, Any]
    request: Mapping[str, Any]
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {"request_id": self.request_id, "run_id": self.run_id, "prompt_hash": self.prompt_hash, "provider": self.provider, "model": self.model, "capabilities": dict(self.capabilities), "request": dict(self.request), "created_at": self.created_at}

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelRequestRecord":
        return cls(required_text(payload.get("request_id"), "model_request_record_id"), required_text(payload.get("run_id"), "model_request_record_run_id"), required_text(payload.get("prompt_hash"), "model_prompt_hash"), required_text(payload.get("provider"), "model_provider"), str(payload.get("model") or ""), json_mapping(payload.get("capabilities"), field="model_record.capabilities"), json_mapping(payload.get("request"), field="model_record.request"), required_text(payload.get("created_at"), "model_request_created_at"))


@dataclass(frozen=True, slots=True)
class ModelResponseRecord:
    request_id: str
    run_id: str
    status: str
    provider: str
    model: str
    response_hash: str
    claimed_response_hash: str
    usage: Mapping[str, Any]
    response: Mapping[str, Any]
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {"request_id": self.request_id, "run_id": self.run_id, "status": self.status, "provider": self.provider, "model": self.model, "response_hash": self.response_hash, "claimed_response_hash": self.claimed_response_hash, "usage": dict(self.usage), "response": dict(self.response), "created_at": self.created_at}

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelResponseRecord":
        return cls(required_text(payload.get("request_id"), "model_response_record_id"), required_text(payload.get("run_id"), "model_response_record_run_id"), required_text(payload.get("status"), "model_response_record_status"), required_text(payload.get("provider"), "model_response_provider"), str(payload.get("model") or ""), required_text(payload.get("response_hash"), "model_response_hash"), str(payload.get("claimed_response_hash") or ""), json_mapping(payload.get("usage"), field="model_response_record.usage"), json_mapping(payload.get("response"), field="model_response_record.response"), required_text(payload.get("created_at"), "model_response_created_at"))


@dataclass(frozen=True, slots=True)
class ModelObservationRecord:
    observation_id: str
    request_id: str
    run_id: str
    action_id: str
    call_id: str
    tool_name: str
    status: str
    input_hash: str
    output_hash: str
    observation: Mapping[str, Any]
    created_at: str
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"observation_id": self.observation_id, "request_id": self.request_id, "run_id": self.run_id, "action_id": self.action_id, "call_id": self.call_id, "tool_name": self.tool_name, "status": self.status, "input_hash": self.input_hash, "output_hash": self.output_hash, "observation": dict(self.observation), "created_at": self.created_at, "metadata": dict(self.metadata)}

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelObservationRecord":
        return cls(required_text(payload.get("observation_id"), "model_observation_id"), required_text(payload.get("request_id"), "model_observation_request_id"), required_text(payload.get("run_id"), "model_observation_run_id"), required_text(payload.get("action_id"), "model_observation_action_id"), required_text(payload.get("call_id"), "model_observation_call_id"), required_text(payload.get("tool_name"), "model_observation_tool"), required_text(payload.get("status"), "model_observation_status"), required_text(payload.get("input_hash"), "model_observation_input_hash"), required_text(payload.get("output_hash"), "model_observation_output_hash"), json_mapping(payload.get("observation"), field="model_observation.payload"), required_text(payload.get("created_at"), "model_observation_created_at"), json_mapping(payload.get("metadata"), field="model_observation.metadata"))


@dataclass(frozen=True, slots=True)
class JournalEntry:
    entry_id: str
    run_id: str
    session_id: str
    sequence: int
    parent_entry_id: str | None
    branch_id: str
    entry_type: str
    raw_table: str
    raw_id: str
    raw_hash: str
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "entry_id": self.entry_id,
            "run_id": self.run_id,
            "session_id": self.session_id,
            "sequence": self.sequence,
            "parent_entry_id": self.parent_entry_id,
            "branch_id": self.branch_id,
            "entry_type": self.entry_type,
            "raw_ref": {
                "table": self.raw_table,
                "id": self.raw_id,
                "sha256": self.raw_hash,
            },
            "created_at": self.created_at,
        }

    @classmethod
    def from_row(cls, row: Mapping[str, Any]) -> "JournalEntry":
        return cls(
            entry_id=str(row["entry_id"]),
            run_id=str(row["run_id"]),
            session_id=str(row["session_id"]),
            sequence=int(row["sequence"]),
            parent_entry_id=(
                str(row["parent_entry_id"]) if row["parent_entry_id"] is not None else None
            ),
            branch_id=str(row["branch_id"]),
            entry_type=str(row["entry_type"]),
            raw_table=str(row["raw_table"]),
            raw_id=str(row["raw_id"]),
            raw_hash=str(row["raw_hash"]),
            created_at=str(row["created_at"]),
        )


class JournalStoreMixin:
    @staticmethod
    def _journal_hash(raw_json: str) -> str:
        return hashlib.sha256(raw_json.encode("utf-8")).hexdigest()

    def _insert_journal_entry(
        self,
        connection: sqlite3.Connection,
        *,
        run_id: str,
        entry_type: str,
        raw_table: str,
        raw_id: str,
        raw_json: str,
        created_at: str,
    ) -> JournalEntry:
        entry_id = "journal-" + hashlib.sha256(
            f"{run_id}\0{raw_table}\0{raw_id}".encode("utf-8")
        ).hexdigest()[:32]
        raw_hash = self._journal_hash(raw_json)
        existing = connection.execute(
            "SELECT j.*, o.session_id FROM session_journal_entries j "
            "JOIN operations o ON o.run_id=j.run_id WHERE j.entry_id=?",
            (entry_id,),
        ).fetchone()
        if existing is not None:
            entry = JournalEntry.from_row(existing)
            if (
                entry.run_id,
                entry.entry_type,
                entry.raw_table,
                entry.raw_id,
                entry.raw_hash,
            ) != (run_id, entry_type, raw_table, raw_id, raw_hash):
                raise ImmutableRecordError(f"immutable_journal_entry:{entry_id}")
            return entry

        operation = connection.execute(
            "SELECT session_id, state_json FROM operations WHERE run_id=?", (run_id,)
        ).fetchone()
        if operation is None:
            raise KeyError(f"operation_not_found:{run_id}")
        state = _load(operation["state_json"], {})
        initial_branch = (
            str(state.get("branch_id") or "main") if isinstance(state, Mapping) else "main"
        )
        now = utc_now()
        connection.execute(
            "INSERT OR IGNORE INTO session_journal_state"
            "(run_id, active_branch_id, version, updated_at) VALUES(?, ?, 1, ?)",
            (run_id, initial_branch, now),
        )
        journal_state = connection.execute(
            "SELECT active_branch_id FROM session_journal_state WHERE run_id=?", (run_id,)
        ).fetchone()
        branch_id = str(journal_state["active_branch_id"])
        head = connection.execute(
            "SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id=?",
            (run_id, branch_id),
        ).fetchone()
        parent_entry_id = str(head["leaf_entry_id"]) if head is not None else None
        if parent_entry_id is None:
            latest = connection.execute(
                "SELECT entry_id FROM session_journal_entries WHERE run_id=? ORDER BY sequence DESC LIMIT 1",
                (run_id,),
            ).fetchone()
            parent_entry_id = str(latest["entry_id"]) if latest is not None else None
        cursor = connection.execute(
            "INSERT INTO session_journal_entries(entry_id, run_id, parent_entry_id, branch_id, "
            "entry_type, raw_table, raw_id, raw_hash, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                entry_id,
                run_id,
                parent_entry_id,
                branch_id,
                entry_type,
                raw_table,
                raw_id,
                raw_hash,
                created_at,
            ),
        )
        connection.execute(
            "INSERT INTO session_journal_heads(run_id, branch_id, leaf_entry_id) VALUES(?, ?, ?) "
            "ON CONFLICT(run_id, branch_id) DO UPDATE SET leaf_entry_id=excluded.leaf_entry_id",
            (run_id, branch_id, entry_id),
        )
        connection.execute(
            "UPDATE session_journal_state SET version=version+1, updated_at=? WHERE run_id=?",
            (now, run_id),
        )
        return JournalEntry(
            entry_id=entry_id,
            run_id=run_id,
            session_id=str(operation["session_id"]),
            sequence=int(cursor.lastrowid),
            parent_entry_id=parent_entry_id,
            branch_id=branch_id,
            entry_type=entry_type,
            raw_table=raw_table,
            raw_id=raw_id,
            raw_hash=raw_hash,
            created_at=created_at,
        )

    def _backfill_session_journal(self, connection: sqlite3.Connection) -> None:
        sources = (
            ("operation_events", "event_id", "", "operation_event", 0),
            ("model_requests", "request_id", "request_json", "model_request", 1),
            ("conversation_messages", "message_id", "message_json", "message", 2),
            ("model_responses", "request_id", "response_json", "model_response", 3),
            ("model_observations", "observation_id", "observation_json", "model_observation", 4),
            ("exploration_records", "record_id", "record_json", "exploration", 5),
            ("recon_digests", "digest_id", "digest_json", "recon_digest", 6),
            ("context_summaries", "summary_id", "summary_json", "compaction", 7),
            ("tactical_attempts", "attempt_id", "attempt_json", "tactical_attempt", 8),
        )
        pending: list[tuple[str, int, int, str, str, str, str, str]] = []
        for table, id_column, json_column, entry_type, priority in sources:
            columns = f"rowid AS journal_rowid, {id_column}, run_id, created_at"
            if table == "operation_events":
                columns += ", event_type, payload_json"
            else:
                columns += f", {json_column}"
            for row in connection.execute(f"SELECT {columns} FROM {table}").fetchall():
                if table == "operation_events":
                    raw_json = _dump(
                        {
                            "event_type": str(row["event_type"]),
                            "payload": _load(row["payload_json"], {}),
                        }
                    )
                    resolved_type = f"event:{row['event_type']}"
                else:
                    raw_json = str(row[json_column])
                    resolved_type = entry_type
                pending.append(
                    (
                        str(row["created_at"]),
                        priority,
                        int(row["journal_rowid"]),
                        str(row["run_id"]),
                        resolved_type,
                        table,
                        str(row[id_column]),
                        raw_json,
                    )
                )
        for created_at, _, _, run_id, entry_type, raw_table, raw_id, raw_json in sorted(pending):
            self._insert_journal_entry(
                connection,
                run_id=run_id,
                entry_type=entry_type,
                raw_table=raw_table,
                raw_id=raw_id,
                raw_json=raw_json,
                created_at=created_at,
            )


class SessionJournal:
    def __init__(self, store: Any) -> None:
        self.store = store

    def entries(self, run_id: str) -> tuple[JournalEntry, ...]:
        self._operation(run_id)
        with self.store.connection() as connection:
            rows = connection.execute(
                "SELECT j.*, o.session_id FROM session_journal_entries j "
                "JOIN operations o ON o.run_id=j.run_id WHERE j.run_id=? ORDER BY j.sequence",
                (run_id,),
            ).fetchall()
        return self._validated_entries(rows)

    @staticmethod
    def _validated_entries(rows: list[Any]) -> tuple[JournalEntry, ...]:
        entries = tuple(JournalEntry.from_row(row) for row in rows)
        by_id = {entry.entry_id: entry for entry in entries}
        for entry in entries:
            parent = by_id.get(entry.parent_entry_id) if entry.parent_entry_id else None
            if entry.parent_entry_id and (parent is None or parent.sequence >= entry.sequence):
                raise ImmutableRecordError(f"journal_parent_integrity:{entry.entry_id}")
        return entries

    def entry(self, run_id: str, entry_id: str) -> JournalEntry:
        matches = {entry.entry_id: entry for entry in self.entries(run_id)}
        if entry_id in matches:
            return matches[entry_id]
        with self.store.connection() as connection:
            foreign = connection.execute(
                "SELECT run_id FROM session_journal_entries WHERE entry_id=?", (entry_id,)
            ).fetchone()
        if foreign is not None:
            raise ValueError(f"journal_entry_run_mismatch:{entry_id}")
        raise KeyError(f"journal_entry_not_found:{entry_id}")

    def leaf_id(self, run_id: str, branch_id: str | None = None) -> str | None:
        self._operation(run_id)
        with self.store.connection() as connection:
            active = connection.execute(
                "SELECT active_branch_id FROM session_journal_state WHERE run_id=?", (run_id,)
            ).fetchone()
            selected = branch_id or (str(active["active_branch_id"]) if active else "main")
            head = connection.execute(
                "SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id=?",
                (run_id, selected),
            ).fetchone()
        return str(head["leaf_entry_id"]) if head is not None else None

    def active_branch_id(self, run_id: str) -> str:
        self._operation(run_id)
        with self.store.connection() as connection:
            state = connection.execute(
                "SELECT active_branch_id FROM session_journal_state WHERE run_id=?", (run_id,)
            ).fetchone()
        return str(state["active_branch_id"]) if state is not None else "main"

    def replay(self, run_id: str, leaf_id: str | None = None) -> tuple[JournalEntry, ...]:
        entries = self.entries(run_id)
        by_id = {entry.entry_id: entry for entry in entries}
        current_id = leaf_id if leaf_id is not None else self.leaf_id(run_id)
        if current_id is None:
            return ()
        if current_id not in by_id:
            self.entry(run_id, current_id)
        path: list[JournalEntry] = []
        seen: set[str] = set()
        while current_id is not None:
            if current_id in seen:
                raise ImmutableRecordError(f"journal_cycle:{current_id}")
            seen.add(current_id)
            current = by_id.get(current_id)
            if current is None:
                raise ImmutableRecordError(f"journal_parent_missing:{current_id}")
            path.append(current)
            current_id = current.parent_entry_id
        path.reverse()
        return tuple(path)

    def branch(
        self,
        run_id: str,
        from_entry_id: str,
        *,
        expected_leaf_id: str | None = None,
    ) -> JournalEntry:
        source = self.entry(run_id, from_entry_id)
        with self.store.transaction(immediate=True) as connection:
            state = connection.execute(
                "SELECT active_branch_id FROM session_journal_state WHERE run_id=?", (run_id,)
            ).fetchone()
            branch_id = str(state["active_branch_id"]) if state else source.branch_id
            head = connection.execute(
                "SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id=?",
                (run_id, branch_id),
            ).fetchone()
            current_leaf = str(head["leaf_entry_id"]) if head else None
            if expected_leaf_id is not None and current_leaf != expected_leaf_id:
                raise StoreConflictError(
                    f"journal_leaf_conflict:{run_id}:{expected_leaf_id}:{current_leaf or ''}"
                )
            connection.execute(
                "INSERT INTO session_journal_heads(run_id, branch_id, leaf_entry_id) VALUES(?, ?, ?) "
                "ON CONFLICT(run_id, branch_id) DO UPDATE SET leaf_entry_id=excluded.leaf_entry_id",
                (run_id, branch_id, from_entry_id),
            )
            connection.execute(
                "UPDATE session_journal_state SET version=version+1, updated_at=? WHERE run_id=?",
                (utc_now(), run_id),
            )
        return source

    def fork(self, run_id: str, from_entry_id: str, branch_id: str) -> JournalEntry:
        source = self.entry(run_id, from_entry_id)
        selected = branch_id.strip()
        if not _BRANCH_ID.fullmatch(selected):
            raise ValueError("journal_branch_id_invalid")
        with self.store.transaction(immediate=True) as connection:
            existing = connection.execute(
                "SELECT 1 FROM session_journal_heads WHERE run_id=? AND branch_id=?",
                (run_id, selected),
            ).fetchone()
            if existing is not None:
                raise StoreConflictError(f"journal_branch_exists:{run_id}:{selected}")
            connection.execute(
                "INSERT INTO session_journal_heads(run_id, branch_id, leaf_entry_id) VALUES(?, ?, ?)",
                (run_id, selected, from_entry_id),
            )
            connection.execute(
                "UPDATE session_journal_state SET active_branch_id=?, version=version+1, updated_at=? "
                "WHERE run_id=?",
                (selected, utc_now(), run_id),
            )
        return source

    def checkout(self, run_id: str, branch_id: str) -> str | None:
        self._operation(run_id)
        if not _BRANCH_ID.fullmatch(branch_id):
            raise ValueError("journal_branch_id_invalid")
        with self.store.transaction(immediate=True) as connection:
            head = connection.execute(
                "SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id=?",
                (run_id, branch_id),
            ).fetchone()
            if head is None:
                raise KeyError(f"journal_branch_not_found:{run_id}:{branch_id}")
            connection.execute(
                "UPDATE session_journal_state SET active_branch_id=?, version=version+1, updated_at=? "
                "WHERE run_id=?",
                (branch_id, utc_now(), run_id),
            )
        return str(head["leaf_entry_id"]) if head["leaf_entry_id"] is not None else None

    def tree(self, run_id: str) -> Mapping[str, Any]:
        return self._tree(self.entries(run_id))

    @staticmethod
    def _tree(entries: tuple[JournalEntry, ...]) -> Mapping[str, Any]:
        children = {entry.entry_id: [] for entry in entries}
        roots: list[str] = []
        for entry in entries:
            if entry.parent_entry_id in children:
                children[entry.parent_entry_id].append(entry.entry_id)
            else:
                roots.append(entry.entry_id)
        return {
            "roots": roots,
            "nodes": {
                entry.entry_id: {"entry": entry.to_dict(), "children": children[entry.entry_id]}
                for entry in entries
            },
        }

    def export(self, run_id: str) -> Mapping[str, Any]:
        with self.store.connection() as connection:
            connection.execute("BEGIN")
            operation = connection.execute(
                "SELECT session_id, status FROM operations WHERE run_id=?", (run_id,)
            ).fetchone()
            if operation is None:
                raise KeyError(f"operation_not_found:{run_id}")
            state = connection.execute(
                "SELECT active_branch_id, version FROM session_journal_state WHERE run_id=?",
                (run_id,),
            ).fetchone()
            heads = connection.execute(
                "SELECT branch_id, leaf_entry_id FROM session_journal_heads "
                "WHERE run_id=? ORDER BY branch_id",
                (run_id,),
            ).fetchall()
            rows = connection.execute(
                "SELECT j.*, o.session_id FROM session_journal_entries j "
                "JOIN operations o ON o.run_id=j.run_id WHERE j.run_id=? ORDER BY j.sequence",
                (run_id,),
            ).fetchall()
            entries = self._validated_entries(rows)
        return {
            "schema_version": 1,
            "session": {
                "session_id": str(operation["session_id"]),
                "run_id": run_id,
                "status": str(operation["status"]),
                "active_branch_id": str(state["active_branch_id"]) if state else "main",
                "journal_version": int(state["version"]) if state else 0,
                "branches": {
                    str(row["branch_id"]): (
                        str(row["leaf_entry_id"]) if row["leaf_entry_id"] is not None else None
                    )
                    for row in heads
                },
            },
            "tree": self._tree(entries),
        }

    def raw(self, run_id: str, entry_id: str) -> Any:
        entry = self.entry(run_id, entry_id)
        with self.store.connection() as connection:
            if entry.raw_table == "operation_events":
                row = connection.execute(
                    "SELECT run_id, event_type, payload_json FROM operation_events WHERE event_id=?",
                    (int(entry.raw_id),),
                ).fetchone()
                if row is None:
                    raise ImmutableRecordError(f"journal_raw_missing:{entry.entry_id}")
                raw = {
                    "event_type": str(row["event_type"]),
                    "payload": _load(row["payload_json"], {}),
                }
                raw_json = _dump(raw)
            else:
                source = _RAW_SOURCES.get(entry.raw_table)
                if source is None:
                    raise ImmutableRecordError(f"journal_raw_table_invalid:{entry.raw_table}")
                id_column, json_column = source
                row = connection.execute(
                    f"SELECT run_id, {json_column} FROM {entry.raw_table} WHERE {id_column}=?",
                    (entry.raw_id,),
                ).fetchone()
                if row is None:
                    raise ImmutableRecordError(f"journal_raw_missing:{entry.entry_id}")
                raw_json = str(row[json_column])
                raw = _load(raw_json, None)
            if str(row["run_id"]) != run_id:
                raise ValueError(f"journal_raw_run_mismatch:{entry.entry_id}")
            if self.store._journal_hash(raw_json) != entry.raw_hash:
                raise ImmutableRecordError(f"journal_raw_hash_mismatch:{entry.entry_id}")
            if raw is None:
                raise ImmutableRecordError(f"journal_raw_invalid:{entry.entry_id}")
            return raw

    def conversation_messages(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "conversation_messages",
            self.store.conversation_messages,
            lambda record: record.message_id,
        )

    def context_summaries(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "context_summaries",
            self.store.context_summaries,
            lambda record: record.summary_id,
        )

    def model_responses(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "model_responses",
            self.store.model_responses,
            lambda record: record.request_id,
        )

    def exploration_records(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "exploration_records",
            self.store.exploration_records,
            lambda record: record.record_id,
        )

    def recon_digests(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "recon_digests",
            self.store.recon_digests,
            lambda record: record.digest_id,
        )

    def tactical_attempts(self, run_id: str) -> tuple[Any, ...]:
        return self._active_records(
            run_id,
            "tactical_attempts",
            self.store.tactical_attempts,
            lambda record: record.attempt_id,
        )

    def _active_records(
        self,
        run_id: str,
        raw_table: str,
        loader: Callable[[str], tuple[Any, ...]],
        identity: Callable[[Any], str],
    ) -> tuple[Any, ...]:
        selected_ids = [
            entry.raw_id for entry in self.replay(run_id) if entry.raw_table == raw_table
        ]
        records = {identity(record): record for record in loader(run_id)}
        try:
            return tuple(records[record_id] for record_id in selected_ids)
        except KeyError as exc:
            raise ImmutableRecordError(f"journal_raw_missing:{raw_table}:{exc.args[0]}") from exc

    def _operation(self, run_id: str) -> Any:
        operation = self.store.load_operation(run_id)
        if operation is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return operation


__all__ = ["JournalEntry", "JournalStoreMixin", "SessionJournal"]
