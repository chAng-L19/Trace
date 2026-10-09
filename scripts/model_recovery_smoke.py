"""Offline recovery checks against the real AgentService and durable journal."""
from __future__ import annotations

import json
import sys
import tempfile
from contextlib import contextmanager
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.model_loop import ModelIntegrityError, ModelLoopError
from redteam_agent.application.model_turn import classify_provider_error
from redteam_agent.providers import FakeModelProvider, ProviderHTTPError
from redteam_agent.providers import OpenAICompatibleProvider, ScriptedStream
from redteam_agent.providers.openai_protocol import stream_events
from provider_protocol_smoke import sse


def response(**values):
    return {"status": "completed", "text": "", "usage": {"total_tokens": 1}, **values}


@contextmanager
def scenario(script, *, turns=8, retries=2, streams=(), **limits):
    with tempfile.TemporaryDirectory(prefix="trace-model-recovery-") as temporary:
        provider = FakeModelProvider(script, streams=streams)
        service = AgentService(root=Path(temporary), model_port=provider,
                               model_max_turns=turns, model_max_retries=retries,
                               model_streaming=bool(streams),
                               load_external_configuration=False)
        try:
            run_id = service.start({"session_id": "recovery-smoke", "objective": "Inspect local fixture",
                                    "targets": [temporary], **limits}).runs[0].run.run_id
            yield service, provider, run_id
        finally:
            service.close()


def nudges(service, run_id):
    return [item for item in service.conversation.messages(run_id)
            if item.source_type == "model_empty_turn_nudge"]


def recoveries(service, run_id):
    return [item["payload"] for item in service.journal.operation_events(run_id)
            if item["event_type"] == "model_provider_recovery"]


