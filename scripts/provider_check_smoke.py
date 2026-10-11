"""Local HTTP checks for explicit provider tests and credential edit semantics."""
from __future__ import annotations

import copy
import json
import os
import sys
import tempfile
import threading
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.application.bootstrap import configuration_projection, resolve_provider


@contextmanager
def endpoint():
    received = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            received.append((self.path, dict(self.headers), payload))
            model = payload["model"]
            key = self.headers.get("x-api-key") or self.headers.get("Authorization", "").removeprefix("Bearer ")
            code = 200
            if model == "http-fail":
                code, document = 401, {"error": {"code": "bad_key", "message": "rejected " + key}}
            elif model == "malformed":
                document = {"unexpected": True}
            elif self.path.endswith("/messages"):
                document = {"id": "msg-check", "type": "message", "role": "assistant", "model": model,
                            "content": [{"type": "text", "text": "OK"}], "stop_reason": "end_turn",
                            "usage": {"input_tokens": 5, "output_tokens": 1}}
            elif self.path.endswith("/responses"):
                document = {"id": "resp-check", "model": model, "status": "completed", "output": [
                    {"type": "message", "content": [{"type": "output_text", "text": "OK"}]}]}
            else:
                message = {"content": "echo " + key if model == "echo" else "OK"}
                finish = "length" if model == "truncated" else "stop"
                if model == "tool-call":
                    message = {"content": "", "tool_calls": [{"id": "call-check", "type": "function",
                        "function": {"name": "must_not_execute", "arguments": "{}"}}]}
                    finish = "tool_calls"
                document = {"model": key if model == "echo" else model, "choices": [
                    {"message": message, "finish_reason": finish}]}
            body = json.dumps(document).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1", received
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    checks = 0
    with tempfile.TemporaryDirectory(prefix="trace-provider-check-") as temporary, endpoint() as (url, calls), endpoint() as (other_url, other_calls):
        with patch.dict(os.environ, {"USERPROFILE": temporary, "REDTEAM_AGENT_HOME": temporary}, clear=True):
            service = AgentService(root=Path(temporary), load_external_configuration=False)
        try:
            control = service.control
            profile = {"provider_id": "saved", "name": "Saved", "provider": "openai-compatible",
                       "base_url": url, "model": "original", "timeout_seconds": 2}
            control.save_provider({**profile, "api_key": "saved-fixture-key"})
            control.save_provider({**profile, "model": "renamed", "api_key": ""})
            assert control.provider_secret("saved", include_environment=False) == "saved-fixture-key"
            checks += 1
            restarted = AgentService(root=Path(temporary), load_external_configuration=False)
            try:
                assert restarted.control.provider_secret("saved", include_environment=False) == "saved-fixture-key"
                assert restarted.control.provider("saved")["api_key_set"] is True
                checks += 2
            finally:
                restarted.close()
            for change in ({"base_url": other_url}, {"provider": "anthropic"},
                           {"api_key_env": "NEW_KEY"}, {"clear_api_key": True}):
                control.save_provider({**profile, **change})
                assert control.provider_secret("saved", include_environment=False) == ""
                control.save_provider({**profile, "api_key": "saved-fixture-key"})
                checks += 1
            control.activate_provider("saved")
            provider, _ = resolve_provider(control, (), environ={})
            service.configure_model(provider, model_name=provider.model)
            assert not calls and not other_calls, "save/activate must not test automatically"
            loop = service.model_loop
            config = copy.deepcopy(service.configuration_projection)

            def snapshot():
                with control.store.connection() as connection:
                    database = tuple(connection.iterdump())
                return database, dict(control._secrets)

            before = snapshot()
            with patch.dict(os.environ, {"USERPROFILE": temporary, "HOME": temporary,
                                         "REDTEAM_AGENT_HOME": temporary}, clear=True):
                projection = configuration_projection(Path(temporary), ())
                assert projection["provider"]["ready"] is True, projection
                assert projection["provider"]["model"] == "original"
                assert "saved-fixture-key" not in json.dumps(projection)
                assert snapshot() == before and not calls and not other_calls
                checks += 1
            with patch.dict(os.environ, {"TRACE_API_BASE_URL": "https://example.invalid/v1", "USERPROFILE": temporary,
                                         "HOME": temporary, "REDTEAM_AGENT_HOME": temporary}, clear=True):
                projection = configuration_projection(Path(temporary), ())
                assert projection["provider"]["ready"] is False, projection
                assert projection["provider"]["reason"] == "missing_credentials"
                assert snapshot() == before and not calls and not other_calls
                checks += 1

            def check(payload, *, ok=True, sent=1, target=calls):
                nonlocal checks
                count = len(target)
                result = control.test_provider(payload)
                assert result["ok"] is ok, result
                assert set(result) == {"ok", "reply", "model", "latency_ms", "error"}
                assert result["latency_ms"] >= 0
                assert bool(result["error"]) is not ok, result
                assert len(target) - count == sent, (payload, result)
                assert snapshot() == before, "test mutated durable settings or credentials"
                assert service.model_loop is loop and loop.model is provider
                assert service.configuration_projection == config
                assert "saved-fixture-key" not in json.dumps(result)
                if sent:
                    body = target[-1][2]
                    assert not body.get("tools") and not body.get("stream")
                checks += 1
                return result

            environment = {"TRACE_MODEL": "wrong-model", "TRACE_API_BASE_URL": "http://127.0.0.1:1",
                           "OPENAI_API_KEY": "ambient-fixture-key", "ANTHROPIC_API_KEY": "ambient-anthropic-key",
                           "CHECK_KEY": "environment-fixture-key", "NEW_KEY": "new-environment-key"}
            with patch.dict(os.environ, environment, clear=True):
                result = check({"provider_id": "saved", "model": "edited", "api_key": "",
                                "max_output_tokens": 123, "reasoning_effort": "high"})
                assert result["reply"] == "OK" and result["model"] == "edited"
                path, headers, body = calls[-1]
                assert path == "/v1/chat/completions" and headers["Authorization"] == "Bearer saved-fixture-key"
                assert body["model"] == "edited" and body["max_completion_tokens"] == 123
                assert body["reasoning_effort"] == "high"
                assert body["messages"] == [{"role": "user", "content": "Reply with OK."}]
                check({"provider_id": "saved", "base_url": url + "/"})
                assert calls[-1][1]["Authorization"] == "Bearer saved-fixture-key"
                check({"provider_id": "saved", "base_url": other_url}, target=other_calls)
                assert "Authorization" not in other_calls[-1][1]
                check({"provider_id": "saved", "provider": "anthropic"})
                assert calls[-1][0] == "/v1/messages" and "x-api-key" not in calls[-1][1]
                check({"provider_id": "saved", "base_url": other_url, "api_key": "replacement-key"}, target=other_calls)
                assert other_calls[-1][1]["Authorization"] == "Bearer replacement-key"
                check({"provider_id": "saved", "clear_api_key": True})
                assert "Authorization" not in calls[-1][1]
                for model in ("http-fail", "malformed", "truncated", "tool-call"):
                    check({"provider_id": "saved", "model": model}, ok=False)
                result = check({"provider_id": "saved", "model": "echo", "api_key": "echo-fixture-key"})
                assert "echo-fixture-key" not in json.dumps(result) and "[REDACTED]" in result["reply"]
                check({"base_url": url, "model": "new-unsaved"})
                assert "Authorization" not in calls[-1][1]
                check({"base_url": url + "/responses", "model": "responses", "max_output_tokens": 99})
                assert calls[-1][2]["max_output_tokens"] == 99
                check({"base_url": url, "provider": "anthropic", "model": "native",
                       "api_key": "native-fixture-key", "max_output_tokens": 128})
                assert calls[-1][1]["x-api-key"] == "native-fixture-key" and calls[-1][2]["max_tokens"] == 128
                for fields in ({"model": ""}, {"provider": "unsupported"}, {"timeout_seconds": -1},
                               {"base_url": "invalid"}, {"max_output_tokens": -1}, {"provider_id": "missing"}):
                    check({"provider_id": "saved", **fields}, ok=False, sent=0)
                check({"provider_id": "saved", "base_url": "https://example.invalid/v1"}, ok=False, sent=0)

                # Persisted environment credentials must not cross an edited endpoint/protocol.
                control.save_provider({**profile, "api_key_env": "CHECK_KEY", "api_key": "saved-fixture-key"})
                before = snapshot()
                check({"provider_id": "saved"})
                assert calls[-1][1]["Authorization"] == "Bearer environment-fixture-key"
                for fields in ({"base_url": other_url}, {"base_url": other_url, "api_key_env": "CHECK_KEY"}):
                    check({"provider_id": "saved", **fields}, target=other_calls)
                    assert "Authorization" not in other_calls[-1][1]
                check({"provider_id": "saved", "provider": "anthropic"})
                assert "x-api-key" not in calls[-1][1]
                check({"provider_id": "saved", "base_url": other_url, "api_key_env": "NEW_KEY"}, target=other_calls)
                assert other_calls[-1][1]["Authorization"] == "Bearer new-environment-key"
                check({"provider_id": "saved", "api_key": "explicit-key"})
                assert calls[-1][1]["Authorization"] == "Bearer explicit-key"
                result = check({"provider_id": "saved", "model": "echo"})
                assert "environment-fixture-key" not in json.dumps(result)
                check({"provider_id": "saved", "clear_api_key": True})
                assert "Authorization" not in calls[-1][1]
        finally:
            service.close()
    print(f"provider_check_smoke: PASS ({checks} checks; local HTTP only)")


if __name__ == "__main__":
    main()
