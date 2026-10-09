from __future__ import annotations

import hashlib
import json
from collections.abc import Iterator, Mapping
from typing import Any

from ..core import ModelRequest, ModelResponse, ModelStreamEvent
from .opaque import chat_continuation, merge_opaque_delta


def token_limit(value: Any) -> int:
    """Accept integer config/environment values without bools or float truncation."""
    if isinstance(value, bool) or not isinstance(value, (int, str)):
        raise ValueError("provider_output_tokens_invalid")
    try:
        parsed = int(value)
    except ValueError:
        raise ValueError("provider_output_tokens_invalid") from None
    if not 0 <= parsed <= 2**31 - 1:
        raise ValueError("provider_output_tokens_invalid")
    return parsed


def request_payload(provider: Any, request: ModelRequest) -> tuple[dict, dict]:
    name_map: dict[str, str] = {}
    tools = [provider._tool(item, name_map) for item in request.tools]
    names = {original: encoded for encoded, original in name_map.items()}
    messages = [provider._message(item, names) for item in request.messages]
    chain = request.continuation.get("chain", ())
    payload: dict[str, Any] = {"model": request.model or provider.model}
    if provider._responses_api:
        inputs: list[dict] = []
        states = {state.get("assistant_request_id"): state for state in chain
                  if state.get("assistant_request_id")}
        for index, message in enumerate(messages):
            if message["role"] == "assistant":
                state = states.get(request.messages[index].get("source_request_id"))
                if state:
                    ids = [call["id"] for call in message.get("tool_calls", ())]
                    text_hash = hashlib.sha256(str(message.get("content") or "").encode()).hexdigest()
                    if (state.get("assistant_projection_text_hash", state.get("assistant_text_hash")) != text_hash
                            or state.get("assistant_call_ids", ids) != ids):
                        raise RuntimeError("provider_continuation_message_mismatch")
                    # Explicit replay makes the local compacted input the history boundary.
                    inputs.extend({**item, "summary": []} for item in state.get("reasoning_items", ()))
            if message["role"] == "tool":
                inputs.append({"type": "function_call_output", "call_id": message["tool_call_id"],
                               "output": message["content"]})
            else:
                if message.get("content") is not None:
                    inputs.append({"role": message["role"], "content": message["content"]})
                for call in message.get("tool_calls", ()):
                    inputs.append({"type": "function_call", "call_id": call["id"], **call["function"]})
        payload["input"] = inputs
        payload["include"] = ["reasoning.encrypted_content"]
        if tools:
            payload["tools"] = [{"type": "function", **item["function"]} for item in tools]
        if request.response_schema:
            payload["text"] = {"format": {"type": "json_schema", "name": "trace_response",
                                            "strict": False, "schema": dict(request.response_schema)}}
    else:
        for continuation in chain:
            opaque = continuation.get("assistant", {})
            call_ids = list(continuation.get("assistant_call_ids") or
                            [call.get("id") for call in opaque.get("tool_calls", ())])
            source_id = continuation.get("assistant_request_id")
            for index in reversed(range(len(messages))):
                message = messages[index]
                if message["role"] != "assistant":
                    continue
                if not source_id or request.messages[index].get("source_request_id") != source_id:
                    continue
                ids = [call.get("id") for call in message.get("tool_calls", ())]
                text_hash = hashlib.sha256(str(message.get("content") or "").encode()).hexdigest()
                expected_hash = continuation.get("assistant_projection_text_hash",
                                                 continuation.get("assistant_text_hash", text_hash))
                if call_ids != ids or expected_hash != text_hash:
                    raise RuntimeError("provider_continuation_message_mismatch")
                message.update({key: value for key, value in opaque.items() if key != "tool_calls"})
                for call in message.get("tool_calls", ()):
                    call.update(next((item for item in opaque.get("tool_calls", ()) if item.get("id") == call["id"]), {}))
                break
        payload["messages"] = messages
        if tools:
            payload["tools"] = tools
        if request.response_schema:
            payload["response_format"] = {"type": "json_schema", "json_schema": {
                "name": "trace_response", "strict": False, "schema": dict(request.response_schema)}}
    if tools:
        payload.update(tool_choice="auto", parallel_tool_calls=bool(request.allow_parallel_tools))
    output_limit = token_limit(getattr(provider, "max_output_tokens", 0) or
                               request.metadata.get("reserved_output_tokens", 0))
    if output_limit > 0:
        payload["max_output_tokens" if provider._responses_api else "max_completion_tokens"] = output_limit
    effort = getattr(provider, "reasoning_effort", "")
    if effort:
        if provider._responses_api:
            payload["reasoning"] = {"effort": effort}
        else:
            payload["reasoning_effort"] = effort
    return payload, name_map


