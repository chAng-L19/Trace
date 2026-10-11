from __future__ import annotations

from dataclasses import replace
from typing import Any, Mapping, Sequence

from ..core import contract_hash
from .conversation_records import (
    ContextSnapshotRecord,
    ContextSummaryRecord,
    ConversationMessageRecord,
    DiagnosticArtifactRecord,
)
from .store_common import ImmutableRecordError, _dump, _load
from .security import redact_sensitive
from .session_journal import SessionJournal


class ConversationStoreMixin:
    def append_conversation_message(
        self,
        record: ConversationMessageRecord,
    ) -> ConversationMessageRecord:
        if contract_hash(record.content) != record.content_hash:
            raise ImmutableRecordError(f"conversation_content_hash_mismatch:{record.message_id}")
        # Hash the same projection that _dump persists, after validating the input.
        content = redact_sensitive(record.content)
        record = replace(record, content=content, content_hash=contract_hash(content),
                         metadata=redact_sensitive(record.metadata))
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT message_json FROM conversation_messages WHERE message_id=?",
                (record.message_id,),
            ).fetchone()
            if row is not None:
                existing = ConversationMessageRecord.from_dict(_load(row["message_json"], {}))
                if existing != replace(
                    record,
                    sequence=existing.sequence,
                    created_at=existing.created_at,
                ):
                    raise ImmutableRecordError(f"immutable_conversation_message:{record.message_id}")
                return existing
            sequence = int(
                connection.execute(
                    "SELECT COALESCE(MAX(sequence), 0) + 1 FROM conversation_messages WHERE run_id=?",
                    (record.run_id,),
                ).fetchone()[0]
            )
            assigned = replace(record, sequence=sequence)
            serialized = _dump(assigned.to_dict())
            connection.execute(
                "INSERT INTO conversation_messages(message_id, run_id, sequence, role, content_hash, "
                "protected, source_type, source_id, message_json, created_at) "
                "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    assigned.message_id,
                    assigned.run_id,
                    assigned.sequence,
                    assigned.role,
                    assigned.content_hash,
                    int(assigned.protected),
                    assigned.source_type,
                    assigned.source_id,
                    serialized,
                    assigned.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=assigned.run_id,
                entry_type="message",
                raw_table="conversation_messages",
                raw_id=assigned.message_id,
                raw_json=serialized,
                created_at=assigned.created_at,
            )
            return assigned

    def conversation_messages(self, run_id: str) -> tuple[ConversationMessageRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM conversation_messages WHERE run_id=? ORDER BY sequence",
                (run_id,),
            ).fetchall()
        return self._conversation_messages_from_rows(rows, run_id)

    @staticmethod
    def _conversation_messages_from_rows(rows: Any, run_id: str) -> tuple[ConversationMessageRecord, ...]:
        records = []
        for row in rows:
            record = ConversationMessageRecord.from_dict(_load(row["message_json"], {}))
            if (
                record.run_id != run_id
                or record.sequence != int(row["sequence"])
                or record.role != str(row["role"])
                or record.content_hash != str(row["content_hash"])
                or record.protected != bool(row["protected"])
                or contract_hash(record.content) != record.content_hash
            ):
                raise ImmutableRecordError(f"conversation_record_integrity:{record.message_id}")
            records.append(record)
        return tuple(records)

    def active_conversation_messages(self, run_id: str, *, limit: int | None = None, offset: int = 0, latest: bool = False) -> tuple[ConversationMessageRecord, ...]:
        with self.connection() as connection:
            connection.execute("BEGIN")
            if connection.execute("SELECT 1 FROM operations WHERE run_id=?", (run_id,)).fetchone() is None:
                raise KeyError(f"operation_not_found:{run_id}")
            active_query = """WITH RECURSIVE active(entry_id,parent_entry_id,raw_table,raw_id,sequence) AS (
                    SELECT j.entry_id,j.parent_entry_id,j.raw_table,j.raw_id,j.sequence
                    FROM session_journal_entries j JOIN session_journal_heads h ON h.leaf_entry_id=j.entry_id
                    JOIN session_journal_state s ON s.run_id=h.run_id AND s.active_branch_id=h.branch_id
                    WHERE h.run_id=? AND j.run_id=h.run_id
                    UNION
                    SELECT j.entry_id,j.parent_entry_id,j.raw_table,j.raw_id,j.sequence
                    FROM session_journal_entries j JOIN active a ON j.entry_id=a.parent_entry_id WHERE j.run_id=?
                ) """
            journal_rows = connection.execute(active_query +
                "SELECT j.*,o.session_id FROM active a JOIN session_journal_entries j ON j.entry_id=a.entry_id "
                "JOIN operations o ON o.run_id=j.run_id ORDER BY j.sequence", (run_id, run_id)).fetchall()
            entries = SessionJournal._validated_entries(journal_rows)
            query = active_query + "SELECT m.* FROM active a JOIN conversation_messages m ON m.message_id=a.raw_id " \
                "WHERE a.raw_table='conversation_messages' AND m.run_id=? ORDER BY a.sequence"
            if latest:
                query += " DESC"
            arguments: tuple[Any, ...] = (run_id, run_id, run_id)
            if limit is not None:
                query += " LIMIT ? OFFSET ?"
                arguments += (int(limit), int(offset))
            rows = connection.execute(query, arguments).fetchall()
            expected_ids = [entry.raw_id for entry in entries if entry.raw_table == "conversation_messages"]
            if latest:
                expected_ids.reverse()
            if limit is not None:
                expected_ids = expected_ids[offset:offset + limit]
            found_ids = {str(row["message_id"]) for row in rows}
            for message_id in expected_ids:
                if message_id not in found_ids:
                    raise ImmutableRecordError(f"journal_raw_missing:conversation_messages:{message_id}")
        return self._conversation_messages_from_rows(rows[::-1] if latest else rows, run_id)

    def conversation_summaries(self, *, limit: int = 100, offset: int = 0,
                               statuses: Sequence[str] = (), search: str = "") -> tuple[dict[str, Any], ...]:
        where, arguments = self._operation_filter(statuses, search)
        with self.connection() as connection:
            connection.execute("BEGIN")
            rows = connection.execute(
                "SELECT o.*,s.active_branch_id,s.version AS journal_version FROM operations o "
                "LEFT JOIN session_journal_state s ON s.run_id=o.run_id" + where +
                " ORDER BY o.updated_at DESC,o.run_id DESC LIMIT ? OFFSET ?",
                (*arguments, max(1, min(1001, int(limit))), max(0, int(offset))),
            ).fetchall()
            if not rows:
                return ()
            run_ids = tuple(str(row["run_id"]) for row in rows)
            placeholders = ",".join("?" for _ in run_ids)
            heads = connection.execute(
                f"SELECT run_id,branch_id,leaf_entry_id FROM session_journal_heads WHERE run_id IN ({placeholders}) ORDER BY branch_id",
                run_ids,
            ).fetchall()
            counts = dict(connection.execute(
                f"""WITH RECURSIVE active(run_id,entry_id,parent_entry_id,raw_table) AS (
                    SELECT j.run_id,j.entry_id,j.parent_entry_id,j.raw_table
                    FROM session_journal_entries j JOIN session_journal_heads h ON h.leaf_entry_id=j.entry_id
                    JOIN session_journal_state s ON s.run_id=h.run_id AND s.active_branch_id=h.branch_id
                    WHERE h.run_id IN ({placeholders}) AND j.run_id=h.run_id
                    UNION
                    SELECT j.run_id,j.entry_id,j.parent_entry_id,j.raw_table
                    FROM session_journal_entries j JOIN active a ON j.entry_id=a.parent_entry_id AND j.run_id=a.run_id
                ) SELECT run_id,COUNT(*) FROM active WHERE raw_table='conversation_messages' GROUP BY run_id""",
                run_ids,
            ).fetchall())
            branches: dict[str, dict[str, Any]] = {run_id: {} for run_id in run_ids}
            for head in heads:
                branches[str(head["run_id"])][str(head["branch_id"])] = head["leaf_entry_id"]
            result = []
            for row in rows:
                state = self._state_from_row(connection, row)
                if state is None:
                    continue
                run_id = str(row["run_id"])
                result.append({"state": state, "message_count": counts.get(run_id, 0), "session": {
                    "session_id": str(row["session_id"]), "run_id": run_id, "status": str(row["status"]),
                    "active_branch_id": str(row["active_branch_id"] or "main"),
                    "journal_version": int(row["journal_version"] or 0), "branches": branches[run_id],
                }})
        return tuple(result)

    def save_context_summary(self, record: ContextSummaryRecord) -> None:
        messages = {item.message_id: item for item in self.conversation_messages(record.run_id)}
        try:
            sources = tuple(messages[item] for item in record.source_message_ids)
        except KeyError as exc:
            raise ImmutableRecordError(f"context_summary_source_missing:{exc.args[0]}") from exc
        source_hash = self.context_source_hash(sources)
        if source_hash != record.source_hash or contract_hash(record.summary) != record.summary_hash:
            raise ImmutableRecordError(f"context_summary_hash_mismatch:{record.summary_id}")
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection,
                table="context_summaries",
                key_column="summary_id",
                key=record.summary_id,
                json_column="summary_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO context_summaries(summary_id, run_id, source_hash, summary_hash, "
                    "source_ids_json, summary_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.summary_id,
                    record.run_id,
                    record.source_hash,
                    record.summary_hash,
                    _dump(list(record.source_message_ids)),
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="compaction",
                raw_table="context_summaries",
                raw_id=record.summary_id,
                raw_json=serialized,
                created_at=record.created_at,
            )

    def context_summaries(self, run_id: str) -> tuple[ContextSummaryRecord, ...]:
        return self._conversation_records(
            "SELECT summary_json FROM context_summaries WHERE run_id=? ORDER BY rowid",
            run_id,
            ContextSummaryRecord,
        )

    def save_context_snapshot(self, record: ContextSnapshotRecord) -> None:
        if contract_hash(record.context) != record.context_hash:
            raise ImmutableRecordError(f"context_snapshot_hash_mismatch:{record.snapshot_id}")
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection,
                table="context_snapshots",
                key_column="snapshot_id",
                key=record.snapshot_id,
                json_column="context_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO context_snapshots(snapshot_id, run_id, source_hash, protected_hash, "
                    "context_hash, context_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.snapshot_id,
                    record.run_id,
                    record.source_hash,
                    record.protected_hash,
                    record.context_hash,
                    serialized,
                    record.created_at,
                ),
            )

    def context_snapshots(self, run_id: str) -> tuple[ContextSnapshotRecord, ...]:
        return self._conversation_records(
            "SELECT context_json FROM context_snapshots WHERE run_id=? ORDER BY rowid",
            run_id,
            ContextSnapshotRecord,
        )

    def save_diagnostic_artifact(self, record: DiagnosticArtifactRecord) -> None:
        if contract_hash(record.payload) != record.content_hash:
            raise ImmutableRecordError(f"diagnostic_content_hash_mismatch:{record.artifact_id}")
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection,
                table="diagnostic_artifacts",
                key_column="artifact_id",
                key=record.artifact_id,
                json_column="artifact_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO diagnostic_artifacts(artifact_id, run_id, artifact_type, source_id, "
                    "content_hash, artifact_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.artifact_id,
                    record.run_id,
                    record.artifact_type,
                    record.source_id,
                    record.content_hash,
                    serialized,
                    record.created_at,
                ),
            )

    def diagnostic_artifacts(self, run_id: str) -> tuple[DiagnosticArtifactRecord, ...]:
        return self._conversation_records(
            "SELECT artifact_json FROM diagnostic_artifacts WHERE run_id=? ORDER BY rowid",
            run_id,
            DiagnosticArtifactRecord,
        )

    @staticmethod
    def context_source_hash(messages: tuple[ConversationMessageRecord, ...]) -> str:
        return contract_hash(
            [{"message_id": item.message_id, "content_hash": item.content_hash} for item in messages]
        )

    def _conversation_records(self, query: str, run_id: str, kind: Any) -> tuple[Any, ...]:
        with self.connection() as connection:
            rows = connection.execute(query, (run_id,)).fetchall()
        return tuple(
            kind.from_dict(payload)
            for row in rows
            if isinstance((payload := _load(row[0], None)), Mapping)
        )
