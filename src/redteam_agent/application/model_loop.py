from __future__ import annotations
import threading
from collections.abc import Mapping, Sequence
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from typing import Any
from uuid import uuid4
from ..core import (
    ModelPort,
    ModelRequest,
    ModelResponse,
    ToolCall,
    ToolPort,
    ToolResult,
    contract_hash,
)
from ..core.contracts import json_mapping
from ..runtime.model_common import utc_now
from ..runtime.artifact_store import ArtifactIntegrityError
from ..runtime.security import safe_error_text
from ..runtime.conversation_records import DiagnosticArtifactRecord
from .contracts import AgentRunView
from ..runtime.session_journal import ModelObservationRecord, ModelRequestRecord, ModelResponseRecord
from .agent_loop_support import (
    ModelIntegrityMixin,
    record_tactical_attempts,
    record_tactical_update,
    tool_catalog_summary,
)
from .bounded_output import BoundedOutput
from .model_turn import run_model_turn
from .model_cycle import run_model_cycles
from .model_stream import MAX_INLINE_MODEL_STREAM_BYTES, invoke_model_stream
MAX_INLINE_MODEL_OBSERVATION_BYTES = 64 * 1024
class ModelLoopError(RuntimeError):
    pass
class ModelIntegrityError(ModelLoopError):
    pass
class ModelInterruptedError(ModelLoopError):
    pass
class ModelContextBudgetError(ModelInterruptedError):
    pass
