from __future__ import annotations

import json
import math
from contextlib import contextmanager
from dataclasses import replace
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from socketserver import BaseRequestHandler, ThreadingTCPServer
from threading import Event, Thread
from typing import Any

import pytest

from redteam_agent import AgentService
from redteam_agent.core import ModelRequest, ToolDefinition, ToolResult
from redteam_agent.providers import OpenAICompatibleProvider, ProviderHTTPError
from redteam_agent.providers import openai_compatible as provider_module


@contextmanager
def _provider_server(*, status: int = 200, response: Any = None, content_type: str = "application/json"):
    captured: dict[str, Any] = {}

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length))
            captured.update(path=self.path, headers=dict(self.headers), payload=payload)
            captured.setdefault("requests", []).append(payload)
            value = response(payload) if callable(response) else response
            raw = (
                json.dumps(value).encode()
                if content_type == "application/json"
                else str(value).encode()
            )
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, format: str, *args: Any) -> None:
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1", captured
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def _request() -> ModelRequest:
    return ModelRequest(
        request_id="request-1",
        run_id="run-1",
        model="trace-model",
        messages=(
            {"role": "system", "content": {"invariant": "evidence first"}},
            {"role": "user", "content": "inspect target"},
            {
                "role": "assistant",
                "content": {
                    "text": "",
                    "structured_output": {"decision": "request"},
                    "tool_calls": [
                        {
                            "call_id": "prior-call",
                            "tool_name": "builtin:http-request",
                            "arguments": {"url": "https://prior.test"},
                        }
                    ],
                },
            },
            {
                "role": "tool",
                "content": {
                    "call_id": "prior-call",
                    "tool_name": "builtin:http-request",
                    "status": "success",
                    "projection": {"status_code": 200},
                },
            },
        ),
        tools=(
            {
                "type": "function",
                "name": "builtin:http-request",
                "description": "Issue HTTP request",
                "input_schema": {
                    "type": "object",
                    "properties": {"url": {"type": "string"}},
                    "required": ["url"],
                },
            },
        ),
        response_schema={"type": "object", "properties": {"decision": {"type": "string"}}},
        allow_parallel_tools=True,
    )


def test_provider_maps_chat_tools_structured_output_and_usage() -> None:
    def response(payload: dict[str, Any]) -> dict[str, Any]:
        safe_name = payload["tools"][0]["function"]["name"]
        return {
            "id": "response-1",
            "model": "trace-model-2026",
            "choices": [
                {
                    "finish_reason": "tool_calls",
                    "message": {
                        "content": '{"decision":"probe"}',
                        "tool_calls": [
                            {
                                "id": "call-1",
                                "type": "function",
                                "function": {
                                    "name": safe_name,
                                    "arguments": '{"url":"https://fixture.test"}',
                                },
                            }
                        ],
                    },
                }
            ],
            "usage": {
                "prompt_tokens": 17,
                "completion_tokens": 5,
                "total_tokens": 22,
                "prompt_tokens_details": {"cached_tokens": 7},
                "completion_tokens_details": {"reasoning_tokens": 2},
            },
        }

    with _provider_server(response=response) as (base_url, captured):
        provider = OpenAICompatibleProvider(base_url, "trace-model", "secret-key")
        result = provider.complete(_request())

    assert captured["path"] == "/v1/chat/completions"
    assert captured["headers"]["Authorization"] == "Bearer secret-key"
    assert json.loads(captured["payload"]["messages"][0]["content"]) == {
        "invariant": "evidence first"
    }
    assistant = captured["payload"]["messages"][2]
    tool_result = captured["payload"]["messages"][3]
    assert json.loads(assistant["content"]) == {"decision": "request"}
    assert assistant["tool_calls"][0]["id"] == "prior-call"
    assert assistant["tool_calls"][0]["function"]["name"] == captured["payload"]["tools"][0]["function"]["name"]
    assert tool_result["tool_call_id"] == "prior-call"
    assert captured["payload"]["parallel_tool_calls"] is True
    assert captured["payload"]["response_format"]["json_schema"]["schema"]["type"] == "object"
    encoded = captured["payload"]["tools"][0]["function"]["name"]
    assert ":" not in encoded
    assert result.structured_output == {"decision": "probe"}
    assert result.tool_calls == (
        {
            "call_id": "call-1",
            "tool_name": "builtin:http-request",
            "arguments": {"url": "https://fixture.test"},
        },
    )
    assert result.usage == {
        "input_tokens": 17,
        "output_tokens": 5,
        "total_tokens": 22,
        "cache_read_tokens": 7,
        "reasoning_tokens": 2,
    }
    assert provider.capabilities().streaming is False
    assert provider.cancel("completed-request") is False


