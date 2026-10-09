"""Offline refusal, streaming, and output-budget regression checks."""
from __future__ import annotations

import io
import json
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService, StartRequest
from redteam_agent.application.model_stream import invoke_model_stream
from redteam_agent.core import ModelRequest, ModelResponse
from redteam_agent.providers import FakeModelProvider, OpenAICompatibleProvider, ScriptedStream
from redteam_agent.providers.openai_protocol import request_payload, responses_response, stream_events


def sse(*documents: dict, done: bool = False) -> io.BytesIO:
    raw = "".join("data: " + json.dumps(item) + "\n\n" for item in documents)
    return io.BytesIO((raw + ("data: [DONE]\n\n" if done else "")).encode())


def rejects_usage(callback):
    try:
        callback()
    except RuntimeError as error:
        assert str(error) == "provider_usage_invalid"
    else:
        raise AssertionError("invalid usage was silently accepted")


def main() -> None:
    request = ModelRequest("offline", "offline", (), metadata={"reserved_output_tokens": 2048})
    provider = OpenAICompatibleProvider("http://127.0.0.1:1/v1", "fixture")
    refusal = "I cannot help with that."
    chat = provider._response(request, {"choices": [{"message": {"content": None, "refusal": refusal},
                                                     "finish_reason": "stop"}]}, {})
    document = {"id": "response-fixture", "status": "completed", "output": [
        {"type": "message", "content": [{"type": "refusal", "refusal": refusal}]}]}
    responses = responses_response(provider, request, document, {})
    for result in (chat, responses):
        assert result.text == refusal and result.metadata["refusal"] is True
        assert result.metadata["refusal_text"] == refusal
        assert result.structured_output == {} and result.status == "completed"
    mixed = provider._response(request, {"choices": [{"message": {
        "content": '{"commit_lifecycle_gate":true}', "refusal": refusal}, "finish_reason": "stop"}]}, {})
    assert mixed.text and mixed.metadata["refusal"] and not mixed.structured_output
    for finish, status in (("length", "interrupted"), ("content_filter", "failed")):
        stopped = provider._response(request, {"choices": [{"message": {"content": None, "refusal": refusal},
                                                            "finish_reason": finish}]}, {})
        assert stopped.status == status and stopped.metadata["refusal_text"] == refusal
    for state, status in (("incomplete", "interrupted"), ("failed", "failed")):
        stopped = responses_response(provider, request, {**document, "status": state}, {})
        assert stopped.status == status and stopped.metadata["refusal_text"] == refusal
    chat_events = list(stream_events(provider, request, sse(
        {"choices": [{"delta": {"refusal": "I cannot "}}]},
        {"choices": [{"delta": {"refusal": "help with that."}, "finish_reason": "stop"}]},
        done=True), {}, byte_limit=65536))
    assert "".join(item.payload.get("delta", "") for item in chat_events) == refusal
    assert chat_events[-1].payload["metadata"]["refusal"]
    assert chat_events[-1].payload["text"] == refusal
    responses_provider = OpenAICompatibleProvider("http://127.0.0.1:1/v1/responses", "fixture")
    response_events = list(stream_events(responses_provider, request, sse(
        {"type": "response.refusal.delta", "delta": refusal},
        {"type": "response.completed", "response": document}), {}, byte_limit=65536))
    assert response_events[0].payload["delta"] == refusal
    assert response_events[-1].payload["metadata"]["refusal_text"] == refusal
    try:
        list(stream_events(provider, request, sse({"choices": [{"delta": {"refusal": refusal}}]}),
                           {}, byte_limit=65536))
    except RuntimeError as error:
        assert "incomplete" in str(error)
    else:
        raise AssertionError("truncated refusal stream accepted")

    call = {"id": "tool-1", "function": {"name": "fixture", "arguments": '{"value":1}'}}
    tool_response = provider._response(request, {"choices": [{
        "message": {"content": None, "tool_calls": [call]}, "finish_reason": "tool_calls"}]}, {"fixture": "fixture:tool"})
    assert tool_response.tool_calls[0]["arguments"] == {"value": 1}
    assert not tool_response.metadata.get("refusal")
    for endpoint, field in (("v1", "max_completion_tokens"), ("v1/responses", "max_output_tokens")):
        configured = OpenAICompatibleProvider("http://127.0.0.1:1/" + endpoint, "fixture",
                                             max_output_tokens=4096, reasoning_effort="high")
        payload, _ = request_payload(configured, request)
        assert payload[field] == 4096
        assert (payload.get("reasoning_effort") or payload.get("reasoning", {}).get("effort")) == "high"
    assert request_payload(provider, request)[0]["max_completion_tokens"] == 2048

    for invalid in (True, -1, 1.5, "2", None, 2**63):
        for field in ("prompt_tokens", "completion_tokens", "total_tokens"):
            rejects_usage(lambda: provider._usage({field: invalid}))
        for field, count in (("prompt_tokens_details", "cached_tokens"),
                             ("completion_tokens_details", "reasoning_tokens")):
            rejects_usage(lambda: provider._usage({field: {count: invalid}}))
        rejects_usage(lambda: responses_response(provider, request,
                      {**document, "usage": {"input_tokens": invalid}}, {}))
    for invalid in ([], "invalid", False):
        rejects_usage(lambda: provider._usage(invalid))
        rejects_usage(lambda: responses_response(provider, request, {**document, "usage": invalid}, {}))
        rejects_usage(lambda: list(stream_events(provider, request,
                      sse({"usage": invalid}, done=True), {}, byte_limit=65536)))
        rejects_usage(lambda: provider._usage({"prompt_tokens_details": invalid}))
    assert provider._usage(None) == {}
    assert provider._usage({"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
                           "prompt_tokens_details": {"cached_tokens": 4},
                           "completion_tokens_details": {"reasoning_tokens": 3}}) == {
        "input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
        "cache_read_tokens": 4, "reasoning_tokens": 3}

    # Use the real context selector and request builder, without an HTTP request.
    for endpoint, field in (("v1", "max_completion_tokens"), ("v1/responses", "max_output_tokens")):
        for maximum in (0, 4096):
            configured = OpenAICompatibleProvider("http://127.0.0.1:1/" + endpoint, "fixture",
                                                  max_output_tokens=maximum, environ={})
            with tempfile.TemporaryDirectory(prefix="trace-output-budget-") as temporary:
                service = AgentService(root=Path(temporary), model_port=configured,
                                       load_external_configuration=False)
                try:
                    run_id = service.start(StartRequest(session_id="budget", objective="Inspect fixture",
                                                        targets=(temporary,))).single.run.run_id
                    def complete(live_request):
                        payload, _ = request_payload(configured, live_request)
                        assert payload[field] == live_request.metadata["reserved_output_tokens"]
                        assert payload[field] == (maximum or 16000)
                        return ModelResponse(request_id=live_request.request_id, status="completed", text="Done")
                    with patch.object(configured, "complete", side_effect=complete) as invoke:
                        service.run(run_id)
                    assert invoke.call_count == 1
                finally:
                    service.close()

    # Exercise the actual stream accumulator: completed metadata must survive.
    with tempfile.TemporaryDirectory(prefix="trace-refusal-") as temporary:
        fake = FakeModelProvider(streams=(ScriptedStream(tuple(chat_events)),))
        service = AgentService(root=Path(temporary), model_port=fake, model_streaming=True,
                               load_external_configuration=False)
        try:
            run_id = service.start(StartRequest(session_id="refusal", objective="Assess fixture://refusal",
                                                targets=("fixture://refusal",))).single.run.run_id
            live_request = ModelRequest("fixture-stream", run_id, ())
            service.model_loop._save_request(live_request)
            result = invoke_model_stream(service.model_loop, live_request)
            assert result.text == refusal and result.metadata["refusal"]
        finally:
            service.close()
    print(json.dumps({"ok": True, "checks": ["chat-refusal", "responses-refusal", "mixed-refusal",
          "chat-stream", "responses-stream", "truncated-stream", "native-tool-call",
          "output-budget", "stream-loop-metadata", "strict-usage", "actual-request-output-reserve",
          "failed-and-truncated-refusal-metadata"]}))


if __name__ == "__main__":
    main()
