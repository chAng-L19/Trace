from __future__ import annotations

from dataclasses import replace
from typing import Any, Mapping, Sequence

from ..core import ExplorationRecord, contract_hash
from ..core.contracts import json_mapping
from .exploration_records import ReconDigestRecord
from .model_common import utc_now


class ExplorationValidationError(ValueError):
    pass


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