@pytest.mark.parametrize(
    ("content_type", "response", "expected"),
    [
        ("application/json", {"error": {"code": "rate_limit", "message": "slow down"}}, "rate_limit"),
        ("text/plain", "gateway unavailable", "gateway unavailable"),
    ],
)
def test_provider_bounds_and_surfaces_http_errors(
    content_type: str, response: Any, expected: str
) -> None:
    with _provider_server(status=429, response=response, content_type=content_type) as (base_url, _):
        provider = OpenAICompatibleProvider(base_url, "trace-model")
        with pytest.raises(ProviderHTTPError) as raised:
            provider.complete(_request())

    assert raised.value.status == 429
    assert expected in str(raised.value)


def test_provider_rejects_unrecognized_tool_call_name() -> None:
    response = {
        "choices": [
            {
                "finish_reason": "tool_calls",
                "message": {
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call-1",
                            "function": {"name": "invented_tool", "arguments": "{}"},
                        }
                    ],
                },
            }
        ]
    }
    with _provider_server(response=response) as (base_url, _):
        provider = OpenAICompatibleProvider(base_url, "trace-model")
        with pytest.raises(RuntimeError, match="provider_tool_call_unknown"):
            provider.complete(_request())


@pytest.mark.parametrize(
    ("finish_reason", "status"),
    [("length", "interrupted"), ("max_tokens", "interrupted"), ("content_filter", "failed")],
)
def test_provider_does_not_execute_nonterminal_finish_reasons(
    finish_reason: str, status: str
) -> None:
    response = {
        "choices": [
            {
                "finish_reason": finish_reason,
                "message": {
                    "content": "partial",
                    "tool_calls": [
                        {
                            "id": "should-not-run",
                            "function": {
                                "name": "builtin_http-request",
                                "arguments": "{}",
                            },
                        }
                    ],
                },
            }
        ],
        "usage": {"prompt_tokens": 2, "completion_tokens": 1},
    }
    with _provider_server(response=response) as (base_url, _):
        provider = OpenAICompatibleProvider(base_url, "trace-model")
        result = provider.complete(_request())
    assert result.status == status
    assert result.tool_calls == ()
    assert result.finish_reason == finish_reason
    assert result.usage["input_tokens"] == 2


def test_provider_validates_connection_configuration() -> None:
    with pytest.raises(ValueError, match="provider_base_url_invalid"):
        OpenAICompatibleProvider("file:///tmp/api", "model")
    with pytest.raises(ValueError, match="provider_model_required"):
        OpenAICompatibleProvider("http://localhost:8000/v1", "")
    for value in (math.nan, math.inf, -math.inf):
        with pytest.raises(ValueError, match="provider_timeout_must_be_positive"):
            OpenAICompatibleProvider("http://localhost:8000/v1", "model", timeout_seconds=value)


