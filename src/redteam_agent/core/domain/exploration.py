from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    bounded_float,
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


EXPLORATION_KINDS = frozenset(
    {
        "hypothesis",
        "attempt",
        "observed_miss",
        "coverage_claim",
        "verified_negative",
        "lead",
        "contradiction",
        "reopen",
    }
)
EXPLORATION_STATUSES = frozenset(
    {"proposed", "active", "observed", "suspended", "reopened", "closed", "unverified"}
)


@dataclass(frozen=True, slots=True)
class ExplorationRecord:
    """Append-only tactical ledger entry.

    This record deliberately models uncertainty and coverage separately.  A
    tool miss is never a global negative unless a later, explicitly scoped
    ``verified_negative`` record supplies coverage and reopening conditions.
    """

    KIND: ClassVar[str] = "exploration_record"

    record_id: str
    run_id: str
    hypothesis_id: str
    kind: str
    status: str
    statement: str
    target: str = ""
    parent_record_ids: tuple[str, ...] = ()
    evidence_refs: tuple[str, ...] = ()
    artifact_refs: tuple[str, ...] = ()
    tool: str = ""
    capabilities: tuple[str, ...] = ()
    action_fingerprint: str = ""
    tested_domain: Mapping[str, Any] = field(default_factory=dict)
    observations: Mapping[str, Any] = field(default_factory=dict)
    coverage: Mapping[str, Any] = field(default_factory=dict)
    confidence: float = 0.0
    uncertainty: str = ""
    reopen_triggers: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)
    created_at: str = ""

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "record_id": self.record_id,
                "run_id": self.run_id,
                "hypothesis_id": self.hypothesis_id,
                "record_kind": self.kind,
                "status": self.status,
                "statement": self.statement,
                "target": self.target,
                "parent_record_ids": list(self.parent_record_ids),
                "evidence_refs": list(self.evidence_refs),
                "artifact_refs": list(self.artifact_refs),
                "tool": self.tool,
                "capabilities": list(self.capabilities),
                "action_fingerprint": self.action_fingerprint,
                "tested_domain": dict(self.tested_domain),
                "observations": dict(self.observations),
                "coverage": dict(self.coverage),
                "confidence": self.confidence,
                "uncertainty": self.uncertainty,
                "reopen_triggers": list(self.reopen_triggers),
                "metadata": dict(self.metadata),
                "created_at": self.created_at,
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ExplorationRecord":
        declared_kind = optional_text(payload.get("kind"))
        if declared_kind not in EXPLORATION_KINDS:
            contract_version(payload, kind=cls.KIND)
        kind = optional_text(payload.get("record_kind")) or (
            declared_kind if declared_kind in EXPLORATION_KINDS else "hypothesis"
        )
        status = optional_text(payload.get("status")) or "unverified"
        if kind not in EXPLORATION_KINDS:
            raise ValueError(f"exploration_kind_invalid:{kind}")
        if status not in EXPLORATION_STATUSES:
            raise ValueError(f"exploration_status_invalid:{status}")
        return cls(
            record_id=required_text(payload.get("record_id") or payload.get("id"), "exploration_record_id"),
            run_id=required_text(payload.get("run_id"), "exploration_run_id"),
            hypothesis_id=required_text(payload.get("hypothesis_id") or "unscoped", "exploration_hypothesis_id"),
            kind=kind,
            status=status,
            statement=required_text(payload.get("statement") or "tactical observation", "exploration_statement"),
            target=optional_text(payload.get("target")),
            parent_record_ids=unique_strings(payload.get("parent_record_ids") or payload.get("parents")),
            evidence_refs=unique_strings(payload.get("evidence_refs")),
            artifact_refs=unique_strings(payload.get("artifact_refs")),
            tool=optional_text(payload.get("tool")),
            capabilities=unique_strings(payload.get("capabilities")),
            action_fingerprint=optional_text(payload.get("action_fingerprint")),
            tested_domain=json_mapping(payload.get("tested_domain"), field="exploration.tested_domain"),
            observations=json_mapping(payload.get("observations"), field="exploration.observations"),
            coverage=json_mapping(payload.get("coverage"), field="exploration.coverage"),
            confidence=bounded_float(
                payload.get("confidence", 0.0),
                default=0.0,
                minimum=0.0,
                maximum=1.0,
                field="exploration.confidence",
            ),
            uncertainty=optional_text(payload.get("uncertainty")),
            reopen_triggers=unique_strings(payload.get("reopen_triggers")),
            metadata=json_mapping(payload.get("metadata"), field="exploration.metadata"),
            created_at=optional_text(payload.get("created_at")),
        )
