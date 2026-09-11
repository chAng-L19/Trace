from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any, Mapping, Sequence

from ..core import ExplorationRecord, contract_hash
from ..core.contracts import json_mapping, required_text, unique_strings
from .store_common import ImmutableRecordError, _dump, _load
from .model_common import utc_now


@dataclass(frozen=True, slots=True)
class ReconDigestRecord:
    digest_id: str
    run_id: str
    source_record_ids: tuple[str, ...]
    source_message_ids: tuple[str, ...]
    source_hash: str
    digest: Mapping[str, Any]
    digest_hash: str
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {"digest_id": self.digest_id, "run_id": self.run_id, "source_record_ids": list(self.source_record_ids), "source_message_ids": list(self.source_message_ids), "source_hash": self.source_hash, "digest": dict(self.digest), "digest_hash": self.digest_hash, "created_at": self.created_at}

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ReconDigestRecord":
        return cls(
            digest_id=required_text(payload.get("digest_id"), "recon_digest_id"),
            run_id=required_text(payload.get("run_id"), "recon_digest_run_id"),
            source_record_ids=unique_strings(payload.get("source_record_ids")),
            source_message_ids=unique_strings(payload.get("source_message_ids")),
            source_hash=required_text(payload.get("source_hash"), "recon_digest_source_hash"),
            digest=json_mapping(payload.get("digest"), field="recon_digest.digest"),
            digest_hash=required_text(payload.get("digest_hash"), "recon_digest_hash"),
            created_at=required_text(payload.get("created_at"), "recon_digest_created_at"),
        )


@dataclass(frozen=True, slots=True)
class TacticalAttemptRecord:
    attempt_id: str
    run_id: str
    request_id: str
    call_id: str
    lifecycle_action_id: str
    action_fingerprint: str
    status: str
    payload: Mapping[str, Any]
    created_at: str
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"attempt_id": self.attempt_id, "run_id": self.run_id, "request_id": self.request_id, "call_id": self.call_id, "lifecycle_action_id": self.lifecycle_action_id, "action_fingerprint": self.action_fingerprint, "status": self.status, "payload": dict(self.payload), "created_at": self.created_at, "metadata": dict(self.metadata)}

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "TacticalAttemptRecord":
        return cls(
            attempt_id=required_text(payload.get("attempt_id"), "tactical_attempt_id"),
            run_id=required_text(payload.get("run_id"), "tactical_attempt_run_id"),
            request_id=required_text(payload.get("request_id"), "tactical_attempt_request_id"),
            call_id=required_text(payload.get("call_id"), "tactical_attempt_call_id"),
            lifecycle_action_id=required_text(payload.get("lifecycle_action_id"), "tactical_attempt_lifecycle_action_id"),
            action_fingerprint=required_text(payload.get("action_fingerprint"), "tactical_attempt_fingerprint"),
            status=required_text(payload.get("status"), "tactical_attempt_status"),
            payload=json_mapping(payload.get("payload"), field="tactical_attempt.payload"),
            created_at=required_text(payload.get("created_at"), "tactical_attempt_created_at"),
            metadata=json_mapping(payload.get("metadata"), field="tactical_attempt.metadata"),
        )


class ExplorationValidationError(ValueError):
    pass


