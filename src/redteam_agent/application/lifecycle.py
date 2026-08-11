from __future__ import annotations

from typing import Final

from ..core import RUN_STATUSES


CANONICAL_RUN_STATUSES: Final = RUN_STATUSES

ALLOWED_RUN_TRANSITIONS: Final = {
    "created": CANONICAL_RUN_STATUSES,
    "running": frozenset(
        {"running", "waiting_worker", "paused_budget", "cancelling", "cancelled", "completed", "failed"}
    ),
    "waiting_worker": frozenset(
        {"waiting_worker", "running", "paused_budget", "cancelling", "cancelled", "completed", "failed"}
    ),
    "paused_budget": frozenset(
        {"paused_budget", "running", "waiting_worker", "cancelling", "cancelled", "completed", "failed"}
    ),
    "cancelling": frozenset({"cancelling", "waiting_worker", "cancelled", "failed"}),
    "cancelled": frozenset({"cancelled"}),
    "completed": frozenset({"completed"}),
    "failed": frozenset({"failed"}),
}


def validate_run_transition(previous: str, current: str) -> None:
    if previous not in CANONICAL_RUN_STATUSES:
        raise ValueError(f"run_status_invalid:{previous}")
    if current not in CANONICAL_RUN_STATUSES:
        raise ValueError(f"run_status_invalid:{current}")
    if current not in ALLOWED_RUN_TRANSITIONS[previous]:
        raise ValueError(f"run_transition_invalid:{previous}:{current}")
