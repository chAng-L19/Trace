from __future__ import annotations

import hashlib
import json
import os
import re
import threading
from bisect import bisect_left
from collections.abc import Mapping
from pathlib import Path
from typing import Any


SCHEMA_VERSION = 10
MAX_HANDOFF_OBSERVATION_BYTES = 2 * 1024 * 1024


class StoreConflictError(RuntimeError):
    pass


class StateVersionConflict(StoreConflictError):
    pass


class LeaseLostError(StoreConflictError):
    pass


class ImmutableRecordError(StoreConflictError):
    pass


def _dump(value: Any) -> str:
    return json.dumps(redact_sensitive(value), ensure_ascii=False, sort_keys=True, default=str)


def _load(value: Any, default: Any = None) -> Any:
    try:
        return json.loads(str(value))
    except (json.JSONDecodeError, TypeError, ValueError):
        return default


SENSITIVE_KEY_RE = re.compile(
    r"(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|cookie|"
    r"session[_-]?token|(?:^|[_-])token(?:$|[_-]))",
    re.IGNORECASE,
)
# Runtime telemetry and budget counters contain the word ``token`` but are not
# credentials.  Redacting them changes nullable budget semantics on a
# persistence round-trip (for example ``None`` -> ``[REDACTED]`` -> ``1``), so
# keep these structural fields byte/shape stable while still redacting actual
# token, API-key, and authorization fields.
NON_SECRET_TOKEN_KEYS = frozenset(
    {
        "token_limit",
        "token_usage_missing",
        "token_usage_acknowledged",
        "tokens_used",
        "input_tokens_used",
        "output_tokens_used",
        "token_budget",
        "token_count",
        "input_tokens",
        "output_tokens",
        "total_tokens",
        "max_tokens",
        "durable_secret_material",
    }
)
SECRET_VALUE_PATTERNS = (
    (re.compile(r"\bsk-[A-Za-z0-9_-]{16,}\b"), "[REDACTED]"),
    (re.compile(
        r"(?i)\b(authorization|proxy-authorization)(\s*:\s*)"
        r"(?:(?:Bearer|Basic)\s+)?[A-Za-z0-9._~+/-]{8,}=*"
    ), r"\1\2[REDACTED]"),
    (re.compile(r"\bBearer\s+[A-Za-z0-9._~+/-]{16,}=*", re.IGNORECASE), "Bearer [REDACTED]"),
    (re.compile(
        r"(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|session[_-]?token)"
        r"(\s*[:=]\s*)[\"']?[^\s\"',;&]+"
    ), r"\1\2[REDACTED]"),
    (re.compile(r"(?i)\b(token)(\s*[:=]\s*)[\"']?[A-Za-z0-9._~+/-]{16,}=*"), r"\1\2[REDACTED]"),
    (re.compile(r"(?i)\b(cookie|set-cookie)(\s*:\s*)[^\r\n]+"), r"\1\2[REDACTED]"),
    (re.compile(
        r"(?i)([?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password)=)"
        r"[^&#\s]+"
    ), r"\1[REDACTED]"),
    (re.compile(r"(?i)(https?://)[^/@\s:]+:[^/@\s]+@"), r"\1[REDACTED]@"),
)

SECRET_REFERENCE_VERSION = "secret-reference-v1"
SECRET_REFERENCE_RE = re.compile(r"\[SECRET_REF:sha256:[0-9a-f]{64}\]")