class AgentLoop(ModelIntegrityMixin):
    """Single model-led loop from context selection through verified observation."""
    _integrity_error = ModelIntegrityError
    _interrupted_error = ModelInterruptedError
    _loop_error = ModelLoopError
    def __init__(
        self,
        *,
        service: Any,
        model: ModelPort,
        tools: ToolPort | None = None,
        model_name: str = "",
        streaming: bool = False,
        max_retries: int = 2,
        max_turns: int = 8,
    ) -> None:
        self.service = service
        self.model = model
        self.tools = tools
        self.model_name = model_name.strip()
        self.streaming = bool(streaming)
        self.max_retries = max(0, int(max_retries))
        self.max_turns = max(1, int(max_turns))
        self._cancelled_runs: set[str] = set()
        self._interrupted_runs: set[str] = set()
        self._active_requests: dict[str, set[str]] = {}
        self._active_calls: dict[str, set[str]] = {}
        self._lock = threading.RLock()
    def run(self, run_id: str, *, max_actions: int | None = None,
            run_until_pause: bool = True, max_cycles: int = 32) -> AgentRunView:
        return run_model_cycles(self, run_id, max_actions=max_actions,
                                run_until_pause=run_until_pause, max_cycles=max_cycles)
    def interrupt(self, run_id: str) -> None:
        with self._lock:
            self._interrupted_runs.add(run_id)
            requests = tuple(self._active_requests.get(run_id, ()))
            calls = tuple(self._active_calls.get(run_id, ()))
        for request_id in requests:
            try:
                self.model.cancel(request_id)
            except Exception as exc:
                self.service.runtime.store.append_event(run_id, "model_cancel_failed", {
                    "request_id": request_id, "error_type": type(exc).__name__, "status": "unknown",
                })
        if self.tools is not None:
            for call_id in calls:
                try:
                    self.tools.cancel(call_id)
                except Exception as exc:
                    self.service.runtime.store.append_event(run_id, "model_tool_cancel_failed", {
                        "call_id": call_id, "error_type": type(exc).__name__, "status": "unknown",
                    })
    def resume(self, run_id: str) -> None:
        with self._lock:
            self._interrupted_runs.discard(run_id)
    def cancel(self, run_id: str) -> None:
        with self._lock:
            self._cancelled_runs.add(run_id)
        self.interrupt(run_id)
    def _is_interrupted(self, run_id: str) -> bool:
        with self._lock:
            return run_id in self._interrupted_runs
    def _record_tactical_update(
        self,
        view: AgentRunView,
        request: ModelRequest,
        response: ModelResponse,
    ) -> Mapping[str, Any] | None:
        return record_tactical_update(self, view, request, response)
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
        record_tactical_attempts(
            self,
            view,
            request,
            response,
            results,
            artifact_ids=artifact_ids,
            tactical_update=tactical_update,
        )
    def _model_turn(self, view: AgentRunView) -> tuple[ModelResponse, ModelRequest]:
        return run_model_turn(self, view)
    def _request(
        self,
        view: AgentRunView,
        *,
        attempt: int,
        force_compaction: bool = False,
        overflow_retry: int = 0,
    ) -> ModelRequest:
        capabilities = self.model.capabilities()
        model_name = self.model_name or str(capabilities.metadata.get("model") or "")
        catalog = None
        definitions = ()
        if self.tools is not None:
            catalog_builder = getattr(self.tools, "catalog", None)
            if callable(catalog_builder):
                catalog = catalog_builder(view.run.run_id, capabilities=view.missing_capabilities)
                definitions = tuple(sorted(catalog.tools, key=lambda item: item.qualified_name))
            else:
                definitions = tuple(sorted(self.tools.discover(), key=lambda item: item.qualified_name))
        tool_catalog = tool_catalog_summary(definitions)
        prompt_builder = getattr(self.tools, "prompt_definitions", None)
        tools = (
            tuple(prompt_builder(catalog))
            if catalog is not None and callable(prompt_builder)
            else tuple(
                {
                    "type": "function",
                    "name": item.qualified_name,
                    "description": item.description,
                    "input_schema": dict(item.input_schema),
                }
                for item in definitions
            )
        )
        response_schema = {
                "type": "object",
                "properties": {
                    "decision": {"type": "string"},
                    "reason": {"type": "string"},
                    "commit_lifecycle_gate": {"type": "boolean"},
                    "tactical_update": {
                        "type": "object",
                        "properties": {
                            "active_hypothesis_id": {"type": "string"},
                            "records": {"type": "array", "items": {"type": "object"}},
                        },
                        "additionalProperties": True,
                    },
                    "tools_expand": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "Request omitted tools by qualified-name glob; use [] for all tools.",
                    },
                },
                "additionalProperties": True,
            }
        selection = self.service.context_selector.prepare_model_context(
            view,
            max_context_tokens=capabilities.max_context_tokens,
            force_compaction=force_compaction,
            overflow_retry=overflow_retry,
            tools=tools,
            response_schema=response_schema,
        )
        if selection.context_overflow_tokens:
            self.service.runtime.pause_run(view.run.run_id, reason=selection.context_status)
            raise ModelContextBudgetError(f"{selection.context_status}:{selection.context_hash}")
        return ModelRequest(
            request_id=f"model-request-{uuid4().hex}",
            run_id=view.run.run_id,
            messages=selection.messages,
            tools=tools,
            response_schema=response_schema,
            model=model_name,
            allow_parallel_tools=capabilities.parallel_tool_calls,
            metadata={
                "action_id": view.next_action,
                "branch_id": view.run.branch_id,
                "plan_revision": int(view.run.metadata.get("plan_revision") or 1),
                "handoff_id": str(view.handoff.get("handoff_id") or ""),
                "attempt": attempt,
                "context_hash": selection.context_hash,
                "estimated_context_tokens": selection.estimated_context_tokens,
                "message_tokens": selection.message_tokens,
                "tool_schema_tokens": selection.tool_schema_tokens,
                "response_schema_tokens": selection.response_schema_tokens,
                "context_status": selection.context_status,
                "provider_context_tokens": selection.provider_context_tokens,
                "selected_tokens": selection.selected_tokens,
                "reserved_output_tokens": selection.reserved_output_tokens,
                "projection_bytes": selection.projection_bytes,
                "cache_read_tokens": selection.cache_read_tokens,
                "cache_write_tokens": selection.cache_write_tokens,
                "context_overflow_tokens": selection.context_overflow_tokens,
                "context_compaction_ids": list(selection.compaction_ids),
                "context_overflow_retry": max(0, int(overflow_retry)),
                "resource_index_hash": selection.resource_index_hash,
                "resource_selection_hash": selection.resource_selection_hash,
                "resource_ids": list(selection.resource_ids),
                "resource_tokens": selection.resource_tokens,
                "tool_catalog_total": len(definitions),
                "tool_catalog_revision": catalog.revision if catalog is not None else "",
                "tool_catalog_expanded": catalog.expanded if catalog is not None else False,
                "tool_catalog": tool_catalog,
            },
        )
    def _save_request(self, request: ModelRequest) -> None:
        capabilities = self.model.capabilities()
        provider = str(capabilities.metadata.get("provider") or type(self.model).__name__)
        self.service.runtime.store.save_model_request(
            ModelRequestRecord(
                request_id=request.request_id,
                run_id=request.run_id,
                prompt_hash=contract_hash(self._prompt_projection(request)),
                provider=provider,
                model=request.model,
                capabilities=capabilities.to_dict(),
                request=request.to_dict(),
                created_at=utc_now(),
            )
        )
        self.service.conversation.record_model_request(request)
    def _invoke(self, request: ModelRequest) -> ModelResponse:
        capabilities = self.model.capabilities()
        if self.streaming:
            if not capabilities.streaming:
                raise ModelLoopError("model_streaming_not_supported")
            return self._invoke_stream(request)
        return self.model.complete(request)
    def _invoke_stream(self, request: ModelRequest) -> ModelResponse:
        return invoke_model_stream(self, request)
    def _validate_response(self, request: ModelRequest, response: ModelResponse) -> ModelResponse:
        if response.request_id != request.request_id:
            raise ModelIntegrityError("model_response_request_mismatch")
        usage = self._normalize_usage(response.usage)
        normalized = replace(response, usage=usage)
        claimed_hash = normalized.response_hash
        authoritative_hash = contract_hash(self._response_projection(normalized))
        status = normalized.status
        call_ids = [
            str(item.get("call_id") or item.get("id") or f"call-{index}").strip()
            for index, item in enumerate(normalized.tool_calls)
        ]
        duplicate_call_id = len(call_ids) != len(set(call_ids))
        if claimed_hash and claimed_hash != authoritative_hash:
            status = "integrity_error"
        if duplicate_call_id:
            status = "integrity_error"
        record = ModelResponseRecord(
            request_id=request.request_id,
            run_id=request.run_id,
            status=status,
            provider=normalized.provider or type(self.model).__name__,
            model=normalized.model or request.model,
            response_hash=authoritative_hash,
            claimed_response_hash=claimed_hash,
            usage=usage,
            response=normalized.to_dict(),
            created_at=utc_now(),
        )
        self.service.runtime.store.save_model_response(record)
        # Provider work has already consumed budget even when its claimed hash
        # is invalid. Account usage before surfacing the integrity failure.
        self._account_response_usage(request, usage)
        if status == "integrity_error":
            if duplicate_call_id:
                raise ModelIntegrityError("model_tool_call_id_duplicate")
            raise ModelIntegrityError("model_response_hash_mismatch")
        if normalized.status not in {"completed", "success"}:
            raise ModelLoopError(f"model_response_failed:{normalized.status}:{normalized.error}")
        self.service.conversation.record_model_response(request.run_id, normalized)
        return replace(normalized, response_hash=authoritative_hash)
    def _account_response_usage(self, request: ModelRequest, usage: Mapping[str, Any]) -> None:
        current = self.service.status(request.run_id)
        if not current.terminal.terminal:
            self.service._record_model_usage(request.run_id, request.request_id, usage)
    def _save_failure_response(self, request: ModelRequest, error: BaseException) -> None:
        thread = threading.current_thread()
        accumulator = thread.__dict__.pop("model_partial_stream", None)
        partial_usage = thread.__dict__.pop("model_partial_usage", None)
        usage = (
            dict(partial_usage[1])
            if isinstance(partial_usage, tuple) and partial_usage[0] == request.request_id
            else {}
        )
        partial_text = ""
        partial_artifact: Mapping[str, Any] = {}
        try:
            if any(
                item.request_id == request.request_id
                for item in self.service.runtime.store.model_responses(request.run_id)
            ):
                return
            if usage:
                self._account_response_usage(request, usage)
            if isinstance(accumulator, BoundedOutput):
                if accumulator.byte_count <= MAX_INLINE_MODEL_STREAM_BYTES:
                    partial_text = accumulator.inline_text()
                else:
                    accumulator.close()
                    artifact = self.service.runtime.artifacts.put_file(
                        accumulator.path,
                        run_id=request.run_id,
                        artifact_type="partial_model_stream",
                        media_type="text/plain; charset=utf-8",
                        preview=accumulator.preview(),
                        metadata={"request_id": request.request_id, "complete": False},
                    )
                    partial_artifact = self.service.runtime.artifacts.project(artifact)
        finally:
            if isinstance(accumulator, BoundedOutput):
                accumulator.discard()
        diagnostic_id = ""
        safe_error = safe_error_text(error)
        if partial_text or partial_artifact:
            payload = {
                "request_id": request.request_id,
                "partial_text": partial_text,
                "partial_artifact": dict(partial_artifact),
                "error": safe_error,
                "promoted": False,
            }
            diagnostic_id = "diagnostic-" + contract_hash(
                {"request_id": request.request_id, "type": "partial_model_stream"}
            )[:32]
            self.service.runtime.store.save_diagnostic_artifact(
                DiagnosticArtifactRecord(
                    artifact_id=diagnostic_id,
                    run_id=request.run_id,
                    artifact_type="partial_model_stream",
                    source_id=request.request_id,
                    content_hash=contract_hash(payload),
                    payload=payload,
                    created_at=utc_now(),
                )
            )
        response = ModelResponse(
            request_id=request.request_id,
            status="interrupted" if isinstance(error, ModelInterruptedError) or self._is_interrupted(request.run_id) else "failed",
            provider=type(self.model).__name__,
            model=request.model,
            text="",
            error=safe_error,
            usage=usage,
            metadata={"diagnostic_artifact_id": diagnostic_id} if diagnostic_id else {},
        )
        projection = response.to_dict()
        projection.pop("response_hash", None)
        self.service.runtime.store.save_model_response(
            ModelResponseRecord(
                request_id=request.request_id,
                run_id=request.run_id,
                status=response.status,
                provider=response.provider,
                model=response.model,
                response_hash=contract_hash(projection),
                claimed_response_hash="",
                usage=usage,
                response=response.to_dict(),
                created_at=utc_now(),
            )
        )
    def _execute_tool_calls(
        self,
        view: AgentRunView,
        request: ModelRequest,
        response: ModelResponse,
        *,
        existing: Mapping[str, ToolResult] | None = None,
        reconcile: bool = False,
    ) -> tuple[ToolResult, ...]:
        if self.tools is None:
            raise ModelLoopError("model_tool_port_required")
        self._ensure_executable(view.run.run_id)
        calls = tuple(self._tool_call(view, request, item, index) for index, item in enumerate(response.tool_calls))
        if len(calls) != len({call.call_id for call in calls}):
            raise ModelIntegrityError("model_tool_call_id_duplicate")
        cached = dict(existing or {})
        pending = tuple(call for call in calls if call.call_id not in cached)
        parallel = (
            len(pending) > 1
            and request.allow_parallel_tools
            and self.model.capabilities().parallel_tool_calls
        )
        if parallel:
            with ThreadPoolExecutor(max_workers=len(pending), thread_name_prefix="model-tool") as pool:
                invoked = tuple(
                    pool.map(
                        lambda call: self._invoke_tool(
                            view,
                            request,
                            call,
                            reconcile=reconcile,
                        ),
                        pending,
                    )
                )
        else:
            invoked = tuple(
                self._invoke_tool(view, request, call, reconcile=reconcile)
                for call in pending
            )
        cached.update((item.call_id, item) for item in invoked)
        return tuple(cached[call.call_id] for call in calls)
    def _ensure_executable(self, run_id: str) -> None:
        if self._is_cancelled(run_id):
            raise ModelInterruptedError("model_loop_cancelled")
        if self._is_interrupted(run_id):
            raise ModelInterruptedError("model_loop_interrupted")
        status = self.service.status(run_id).run.status
        if status != "waiting_worker":
            raise ModelInterruptedError(f"model_loop_not_executable:{status}")
    def _tool_call(
        self,
        view: AgentRunView,
        request: ModelRequest,
        payload: Mapping[str, Any],
        index: int,
    ) -> ToolCall:
        call_id = str(payload.get("call_id") or payload.get("id") or f"call-{index}").strip()
        tool_name = str(payload.get("tool_name") or payload.get("name") or payload.get("tool") or "").strip()
        if not call_id or not tool_name:
            raise ModelIntegrityError("model_tool_call_identity_required")
        arguments = json_mapping(payload.get("arguments"), field="model_tool_call.arguments")
        cancellation_id = "tool-exec-" + contract_hash(
            {"run_id": view.run.run_id, "request_id": request.request_id, "call_id": call_id}
        )[:32]
        return ToolCall(
            call_id=call_id,
            run_id=view.run.run_id,
            tool_name=tool_name,
            arguments=arguments,
            idempotency_key=contract_hash(
                {
                    "run_id": view.run.run_id,
                    "request_id": request.request_id,
                    "call_id": call_id,
                    "tool_name": tool_name,
                    "arguments": arguments,
                }
            ),
            metadata={
                "action_id": view.next_action,
                "request_id": request.request_id,
                "cancellation_id": cancellation_id,
            },
        )
    def _invoke_tool(
        self,
        view: AgentRunView,
        request: ModelRequest,
        call: ToolCall,
        *,
        reconcile: bool = False,
    ) -> ToolResult:
        assert self.tools is not None
        self._ensure_executable(view.run.run_id)
        cancellation_id = str(call.metadata.get("cancellation_id") or call.call_id)
        with self._lock:
            if view.run.run_id in self._cancelled_runs or view.run.run_id in self._interrupted_runs:
                raise ModelInterruptedError("model_loop_interrupted")
            self._active_calls.setdefault(view.run.run_id, set()).add(cancellation_id)
        try:
            result = self.tools.reconcile(call) if reconcile else None
            if result is None:
                result = self.tools.invoke(call)
        finally:
            self._track(self._active_calls, view.run.run_id, cancellation_id, add=False)
        if result.call_id != call.call_id or result.tool_name != call.tool_name:
            raise ModelIntegrityError("tool_result_identity_mismatch")
        input_hash = contract_hash(call.to_dict())
        output_hash = self._tool_output_hash(result)
        mismatch = bool(
            (result.input_hash and result.input_hash != input_hash)
            or (result.output_hash and result.output_hash != output_hash)
        )
        normalized = replace(result, input_hash=input_hash, output_hash=output_hash)
        observation_id = "model-observation-" + contract_hash(
            {"request_id": request.request_id, "call_id": call.call_id}
        )[:32]
        durable_observation = {"tool_result": normalized.to_dict()}
        artifact_id = ""
        bounded = BoundedOutput.capture_json(normalized.to_dict())
        try:
            if bounded.byte_count > MAX_INLINE_MODEL_OBSERVATION_BYTES:
                bounded.close()
                artifact = self.service.runtime.artifacts.put_file(
                    bounded.path,
                    run_id=view.run.run_id,
                    artifact_type="model_observation_tool_result",
                    media_type="application/json",
                    preview={
                        "request_id": request.request_id,
                        "call_id": call.call_id,
                        "tool_name": call.tool_name,
                        "status": normalized.status,
                        "output_hash": output_hash,
                        **bounded.preview(),
                    },
                    metadata={"action_id": view.next_action},
                )
                artifact_id = artifact.artifact_id
                durable_observation = {
                    "tool_result_artifact": self.service.runtime.artifacts.project(artifact)
                }
        finally:
            bounded.discard()
        self.service.runtime.store.save_model_observation(
            ModelObservationRecord(
                observation_id=observation_id,
                request_id=request.request_id,
                run_id=view.run.run_id,
                action_id=view.next_action,
                call_id=call.call_id,
                tool_name=call.tool_name,
                status="integrity_error" if mismatch else result.status,
                input_hash=input_hash,
                output_hash=output_hash,
                observation=durable_observation,
                created_at=utc_now(),
                metadata={
                    "claimed_input_hash": result.input_hash,
                    "claimed_output_hash": result.output_hash,
                    "complete_result_artifact": artifact_id,
                },
            )
        )
        if mismatch:
            raise ModelIntegrityError("tool_result_hash_mismatch")
        return normalized
    def _recover_pending_turn(
        self,
        view: AgentRunView,
    ) -> tuple[ModelResponse, ModelRequest, Mapping[str, ToolResult]] | None:
        store = self.service.runtime.store
        journal = self.service.journal
        requests = {item.request_id: item for item in journal.model_requests(view.run.run_id)}
        observations = journal.model_observations(view.run.run_id)
        by_request: dict[str, dict[str, ToolResult]] = {}
        for record in observations:
            if record.action_id != view.next_action:
                continue
            if record.status == "integrity_error":
                raise ModelIntegrityError("model_observation_integrity_error")
            raw = record.observation.get("tool_result")
            if not isinstance(raw, Mapping):
                artifact = record.observation.get("tool_result_artifact")
                if isinstance(artifact, Mapping):
                    artifact_id = str(artifact.get("artifact_ref") or "")
                    try:
                        loaded = self.service.runtime.artifacts.read_json(
                            artifact_id,
                            run_id=view.run.run_id,
                        )
                    except (ArtifactIntegrityError, KeyError, ValueError) as exc:
                        raise ModelIntegrityError("model_observation_artifact_invalid") from exc
                    raw = loaded if isinstance(loaded, Mapping) else None
            if not isinstance(raw, Mapping):
                raise ModelIntegrityError("model_observation_tool_result_missing")
            result = ToolResult.from_dict(raw)
            if (
                result.call_id != record.call_id
                or result.tool_name != record.tool_name
                or result.input_hash != record.input_hash
                or result.output_hash != record.output_hash
                or self._tool_output_hash(result) != record.output_hash
            ):
                raise ModelIntegrityError("model_observation_integrity_mismatch")
            by_request.setdefault(record.request_id, {})[record.call_id] = result
        consumed_requests = {
            str(item["payload"].get("request_id") or "")
            for item in journal.operation_events(view.run.run_id)
            if item["event_type"] == "model_turn_consumed"
        }
        for response_record in reversed(journal.model_responses(view.run.run_id)):
            if response_record.status not in {"completed", "success"}:
                continue
            request_record = requests.get(response_record.request_id)
            if request_record is None:
                raise ModelIntegrityError("model_response_request_record_missing")
            request = ModelRequest.from_dict(request_record.request)
            if str(request.metadata.get("action_id") or "") != view.next_action:
                continue
            if contract_hash(self._prompt_projection(request)) != request_record.prompt_hash:
                raise ModelIntegrityError("model_request_prompt_hash_mismatch")
            response = ModelResponse.from_dict(response_record.response)
            if contract_hash(self._response_projection(response)) != response_record.response_hash:
                raise ModelIntegrityError("model_response_record_hash_mismatch")
            if not response.tool_calls:
                continue
            if request.request_id in consumed_requests:
                continue
            return response, request, by_request.get(request.request_id, {})
        return None

    def _mark_turn_consumed(
        self,
        view: AgentRunView,
        request: ModelRequest,
        reason: str,
    ) -> None:
        self.service.runtime.store.append_event(
            view.run.run_id,
            "model_turn_consumed",
            {
                "request_id": request.request_id,
                "action_id": view.next_action,
                "branch_id": view.run.branch_id,
                "reason": reason,
            },
        )
    @staticmethod
    def _target_from_results(results: Sequence[ToolResult]) -> str:
        for result in results:
            output = result.output
            if isinstance(output, str) and output.strip():
                return output.strip()
            if isinstance(output, Mapping):
                target = str(output.get("target") or "").strip()
                if target:
                    return target
                targets = output.get("targets")
                if isinstance(targets, Sequence) and not isinstance(targets, (str, bytes)):
                    values = tuple(str(item).strip() for item in targets if str(item).strip())
                    if len(values) == 1:
                        return values[0]
        raise ModelLoopError("provide_target_tool_result_missing_target")
    def _is_cancelled(self, run_id: str) -> bool:
        with self._lock:
            return run_id in self._cancelled_runs
    def _track(self, registry: dict[str, set[str]], run_id: str, item: str, *, add: bool) -> None:
        with self._lock:
            bucket = registry.setdefault(run_id, set())
            if add:
                bucket.add(item)
            else:
                bucket.discard(item)
                if not bucket:
                    registry.pop(run_id, None)
ModelLoop = AgentLoop
__all__ = [
    "AgentLoop",
    "ModelIntegrityError",
    "ModelInterruptedError",
    "ModelLoop",
    "ModelLoopError",
]
