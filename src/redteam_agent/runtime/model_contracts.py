from __future__ import annotations

import hashlib
import json
import math
from datetime import datetime, timezone
from dataclasses import asdict, dataclass, field
from typing import Any, Mapping
from uuid import uuid4


def utc_now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def _utc_datetime(value: Any) -> datetime | None:
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    return (parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)).astimezone(timezone.utc)


def _safe_int(value: Any, default: int) -> int:
    try:
        return int(value)
    except (TypeError, ValueError, OverflowError):
        return default


def _safe_float(value: Any, default: float) -> float:
    try:
        parsed = float(value)
    except (TypeError, ValueError, OverflowError):
        return default
    return parsed if math.isfinite(parsed) else default


def _mapping(value: Any) -> Mapping[str, Any]:
    return value if isinstance(value, Mapping) else {}


def _sequence(value: Any) -> tuple[Any, ...] | list[Any]:
    return value if isinstance(value, (list, tuple)) else ()

@dataclass(frozen=True)
class SuccessPredicate:
    kind: str
    subject: str = ""
    operator: str = "exists"
    value: Any = True
    description: str = ""

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "SuccessPredicate":
        return cls(
            kind=str(payload.get("kind") or "artifact_verified"),
            subject=str(payload.get("subject") or ""),
            operator=str(payload.get("operator") or "exists"),
            value=payload.get("value", True),
            description=str(payload.get("description") or ""),
        )


@dataclass(frozen=True)
class GoalCriterion:
    criterion_id: str
    statement: str
    target: str = ""
    workflow_id: str = ""

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "GoalCriterion":
        return cls(
            criterion_id=str(payload.get("criterion_id") or payload.get("id") or ""),
            statement=str(payload.get("statement") or ""),
            target=str(payload.get("target") or ""),
            workflow_id=str(payload.get("workflow_id") or ""),
        )


