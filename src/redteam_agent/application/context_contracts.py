from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping


@dataclass(frozen=True, slots=True)
class ContextBudget:
    """The single context-window budget used by selection and compaction."""

    window_tokens: int = 0
    reserved_output_tokens: int = 0
    keep_recent_messages: int = 32
    fallback_bytes_per_token: int = 3
    max_compaction_retries: int = 1

    @classmethod
    def from_values(
        cls,
        *,
        window_tokens: int = 0,
        reserved_output_tokens: int | None = None,
        keep_recent_messages: int = 32,
        fallback_bytes_per_token: int = 3,
        max_compaction_retries: int = 1,
    ) -> "ContextBudget":
        window = max(0, int(window_tokens))
        reserve = (
            max(0, int(reserved_output_tokens))
            if reserved_output_tokens is not None
            else (max(1024, min(32768, window // 8)) if window else 0)
        )
        return cls(
            window_tokens=window,
            reserved_output_tokens=reserve,
            keep_recent_messages=max(0, int(keep_recent_messages)),
            fallback_bytes_per_token=max(1, int(fallback_bytes_per_token)),
            max_compaction_retries=max(0, int(max_compaction_retries)),
        )

    @property
    def available_tokens(self) -> int:
        return max(0, self.window_tokens - self.reserved_output_tokens)

@dataclass(frozen=True, slots=True)
class ContextSelection:
    run_id: str
    messages: tuple[Mapping[str, Any], ...]
    protected_context: Mapping[str, Any]
    source_message_ids: tuple[str, ...]
    summary_ids: tuple[str, ...]
    source_hash: str
    protected_hash: str
    context_hash: str
    estimated_context_tokens: int = 0
    provider_context_tokens: int | None = None
    selected_tokens: int = 0
    reserved_output_tokens: int = 0
    projection_bytes: int = 0
    cache_read_tokens: int | None = None
    cache_write_tokens: int | None = None
    context_overflow_tokens: int = 0
    compaction_ids: tuple[str, ...] = ()
    overflow_retry: int = 0
    resource_index_hash: str = ""
    resource_selection_hash: str = ""
    resource_ids: tuple[str, ...] = ()
    resource_tokens: int = 0