def responses_response(provider: Any, request: ModelRequest, document: Mapping, name_map: Mapping) -> ModelResponse:
    text, calls, encrypted, refusals = [], [], [], []
    for item in document.get("output") or ():
        if not isinstance(item, Mapping):
            raise RuntimeError("provider_response_output_invalid")
        if item.get("type") == "message":
            for part in item.get("content", ()):
                if isinstance(part, Mapping) and part.get("type") == "output_text":
                    text.append(str(part.get("text") or ""))
                elif isinstance(part, Mapping) and part.get("type") == "refusal":
                    refusal = str(part.get("refusal") or "")
                    refusals.append(refusal)
                    text.append(refusal)
        elif item.get("type") == "function_call":
            calls.append({"id": item.get("call_id"), "function": {"name": item.get("name"), "arguments": item.get("arguments")}})
        elif item.get("type") == "reasoning" and item.get("encrypted_content"):
            encrypted.append({key: item[key] for key in ("id", "type", "encrypted_content") if key in item})
    state = document.get("status")
    finish = "stop" if state == "completed" else "length" if state == "incomplete" else "failed"
    usage = document.get("usage")
    if usage is None:
        usage = {}
    if not isinstance(usage, Mapping):
        raise RuntimeError("provider_usage_invalid")
    result = provider._response(request, {
        "id": document.get("id"), "model": document.get("model"),
        "choices": [{"message": {"content": "".join(text), "refusal": "".join(refusals),
                                  "tool_calls": calls}, "finish_reason": finish}],
        "usage": {target: usage[source] for source, target in (
            ("input_tokens", "prompt_tokens"), ("output_tokens", "completion_tokens"),
            ("total_tokens", "total_tokens"), ("input_tokens_details", "prompt_tokens_details"),
            ("output_tokens_details", "completion_tokens_details")) if source in usage},
    }, name_map)
    continuation = {}
    if encrypted:
        continuation = {"reasoning_items": encrypted, "assistant_request_id": request.request_id,
                        "assistant_text_hash": hashlib.sha256(result.text.encode()).hexdigest(),
                        "assistant_call_ids": [call["call_id"] for call in result.tool_calls]}
    from dataclasses import replace
    return replace(result, continuation=continuation)


def sse_documents(response: Any, *, byte_limit: int) -> Iterator[Mapping | None]:
    data: list[bytes] = []
    byte_count = 0
    while True:
        line = response.readline(byte_limit + 1)
        byte_count += len(line)
        if byte_count > byte_limit:
            raise RuntimeError("provider_response_too_large")
        if not line:
            if data:
                raise RuntimeError("provider_stream_truncated_event")
            return
        if line.rstrip(b"\r\n") == b"":
            if not data:
                continue
            raw = b"\n".join(data)
            data = []
            if raw == b"[DONE]":
                yield None
                return
            document = json.loads(raw.decode("utf-8"))
            if not isinstance(document, Mapping):
                raise RuntimeError("provider_stream_event_invalid")
            yield document
        elif line.startswith(b"data:"):
            data.append(line[5:].lstrip(b" ").rstrip(b"\r\n"))


