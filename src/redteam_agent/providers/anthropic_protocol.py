from __future__ import annotations

import hashlib
import json
from collections.abc import Iterator, Mapping
from typing import Any

from ..core import ModelRequest, ModelResponse, ModelStreamEvent
from .openai_protocol import sse_documents, token_limit


def request_payload(provider: Any, request: ModelRequest) -> tuple[dict, dict]:
    name_map: dict[str, str] = {}
    tools = [provider._tool(tool, name_map)["function"] for tool in request.tools]
    names = {original: encoded for encoded, original in name_map.items()}
    messages, system, assistant_sources = [], [], {}
    for item in request.messages:
        message = provider._message(item, names)
        role, content = message["role"], message.get("content")
        if role in {"system", "developer"}:
            if content:
                system.append(provider._text(content))
            continue
        if role == "tool":
            blocks = [{"type": "tool_result", "tool_use_id": message["tool_call_id"],
                       "content": content if isinstance(content, (str, list)) else ""}]
            original = item.get("content")
            if isinstance(original, Mapping) and original.get("status") in {"error", "failed"}:
                blocks[0]["is_error"] = True
            role = "user"
        elif role in {"assistant", "user"}:
            blocks = ([{"type": "text", "text": content}] if isinstance(content, str) and content
                      else list(content) if isinstance(content, list) else [])
            for call in message.get("tool_calls", ()):
                blocks.append({"type": "tool_use", "id": call["id"], "name": call["function"]["name"],
                               "input": json.loads(call["function"]["arguments"])})
        else:
            raise ValueError("provider_message_role_invalid")
        if not blocks:
            continue
        projected = {"role": role, "content": blocks}
        messages.append(projected)
        source = item.get("source_request_id")
        if role == "assistant" and source:
            if source in assistant_sources:
                raise RuntimeError("provider_continuation_identity_duplicate")
            assistant_sources[source] = projected
    # Compaction may remove an entire turn. Never attach its signature to equal text or call IDs.
    for state in request.continuation.get("chain", ()):
        message = assistant_sources.get(state.get("assistant_request_id"))
        if message is None:
            continue
        if state.get("projection_block_layout_unavailable"):
            raise RuntimeError("provider_continuation_projection_layout_unavailable")
        ids = set(state.get("assistant_call_ids") or ())
        blocks = message["content"]
        joined = "".join(block.get("text", "") for block in blocks if block.get("type") == "text")
        if ids != {block.get("id") for block in blocks if block.get("type") == "tool_use"} or state.get(
            "assistant_projection_text_hash", state.get("assistant_text_hash")) != hashlib.sha256(joined.encode()).hexdigest():
            raise RuntimeError("provider_continuation_message_mismatch")
        thinking = {}
        for opaque in state.get("thinking_blocks", ()):
            block = {key: opaque[key] for key in ("type", "signature", "data") if key in opaque}
            if block.get("type") == "thinking":
                block["thinking"] = ""
            thinking[int(opaque.get("index", 0))] = block
        layout = state.get("projection_block_layout", state.get("block_layout", ()))
        if layout:
            tool_blocks = {block["id"]: block for block in blocks if block.get("type") == "tool_use"}
            rebuilt, offset = [], 0
            for index, part in enumerate(layout):
                if part["type"] == "text":
                    length = int(part["length"])
                    rebuilt.append({"type": "text", "text": joined[offset:offset + length]})
                    offset += length
                elif part["type"] == "tool_use":
                    rebuilt.append(tool_blocks[part["id"]])
                else:
                    rebuilt.append(thinking[index])
            if offset != len(joined):
                raise RuntimeError("provider_continuation_text_mismatch")
            message["content"] = rebuilt
        else:
            for index, block in thinking.items():
                blocks.insert(min(index, len(blocks)), block)
    merged: list[dict] = []
    for message in messages:
        if merged and merged[-1]["role"] == message["role"]:
            merged[-1]["content"].extend(message["content"])
        else:
            merged.append(message)
    maximum = provider.max_output_tokens or token_limit(request.metadata.get("reserved_output_tokens", 4096))
    if maximum <= 0 or (provider.thinking_budget_tokens and provider.thinking_budget_tokens >= maximum):
        raise ValueError("provider_thinking_budget_exceeds_output")
    payload: dict[str, Any] = {"model": request.model or provider.model, "max_tokens": maximum, "messages": merged}
    if request.response_schema:
        # The runtime schema permits open objects; native strict JSON schema does not.
        system.append("When answering without tools, return a JSON object matching this schema: " +
                      json.dumps(dict(request.response_schema), ensure_ascii=False, separators=(",", ":")))
    if system:
        payload["system"] = "\n\n".join(system)
    if tools:
        payload["tools"] = [{"name": tool["name"], "description": tool.get("description", ""),
                             "input_schema": tool["parameters"]} for tool in tools]
        payload["tool_choice"] = {"type": "auto", "disable_parallel_tool_use": not request.allow_parallel_tools}
    if provider.thinking_type:
        payload["thinking"] = {"type": provider.thinking_type}
        if provider.thinking_type != "disabled":
            # Official omitted display retains the encrypted signature, never plaintext reasoning.
            payload["thinking"]["display"] = "omitted"
        if provider.thinking_type == "enabled":
            payload["thinking"]["budget_tokens"] = provider.thinking_budget_tokens
    if provider.reasoning_effort:
        payload["output_config"] = {"effort": provider.reasoning_effort}
    return payload, name_map


