"""Offline checks for bounded history, explicit Responses replay and request deadlines."""
from __future__ import annotations

import io
import hashlib
import json
import sys
import threading
import time
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from model_recovery_smoke import scenario, response
from task_protection_smoke import declare, hypothesis
from redteam_agent.core import ModelRequest, ModelResponse, ModelStreamEvent
from redteam_agent.providers import AnthropicProvider, FakeModelProvider, OpenAICompatibleProvider, ProviderHTTPError
from redteam_agent.providers.openai_protocol import request_payload, responses_response
from redteam_agent.providers.anthropic_protocol import request_payload as anthropic_payload, message_response
from redteam_agent.providers.openai_compatible import retry_after_seconds
from redteam_agent.application.model_continuation import prepare_continuation, hydrate_continuation
from redteam_agent.application.model_loop import ModelIntegrityError, ModelLoopError
from redteam_agent.application.model_turn import invoke_with_deadline, retry_backoff
from redteam_agent.application.context_retention import retained_tactical_state


def deadline(seconds):
    return (datetime.now(timezone.utc) + timedelta(seconds=seconds)).isoformat()


def check_history():
    nested = retained_tactical_state([SimpleNamespace(source_type="fixture", content={
        "hypotheses": [{"id": "h1", "observations": {"note": "nested"}},
                       {"id": "h2", "metadata": {"confidence": "unverified"}}]})])
    assert [item["id"] for item in nested["unverified_hypotheses"]] == ["h1", "h2"]
    with scenario([]) as (service, _provider, run_id):
        hypothesis(service, run_id, "current-focus")
        declare(service, run_id, "current-focus")
        for index in range(90):
            service.conversation.append(
                run_id=run_id, role="assistant", protected=False, source_type="fixture",
                source_id=str(index), content={"hypotheses": [{"id": str(index), "statement": "x" * 800}],
                                              "artifact_refs": ["artifact-" + str(index)]},
            )
        selector = service.context_selector
        selection = selector.prepare_model_context(service.status(run_id), max_context_tokens=16000,
                                                   force_compaction=True)
        retained = selection.protected_context["active_plan"]["retained_tactical_state"]
        assert selection.context_status == "ready", selection.context_status
        assert len(retained["unverified_hypotheses"]) == 8
        assert retained["unverified_hypotheses"][0]["id"] == "89"
        assert len(retained["referenced_artifacts"]) == 32
        assert selection.protected_context["current_task"]["focus"]["hypothesis_id"] == "current-focus"
        assert selection.protected_context["original_goal"]["objective"] == "Inspect local fixture"
        assert len(service.conversation.messages(run_id)) > 90
        for index in range(270):
            service.conversation.append(run_id=run_id, role="user", protected=False, content={"index": index},
                                        source_type="fixture-tail", source_id=str(index))
        retained = selector._retained_tactical_state(run_id)
        assert retained["historical_projection"]["recent_messages_scanned"] == 256
        assert retained["historical_projection"]["truncated"]


