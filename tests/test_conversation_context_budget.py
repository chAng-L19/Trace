from __future__ import annotations

from collections.abc import Mapping
from pathlib import Path
from typing import Any

import pytest

from redteam_agent import AgentService, BudgetDelta, StartRequest
from redteam_agent.application import ModelLoopError
from redteam_agent.core import ModelResponse, ToolCall, ToolDefinition, ToolResult, contract_hash
from redteam_agent.providers import FakeModelProvider, ScriptedStream


class TargetToolPort:
    def __init__(self, target: str) -> None:
        self.target = target
        self.calls: list[ToolCall] = []

    def discover(self):
        return (
            ToolDefinition(
                qualified_name="fixture:target",
                name="target",
                server="fixture",
                input_schema={"type": "object"},
            ),
        )

    def invoke(self, call: ToolCall):
        self.calls.append(call)
        return ToolResult(
            call_id=call.call_id,
            status="success",
            tool_name=call.tool_name,
            output={"target": self.target},
        )

    def reconcile(self, call: ToolCall):
        return None

    def cancel(self, call_id: str):
        return True


def _request(
    *,
    token_limit: int | None = None,
    time_limit_seconds: float | None = None,
) -> StartRequest:
    return StartRequest(
        session_id="phase4-context-budget",
        objective="Give me a plan for the supplied target; do not make changes yet or run tests",
        max_actions=16,
        token_limit=token_limit,
        time_limit_seconds=time_limit_seconds,
    )


def _tool_response(usage: Mapping[str, Any]) -> ModelResponse:
    return ModelResponse(
        request_id="placeholder",
        status="completed",
        tool_calls=(
            {"id": "target-call", "name": "fixture:target", "arguments": {}},
        ),
        usage=usage,
        finish_reason="tool_calls",
    )


def test_complete_system_user_assistant_tool_transcript_is_persistent(tmp_path: Path) -> None:
    target = tmp_path / "transcript.txt"
    target.write_text("transcript", encoding="utf-8")
    provider = FakeModelProvider([_tool_response({"input_tokens": 4, "output_tokens": 2})])
    tools = TargetToolPort(str(target))
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = service.start(_request()).single.run.run_id

    completed = service.run(run_id)
    transcript = service.transcript(run_id)

    assert completed.run.status == "completed"
    assert {item.role for item in transcript} >= {"system", "user", "assistant", "tool"}
    assert {item.source_type for item in transcript} >= {
        "original_goal",
        "system_base",
        "action_prompt",
        "model_request_projection",
        "model_response",
        "tool_result",
    }
    assert [item.sequence for item in transcript] == list(range(1, len(transcript) + 1))
    assert all(contract_hash(item.content) == item.content_hash for item in transcript)


def test_compaction_is_append_only_and_summary_has_source_lineage(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "operations")
    run_id = service.start(_request()).single.run.run_id
    for index in range(8):
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"index": index, "detail": f"message-{index}"},
            protected=False,
            source_type="fixture",
            source_id=str(index),
        )
    before = service.transcript(run_id)
    sources = tuple(item for item in before if not item.protected)[:5]

    summary = service.compact_context(run_id, tuple(item.message_id for item in sources))
    after = service.transcript(run_id)

    assert summary is not None
    assert after == before
    assert summary.source_hash == service.runtime.store.context_source_hash(sources)
    assert summary.summary_hash == contract_hash(summary.summary)
    assert summary.source_message_ids == tuple(item.message_id for item in sources)


def test_protected_context_survives_zero_message_selection(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "operations")
    started = service.start(_request()).single

    selection = service.select_context(started.run.run_id, max_messages=0)

    assert selection.protected_context["original_goal"]["objective"] == started.goal.objective
    assert set(selection.protected_context) == {
        "original_goal",
        "unsatisfied_clauses",
        "active_plan",
        "critical_evidence_refs",
        "irreversible_state",
    }
    protected_ids = {
        item.message_id for item in service.transcript(started.run.run_id) if item.protected
    }
    assert protected_ids <= set(selection.source_message_ids)
    assert selection.messages[0]["content"]["protected_context"] == selection.protected_context


def test_protected_messages_cannot_be_compacted(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "operations")
    run_id = service.start(_request()).single.run.run_id
    protected = next(item for item in service.transcript(run_id) if item.protected)

    with pytest.raises(ValueError, match="protected_context_cannot_be_compacted"):
        service.compact_context(run_id, (protected.message_id,))