def stream_events(provider: Any, request: ModelRequest, response: Any, name_map: Mapping, *, byte_limit: int) -> Iterator[ModelStreamEvent]:
    sequence = 0
    text, calls, usage, opaque, refusals = [], {}, {}, {}, []
    response_id, model, finish = "", request.model, ""
    result = None
    done = False
    for document in sse_documents(response, byte_limit=byte_limit):
        if document is None:
            done = True
            break
        if document.get("error"):
            from .openai_compatible import ProviderHTTPError
            code, detail = provider._error(document, b"")
            status = {"rate_limit_exceeded": 429, "rate_limit_error": 429,
                      "server_error": 500, "api_error": 500,
                      "overloaded_error": 503}.get(code, 400)
            raise ProviderHTTPError(status, code, detail)
        events: list[tuple[str, dict]] = []
        if provider._responses_api:
            if result is not None:
                raise RuntimeError("provider_stream_event_after_completion")
            kind = str(document.get("type") or "")
            if kind in {"response.output_text.delta", "response.refusal.delta"}:
                value = str(document.get("delta") or "")
                if kind == "response.refusal.delta":
                    refusals.append(value)
                events.append(("text_delta", {"delta": value, **({"metadata": {
                    "refusal": True, "refusal_text": "".join(refusals), "response_category": "refusal"}}
                    if kind == "response.refusal.delta" else {})}))
            elif kind in {"response.completed", "response.incomplete", "response.failed"}:
                if result is not None:
                    raise RuntimeError("provider_stream_duplicate_completion")
                result = responses_response(provider, request, document["response"], name_map)
                done = True
            elif kind in {"response.created", "response.in_progress"}:
                events.append(("status", {"status": kind.rpartition(".")[2]}))
        else:
            response_id = str(document.get("id") or response_id)
            model = str(document.get("model") or model)
            if document.get("usage") is not None:
                usage = provider._usage(document["usage"])
                events.append(("usage", dict(usage)))
            for choice in document.get("choices") or ():
                if choice.get("index", 0) != 0:
                    continue
                delta = choice.get("delta") or {}
                if delta.get("content"):
                    if finish:
                        raise RuntimeError("provider_stream_event_after_finish")
                    value = provider._text(delta["content"])
                    text.append(value)
                    events.append(("text_delta", {"delta": value}))
                if delta.get("refusal"):
                    if finish:
                        raise RuntimeError("provider_stream_event_after_finish")
                    value = provider._text(delta["refusal"])
                    refusals.append(value)
                    text.append(value)
                    events.append(("text_delta", {"delta": value, "metadata": {
                        "refusal": True, "refusal_text": "".join(refusals), "response_category": "refusal"}}))
                merge_opaque_delta(opaque, chat_continuation({key: value for key, value in delta.items() if key != "tool_calls"}))
                for call in delta.get("tool_calls") or ():
                    if finish:
                        raise RuntimeError("provider_stream_event_after_finish")
                    index = call.get("index", 0)
                    saved = calls.setdefault(index, {"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
                    for key in ("id", "type"):
                        if call.get(key):
                            saved[key] = call[key]
                    for key in ("name", "arguments"):
                        saved["function"][key] += str((call.get("function") or {}).get(key) or "")
                    signatures = chat_continuation({"tool_calls": [call]}).get("tool_calls", ())
                    if signatures:
                        merge_opaque_delta(saved, {key: value for key, value in signatures[0].items() if key != "id"})
                if choice.get("finish_reason"):
                    finish = str(choice["finish_reason"])
        for kind, payload in events:
            yield ModelStreamEvent(request.request_id, sequence, kind, payload)
            sequence += 1
    if not done or (not provider._responses_api and not finish):
        raise RuntimeError("provider_stream_incomplete")
    if result is None:
        result = provider._response(request, {"id": response_id, "model": model,
            "choices": [{"finish_reason": finish, "message": {"content": "".join(text),
                         "refusal": "".join(refusals), "tool_calls": list(calls.values()), **opaque}}]}, name_map)
        from dataclasses import replace
        result = replace(result, usage=usage)
    payload = {key: value for key, value in result.to_dict().items()
               if key not in {"kind", "schema_version", "request_id", "response_hash"}}
    yield ModelStreamEvent(request.request_id, sequence, "completed", payload)
