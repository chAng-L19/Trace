from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    bounded_int,
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class SearchNode:
    KIND: ClassVar[str] = "search_node"

    node_id: str
    run_id: str
    intent_id: str
    statement: str
    node_type: str = "hypothesis"
    status: str = "proposed"
    priority: int = 50
    target: str = ""
    parent_node_id: str = ""
    required_capabilities: tuple[str, ...] = ()
    evidence_refs: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "node_id": self.node_id,
                "run_id": self.run_id,
                "intent_id": self.intent_id,
                "statement": self.statement,
                "node_type": self.node_type,
                "status": self.status,
                "priority": self.priority,
                "target": self.target,
                "parent_node_id": self.parent_node_id,
                "required_capabilities": list(self.required_capabilities),
                "evidence_refs": list(self.evidence_refs),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "SearchNode":
        contract_version(payload, kind=cls.KIND)
        return cls(
            node_id=required_text(payload.get("node_id") or payload.get("id"), "search_node_id"),
            run_id=required_text(payload.get("run_id"), "search_run_id"),
            intent_id=required_text(payload.get("intent_id"), "search_intent_id"),
            statement=required_text(payload.get("statement"), "search_statement"),
            node_type=optional_text(payload.get("node_type") or payload.get("type")) or "hypothesis",
            status=optional_text(payload.get("status")) or "proposed",
            priority=bounded_int(
                payload.get("priority", 50),
                default=50,
                minimum=0,
                maximum=100,
                field="search_priority",
            ),
            target=optional_text(payload.get("target")),
            parent_node_id=optional_text(payload.get("parent_node_id") or payload.get("parent_id")),
            required_capabilities=unique_strings(payload.get("required_capabilities")),
            evidence_refs=unique_strings(payload.get("evidence_refs")),
            metadata=json_mapping(payload.get("metadata"), field="search_node.metadata"),
        )
