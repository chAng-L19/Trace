from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from ..core import ToolResult


MAX_PROJECTION_STRING = 2048
MAX_PROJECTION_ITEMS = 24


@dataclass(frozen=True, slots=True)
class ToolObservationProjection:
    content: Mapping[str, Any]
    raw_bytes: int
    raw_sha256: str
    semantic_fields: tuple[str, ...]


class ToolObservationProjector:
    """Create bounded, model-facing projections while retaining raw CAS bytes."""

    STATUS_KEYS = ("status_code", "status", "http_status", "code")
    HEADER_KEYS = ("headers", "response_headers")
    BODY_KEYS = ("body", "response_body", "text", "content", "data")
    TIMING_KEYS = ("elapsed_ms", "duration_ms", "latency_ms", "timing", "elapsed")
    ENUM_KEYS = ("routes", "paths", "endpoints", "urls", "parameters", "assets", "results")

    def project(
        self,
        result: ToolResult,
        *,
        raw_artifact: Mapping[str, Any],
    ) -> ToolObservationProjection:
        raw = json.dumps(
            result.output,
            ensure_ascii=False,
            sort_keys=True,
            default=str,
        ).encode("utf-8")
        output = result.output
        semantic: dict[str, Any] = {}
        if isinstance(output, Mapping):
            status = self._find(output, self.STATUS_KEYS)
            if status is not None:
                semantic["status_code"] = self._bounded(status)
            headers = self._find(output, self.HEADER_KEYS)
            if isinstance(headers, Mapping):
                semantic["headers"] = self._bounded(headers)
            body = self._find(output, self.BODY_KEYS)
            if body is not None:
                body_bytes = self._render_bytes(body)
                semantic["body"] = {
                    "byte_count": len(body_bytes),
                    "sha256": hashlib.sha256(body_bytes).hexdigest(),
                    "preview": body_bytes[:MAX_PROJECTION_STRING].decode("utf-8", errors="replace"),
                    "truncated": len(body_bytes) > MAX_PROJECTION_STRING,
                }
            timing = self._find(output, self.TIMING_KEYS)
            if timing is not None:
                semantic["timing"] = self._bounded(timing)
            request = output.get("request")
            if isinstance(request, Mapping):
                semantic["request"] = self._bounded(
                    {key: request[key] for key in ("method", "url", "path", "headers") if key in request}
                )
            baseline = output.get("baseline")
            observed = output.get("observed")
            if baseline is not None or observed is not None:
                semantic["comparison"] = self._comparison(baseline, observed)
            enumeration = self._enumeration(output)
            if enumeration:
                semantic["enumeration"] = enumeration
            semantic["structured_summary"] = self._bounded(output)
        else:
            semantic["structured_summary"] = self._bounded(output)
        content = {
            "call_id": result.call_id,
            "tool_name": result.tool_name,
            "status": result.status,
            "input_hash": result.input_hash,
            "output_hash": result.output_hash,
            "retryable": result.retryable,
            "error": result.error,
            "projection": semantic,
            "output": {
                "artifact": dict(raw_artifact),
            },
            "raw": {
                "artifact": dict(raw_artifact),
                "byte_count": int(raw_artifact.get("byte_count") or len(raw)),
                "sha256": str(raw_artifact.get("content_hash") or hashlib.sha256(raw).hexdigest()),
                "output_byte_count": len(raw),
                "output_sha256": hashlib.sha256(raw).hexdigest(),
                "authority": "complete_tool_result",
            },
            "metadata": {
                **dict(result.metadata),
                "complete_output_artifact": str(raw_artifact.get("artifact_ref") or ""),
                "projection_kind": "ai_friendly_tool_observation_v1",
            },
        }
        return ToolObservationProjection(
            content=content,
            raw_bytes=len(raw),
            raw_sha256=hashlib.sha256(raw).hexdigest(),
            semantic_fields=tuple(semantic),
        )

    @classmethod
    def _find(cls, value: Mapping[str, Any], keys: Sequence[str]) -> Any:
        for key in keys:
            if key in value:
                return value[key]
        for child in value.values():
            if isinstance(child, Mapping):
                found = cls._find(child, keys)
                if found is not None:
                    return found
        return None

    @classmethod
    def _bounded(cls, value: Any, *, depth: int = 0) -> Any:
        if depth >= 4:
            return "<depth-limited>"
        if isinstance(value, str):
            return value if len(value) <= MAX_PROJECTION_STRING else value[:MAX_PROJECTION_STRING] + "..."
        if value is None or isinstance(value, (bool, int, float)):
            return value
        if isinstance(value, Mapping):
            items = list(value.items())
            projected = {
                str(key): cls._bounded(child, depth=depth + 1)
                for key, child in items[:MAX_PROJECTION_ITEMS]
            }
            if len(items) > MAX_PROJECTION_ITEMS:
                projected["_omitted_keys"] = len(items) - MAX_PROJECTION_ITEMS
            return projected
        if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
            projected = [cls._bounded(item, depth=depth + 1) for item in value[:MAX_PROJECTION_ITEMS]]
            if len(value) > MAX_PROJECTION_ITEMS:
                projected.append({"_omitted_items": len(value) - MAX_PROJECTION_ITEMS})
            return projected
        return cls._bounded(str(value), depth=depth + 1)

    @classmethod
    def _comparison(cls, baseline: Any, observed: Any) -> Mapping[str, Any]:
        result: dict[str, Any] = {
            "baseline": cls._bounded(baseline),
            "observed": cls._bounded(observed),
            "different": baseline != observed,
        }
        if isinstance(baseline, Mapping) and isinstance(observed, Mapping):
            keys = sorted(set(baseline) | set(observed))
            result["changed_fields"] = [
                str(key) for key in keys if baseline.get(key) != observed.get(key)
            ][:MAX_PROJECTION_ITEMS]
        return result

    @classmethod
    def _enumeration(cls, output: Mapping[str, Any]) -> Mapping[str, Any]:
        result: dict[str, Any] = {}
        for key in cls.ENUM_KEYS:
            value = output.get(key)
            if not isinstance(value, Sequence) or isinstance(value, (str, bytes, bytearray)):
                continue
            result[key] = {
                "count": len(value),
                "sample": cls._bounded(value[:MAX_PROJECTION_ITEMS]),
                "complete": len(value) <= MAX_PROJECTION_ITEMS,
            }
        coverage = output.get("coverage")
        if isinstance(coverage, Mapping):
            result["coverage"] = cls._bounded(coverage)
        return result

    @staticmethod
    def _render_bytes(value: Any) -> bytes:
        if isinstance(value, bytes):
            return value
        if isinstance(value, str):
            return value.encode("utf-8", errors="replace")
        return json.dumps(value, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
