from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from typing import Any

from ..core import ModelRequest, ModelResponse, ToolResult, contract_hash
from ..core.contracts import ContractError, json_mapping, json_value
from ..runtime.exploration import TacticalAttemptRecord
from ..runtime.model_common import utc_now


class ModelIntegrityMixin:
    """Hash and usage checks shared by the single model loop."""

    @staticmethod
    def _normalize_usage(usage: Mapping[str, Any]) -> dict[str, Any]:
        from .model_loop import ModelIntegrityError

        try:
            normalized = json_mapping(usage, field="model_response.usage")
        except ContractError as exc:
            raise ModelIntegrityError(f"model_usage_invalid:{exc}") from exc
        for key, value in tuple(normalized.items()):
            normalized_key = key.casefold()
            if not (normalized_key.endswith("_tokens") or normalized_key in {"prompt_tokens", "completion_tokens"}):
                continue
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ModelIntegrityError(f"model_usage_invalid:{key}")
            if not math.isfinite(float(value)) or int(value) != value or value < 0 or value > 2**63 - 1:
                raise ModelIntegrityError(f"model_usage_invalid:{key}")
            normalized[key] = int(value)
        return normalized

    @staticmethod
    def _prompt_projection(request: ModelRequest) -> dict[str, Any]:
        return {"messages": [dict(item) for item in request.messages], "tools": [dict(item) for item in request.tools], "response_schema": dict(request.response_schema), "model": request.model, "allow_parallel_tools": request.allow_parallel_tools}

    @staticmethod
    def _response_projection(response: ModelResponse) -> dict[str, Any]:
        projection = response.to_dict()
        projection.pop("response_hash", None)
        return projection

    @staticmethod
    def _tool_output_hash(result: ToolResult) -> str:
        return contract_hash({"call_id": result.call_id, "status": result.status, "tool_name": result.tool_name, "output": json_value(result.output, field="tool_result.output"), "error": result.error, "retryable": result.retryable})


def tool_catalog_summary(definitions: Sequence[Any]) -> Mapping[str, Any]:
    grouped: dict[str, dict[str, Any]] = {}
    for item in definitions:
        server = grouped.setdefault(
            item.server,
            {
                "tool_count": 0,
                "capabilities": [],
                "preset": str(item.metadata.get("mcp_preset") or ""),
                "scope": str(item.metadata.get("mcp_scope") or ""),
            },
        )
        server["tool_count"] += 1
        server["capabilities"] = sorted(set(server["capabilities"]) | set(item.capabilities))
    return grouped


def handle_tool_expand(loop: Any, run_id: str, response: ModelResponse) -> bool:
    requested = response.structured_output.get("tools_expand")
    if requested is None:
        return False
    if not isinstance(requested, Sequence) or isinstance(requested, (str, bytes)):
        return False
    selectors = tuple(str(item) for item in requested if isinstance(item, str))
    if len(selectors) != len(requested):
        return False
    loop.service.expand_tools(run_id, selectors)
    return True


def record_tactical_update(
    loop: Any,
    view: Any,
    request: ModelRequest,
    response: ModelResponse,
) -> Mapping[str, Any] | None:
    update = response.structured_output.get("tactical_update")
    if not isinstance(update, Mapping):
        return None
    loop.service.exploration.record_model_update(view.run.run_id, request.request_id, update)
    return dict(update)


def record_tactical_attempts(
    loop: Any,
    view: Any,
    request: ModelRequest,
    response: ModelResponse,
    results: Sequence[ToolResult],
    *,
    artifact_ids: Mapping[str, str],
    tactical_update: Mapping[str, Any] | None,
) -> None:
    calls = {
        str(item.get("call_id") or item.get("id") or f"call-{index}"): item
        for index, item in enumerate(response.tool_calls)
        if isinstance(item, Mapping)
    }
    discover_for = getattr(loop.tools, "discover_for", None) if loop.tools is not None else None
    available = (
        discover_for(view.run.run_id, capabilities=view.missing_capabilities)
        if callable(discover_for)
        else (loop.tools.discover() if loop.tools is not None else ())
    )
    capabilities_by_tool = {item.qualified_name: tuple(item.capabilities) for item in available}
    active_hypothesis = str((tactical_update or {}).get("active_hypothesis_id") or "unscoped")
    target = view.goal.targets[0] if view.goal.targets else ""
    for result in results:
        raw_call = calls.get(result.call_id, {})
        arguments = raw_call.get("arguments") if isinstance(raw_call, Mapping) else {}
        arguments = arguments if isinstance(arguments, Mapping) else {}
        fingerprint = contract_hash({"tool_name": result.tool_name, "arguments": arguments, "target": target})
        artifact_id = str(artifact_ids.get(result.call_id) or "")
        attempt_id = "tactical-attempt-" + contract_hash(
            {"run_id": view.run.run_id, "request_id": request.request_id, "call_id": result.call_id}
        )[:32]
        attempt = TacticalAttemptRecord(
            attempt_id=attempt_id,
            run_id=view.run.run_id,
            request_id=request.request_id,
            call_id=result.call_id,
            lifecycle_action_id=view.next_action,
            action_fingerprint=fingerprint,
            status=result.status,
            payload={
                "tool": result.tool_name,
                "arguments_hash": contract_hash(arguments),
                "input_hash": result.input_hash,
                "output_hash": result.output_hash,
                "raw_artifact_ref": artifact_id,
                "error": result.error,
            },
            created_at=utc_now(),
            metadata={"reconcile": False},
        )
        saved_attempt = loop.service.runtime.record_tactical_attempt(attempt)
        record_id = "exploration-" + contract_hash({"kind": "attempt", "attempt_id": saved_attempt.attempt_id})[:32]
        loop.service.exploration.record(
            {
                "record_id": record_id,
                "run_id": view.run.run_id,
                "hypothesis_id": active_hypothesis,
                "kind": "attempt",
                "status": "observed" if result.status == "success" else "unverified",
                "statement": f"Executed {result.tool_name} for lifecycle gate {view.next_action}",
                "target": target,
                "artifact_refs": [artifact_id] if artifact_id else [],
                "tool": result.tool_name,
                "capabilities": list(capabilities_by_tool.get(result.tool_name, ())),
                "action_fingerprint": fingerprint,
                "observations": {
                    "tool_status": result.status,
                    "output_hash": result.output_hash,
                    "error": result.error,
                },
                "uncertainty": "tool execution is an observation, not a global hypothesis verdict",
                "metadata": {
                    "request_id": request.request_id,
                    "call_id": result.call_id,
                    "tactical_attempt_id": saved_attempt.attempt_id,
                },
                "created_at": saved_attempt.created_at,
            }
        )
