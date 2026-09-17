from __future__ import annotations

import hashlib
import json
import threading
from collections.abc import Mapping, Sequence
from dataclasses import replace
from typing import Any

from ..core import ModelRequest, ModelResponse
from .bounded_output import BoundedOutput


MAX_INLINE_MODEL_STREAM_BYTES = 64 * 1024


def invoke_model_stream(loop: Any, request: ModelRequest) -> ModelResponse:
    accumulator = BoundedOutput()
    tool_calls: list[Mapping[str, Any]] = []
    usage: Mapping[str, Any] = {}
    structured: Mapping[str, Any] = {}
    finish_reason = ""
    expected_sequence = 0
    completed = False
    try:
        for event in loop.model.stream(request):
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
            loop.service.runtime.store.save_model_stream_event(event)
            if event.event_type == "tool_call":
                tool_calls.append(payload)
            elif event.event_type == "usage":
                usage = loop._normalize_usage(payload)
            elif event.event_type == "completed":
                completed = True
                if isinstance(payload.get("tool_calls"), Sequence):
                    tool_calls.extend(
                        dict(item) for item in payload["tool_calls"] if isinstance(item, Mapping)
                    )
                if isinstance(payload.get("usage"), Mapping):
                    usage = loop._normalize_usage(payload["usage"])
                if isinstance(payload.get("structured_output"), Mapping):
                    structured = dict(payload["structured_output"])
                finish_reason = str(payload.get("finish_reason") or "stop")
        if not completed:
            raise loop._interrupted_error("model_stream_incomplete")
    except BaseException:
        setattr(threading.current_thread(), "model_partial_usage", (request.request_id, usage))
        accumulator.close()
        if accumulator.byte_count:
            setattr(threading.current_thread(), "model_partial_stream", accumulator)
        else:
            accumulator.discard()
        raise
    status, error = "completed", ""
    if finish_reason in {"length", "max_tokens"}:
        status, error = "interrupted", f"finish_reason:{finish_reason}"
    elif finish_reason not in {"", "stop", "tool_calls", "function_call", "end_turn", "stop_sequence"}:
        status, error = "failed", f"finish_reason:{finish_reason}"
    metadata: dict[str, Any] = {}
    if accumulator.byte_count <= MAX_INLINE_MODEL_STREAM_BYTES:
        text = accumulator.inline_text()
        accumulator.discard()
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
        accumulator.discard()
        projection = loop.service.runtime.artifacts.project(artifact)
        text = json.dumps({"complete_text_artifact": projection}, ensure_ascii=False, sort_keys=True)
        metadata["complete_text_artifact"] = artifact.artifact_id
    return ModelResponse(
        request_id=request.request_id,
        status=status,
        text=text,
        structured_output=structured,
        tool_calls=tuple(tool_calls) if status == "completed" else (),
        usage=usage,
        finish_reason=finish_reason,
        error=error,
        metadata=metadata,
    )