def usage_values(raw: Any) -> dict:
    if raw is None:
        return {}
    if not isinstance(raw, Mapping):
        raise RuntimeError("provider_usage_invalid")
    usage = {}
    for source, target in (("input_tokens", "input_tokens"), ("output_tokens", "output_tokens"),
                           ("cache_read_input_tokens", "cache_read_tokens"),
                           ("cache_creation_input_tokens", "cache_write_tokens")):
        if source not in raw:
            continue
        value = raw[source]
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            raise RuntimeError("provider_usage_invalid")
        usage[target] = value
    if "input_tokens" in usage:
        # Anthropic input_tokens excludes cached input; run budgets must include it.
        usage["input_tokens"] += usage.get("cache_read_tokens", 0) + usage.get("cache_write_tokens", 0)
    if "input_tokens" in usage and "output_tokens" in usage:
        usage["total_tokens"] = usage["input_tokens"] + usage["output_tokens"]
    return usage


def message_response(provider: Any, request: ModelRequest, document: Mapping, names: Mapping) -> ModelResponse:
    if document.get("type") != "message" or document.get("role") != "assistant" or not isinstance(document.get("content"), list):
        raise RuntimeError("provider_response_message_invalid")
    text, calls, opaque = [], [], []
    for index, block in enumerate(document["content"]):
        if not isinstance(block, Mapping):
            raise RuntimeError("provider_response_block_invalid")
        kind = block.get("type")
        if kind == "text":
            text.append(str(block.get("text") or ""))
        elif kind == "tool_use":
            if not block.get("id") or not isinstance(block.get("input"), Mapping):
                raise RuntimeError("provider_tool_call_invalid")
            calls.append({"id": block["id"], "function": {"name": block.get("name"), "arguments": block["input"]}})
        elif kind in {"thinking", "redacted_thinking"}:
            # An incompatible gateway must not silently corrupt signed thinking on replay.
            if kind == "thinking" and (block.get("thinking") or not isinstance(block.get("signature"), str) or not block["signature"]):
                raise RuntimeError("provider_omitted_thinking_required")
            if kind == "redacted_thinking" and (not isinstance(block.get("data"), str) or not block["data"]):
                raise RuntimeError("provider_redacted_thinking_invalid")
            opaque.append({"index": index, **{key: block[key] for key in ("type", "signature", "data") if key in block}})
        else:
            raise RuntimeError("provider_response_block_unsupported")
    joined = "".join(text)
    if len({call["id"] for call in calls}) != len(calls):
        raise RuntimeError("provider_tool_call_id_duplicate")
    finish = str(document.get("stop_reason") or "")
    status = "completed" if finish in {"end_turn", "tool_use", "stop_sequence", "refusal"} else "interrupted" if finish in {"max_tokens", "model_context_window_exceeded", "pause_turn"} else "failed"
    if finish == "tool_use" and not calls:
        raise RuntimeError("provider_tool_use_missing")
    if calls and status == "completed" and finish not in {"tool_use", "refusal"}:
        raise RuntimeError("provider_tool_use_stop_reason_invalid")
    structured = {}
    if joined:
        try:
            parsed = json.loads(joined)
            if isinstance(parsed, Mapping):
                structured = dict(parsed)
        except json.JSONDecodeError:
            pass
    return ModelResponse(request_id=request.request_id, status=status, provider="anthropic",
        model=str(document.get("model") or request.model or provider.model), text=joined,
        structured_output={} if finish == "refusal" else structured,
        tool_calls=provider._tool_calls(calls, names) if status == "completed" and finish != "refusal" else (),
        usage=usage_values(document.get("usage")), finish_reason=finish,
        error="" if status == "completed" else "provider_refusal" if finish == "refusal" else f"finish_reason:{finish}",
        response_id=str(document.get("id") or ""), metadata={"provider_response_id": str(document.get("id") or ""),
            **({"refusal": True, "refusal_text": joined, "response_category": "refusal"} if finish == "refusal" else {})},
        continuation={"thinking_blocks": opaque, "assistant_request_id": request.request_id,
                      "assistant_call_ids": [call["id"] for call in calls],
                      "block_layout": [{"type": block["type"],
                          **({"length": len(block.get("text", ""))} if block["type"] == "text" else {}),
                          **({"id": block["id"]} if block["type"] == "tool_use" else {})}
                          for block in document["content"]],
                      "assistant_text_hash": hashlib.sha256(joined.encode()).hexdigest()} if opaque else {})


