from __future__ import annotations

import json
import hashlib
import threading
from collections.abc import Mapping, Sequence
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from typing import Any
from uuid import uuid4

from ..core import (
    ModelCapabilities,
    ModelPort,
    ModelRequest,
    ModelResponse,
    ModelStreamEvent,
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
from .contracts import AgentRunView, Observation
from ..runtime.model_records import ModelObservationRecord, ModelRequestRecord, ModelResponseRecord
from .model_integrity import ModelIntegrityMixin
from .agent_loop_support import (
    handle_tool_expand,
    record_tactical_attempts,
    record_tactical_update,
    tool_catalog_summary,
)
from .stream_accumulator import StreamTextAccumulator

MAX_INLINE_MODEL_OBSERVATION_BYTES = 64 * 1024
MAX_INLINE_MODEL_STREAM_BYTES = 64 * 1024

class ModelLoopError(RuntimeError):
    pass


class ModelIntegrityError(ModelLoopError):
    pass


class ModelInterruptedError(ModelLoopError):
    pass


class AgentLoop(ModelIntegrityMixin):
    """Single model-led loop from context selection through verified observation."""

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
        self._active_requests: dict[str, set[str]] = {}
        self._active_calls: dict[str, set[str]] = {}
        self._lock = threading.RLock()

    def run(self, run_id: str, *, max_actions: int | None = None) -> AgentRunView:
        view = self.service._resume_runtime(run_id, max_actions=max_actions)
        for _ in range(self.max_turns):
            if view.terminal.terminal or view.run.status in {"completed", "failed", "cancelled"}:
                return view
            if self._is_cancelled(run_id):
                return self.service.cancel(run_id, reason="model_loop_cancelled")
            if view.run.status == "paused_budget":
                return view
            budget_view = self.service._enforce_runtime_budget(run_id)
            if budget_view.run.status == "paused_budget":
                return budget_view
            if view.run.status != "waiting_worker" or not view.next_action:
                return view

            recovered = self._recover_pending_turn(view)
            if recovered is None:
                response, request = self._model_turn(view)
                existing: Mapping[str, ToolResult] = {}
                reconcile = False
            else:
                response, request, existing = recovered
                reconcile = True
                self.service.conversation.record_model_response(run_id, response)
            tactical_update = self._record_tactical_update(view, request, response)
            budget_view = self.service._record_model_usage(
                run_id,
                request.request_id,
                response.usage,
            )
            if budget_view.run.status == "paused_budget":
                return budget_view
            if handle_tool_expand(self, run_id, response):
                view = self.service.status(run_id)
                continue
            if not response.tool_calls:
                return budget_view
            results = self._execute_tool_calls(
                view,
                request,
                response,
                existing=existing,
                reconcile=reconcile,
            )
            artifact_ids = self.service.conversation.record_tool_results(
                request.request_id,
                view.run.run_id,
                results,
            )
            self._record_tactical_attempts(
                view,
                request,
                response,
                results,
                artifact_ids=artifact_ids,
                tactical_update=tactical_update,
            )
            successful = tuple(item for item in results if item.status == "success")
            if not successful:
                return view

            if tactical_update is not None and not bool(
                response.structured_output.get("commit_lifecycle_gate", False)
            ):
                view = self.service.status(run_id)
                continue

            if view.next_action == "provide_target":
                target = self._target_from_results(successful)
                self.service.runtime.provide_target(run_id, targets=(target,))
                view = self.service._resume_runtime(run_id, max_actions=max_actions)
                continue

            output: Any
            if len(successful) == 1:
                output = successful[0].output
            else:
                output = {"tool_results": [item.to_dict() for item in successful]}
            receipt = dict(view.handoff)
            observation = Observation(
                action_id=view.next_action,
                output=output,
                tool="model-loop:" + ",".join(item.tool_name for item in successful),
                usage={"_accounted_request_id": request.request_id},
                idempotency_key=contract_hash(
                    {
                        "run_id": run_id,
                        "request_id": request.request_id,
                        "action_id": view.next_action,
                        "calls": [item.call_id for item in successful],
                    }
                ),
                continue_run=False,
                handoff_id=str(receipt.get("handoff_id") or ""),
                handoff_token=str(receipt.get("handoff_token") or ""),
                attempt_id=str(receipt.get("attempt_id") or ""),
                contract_hash=str(receipt.get("contract_hash") or ""),
            )
            self.service._submit_runtime_observation(run_id, observation)
            view = self.service._resume_runtime(run_id, max_actions=max_actions)
        return view

    def cancel(self, run_id: str) -> None:
        with self._lock:
            self._cancelled_runs.add(run_id)
            requests = tuple(self._active_requests.get(run_id, ()))
            calls = tuple(self._active_calls.get(run_id, ()))
        for request_id in requests:
            self.model.cancel(request_id)
        if self.tools is not None:
            for call_id in calls:
                self.tools.cancel(call_id)

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
        last_error: BaseException | None = None
        for attempt in range(self.max_retries + 1):
            if self._is_cancelled(view.run.run_id):
                raise ModelInterruptedError("model_loop_cancelled")
            request = self._request(view, attempt=attempt)
            self._save_request(request)
            self._track(self._active_requests, view.run.run_id, request.request_id, add=True)
            try:
                response = self._invoke(request)
                validated = self._validate_response(request, response)
                return validated, request
            except ModelIntegrityError:
                raise
            except BaseException as exc:
                last_error = exc
                self._save_failure_response(request, exc)
            finally:
                self._track(self._active_requests, view.run.run_id, request.request_id, add=False)
        raise ModelLoopError(f"model_provider_retries_exhausted:{last_error}") from last_error

    def _request(self, view: AgentRunView, *, attempt: int) -> ModelRequest:
        capabilities = self.model.capabilities()
        model_name = self.model_name or str(capabilities.metadata.get("model") or "")
        selection = self.service.context_selector.prepare_model_context(
            view,
            max_context_tokens=capabilities.max_context_tokens,
        )
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
        return ModelRequest(
            request_id=f"model-request-{uuid4().hex}",
            run_id=view.run.run_id,
            messages=selection.messages,
            tools=tools,
            response_schema={
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
            },
            model=model_name,
            allow_parallel_tools=capabilities.parallel_tool_calls,
            metadata={
                "action_id": view.next_action,
                "attempt": attempt,
                "context_hash": selection.context_hash,
                "estimated_context_tokens": selection.estimated_context_tokens,
                "provider_context_tokens": selection.provider_context_tokens,
                "selected_tokens": selection.selected_tokens,
                "reserved_output_tokens": selection.reserved_output_tokens,
                "projection_bytes": selection.projection_bytes,
                "cache_read_tokens": selection.cache_read_tokens,
                "cache_write_tokens": selection.cache_write_tokens,
                "context_overflow_tokens": selection.context_overflow_tokens,
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
        accumulator = StreamTextAccumulator()
        tool_calls: list[Mapping[str, Any]] = []
        usage: Mapping[str, Any] = {}
        structured: Mapping[str, Any] = {}
        finish_reason = ""
        expected_sequence = 0
        completed = False
        try:
            for event in self.model.stream(request):
                if event.request_id != request.request_id:
                    raise ModelIntegrityError("model_stream_request_mismatch")
                if event.sequence != expected_sequence:
                    raise ModelIntegrityError(
                        f"model_stream_sequence_mismatch:{event.sequence}:{expected_sequence}"
                    )
                expected_sequence += 1
                payload = dict(event.payload)
                if event.event_type in {"text", "text_delta"}:
                    text_delta = str(payload.get("delta") or payload.get("text") or "")
                    accumulator.append(text_delta)
                    raw_delta = text_delta.encode("utf-8", errors="replace")
                    event = replace(
                        event,
                        payload={
                            "projected": True,
                            "byte_count": len(raw_delta),
                            "content_hash": hashlib.sha256(raw_delta).hexdigest(),
                            "preview": raw_delta[:1024].decode("utf-8", errors="replace"),
                        },
                    )
                self.service.runtime.store.save_model_stream_event(event)
                if event.event_type == "tool_call":
                    tool_calls.append(payload)
                elif event.event_type == "usage":
                    usage = json_mapping(payload, field="model_stream.usage")
                elif event.event_type == "completed":
                    completed = True
                    if isinstance(payload.get("tool_calls"), Sequence):
                        tool_calls.extend(
                            dict(item) for item in payload["tool_calls"] if isinstance(item, Mapping)
                        )
                    if isinstance(payload.get("usage"), Mapping):
                        usage = dict(payload["usage"])
                    if isinstance(payload.get("structured_output"), Mapping):
                        structured = dict(payload["structured_output"])
                    finish_reason = str(payload.get("finish_reason") or "stop")
            if not completed:
                raise ModelInterruptedError("model_stream_incomplete")
        except BaseException:
            accumulator.close()
            if accumulator.byte_count:
                setattr(threading.current_thread(), "model_partial_stream", accumulator)
            else:
                accumulator.discard()
            raise
        metadata: dict[str, Any] = {}
        if accumulator.byte_count <= MAX_INLINE_MODEL_STREAM_BYTES:
            text = accumulator.inline_text()
            accumulator.discard()
        else:
            accumulator.close()
            artifact = self.service.runtime.artifacts.put_file(
                accumulator.path,
                run_id=request.run_id,
                artifact_type="model_stream_text",
                media_type="text/plain; charset=utf-8",
                preview=accumulator.preview(),
                metadata={"request_id": request.request_id, "complete": True},
            )
            accumulator.discard()
            projection = self.service.runtime.artifacts.project(artifact)
            text = json.dumps({"complete_text_artifact": projection}, ensure_ascii=False, sort_keys=True)
            metadata["complete_text_artifact"] = artifact.artifact_id
        return ModelResponse(
            request_id=request.request_id,
            status="completed",
            text=text,
            structured_output=structured,
            tool_calls=tuple(tool_calls),
            usage=usage,
            finish_reason=finish_reason,
            metadata=metadata,
        )

    def _validate_response(self, request: ModelRequest, response: ModelResponse) -> ModelResponse:
        if response.request_id != request.request_id:
            raise ModelIntegrityError("model_response_request_mismatch")
        usage = self._normalize_usage(response.usage)
        normalized = replace(response, usage=usage)
        claimed_hash = normalized.response_hash
        authoritative_hash = contract_hash(self._response_projection(normalized))
        status = normalized.status
        if claimed_hash and claimed_hash != authoritative_hash:
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
        if status == "integrity_error":
            raise ModelIntegrityError("model_response_hash_mismatch")
        if normalized.status not in {"completed", "success"}:
            raise ModelLoopError(f"model_response_failed:{normalized.status}:{normalized.error}")
        self.service.conversation.record_model_response(request.run_id, normalized)
        return replace(normalized, response_hash=authoritative_hash)

    def _save_failure_response(self, request: ModelRequest, error: BaseException) -> None:
        if any(
            item.request_id == request.request_id
            for item in self.service.runtime.store.model_responses(request.run_id)
        ):
            return
        accumulator = getattr(threading.current_thread(), "model_partial_stream", None)
        partial_text = ""
        partial_artifact: Mapping[str, Any] = {}
        if isinstance(accumulator, StreamTextAccumulator):
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
            status="interrupted" if isinstance(error, ModelInterruptedError) else "failed",
            provider=type(self.model).__name__,
            model=request.model,
            text="",
            error=safe_error,
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
                usage={},
                response=response.to_dict(),
                created_at=utc_now(),
            )
        )
        if hasattr(threading.current_thread(), "model_partial_stream"):
            delattr(threading.current_thread(), "model_partial_stream")

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
        calls = tuple(self._tool_call(view, request, item, index) for index, item in enumerate(response.tool_calls))
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
            metadata={"action_id": view.next_action, "request_id": request.request_id},
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
        self._track(self._active_calls, view.run.run_id, call.call_id, add=True)
        try:
            result = self.tools.reconcile(call) if reconcile else None
            if result is None:
                result = self.tools.invoke(call)
        finally:
            self._track(self._active_calls, view.run.run_id, call.call_id, add=False)
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
        encoded = json.dumps(
            durable_observation, ensure_ascii=False, sort_keys=True, default=str
        ).encode("utf-8")
        artifact_id = ""
        if len(encoded) > MAX_INLINE_MODEL_OBSERVATION_BYTES:
            artifact = self.service.runtime.artifacts.put_json(
                normalized.to_dict(),
                run_id=view.run.run_id,
                artifact_type="model_observation_tool_result",
                preview={
                    "request_id": request.request_id,
                    "call_id": call.call_id,
                    "tool_name": call.tool_name,
                    "status": normalized.status,
                    "output_hash": output_hash,
                    "byte_count": len(encoded),
                },
                metadata={"action_id": view.next_action},
            )
            artifact_id = artifact.artifact_id
            durable_observation = {
                "tool_result_artifact": self.service.runtime.artifacts.project(artifact)
            }
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
        requests = {item.request_id: item for item in store.model_requests(view.run.run_id)}
        observations = store.model_observations(view.run.run_id)
        by_request: dict[str, dict[str, ToolResult]] = {}
        for record in observations:
            if record.action_id != view.next_action or record.status != "success":
                continue
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

        for response_record in reversed(store.model_responses(view.run.run_id)):
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
            tactical_update = response.structured_output.get("tactical_update")
            if (
                isinstance(tactical_update, Mapping)
                and not bool(response.structured_output.get("commit_lifecycle_gate", False))
            ):
                call_ids = {
                    str(item.get("call_id") or item.get("id") or f"call-{index}")
                    for index, item in enumerate(response.tool_calls)
                    if isinstance(item, Mapping)
                }
                completed_ids = {
                    item.call_id
                    for item in store.tactical_attempts(view.run.run_id)
                    if item.request_id == request.request_id
                }
                if call_ids and call_ids.issubset(completed_ids):
                    continue
            return response, request, by_request.get(request.request_id, {})
        return None

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
