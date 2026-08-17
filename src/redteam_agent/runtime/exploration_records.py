from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

from ..core.contracts import json_mapping, required_text, unique_strings


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
        return {
            "digest_id": self.digest_id,
            "run_id": self.run_id,
            "source_record_ids": list(self.source_record_ids),
            "source_message_ids": list(self.source_message_ids),
            "source_hash": self.source_hash,
            "digest": dict(self.digest),
            "digest_hash": self.digest_hash,
            "created_at": self.created_at,
        }

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
        return {
            "attempt_id": self.attempt_id,
            "run_id": self.run_id,
            "request_id": self.request_id,
            "call_id": self.call_id,
            "lifecycle_action_id": self.lifecycle_action_id,
            "action_fingerprint": self.action_fingerprint,
            "status": self.status,
            "payload": dict(self.payload),
            "created_at": self.created_at,
            "metadata": dict(self.metadata),
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "TacticalAttemptRecord":
        return cls(
            attempt_id=required_text(payload.get("attempt_id"), "tactical_attempt_id"),
            run_id=required_text(payload.get("run_id"), "tactical_attempt_run_id"),
            request_id=required_text(payload.get("request_id"), "tactical_attempt_request_id"),
            call_id=required_text(payload.get("call_id"), "tactical_attempt_call_id"),
            lifecycle_action_id=required_text(
                payload.get("lifecycle_action_id"), "tactical_attempt_lifecycle_action_id"
            ),
            action_fingerprint=required_text(
                payload.get("action_fingerprint"), "tactical_attempt_fingerprint"
            ),
            status=required_text(payload.get("status"), "tactical_attempt_status"),
            payload=json_mapping(payload.get("payload"), field="tactical_attempt.payload"),
            created_at=required_text(payload.get("created_at"), "tactical_attempt_created_at"),
            metadata=json_mapping(payload.get("metadata"), field="tactical_attempt.metadata"),
        )
