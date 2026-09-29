from __future__ import annotations

import hashlib
import json
from collections.abc import Iterator, Mapping
from typing import Any

from ..core import ModelRequest, ModelResponse, ModelStreamEvent
from .opaque import chat_continuation, merge_opaque_delta


def request_payload(provider: Any, request: ModelRequest) -> tuple[dict, dict]:
    name_map: dict[str, str] = {}
    tools = [provider._tool(item, name_map) for item in request.tools]
    names = {original: encoded for encoded, original in name_map.items()}
    messages = [provider._message(item, names) for item in request.messages]
    chain = request.continuation.get("chain", ())
    payload: dict[str, Any] = {"model": request.model or provider.model}
    if provider._responses_api:
        latest = chain[-1] if chain else {}
        last_assistant = max((i for i, message in enumerate(messages) if message["role"] == "assistant"), default=-1)
        if latest.get("previous_response_id"):
            payload["previous_response_id"] = latest["previous_response_id"]
            messages = [message for i, message in enumerate(messages)
                        if i > last_assistant or message["role"] in {"system", "developer"}]
        inputs: list[dict] = []
        reasoning = list(latest.get("reasoning_items") or ()) if not latest.get("previous_response_id") else []
        for index, message in enumerate(messages):
            if index == last_assistant:
                inputs.extend(reasoning)
            if message["role"] == "tool":
                inputs.append({"type": "function_call_output", "call_id": message["tool_call_id"],
                               "output": message["content"]})
            else:
                if message.get("content") is not None:
                    inputs.append({"role": message["role"], "content": message["content"]})
                for call in message.get("tool_calls", ()):
                    inputs.append({"type": "function_call", "call_id": call["id"], **call["function"]})
        payload["input"] = inputs
        if reasoning and last_assistant < 0:
            inputs[:0] = reasoning
        payload["include"] = ["reasoning.encrypted_content"]
        if tools:
            payload["tools"] = [{"type": "function", **item["function"]} for item in tools]
        if request.response_schema:
            payload["text"] = {"format": {"type": "json_schema", "name": "trace_response",
                                            "strict": False, "schema": dict(request.response_schema)}}
    else:
        for continuation in chain:
            opaque = continuation.get("assistant", {})
            call_ids = set(continuation.get("assistant_call_ids") or
                           [call.get("id") for call in opaque.get("tool_calls", ())])
            for message in reversed(messages):
                if message["role"] != "assistant":
                    continue
                ids = {call.get("id") for call in message.get("tool_calls", ())}
                if call_ids and not call_ids.issubset(ids):
                    continue
                if not call_ids and continuation.get("assistant_text_hash") != hashlib.sha256(
                    str(message.get("content") or "").encode()
                ).hexdigest():
                    continue
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
    return payload, name_map


def responses_response(provider: Any, request: ModelRequest, document: Mapping, name_map: Mapping) -> ModelResponse:
    text, calls, encrypted = [], [], []
    for item in document.get("output") or ():
        if not isinstance(item, Mapping):
            raise RuntimeError("provider_response_output_invalid")
        if item.get("type") == "message":
            text.extend(str(part.get("text") or "") for part in item.get("content", ())
                        if isinstance(part, Mapping) and part.get("type") == "output_text")
        elif item.get("type") == "function_call":
            calls.append({"id": item.get("call_id"), "function": {"name": item.get("name"), "arguments": item.get("arguments")}})
        elif item.get("type") == "reasoning" and item.get("encrypted_content"):
            encrypted.append({key: item[key] for key in ("id", "type", "encrypted_content") if key in item})
    state = document.get("status")
    finish = "stop" if state == "completed" else "length" if state == "incomplete" else "failed"
    usage = document.get("usage") or {}
    result = provider._response(request, {
        "id": document.get("id"), "model": document.get("model"),
        "choices": [{"message": {"content": "".join(text), "tool_calls": calls}, "finish_reason": finish}],
        "usage": {"prompt_tokens": usage.get("input_tokens"), "completion_tokens": usage.get("output_tokens"),
                  "total_tokens": usage.get("total_tokens"), "prompt_tokens_details": usage.get("input_tokens_details"),
                  "completion_tokens_details": usage.get("output_tokens_details")},
    }, name_map)
    continuation = {}
    if document.get("id"):
        continuation["previous_response_id"] = document["id"]
    if encrypted:
        continuation["reasoning_items"] = encrypted
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
    text, calls, usage, opaque = [], {}, {}, {}
    response_id, model, finish = "", request.model, ""
    result = None
    done = False
    for document in sse_documents(response, byte_limit=byte_limit):
        if document is None:
            done = True
            break
        if document.get("error"):
            raise RuntimeError("provider_stream_error")
        events: list[tuple[str, dict]] = []
        if provider._responses_api:
            if result is not None:
                raise RuntimeError("provider_stream_event_after_completion")
            kind = str(document.get("type") or "")
            if kind == "response.output_text.delta":
                events.append(("text_delta", {"delta": str(document.get("delta") or "")}))
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
            if isinstance(document.get("usage"), Mapping):
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
                         "tool_calls": list(calls.values()), **opaque}}]}, name_map)
        from dataclasses import replace
        result = replace(result, usage=usage)
    payload = {key: value for key, value in result.to_dict().items()
               if key not in {"kind", "schema_version", "request_id", "response_hash"}}
    yield ModelStreamEvent(request.request_id, sequence, "completed", payload)