class ExplorationStoreMixin:
    def save_exploration_record(self, record: ExplorationRecord) -> ExplorationRecord:
        serialized, record_hash = _dump(record.to_dict()), contract_hash(record.to_dict())
        with self.transaction(immediate=True) as connection:
            if connection.execute("SELECT 1 FROM operations WHERE run_id=?", (record.run_id,)).fetchone() is None:
                raise KeyError(f"operation_not_found:{record.run_id}")
            row = connection.execute("SELECT record_json FROM exploration_records WHERE record_id=?", (record.record_id,)).fetchone()
            if row is not None:
                existing = ExplorationRecord.from_dict(_load(row["record_json"], {}))
                if existing != record:
                    raise ImmutableRecordError(f"immutable_exploration_record:{record.record_id}")
                return existing
            connection.execute("INSERT INTO exploration_records(record_id, run_id, hypothesis_id, kind, status, record_hash, record_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)", (record.record_id, record.run_id, record.hypothesis_id, record.kind, record.status, record_hash, serialized, record.created_at))
            self._insert_journal_entry(connection, run_id=record.run_id, entry_type="exploration", raw_table="exploration_records", raw_id=record.record_id, raw_json=serialized, created_at=record.created_at)
        return record

    def exploration_records(self, run_id: str) -> tuple[ExplorationRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute("SELECT record_json, record_hash FROM exploration_records WHERE run_id=? ORDER BY created_at, rowid", (run_id,)).fetchall()
        records = [ExplorationRecord.from_dict(_load(row["record_json"], {})) for row in rows]
        for record, row in zip(records, rows):
            if record.run_id != run_id or contract_hash(record.to_dict()) != str(row["record_hash"]):
                raise ImmutableRecordError(f"exploration_record_integrity:{record.record_id}")
        return tuple(records)

    def save_recon_digest(self, record: ReconDigestRecord) -> ReconDigestRecord:
        if contract_hash(record.digest) != record.digest_hash or self._recon_source_hash(record) != record.source_hash:
            raise ImmutableRecordError(f"recon_digest_hash_mismatch:{record.digest_id}")
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            row = connection.execute("SELECT digest_json FROM recon_digests WHERE digest_id=?", (record.digest_id,)).fetchone()
            if row is not None:
                existing = ReconDigestRecord.from_dict(_load(row["digest_json"], {}))
                if existing != record:
                    raise ImmutableRecordError(f"immutable_recon_digest:{record.digest_id}")
                return existing
            connection.execute("INSERT INTO recon_digests(digest_id, run_id, source_hash, digest_hash, source_ids_json, digest_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)", (record.digest_id, record.run_id, record.source_hash, record.digest_hash, _dump({"record_ids": list(record.source_record_ids), "message_ids": list(record.source_message_ids)}), serialized, record.created_at))
            self._insert_journal_entry(connection, run_id=record.run_id, entry_type="recon_digest", raw_table="recon_digests", raw_id=record.digest_id, raw_json=serialized, created_at=record.created_at)
        return record

    def recon_digests(self, run_id: str) -> tuple[ReconDigestRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute("SELECT digest_json FROM recon_digests WHERE run_id=? ORDER BY created_at, rowid", (run_id,)).fetchall()
        records = [ReconDigestRecord.from_dict(payload) for row in rows if isinstance((payload := _load(row["digest_json"], None)), Mapping)]
        for record in records:
            if record.run_id != run_id or contract_hash(record.digest) != record.digest_hash or self._recon_source_hash(record) != record.source_hash:
                raise ImmutableRecordError(f"recon_digest_integrity:{record.digest_id}")
        return tuple(records)

    def _recon_source_hash(self, record: ReconDigestRecord) -> str:
        exploration = {item.record_id: item for item in self.exploration_records(record.run_id)}
        messages = {item.message_id: item for item in self.conversation_messages(record.run_id)}
        try:
            records = [{"record_id": key, "record_hash": contract_hash(exploration[key].to_dict())} for key in record.source_record_ids]
            source_messages = [{"message_id": key, "content_hash": messages[key].content_hash} for key in record.source_message_ids]
        except KeyError as exc:
            raise ImmutableRecordError(f"recon_digest_source_missing:{exc.args[0]}") from exc
        return contract_hash({"records": records, "messages": source_messages})

    def save_tactical_attempt(self, record: TacticalAttemptRecord) -> tuple[TacticalAttemptRecord, bool]:
        serialized, attempt_hash = _dump(record.to_dict()), contract_hash(record.to_dict())
        with self.transaction(immediate=True) as connection:
            row = connection.execute("SELECT attempt_json, attempt_hash FROM tactical_attempts WHERE run_id=? AND request_id=? AND call_id=?", (record.run_id, record.request_id, record.call_id)).fetchone()
            if row is not None:
                existing = TacticalAttemptRecord.from_dict(_load(row["attempt_json"], {}))
                if contract_hash(existing.to_dict()) != str(row["attempt_hash"]):
                    raise ImmutableRecordError(f"tactical_attempt_integrity:{existing.attempt_id}")
                comparable = replace(record, attempt_id=existing.attempt_id, created_at=existing.created_at)
                if existing != comparable:
                    raise ImmutableRecordError(f"immutable_tactical_attempt:{record.run_id}:{record.request_id}:{record.call_id}")
                return existing, False
            connection.execute("INSERT INTO tactical_attempts(attempt_id, run_id, request_id, call_id, lifecycle_action_id, action_fingerprint, status, attempt_hash, attempt_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", (record.attempt_id, record.run_id, record.request_id, record.call_id, record.lifecycle_action_id, record.action_fingerprint, record.status, attempt_hash, serialized, record.created_at))
            self._insert_journal_entry(connection, run_id=record.run_id, entry_type="tactical_attempt", raw_table="tactical_attempts", raw_id=record.attempt_id, raw_json=serialized, created_at=record.created_at)
        return record, True

    def tactical_attempts(self, run_id: str) -> tuple[TacticalAttemptRecord, ...]:
        with self.connection() as connection:
            rows = connection.execute("SELECT * FROM tactical_attempts WHERE run_id=? ORDER BY created_at, rowid", (run_id,)).fetchall()
        records = [TacticalAttemptRecord.from_dict(payload) for row in rows if isinstance((payload := _load(row["attempt_json"], None)), Mapping)]
        for record, row in zip(records, rows):
            if record.run_id != run_id or record.attempt_id != str(row["attempt_id"]) or record.request_id != str(row["request_id"]) or record.call_id != str(row["call_id"]) or record.lifecycle_action_id != str(row["lifecycle_action_id"]) or record.action_fingerprint != str(row["action_fingerprint"]) or record.status != str(row["status"]) or contract_hash(record.to_dict()) != str(row["attempt_hash"]):
                raise ImmutableRecordError(f"tactical_attempt_integrity:{record.attempt_id}")
        return tuple(records)


class ExplorationLedger:
    """Append-only, model-authored tactical ledger with runtime invariants."""

    def __init__(
        self,
        store: Any,
        artifacts: Any,
        evidence_graph: Any,
        *,
        journal: Any | None = None,
    ) -> None:
        self.store = store
        self.artifacts = artifacts
        self.evidence_graph = evidence_graph
        self.journal = journal

    def _records(self, run_id: str) -> tuple[ExplorationRecord, ...]:
        if self.journal is not None:
            return self.journal.exploration_records(run_id)
        return self.store.exploration_records(run_id)

    def _attempts(self, run_id: str) -> tuple[Any, ...]:
        if self.journal is not None:
            return self.journal.tactical_attempts(run_id)
        return self.store.tactical_attempts(run_id)

    def record(self, value: ExplorationRecord | Mapping[str, Any]) -> ExplorationRecord:
        record = value if isinstance(value, ExplorationRecord) else ExplorationRecord.from_dict(value)
        state = self.store.load_operation(record.run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{record.run_id}")
        target = record.target or (state.goal.targets[0] if state.goal.targets else "")
        if target and state.goal.targets and target not in state.goal.targets:
            raise ExplorationValidationError(f"exploration_target_out_of_scope:{target}")
        records = self._records(record.run_id)
        by_id = {item.record_id: item for item in records}
        for parent_id in record.parent_record_ids:
            parent = by_id.get(parent_id)
            if parent is None:
                raise ExplorationValidationError(f"exploration_parent_missing:{parent_id}")
            if parent.run_id != record.run_id:
                raise ExplorationValidationError(f"exploration_parent_scope_mismatch:{parent_id}")
        evidence_by_id = {
            item.evidence_id: item for item in self.evidence_graph.list(record.run_id)
        }
        for evidence_id in record.evidence_refs:
            if evidence_id not in evidence_by_id:
                raise ExplorationValidationError(f"exploration_evidence_missing:{evidence_id}")
        for artifact_id in record.artifact_refs:
            try:
                self.artifacts.verify(artifact_id, run_id=record.run_id)
            except (KeyError, ValueError) as exc:
                raise ExplorationValidationError(
                    f"exploration_artifact_invalid:{artifact_id}"
                ) from exc
        self._validate_negative_semantics(record)
        resolved = replace(
            record,
            target=target,
            created_at=record.created_at or utc_now(),
        )
        saved = self.store.save_exploration_record(resolved)
        self._auto_reopen(saved)
        return saved

    @staticmethod
    def _validate_negative_semantics(record: ExplorationRecord) -> None:
        if record.kind == "observed_miss":
            if record.status == "closed":
                raise ExplorationValidationError("observed_miss_cannot_close_hypothesis")
            if (
                not record.tested_domain
                or not record.observations
                or not record.coverage
                or not record.uncertainty
                or not record.reopen_triggers
            ):
                raise ExplorationValidationError(
                    "observed_miss_requires_domain_observations_coverage_uncertainty_and_reopen"
                )
        if record.kind == "coverage_claim":
            if not record.tested_domain or not record.coverage:
                raise ExplorationValidationError("coverage_claim_requires_domain_and_coverage")
            if record.status == "closed":
                raise ExplorationValidationError("coverage_claim_cannot_close_hypothesis")
        if record.kind == "verified_negative":
            if record.status != "closed":
                raise ExplorationValidationError("verified_negative_requires_closed_status")
            if not record.tested_domain or not record.coverage or not record.observations:
                raise ExplorationValidationError(
                    "verified_negative_requires_domain_coverage_and_observations"
                )
            if not record.evidence_refs and not record.artifact_refs:
                raise ExplorationValidationError("verified_negative_requires_source_refs")
            if not record.reopen_triggers:
                raise ExplorationValidationError("verified_negative_requires_reopen_triggers")
            if record.confidence <= 0.0:
                raise ExplorationValidationError("verified_negative_requires_confidence")

    def record_model_update(
        self,
        run_id: str,
        request_id: str,
        update: Mapping[str, Any],
    ) -> tuple[ExplorationRecord, ...]:
        raw_records = update.get("records")
        if not isinstance(raw_records, Sequence) or isinstance(raw_records, (str, bytes)):
            return ()
        saved: list[ExplorationRecord] = []
        existing_by_id = {
            item.record_id: item for item in self._records(run_id)
        }
        for index, raw in enumerate(raw_records):
            if not isinstance(raw, Mapping):
                continue
            payload = dict(raw)
            payload["run_id"] = run_id
            payload.setdefault("hypothesis_id", f"hypothesis-{contract_hash(payload)[:16]}")
            payload.setdefault(
                "record_id",
                "exploration-"
                + contract_hash(
                    {
                        "run_id": run_id,
                        "request_id": request_id,
                        "index": index,
                        "payload": payload,
                    }
                )[:32],
            )
            existing = existing_by_id.get(str(payload["record_id"]))
            if existing is not None:
                saved.append(existing)
                continue
            payload.setdefault("created_at", utc_now())
            metadata = json_mapping(payload.get("metadata"), field="exploration.metadata")
            payload["metadata"] = {**metadata, "source": "model_structured_output", "request_id": request_id}
            saved.append(self.record(ExplorationRecord.from_dict(payload)))
        return tuple(saved)

    def current(self, run_id: str) -> tuple[ExplorationRecord, ...]:
        latest: dict[str, ExplorationRecord] = {}
        order: list[str] = []
        for record in self._records(run_id):
            if record.hypothesis_id not in latest:
                order.append(record.hypothesis_id)
            latest[record.hypothesis_id] = record
        return tuple(latest[item] for item in order)

    def projection(self, run_id: str, *, limit: int = 32) -> Mapping[str, Any]:
        current = self.current(run_id)
        records = self._records(run_id)
        attempts = self._attempts(run_id)
        repeated = self.repeated_actions(run_id)
        selected = current[-max(1, int(limit)) :]
        return {
            "authority": "navigation_only_not_evidence",
            "record_count": len(records),
            "active": [self._project_record(item) for item in selected if item.status in {"active", "reopened", "proposed"}],
            "suspended": [self._project_record(item) for item in selected if item.status == "suspended"],
            "unresolved_contradictions": [
                self._project_record(item) for item in selected if item.kind == "contradiction" and item.status != "closed"
            ],
            "recent_attempts": [
                {
                    "attempt_id": item.attempt_id,
                    "lifecycle_action_id": item.lifecycle_action_id,
                    "action_fingerprint": item.action_fingerprint,
                    "status": item.status,
                    "tool": str(item.payload.get("tool") or ""),
                    "raw_artifact_ref": str(item.payload.get("raw_artifact_ref") or ""),
                }
                for item in attempts[-8:]
            ],
            "repeated_action_signals": repeated,
        }

    @staticmethod
    def _project_record(record: ExplorationRecord) -> Mapping[str, Any]:
        return {
            "record_id": record.record_id,
            "hypothesis_id": record.hypothesis_id,
            "kind": record.kind,
            "status": record.status,
            "statement": record.statement,
            "target": record.target,
            "evidence_refs": list(record.evidence_refs),
            "artifact_refs": list(record.artifact_refs),
            "capabilities": list(record.capabilities),
            "coverage": dict(record.coverage),
            "uncertainty": record.uncertainty,
            "reopen_triggers": list(record.reopen_triggers),
        }

    def repeated_actions(self, run_id: str) -> list[Mapping[str, Any]]:
        counts: dict[str, int] = {}
        last: dict[str, str] = {}
        for attempt in self._attempts(run_id):
            counts[attempt.action_fingerprint] = counts.get(attempt.action_fingerprint, 0) + 1
            last[attempt.action_fingerprint] = attempt.attempt_id
        return [
            {
                "action_fingerprint": fingerprint,
                "count": count,
                "last_attempt_id": last[fingerprint],
                "instruction": "review_for_stagnation_without_closing_the_hypothesis",
            }
            for fingerprint, count in sorted(counts.items())
            if count > 1
        ]

    def _auto_reopen(self, source: ExplorationRecord) -> tuple[ExplorationRecord, ...]:
        if source.kind == "reopen":
            return ()
        signals = {
            *source.capabilities,
            *(f"capability:{item}" for item in source.capabilities),
            *source.evidence_refs,
            *(f"evidence:{item}" for item in source.evidence_refs),
            *source.artifact_refs,
            *(f"artifact:{item}" for item in source.artifact_refs),
        }
        metadata_signals = source.metadata.get("signals")
        if isinstance(metadata_signals, Sequence) and not isinstance(metadata_signals, (str, bytes)):
            signals.update(str(item) for item in metadata_signals if str(item))
        if not signals:
            return ()
        reopened: list[ExplorationRecord] = []
        for current in self.current(source.run_id):
            if current.hypothesis_id == source.hypothesis_id:
                continue
            if current.status not in {"suspended", "closed"}:
                continue
            matched = sorted(set(current.reopen_triggers) & signals)
            if not matched:
                continue
            record_id = "exploration-" + contract_hash(
                {
                    "kind": "reopen",
                    "source": source.record_id,
                    "hypothesis": current.hypothesis_id,
                    "matched": matched,
                }
            )[:32]
            reopen = ExplorationRecord(
                record_id=record_id,
                run_id=source.run_id,
                hypothesis_id=current.hypothesis_id,
                kind="reopen",
                status="reopened",
                statement=f"Reopened after new signal: {', '.join(matched)}",
                target=current.target,
                parent_record_ids=(current.record_id, source.record_id),
                evidence_refs=source.evidence_refs,
                artifact_refs=source.artifact_refs,
                capabilities=source.capabilities,
                reopen_triggers=current.reopen_triggers,
                metadata={"matched_triggers": matched, "automatic": True},
                created_at=source.created_at,
            )
            reopened.append(self.store.save_exploration_record(reopen))
        return tuple(reopened)

    def build_recon_digest(
        self,
        run_id: str,
        *,
        source_message_ids: Sequence[str] = (),
    ) -> ReconDigestRecord:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        records = self._records(run_id)
        current = self.current(run_id)
        attempts = self._attempts(run_id)
        evidence = self.evidence_graph.list(run_id)
        artifact_refs = list(
            dict.fromkeys(
                artifact_id
                for record in records
                for artifact_id in record.artifact_refs
            )
        )
        digest = {
            "kind": "recon_digest_projection",
            "authority": "navigation_only_sources_remain_authoritative",
            "targets": list(state.goal.targets),
            "run_status": state.status,
            "lifecycle_action_id": state.current_action_id,
            "raw_artifact_refs": artifact_refs,
            "attempted_actions": [
                {
                    "attempt_id": item.attempt_id,
                    "tool": str(item.payload.get("tool") or ""),
                    "action_fingerprint": item.action_fingerprint,
                    "status": item.status,
                    "raw_artifact_ref": str(item.payload.get("raw_artifact_ref") or ""),
                }
                for item in attempts[-32:]
            ],
            "confirmed_observations": [
                {
                    "evidence_id": item.evidence_id,
                    "artifact_type": item.artifact_type,
                    "target": item.target,
                }
                for item in evidence
                if item.verified
            ],
            "unverified_hypotheses": [
                self._project_record(item)
                for item in current
                if item.kind in {"hypothesis", "lead", "observed_miss", "reopen"}
                and item.status != "closed"
            ],
            "unresolved_contradictions": [
                self._project_record(item)
                for item in current
                if item.kind == "contradiction" and item.status != "closed"
            ],
            "reopen_triggers": {
                item.hypothesis_id: list(item.reopen_triggers)
                for item in current
                if item.reopen_triggers
            },
            "repeated_action_signals": self.repeated_actions(run_id),
        }
        selected_message_ids = tuple(
            dict.fromkeys(str(item) for item in source_message_ids if str(item))
        )
        messages_by_id = {
            item.message_id: item
            for item in (
                self.journal.conversation_messages(run_id)
                if self.journal is not None
                else self.store.conversation_messages(run_id)
            )
        }
        missing_messages = [item for item in selected_message_ids if item not in messages_by_id]
        if missing_messages:
            raise ExplorationValidationError(
                f"recon_digest_message_missing:{missing_messages[0]}"
            )
        source_projection = {
            "records": [
                {"record_id": item.record_id, "record_hash": contract_hash(item.to_dict())}
                for item in records
            ],
            "messages": [
                {
                    "message_id": message_id,
                    "content_hash": messages_by_id[message_id].content_hash,
                }
                for message_id in selected_message_ids
            ],
        }
        source_hash = contract_hash(source_projection)
        digest_hash = contract_hash(digest)
        if self.journal is not None:
            for existing in self.journal.recon_digests(run_id):
                if existing.source_hash == source_hash:
                    return existing
        digest_identity = {
            "source_hash": source_hash,
            "parent_entry_id": self.journal.leaf_id(run_id) if self.journal is not None else None,
            "branch_id": self.journal.active_branch_id(run_id) if self.journal is not None else "",
        }
        record = ReconDigestRecord(
            digest_id="recon-digest-" + contract_hash(digest_identity)[:32],
            run_id=run_id,
            source_record_ids=tuple(item.record_id for item in records),
            source_message_ids=selected_message_ids,
            source_hash=source_hash,
            digest=digest,
            digest_hash=digest_hash,
            created_at=utc_now(),
        )
        return self.store.save_recon_digest(record)
