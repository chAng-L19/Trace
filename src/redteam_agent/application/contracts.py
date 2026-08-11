from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from typing import Any, Mapping, Sequence

from ..core import Evidence, Event, Goal, Run, TerminalDecision
from ..core.contracts import ContractError, json_mapping, json_value, required_text, unique_strings


@dataclass(frozen=True, slots=True)
class StartRequest:
    session_id: str
    objective: str
    targets: tuple[str, ...] = ()
    workflow_hint: str = ""
    starting_context: Mapping[str, Any] = field(default_factory=dict)
    constraints: Mapping[str, Any] = field(default_factory=dict)
    success_predicates: tuple[Mapping[str, Any], ...] = ()
    max_actions: int = 64
    max_retries_per_action: int = 2
    token_limit: int | None = None
    time_limit_seconds: float | None = None
    deadline: str = ""

    @classmethod
    def from_value(cls, value: "StartRequest | Mapping[str, Any]") -> "StartRequest":
        if isinstance(value, cls):
            request = value
        elif isinstance(value, Mapping):
            raw_predicates = value.get("success_predicates")
            predicates = (
                tuple(dict(item) for item in raw_predicates if isinstance(item, Mapping))
                if isinstance(raw_predicates, Sequence) and not isinstance(raw_predicates, (str, bytes))
                else ()
            )
            request = cls(
                session_id=str(value.get("session_id") or ""),
                objective=str(value.get("objective") or ""),
                targets=unique_strings(value.get("targets")),
                workflow_hint=str(value.get("workflow_hint") or ""),
                starting_context=json_mapping(value.get("starting_context"), field="start.starting_context"),
                constraints=json_mapping(value.get("constraints"), field="start.constraints"),
                success_predicates=predicates,
                max_actions=int(value.get("max_actions", 64)),
                max_retries_per_action=int(value.get("max_retries_per_action", 2)),
                token_limit=(int(value["token_limit"]) if value.get("token_limit") is not None else None),
                time_limit_seconds=(
                    float(value["time_limit_seconds"])
                    if value.get("time_limit_seconds") is not None
                    else None
                ),
                deadline=str(value.get("deadline") or ""),
            )
        else:
            raise TypeError("start_request_must_be_mapping")
        return request.validated()

    def validated(self) -> "StartRequest":
        session_id = required_text(self.session_id, "session_id")
        objective = required_text(self.objective, "objective")
        if not 1 <= int(self.max_actions) <= 4096:
            raise ContractError("max_actions_out_of_range:1:4096")
        if not 0 <= int(self.max_retries_per_action) <= 32:
            raise ContractError("max_retries_per_action_out_of_range:0:32")
        if self.token_limit is not None and int(self.token_limit) <= 0:
            raise ContractError("token_limit_must_be_positive")
        if self.time_limit_seconds is not None and (
            not math.isfinite(float(self.time_limit_seconds)) or float(self.time_limit_seconds) <= 0
        ):
            raise ContractError("time_limit_seconds_must_be_positive_finite")
        return replace(
            self,
            session_id=session_id,
            objective=objective,
            targets=tuple(dict.fromkeys(item.strip() for item in self.targets if item.strip())),
            workflow_hint=self.workflow_hint.strip(),
            starting_context=json_mapping(self.starting_context, field="start.starting_context"),
            constraints=json_mapping(self.constraints, field="start.constraints"),
            success_predicates=tuple(
                json_mapping(item, field="start.success_predicate")
                for item in self.success_predicates
            ),
            max_actions=int(self.max_actions),
            max_retries_per_action=int(self.max_retries_per_action),
            token_limit=int(self.token_limit) if self.token_limit is not None else None,
            time_limit_seconds=(
                float(self.time_limit_seconds) if self.time_limit_seconds is not None else None
            ),
            deadline=self.deadline.strip(),
        )

    def runtime_arguments(self) -> dict[str, Any]:
        return {
            "session_id": self.session_id,
            "objective": self.objective,
            "targets": self.targets,
            "workflow_hint": self.workflow_hint,
            "starting_context": dict(self.starting_context),
            "constraints": dict(self.constraints),
            "success_predicates": tuple(dict(item) for item in self.success_predicates),
            "max_actions": self.max_actions,
            "max_retries_per_action": self.max_retries_per_action,
            "token_limit": self.token_limit,
            "time_limit_seconds": self.time_limit_seconds,
            "deadline": self.deadline,
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "session_id": self.session_id,
            "objective": self.objective,
            "targets": list(self.targets),
            "workflow_hint": self.workflow_hint,
            "starting_context": dict(self.starting_context),
            "constraints": dict(self.constraints),
            "success_predicates": [dict(item) for item in self.success_predicates],
            "max_actions": self.max_actions,
            "max_retries_per_action": self.max_retries_per_action,
            "token_limit": self.token_limit,
            "time_limit_seconds": self.time_limit_seconds,
            "deadline": self.deadline,
        }


