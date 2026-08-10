from __future__ import annotations

import json
from typing import Any

from .security import redact_sensitive

SCHEMA_VERSION = 4
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
    """Serialize a redacted durable projection.

    Prompt hashes, byte counts, and clause identifiers remain available for
    integrity checks. Raw credential material never regains precedence after
    recursive redaction, including when it appeared inside the objective.
    """
    return json.dumps(redact_sensitive(value), ensure_ascii=False, sort_keys=True, default=str)

def _load(value: Any, default: Any = None) -> Any:
    try:
        return json.loads(str(value))
    except (json.JSONDecodeError, TypeError, ValueError):
        return default