def stream_events(provider: Any, request: ModelRequest, response: Any, names: Mapping, *, byte_limit: int) -> Iterator[ModelStreamEvent]:
    message, blocks, opened, partial = None, {}, set(), {}
    sequence, finished, stopped = 0, False, False
    for event in sse_documents(response, byte_limit=byte_limit):
        if event is None or stopped:
            raise RuntimeError("provider_stream_event_after_completion")
        kind = event.get("type")
        if kind in {"content_block_start", "content_block_delta", "content_block_stop"}:
            index = event.get("index")
            if not isinstance(index, int) or isinstance(index, bool) or index < 0:
                raise RuntimeError("provider_stream_block_index_invalid")
        if kind == "error":
            from .openai_compatible import ProviderHTTPError
            code, detail = provider._error(event, b"")
            status = {"rate_limit_error": 429, "overloaded_error": 529, "api_error": 500}.get(code, 400)
            raise ProviderHTTPError(status, code, detail)
        if kind == "ping":
            continue
        if kind == "message_start":
            if message is not None or not isinstance(event.get("message"), Mapping):
                raise RuntimeError("provider_stream_message_start_invalid")
            message = dict(event["message"])
            if message.get("content") != [] or message.get("stop_reason"):
                raise RuntimeError("provider_stream_message_start_invalid")
            usage_values(message.get("usage"))
            message["usage"] = dict(message.get("usage") or {})
            if message["usage"]:
                yield ModelStreamEvent(request.request_id, sequence, "usage", usage_values(message["usage"]))
                sequence += 1
        elif message is None:
            raise RuntimeError("provider_stream_message_start_missing")
        elif kind == "content_block_start":
            index, block = event.get("index"), event.get("content_block")
            if finished or not isinstance(index, int) or isinstance(index, bool) or index != len(blocks) or not isinstance(block, Mapping):
                raise RuntimeError("provider_stream_block_start_invalid")
            blocks[index] = dict(block)
            opened.add(index)
            if block.get("type") == "text" and block.get("text"):
                yield ModelStreamEvent(request.request_id, sequence, "text_delta", {"delta": block["text"]})
                sequence += 1
        elif kind == "content_block_delta":
            index, delta = event.get("index"), event.get("delta")
            if index not in opened or finished or not isinstance(delta, Mapping):
                raise RuntimeError("provider_stream_block_delta_invalid")
            block, delta_type = blocks[index], delta.get("type")
            if delta_type == "text_delta" and block.get("type") == "text":
                value = str(delta.get("text") or "")
                block["text"] = block.get("text", "") + value
                yield ModelStreamEvent(request.request_id, sequence, "text_delta", {"delta": value})
                sequence += 1
            elif delta_type == "input_json_delta" and block.get("type") == "tool_use":
                partial[index] = partial.get(index, "") + str(delta.get("partial_json") or "")
            elif delta_type == "signature_delta" and block.get("type") == "thinking":
                block["signature"] = block.get("signature", "") + str(delta.get("signature") or "")
            elif delta_type == "thinking_delta" and block.get("type") == "thinking":
                if delta.get("thinking"):
                    raise RuntimeError("provider_omitted_thinking_required")
            else:
                raise RuntimeError("provider_stream_delta_unsupported")
        elif kind == "content_block_stop":
            index = event.get("index")
            if index not in opened:
                raise RuntimeError("provider_stream_block_stop_invalid")
            opened.remove(index)
            if index in partial:
                try:
                    blocks[index]["input"] = json.loads(partial.pop(index))
                except json.JSONDecodeError as exc:
                    raise RuntimeError("provider_tool_arguments_invalid_json") from exc
        elif kind == "message_delta":
            if opened or finished or not isinstance(event.get("delta"), Mapping):
                raise RuntimeError("provider_stream_message_delta_invalid")
            usage_values(event.get("usage"))
            message.update(event["delta"])
            message["usage"].update(event.get("usage") or {})
            if message["usage"]:
                yield ModelStreamEvent(request.request_id, sequence, "usage", usage_values(message["usage"]))
                sequence += 1
            finished = bool(message.get("stop_reason"))
        elif kind == "message_stop":
            if not finished or opened:
                raise RuntimeError("provider_stream_incomplete")
            stopped = True
        # Unknown event types are forward-compatible; known malformed states fail closed.
    if not stopped or message is None:
        raise RuntimeError("provider_stream_incomplete")
    message["content"] = list(blocks.values())
    result = message_response(provider, request, message, names)
    if result.usage:
        yield ModelStreamEvent(request.request_id, sequence, "usage", dict(result.usage))
        sequence += 1
    payload = {key: value for key, value in result.to_dict().items()
               if key not in {"kind", "schema_version", "request_id", "response_hash"}}
    yield ModelStreamEvent(request.request_id, sequence, "completed", payload)