@dataclass(frozen=True, slots=True)
class BudgetDelta:
    actions: int = 0
    tokens: int = 0
    time_seconds: float = 0.0
    deadline: str = ""
    idempotency_key: str = ""
    acknowledge_missing_usage: bool = False

    @classmethod
    def from_value(cls, value: "BudgetDelta | Mapping[str, Any] | None") -> "BudgetDelta":
        if value is None:
            return cls()
        if isinstance(value, cls):
            delta = value
        elif isinstance(value, Mapping):
            delta = cls(
                actions=int(value.get("actions", 0)),
                tokens=int(value.get("tokens", 0)),
                time_seconds=float(value.get("time_seconds", 0.0)),
                deadline=str(value.get("deadline") or ""),
                idempotency_key=str(value.get("idempotency_key") or ""),
                acknowledge_missing_usage=bool(value.get("acknowledge_missing_usage", False)),
            )
        else:
            raise TypeError("budget_delta_must_be_mapping")
        if delta.actions < 0 or delta.tokens < 0 or delta.time_seconds < 0:
            raise ContractError("budget_delta_must_be_nonnegative")
        if not math.isfinite(delta.time_seconds):
            raise ContractError("budget_time_delta_must_be_finite")
        return replace(
            delta,
            deadline=delta.deadline.strip(),
            idempotency_key=delta.idempotency_key.strip(),
        )

    @property
    def changes_budget(self) -> bool:
        return bool(
            self.actions
            or self.tokens
            or self.time_seconds
            or self.deadline
            or self.acknowledge_missing_usage
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "actions": self.actions,
            "tokens": self.tokens,
            "time_seconds": self.time_seconds,
            "deadline": self.deadline,
            "idempotency_key": self.idempotency_key,
            "acknowledge_missing_usage": self.acknowledge_missing_usage,
        }


@dataclass(frozen=True, slots=True)
class Observation:
    action_id: str
    output: Any
    tool: str = "host-agent"
    usage: Mapping[str, Any] = field(default_factory=dict)
    idempotency_key: str = ""
    continue_run: bool = True
    max_actions: int | None = None
    handoff_id: str = ""
    handoff_token: str = ""
    attempt_id: str = ""
    contract_hash: str = ""

    @classmethod
    def from_value(cls, value: "Observation | Mapping[str, Any]") -> "Observation":
        if isinstance(value, cls):
            observation = value
        elif isinstance(value, Mapping):
            observation = cls(
                action_id=str(value.get("action_id") or ""),
                output=json_value(value.get("output"), field="observation.output"),
                tool=str(value.get("tool") or "host-agent"),
                usage=json_mapping(value.get("usage"), field="observation.usage"),
                idempotency_key=str(value.get("idempotency_key") or ""),
                continue_run=bool(value.get("continue_run", True)),
                max_actions=(int(value["max_actions"]) if value.get("max_actions") is not None else None),
                handoff_id=str(value.get("handoff_id") or ""),
                handoff_token=str(value.get("handoff_token") or ""),
                attempt_id=str(value.get("attempt_id") or ""),
                contract_hash=str(value.get("contract_hash") or ""),
            )
        else:
            raise TypeError("observation_must_be_mapping")
        return observation.validated()

    def validated(self) -> "Observation":
        action_id = required_text(self.action_id, "observation_action_id")
        if self.max_actions is not None and int(self.max_actions) <= 0:
            raise ContractError("observation_max_actions_must_be_positive")
        receipt = (self.handoff_id, self.handoff_token, self.attempt_id, self.contract_hash)
        if any(receipt) and not all(receipt):
            raise ContractError("handoff_receipt_fields_incomplete")
        return replace(
            self,
            action_id=action_id,
            output=json_value(self.output, field="observation.output"),
            tool=self.tool.strip() or "host-agent",
            usage=json_mapping(self.usage, field="observation.usage"),
            idempotency_key=self.idempotency_key.strip(),
            max_actions=int(self.max_actions) if self.max_actions is not None else None,
            handoff_id=self.handoff_id.strip(),
            handoff_token=self.handoff_token.strip(),
            attempt_id=self.attempt_id.strip(),
            contract_hash=self.contract_hash.strip(),
        )

    @property
    def has_handoff_receipt(self) -> bool:
        return bool(self.handoff_id)


@dataclass(frozen=True, slots=True)
class AgentRunView:
    run: Run
    goal: Goal
    evidence: tuple[Evidence, ...]
    terminal: TerminalDecision
    next_action: str = ""
    missing_capabilities: tuple[str, ...] = ()
    handoff: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "run": self.run.to_dict(),
            "goal": self.goal.to_dict(),
            "evidence": [item.to_dict() for item in self.evidence],
            "terminal": self.terminal.to_dict(),
            "next_action": self.next_action,
            "missing_capabilities": list(self.missing_capabilities),
            "handoff": dict(self.handoff),
        }


@dataclass(frozen=True, slots=True)
class AgentStartResult:
    runs: tuple[AgentRunView, ...]
    batch_id: str = ""

    @property
    def run_ids(self) -> tuple[str, ...]:
        return tuple(item.run.run_id for item in self.runs)

    @property
    def single(self) -> AgentRunView:
        if len(self.runs) != 1:
            raise ValueError(f"start_result_not_single:{len(self.runs)}")
        return self.runs[0]

    def to_dict(self) -> dict[str, Any]:
        return {
            "batch_id": self.batch_id,
            "runs": [item.to_dict() for item in self.runs],
        }


AgentEvent = Event
