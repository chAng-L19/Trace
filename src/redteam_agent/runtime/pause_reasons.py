from __future__ import annotations

"""Shared pause reason vocabulary used by lifecycle and runtime facades."""

OPERATOR_PAUSE_REASONS = frozenset({"user_requested", "operator_pause"})
BUDGET_PAUSE_REASONS = frozenset(
    {
        "action_limit_exhausted",
        "token_limit_exhausted",
        "token_usage_unknown",
        "time_limit_exhausted",
        "cycle_action_limit",
    }
)

__all__ = ["BUDGET_PAUSE_REASONS", "OPERATOR_PAUSE_REASONS"]
