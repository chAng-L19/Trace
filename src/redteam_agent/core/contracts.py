from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Mapping, Sequence
from typing import Any


CORE_SCHEMA_VERSION = 1


class ContractError(ValueError):
    """Base error for invalid or unsupported core contract payloads."""


class ContractVersionError(ContractError):
    pass


def contract_version(
    payload: Mapping[str, Any],
    *,
    current: int = CORE_SCHEMA_VERSION,
    kind: str = "",
) -> int:
    declared_kind = str(payload.get("kind") or "")
    if declared_kind and kind and declared_kind != kind:
        raise ContractError(f"contract_kind_mismatch:{declared_kind}:{kind}")
    raw = payload.get("schema_version", 0)
    if isinstance(raw, bool):
        raise ContractVersionError("schema_version_invalid")
    try:
        version = int(raw)
    except (TypeError, ValueError, OverflowError) as exc:
        raise ContractVersionError("schema_version_invalid") from exc
    if version < 0:
        raise ContractVersionError("schema_version_invalid")
    if version > current:
        raise ContractVersionError(f"schema_version_unsupported:{version}:{current}")
    return version


def required_text(value: Any, field: str) -> str:
    text = str(value or "").strip()
    if not text:
        raise ContractError(f"{field}_required")
    return text


def optional_text(value: Any) -> str:
    return str(value or "").strip()


def unique_strings(value: Any) -> tuple[str, ...]:
    if isinstance(value, str) or not isinstance(value, Sequence):
        return ()
    return tuple(dict.fromkeys(str(item).strip() for item in value if str(item).strip()))


def json_value(value: Any, *, field: str = "value") -> Any:
    if value is None or isinstance(value, (str, bool, int)):
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ContractError(f"{field}_must_be_finite")
        return value
    if isinstance(value, Mapping):
        return {
            str(key): json_value(item, field=f"{field}.{key}")
            for key, item in sorted(value.items(), key=lambda pair: str(pair[0]))
        }
    if isinstance(value, (list, tuple)):
        return [json_value(item, field=f"{field}[]") for item in value]
    raise ContractError(f"{field}_must_be_json")


def json_mapping(value: Any, *, field: str = "value") -> dict[str, Any]:
    if value is None:
        return {}
    if not isinstance(value, Mapping):
        raise ContractError(f"{field}_must_be_object")
    normalized = json_value(value, field=field)
    return dict(normalized)


def bounded_int(value: Any, *, default: int, minimum: int, maximum: int, field: str) -> int:
    try:
        result = int(value)
    except (TypeError, ValueError, OverflowError):
        result = default
    if result < minimum or result > maximum:
        raise ContractError(f"{field}_out_of_range:{minimum}:{maximum}")
    return result


def bounded_float(
    value: Any,
    *,
    default: float,
    minimum: float,
    maximum: float,
    field: str,
) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError, OverflowError):
        result = default
    if not math.isfinite(result) or result < minimum or result > maximum:
        raise ContractError(f"{field}_out_of_range:{minimum}:{maximum}")
    return result


def optional_nonnegative_int(value: Any, *, field: str) -> int | None:
    if value in (None, ""):
        return None
    result = bounded_int(value, default=0, minimum=0, maximum=2**63 - 1, field=field)
    return result


def optional_positive_float(value: Any, *, field: str) -> float | None:
    if value in (None, ""):
        return None
    try:
        result = float(value)
    except (TypeError, ValueError, OverflowError) as exc:
        raise ContractError(f"{field}_invalid") from exc
    if not math.isfinite(result) or result <= 0:
        raise ContractError(f"{field}_must_be_positive_finite")
    return result


def versioned_payload(kind: str, payload: Mapping[str, Any]) -> dict[str, Any]:
    return {
        **json_mapping(payload, field=kind),
        "schema_version": CORE_SCHEMA_VERSION,
        "kind": kind,
    }


def canonical_json(value: Any) -> bytes:
    normalized = json_value(value)
    return json.dumps(
        normalized,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def contract_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()