# These expressions identify only the credential value span.  Durable goal
# state uses a deterministic, non-reversible reference for each span while the
# original value is retained exclusively in the process-local CredentialVault.
_SECRET_CAPTURE_PATTERNS = (
    re.compile(r"(?P<secret>\bsk-[A-Za-z0-9_-]{16,}\b)"),
    re.compile(
        r"(?i)\b(?:authorization|proxy-authorization)(?:\s*:\s*)"
        r"(?:(?:Bearer|Basic)\s+)?(?P<secret>[A-Za-z0-9._~+/-]{8,}=*)"
    ),
    re.compile(r"(?i)\bBearer\s+(?P<secret>[A-Za-z0-9._~+/-]{16,}=*)"),
    re.compile(
        r"(?i)\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|session[_-]?token)"
        r"(?:\s*[:=]\s*)[\"']?(?P<secret>[^\s\"',;&]+)"
    ),
    re.compile(r"(?i)\btoken(?:\s*[:=]\s*)[\"']?(?P<secret>[A-Za-z0-9._~+/-]{16,}=*)"),
    re.compile(r"(?i)\b(?:cookie|set-cookie)(?:\s*:\s*)(?P<secret>[^\r\n]+)"),
    re.compile(
        r"(?i)[?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password)="
        r"(?P<secret>[^&#\s;,)\]}]+)"
    ),
    re.compile(r"(?i)https?://(?P<secret>[^/@\s:]+:[^/@\s]+)@"),
)


def secret_reference(value: Any) -> str:
    raw = value if isinstance(value, str) else str(value)
    digest = hashlib.sha256(raw.encode("utf-8", errors="replace")).hexdigest()
    return f"[SECRET_REF:sha256:{digest}]"


def is_secret_reference(value: Any) -> bool:
    return isinstance(value, str) and SECRET_REFERENCE_RE.fullmatch(value) is not None


class EphemeralCredentialBindings(dict[str, str]):
    """Mutable process-only bindings with a value-free diagnostic repr."""

    def __repr__(self) -> str:
        return f"EphemeralCredentialBindings(count={len(self)})"

    __str__ = __repr__


def _project_text(value: str) -> tuple[str, dict[str, str]]:
    """Replace credential spans with stable references without retaining text.

    Matches are collected against the unmodified source first.  This makes the
    transformation idempotent and prevents a later expression from treating a
    reference emitted by an earlier expression as fresh secret material.
    """

    protected = tuple(match.span() for match in SECRET_REFERENCE_RE.finditer(value))
    protected_starts = tuple(start for start, _ in protected)

    def overlaps_protected(start: int, end: int) -> bool:
        insertion = bisect_left(protected_starts, end)
        return insertion > 0 and protected[insertion - 1][1] > start

    spans: dict[tuple[int, int], str] = {}
    for pattern in _SECRET_CAPTURE_PATTERNS:
        for match in pattern.finditer(value):
            start, end = match.span("secret")
            raw = match.group("secret")
            if (
                not raw
                or is_secret_reference(raw)
                or overlaps_protected(start, end)
            ):
                continue
            spans.setdefault((start, end), raw)
    if not spans:
        return value, EphemeralCredentialBindings()

    # Prefer the widest match where detectors overlap, then discard any span
    # contained by an already selected match.
    selected: list[tuple[int, int, str]] = []
    for (start, end), raw in sorted(spans.items(), key=lambda item: (item[0][0], -(item[0][1] - item[0][0]))):
        if selected and start < selected[-1][1]:
            continue
        selected.append((start, end, raw))

    bindings: dict[str, str] = EphemeralCredentialBindings()
    projected_parts: list[str] = []
    offset = 0
    for start, end, raw in selected:
        reference = secret_reference(raw)
        bindings[reference] = raw
        projected_parts.extend((value[offset:start], reference))
        offset = end
    projected_parts.append(value[offset:])
    return "".join(projected_parts), bindings


