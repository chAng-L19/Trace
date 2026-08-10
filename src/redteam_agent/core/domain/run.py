from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, ClassVar, Mapping

from ..contracts import (
    bounded_int,
    contract_version,
    json_mapping,
    optional_nonnegative_int,
    optional_positive_float,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


RUN_STATUSES = frozenset(
    {
        "created",
        "running",
        "waiting_worker",
        "paused_budget",
        "cancelling",
        "cancelled",
        "completed",
        "failed",
    }
)


@dataclass(frozen=True, slots=True)
class Budget:
    KIND: ClassVar[str] = "budget"

    action_limit: int = 64
    token_limit: int | None = None
    time_limit_seconds: float | None = None
    actions_used: int = 0
    input_tokens_used: int | None = None
    output_tokens_used: int | None = None
    token_usage_missing: int = 0
    started_at: str = ""
    deadline: str = ""
    pause_reason: str = ""
    paused_at: str = ""

    @property
    def remaining_actions(self) -> int:
        return max(0, self.action_limit - self.actions_used)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "action_limit": self.action_limit,
                "token_limit": self.token_limit,
                "time_limit_seconds": self.time_limit_seconds,
                "actions_used": self.actions_used,
                "input_tokens_used": self.input_tokens_used,
                "output_tokens_used": self.output_tokens_used,
                "token_usage_missing": self.token_usage_missing,
                "started_at": self.started_at,
                "deadline": self.deadline,
                "pause_reason": self.pause_reason,
                "paused_at": self.paused_at,
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Budget":
        contract_version(payload, kind=cls.KIND)
        legacy_total = optional_nonnegative_int(payload.get("tokens_used"), field="tokens_used")
        input_used = optional_nonnegative_int(payload.get("input_tokens_used"), field="input_tokens_used")
        output_used = optional_nonnegative_int(payload.get("output_tokens_used"), field="output_tokens_used")
        if input_used is None and output_used is None and legacy_total is not None:
            output_used = legacy_total
        return cls(
            action_limit=bounded_int(
                payload.get("action_limit", 64),
                default=64,
                minimum=1,
                maximum=4096,
                field="action_limit",
            ),
            token_limit=optional_nonnegative_int(payload.get("token_limit"), field="token_limit"),
            time_limit_seconds=optional_positive_float(
                payload.get("time_limit_seconds"),
                field="time_limit_seconds",
            ),
            actions_used=bounded_int(
                payload.get("actions_used", 0),
                default=0,
                minimum=0,
                maximum=2**63 - 1,
                field="actions_used",
            ),
            input_tokens_used=input_used,
            output_tokens_used=output_used,
            token_usage_missing=bounded_int(
                payload.get("token_usage_missing", 0),
                default=0,
                minimum=0,
                maximum=2**63 - 1,
                field="token_usage_missing",
            ),
            started_at=optional_text(payload.get("started_at")),
            deadline=optional_text(payload.get("deadline")),
            pause_reason=optional_text(payload.get("pause_reason")),
            paused_at=optional_text(payload.get("paused_at")),
        )


@dataclass(frozen=True, slots=True)
class Run:
    KIND: ClassVar[str] = "run"

    run_id: str
    session_id: str
    goal_id: str
    status: str
    state_version: int = 0
    branch_id: str = "main"
    current_intent_id: str = ""
    current_search_node_id: str = ""
    budget: Budget = field(default_factory=Budget)
    evidence_ids: tuple[str, ...] = ()
    created_at: str = ""
    updated_at: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "run_id": self.run_id,
                "session_id": self.session_id,
                "goal_id": self.goal_id,
                "status": self.status,
                "state_version": self.state_version,
                "branch_id": self.branch_id,
                "current_intent_id": self.current_intent_id,
                "current_search_node_id": self.current_search_node_id,
                "budget": self.budget.to_dict(),
                "evidence_ids": list(self.evidence_ids),
                "created_at": self.created_at,
                "updated_at": self.updated_at,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Run":
        contract_version(payload, kind=cls.KIND)
        status = optional_text(payload.get("status")) or "created"
        if status not in RUN_STATUSES:
            raise ValueError(f"run_status_invalid:{status}")
        budget_payload = payload.get("budget") if isinstance(payload.get("budget"), Mapping) else {}
        return cls(
            run_id=required_text(payload.get("run_id") or payload.get("id"), "run_id"),
            session_id=required_text(payload.get("session_id"), "session_id"),
            goal_id=required_text(payload.get("goal_id"), "run_goal_id"),
            status=status,
            state_version=bounded_int(
                payload.get("state_version", payload.get("version", 0)),
                default=0,
                minimum=0,
                maximum=2**63 - 1,
                field="state_version",
            ),
            branch_id=optional_text(payload.get("branch_id")) or "main",
            current_intent_id=optional_text(payload.get("current_intent_id")),
            current_search_node_id=optional_text(
                payload.get("current_search_node_id") or payload.get("current_action_id")
            ),
            budget=Budget.from_dict(budget_payload),
            evidence_ids=unique_strings(payload.get("evidence_ids")),
            created_at=optional_text(payload.get("created_at")),
            updated_at=optional_text(payload.get("updated_at")),
            metadata=json_mapping(payload.get("metadata"), field="run.metadata"),
        )


@dataclass(frozen=True, slots=True)
class TerminalDecision:
    KIND: ClassVar[str] = "terminal_decision"

    terminal: bool
    success: bool
    reason: str
    satisfied: tuple[str, ...] = ()
    missing: tuple[str, ...] = ()
    evidence_refs: tuple[str, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "terminal": self.terminal,
                "success": self.success,
                "reason": self.reason,
                "satisfied": list(self.satisfied),
                "missing": list(self.missing),
                "evidence_refs": list(self.evidence_refs),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "TerminalDecision":
        contract_version(payload, kind=cls.KIND)
        return cls(
            terminal=bool(payload.get("terminal", False)),
            success=bool(payload.get("success", False)),
            reason=optional_text(payload.get("reason")),
            satisfied=unique_strings(payload.get("satisfied")),
            missing=unique_strings(payload.get("missing")),
            evidence_refs=unique_strings(payload.get("evidence_refs")),
            metadata=json_mapping(payload.get("metadata"), field="terminal_decision.metadata"),
        )
