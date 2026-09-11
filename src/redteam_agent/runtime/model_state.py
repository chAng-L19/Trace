from __future__ import annotations

import math
from dataclasses import asdict, dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Mapping
from uuid import uuid4

from .model_contracts import (
    ActionSpec,
    GoalContract,
    GoalCriterion,
    SuccessPredicate,
    WorkflowSpec,
    _mapping,
    _safe_float,
    _safe_int,
    _sequence,
    _utc_datetime,
    utc_now,
)

@dataclass(frozen=True)
class ToolDescriptor:
    server: str
    name: str
    description: str
    input_schema: Mapping[str, Any]
    capabilities: tuple[str, ...]
    source: str = "live-mcp"
    healthy: bool = True
    priority: int = 100
    version: str = "unknown"
    schema_hash: str = ""
    side_effecting: bool = True
    supports_reconcile: bool = False
    metadata: Mapping[str, Any] = field(default_factory=dict)

    @property
    def qualified_name(self) -> str:
        return f"{self.server}:{self.name}" if self.server else self.name


@dataclass(frozen=True, repr=False)
class ToolCallResult:
    status: str
    output: Any = None
    error: str = ""
    tool: str = ""
    started_at: str = ""
    finished_at: str = field(default_factory=utc_now)
    retryable: bool = False
    call_id: str = ""
    input_hash: str = ""
    output_hash: str = ""
    tool_version: str = ""

    def __repr__(self) -> str:
        return (
            "ToolCallResult("
            f"status={self.status!r}, tool={self.tool!r}, retryable={self.retryable}, "
            f"call_id={self.call_id!r}, input_hash={self.input_hash!r}, output_hash={self.output_hash!r})"
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ToolCallResult":
        return cls(
            status=str(payload.get("status") or "failed"),
            output=payload.get("output"),
            error=str(payload.get("error") or ""),
            tool=str(payload.get("tool") or ""),
            started_at=str(payload.get("started_at") or ""),
            finished_at=str(payload.get("finished_at") or utc_now()),
            retryable=bool(payload.get("retryable", False)),
            call_id=str(payload.get("call_id") or ""),
            input_hash=str(payload.get("input_hash") or ""),
            output_hash=str(payload.get("output_hash") or ""),
            tool_version=str(payload.get("tool_version") or ""),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class RunBudget:
    """Persistent run-wide continuation budget.

    ``actions_used`` counts real TaskAttempts, not resume cycles.  Token usage is
    nullable on purpose: the lightweight Codex host does not always expose model
    usage, and the runtime must not manufacture a zero.  ``deadline`` is an
    absolute UTC wall-clock bound so a stopped process cannot reset a time budget.
    """

    action_limit: int = 64
    token_limit: int | None = None
    time_limit_seconds: float | None = None
    actions_used: int = 0
    tokens_used: int | None = None
    input_tokens_used: int | None = None
    output_tokens_used: int | None = None
    token_usage_missing: int = 0
    token_usage_acknowledged: int = 0
    started_at: str = field(default_factory=utc_now)
    deadline: str = ""
    pause_reason: str = ""
    paused_at: str = ""

    @classmethod
    def create(
        cls,
        *,
        action_limit: int,
        token_limit: int | None = None,
        time_limit_seconds: float | None = None,
        started_at: str = "",
        deadline: str = "",
    ) -> "RunBudget":
        started = _utc_datetime(started_at) or datetime.now(timezone.utc).replace(microsecond=0)
        bounded_tokens = None if token_limit is None else max(1, int(token_limit))
        if time_limit_seconds is None:
            bounded_time = None
        else:
            raw_time = float(time_limit_seconds)
            if not math.isfinite(raw_time):
                raise ValueError("time_limit_seconds_must_be_finite")
            bounded_time = max(0.1, raw_time)
        resolved_deadline = _utc_datetime(deadline)
        if resolved_deadline is None and bounded_time is not None:
            resolved_deadline = started + timedelta(seconds=bounded_time)
        return cls(
            action_limit=max(1, min(4096, int(action_limit))),
            token_limit=bounded_tokens,
            time_limit_seconds=bounded_time,
            started_at=started.replace(microsecond=0).isoformat(),
            deadline=resolved_deadline.replace(microsecond=0).isoformat() if resolved_deadline else "",
        )

    @classmethod
    def from_dict(
        cls,
        payload: Mapping[str, Any],
        *,
        fallback_action_limit: int = 64,
        fallback_actions_used: int = 0,
        fallback_started_at: str = "",
    ) -> "RunBudget":
        if not payload:
            budget = cls.create(
                action_limit=fallback_action_limit,
                started_at=fallback_started_at,
            )
            budget.actions_used = max(0, int(fallback_actions_used))
            return budget
        raw_token_limit = payload.get("token_limit")
        raw_time_limit = payload.get("time_limit_seconds")
        raw_tokens_used = payload.get("tokens_used")
        raw_input_used = payload.get("input_tokens_used")
        raw_output_used = payload.get("output_tokens_used")
        budget = cls.create(
            action_limit=_safe_int(payload.get("action_limit"), fallback_action_limit),
            token_limit=(None if raw_token_limit in (None, "") else _safe_int(raw_token_limit, 0)),
            time_limit_seconds=(None if raw_time_limit in (None, "") else _safe_float(raw_time_limit, 0.0)),
            started_at=str(payload.get("started_at") or fallback_started_at),
            deadline=str(payload.get("deadline") or ""),
        )
        budget.actions_used = max(0, _safe_int(payload.get("actions_used"), fallback_actions_used))
        budget.tokens_used = None if raw_tokens_used in (None, "") else max(0, _safe_int(raw_tokens_used, 0))
        budget.input_tokens_used = (
            None if raw_input_used in (None, "") else max(0, _safe_int(raw_input_used, 0))
        )
        budget.output_tokens_used = (
            None if raw_output_used in (None, "") else max(0, _safe_int(raw_output_used, 0))
        )
        budget.token_usage_missing = max(0, _safe_int(payload.get("token_usage_missing"), 0))
        budget.token_usage_acknowledged = max(
            0,
            min(
                budget.token_usage_missing,
                _safe_int(payload.get("token_usage_acknowledged"), 0),
            ),
        )
        budget.pause_reason = str(payload.get("pause_reason") or "")
        budget.paused_at = str(payload.get("paused_at") or "")
        return budget

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def record_action(self) -> None:
        self.actions_used += 1

    @staticmethod
    def normalized_token_usage(
        usage: Mapping[str, Any] | None,
    ) -> tuple[int | None, int | None, int | None]:
        source = usage if isinstance(usage, Mapping) else {}
        if isinstance(source.get("usage"), Mapping):
            source = source["usage"]

        def count(name: str) -> int | None:
            raw = source.get(name)
            if isinstance(raw, bool) or raw is None:
                return None
            try:
                value = int(raw)
            except (TypeError, ValueError, OverflowError):
                return None
            return value if value >= 0 and value == raw else None

        input_tokens = count("input_tokens")
        output_tokens = count("output_tokens")
        total_tokens = count("total_tokens")
        if total_tokens is None:
            total_tokens = count("tokens")
        if total_tokens is None and input_tokens is not None and output_tokens is not None:
            total_tokens = input_tokens + output_tokens
        return input_tokens, output_tokens, total_tokens

    def record_token_usage(
        self,
        usage: Mapping[str, Any] | None,
        *,
        required: bool = False,
    ) -> tuple[int | None, int | None, int | None]:
        input_tokens, output_tokens, total_tokens = self.normalized_token_usage(usage)
        if input_tokens is not None:
            self.input_tokens_used = (self.input_tokens_used or 0) + input_tokens
        if output_tokens is not None:
            self.output_tokens_used = (self.output_tokens_used or 0) + output_tokens
        if total_tokens is None:
            if required:
                self.token_usage_missing += 1
            return input_tokens, output_tokens, None
        self.tokens_used = (self.tokens_used or 0) + total_tokens
        return input_tokens, output_tokens, total_tokens

    def extend(
        self,
        *,
        action_limit: int | None = None,
        token_limit: int | None = None,
        time_limit_seconds: float | None = None,
        deadline: str = "",
    ) -> bool:
        changed = False
        if action_limit is not None and int(action_limit) > self.action_limit:
            self.action_limit = max(1, min(4096, int(action_limit)))
            changed = True
        if token_limit is not None and (self.token_limit is None or int(token_limit) > self.token_limit):
            self.token_limit = max(1, int(token_limit))
            changed = True
        raw_time: float | None = None
        if time_limit_seconds is not None:
            raw_time = float(time_limit_seconds)
            if not math.isfinite(raw_time):
                raise ValueError("time_limit_seconds_must_be_finite")
        if raw_time is not None and (
            self.time_limit_seconds is None or raw_time > self.time_limit_seconds
        ):
            self.time_limit_seconds = max(0.1, raw_time)
            started = _utc_datetime(self.started_at) or datetime.now(timezone.utc)
            candidate = started + timedelta(seconds=self.time_limit_seconds)
            current = _utc_datetime(self.deadline)
            if current is None or candidate > current:
                self.deadline = candidate.replace(microsecond=0).isoformat()
            changed = True
        explicit_deadline = _utc_datetime(deadline)
        current_deadline = _utc_datetime(self.deadline)
        if explicit_deadline is not None and (current_deadline is None or explicit_deadline > current_deadline):
            self.deadline = explicit_deadline.replace(microsecond=0).isoformat()
            changed = True
        if changed:
            self.pause_reason = ""
            self.paused_at = ""
        return changed

    def apply_delta(
        self,
        *,
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
        acknowledge_missing_usage: bool = False,
    ) -> bool:
        action_delta = max(0, int(actions))
        token_delta = max(0, int(tokens))
        raw_time_delta = float(time_seconds)
        if not math.isfinite(raw_time_delta):
            raise ValueError("budget_time_delta_must_be_finite")
        time_delta = max(0.0, raw_time_delta)
        if not any((action_delta, token_delta, time_delta, deadline, acknowledge_missing_usage)):
            return False
        if action_delta:
            self.action_limit = min(4096, self.action_limit + action_delta)
        if token_delta:
            base = self.token_limit if self.token_limit is not None else (self.tokens_used or 0)
            self.token_limit = base + token_delta
        if time_delta:
            self.time_limit_seconds = (self.time_limit_seconds or 0.0) + time_delta
            current = _utc_datetime(self.deadline)
            now = datetime.now(timezone.utc).replace(microsecond=0)
            base = current if current is not None and current > now else now
            self.deadline = (base + timedelta(seconds=time_delta)).replace(microsecond=0).isoformat()
        explicit_deadline = _utc_datetime(deadline)
        current_deadline = _utc_datetime(self.deadline)
        if explicit_deadline is not None and (current_deadline is None or explicit_deadline > current_deadline):
            self.deadline = explicit_deadline.replace(microsecond=0).isoformat()
        if acknowledge_missing_usage:
            self.token_usage_acknowledged = self.token_usage_missing
        self.resume()
        return True

    def exhaustion_reason(self, *, now: str = "") -> str:
        if self.actions_used >= self.action_limit:
            return "action_limit_exhausted"
        if self.token_limit is not None:
            if self.token_usage_missing > self.token_usage_acknowledged:
                return "token_usage_unknown"
            if self.tokens_used is not None and self.tokens_used >= self.token_limit:
                return "token_limit_exhausted"
        deadline = _utc_datetime(self.deadline)
        current = _utc_datetime(now) or datetime.now(timezone.utc)
        if deadline is not None and current >= deadline:
            return "time_limit_exhausted"
        return ""

    def pause(self, reason: str) -> None:
        self.pause_reason = str(reason or "paused")
        self.paused_at = utc_now()

    def resume(self) -> None:
        self.pause_reason = ""
        self.paused_at = ""


@dataclass(frozen=True)
class LeaseToken:
    run_id: str
    action_id: str
    owner: str
    fencing_token: int
    expires_at: float

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "LeaseToken":
        return cls(
            run_id=str(payload.get("run_id") or ""),
            action_id=str(payload.get("action_id") or ""),
            owner=str(payload.get("owner") or ""),
            fencing_token=max(0, _safe_int(payload.get("fencing_token"), 0)),
            expires_at=max(0.0, _safe_float(payload.get("expires_at"), 0.0)),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class TaskAttempt:
    attempt_id: str
    run_id: str
    branch_id: str
    plan_revision: int
    action_id: str
    tool: str
    tool_version: str
    input_hash: str
    idempotency_key: str
    status: str = "prepared"
    fencing_token: int = 0
    result: Any = None
    error: str = ""
    started_at: str = field(default_factory=utc_now)
    finished_at: str = ""

    @classmethod
    def create(
        cls,
        *,
        run_id: str,
        branch_id: str,
        plan_revision: int,
        action_id: str,
        tool: str,
        tool_version: str,
        input_hash: str,
        idempotency_key: str,
        fencing_token: int = 0,
    ) -> "TaskAttempt":
        return cls(
            attempt_id=f"attempt-{uuid4().hex}",
            run_id=run_id,
            branch_id=branch_id or "main",
            plan_revision=max(1, int(plan_revision)),
            action_id=action_id,
            tool=tool,
            tool_version=tool_version or "unknown",
            input_hash=input_hash,
            idempotency_key=idempotency_key,
            fencing_token=max(0, int(fencing_token)),
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "TaskAttempt":
        return cls(
            attempt_id=str(payload.get("attempt_id") or f"attempt-{uuid4().hex}"),
            run_id=str(payload.get("run_id") or ""),
            branch_id=str(payload.get("branch_id") or "main"),
            plan_revision=max(1, _safe_int(payload.get("plan_revision"), 1)),
            action_id=str(payload.get("action_id") or ""),
            tool=str(payload.get("tool") or ""),
            tool_version=str(payload.get("tool_version") or "unknown"),
            input_hash=str(payload.get("input_hash") or ""),
            idempotency_key=str(payload.get("idempotency_key") or ""),
            status=str(payload.get("status") or "prepared"),
            fencing_token=max(0, _safe_int(payload.get("fencing_token"), 0)),
            result=payload.get("result"),
            error=str(payload.get("error") or ""),
            started_at=str(payload.get("started_at") or utc_now()),
            finished_at=str(payload.get("finished_at") or ""),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class FactRecord:
    fact_id: str
    run_id: str
    branch_id: str
    plan_revision: int
    key: str
    value: Any
    version: int
    source_evidence_ids: tuple[str, ...] = ()
    parent_fact_ids: tuple[str, ...] = ()
    valid: bool = True
    invalidated_by: tuple[str, ...] = ()
    invalidation_reason: str = ""
    created_at: str = field(default_factory=utc_now)

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "FactRecord":
        return cls(
            fact_id=str(payload.get("fact_id") or f"fact-{uuid4().hex}"),
            run_id=str(payload.get("run_id") or ""),
            branch_id=str(payload.get("branch_id") or "main"),
            plan_revision=max(1, _safe_int(payload.get("plan_revision"), 1)),
            key=str(payload.get("key") or ""),
            value=payload.get("value"),
            version=max(1, _safe_int(payload.get("version"), 1)),
            source_evidence_ids=tuple(str(item) for item in _sequence(payload.get("source_evidence_ids"))),
            parent_fact_ids=tuple(str(item) for item in _sequence(payload.get("parent_fact_ids"))),
            valid=bool(payload.get("valid", True)),
            invalidated_by=tuple(str(item) for item in _sequence(payload.get("invalidated_by"))),
            invalidation_reason=str(payload.get("invalidation_reason") or ""),
            created_at=str(payload.get("created_at") or utc_now()),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class GatePredicate:
    gate_id: str
    predicate: str
    inputs: tuple[str, ...] = ()
    expected: Any = True
    operator: str = "eq"
    on_pass: str = "advance"
    on_fail: str = "replan"
    description: str = ""

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "GatePredicate":
        return cls(
            gate_id=str(payload.get("gate_id") or payload.get("id") or ""),
            predicate=str(payload.get("predicate") or ""),
            inputs=tuple(str(item) for item in _sequence(payload.get("inputs"))),
            expected=payload.get("expected", True),
            operator=str(payload.get("operator") or "eq"),
            on_pass=str(payload.get("on_pass") or "advance"),
            on_fail=str(payload.get("on_fail") or "replan"),
            description=str(payload.get("description") or ""),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class ReviewRecord:
    review_id: str
    run_id: str
    branch_id: str
    plan_revision: int
    scope: str
    subject_id: str
    decision: str
    gate_results: tuple[Mapping[str, Any], ...] = ()
    evidence_ids: tuple[str, ...] = ()
    reason: str = ""
    reviewer: str = "runtime"
    created_at: str = field(default_factory=utc_now)

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ReviewRecord":
        return cls(
            review_id=str(payload.get("review_id") or f"review-{uuid4().hex}"),
            run_id=str(payload.get("run_id") or ""),
            branch_id=str(payload.get("branch_id") or "main"),
            plan_revision=max(1, _safe_int(payload.get("plan_revision"), 1)),
            scope=str(payload.get("scope") or "task"),
            subject_id=str(payload.get("subject_id") or ""),
            decision=str(payload.get("decision") or "pending"),
            gate_results=tuple(dict(item) for item in _sequence(payload.get("gate_results")) if isinstance(item, Mapping)),
            evidence_ids=tuple(str(item) for item in _sequence(payload.get("evidence_ids"))),
            reason=str(payload.get("reason") or ""),
            reviewer=str(payload.get("reviewer") or "runtime"),
            created_at=str(payload.get("created_at") or utc_now()),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class EvidenceProvenance:
    run_id: str
    branch_id: str
    plan_revision: int
    action_id: str
    attempt_id: str
    tool: str
    tool_version: str = "unknown"
    input_hash: str = ""
    output_hash: str = ""
    verifier: str = ""
    verifier_version: str = "unknown"
    fact_versions: Mapping[str, int] = field(default_factory=dict)
    parent_ids: tuple[str, ...] = ()
    target: str = ""

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "EvidenceProvenance":
        return cls(
            run_id=str(payload.get("run_id") or ""),
            branch_id=str(payload.get("branch_id") or "main"),
            plan_revision=max(1, _safe_int(payload.get("plan_revision"), 1)),
            action_id=str(payload.get("action_id") or ""),
            attempt_id=str(payload.get("attempt_id") or ""),
            tool=str(payload.get("tool") or ""),
            tool_version=str(payload.get("tool_version") or "unknown"),
            input_hash=str(payload.get("input_hash") or ""),
            output_hash=str(payload.get("output_hash") or ""),
            verifier=str(payload.get("verifier") or ""),
            verifier_version=str(payload.get("verifier_version") or "unknown"),
            fact_versions={str(key): max(0, _safe_int(value, 0)) for key, value in _mapping(payload.get("fact_versions")).items()},
            parent_ids=tuple(str(item) for item in _sequence(payload.get("parent_ids"))),
            target=str(payload.get("target") or ""),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class EvidenceNode:
    evidence_id: str
    run_id: str
    action_id: str
    artifact_type: str
    target: str
    tool: str
    payload: Any
    content_hash: str
    parent_ids: tuple[str, ...]
    verifier: str
    confidence: float
    verified: bool
    provenance: EvidenceProvenance | None = None
    trust: str = "runtime_verified"
    created_at: str = field(default_factory=utc_now)

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "EvidenceNode":
        return cls(
            evidence_id=str(payload.get("evidence_id") or ""),
            run_id=str(payload.get("run_id") or ""),
            action_id=str(payload.get("action_id") or ""),
            artifact_type=str(payload.get("artifact_type") or ""),
            target=str(payload.get("target") or ""),
            tool=str(payload.get("tool") or ""),
            payload=payload.get("payload"),
            content_hash=str(payload.get("content_hash") or ""),
            parent_ids=tuple(str(item) for item in _sequence(payload.get("parent_ids"))),
            verifier=str(payload.get("verifier") or ""),
            confidence=_safe_float(payload.get("confidence"), 0.0),
            verified=bool(payload.get("verified", False)),
            provenance=(
                EvidenceProvenance.from_dict(payload["provenance"])
                if isinstance(payload.get("provenance"), Mapping)
                else None
            ),
            trust=str(
                payload.get("trust")
                or ("runtime_verified" if payload.get("verified", False) else "unverified")
            ),
            created_at=str(payload.get("created_at") or utc_now()),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class OperationState:
    run_id: str
    session_id: str
    goal: GoalContract
    workflow_id: str
    workflow_version: int = 1
    workflow_fingerprint: str = ""
    workflow_snapshot: Mapping[str, Any] = field(default_factory=dict)
    state_version: int = 0
    branch_id: str = "main"
    plan_revision: int = 1
    plan_id: str = ""
    plan_snapshot: Mapping[str, Any] = field(default_factory=dict)
    budget: RunBudget = field(default_factory=RunBudget)
    status: str = "running"
    action_status: dict[str, str] = field(default_factory=dict)
    action_attempts: dict[str, int] = field(default_factory=dict)
    action_tools_tried: dict[str, list[str]] = field(default_factory=dict)
    action_tools_succeeded: dict[str, list[str]] = field(default_factory=dict)
    evidence_ids: list[str] = field(default_factory=list)
    current_action_id: str = ""
    terminal_reason: str = ""
    failure_reason: str = ""
    cancel_reason: str = ""
    cleanup_status: str = "not_started"
    credential_refs: list[str] = field(default_factory=list)
    dependencies: dict[str, dict[str, Any]] = field(default_factory=dict)
    created_at: str = field(default_factory=utc_now)
    updated_at: str = field(default_factory=utc_now)

    @classmethod
    def create(cls, *, session_id: str, goal: GoalContract, workflow: WorkflowSpec) -> "OperationState":
        state = cls(
            run_id=f"run-{uuid4().hex}",
            session_id=session_id,
            goal=goal,
            workflow_id=workflow.workflow_id,
            workflow_version=workflow.version,
            workflow_fingerprint=workflow.fingerprint,
            workflow_snapshot=workflow.to_dict(),
            branch_id="main",
            plan_revision=1,
            plan_id=f"plan-{workflow.fingerprint[:32]}",
            plan_snapshot={
                "plan_id": f"plan-{workflow.fingerprint[:32]}",
                "branch_id": "main",
                "revision": 1,
                "workflow": workflow.to_dict(),
            },
            action_status={action.action_id: "pending" for action in workflow.actions},
            action_attempts={action.action_id: 0 for action in workflow.actions},
            action_tools_tried={action.action_id: [] for action in workflow.actions},
            action_tools_succeeded={action.action_id: [] for action in workflow.actions},
        )
        state.budget = RunBudget.create(
            action_limit=goal.max_actions,
            started_at=state.created_at,
        )
        return state

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "OperationState":
        goal = GoalContract.from_dict(_mapping(payload.get("goal")))
        action_attempts = {
            str(key): max(0, _safe_int(value, 0))
            for key, value in _mapping(payload.get("action_attempts")).items()
        }
        created_at = str(payload.get("created_at") or utc_now())
        return cls(
            run_id=str(payload.get("run_id") or ""),
            session_id=str(payload.get("session_id") or ""),
            goal=goal,
            workflow_id=str(payload.get("workflow_id") or ""),
            workflow_version=max(1, _safe_int(payload.get("workflow_version"), 1)),
            workflow_fingerprint=str(payload.get("workflow_fingerprint") or ""),
            workflow_snapshot=dict(_mapping(payload.get("workflow_snapshot"))),
            state_version=max(0, _safe_int(payload.get("state_version"), 0)),
            branch_id=str(payload.get("branch_id") or "main"),
            plan_revision=max(1, _safe_int(payload.get("plan_revision"), 1)),
            plan_id=str(payload.get("plan_id") or ""),
            plan_snapshot=dict(_mapping(payload.get("plan_snapshot"))),
            budget=RunBudget.from_dict(
                _mapping(payload.get("budget")),
                fallback_action_limit=goal.max_actions,
                fallback_actions_used=sum(action_attempts.values()),
                fallback_started_at=created_at,
            ),
            status=str(payload.get("status") or "running"),
            action_status={str(key): str(value) for key, value in _mapping(payload.get("action_status")).items()},
            action_attempts=action_attempts,
            action_tools_tried={
                str(key): [str(item) for item in value]
                for key, value in _mapping(payload.get("action_tools_tried")).items()
                if isinstance(value, list)
            },
            action_tools_succeeded={
                str(key): [str(item) for item in value]
                for key, value in _mapping(payload.get("action_tools_succeeded")).items()
                if isinstance(value, list)
            },
            evidence_ids=[str(item) for item in _sequence(payload.get("evidence_ids"))],
            current_action_id=str(payload.get("current_action_id") or ""),
            terminal_reason=str(payload.get("terminal_reason") or ""),
            failure_reason=str(payload.get("failure_reason") or ""),
            cancel_reason=str(payload.get("cancel_reason") or ""),
            cleanup_status=str(payload.get("cleanup_status") or "not_started"),
            credential_refs=[str(item) for item in _sequence(payload.get("credential_refs"))],
            dependencies={
                str(key): dict(value)
                for key, value in _mapping(payload.get("dependencies")).items()
                if isinstance(value, Mapping)
            },
            created_at=created_at,
            updated_at=str(payload.get("updated_at") or utc_now()),
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True)
class TerminalDecision:
    terminal: bool
    success: bool
    reason: str
    satisfied: tuple[str, ...] = ()
    missing: tuple[str, ...] = ()