def project_sensitive(value: Any, key: str = "") -> tuple[Any, dict[str, str]]:
    """Return a durable Secret-Reference projection plus ephemeral bindings."""

    normalized_key = key.strip().casefold().replace("-", "_")
    if (
        key
        and normalized_key not in NON_SECRET_TOKEN_KEYS
        and SENSITIVE_KEY_RE.search(key)
        and value not in (None, "")
    ):
        if is_secret_reference(value):
            return value, EphemeralCredentialBindings()
        reference = secret_reference(value)
        bindings = EphemeralCredentialBindings()
        bindings[reference] = value if isinstance(value, str) else str(value)
        return reference, bindings
    if isinstance(value, Mapping):
        projected: dict[str, Any] = {}
        bindings: dict[str, str] = EphemeralCredentialBindings()
        for item_key, item_value in value.items():
            item, discovered = project_sensitive(item_value, str(item_key))
            projected[str(item_key)] = item
            bindings.update(discovered)
        return projected, bindings
    if isinstance(value, list):
        projected_items: list[Any] = []
        bindings: dict[str, str] = EphemeralCredentialBindings()
        for item_value in value:
            item, discovered = project_sensitive(item_value)
            projected_items.append(item)
            bindings.update(discovered)
        return projected_items, bindings
    if isinstance(value, tuple):
        projected_items: list[Any] = []
        bindings: dict[str, str] = EphemeralCredentialBindings()
        for item_value in value:
            item, discovered = project_sensitive(item_value)
            projected_items.append(item)
            bindings.update(discovered)
        return tuple(projected_items), bindings
    if isinstance(value, str):
        return _project_text(value)
    return value, EphemeralCredentialBindings()


def find_secret_references(value: Any) -> tuple[str, ...]:
    found: set[str] = set()

    def visit(item: Any) -> None:
        if isinstance(item, Mapping):
            for nested in item.values():
                visit(nested)
        elif isinstance(item, (list, tuple)):
            for nested in item:
                visit(nested)
        elif isinstance(item, str):
            found.update(match.group(0) for match in SECRET_REFERENCE_RE.finditer(item))

    visit(value)
    return tuple(sorted(found))


class CredentialVault:
    """Process-local credential bindings; repr and durable state expose none."""

    __slots__ = ("_bindings", "_lock")

    def __init__(self) -> None:
        self._bindings: dict[str, str] = {}
        self._lock = threading.RLock()

    def __repr__(self) -> str:
        with self._lock:
            count = len(self._bindings)
        return f"CredentialVault(bindings={count}, storage=process-local)"

    def bind_many(self, bindings: Mapping[str, Any]) -> None:
        checked: dict[str, str] = {}
        for reference, raw_value in bindings.items():
            raw = raw_value if isinstance(raw_value, str) else str(raw_value)
            if not is_secret_reference(reference) or secret_reference(raw) != reference:
                raise ValueError("credential_binding_reference_mismatch")
            checked[str(reference)] = raw
        with self._lock:
            self._bindings.update(checked)

    def missing(self, references: Any) -> tuple[str, ...]:
        required = tuple(dict.fromkeys(str(item) for item in references if is_secret_reference(item)))
        with self._lock:
            return tuple(reference for reference in required if reference not in self._bindings)

    def resolve(self, value: Any) -> Any:
        with self._lock:
            bindings = dict(self._bindings)

        def replace(item: Any) -> Any:
            if isinstance(item, Mapping):
                return {str(key): replace(nested) for key, nested in item.items()}
            if isinstance(item, list):
                return [replace(nested) for nested in item]
            if isinstance(item, tuple):
                return tuple(replace(nested) for nested in item)
            if isinstance(item, str):
                return SECRET_REFERENCE_RE.sub(lambda match: bindings.get(match.group(0), match.group(0)), item)
            return item

        return replace(value)

    def project(self, value: Any) -> Any:
        """Convert tool-channel values back to the durable reference domain."""

        with self._lock:
            bindings = sorted(self._bindings.items(), key=lambda item: len(item[1]), reverse=True)

        def replace_known(item: Any) -> Any:
            if isinstance(item, Mapping):
                return {str(key): replace_known(nested) for key, nested in item.items()}
            if isinstance(item, list):
                return [replace_known(nested) for nested in item]
            if isinstance(item, tuple):
                return tuple(replace_known(nested) for nested in item)
            if not isinstance(item, str):
                return item
            parts: list[str] = []
            offset = 0
            for existing in SECRET_REFERENCE_RE.finditer(item):
                segment = item[offset:existing.start()]
                for reference, raw in bindings:
                    if raw:
                        segment = segment.replace(raw, reference)
                parts.extend((segment, existing.group(0)))
                offset = existing.end()
            tail = item[offset:]
            for reference, raw in bindings:
                if raw:
                    tail = tail.replace(raw, reference)
            parts.append(tail)
            return "".join(parts)

        projected, discovered = project_sensitive(replace_known(value))
        self.bind_many(discovered)
        return projected


