from __future__ import annotations

import json
from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import ContextBudget, ModelIntegrityError
from redteam_agent.core import ModelCapabilities, ModelResponse, ToolResult
from redteam_agent.providers import FakeModelProvider


def _service(tmp_path: Path, *, context_tokens: int = 4096) -> tuple[AgentService, str]:
    capabilities = ModelCapabilities(
        native_system_role=True,
        native_tool_calls=True,
        parallel_tool_calls=True,
        structured_output=True,
        usage_reporting=True,
        max_context_tokens=context_tokens,
        metadata={"provider": "fake", "model": "context-fixture"},
    )
    provider = FakeModelProvider(
        [ModelResponse(request_id="placeholder", status="completed", text="wait")],
        capabilities=capabilities,
    )
    service = AgentService(root=tmp_path / "runtime", model_port=provider)
    run_id = service.start(
        StartRequest(session_id="context", objective="Preserve attack-chain evidence while reducing prompt waste")
    ).single.run.run_id
    return service, run_id


def test_context_budget_keeps_assistant_tool_turn_atomic(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, context_tokens=2400)
    for index in range(8):
        request_id = f"request-{index}"
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"tool_calls": [{"id": f"call-{index}"}], "padding": "a" * 700},
            protected=False,
            source_type="model_response",
            source_id=request_id,
            metadata={"request_id": request_id},
        )
        service.conversation.append(
            run_id=run_id,
            role="tool",
            content={"call_id": f"call-{index}", "output": "b" * 700},
            protected=False,
            source_type="tool_result",
            source_id=f"{request_id}:call-{index}",
            metadata={"request_id": request_id, "call_id": f"call-{index}"},
        )

    selection = service.context_selector.prepare_model_context(
        service.status(run_id), max_context_tokens=2400
    )
    selected = set(selection.source_message_ids)
    transcript = service.transcript(run_id)
    for index in range(8):
        turn = {
            item.message_id
            for item in transcript
            if str(item.metadata.get("request_id") or "") == f"request-{index}"
        }
        assert not (turn & selected) or turn <= selected
    assert selection.context_overflow_tokens > 0
    assert selection.reserved_output_tokens >= 1024
    assert len(service.transcript(run_id)) == len(transcript)


def test_context_budget_groups_interleaved_request_messages_atomically(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, context_tokens=2400)
    first = service.conversation.append(
        run_id=run_id,
        role="assistant",
        content={"tool_calls": [{"id": "call-1"}], "padding": "a" * 120},
        protected=False,
        source_type="model_response",
        source_id="request-1",
        metadata={"request_id": "request-1"},
    )
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content={"text": "interleaved", "padding": "b" * 120},
        protected=False,
        source_type="model_response",
        source_id="request-2",
        metadata={"request_id": "request-2"},
    )
    second = service.conversation.append(
        run_id=run_id,
        role="tool",
        content={"call_id": "call-1", "output": "c" * 120},
        protected=False,
        source_type="tool_result",
        source_id="request-1:call-1",
        metadata={"request_id": "request-1", "call_id": "call-1"},
    )

    groups = service.context_selector._atomic_groups((first, service.conversation.messages(run_id)[-2], second))

    assert groups[0] == (first, second)
    assert groups[1][0].source_id == "request-2"


def test_context_budget_selection_without_turn_boundary_has_no_compaction_side_effect(
    tmp_path: Path,
) -> None:
    service, run_id = _service(tmp_path)
    for index in range(60):
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"index": index},
            protected=False,
            source_type="fixture",
            source_id=str(index),
        )

    selection = service.context_selector.select(
        service.status(run_id),
        max_messages=2,
        turn_boundary=False,
    )

    assert selection.compaction_ids == ()
    assert service.journal.context_summaries(run_id) == ()


def test_context_budget_is_explicit_and_clamps_invalid_values() -> None:
    budget = ContextBudget.from_values(
        window_tokens=-1,
        reserved_output_tokens=-2,
        keep_recent_messages=-3,
        fallback_bytes_per_token=0,
        max_compaction_retries=-4,
    )

    assert budget.window_tokens == 0
    assert budget.reserved_output_tokens == 0
    assert budget.keep_recent_messages == 0
    assert budget.fallback_bytes_per_token == 1
    assert budget.max_compaction_retries == 0
    assert budget.available_tokens == 0


def test_context_overflow_gets_one_compaction_retry(tmp_path: Path) -> None:
    capabilities = ModelCapabilities(
        native_system_role=True,
        native_tool_calls=True,
        structured_output=True,
        usage_reporting=True,
        max_context_tokens=512,
        metadata={"provider": "fake", "model": "overflow-fixture"},
    )
    provider = FakeModelProvider(
        [
            RuntimeError("context_length_exceeded"),
            ModelResponse(request_id="placeholder", status="completed", text="recovered"),
        ],
        capabilities=capabilities,
    )
    service = AgentService(
        root=tmp_path / "runtime",
        model_port=provider,
        model_max_retries=0,
    )
    run_id = service.start(
        StartRequest(session_id="overflow", objective="Preserve a bounded context")
    ).single.run.run_id
    for index in range(12):
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"index": index, "padding": "x" * 400},
            protected=False,
            source_type="fixture",
            source_id=str(index),
        )

    service.run(run_id)

    assert len(provider.requests) == 2
    assert provider.requests[0].metadata["context_overflow_retry"] == 0
    assert provider.requests[1].metadata["context_overflow_retry"] == 1
    assert provider.requests[1].metadata["context_compaction_ids"]


def test_large_tool_result_is_projected_but_complete_cas_remains_readable(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    result = ToolResult(
        call_id="large-call",
        status="success",
        tool_name="fixture:large",
        output={"attack_chain": "链路" * 100_000, "evidence": ["e-1", "e-2"]},
        output_hash="f" * 64,
    )

    service.conversation.record_tool_results("large-request", run_id, (result,))
    message = service.transcript(run_id)[-1]
    artifact_id = message.content["metadata"]["complete_output_artifact"]
    loaded = service.runtime.artifacts.read_json(artifact_id, run_id=run_id)

    assert loaded["output"] == result.output
    assert len(json.dumps(message.content, ensure_ascii=False).encode("utf-8")) < 80_000
    assert message.content["output"]["artifact"]["content_hash"]


def test_model_request_records_context_projection_metrics_and_stable_prefix(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    assert service.model_loop is not None
    service.run(run_id)
    request = service.runtime.store.model_requests(run_id)[0].request
    metadata = request["metadata"]

    assert request["messages"][0] == {
        "role": "system",
        "content": {"system_invariant": request["messages"][0]["content"]["system_invariant"]},
    }
    assert metadata["context_hash"]
    assert metadata["estimated_context_tokens"] > 0
    assert metadata["selected_tokens"] >= 0
    assert metadata["reserved_output_tokens"] > 0
    assert metadata["projection_bytes"] > 0
    assert metadata["context_overflow_tokens"] >= 0


@pytest.mark.parametrize(
    "usage",
    [
        {"input_tokens": -1},
        {"input_tokens": float("inf")},
        {"input_tokens": 2**100},
        {"cache_read_tokens": -1},
    ],
)
def test_adversarial_provider_usage_is_rejected(tmp_path: Path, usage) -> None:
    service, run_id = _service(tmp_path)
    assert service.model_loop is not None

    with pytest.raises(ModelIntegrityError, match="model_usage_invalid"):
        service.model_loop._normalize_usage(usage)
