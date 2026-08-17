from __future__ import annotations

import math
from typing import Any, Mapping

from ..core import ModelRequest, ModelResponse, ToolResult, contract_hash
from ..core.contracts import ContractError, json_mapping, json_value


class ModelIntegrityMixin:
    @staticmethod
    def _normalize_usage(usage: Mapping[str, Any]) -> dict[str, Any]:
        from .model_loop import ModelIntegrityError

        try:
            normalized = json_mapping(usage, field="model_response.usage")
        except ContractError as exc:
            raise ModelIntegrityError(f"model_usage_invalid:{exc}") from exc
        for key, value in tuple(normalized.items()):
            normalized_key = key.casefold()
            if not (
                normalized_key.endswith("_tokens")
                or normalized_key in {"prompt_tokens", "completion_tokens"}
            ):
                continue
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ModelIntegrityError(f"model_usage_invalid:{key}")
            if (
                not math.isfinite(float(value))
                or int(value) != value
                or value < 0
                or value > 2**63 - 1
            ):
                raise ModelIntegrityError(f"model_usage_invalid:{key}")
            normalized[key] = int(value)
        return normalized

    @staticmethod
    def _prompt_projection(request: ModelRequest) -> dict[str, Any]:
        return {
            "messages": [dict(item) for item in request.messages],
            "tools": [dict(item) for item in request.tools],
            "response_schema": dict(request.response_schema),
            "model": request.model,
            "allow_parallel_tools": request.allow_parallel_tools,
        }

    @staticmethod
    def _response_projection(response: ModelResponse) -> dict[str, Any]:
        projection = response.to_dict()
        projection.pop("response_hash", None)
        return projection

    @staticmethod
    def _tool_output_hash(result: ToolResult) -> str:
        return contract_hash(
            {
                "call_id": result.call_id,
                "status": result.status,
                "tool_name": result.tool_name,
                "output": json_value(result.output, field="tool_result.output"),
                "error": result.error,
                "retryable": result.retryable,
            }
        )
