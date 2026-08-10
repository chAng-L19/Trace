from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    CORE_SCHEMA_VERSION,
    bounded_int,
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class GoalCriterion:
    KIND: ClassVar[str] = "goal_criterion"

    criterion_id: str
    statement: str
    target: str = ""
    required_artifacts: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "criterion_id": self.criterion_id,
                "statement": self.statement,
                "target": self.target,
                "required_artifacts": list(self.required_artifacts),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "GoalCriterion":
        contract_version(payload, kind=cls.KIND)
        metadata = json_mapping(payload.get("metadata"), field="goal_criterion.metadata")
        workflow_id = optional_text(payload.get("workflow_id"))
        if workflow_id and "workflow_id" not in metadata:
            metadata["workflow_id"] = workflow_id
        return cls(
            criterion_id=required_text(payload.get("criterion_id") or payload.get("id"), "criterion_id"),
            statement=required_text(payload.get("statement"), "criterion_statement"),
            target=optional_text(payload.get("target")),
            required_artifacts=unique_strings(payload.get("required_artifacts")),
            metadata=metadata,
        )


@dataclass(frozen=True, slots=True)
class Goal:
    KIND: ClassVar[str] = "goal"
    SCHEMA_VERSION: ClassVar[int] = CORE_SCHEMA_VERSION

    goal_id: str
    objective: str
    targets: tuple[str, ...]
    criteria: tuple[GoalCriterion, ...] = ()
    constraints: Mapping[str, Any] = field(default_factory=dict)
    success_predicates: tuple[Mapping[str, Any], ...] = ()
    evidence_standard: str = "reproducible"
    max_actions: int = 64
    max_retries_per_action: int = 2
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "goal_id": self.goal_id,
                "objective": self.objective,
                "targets": list(self.targets),
                "criteria": [criterion.to_dict() for criterion in self.criteria],
                "constraints": dict(self.constraints),
                "success_predicates": [dict(predicate) for predicate in self.success_predicates],
                "evidence_standard": self.evidence_standard,
                "max_actions": self.max_actions,
                "max_retries_per_action": self.max_retries_per_action,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Goal":
        contract_version(payload, kind=cls.KIND)
        raw_criteria = payload.get("criteria", payload.get("success_criteria", ()))
        criteria = tuple(
            item if isinstance(item, GoalCriterion) else GoalCriterion.from_dict(item)
            for item in raw_criteria
            if isinstance(item, (GoalCriterion, Mapping))
        )
        raw_predicates = payload.get("success_predicates", ())
        predicates = tuple(
            json_mapping(item, field="goal.success_predicates[]")
            for item in raw_predicates
            if isinstance(item, Mapping)
        )
        return cls(
            goal_id=required_text(payload.get("goal_id") or payload.get("id"), "goal_id"),
            objective=required_text(payload.get("objective"), "objective"),
            targets=unique_strings(payload.get("targets")),
            criteria=criteria,
            constraints=json_mapping(payload.get("constraints"), field="goal.constraints"),
            success_predicates=predicates,
            evidence_standard=optional_text(payload.get("evidence_standard")) or "reproducible",
            max_actions=bounded_int(
                payload.get("max_actions", 64),
                default=64,
                minimum=1,
                maximum=4096,
                field="max_actions",
            ),
            max_retries_per_action=bounded_int(
                payload.get("max_retries_per_action", 2),
                default=2,
                minimum=0,
                maximum=8,
                field="max_retries_per_action",
            ),
            metadata=json_mapping(payload.get("metadata"), field="goal.metadata"),
        )


@dataclass(frozen=True, slots=True)
class Intent:
    KIND: ClassVar[str] = "intent"

    intent_id: str
    goal_id: str
    statement: str
    intent_type: str = "hypothesis"
    status: str = "proposed"
    priority: int = 50
    target: str = ""
    parent_intent_id: str = ""
    required_evidence: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "intent_id": self.intent_id,
                "goal_id": self.goal_id,
                "statement": self.statement,
                "intent_type": self.intent_type,
                "status": self.status,
                "priority": self.priority,
                "target": self.target,
                "parent_intent_id": self.parent_intent_id,
                "required_evidence": list(self.required_evidence),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Intent":
        contract_version(payload, kind=cls.KIND)
        return cls(
            intent_id=required_text(payload.get("intent_id") or payload.get("id"), "intent_id"),
            goal_id=required_text(payload.get("goal_id"), "intent_goal_id"),
            statement=required_text(payload.get("statement"), "intent_statement"),
            intent_type=optional_text(payload.get("intent_type") or payload.get("type")) or "hypothesis",
            status=optional_text(payload.get("status")) or "proposed",
            priority=bounded_int(
                payload.get("priority", 50),
                default=50,
                minimum=0,
                maximum=100,
                field="intent_priority",
            ),
            target=optional_text(payload.get("target")),
            parent_intent_id=optional_text(payload.get("parent_intent_id") or payload.get("parent_id")),
            required_evidence=unique_strings(payload.get("required_evidence")),
            metadata=json_mapping(payload.get("metadata"), field="intent.metadata"),
        )
