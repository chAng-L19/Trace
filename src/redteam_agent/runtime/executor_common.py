from __future__ import annotations

from dataclasses import dataclass

from .models import EvidenceNode, FactRecord, ReviewRecord
from .plan import PlanRevision


@dataclass(frozen=True)
class ExecutionOutcome:
    progressed: bool
    reason: str
    event_type: str
    evidence: EvidenceNode | None = None
    fact: FactRecord | None = None
    review: ReviewRecord | None = None
    plan: PlanRevision | None = None
    added_action_ids: tuple[str, ...] = ()


__all__ = ["ExecutionOutcome"]