@pytest.mark.parametrize("ledger_format", [False, True])
def test_provider_preserves_tool_history_on_second_http_request(ledger_format: bool) -> None:
    def response(payload: dict[str, Any]) -> dict[str, Any]:
        return {"choices": [{"message": {"content": None, "tool_calls": [{
            "id": "call-history", "type": "function", "function": {
                "name": payload["tools"][0]["function"]["name"], "arguments": '{"url":"x"}'
            }
        }]}, "finish_reason": "tool_calls"}]}

    request = _request()
    with _provider_server(response=response) as (base_url, captured):
        provider = OpenAICompatibleProvider(base_url, "trace-model")
        first = provider.complete(request)
        if ledger_format:
            assistant = {"role": "assistant", "content": {
                "text": first.text, "structured_output": {}, "tool_calls": list(first.tool_calls),
                "status": "completed", "finish_reason": "tool_calls",
            }}
            tool = {"role": "tool", "content": {
                "call_id": "call-history", "tool_name": "builtin:http-request", "output": "ok"
            }}
        else:
            assistant = {"role": "assistant", "content": None, "tool_calls": [{
                "id": "call-history", "type": "function", "function": {
                    "name": "builtin:http-request", "arguments": '{"url":"x"}'
                }
            }]}
            tool = {"role": "tool", "content": "ok", "tool_call_id": "call-history"}
        provider.complete(replace(request, messages=(*request.messages, assistant, tool)))

    messages = captured["payload"]["messages"]
    assert messages[-2]["tool_calls"][0] == {
        "id": "call-history", "type": "function", "function": {
            "name": captured["payload"]["tools"][0]["function"]["name"],
            "arguments": '{"url":"x"}',
        },
    }
    assert messages[-2]["content"] in (None, "")
    assert messages[-1]["tool_call_id"] == "call-history"


def test_provider_preserves_native_content_parts() -> None:
    parts = [{"type": "text", "text": "inspect target"}]
    request = replace(_request(), messages=({"role": "user", "content": parts},))
    with _provider_server(response={"choices": [{"message": {"content": "ok"}}]}) as (url, captured):
        OpenAICompatibleProvider(url, "trace-model").complete(request)
    assert captured["payload"]["messages"][0]["content"] == parts


@pytest.mark.parametrize("field", ["message", "code", "plain", "truncated"])
def test_provider_redacts_api_key_from_http_errors(field: str) -> None:
    api_key = "sk-fixture-private-123456789"
    value = ("x" * 2043 if field == "truncated" else "invalid API key: ") + api_key
    response = value if field == "plain" else {"error": {field if field != "truncated" else "message": value}}
    with _provider_server(status=401, response=response,
                          content_type="text/plain" if field == "plain" else "application/json") as (url, _):
        with pytest.raises(ProviderHTTPError) as raised:
            OpenAICompatibleProvider(url, "trace-model", api_key).complete(_request())
    assert api_key not in str(raised.value)
    assert "sk-fi" not in str(raised.value)
    assert "[REDACTED]" in str(raised.value) or field == "truncated"


@pytest.mark.parametrize("phase", ["headers", "body"])
@pytest.mark.parametrize("http_version", ["HTTP/1.0", "HTTP/1.1"])
def test_provider_cancel_interrupts_in_flight_http_request(phase: str, http_version: str) -> None:
    entered, release, finished, cancelled = Event(), Event(), Event(), Event()
    outcomes: list[Any] = []

    class Handler(BaseHTTPRequestHandler):
        protocol_version = http_version

        def do_POST(self) -> None:  # noqa: N802
            self.rfile.read(int(self.headers["Content-Length"]))
            raw = b'{"choices":[{"message":{"content":"late response"}}]}'
            if phase == "headers":
                entered.set()
                release.wait(5)
            try:
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                if phase == "body":
                    self.wfile.write(raw[:1])
                    self.wfile.flush()
                    entered.set()
                    release.wait(5)
                    raw = raw[1:]
                self.wfile.write(raw)
            except OSError:
                pass

        def log_message(self, format: str, *args: Any) -> None:
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    serving = Thread(target=server.serve_forever, daemon=True)
    serving.start()
    provider = OpenAICompatibleProvider(f"http://127.0.0.1:{server.server_port}/v1", "model", timeout_seconds=5)

    def complete() -> None:
        try:
            outcomes.append(provider.complete(_request()))
        except Exception as exc:
            outcomes.append(exc)
        finally:
            finished.set()

    def cancel() -> None:
        outcomes.append(provider.cancel("request-1"))
        cancelled.set()

    worker = Thread(target=complete, daemon=True)
    canceller = Thread(target=cancel, daemon=True)
    worker.start()
    try:
        assert entered.wait(2)
        canceller.start()
        prompt_cancel = cancelled.wait(0.75)
        prompt_completion = finished.wait(0.75)
    finally:
        release.set()
        worker.join(3)
        if canceller.ident is not None:
            canceller.join(3)
        server.shutdown()
        server.server_close()
        serving.join(3)
    assert prompt_cancel, "cancel blocked until the remote server released its response"
    assert prompt_completion, "complete remained blocked after cancel returned"
    assert True in outcomes
    assert any(isinstance(item, RuntimeError) and str(item) == "provider_request_cancelled" for item in outcomes)
    assert provider.cancel("request-1") is False


