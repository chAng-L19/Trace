from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, ClassVar, Protocol, runtime_checkable

from ..contracts import (
    bounded_int,
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class Event:
    KIND: ClassVar[str] = "event"

    run_id: str
    event_type: str
    payload: Mapping[str, Any]
    sequence: int = 0
    created_at: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "run_id": self.run_id,
                "event_type": self.event_type,
                "payload": dict(self.payload),
                "sequence": self.sequence,
                "created_at": self.created_at,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Event":
        contract_version(payload, kind=cls.KIND)
        return cls(
            run_id=required_text(payload.get("run_id"), "event_run_id"),
            event_type=required_text(payload.get("event_type") or payload.get("type"), "event_type"),
            payload=json_mapping(payload.get("payload"), field="event.payload"),
            sequence=bounded_int(
                payload.get("sequence", payload.get("event_id", 0)),
                default=0,
                minimum=0,
                maximum=2**63 - 1,
                field="event_sequence",
            ),
            created_at=optional_text(payload.get("created_at")),
            metadata=json_mapping(payload.get("metadata"), field="event.metadata"),
        )


@runtime_checkable
class EventPort(Protocol):
    def append(self, event: Event) -> None: ...

    def read(self, run_id: str, *, after_sequence: int = 0, limit: int = 200) -> tuple[Event, ...]: ...
