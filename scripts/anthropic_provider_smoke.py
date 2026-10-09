"""Offline native Messages regression: real HTTP/SSE, AgentService and cancellation."""
from __future__ import annotations

import io
import json
import os
import sys
import tempfile
import threading
from dataclasses import replace
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.application.bootstrap import resolve_provider, configuration_projection
from redteam_agent.core import ModelRequest
from redteam_agent.providers import AnthropicProvider, OpenAICompatibleProvider, ProviderHTTPError
from redteam_agent.providers.anthropic_protocol import message_response, request_payload, stream_events, usage_values
from redteam_agent.adapters.web import WebApi


def message(content, stop="end_turn"):
    return {"type": "message", "role": "assistant", "id": "msg-fixture", "model": "claude-fixture",
            "content": content, "stop_reason": stop, "usage": {"input_tokens": 10, "output_tokens": 5,
                "cache_read_input_tokens": 7, "cache_creation_input_tokens": 3}}


def sse(document):
    events = [{"type": "message_start", "message": {**document, "content": [], "stop_reason": None,
               "usage": {**document["usage"], "output_tokens": 0}}}]
    for index, block in enumerate(document["content"]):
        start = dict(block)
        deltas = []
        if block["type"] == "tool_use":
            start["input"] = {}
            encoded = json.dumps(block["input"])
            deltas = [{"type": "input_json_delta", "partial_json": part} for part in (encoded[:2], encoded[2:])]
        elif block["type"] == "thinking":
            start["signature"] = ""
            deltas = [{"type": "thinking_delta", "thinking": ""},
                      {"type": "signature_delta", "signature": block["signature"]}]
        elif block["type"] == "text":
            start["text"] = ""
            deltas = [{"type": "text_delta", "text": block["text"]}]
        events.append({"type": "content_block_start", "index": index, "content_block": start})
        events.extend({"type": "content_block_delta", "index": index, "delta": delta} for delta in deltas)
        events.append({"type": "content_block_stop", "index": index})
    events.extend([{"type": "message_delta", "delta": {"stop_reason": document["stop_reason"]},
                    "usage": {"output_tokens": 5}}, {"type": "message_stop"}])
    return b"".join(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode() for event in events)


