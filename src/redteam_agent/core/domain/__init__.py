import sys
from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import CORE_SCHEMA_VERSION, bounded_int, contract_version, json_mapping, optional_text, required_text, unique_strings, versioned_payload


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
        return versioned_payload(self.KIND, {"asset_id": self.asset_id, "run_id": self.run_id, "asset_type": self.asset_type, "name": self.name, "target": self.target, "status": self.status, "evidence_ids": list(self.evidence_ids), "attributes": dict(self.attributes)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Asset":
        contract_version(payload, kind=cls.KIND)
        return cls(required_text(payload.get("asset_id") or payload.get("id"), "asset_id"), required_text(payload.get("run_id"), "asset_run_id"), required_text(payload.get("asset_type") or payload.get("type"), "asset_type"), required_text(payload.get("name") or payload.get("value"), "asset_name"), optional_text(payload.get("target")), optional_text(payload.get("status")) or "verified", unique_strings(payload.get("evidence_ids")), json_mapping(payload.get("attributes"), field="asset.attributes"))


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
        return versioned_payload(self.KIND, {"path_id": self.path_id, "run_id": self.run_id, "title": self.title, "status": self.status, "asset_ids": list(self.asset_ids), "relationship_ids": list(self.relationship_ids), "finding_ids": list(self.finding_ids), "evidence_ids": list(self.evidence_ids), "impact": self.impact, "metadata": dict(self.metadata)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "AttackPath":
        contract_version(payload, kind=cls.KIND)
        return cls(required_text(payload.get("path_id") or payload.get("id"), "attack_path_id"), required_text(payload.get("run_id"), "attack_path_run_id"), required_text(payload.get("title"), "attack_path_title"), optional_text(payload.get("status")) or "candidate", unique_strings(payload.get("asset_ids")), unique_strings(payload.get("relationship_ids")), unique_strings(payload.get("finding_ids")), unique_strings(payload.get("evidence_ids")), optional_text(payload.get("impact")), json_mapping(payload.get("metadata"), field="attack_path.metadata"))


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
        return versioned_payload(self.KIND, {"node_id": self.node_id, "run_id": self.run_id, "intent_id": self.intent_id, "statement": self.statement, "node_type": self.node_type, "status": self.status, "priority": self.priority, "target": self.target, "parent_node_id": self.parent_node_id, "required_capabilities": list(self.required_capabilities), "evidence_refs": list(self.evidence_refs), "metadata": dict(self.metadata)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "SearchNode":
        contract_version(payload, kind=cls.KIND)
        return cls(required_text(payload.get("node_id") or payload.get("id"), "search_node_id"), required_text(payload.get("run_id"), "search_run_id"), required_text(payload.get("intent_id"), "search_intent_id"), required_text(payload.get("statement"), "search_statement"), optional_text(payload.get("node_type") or payload.get("type")) or "hypothesis", optional_text(payload.get("status")) or "proposed", bounded_int(payload.get("priority", 50), default=50, minimum=0, maximum=100, field="search_priority"), optional_text(payload.get("target")), optional_text(payload.get("parent_node_id") or payload.get("parent_id")), unique_strings(payload.get("required_capabilities")), unique_strings(payload.get("evidence_refs")), json_mapping(payload.get("metadata"), field="search_node.metadata"))


@dataclass(frozen=True, slots=True)
class GoalCriterion:
    KIND: ClassVar[str] = "goal_criterion"
    criterion_id: str
    statement: str
    target: str = ""
    required_artifacts: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(self.KIND, {"criterion_id": self.criterion_id, "statement": self.statement, "target": self.target, "required_artifacts": list(self.required_artifacts), "metadata": dict(self.metadata)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "GoalCriterion":
        contract_version(payload, kind=cls.KIND)
        metadata = json_mapping(payload.get("metadata"), field="goal_criterion.metadata")
        workflow_id = optional_text(payload.get("workflow_id"))
        if workflow_id and "workflow_id" not in metadata:
            metadata["workflow_id"] = workflow_id
        return cls(required_text(payload.get("criterion_id") or payload.get("id"), "criterion_id"), required_text(payload.get("statement"), "criterion_statement"), optional_text(payload.get("target")), unique_strings(payload.get("required_artifacts")), metadata)


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
        return versioned_payload(self.KIND, {"goal_id": self.goal_id, "objective": self.objective, "targets": list(self.targets), "criteria": [criterion.to_dict() for criterion in self.criteria], "constraints": dict(self.constraints), "success_predicates": [dict(predicate) for predicate in self.success_predicates], "evidence_standard": self.evidence_standard, "max_actions": self.max_actions, "max_retries_per_action": self.max_retries_per_action, "metadata": dict(self.metadata)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Goal":
        contract_version(payload, kind=cls.KIND)
        criteria = tuple(item if isinstance(item, GoalCriterion) else GoalCriterion.from_dict(item) for item in payload.get("criteria", payload.get("success_criteria", ())) if isinstance(item, (GoalCriterion, Mapping)))
        predicates = tuple(json_mapping(item, field="goal.success_predicates[]") for item in payload.get("success_predicates", ()) if isinstance(item, Mapping))
        return cls(required_text(payload.get("goal_id") or payload.get("id"), "goal_id"), required_text(payload.get("objective"), "objective"), unique_strings(payload.get("targets")), criteria, json_mapping(payload.get("constraints"), field="goal.constraints"), predicates, optional_text(payload.get("evidence_standard")) or "reproducible", bounded_int(payload.get("max_actions", 64), default=64, minimum=1, maximum=4096, field="max_actions"), bounded_int(payload.get("max_retries_per_action", 2), default=2, minimum=0, maximum=8, field="max_retries_per_action"), json_mapping(payload.get("metadata"), field="goal.metadata"))


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
        return versioned_payload(self.KIND, {"intent_id": self.intent_id, "goal_id": self.goal_id, "statement": self.statement, "intent_type": self.intent_type, "status": self.status, "priority": self.priority, "target": self.target, "parent_intent_id": self.parent_intent_id, "required_evidence": list(self.required_evidence), "metadata": dict(self.metadata)})

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Intent":
        contract_version(payload, kind=cls.KIND)
        return cls(required_text(payload.get("intent_id") or payload.get("id"), "intent_id"), required_text(payload.get("goal_id"), "intent_goal_id"), required_text(payload.get("statement"), "intent_statement"), optional_text(payload.get("intent_type") or payload.get("type")) or "hypothesis", optional_text(payload.get("status")) or "proposed", bounded_int(payload.get("priority", 50), default=50, minimum=0, maximum=100, field="intent_priority"), optional_text(payload.get("target")), optional_text(payload.get("parent_intent_id") or payload.get("parent_id")), unique_strings(payload.get("required_evidence")), json_mapping(payload.get("metadata"), field="intent.metadata"))


from .evidence import Evidence, EvidenceProvenance, Finding
from .exploration import (
    EXPLORATION_KINDS,
    EXPLORATION_STATUSES,
    ExplorationRecord,
)
import sys
from .run import Budget, RUN_STATUSES, Run, TerminalDecision

sys.modules[f"{__name__}.assets"] = sys.modules[__name__]
sys.modules[f"{__name__}.search"] = sys.modules[__name__]
sys.modules[f"{__name__}.goal"] = sys.modules[__name__]

__all__ = [
    "Asset",
    "AttackPath",
    "Budget",
    "Evidence",
    "EvidenceProvenance",
    "Finding",
    "EXPLORATION_KINDS",
    "EXPLORATION_STATUSES",
    "ExplorationRecord",
    "Goal",
    "GoalCriterion",
    "Intent",
    "RUN_STATUSES",
    "Run",
    "SearchNode",
    "TerminalDecision",
]
