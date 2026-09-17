from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import ModelIntegrityError, ModelLoopError
from redteam_agent.core import ModelResponse
from redteam_agent.providers import FakeModelProvider, ScriptedStream


def _start(service: AgentService, *, token_limit: int | None = None) -> str:
    return service.start(StartRequest(
        session_id="model-boundaries", objective="Inspect the supplied target",
        token_limit=token_limit,
    )).single.run.run_id


def test_provider_retry_stops_after_manual_pause(tmp_path: Path) -> None:
    def pause_then_fail(request):
        service.pause(request.run_id, reason="manual_review")
        return ConnectionError("request interrupted")

    provider = FakeModelProvider([
        pause_then_fail,
        ModelResponse(request_id="placeholder", status="completed", text="unexpected retry"),
    ])
    service = AgentService(root=tmp_path / "runtime", model_port=provider)
    try:
        run_id = _start(service)
        try:
            service.run(run_id)
        except ModelLoopError:
            pass
        assert len(provider.requests) == 1
        assert service.status(run_id).run.status == "paused_budget"
    finally:
        service.close()


def test_interrupted_response_usage_stops_retry_at_token_budget(tmp_path: Path) -> None:
    provider = FakeModelProvider([
        ModelResponse(request_id="placeholder", status="interrupted", error="length",
                      usage={"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}),
        ModelResponse(request_id="placeholder", status="completed", text="unexpected retry",
                      usage={"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}),
    ])
    service = AgentService(root=tmp_path / "runtime", model_port=provider)
    try:
        run_id = _start(service, token_limit=10)
        try:
            service.run(run_id)
        except ModelLoopError:
            pass
        assert len(provider.requests) == 1
        state = service.runtime.store.load_operation(run_id)
        assert state.budget.tokens_used == 15
        assert state.status == "paused_budget"
    finally:
        service.close()


@pytest.mark.parametrize("finish_reason", ["length", "max_tokens", "content_filter", "error", "blocked"])
def test_stream_non_success_finish_reason_is_not_completed(tmp_path: Path, finish_reason: str) -> None:
    provider = FakeModelProvider(streams=[ScriptedStream(events=(
        {"event_type": "text_delta", "payload": {"delta": "partial"}},
        {"event_type": "completed", "payload": {
            "finish_reason": finish_reason,
            "usage": {"input_tokens": 3, "output_tokens": 2},
        }},
    ))])
    service = AgentService(root=tmp_path / "runtime", model_port=provider,
                           model_streaming=True, model_max_retries=0)
    try:
        run_id = _start(service)
        with pytest.raises(ModelLoopError):
            service.run(run_id)
        response = service.runtime.store.model_responses(run_id)[0]
        assert response.status in {"interrupted", "failed"}
        assert response.usage == {"input_tokens": 3, "output_tokens": 2}
    finally:
        service.close()


def test_cut_stream_usage_is_retained_and_charged(tmp_path: Path) -> None:
    provider = FakeModelProvider(streams=[ScriptedStream(events=(
        {"event_type": "usage", "payload": {"input_tokens": 12, "output_tokens": 3}},
        {"event_type": "text_delta", "payload": {"delta": "partial"}},
    ), error=ConnectionError("cut"))])
    service = AgentService(root=tmp_path / "runtime", model_port=provider,
                           model_streaming=True, model_max_retries=0)
    try:
        run_id = _start(service, token_limit=10)
        with pytest.raises(ModelLoopError):
            service.run(run_id)
        response = service.runtime.store.model_responses(run_id)[0]
        assert response.usage == {"input_tokens": 12, "output_tokens": 3}
        state = service.runtime.store.load_operation(run_id)
        assert state.budget.tokens_used == 15
        assert state.status == "paused_budget"
    finally:
        service.close()


def test_stream_rejects_tool_calls_after_completion(tmp_path: Path) -> None:
    provider = FakeModelProvider(streams=[ScriptedStream(events=(
        {"event_type": "completed", "payload": {}},
        {"event_type": "tool_call", "payload": {
            "id": "late-call", "name": "fixture:late", "arguments": {},
        }},
    ))])
    service = AgentService(root=tmp_path / "runtime", model_port=provider,
                           model_streaming=True, model_max_retries=0)
    try:
        run_id = _start(service)
        with pytest.raises(ModelIntegrityError, match="after_completion"):
            service.run(run_id)
    finally:
        service.close()


def test_repeated_automatic_compaction_keeps_prior_summary_sources(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        run_id = _start(service)
        service.context_selector.compaction_threshold = 2
        sources = []
        for index in range(6):
            sources.append(service.conversation.append(
                run_id=run_id, role="user", content=f"observation-{index}",
                protected=False, source_type="fixture", source_id=str(index),
            ))
            if index == 3:
                service.context_selector.select(service.status(run_id), max_messages=1,
                                                turn_boundary=True)
        selection = service.context_selector.select(service.status(run_id), max_messages=1,
                                                    turn_boundary=True)
        projected_ids = set(selection.source_message_ids)
        for summary in service.journal.context_summaries(run_id):
            if summary.summary_id in selection.summary_ids:
                projected_ids.update(summary.source_message_ids)
        assert {item.message_id for item in sources} <= projected_ids
    finally:
        service.close()