@contextmanager
def endpoint(responder):
    received, errors = [], []
    entered, release = threading.Event(), threading.Event()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            received.append(payload)
            try:
                assert self.path == "/v1/messages"
                assert self.headers.get("x-api-key") == "fixture-key"
                assert self.headers.get("Authorization") is None
                assert self.headers.get("anthropic-version") == "2023-06-01"
                result = responder(payload, len(received))
                if result is None:
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    self.wfile.flush()
                    entered.set()
                    release.wait(5)
                    return
                status, document = result if isinstance(result, tuple) else (200, result)
                raw = sse(document) if payload.get("stream") and status == 200 else json.dumps(document).encode()
                self.send_response(status)
                self.send_header("Content-Type", "text/event-stream" if payload.get("stream") and status == 200 else "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            except Exception as error:
                errors.append(error)
                self.send_error(500)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1", received, entered, release
        assert not errors, errors
    finally:
        release.set()
        server.shutdown()
        server.server_close()
        thread.join(2)


def loop_check(streaming):
    tool_name = OpenAICompatibleProvider._encoded_tool_name("agent:transcript")
    initial = message([
        {"type": "thinking", "thinking": "", "signature": "opaque-signature-fixture"},
        {"type": "text", "text": "Read local transcript."},
        {"type": "redacted_thinking", "data": "encrypted-fixture"},
        {"type": "tool_use", "id": "tool-fixture", "name": tool_name, "input": {"limit": 1}},
        {"type": "tool_use", "id": "tool-fixture-second", "name": tool_name, "input": {"limit": 2}},
    ], "tool_use")

    def respond(payload, count):
        assert payload["thinking"] == {"type": "adaptive", "display": "omitted"}
        assert payload["output_config"] == {"effort": "high"}
        assert payload["max_tokens"] == 8192
        assert all(item["role"] in {"user", "assistant"} for item in payload["messages"])
        if count == 1:
            assert any(tool["name"] == tool_name for tool in payload["tools"])
            return initial
        assert count == 2
        assistant = next(item for item in payload["messages"] if item["role"] == "assistant")
        assert assistant["content"] == initial["content"], assistant
        results = [block for item in payload["messages"] for block in item["content"] if block["type"] == "tool_result"]
        assert {item["tool_use_id"] for item in results} == {"tool-fixture", "tool-fixture-second"}
        assert len(results) == 2
        return message([{"type": "text", "text": '{"decision":"waiting_input","reason":"fixture verified"}'}])

    with endpoint(respond) as (base_url, received, _, _), tempfile.TemporaryDirectory(prefix="trace-anthropic-") as temporary:
        provider = AnthropicProvider(base_url, "claude-fixture", "fixture-key", max_output_tokens=8192,
                                     thinking_type="adaptive", reasoning_effort="high")
        service = AgentService(root=Path(temporary), model_port=provider, model_streaming=streaming,
                               load_external_configuration=False)
        try:
            run_id = service.start({"session_id": "anthropic", "objective": "Inspect fixture://anthropic",
                                    "targets": ["fixture://anthropic"]}).runs[0].run.run_id
            result = service.run(run_id)
            assert len(received) == 2
            assert result.run.budget.pause_reason == "waiting_input"
            records = service.journal.model_responses(run_id)
            ref = records[0].response["continuation"]
            state = service.runtime.artifacts.read_json(ref["artifact_id"], run_id=run_id)
            assert state["thinking_blocks"][0]["signature"] == "opaque-signature-fixture"
            assert '"thinking":' not in json.dumps(state)
            assert records[0].response["usage"]["total_tokens"] == 25
            profile = service.control.save_provider({"provider_id": "native", "name": "Native", "provider": "anthropic",
                "base_url": base_url, "model": "claude-fixture", "max_output_tokens": 8192,
                "thinking_type": "adaptive", "reasoning_effort": "high", "api_key": "fixture-key"})
            assert profile["provider"] == "anthropic" and "api_key" not in profile
            service.control.activate_provider("native")
            restored, sources = resolve_provider(service.control, (), environ={})
            assert isinstance(restored, AnthropicProvider) and restored.max_output_tokens == 8192
            assert sources["thinking_type"] == "persisted:active_provider"
            with patch.dict(os.environ, {"REDTEAM_AGENT_HOME": temporary}, clear=True):
                assert configuration_projection(Path(temporary), [])["provider"]["configured"]
        finally:
            service.close()


def rejects(operation, error=ValueError):
    try:
        operation()
    except error:
        return
    raise AssertionError("invalid input accepted")


def binding_and_boundary_checks():
    provider = AnthropicProvider("http://127.0.0.1:1/v1", "claude-fixture")
    request = ModelRequest("first", "offline", ())
    states = [message_response(provider, replace(request, request_id=identity), message([
        {"type": "thinking", "thinking": "", "signature": "sig-" + identity},
        {"type": "text", "text": "ok"}]), {}).continuation for identity in ("first", "second")]
    messages = tuple({"role": "assistant", "content": "ok", "source_request_id": identity}
                     if index % 2 else {"role": "user", "content": "next"}
                     for index, identity in enumerate(("", "first", "", "second", "")))
    request = replace(request, messages=messages, continuation={"chain": states})
    payload, _ = request_payload(provider, request)
    assistants = [item for item in payload["messages"] if item["role"] == "assistant"]
    assert [item["content"][0]["signature"] for item in assistants] == ["sig-first", "sig-second"]
    assert not any("source_request_id" in item for item in payload["messages"])
    compacted = replace(request, messages=messages[2:])
    assistants = [item for item in request_payload(provider, compacted)[0]["messages"] if item["role"] == "assistant"]
    assert len(assistants) == 1 and assistants[0]["content"][0]["signature"] == "sig-second"
    legacy = [{key: value for key, value in state.items() if key != "assistant_request_id"} for state in states]
    unsigned = request_payload(provider, replace(compacted, continuation={"chain": legacy}))[0]
    assert "signature" not in json.dumps(unsigned)
    tool = {"name": "fixture", "input_schema": {"type": "object"}}
    calls = [{"call_id": "reused", "tool_name": "fixture", "arguments": {}}]
    tool_messages = tuple({"role": "assistant", "source_request_id": identity,
        "content": {"text": "ok", "tool_calls": calls}} for identity in ("first", "second"))
    tool_state = message_response(provider, replace(request, request_id="first"), message([
        {"type": "thinking", "thinking": "", "signature": "sig-first"},
        {"type": "text", "text": "ok"},
        {"type": "tool_use", "name": "fixture", "id": "reused", "input": {}}], "tool_use"), {"fixture": "fixture"}).continuation
    compacted_tool = replace(request, tools=(tool,), messages=(tool_messages[1],), continuation={"chain": [tool_state]})
    assert "signature" not in json.dumps(request_payload(provider, compacted_tool)[0])
    for value in (True, -0.5, 1.9, 2.0, "1.9", -1):
        for factory in (AnthropicProvider, OpenAICompatibleProvider):
            rejects(lambda: factory("http://127.0.0.1:1/v1", "fixture", max_output_tokens=value))
        rejects(lambda: resolve_provider(None, (), {"model": "fixture", "max_output_tokens": value}, environ={}))
        rejects(lambda: request_payload(provider, replace(request, metadata={"reserved_output_tokens": value})))
    for value in (-1, 1.9, True, "1", None):
        for name in ("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"):
            rejects(lambda: usage_values({"input_tokens": 10, "output_tokens": 5, name: value}), RuntimeError)
    raw = sse(message([{"type": "text", "text": "ok"}]))
    for event_type in ("content_block_start", "content_block_delta", "content_block_stop"):
        for value in (False, -1, 0.0, "0"):
            replaced = raw.replace(json.dumps({"type": event_type, "index": 0})[:-1].encode(),
                json.dumps({"type": event_type, "index": value})[:-1].encode())
            rejects(lambda: list(stream_events(provider, request, io.BytesIO(replaced), {}, byte_limit=65536)), RuntimeError)
    for value in (-1, 1.5, True):
        malformed = message([{"type": "text", "text": "ok"}])
        malformed["usage"]["cache_read_input_tokens"] = value
        rejects(lambda: list(stream_events(provider, request, io.BytesIO(sse(malformed)), {}, byte_limit=65536)), RuntimeError)


def web_configuration_checks():
    with tempfile.TemporaryDirectory(prefix="trace-anthropic-config-") as temporary:
        service = AgentService(root=Path(temporary), load_external_configuration=False)
        try:
            profile = {"provider_id": "openai", "name": "OpenAI", "model": "fixture", "base_url": "http://127.0.0.1:1/v1"}
            for value in (True, 1.9, -0.5):
                rejects(lambda: service.control.save_provider({**profile, "max_output_tokens": value}))
            service.control.save_provider(profile)
            api = WebApi(service)
            environment = {"REDTEAM_AGENT_HOME": temporary, "TRACE_THINKING_TYPE": "adaptive", "TRACE_REASONING_EFFORT": "high"}
            with patch.dict(os.environ, environment, clear=True):
                response = api.dispatch("POST", "/api/providers/active", body={"provider_id": "openai"})
                assert response.status == 200, response.body
                assert service.model_loop.model.reasoning_effort == ""
                assert service.control.provider("openai")["active"]
                inherited, _ = resolve_provider(None, (), {"model": "fixture", "provider": "anthropic",
                    "thinking_type": None, "reasoning_effort": None})
                assert inherited.thinking_type == "adaptive" and inherited.reasoning_effort == "high"
                service.control.save_provider({**profile, "provider_id": "other"})
                before = service.model_loop
                with patch.object(service, "configure_model", side_effect=ValueError("fixture_configuration_failed")):
                    response = api.dispatch("POST", "/api/providers/active", body={"provider_id": "other"})
                    assert response.status == 400
                assert service.model_loop is before and service.control.provider("openai")["active"]
                assert not service.control.provider("other")["active"]
                with patch.object(service.control, "activate_provider", side_effect=ValueError("fixture_commit_failed")):
                    response = api.dispatch("POST", "/api/providers/active", body={"provider_id": "other"})
                    assert response.status == 400
                assert service.model_loop is before and service.control.provider("openai")["active"]
                response = api.dispatch("POST", "/api/providers", body={**profile,
                    "model": "updated-fixture", "api_key": "updated-fixture-key"})
                assert response.status == 201
                assert service.model_loop.model.model == "updated-fixture"
                assert service.model_loop.model._credential() == "updated-fixture-key"
                before = service.model_loop
                saved_before = service.control.provider("openai")
                sources_before = dict(service.configuration_projection["provider_sources"])
                original_configure = service.configure_model
                def configure_then_fail(*args, **kwargs):
                    assert service._write_local.depth > 0
                    original_configure(*args, **kwargs)
                    raise ValueError("fixture_configuration_failed")
                with patch.object(service, "configure_model", side_effect=configure_then_fail):
                    response = api.dispatch("POST", "/api/providers", body={**profile,
                        "model": "failed-edit", "api_key": "failed-fixture-key"})
                    assert response.status == 400
                assert service.model_loop is before and service.control.provider("openai") == saved_before
                assert service.control.provider_secret("openai", include_environment=False) == "updated-fixture-key"
                assert service.configuration_projection["provider_sources"] == sources_before
                transaction = service.runtime.store.transaction
                @contextmanager
                def fail_candidate_commit(*, immediate=False):
                    with transaction(immediate=immediate) as connection:
                        yield connection
                        row = connection.execute("SELECT model FROM trace_providers WHERE provider_id='openai'").fetchone()
                        if row["model"] == "failed-commit":
                            assert service.model_loop.model.model == "failed-commit"
                            raise ValueError("fixture_commit_failed")
                with patch.object(service.runtime.store, "transaction", fail_candidate_commit):
                    response = api.dispatch("POST", "/api/providers", body={**profile,
                        "model": "failed-commit", "api_key": "uncommitted-fixture-key"})
                    assert response.status == 400
                assert service.model_loop is before and service.control.provider("openai") == saved_before
                assert service.control.provider_secret("openai", include_environment=False) == "updated-fixture-key"
                assert service.configuration_projection["provider_sources"] == sources_before
        finally:
            service.close()


def main():
    binding_and_boundary_checks()
    web_configuration_checks()
    checks = []
    for streaming in (False, True):
        loop_check(streaming)
        checks.append("sse-loop-continuation" if streaming else "json-loop-continuation")
    request = ModelRequest("offline", "offline", ({"role": "user", "content": "hello"},),
                           metadata={"reserved_output_tokens": 2048})
    provider = AnthropicProvider("http://127.0.0.1:1/v1", "claude-fixture")
    assert request_payload(provider, request)[0]["max_tokens"] == 2048
    native, sources = resolve_provider(None, (), {"model": "claude-fixture"}, environ={"TRACE_PROVIDER": "anthropic"})
    assert isinstance(native, AnthropicProvider) and native._api_key_env == "ANTHROPIC_API_KEY"
    manual = AnthropicProvider("http://127.0.0.1:1/v1", "claude-fixture", thinking_type="enabled",
                               thinking_budget_tokens=1024, max_output_tokens=2048)
    assert request_payload(manual, request)[0]["thinking"] == {
        "type": "enabled", "budget_tokens": 1024, "display": "omitted"}
    for invalid in ({"thinking_type": "enabled", "thinking_budget_tokens": 1023},
                    {"thinking_type": "adaptive", "thinking_budget_tokens": 1024},
                    {"thinking_type": "enabled", "thinking_budget_tokens": 2048, "max_output_tokens": 2048}):
        try:
            AnthropicProvider("http://127.0.0.1:1/v1", "claude-fixture", **invalid)
            raise AssertionError("invalid thinking budget accepted")
        except ValueError:
            pass
    document = message([{"type": "text", "text": "No"}], "refusal")
    result = message_response(provider, request, document, {})
    assert result.metadata["refusal"] and result.status == "completed" and not result.tool_calls
    interrupted = message_response(provider, request, message([], "max_tokens"), {})
    assert interrupted.status == "interrupted"
    for streaming in (False, True):
        plain = message([{"type": "thinking", "thinking": "must-not-persist", "signature": "opaque"}])
        try:
            if streaming:
                raw = sse(plain).replace(b'"thinking": ""', b'"thinking": "must-not-persist"')
                list(stream_events(provider, request, io.BytesIO(raw), {}, byte_limit=65536))
            else:
                message_response(provider, request, plain, {})
            raise AssertionError("plaintext thinking accepted")
        except RuntimeError as error:
            assert str(error) == "provider_omitted_thinking_required"
    assert provider.capabilities().metadata["continuation_scope"] != OpenAICompatibleProvider(
        "http://127.0.0.1:1/v1", "claude-fixture").capabilities().metadata["continuation_scope"]
    truncated = sse(message([{"type": "text", "text": "partial"}])).split(b"event: message_stop")[0]
    for raw in (truncated, b'data: {"type":"error","error":{"type":"overloaded_error"}}\n\n'):
        try:
            list(stream_events(provider, request, io.BytesIO(raw), {}, byte_limit=65536))
            raise AssertionError("malformed stream accepted")
        except RuntimeError:
            pass
    checks.extend(["defaults-and-output-budget", "manual-thinking-validation", "parallel-tool-results",
                   "refusal", "truncation", "sse-error",
                   "plaintext-thinking-rejected", "protocol-continuation-isolation"])
    with endpoint(lambda *_: (401, {"error": {"type": "authentication_error", "message": "fixture-key rejected"}})) as (url, _, _, _):
        try:
            AnthropicProvider(url, "claude-fixture", "fixture-key").complete(request)
            raise AssertionError("http error accepted")
        except ProviderHTTPError as error:
            assert error.status == 401 and "fixture-key" not in str(error)
    with endpoint(lambda *_: None) as (url, _, entered, release):
        blocked = AnthropicProvider(url, "claude-fixture", "fixture-key")
        errors = []
        def consume():
            try:
                list(blocked.stream(request))
            except Exception as error:
                errors.append(str(error))
        thread = threading.Thread(target=consume)
        thread.start()
        assert entered.wait(2)
        assert blocked.cancel(request.request_id)
        thread.join(2)
        release.set()
        assert not thread.is_alive() and errors == ["provider_request_cancelled"]
        assert not blocked.cancel(request.request_id)
    checks.extend(["http-error-redaction", "cancel-blocked-sse", "duplicate-text-identity-binding",
        "compaction-signature-isolation", "noninteger-token-limits", "strict-usage-and-sse-index",
        "web-explicit-empty-overrides", "web-activation-failure-rollback",
        "web-active-edit-and-key-update", "web-active-edit-configure-and-commit-rollback"])
    print(json.dumps({"ok": True, "checks": checks}))


if __name__ == "__main__":
    main()
