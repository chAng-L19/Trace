"""Byte-bounded, navigation-preserving projections for the web search graph."""

from __future__ import annotations

import json
from typing import Any, Mapping

from ..core import contract_hash

MAX_SEARCH_RECORD_BYTES = 16 * 1024
MAX_SEARCH_RECORDS = 256
_IDENTITIES = frozenset({
    "record_id", "run_id", "hypothesis_id", "attempt_id", "lifecycle_action_id",
    "action_fingerprint",
})
_REFERENCES = frozenset({"parent_record_ids", "evidence_refs", "artifact_refs"})
_LANES = frozenset({"active", "suspended", "unresolved_contradictions"})
_NAVIGATION_FIELDS = frozenset({
    *_IDENTITIES, *_REFERENCES, "kind", "record_kind", "status", "statement",
    "target", "tool", "capabilities", "confidence", "created_at",
})


def _size(value: Any) -> int:
    return len(json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def _reference(value: Any) -> dict[str, Any]:
    return {"omitted": True, "content_hash": contract_hash(value)}


def _identity(value: str) -> str:
    # Use the same alias for node IDs and their incoming references. Ordinary
    # runtime-generated IDs are kept verbatim; aliases are only for long input.
    return value if _size(value) <= 256 else f"sha256:{contract_hash(value)}"


def _text(value: str, limit: int) -> str:
    if _size(value) <= limit:
        return value
    low, high = 0, min(len(value), limit)
    while low < high:
        middle = (low + high + 1) // 2
        if _size(value[:middle] + "…") <= limit:
            low = middle
        else:
            high = middle - 1
    return value[:low] + "…"


def _field(key: str, value: Any, *, compact: bool) -> Any:
    if isinstance(value, str):
        if key in _IDENTITIES:
            return _identity(value)
        return _text(value, 2048 if key == "statement" else 512) if compact else value
    if isinstance(value, (list, tuple)):
        result = []
        for item in value:
            projected = _identity(item) if key in _REFERENCES and isinstance(item, str) else item
            if compact and isinstance(projected, str):
                projected = _text(projected, 512)
            elif compact and _size(projected) > 512:
                projected = _reference(projected)
            if compact and _size([*result, projected]) > 1024:
                break
            result.append(projected)
        return result
    if compact and _size(value) > 512:
        return _reference(value)
    return value


def search_record_projection(record: Mapping[str, Any]) -> dict[str, Any]:
    """Keep small records compatible and cap each large record at 16 KiB.

    Large structured fields become hash references. Display text remains text,
    and shortened navigation lists remain lists. A source hash and field names
    make every omission explicit without embedding the original raw content.
    """
    source = dict(record)
    compact = _size(source) > MAX_SEARCH_RECORD_BYTES
    projected = {key: _field(key, value, compact=compact) for key, value in source.items()}
    omitted = [key for key, value in source.items() if projected[key] != value]
    if omitted:
        projected["projection_omissions"] = {"fields": omitted, "content_hash": contract_hash(source)}
    if _size(projected) <= MAX_SEARCH_RECORD_BYTES:
        return projected

    # Pathological input can spend the entire budget on keys or many fields.
    # Keep the navigable node instead of replacing the whole record by a hash.
    projected = {
        key: _field(key, value, compact=True)
        for key, value in source.items() if key in _NAVIGATION_FIELDS
    }
    projected["projection_omissions"] = {
        "fields": ["*"], "content_hash": contract_hash(source),
    }
    return projected


def search_graph_projection(graph: Mapping[str, Any]) -> dict[str, Any]:
    """Bound duplicate summary lanes as well as the new records collection.

    At most 32 entries per lane and 16 KiB per entry keep all three lanes below
    1.5 MiB; the 256 records consume at most 4 MiB of the 8 MiB response budget.
    """
    result = {}
    omitted = []
    for key, value in graph.items():
        if key in _LANES and isinstance(value, (list, tuple)):
            result[key] = [search_record_projection(item) for item in value[-32:]]
        elif key in {"recent_attempts", "repeated_action_signals"} and isinstance(value, (list, tuple)):
            result[key] = [search_record_projection(item) for item in value[-32:]]
        elif _size(value) > 4096:
            result[key] = _reference(value)
        else:
            result[key] = value
        if result[key] != value:
            omitted.append(key)
    if omitted:
        result["projection_omissions"] = {"fields": omitted, "content_hash": contract_hash(graph)}
    return result