def test_token_limit_pauses_before_tool_and_resumes_from_durable_response(tmp_path: Path) -> None:
    target = tmp_path / "token-limit.txt"
    target.write_text("token-limit", encoding="utf-8")
    provider = FakeModelProvider([_tool_response({"input_tokens": 9, "output_tokens": 5})])
    tools = TargetToolPort(str(target))
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    assert service.model_loop is not None
    service.model_loop.max_turns = 1
    run_id = service.start(_request(token_limit=10)).single.run.run_id

    paused = service.run(run_id)

    assert paused.run.status == "paused_budget"
    assert paused.run.budget.input_tokens_used == 9
    assert paused.run.budget.output_tokens_used == 5
    assert paused.run.budget.pause_reason == "token_limit_exhausted"
    assert tools.calls == []
    assert paused.terminal.terminal is False

    completed = service.run(
        run_id,
        BudgetDelta(tokens=10, idempotency_key="phase4-token-extension"),
    )

    assert completed.run.status in {"running", "waiting_worker"}
    assert len(provider.requests) == 1
    assert len(tools.calls) == 1
    assert completed.run.budget.input_tokens_used == 9
    assert completed.run.budget.output_tokens_used == 5


def test_missing_usage_is_not_fabricated_and_requires_explicit_acknowledgement(
    tmp_path: Path,
) -> None:
    target = tmp_path / "missing-usage.txt"
    target.write_text("missing-usage", encoding="utf-8")
    provider = FakeModelProvider([_tool_response({})])
    tools = TargetToolPort(str(target))
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = service.start(_request(token_limit=100)).single.run.run_id

    paused = service.run(run_id)

    assert paused.run.status == "paused_budget"
    assert paused.run.budget.input_tokens_used is None
    assert paused.run.budget.output_tokens_used is None
    assert paused.run.budget.token_usage_missing == 1
    assert paused.run.budget.pause_reason == "token_usage_unknown"
    state = service.runtime.store.load_operation(run_id)
    assert state is not None and state.budget.tokens_used is None
    assert tools.calls == []

    completed = service.run(
        run_id,
        BudgetDelta(
            acknowledge_missing_usage=True,
            idempotency_key="phase4-acknowledge-unknown-usage",
        ),
    )

    assert completed.run.status == "completed"
    final_state = service.runtime.store.load_operation(run_id)
    assert final_state is not None
    assert final_state.budget.tokens_used is None
    assert final_state.budget.token_usage_missing == 1
    assert final_state.budget.token_usage_acknowledged == 1


def test_model_usage_commit_is_idempotent_per_request(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [ModelResponse(request_id="placeholder", status="completed", usage={"total_tokens": 7})]
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider)
    run_id = service.start(_request()).single.run.run_id
    service.run(run_id)
    request_id = service.runtime.store.model_requests(run_id)[0].request_id

    first = service._record_model_usage(run_id, request_id, {"total_tokens": 7})
    second = service._record_model_usage(run_id, request_id, {"total_tokens": 7})

    assert first.run.budget.output_tokens_used is None
    assert second.run.budget.output_tokens_used is None
    state = service.runtime.store.load_operation(run_id)
    assert state is not None and state.budget.tokens_used == 7
    assert len(service.runtime.store.model_budget_usage(run_id)) == 1


def test_expired_absolute_deadline_pauses_before_provider_call(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [ModelResponse(request_id="placeholder", status="completed", text="late")]
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider)
    run_id = service.start(_request(time_limit_seconds=0.1)).single.run.run_id

    paused = service.run(run_id)

    assert paused.run.status == "paused_budget"
    assert paused.run.budget.pause_reason == "time_limit_exhausted"
    assert provider.requests == []
    assert paused.terminal.terminal is False


def test_interrupted_stream_partial_is_only_a_diagnostic_artifact(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        streams=[
            ScriptedStream(
                events=(
                    {"sequence": 0, "event_type": "text_delta", "payload": {"delta": "partial"}},
                ),
                error=ConnectionError("stream-cut"),
            )
        ]
    )
    service = AgentService(
        root=tmp_path / "operations",
        model_port=provider,
        model_streaming=True,
        model_max_retries=0,
    )
    run_id = service.start(_request()).single.run.run_id

    with pytest.raises(ModelLoopError, match="retries_exhausted"):
        service.run(run_id)

    diagnostics = service.runtime.store.diagnostic_artifacts(run_id)
    transcript = service.transcript(run_id)
    assert len(diagnostics) == 1
    assert diagnostics[0].artifact_type == "partial_model_stream"
    assert diagnostics[0].payload["partial_text"] == "partial"
    assert diagnostics[0].payload["promoted"] is False
    assert all(item.source_type != "model_response" for item in transcript)
    response = service.runtime.store.model_responses(run_id)[0]
    assert response.response["text"] == ""
    assert response.response["metadata"]["diagnostic_artifact_id"] == diagnostics[0].artifact_id
    assert service.status(run_id).evidence == ()
