"""Compatibility import path for the canonical terminal result projection."""

from .terminal_judge import (
    ARTIFACT_PHASES, MAX_INLINE_EVIDENCE_BYTES, TERMINAL_FAILURE_STATUSES,
    OperationResult, _goal_contract_payload,
)

__all__ = ["ARTIFACT_PHASES", "MAX_INLINE_EVIDENCE_BYTES", "TERMINAL_FAILURE_STATUSES", "OperationResult", "_goal_contract_payload"]
