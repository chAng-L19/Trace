from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

from ..core.contracts import json_mapping, json_value, required_text, unique_strings


@dataclass(frozen=True, slots=True)
class ConversationMessageRecord:
    message_id: str
    run_id: str
    sequence: int
    role: str
    content: Any
    content_hash: str
    protected: bool
    source_type: str
    source_id: str
    created_at: str
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "message_id": self.message_id,
            "run_id": self.run_id,
            "sequence": self.sequence,
            "role": self.role,
            "content": json_value(self.content, field="conversation.content"),
            "content_hash": self.content_hash,
            "protected": self.protected,
            "source_type": self.source_type,
            "source_id": self.source_id,
            "created_at": self.created_at,
            "metadata": dict(self.metadata),
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ConversationMessageRecord":
        return cls(
            message_id=required_text(payload.get("message_id"), "conversation_message_id"),
            run_id=required_text(payload.get("run_id"), "conversation_run_id"),
            sequence=max(0, int(payload.get("sequence", 0))),
            role=required_text(payload.get("role"), "conversation_role"),
            content=json_value(payload.get("content"), field="conversation.content"),
            content_hash=required_text(payload.get("content_hash"), "conversation_content_hash"),
            protected=bool(payload.get("protected", False)),
            source_type=required_text(payload.get("source_type"), "conversation_source_type"),
            source_id=required_text(payload.get("source_id"), "conversation_source_id"),
            created_at=required_text(payload.get("created_at"), "conversation_created_at"),
            metadata=json_mapping(payload.get("metadata"), field="conversation.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ContextSummaryRecord:
    summary_id: str
    run_id: str
    source_message_ids: tuple[str, ...]
    source_hash: str
    summary: Mapping[str, Any]
    summary_hash: str
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "summary_id": self.summary_id,
            "run_id": self.run_id,
            "source_message_ids": list(self.source_message_ids),
            "source_hash": self.source_hash,
            "summary": dict(self.summary),
            "summary_hash": self.summary_hash,
            "created_at": self.created_at,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ContextSummaryRecord":
        return cls(
            summary_id=required_text(payload.get("summary_id"), "context_summary_id"),
            run_id=required_text(payload.get("run_id"), "context_summary_run_id"),
            source_message_ids=unique_strings(payload.get("source_message_ids")),
            source_hash=required_text(payload.get("source_hash"), "context_source_hash"),
            summary=json_mapping(payload.get("summary"), field="context_summary.summary"),
            summary_hash=required_text(payload.get("summary_hash"), "context_summary_hash"),
            created_at=required_text(payload.get("created_at"), "context_summary_created_at"),
        )


@dataclass(frozen=True, slots=True)
class ContextSnapshotRecord:
    snapshot_id: str
    run_id: str
    source_hash: str
    protected_hash: str
    context_hash: str
    context: Mapping[str, Any]
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "snapshot_id": self.snapshot_id,
            "run_id": self.run_id,
            "source_hash": self.source_hash,
            "protected_hash": self.protected_hash,
            "context_hash": self.context_hash,
            "context": dict(self.context),
            "created_at": self.created_at,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ContextSnapshotRecord":
        return cls(
            snapshot_id=required_text(payload.get("snapshot_id"), "context_snapshot_id"),
            run_id=required_text(payload.get("run_id"), "context_snapshot_run_id"),
            source_hash=required_text(payload.get("source_hash"), "context_snapshot_source_hash"),
            protected_hash=required_text(payload.get("protected_hash"), "context_protected_hash"),
            context_hash=required_text(payload.get("context_hash"), "context_snapshot_hash"),
            context=json_mapping(payload.get("context"), field="context_snapshot.context"),
            created_at=required_text(payload.get("created_at"), "context_snapshot_created_at"),
        )


@dataclass(frozen=True, slots=True)
class DiagnosticArtifactRecord:
    artifact_id: str
    run_id: str
    artifact_type: str
    source_id: str
    content_hash: str
    payload: Mapping[str, Any]
    created_at: str
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "artifact_id": self.artifact_id,
            "run_id": self.run_id,
            "artifact_type": self.artifact_type,
            "source_id": self.source_id,
            "content_hash": self.content_hash,
            "payload": dict(self.payload),
            "created_at": self.created_at,
            "metadata": dict(self.metadata),
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "DiagnosticArtifactRecord":
        return cls(
            artifact_id=required_text(payload.get("artifact_id"), "diagnostic_artifact_id"),
            run_id=required_text(payload.get("run_id"), "diagnostic_run_id"),
            artifact_type=required_text(payload.get("artifact_type"), "diagnostic_artifact_type"),
            source_id=required_text(payload.get("source_id"), "diagnostic_source_id"),
            content_hash=required_text(payload.get("content_hash"), "diagnostic_content_hash"),
            payload=json_mapping(payload.get("payload"), field="diagnostic.payload"),
            created_at=required_text(payload.get("created_at"), "diagnostic_created_at"),
            metadata=json_mapping(payload.get("metadata"), field="diagnostic.metadata"),
        )
