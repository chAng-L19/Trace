from __future__ import annotations

import hashlib
import json
import sys
import threading
from collections.abc import Mapping, Sequence
from dataclasses import replace
from typing import Any

from ..core import ModelRequest, ModelResponse, contract_hash
from ..providers.opaque import opaque_only
from .bounded_output import BoundedOutput


MAX_INLINE_MODEL_STREAM_BYTES = 64 * 1024


def invoke_model_stream(loop: Any, request: ModelRequest) -> ModelResponse:
    accumulator = BoundedOutput()
    tool_calls: list[Mapping[str, Any]] = []
    usage: Mapping[str, Any] = {}
    structured: Mapping[str, Any] = {}
    finish_reason = ""
    continuation: Mapping[str, Any] = {}
    response_id = provider = model = ""
    expected_sequence = 0
    completed = False
    stream = None
    try:
        stream = loop.model.stream(request)
        for event in stream:
            if loop._is_cancelled(request.run_id) or loop._is_interrupted(request.run_id):
                raise loop._interrupted_error("model_stream_interrupted")
            if completed:
                raise loop._integrity_error("model_stream_event_after_completion")
            if event.request_id != request.request_id:
                raise loop._integrity_error("model_stream_request_mismatch")
            if event.sequence != expected_sequence:
                raise loop._integrity_error(
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
            elif event.event_type == "status":
                event = replace(event, payload={"status": str(payload.get("status") or "")[:128]})
            elif event.event_type == "usage":
                event = replace(event, payload=loop._normalize_usage(payload))
            else:
                event = replace(event, payload={"projected": True, "content_hash": contract_hash(opaque_only(payload))})
            loop.service.runtime.store.save_model_stream_event(event)
            loop.service.runtime.store.append_event(request.run_id, "model_stream", {
                "request_id": request.request_id, "sequence": event.sequence,
                "event_type": event.event_type, "delta": dict(event.payload),
                "provisional": True,
            })
            if event.event_type == "tool_call":
                tool_calls.append(payload)
            elif event.event_type == "usage":
                usage = loop._normalize_usage(payload)
            elif event.event_type == "completed":
                completed = True
                final_text = payload.get("text")
                if isinstance(final_text, str):
                    if not accumulator.byte_count:
                        accumulator.append(final_text)
                    elif hashlib.sha256(final_text.encode("utf-8")).hexdigest() != accumulator.preview()["content_hash"]:
                        raise loop._integrity_error("model_stream_text_mismatch")
                if isinstance(payload.get("tool_calls"), Sequence):
                    tool_calls.extend(
                        dict(item) for item in payload["tool_calls"] if isinstance(item, Mapping)
                    )
                if isinstance(payload.get("usage"), Mapping):
                    usage = loop._normalize_usage(payload["usage"])
                if isinstance(payload.get("structured_output"), Mapping):
                    structured = dict(payload["structured_output"])
                finish_reason = str(payload.get("finish_reason") or "stop")
                continuation = opaque_only(payload.get("continuation") or {})
                response_id = str(payload.get("response_id") or "")
                provider, model = str(payload.get("provider") or ""), str(payload.get("model") or "")
        if not completed:
            raise loop._interrupted_error("model_stream_incomplete")
        if finish_reason in {"length", "max_tokens"}:
            raise loop._interrupted_error(f"finish_reason:{finish_reason}")
        if finish_reason not in {"", "stop", "tool_calls", "function_call", "end_turn", "stop_sequence"}:
            raise loop._loop_error(f"finish_reason:{finish_reason}")
    except BaseException:
        setattr(threading.current_thread(), "model_partial_usage", (request.request_id, usage))
        accumulator.close()
        if accumulator.byte_count:
            setattr(threading.current_thread(), "model_partial_stream", accumulator)
        else:
            accumulator.discard()
        raise
    finally:
        active_error = sys.exception()
        close = getattr(stream, "close", None)
        if callable(close):
            try:
                close()
            except BaseException as exc:
                if active_error is not None and isinstance(exc, Exception):
                    loop.service.runtime.store.append_event(request.run_id, "model_stream_cleanup_failed", {
                        "request_id": request.request_id, "error_type": type(exc).__name__,
                    })
                else:
                    setattr(threading.current_thread(), "model_partial_usage", (request.request_id, usage))
                    accumulator.close()
                    if accumulator.byte_count:
                        setattr(threading.current_thread(), "model_partial_stream", accumulator)
                    else:
                        accumulator.discard()
                    raise
    status, error = "completed", ""
    metadata: dict[str, Any] = {}
    try:
        if accumulator.byte_count <= MAX_INLINE_MODEL_STREAM_BYTES:
            text = accumulator.inline_text()
        else:
            accumulator.close()
            artifact = loop.service.runtime.artifacts.put_file(
                accumulator.path,
                run_id=request.run_id,
                artifact_type="model_stream_text",
                media_type="text/plain; charset=utf-8",
                preview=accumulator.preview(),
                metadata={"request_id": request.request_id, "complete": True},
            )
            projection = loop.service.runtime.artifacts.project(artifact)
            text = json.dumps({"complete_text_artifact": projection}, ensure_ascii=False, sort_keys=True)
            metadata["complete_text_artifact"] = artifact.artifact_id
    except BaseException:
        setattr(threading.current_thread(), "model_partial_usage", (request.request_id, usage))
        raise
    finally:
        accumulator.discard()
    return ModelResponse(
        request_id=request.request_id,
        status=status,
        provider=provider,
        model=model,
        text=text,
        structured_output=structured,
        tool_calls=tuple(tool_calls) if status == "completed" else (),
        usage=usage,
        finish_reason=finish_reason,
        error=error,
        metadata=metadata,
        response_id=response_id,
        continuation=continuation,
    )
