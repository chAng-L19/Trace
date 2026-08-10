from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class Asset:
    KIND: ClassVar[str] = "asset"

    asset_id: str
    run_id: str
    asset_type: str
    name: str
    target: str = ""
    status: str = "verified"
    evidence_ids: tuple[str, ...] = ()
    attributes: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "asset_id": self.asset_id,
                "run_id": self.run_id,
                "asset_type": self.asset_type,
                "name": self.name,
                "target": self.target,
                "status": self.status,
                "evidence_ids": list(self.evidence_ids),
                "attributes": dict(self.attributes),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Asset":
        contract_version(payload, kind=cls.KIND)
        return cls(
            asset_id=required_text(payload.get("asset_id") or payload.get("id"), "asset_id"),
            run_id=required_text(payload.get("run_id"), "asset_run_id"),
            asset_type=required_text(payload.get("asset_type") or payload.get("type"), "asset_type"),
            name=required_text(payload.get("name") or payload.get("value"), "asset_name"),
            target=optional_text(payload.get("target")),
            status=optional_text(payload.get("status")) or "verified",
            evidence_ids=unique_strings(payload.get("evidence_ids")),
            attributes=json_mapping(payload.get("attributes"), field="asset.attributes"),
        )


@dataclass(frozen=True, slots=True)
class AttackPath:
    KIND: ClassVar[str] = "attack_path"

    path_id: str
    run_id: str
    title: str
    status: str = "candidate"
    asset_ids: tuple[str, ...] = ()
    relationship_ids: tuple[str, ...] = ()
    finding_ids: tuple[str, ...] = ()
    evidence_ids: tuple[str, ...] = ()
    impact: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "path_id": self.path_id,
                "run_id": self.run_id,
                "title": self.title,
                "status": self.status,
                "asset_ids": list(self.asset_ids),
                "relationship_ids": list(self.relationship_ids),
                "finding_ids": list(self.finding_ids),
                "evidence_ids": list(self.evidence_ids),
                "impact": self.impact,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "AttackPath":
        contract_version(payload, kind=cls.KIND)
        return cls(
            path_id=required_text(payload.get("path_id") or payload.get("id"), "attack_path_id"),
            run_id=required_text(payload.get("run_id"), "attack_path_run_id"),
            title=required_text(payload.get("title"), "attack_path_title"),
            status=optional_text(payload.get("status")) or "candidate",
            asset_ids=unique_strings(payload.get("asset_ids")),
            relationship_ids=unique_strings(payload.get("relationship_ids")),
            finding_ids=unique_strings(payload.get("finding_ids")),
            evidence_ids=unique_strings(payload.get("evidence_ids")),
            impact=optional_text(payload.get("impact")),
            metadata=json_mapping(payload.get("metadata"), field="attack_path.metadata"),
        )
