from __future__ import annotations

from dataclasses import replace
from typing import Any, Mapping

from ..core import ExplorationRecord, contract_hash
from .exploration_records import ReconDigestRecord, TacticalAttemptRecord
from .store_common import ImmutableRecordError, _dump, _load


class ExplorationStoreMixin:
    def save_exploration_record(self, record: ExplorationRecord) -> ExplorationRecord:
        serialized = _dump(record.to_dict())
        record_hash = contract_hash(record.to_dict())
        with self.transaction(immediate=True) as connection:
            operation = connection.execute(
                "SELECT 1 FROM operations WHERE run_id=?", (record.run_id,)
            ).fetchone()
            if operation is None:
                raise KeyError(f"operation_not_found:{record.run_id}")
            row = connection.execute(
                "SELECT record_json FROM exploration_records WHERE record_id=?",
                (record.record_id,),
            ).fetchone()
            if row is not None:
                existing = ExplorationRecord.from_dict(_load(row["record_json"], {}))
                if existing != record:
                    raise ImmutableRecordError(f"immutable_exploration_record:{record.record_id}")
                return existing
            connection.execute(
                "INSERT INTO exploration_records(record_id, run_id, hypothesis_id, kind, status, "
                "record_hash, record_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    record.record_id,
                    record.run_id,
                    record.hypothesis_id,
                    record.kind,
                    record.status,
                    record_hash,
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="exploration",
                raw_table="exploration_records",
                raw_id=record.record_id,
                raw_json=serialized,
                created_at=record.created_at,
            )
        return record

    def exploration_records(self, run_id: str) -> tuple[ExplorationRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT record_json, record_hash FROM exploration_records "
                "WHERE run_id=? ORDER BY created_at, rowid",
                (run_id,),
            ).fetchall()
        records: list[ExplorationRecord] = []
        for row in rows:
            payload = _load(row["record_json"], {})
            record = ExplorationRecord.from_dict(payload)
            if record.run_id != run_id or contract_hash(record.to_dict()) != str(row["record_hash"]):
                raise ImmutableRecordError(f"exploration_record_integrity:{record.record_id}")
            records.append(record)
        return tuple(records)

    def save_recon_digest(self, record: ReconDigestRecord) -> ReconDigestRecord:
        if contract_hash(record.digest) != record.digest_hash:
            raise ImmutableRecordError(f"recon_digest_hash_mismatch:{record.digest_id}")
        if self._recon_source_hash(record) != record.source_hash:
            raise ImmutableRecordError(f"recon_digest_source_hash_mismatch:{record.digest_id}")
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT digest_json FROM recon_digests WHERE digest_id=?", (record.digest_id,)
            ).fetchone()
            if row is not None:
                existing = ReconDigestRecord.from_dict(_load(row["digest_json"], {}))
                if existing != record:
                    raise ImmutableRecordError(f"immutable_recon_digest:{record.digest_id}")
                return existing
            connection.execute(
                "INSERT INTO recon_digests(digest_id, run_id, source_hash, digest_hash, "
                "source_ids_json, digest_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)",
                (
                    record.digest_id,
                    record.run_id,
                    record.source_hash,
                    record.digest_hash,
                    _dump(
                        {
                            "record_ids": list(record.source_record_ids),
                            "message_ids": list(record.source_message_ids),
                        }
                    ),
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="recon_digest",
                raw_table="recon_digests",
                raw_id=record.digest_id,
                raw_json=serialized,
                created_at=record.created_at,
            )
        return record

    def recon_digests(self, run_id: str) -> tuple[ReconDigestRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT digest_json FROM recon_digests WHERE run_id=? ORDER BY created_at, rowid",
                (run_id,),
            ).fetchall()
        records: list[ReconDigestRecord] = []
        for row in rows:
            payload = _load(row["digest_json"], None)
            if not isinstance(payload, Mapping):
                continue
            record = ReconDigestRecord.from_dict(payload)
            if (
                record.run_id != run_id
                or contract_hash(record.digest) != record.digest_hash
                or self._recon_source_hash(record) != record.source_hash
            ):
                raise ImmutableRecordError(f"recon_digest_integrity:{record.digest_id}")
            records.append(record)
        return tuple(records)

    def _recon_source_hash(self, record: ReconDigestRecord) -> str:
        exploration = {
            item.record_id: item for item in self.exploration_records(record.run_id)
        }
        messages = {
            item.message_id: item for item in self.conversation_messages(record.run_id)
        }
        try:
            record_projection = [
                {
                    "record_id": record_id,
                    "record_hash": contract_hash(exploration[record_id].to_dict()),
                }
                for record_id in record.source_record_ids
            ]
            message_projection = [
                {
                    "message_id": message_id,
                    "content_hash": messages[message_id].content_hash,
                }
                for message_id in record.source_message_ids
            ]
        except KeyError as exc:
            raise ImmutableRecordError(f"recon_digest_source_missing:{exc.args[0]}") from exc
        return contract_hash({"records": record_projection, "messages": message_projection})

    def save_tactical_attempt(self, record: TacticalAttemptRecord) -> tuple[TacticalAttemptRecord, bool]:
        serialized = _dump(record.to_dict())
        attempt_hash = contract_hash(record.to_dict())
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT attempt_json, attempt_hash FROM tactical_attempts "
                "WHERE run_id=? AND request_id=? AND call_id=?",
                (record.run_id, record.request_id, record.call_id),
            ).fetchone()
            if row is not None:
                existing = TacticalAttemptRecord.from_dict(_load(row["attempt_json"], {}))
                if contract_hash(existing.to_dict()) != str(row["attempt_hash"]):
                    raise ImmutableRecordError(f"tactical_attempt_integrity:{existing.attempt_id}")
                comparable = replace(record, attempt_id=existing.attempt_id, created_at=existing.created_at)
                if existing != comparable:
                    raise ImmutableRecordError(
                        f"immutable_tactical_attempt:{record.run_id}:{record.request_id}:{record.call_id}"
                    )
                return existing, False
            connection.execute(
                "INSERT INTO tactical_attempts(attempt_id, run_id, request_id, call_id, "
                "lifecycle_action_id, action_fingerprint, status, attempt_hash, attempt_json, created_at) "
                "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    record.attempt_id,
                    record.run_id,
                    record.request_id,
                    record.call_id,
                    record.lifecycle_action_id,
                    record.action_fingerprint,
                    record.status,
                    attempt_hash,
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="tactical_attempt",
                raw_table="tactical_attempts",
                raw_id=record.attempt_id,
                raw_json=serialized,
                created_at=record.created_at,
            )
        return record, True

    def tactical_attempts(self, run_id: str) -> tuple[TacticalAttemptRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM tactical_attempts WHERE run_id=? ORDER BY created_at, rowid",
                (run_id,),
            ).fetchall()
        records: list[TacticalAttemptRecord] = []
        for row in rows:
            payload = _load(row["attempt_json"], None)
            if not isinstance(payload, Mapping):
                continue
            record = TacticalAttemptRecord.from_dict(payload)
            if (
                record.run_id != run_id
                or record.attempt_id != str(row["attempt_id"])
                or record.request_id != str(row["request_id"])
                or record.call_id != str(row["call_id"])
                or record.lifecycle_action_id != str(row["lifecycle_action_id"])
                or record.action_fingerprint != str(row["action_fingerprint"])
                or record.status != str(row["status"])
                or contract_hash(record.to_dict()) != str(row["attempt_hash"])
            ):
                raise ImmutableRecordError(f"tactical_attempt_integrity:{record.attempt_id}")
            records.append(record)
        return tuple(records)
