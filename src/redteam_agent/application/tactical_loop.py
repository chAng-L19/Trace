from __future__ import annotations

from typing import Any, Mapping, Sequence

from ..core import ModelRequest, ModelResponse, ToolResult, contract_hash
from ..runtime.exploration_records import TacticalAttemptRecord
from ..runtime.model_common import utc_now
from .contracts import AgentRunView


class TacticalLoopMixin:
    """Model-led tactical ledger and per-tool budget accounting."""

    service: Any
    tools: Any

    def _record_tactical_update(
        self,
        view: AgentRunView,
        request: ModelRequest,
        response: ModelResponse,
    ) -> Mapping[str, Any] | None:
        update = response.structured_output.get("tactical_update")
        if not isinstance(update, Mapping):
            return None
        self.service.exploration.record_model_update(
            view.run.run_id,
            request.request_id,
            update,
        )
        return dict(update)

    def _record_tactical_attempts(
        self,
        view: AgentRunView,
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
        capabilities_by_tool = {
            item.qualified_name: tuple(item.capabilities)
            for item in (self.tools.discover() if self.tools is not None else ())
        }
        active_hypothesis = str(
            (tactical_update or {}).get("active_hypothesis_id") or "unscoped"
        )
        target = view.goal.targets[0] if view.goal.targets else ""
        for result in results:
            raw_call = calls.get(result.call_id, {})
            arguments = raw_call.get("arguments") if isinstance(raw_call, Mapping) else {}
            arguments = arguments if isinstance(arguments, Mapping) else {}
            fingerprint = contract_hash(
                {
                    "tool_name": result.tool_name,
                    "arguments": arguments,
                    "target": target,
                }
            )
            artifact_id = str(artifact_ids.get(result.call_id) or "")
            attempt_id = "tactical-attempt-" + contract_hash(
                {
                    "run_id": view.run.run_id,
                    "request_id": request.request_id,
                    "call_id": result.call_id,
                }
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
            saved_attempt = self.service.runtime.record_tactical_attempt(attempt)
            record_id = "exploration-" + contract_hash(
                {"kind": "attempt", "attempt_id": saved_attempt.attempt_id}
            )[:32]
            self.service.exploration.record(
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
                    "uncertainty": (
                        "tool execution is an observation, not a global hypothesis verdict"
                    ),
                    "metadata": {
                        "request_id": request.request_id,
                        "call_id": result.call_id,
                        "tactical_attempt_id": saved_attempt.attempt_id,
                    },
                    "created_at": saved_attempt.created_at,
                }
            )