def _redacted_digest(value: Any) -> str:
    if isinstance(value, str) and value.startswith("[REDACTED"):
        return value
    digest = hashlib.sha256(str(value).encode("utf-8", errors="replace")).hexdigest()[:12]
    return f"[REDACTED sha256:{digest}]"


def redact_sensitive(value: Any, key: str = "") -> Any:
    normalized_key = key.strip().casefold().replace("-", "_")
    if key and normalized_key not in NON_SECRET_TOKEN_KEYS and SENSITIVE_KEY_RE.search(key):
        if is_secret_reference(value):
            return value
        return _redacted_digest(value)
    if isinstance(value, Mapping):
        return {str(item_key): redact_sensitive(item_value, str(item_key)) for item_key, item_value in value.items()}
    if isinstance(value, list):
        return [redact_sensitive(item) for item in value]
    if isinstance(value, tuple):
        return tuple(redact_sensitive(item) for item in value)
    if isinstance(value, str):
        parts: list[str] = []
        offset = 0
        for reference in SECRET_REFERENCE_RE.finditer(value):
            segment = value[offset:reference.start()]
            for pattern, replacement in SECRET_VALUE_PATTERNS:
                segment = pattern.sub(replacement, segment)
            parts.extend((segment, reference.group(0)))
            offset = reference.end()
        tail = value[offset:]
        for pattern, replacement in SECRET_VALUE_PATTERNS:
            tail = pattern.sub(replacement, tail)
        parts.append(tail)
        return "".join(parts)
    return value


def canonicalize_sensitive_text(value: str) -> tuple[str, dict[str, Any]]:
    """Return a deterministic Secret-Reference projection and audit metadata.

    The projection is not described as the verbatim original when credentials
    were replaced.  Raw bindings are intentionally omitted from the return
    value; callers that need the temporary tool-channel values use
    :func:`project_sensitive` and place its bindings in ``CredentialVault``.
    """

    original = value if isinstance(value, str) else str(value)
    canonical, bindings = _project_text(original)
    return canonical, {
        "applied": canonical != original,
        "representation": SECRET_REFERENCE_VERSION if bindings else "original-source",
        "original_sha256": hashlib.sha256(original.encode("utf-8")).hexdigest(),
        "original_bytes": len(original.encode("utf-8")),
        "canonical_sha256": hashlib.sha256(canonical.encode("utf-8")).hexdigest(),
        "canonical_bytes": len(canonical.encode("utf-8")),
        "credential_refs": sorted(bindings),
    }


def safe_error_text(value: Any, *, limit: int = 512) -> str:
    """Return bounded diagnostic data with credentials removed.

    Discovery and transport exceptions frequently incorporate an input URL,
    headers, or a remote response body.  Keep those strings suitable for
    durable events and JSON-RPC responses without allowing them to become a
    secret-exfiltration channel.
    """

    normalized = str(redact_sensitive(str(value)))
    bounded = max(32, int(limit))
    return normalized if len(normalized) <= bounded else f"{normalized[:bounded]}...[truncated]"


def secure_directory(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    if os.name != "nt":
        path.chmod(0o700)


def secure_file(path: Path) -> None:
    if os.name != "nt" and path.exists():
        path.chmod(0o600)