def check_responses():
    wire = OpenAICompatibleProvider("http://127.0.0.1:1/v1/responses", "fixture", environ={})
    calls, received = [], []

    def respond(request):
        payload, names = request_payload(wire, request)
        received.append(payload)
        assert "previous_response_id" not in payload
        if len(received) == 1:
            encoded = wire._encoded_tool_name("agent:transcript")
            calls.extend({"type": "function_call", "call_id": f"call-{index}", "name": encoded,
                          "arguments": json.dumps({"limit": index + 1})} for index in range(2))
            document = {"id": "resp-old-history", "status": "completed", "output": [
                {"id": "rs-fixture", "type": "reasoning", "encrypted_content": "opaque-fixture"},
                {"type": "message", "content": [{"type": "output_text", "text": "Read transcript."}]},
                *calls], "usage": {"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}
            return responses_response(wire, request, document, names)
        inputs = payload["input"]
        reasoning = [item for item in inputs if item.get("type") == "reasoning"]
        assert reasoning == [{"id": "rs-fixture", "type": "reasoning",
                              "encrypted_content": "opaque-fixture", "summary": []}]
        assert len([item for item in inputs if item.get("type") == "function_call"]) == 2
        assert {item["call_id"] for item in inputs if item.get("type") == "function_call_output"} == {
            "call-0", "call-1"}
        assert inputs.index(reasoning[0]) < next(index for index, item in enumerate(inputs)
                                               if item.get("type") == "function_call")
        return response(text='{"decision":"waiting_input"}', structured_output={"decision": "waiting_input"})

    with scenario([respond, respond]) as (service, provider, run_id):
        provider._capabilities = replace(provider.capabilities(), metadata={
            **provider.capabilities().metadata, "opaque_continuation": True})
        result = service.run(run_id)
        assert result.run.budget.pause_reason == "waiting_input" and len(received) == 2
        records = service.journal.model_responses(run_id)
        state = service.runtime.artifacts.read_json(records[0].response["continuation"]["artifact_id"],
                                                   run_id=run_id)
        assert state["assistant_request_id"] == records[0].request_id
        assert "previous_response_id" not in state
        loop = service.agent_loop
        current = loop._request(service.status(run_id), attempt=0)
        bound = prepare_continuation(loop, current)
        hydrated = hydrate_continuation(loop, bound)
        payload, _ = request_payload(wire, hydrated)
        assert any(item.get("type") == "reasoning" for item in payload["input"])
        try:
            hydrate_continuation(loop, replace(bound, model="different-model"))
        except ModelIntegrityError:
            pass
        else:
            raise AssertionError("cross-model continuation was accepted")
        old_capabilities = provider._capabilities
        provider._capabilities = replace(old_capabilities, metadata={
            **old_capabilities.metadata, "continuation_scope": "different-scope"})
        try:
            hydrate_continuation(loop, bound)
        except ModelIntegrityError:
            pass
        else:
            raise AssertionError("cross-scope continuation was accepted")
        finally:
            provider._capabilities = old_capabilities
        compacted = prepare_continuation(loop, loop._request(service.status(run_id), attempt=0,
                                                            force_compaction=True, overflow_retry=1))
        compacted_payload, _ = request_payload(wire, hydrate_continuation(loop, compacted))
        assert "previous_response_id" not in compacted_payload
        assert not any(item.get("type") in {"reasoning", "function_call", "function_call_output"}
                       for item in compacted_payload["input"])
        assert service.runtime.artifacts.read_json(records[0].response["continuation"]["artifact_id"],
                                                  run_id=run_id) == state


def check_projected_continuations():
    secret = "projection-fixture-credential-value"
    for protocol, sample in (("responses", "known"), ("responses", "json"),
                             ("anthropic", "known"), ("anthropic", "json"),
                             ("anthropic", "split-json")):
        wire = (OpenAICompatibleProvider("http://127.0.0.1:1/v1/responses", "fixture", environ={})
                if protocol == "responses" else AnthropicProvider("http://127.0.0.1:1/v1", "fixture", environ={}))
        encode = request_payload if protocol == "responses" else anthropic_payload
        original = ("Bound " + secret + " tail" if sample == "known" else
                    '{"password":"' + secret + '","note":"\\u4e2d"}')
        received = []

        def respond(request):
            payload, names = encode(wire, request)
            received.append(payload)
            if len(received) == 1:
                name = wire._encoded_tool_name("agent:transcript")
                if protocol == "responses":
                    return responses_response(wire, request, {"status": "completed", "output": [
                        {"id": "reasoning", "type": "reasoning", "encrypted_content": "bound-reasoning"},
                        {"type": "message", "content": [{"type": "output_text", "text": original}]},
                        {"type": "function_call", "call_id": "projection-call", "name": name,
                         "arguments": '{"limit":1}'}], "usage": {"total_tokens": 1}}, names)
                pieces = ([original[:20], original[20:]] if sample == "split-json" else
                          [original[:len(original) - 5], original[-5:]] if sample == "known" else [original])
                return message_response(wire, request, {"type": "message", "role": "assistant",
                    "stop_reason": "tool_use", "content": [
                        {"type": "thinking", "thinking": "", "signature": "bound-thinking"},
                        *({"type": "text", "text": text} for text in pieces),
                        {"type": "tool_use", "id": "projection-call", "name": name, "input": {"limit": 1}}],
                    "usage": {"input_tokens": 1, "output_tokens": 1}}, names)
            assert secret not in json.dumps(payload)
            assert "[SECRET_REF:sha256:" in json.dumps(payload)
            assert ("bound-reasoning" if protocol == "responses" else "bound-thinking") in json.dumps(payload)
            return response(text='{"decision":"waiting_input"}', structured_output={"decision": "waiting_input"})

        with scenario([respond, respond]) as (service, provider, run_id):
            provider._capabilities = replace(provider.capabilities(), metadata={
                **provider.capabilities().metadata, "opaque_continuation": True})
            if sample == "known":
                service.runtime._capture_credentials({"password": secret})
            if sample == "split-json":
                try:
                    service.run(run_id)
                except ModelLoopError as error:
                    assert "provider_continuation_projection_layout_unavailable" in str(error)
                else:
                    raise AssertionError("cross-block secret replay should stop before dispatch")
                record = service.journal.model_responses(run_id)[0]
                state = service.runtime.artifacts.read_json(record.response["continuation"]["artifact_id"], run_id=run_id)
                assert state["projection_block_layout_unavailable"] and len(received) == 1
                assert len(provider.requests) == 2
                continue
            result = service.run(run_id)
            assert result.run.budget.pause_reason == "waiting_input" and len(received) == 2
            record = service.journal.model_responses(run_id)[0]
            state = service.runtime.artifacts.read_json(record.response["continuation"]["artifact_id"], run_id=run_id)
            assert state["assistant_text_hash"] == state["assistant_wire_text_hash"] == hashlib.sha256(original.encode()).hexdigest()
            assert state["assistant_projection_text_hash"] == hashlib.sha256(record.response["text"].encode()).hexdigest()
            assert state["assistant_wire_text_hash"] != state["assistant_projection_text_hash"]
            if protocol == "anthropic":
                assert sum(part.get("length", 0) for part in state["projection_block_layout"]) == len(record.response["text"])
            request = hydrate_continuation(service.agent_loop, prepare_continuation(service.agent_loop,
                service.agent_loop._request(service.status(run_id), attempt=0)))
            changed = tuple({**item, "content": str(item["content"]) + "tampered"}
                            if item.get("source_request_id") == record.request_id else item for item in request.messages)
            try:
                encode(wire, replace(request, messages=changed))
            except RuntimeError as error:
                assert str(error) == "provider_continuation_message_mismatch"
            else:
                raise AssertionError("tampered projection was accepted")
            changed = tuple({**item, "source_request_id": "unrelated-request"}
                            if item.get("source_request_id") == record.request_id else item for item in request.messages)
            detached, _ = encode(wire, replace(request, messages=changed))
            assert "bound-reasoning" not in json.dumps(detached) and "bound-thinking" not in json.dumps(detached)
            detached, _ = encode(wire, replace(request, continuation={"chain": [
                {**state, "assistant_request_id": "unrelated-request"}]}))
            assert "bound-reasoning" not in json.dumps(detached) and "bound-thinking" not in json.dumps(detached)


class IdleSocket:
    def __init__(self):
        self.timeout = 0.01

    def settimeout(self, value):
        self.timeout = value

    def recv_into(self, buffer):
        time.sleep(min(0.01, self.timeout))
        raise TimeoutError("fixture poll")

    def makefile(self, mode, buffering=0):
        return io.BytesIO()

    def shutdown(self, how):
        pass

    def close(self):
        pass


class IdleConnection:
    def __init__(self):
        self.sock = None
        self.timeout = 120

    def connect(self):
        assert self.timeout < 1
        self.sock = IdleSocket()

    def request(self, *args, **kwargs):
        pass

    def getresponse(self):
        reader = self.sock.makefile("rb")
        try:
            reader.read(1)
        finally:
            reader.close()

    def close(self):
        pass


def check_transport_deadlines():
    for kind in (OpenAICompatibleProvider, AnthropicProvider):
        provider = kind("http://127.0.0.1:1/v1", "fixture", environ={})
        request = ModelRequest("deadline", "fixture", (), metadata={"runtime_deadline": deadline(0.12)})
        started = time.monotonic()
        with patch.object(provider, "_connection", return_value=IdleConnection()):
            try:
                with provider._exchange(request, {}):
                    raise AssertionError("stalled response completed")
            except TimeoutError:
                pass
        assert 0.08 < time.monotonic() - started < 0.7
        assert not provider._active
        for invalid in ("invalid", deadline(-1)):
            with patch.object(provider, "_connection", return_value=IdleConnection()) as connection:
                try:
                    with provider._exchange(replace(request, metadata={"runtime_deadline": invalid}), {}):
                        raise AssertionError("invalid deadline accepted")
                except (ValueError, TimeoutError):
                    pass
                assert not provider._active


class WaitingProvider(FakeModelProvider):
    def __init__(self, streaming=False):
        super().__init__()
        self.released = threading.Event()
        self.entered = threading.Event()
        self.streaming = streaming

    def complete(self, request):
        self.requests.append(request)
        self.entered.set()
        assert self.released.wait(8), "deadline did not cancel the cooperative provider"
        raise RuntimeError("provider_request_cancelled")

    def stream(self, request):
        self.requests.append(request)
        self.entered.set()
        yield ModelStreamEvent(request.request_id, 0, "text_delta", {"delta": "No", "metadata": {
            "refusal": True, "refusal_text": "No", "response_category": "refusal"}})
        yield ModelStreamEvent(request.request_id, 1, "usage", {"total_tokens": 1})
        assert self.released.wait(8), "deadline did not cancel the stream"
        raise RuntimeError("provider_request_cancelled")

    def cancel(self, request_id):
        self.cancelled.append(request_id)
        self.released.set()
        return True


def check_runtime_deadlines():
    for streaming in (False, True):
        with scenario([], time_limit_seconds=3) as (service, _original, run_id):
            provider = WaitingProvider(streaming)
            service.configure_model(provider, streaming=streaming)
            with patch("redteam_agent.application.model_turn.threading.Timer", wraps=threading.Timer) as timers:
                result = service.run(run_id)
            assert result.run.budget.pause_reason == "time_limit_exhausted"
            assert len(provider.requests) == 1 and len(provider.cancelled) == 1
            assert provider.cancelled[0] == provider.requests[0].request_id
            assert len(timers.call_args_list) == 1
            # Check the request deadline, excluding durable writes after cancellation.
            assert 0 < timers.call_args.args[0] <= provider.requests[0].metadata["remaining_time_seconds"]
            assert provider.requests[0].metadata["runtime_deadline"]
            assert provider.requests[0].metadata["remaining_time_seconds"] <= 3
            stored = service.journal.model_responses(run_id)[0].response
            assert not stored["tool_calls"]
            if streaming:
                assert stored["metadata"]["refusal_text"] == "No" and stored["usage"]["total_tokens"] == 1
    cancelled = []
    loop = SimpleNamespace(_invoke=lambda request: "completed", model=SimpleNamespace(cancel=cancelled.append))
    request = ModelRequest("complete-before-deadline", "fixture", (), metadata={"runtime_deadline": deadline(0.1)})
    assert invoke_with_deadline(loop, request) == "completed"
    time.sleep(0.15)
    assert not cancelled
    with scenario([]) as (service, _original, run_id):
        provider = WaitingProvider()
        service.configure_model(provider)
        outcomes = []
        thread = threading.Thread(target=lambda: outcomes.append(service.run(run_id)))
        thread.start()
        assert provider.entered.wait(3)
        service.cancel(run_id)
        thread.join(timeout=3)
        assert not thread.is_alive() and outcomes and outcomes[0].run.status == "cancelled"


def check_retry_after():
    for headers, expected in (({"retry-after-ms": "250", "retry-after": "10"}, 0.25),
                              ({"retry-after": "2"}, 2), ({"retry-after": "99999"}, 99999),
                              ({"retry-after": "nan"}, 0), ({"retry-after": "invalid"}, 0)):
        assert retry_after_seconds(SimpleNamespace(getheader=headers.get)) == expected
    now = [0.0]
    current = SimpleNamespace(run=SimpleNamespace(status="waiting_worker"), terminal=SimpleNamespace(terminal=False))
    loop = SimpleNamespace(_is_cancelled=lambda run: False, _is_interrupted=lambda run: False,
                           service=SimpleNamespace(_enforce_runtime_budget=lambda run: current))
    with patch("redteam_agent.application.model_turn.time.monotonic", side_effect=lambda: now[0]), \
         patch("redteam_agent.application.model_turn.time.sleep", side_effect=lambda delay: now.__setitem__(0, now[0] + delay)), \
         patch("redteam_agent.application.model_turn.random.uniform", return_value=0.125):
        retry_backoff(loop, "fixture", 1, retry_after=0.5)
    assert now[0] == 0.625
    with scenario([ProviderHTTPError(429, "rate_limit", "slow", retry_after=301)]) as (service, provider, run_id):
        try:
            service.run(run_id)
        except ModelLoopError:
            pass
        else:
            raise AssertionError("oversized server delay should stop")
        assert len(provider.requests) == 1
        recovery = next(item["payload"] for item in service.journal.operation_events(run_id)
                        if item["event_type"] == "model_provider_recovery")
        assert recovery["retry_delay_limit_exceeded"] and recovery["action"] == "stop"


if __name__ == "__main__":
    checks = []
    for name, operation in (("bounded-history-and-active-focus", check_history),
                            ("explicit-responses-tools-reasoning-compaction-and-binding", check_responses),
                            ("projected-continuation-hashes-layout-and-request-binding", check_projected_continuations),
                            ("openai-and-anthropic-transport-deadlines", check_transport_deadlines),
                            ("runtime-deadline-cancel-refusal-usage-and-timer-cleanup", check_runtime_deadlines),
                            ("retry-after-header-precedence-and-bounded-backoff", check_retry_after)):
        operation()
        checks.append(name)
    print(json.dumps({"ok": True, "checks": checks}))