def test_provider_cancel_interrupts_tls_handshake() -> None:
    entered, release, finished = Event(), Event(), Event()
    outcomes: list[Any] = []

    class Handler(BaseRequestHandler):
        def handle(self) -> None:
            self.request.recv(4096)
            entered.set()
            release.wait(4)

    server = ThreadingTCPServer(("127.0.0.1", 0), Handler)
    serving = Thread(target=server.serve_forever, daemon=True)
    serving.start()
    provider = OpenAICompatibleProvider(
        f"https://127.0.0.1:{server.server_address[1]}/v1",
        "model",
        timeout_seconds=3,
    )

    def complete() -> None:
        try:
            outcomes.append(provider.complete(_request()))
        except Exception as exc:
            outcomes.append(exc)
        finally:
            finished.set()

    worker = Thread(target=complete, daemon=True)
    worker.start()
    try:
        assert entered.wait(2)
        assert provider.cancel("request-1") is True
        assert finished.wait(0.75)
    finally:
        release.set()
        worker.join(3)
        server.shutdown()
        server.server_close()
        serving.join(3)
    assert any(
        isinstance(item, RuntimeError) and str(item) == "provider_request_cancelled"
        for item in outcomes
    )


@pytest.mark.parametrize("status,limit_name", [(200, "MAX_RESPONSE_BYTES"), (429, "MAX_ERROR_BYTES")])
def test_provider_enforces_http_body_size_limit(monkeypatch: pytest.MonkeyPatch, status: int, limit_name: str) -> None:
    monkeypatch.setattr(provider_module, limit_name, 64)
    with _provider_server(status=status, response="x" * 65, content_type="text/plain") as (url, _):
        with pytest.raises(RuntimeError, match="provider_response_too_large"):
            OpenAICompatibleProvider(url, "model").complete(_request())


def test_provider_drives_agent_service_across_tool_turns(tmp_path) -> None:
    class Tools:
        def discover(self):
            return (
                ToolDefinition(
                    qualified_name="fixture:target",
                    name="target",
                    server="fixture",
                    description="Return the local target",
                    input_schema={"type": "object", "properties": {}},
                    capabilities=("target_input",),
                    metadata={"source": "builtin"},
                ),
            )

        def invoke(self, call):
            return ToolResult(call.call_id, "success", call.tool_name, {"target": "fixture.local"})

        def reconcile(self, call):
            return None

        def cancel(self, call_id):
            return True

    call_number = 0

    def response(payload: dict[str, Any]) -> dict[str, Any]:
        nonlocal call_number
        call_number += 1
        name = payload["tools"][0]["function"]["name"]
        tool_calls = [
            {
                "id": f"call-{call_number}",
                "function": {"name": name, "arguments": "{}"},
            }
        ]
        return {
            "id": f"response-{call_number}",
            "model": "trace-model",
            "choices": [
                {
                    "finish_reason": "tool_calls",
                    "message": {
                        "content": '{"decision":"invoke"}',
                        "tool_calls": tool_calls,
                    },
                }
            ],
            "usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
        }

    with _provider_server(response=response) as (base_url, captured):
        service = AgentService(
            root=tmp_path / "runtime",
            model_port=OpenAICompatibleProvider(base_url, "trace-model"),
            tool_port=Tools(),
            model_name="trace-model",
            model_max_turns=2,
        )
        try:
            run_id = service.start(
                {"session_id": "provider-e2e", "objective": "Inspect the supplied target"}
            ).single.run.run_id
            result = service.run(run_id)
        finally:
            service.close()

    assert result.goal.targets == ("fixture.local",)
    assert len(captured["requests"]) == 2
    follow_up = captured["requests"][1]["messages"]
    assistant = next(item for item in follow_up if item["role"] == "assistant")
    tool_result = next(item for item in follow_up if item["role"] == "tool")
    assert assistant["tool_calls"][0]["id"] == tool_result["tool_call_id"] == "call-1"
    assert result.run.status in {"running", "waiting_worker"}
