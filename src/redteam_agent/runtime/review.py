from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any, Callable, Mapping, Sequence
from uuid import uuid4

from .models import GatePredicate, ReviewRecord


class GateEvaluationError(ValueError):
    pass


@dataclass(frozen=True)
class GateResult:
    gate_id: str
    passed: bool
    actual: Any
    expected: Any
    operator: str
    decision: str
    reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _resolve(context: Mapping[str, Any], reference: str) -> Any:
    if reference in context:
        return context[reference]
    current: Any = context
    for part in reference.split("."):
        if isinstance(current, Mapping) and part in current:
            current = current[part]
        else:
            return None
    return current


def _compare(actual: Any, operator: str, expected: Any) -> bool:
    if operator == "exists":
        return actual not in (None, False, "", [], {}, ())
    if operator == "eq":
        return actual == expected
    if operator == "ne":
        return actual != expected
    if operator == "contains":
        try:
            return expected in actual
        except TypeError:
            return False
    if operator == "in":
        try:
            return actual in expected
        except TypeError:
            return False
    try:
        if operator == "gte":
            return actual >= expected
        if operator == "gt":
            return actual > expected
        if operator == "lte":
            return actual <= expected
        if operator == "lt":
            return actual < expected
    except TypeError:
        return False
    raise GateEvaluationError(f"gate_operator_unknown:{operator}")


class ReviewEngine:
    def __init__(self, predicates: Mapping[str, Callable[[Sequence[Any], Mapping[str, Any]], Any]] | None = None) -> None:
        self.predicates = dict(predicates or {})

    def evaluate_gate(self, gate: GatePredicate, context: Mapping[str, Any]) -> GateResult:
        values = tuple(_resolve(context, item) for item in gate.inputs)
        handler = self.predicates.get(gate.predicate)
        if handler is not None:
            actual = handler(values, context)
        elif gate.predicate in context:
            actual = context[gate.predicate]
        elif len(values) == 1:
            actual = values[0]
        else:
            actual = values
        passed = _compare(actual, gate.operator, gate.expected)
        return GateResult(
            gate_id=gate.gate_id,
            passed=passed,
            actual=actual,
            expected=gate.expected,
            operator=gate.operator,
            decision=gate.on_pass if passed else gate.on_fail,
            reason=gate.description,
        )

    def review(
        self,
        *,
        run_id: str,
        branch_id: str,
        plan_revision: int,
        scope: str,
        subject_id: str,
        gates: Sequence[GatePredicate],
        context: Mapping[str, Any],
        evidence_ids: Sequence[str] = (),
        reviewer: str = "runtime",
    ) -> ReviewRecord:
        results = tuple(self.evaluate_gate(gate, context) for gate in gates)
        failures = tuple(item for item in results if not item.passed)
        if not failures:
            decision = "pass"
            reason = "all_gates_passed"
        elif any(item.decision in {"abort", "fail", "stop"} for item in failures):
            decision = "fail"
            reason = "gate_failed"
        else:
            decision = "replan"
            reason = "gate_replan_required"
        return ReviewRecord(
            review_id=f"review-{uuid4().hex}",
            run_id=run_id,
            branch_id=branch_id or "main",
            plan_revision=max(1, int(plan_revision)),
            scope=scope,
            subject_id=subject_id,
            decision=decision,
            gate_results=tuple(item.to_dict() for item in results),
            evidence_ids=tuple(dict.fromkeys(str(item) for item in evidence_ids if str(item))),
            reason=reason,
            reviewer=reviewer,
        )


__all__ = ["GateEvaluationError", "GateResult", "ReviewEngine"]