@dataclass(frozen=True)
class GoalContract:
    goal_id: str
    objective: str
    targets: tuple[str, ...]
    workflow_hint: str = ""
    workflow_hints: tuple[str, ...] = ()
    starting_context: Mapping[str, Any] = field(default_factory=dict)
    constraints: Mapping[str, Any] = field(default_factory=dict)
    success_criteria: tuple[GoalCriterion, ...] = ()
    success_predicates: tuple[SuccessPredicate, ...] = ()
    intent_envelope: Mapping[str, Any] = field(default_factory=dict)
    stop_conditions: tuple[str, ...] = ()
    evidence_standard: str = "reproducible"
    max_actions: int = 64
    max_retries_per_action: int = 2
    created_at: str = field(default_factory=utc_now)

    @classmethod
    def create(
        cls,
        *,
        objective: str,
        targets: tuple[str, ...],
        workflow_hint: str = "",
        workflow_hints: tuple[str, ...] = (),
        starting_context: Mapping[str, Any] | None = None,
        constraints: Mapping[str, Any] | None = None,
        success_criteria: tuple[GoalCriterion, ...] = (),
        success_predicates: tuple[SuccessPredicate, ...] = (),
        intent_envelope: Mapping[str, Any] | None = None,
        stop_conditions: tuple[str, ...] = (),
        evidence_standard: str = "reproducible",
        max_actions: int = 64,
        max_retries_per_action: int = 2,
    ) -> "GoalContract":
        return cls(
            goal_id=f"goal-{uuid4().hex}",
            objective=objective,
            targets=tuple(dict.fromkeys(target.strip() for target in targets if target.strip())),
            workflow_hint=workflow_hint.strip(),
            workflow_hints=tuple(dict.fromkeys(item.strip() for item in workflow_hints if item.strip())),
            starting_context=dict(starting_context or {}),
            constraints=dict(constraints or {}),
            success_criteria=success_criteria,
            success_predicates=success_predicates,
            intent_envelope=dict(intent_envelope or {}),
            stop_conditions=stop_conditions,
            evidence_standard=evidence_standard,
            max_actions=max(1, min(4096, int(max_actions))),
            max_retries_per_action=max(0, min(8, int(max_retries_per_action))),
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "GoalContract":
        predicates = payload.get("success_predicates", ())
        criteria = payload.get("success_criteria", ())
        return cls(
            goal_id=str(payload.get("goal_id") or f"goal-{uuid4().hex}"),
            objective=str(payload.get("objective") or ""),
            targets=tuple(str(item) for item in _sequence(payload.get("targets")) if str(item).strip()),
            workflow_hint=str(payload.get("workflow_hint") or ""),
            workflow_hints=tuple(str(item) for item in _sequence(payload.get("workflow_hints")) if str(item).strip()),
            starting_context=dict(_mapping(payload.get("starting_context"))),
            constraints=dict(_mapping(payload.get("constraints"))),
            success_criteria=tuple(
                item if isinstance(item, GoalCriterion) else GoalCriterion.from_dict(item)
                for item in criteria
                if isinstance(item, (GoalCriterion, Mapping))
            ),
            success_predicates=tuple(
                item if isinstance(item, SuccessPredicate) else SuccessPredicate.from_dict(item)
                for item in predicates
                if isinstance(item, (SuccessPredicate, Mapping))
            ),
            intent_envelope=dict(_mapping(payload.get("intent_envelope"))),
            stop_conditions=tuple(str(item) for item in _sequence(payload.get("stop_conditions"))),
            evidence_standard=str(payload.get("evidence_standard") or "reproducible"),
            max_actions=max(1, min(4096, _safe_int(payload.get("max_actions"), 64))),
            max_retries_per_action=max(0, min(8, _safe_int(payload.get("max_retries_per_action"), 2))),
            created_at=str(payload.get("created_at") or utc_now()),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class ActionSpec:
    action_id: str
    name: str
    required_capabilities: tuple[str, ...]
    expected_artifact: str
    verifier: str
    depends_on: tuple[str, ...] = ()
    optional: bool = False
    risk: str = "active_low"
    timeout_seconds: float = 60.0
    max_retries: int = 2
    rollback_action: str = ""
    parameters: Mapping[str, Any] = field(default_factory=dict)
    attack_tags: tuple[str, ...] = ()
    tool_strategy: str = "first_verified"
    min_tool_results: int = 1
    max_tool_results: int = 1

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ActionSpec":
        capabilities = payload.get("required_capabilities") or payload.get("capabilities") or ()
        if isinstance(capabilities, str):
            capabilities = (capabilities,)
        raw_timeout = payload.get("timeout_seconds", 60.0)
        timeout_seconds = 60.0 if raw_timeout in (None, "") else float(raw_timeout)
        if not math.isfinite(timeout_seconds):
            raise ValueError("action_timeout_must_be_finite")
        raw_max_retries = payload.get("max_retries", 2)
        max_retries = 2 if raw_max_retries in (None, "") else int(raw_max_retries)
        return cls(
            action_id=str(payload.get("action_id") or payload.get("id") or ""),
            name=str(payload.get("name") or payload.get("action_id") or payload.get("id") or ""),
            required_capabilities=tuple(str(item) for item in capabilities),
            expected_artifact=str(payload.get("expected_artifact") or ""),
            verifier=str(payload.get("verifier") or payload.get("expected_artifact") or "generic"),
            depends_on=tuple(str(item) for item in payload.get("depends_on", ())),
            optional=bool(payload.get("optional", False)),
            risk=str(payload.get("risk") or "active_low"),
            timeout_seconds=max(0.1, timeout_seconds),
            max_retries=max(0, min(8, max_retries)),
            rollback_action=str(payload.get("rollback_action") or ""),
            parameters=dict(payload.get("parameters") or {}),
            attack_tags=tuple(str(item) for item in payload.get("attack_tags", ())),
            tool_strategy=str(payload.get("tool_strategy") or "first_verified"),
            min_tool_results=max(1, min(16, int(payload.get("min_tool_results") or 1))),
            max_tool_results=max(
                1,
                min(
                    16,
                    max(
                        int(payload.get("min_tool_results") or 1),
                        int(payload.get("max_tool_results") or 1),
                    ),
                ),
            ),
        )


@dataclass(frozen=True)
class WorkflowSpec:
    workflow_id: str
    version: int
    name: str
    description: str
    match_tags: tuple[str, ...]
    actions: tuple[ActionSpec, ...]
    terminal_predicates: tuple[SuccessPredicate, ...]
    required_artifacts: tuple[str, ...]

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "WorkflowSpec":
        workflow = payload.get("workflow") if isinstance(payload.get("workflow"), Mapping) else payload
        actions = payload.get("actions") or workflow.get("actions") or ()
        predicates = payload.get("terminal_predicates") or workflow.get("terminal_predicates") or ()
        return cls(
            workflow_id=str(workflow.get("id") or workflow.get("workflow_id") or ""),
            version=max(1, int(workflow.get("version") or 1)),
            name=str(workflow.get("name") or workflow.get("id") or ""),
            description=str(workflow.get("description") or ""),
            match_tags=tuple(str(item).casefold() for item in workflow.get("match_tags", ())),
            actions=tuple(ActionSpec.from_dict(item) for item in actions if isinstance(item, Mapping)),
            terminal_predicates=tuple(
                SuccessPredicate.from_dict(item) for item in predicates if isinstance(item, Mapping)
            ),
            required_artifacts=tuple(str(item) for item in workflow.get("required_artifacts", ())),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @property
    def fingerprint(self) -> str:
        serialized = json.dumps(self.to_dict(), ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
        return hashlib.sha256(serialized).hexdigest()


