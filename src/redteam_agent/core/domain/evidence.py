from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    contract_version,
    json_mapping,
    json_value,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class EvidenceProvenance:
    KIND: ClassVar[str] = "evidence_provenance"

    run_id: str
    branch_id: str
    action_id: str
    attempt_id: str
    tool: str
    tool_version: str
    input_hash: str
    output_hash: str
    target: str
    plan_revision: int = 1
    parent_ids: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "run_id": self.run_id,
                "branch_id": self.branch_id,
                "action_id": self.action_id,
                "attempt_id": self.attempt_id,
                "tool": self.tool,
                "tool_version": self.tool_version,
                "input_hash": self.input_hash,
                "output_hash": self.output_hash,
                "target": self.target,
                "plan_revision": self.plan_revision,
                "parent_ids": list(self.parent_ids),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "EvidenceProvenance":
        contract_version(payload, kind=cls.KIND)
        try:
            revision = max(1, int(payload.get("plan_revision", 1)))
        except (TypeError, ValueError, OverflowError):
            revision = 1
        return cls(
            run_id=required_text(payload.get("run_id"), "provenance_run_id"),
            branch_id=optional_text(payload.get("branch_id")) or "main",
            action_id=required_text(payload.get("action_id"), "provenance_action_id"),
            attempt_id=required_text(payload.get("attempt_id"), "provenance_attempt_id"),
            tool=required_text(payload.get("tool"), "provenance_tool"),
            tool_version=optional_text(payload.get("tool_version")) or "unknown",
            input_hash=optional_text(payload.get("input_hash")),
            output_hash=optional_text(payload.get("output_hash")),
            target=optional_text(payload.get("target")),
            plan_revision=revision,
            parent_ids=unique_strings(payload.get("parent_ids")),
            metadata=json_mapping(payload.get("metadata"), field="evidence_provenance.metadata"),
        )


@dataclass(frozen=True, slots=True)
class Evidence:
    KIND: ClassVar[str] = "evidence"

    evidence_id: str
    run_id: str
    artifact_type: str
    target: str
    action_id: str
    tool: str
    payload: Any
    content_hash: str
    parent_ids: tuple[str, ...] = ()
    verifier: str = ""
    confidence: float = 0.0
    verified: bool = False
    trust: str = "unverified"
    provenance: EvidenceProvenance | None = None
    created_at: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "evidence_id": self.evidence_id,
                "run_id": self.run_id,
                "artifact_type": self.artifact_type,
                "target": self.target,
                "action_id": self.action_id,
                "tool": self.tool,
                "payload": json_value(self.payload, field="evidence.payload"),
                "content_hash": self.content_hash,
                "parent_ids": list(self.parent_ids),
                "verifier": self.verifier,
                "confidence": self.confidence,
                "verified": self.verified,
                "trust": self.trust,
                "provenance": self.provenance.to_dict() if self.provenance else None,
                "created_at": self.created_at,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Evidence":
        contract_version(payload, kind=cls.KIND)
        raw_provenance = payload.get("provenance")
        provenance = EvidenceProvenance.from_dict(raw_provenance) if isinstance(raw_provenance, Mapping) else None
        try:
            confidence = float(payload.get("confidence", 0.0))
        except (TypeError, ValueError, OverflowError):
            confidence = 0.0
        return cls(
            evidence_id=required_text(payload.get("evidence_id") or payload.get("id"), "evidence_id"),
            run_id=required_text(payload.get("run_id"), "evidence_run_id"),
            artifact_type=required_text(payload.get("artifact_type"), "artifact_type"),
            target=optional_text(payload.get("target")),
            action_id=required_text(payload.get("action_id"), "evidence_action_id"),
            tool=required_text(payload.get("tool"), "evidence_tool"),
            payload=json_value(payload.get("payload"), field="evidence.payload"),
            content_hash=required_text(payload.get("content_hash"), "evidence_content_hash"),
            parent_ids=unique_strings(payload.get("parent_ids")),
            verifier=optional_text(payload.get("verifier")),
            confidence=max(0.0, min(1.0, confidence)),
            verified=bool(payload.get("verified", False)),
            trust=optional_text(payload.get("trust")) or "unverified",
            provenance=provenance,
            created_at=optional_text(payload.get("created_at")),
            metadata=json_mapping(payload.get("metadata"), field="evidence.metadata"),
        )


@dataclass(frozen=True, slots=True)
class Finding:
    KIND: ClassVar[str] = "finding"

    finding_id: str
    run_id: str
    title: str
    severity: str
    status: str = "candidate"
    description: str = ""
    target: str = ""
    asset_ids: tuple[str, ...] = ()
    reproduction_evidence_ids: tuple[str, ...] = ()
    impact_evidence_ids: tuple[str, ...] = ()
    negative_control_evidence_ids: tuple[str, ...] = ()
    cleanup_evidence_ids: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "finding_id": self.finding_id,
                "run_id": self.run_id,
                "title": self.title,
                "severity": self.severity,
                "status": self.status,
                "description": self.description,
                "target": self.target,
                "asset_ids": list(self.asset_ids),
                "reproduction_evidence_ids": list(self.reproduction_evidence_ids),
                "impact_evidence_ids": list(self.impact_evidence_ids),
                "negative_control_evidence_ids": list(self.negative_control_evidence_ids),
                "cleanup_evidence_ids": list(self.cleanup_evidence_ids),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Finding":
        contract_version(payload, kind=cls.KIND)
        return cls(
            finding_id=required_text(payload.get("finding_id") or payload.get("id"), "finding_id"),
            run_id=required_text(payload.get("run_id"), "finding_run_id"),
            title=required_text(payload.get("title"), "finding_title"),
            severity=optional_text(payload.get("severity")) or "unknown",
            status=optional_text(payload.get("status")) or "candidate",
            description=optional_text(payload.get("description")),
            target=optional_text(payload.get("target")),
            asset_ids=unique_strings(payload.get("asset_ids")),
            reproduction_evidence_ids=unique_strings(payload.get("reproduction_evidence_ids")),
            impact_evidence_ids=unique_strings(payload.get("impact_evidence_ids")),
            negative_control_evidence_ids=unique_strings(payload.get("negative_control_evidence_ids")),
            cleanup_evidence_ids=unique_strings(payload.get("cleanup_evidence_ids")),
            metadata=json_mapping(payload.get("metadata"), field="finding.metadata"),
        )