def main():
    checks = []
    with scenario([response(), response(text="Recovered")]) as (service, provider, run_id):
        service.run(run_id)
        assert len(provider.requests) == 2 and len(nudges(service, run_id)) == 1
        assert "previous response was empty" in json.dumps(provider.requests[1].messages)
        checks.append("durable_nudge_in_next_prompt")

    with scenario([response()] * 4) as (service, provider, run_id):
        result = service.run(run_id)
        assert len(provider.requests) == 3 and len(nudges(service, run_id)) == 2
        assert result.run.budget.pause_reason == "model_no_progress"
        # Replacing the loop simulates losing all transient recovery state.
        service.configure_model(provider)
        service.resume(run_id)
        assert len(provider.requests) == 4 and len(nudges(service, run_id)) == 2
        checks.append("durable_limit_survives_new_loop")

    for value in (response(text="Plain answer"),
                  response(metadata={"refusal": True, "refusal_text": "No", "response_category": "refusal"}),
                  response(metadata={"refusal_text": "No"}),
                  response(structured_output={"decision": "waiting_input"}),
                  response(structured_output={"decision": "done"}),
                  response(structured_output={"tactical_update": {}})):
        with scenario([value]) as (service, provider, run_id):
            result = service.run(run_id)
            assert len(provider.requests) == 1 and not nudges(service, run_id)
            if value.get("structured_output", {}).get("decision") == "waiting_input":
                assert result.run.budget.pause_reason == "waiting_input"
    checks.append("nonempty_refusal_and_decisions_never_nudged")

    mixed = response(text='{"commit_lifecycle_gate":true}',
                     metadata={"refusal": True, "refusal_text": "No", "response_category": "refusal"},
                     structured_output={"commit_lifecycle_gate": True, "tactical_update": {"objective": "bad"}},
                     tool_calls=[{"call_id": "refused-call", "tool_name": "fixture:tool", "arguments": {}}])
    for limits in ({}, {"token_limit": 1}, {"max_actions": 1}):
        with scenario([mixed, response(text="After explicit resume")], **limits) as (service, provider, run_id):
            with patch.object(service.agent_loop, "_execute_tool_calls") as execute, \
                 patch.object(service.agent_loop, "_record_tactical_update") as tactical:
                result = service.run(run_id)
                execute.assert_not_called()
                tactical.assert_not_called()
            if limits.get("max_actions") == 1:
                assert not provider.requests and result.run.budget.pause_reason == "action_limit_exhausted"
                continue
            assert len(provider.requests) == 1 and not nudges(service, run_id)
            assert result.run.budget.pause_reason == ("token_limit_exhausted" if limits.get("token_limit") else "model_refusal"), (limits, result.run.budget.pause_reason)
            stored = service.journal.model_responses(run_id)[0].response
            assert stored["metadata"]["refusal"] and stored["tool_calls"]
            service.configure_model(provider)
            assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
            if not limits.get("token_limit"):
                with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
                    service.resume(run_id)
                    execute.assert_not_called()
                assert len(provider.requests) == 2
    checks.append("mixed_refusal_no_execution_and_durable_consumption")

    wire_provider = OpenAICompatibleProvider("http://127.0.0.1:1/v1", "fixture", environ={})
    from redteam_agent.core import ModelRequest
    wire_request = ModelRequest("offline", "offline", ())
    mixed_events = tuple(stream_events(wire_provider, wire_request, sse(
        {"choices": [{"delta": {"content": "Partial answer", "refusal": "No", "tool_calls": [
            {"index": 0, "id": "refused-call", "function": {"name": "fixture", "arguments": "{}"}}]},
            "finish_reason": "tool_calls"}], "usage": {"total_tokens": 1}}, done=True),
        {"fixture": "fixture:tool"}, byte_limit=65536))
    with scenario([], streams=(ScriptedStream(mixed_events),)) as (service, provider, run_id):
        with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
            result = service.run(run_id)
        execute.assert_not_called()
        assert result.run.budget.pause_reason == "model_refusal"
        assert len(provider.requests) == 1 and not nudges(service, run_id)
        stored = service.journal.model_responses(run_id)[0].response
        assert stored["text"] == "Partial answerNo" and stored["metadata"]["refusal_text"] == "No"
        assert stored["tool_calls"][0]["call_id"] == "refused-call"
        service.configure_model(provider, streaming=True)
        assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
    checks.append("mixed_refusal_stream_real_loop")

    stopped = (*mixed_events[:-1], replace(mixed_events[-1], payload={
        **mixed_events[-1].payload, "finish_reason": "length", "status": "interrupted"}))
    with scenario([], streams=(ScriptedStream(stopped),)) as (service, provider, run_id):
        with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
            service.run(run_id)
        execute.assert_not_called()
        stored = service.journal.model_responses(run_id)[0].response
        assert stored["status"] == "interrupted" and stored["finish_reason"] == "length"
        assert stored["metadata"]["refusal"] and stored["metadata"]["refusal_text"] == "No"
        assert stored["usage"]["total_tokens"] == 1 and not stored["tool_calls"]
        assert len(provider.requests) == 1 and not nudges(service, run_id)
        service.configure_model(provider, streaming=True)
        assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
    checks.append("truncated_refusal_stream_metadata_and_no_replay")

    for endpoint in ("v1", "v1/responses"):
        parser = OpenAICompatibleProvider("http://127.0.0.1:1/" + endpoint, "fixture", environ={})
        delta = ({"type": "response.refusal.delta", "delta": "No"} if parser._responses_api else
                 {"choices": [{"delta": {"refusal": "No"}}]})
        for tail in ((), ({"error": {"code": "rate_limit_exceeded", "message": "slow down"}},)):
            partial, failure = [], None
            try:
                partial.extend(stream_events(parser, wire_request, sse(delta, *tail), {}, byte_limit=65536))
            except RuntimeError as error:
                failure = error
            assert failure is not None
            with scenario([], streams=(ScriptedStream(tuple(partial), error=failure),)) as (service, provider, run_id):
                with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
                    try:
                        service.run(run_id)
                        raise AssertionError("partial refusal stream failure was ignored")
                    except ModelLoopError:
                        pass
                execute.assert_not_called()
                stored = service.journal.model_responses(run_id)[0].response
                assert stored["metadata"]["refusal_text"] == "No" and not stored["tool_calls"]
                assert len(provider.requests) == 1 and not nudges(service, run_id)
                assert recoveries(service, run_id)[0]["category"] == "refusal"
                assert recoveries(service, run_id)[0]["action"] == "stop"
    checks.append("refusal_delta_survives_disconnect_and_rate_error")

    for code, category in (("rate_limit_exceeded", "rate_limit"), ("insufficient_quota", "quota"),
                            ("server_error", "transport"), ("invalid_request_error", "protocol")):
        try:
            list(stream_events(wire_provider, wire_request,
                 sse({"error": {"code": code, "message": "fixture"}}), {}, byte_limit=65536))
        except ProviderHTTPError as error:
            assert classify_provider_error(error) == (category, category in {"rate_limit", "transport"})
        else:
            raise AssertionError("SSE error was ignored")
    checks.append("structured_sse_error_classification")

    with scenario([response()] * 3, turns=1) as (service, provider, run_id):
        result = service.run(run_id, max_cycles=8)
        assert len(provider.requests) == 2
        assert result.run.budget.pause_reason == "model_no_progress"
        checks.append("cycle_fingerprint_stops_no_progress")

    with scenario([response()] * 3, turns=1) as (service, provider, run_id):
        result = service.run(run_id, max_cycles=1)
        assert len(provider.requests) == 1 and result.run.budget.pause_reason == "model_cycle_limit"
        checks.append("nudge_uses_original_turn_and_cycle_limits")

    with scenario([response(usage={"total_tokens": 10})], token_limit=5) as (service, provider, run_id):
        result = service.run(run_id)
        assert result.run.status == "paused_budget" and not nudges(service, run_id)
        checks.append("empty_response_cannot_exceed_token_budget")

    for state in ("unknown", "waiting_worker"):
        with scenario([response()]) as (service, provider, run_id):
            with patch.object(service.worker_records, "records", return_value=(SimpleNamespace(status=state),)):
                service.run(run_id)
            assert not provider.requests and not nudges(service, run_id)
    checks.append("external_workers_never_nudged")

    with scenario([]) as (service, provider, run_id):
        def cancel_then_empty(request):
            service.cancel(run_id)
            return response()
        provider._responses.append(cancel_then_empty)
        result = service.run(run_id)
        assert result.run.status == "cancelled" and not nudges(service, run_id)
        checks.append("empty_response_after_cancel_never_nudged")

    for error, category in ((TimeoutError("timed out"), "transport"),
                            (ConnectionError("closed"), "transport"),
                            (ProviderHTTPError(429, "rate_limit_exceeded", "slow down"), "rate_limit"),
                            (ProviderHTTPError(503, "unavailable", "retry"), "transport")):
        with scenario([error, response(text="Recovered")]) as (service, provider, run_id):
            service.run(run_id)
            assert len(provider.requests) == 2
            assert recoveries(service, run_id)[0]["category"] == category
            assert len(service.journal.model_responses(run_id)) == 2
    checks.append("retry_allowlist_and_durable_failures")

    errors = ((ProviderHTTPError(401, "invalid_api_key", "bad key"), "auth"),
              (ProviderHTTPError(429, "insufficient_quota", "quota"), "quota"),
              (ProviderHTTPError(400, "invalid_request", "bad payload"), "protocol"),
              (ProviderHTTPError(408, "timeout", "client timeout"), "protocol"),
              (ProviderHTTPError(404, "not_found", "context_length_exceeded"), "protocol"),
              (ProviderHTTPError(400, "content_policy_violation", "refused"), "refusal"),
              (RuntimeError("provider_response_not_json"), "protocol"))
    for error, category in errors:
        assert classify_provider_error(error) == (category, False)
        with scenario([error, response()]) as (service, provider, run_id):
            try:
                service.run(run_id)
                raise AssertionError("nonretryable_error_did_not_stop")
            except ModelLoopError:
                pass
            assert len(provider.requests) == 1
            assert recoveries(service, run_id)[0]["action"] == "stop"
    checks.append("auth_quota_refusal_protocol_and_4xx_stop")

    with scenario([TimeoutError("transient")] * 4) as (service, provider, run_id):
        try:
            service.run(run_id)
            raise AssertionError("retry_limit_not_enforced")
        except ModelLoopError:
            pass
        assert len(provider.requests) == 3
        checks.append("provider_retry_limit")

    overflow = ProviderHTTPError(400, "context_length_exceeded", "too long")
    with scenario([overflow, overflow], retries=0) as (service, provider, run_id):
        with patch.object(service.agent_loop, "_request", wraps=service.agent_loop._request) as make_request:
            try:
                service.run(run_id)
                raise AssertionError("overflow_limit_not_enforced")
            except ModelLoopError:
                pass
        assert len(provider.requests) == 2
        assert [call.kwargs["force_compaction"] for call in make_request.call_args_list] == [False, True]
        assert [event["action"] for event in recoveries(service, run_id)] == ["compact", "stop"]
        checks.append("context_overflow_compacts_once")

    with scenario([overflow, TimeoutError("transient"), response(text="Recovered")]) as (service, provider, run_id):
        with patch.object(service.agent_loop, "_request", wraps=service.agent_loop._request) as make_request:
            service.run(run_id)
        assert [call.kwargs["force_compaction"] for call in make_request.call_args_list] == [False, True, False]
        checks.append("transport_after_compaction_does_not_recompact")

    with scenario([ProviderHTTPError(429, "rate_limit", "transient", retry_after=1), response()]) as (service, provider, run_id):
        with patch("redteam_agent.application.model_turn.time.monotonic", return_value=0), \
             patch("redteam_agent.application.model_turn.time.sleep", side_effect=lambda _: service.cancel(run_id)):
            result = service.run(run_id)
        assert result.run.status == "cancelled" and len(provider.requests) == 1
        checks.append("backoff_cancellation")

    with scenario([], token_limit=5) as (service, provider, run_id):
        def consume_then_fail(request):
            service._record_model_usage(run_id, request.request_id, {"total_tokens": 10})
            return TimeoutError("transient")
        provider._responses.extend([consume_then_fail, response()])
        result = service.run(run_id)
        assert result.run.status == "paused_budget" and len(provider.requests) == 1
        checks.append("backoff_budget_stop")

    with scenario([response(response_hash="invalid")]) as (service, provider, run_id):
        try:
            service.run(run_id)
            raise AssertionError("integrity_error_was_retried")
        except ModelIntegrityError:
            pass
        assert len(provider.requests) == 1 and not recoveries(service, run_id)
        checks.append("integrity_failures_not_retried")

    with scenario([KeyboardInterrupt()]) as (service, provider, run_id):
        try:
            service.run(run_id)
            raise AssertionError("process_signal_was_swallowed")
        except KeyboardInterrupt:
            pass
        assert len(provider.requests) == 1 and not recoveries(service, run_id)
        assert service.status(run_id).run.budget.pause_reason == "service_shutdown"
        checks.append("process_signal_preserved")

    print(json.dumps({"ok": True, "checks": checks}, indent=2))


if __name__ == "__main__":
    main()
