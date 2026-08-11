from __future__ import annotations

import threading
import time
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import ModelIntegrityError, ModelLoopError
from redteam_agent.core import (
    ModelCapabilities,
    ModelResponse,
    ToolCall,
    ToolDefinition,
    ToolResult,
)
from redteam_agent.providers import FakeModelProvider, ScriptedStream


class ScriptedToolPort:
    def __init__(
        self,
        outputs: Mapping[str, Any | Callable[[ToolCall], Any]],
        *,
        result_hashes: Mapping[str, tuple[str, str]] | None = None,
    ) -> None:
        self.outputs = dict(outputs)
        self.result_hashes = dict(result_hashes or {})
        self.calls: list[ToolCall] = []
        self.cancelled: list[str] = []
        self._lock = threading.Lock()

    def discover(self) -> tuple[ToolDefinition, ...]:
        return tuple(
            ToolDefinition(
                qualified_name=name,
                name=name.rpartition(":")[2],
                server=name.partition(":")[0],
                description=f"Fixture tool {name}",
                input_schema={"type": "object", "additionalProperties": True},
            )
            for name in self.outputs
        )

    def invoke(self, call: ToolCall) -> ToolResult:
        with self._lock:
            self.calls.append(call)
        output = self.outputs[call.tool_name]
        if callable(output):
            output = output(call)
        input_hash, output_hash = self.result_hashes.get(call.tool_name, ("", ""))
        return ToolResult(
            call_id=call.call_id,
            status="success",
            tool_name=call.tool_name,
            output=output,
            input_hash=input_hash,
            output_hash=output_hash,
        )

    def reconcile(self, call: ToolCall) -> ToolResult | None:
        del call
        return None

    def cancel(self, call_id: str) -> bool:
        self.cancelled.append(call_id)
        return True


def _start_waiting(service: AgentService) -> str:
    return service.start(
        StartRequest(
            session_id="phase3-model-loop",
            objective="Give me a plan for the supplied target; do not make changes yet or run tests",
            max_actions=16,
        )
    ).single.run.run_id


def _tool_response(*calls: Mapping[str, Any], provider: str = "fake") -> ModelResponse:
    return ModelResponse(
        request_id="placeholder",
        status="completed",
        provider=provider,
        model="fixture-model",
        tool_calls=tuple(calls),
        usage={"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
        finish_reason="tool_calls",
    )


def test_model_loop_completes_a_recoverable_end_to_end_run(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("phase-3", encoding="utf-8")
    provider = FakeModelProvider(
        [_tool_response({"id": "discover-1", "name": "fixture:discover", "arguments": {}})]
    )
    tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = _start_waiting(service)

    completed = service.run(run_id)

    assert completed.run.status == "completed"
    assert completed.terminal.success is True
    assert provider.requests[0].messages[0]["role"] == "system"
    assert provider.requests[0].allow_parallel_tools is True
    requests = service.runtime.store.model_requests(run_id)
    responses = service.runtime.store.model_responses(run_id)
    observations = service.runtime.store.model_observations(run_id)
    assert len(requests) == len(responses) == len(observations) == 1
    assert requests[0].prompt_hash
    assert requests[0].provider == "fake"
    assert responses[0].response_hash
    assert responses[0].usage == {"input_tokens": 10, "output_tokens": 4, "total_tokens": 14}
    assert observations[0].observation["tool_result"]["output"] == {"target": str(target)}


@pytest.mark.parametrize(
    ("text", "structured"),
    [
        ("Need a target discovery tool.", {}),
        ("", {"decision": "wait", "reason": "tool unavailable"}),
    ],
)
def test_text_and_structured_responses_are_durable_but_not_evidence(
    tmp_path: Path,
    text: str,
    structured: Mapping[str, Any],
) -> None:
    provider = FakeModelProvider(
        [
            ModelResponse(
                request_id="placeholder",
                status="completed",
                text=text,
                structured_output=structured,
                finish_reason="stop",
            )
        ]
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider)
    run_id = _start_waiting(service)

    waiting = service.run(run_id)

    assert waiting.run.status == "waiting_worker"
    assert waiting.evidence == ()
    response = service.runtime.store.model_responses(run_id)[0]
    assert response.response["text"] == text
    assert response.response["structured_output"] == dict(structured)


def test_model_cannot_inject_direct_evidence(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [
            ModelResponse(
                request_id="placeholder",
                status="completed",
                structured_output={
                    "evidence": [{"verified": True, "artifact_type": "final_report"}],
                    "terminal": {"success": True},
                },
            )
        ]
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider)
    run_id = _start_waiting(service)

    view = service.run(run_id)

    assert view.run.status == "waiting_worker"
    assert view.evidence == ()
    assert view.terminal.terminal is False
    assert service.runtime.store.model_observations(run_id) == ()


def test_parallel_tool_calls_are_all_captured_as_observations(tmp_path: Path) -> None:
    target = tmp_path / "parallel.txt"
    target.write_text("parallel", encoding="utf-8")
    provider = FakeModelProvider(
        [
            _tool_response(
                {"id": "call-a", "name": "fixture:left", "arguments": {}},
                {"id": "call-b", "name": "fixture:right", "arguments": {}},
            )
        ]
    )
    barrier = threading.Barrier(2)

    def parallel_output(call: ToolCall) -> Mapping[str, str]:
        barrier.wait(timeout=3)
        return {"target": str(target), "source": call.tool_name}

    tools = ScriptedToolPort(
        {"fixture:left": parallel_output, "fixture:right": parallel_output}
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = _start_waiting(service)

    completed = service.run(run_id)

    assert completed.run.status == "completed"
    assert {item.call_id for item in service.runtime.store.model_observations(run_id)} == {
        "call-a",
        "call-b",
    }
    assert len({call.idempotency_key for call in tools.calls}) == 2


def test_streaming_completion_preserves_events_and_usage(tmp_path: Path) -> None:
    target = tmp_path / "stream.txt"
    target.write_text("stream", encoding="utf-8")
    provider = FakeModelProvider(
        streams=[
            ScriptedStream(
                events=(
                    {"sequence": 0, "event_type": "text_delta", "payload": {"delta": "checking"}},
                    {
                        "sequence": 1,
                        "event_type": "completed",
                        "payload": {
                            "tool_calls": [
                                {"id": "stream-call", "name": "fixture:discover", "arguments": {}}
                            ],
                            "usage": {"input_tokens": 3, "output_tokens": 2},
                        },
                    },
                )
            )
        ]
    )
    tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
    service = AgentService(
        root=tmp_path / "operations",
        model_port=provider,
        tool_port=tools,
        model_streaming=True,
    )
    run_id = _start_waiting(service)

    completed = service.run(run_id)

    request_id = service.runtime.store.model_requests(run_id)[0].request_id
    assert completed.run.status == "completed"
    assert [item.event_type for item in service.runtime.store.model_stream_events(request_id)] == [
        "text_delta",
        "completed",
    ]
    assert service.runtime.store.model_responses(run_id)[0].usage == {
        "input_tokens": 3,
        "output_tokens": 2,
    }


def test_stream_interruption_is_durable_and_retryable(tmp_path: Path) -> None:
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
    run_id = _start_waiting(service)

    with pytest.raises(ModelLoopError, match="retries_exhausted"):
        service.run(run_id)

    response = service.runtime.store.model_responses(run_id)[0]
    assert response.status == "failed"
    assert response.response["text"] == "partial"
    assert service.status(run_id).terminal.terminal is False


def test_transient_provider_failure_retries_with_a_new_request(tmp_path: Path) -> None:
    target = tmp_path / "retry.txt"
    target.write_text("retry", encoding="utf-8")
    provider = FakeModelProvider(
        [
            ConnectionError("transient"),
            _tool_response({"id": "retry-call", "name": "fixture:discover", "arguments": {}}),
        ]
    )
    tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = _start_waiting(service)

    completed = service.run(run_id)

    requests = service.runtime.store.model_requests(run_id)
    responses = service.runtime.store.model_responses(run_id)
    assert completed.run.status == "completed"
    assert len(requests) == len(responses) == 2
    assert requests[0].request_id != requests[1].request_id
    assert responses[0].status == "failed"
    assert responses[1].status == "completed"


def test_restart_after_durable_model_response_resumes_missing_tool_call(tmp_path: Path) -> None:
    target = tmp_path / "recover-response.txt"
    target.write_text("recover-response", encoding="utf-8")
    root = tmp_path / "operations"
    first_provider = FakeModelProvider(
        [_tool_response({"id": "recover-call", "name": "fixture:discover", "arguments": {}})]
    )
    tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
    first = AgentService(root=root, model_port=first_provider, tool_port=tools)
    run_id = _start_waiting(first)
    assert first.model_loop is not None

    first.model_loop._execute_tool_calls = lambda *_args, **_kwargs: (_ for _ in ()).throw(  # type: ignore[method-assign]
        RuntimeError("crash-after-model-response")
    )
    with pytest.raises(RuntimeError, match="crash-after-model-response"):
        first.run(run_id)

    second_provider = FakeModelProvider([])
    recovered = AgentService(root=root, model_port=second_provider, tool_port=tools)
    completed = recovered.run(run_id)

    assert completed.run.status == "completed"
    assert second_provider.requests == []
    assert len(tools.calls) == 1
    assert len(recovered.runtime.store.model_requests(run_id)) == 1
    assert len(recovered.runtime.store.model_observations(run_id)) == 1


def test_restart_after_durable_tool_observation_does_not_repeat_side_effect(
    tmp_path: Path,
) -> None:
    target = tmp_path / "recover-observation.txt"
    target.write_text("recover-observation", encoding="utf-8")
    root = tmp_path / "operations"
    first_provider = FakeModelProvider(
        [_tool_response({"id": "recover-observation", "name": "fixture:discover", "arguments": {}})]
    )
    tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
    first = AgentService(root=root, model_port=first_provider, tool_port=tools)
    run_id = _start_waiting(first)
    assert first.model_loop is not None
    first.model_loop._target_from_results = lambda _results: (_ for _ in ()).throw(  # type: ignore[method-assign]
        RuntimeError("crash-after-tool-observation")
    )

    with pytest.raises(RuntimeError, match="crash-after-tool-observation"):
        first.run(run_id)
    assert len(tools.calls) == 1
    assert len(first.runtime.store.model_observations(run_id)) == 1

    second_provider = FakeModelProvider([])
    recovered = AgentService(root=root, model_port=second_provider, tool_port=tools)
    completed = recovered.run(run_id)

    assert completed.run.status == "completed"
    assert second_provider.requests == []
    assert len(tools.calls) == 1


def test_provider_claimed_response_hash_is_not_trusted(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [
            ModelResponse(
                request_id="placeholder",
                status="completed",
                text="tampered",
                response_hash="0" * 64,
            )
        ]
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider)
    run_id = _start_waiting(service)

    with pytest.raises(ModelIntegrityError, match="response_hash_mismatch"):
        service.run(run_id)

    record = service.runtime.store.model_responses(run_id)[0]
    assert record.status == "integrity_error"
    assert record.claimed_response_hash == "0" * 64
    assert record.response_hash != record.claimed_response_hash


def test_tool_claimed_hashes_are_not_trusted(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [_tool_response({"id": "bad-tool", "name": "fixture:discover", "arguments": {}})]
    )
    tools = ScriptedToolPort(
        {"fixture:discover": {"target": "fixture://target"}},
        result_hashes={"fixture:discover": ("a" * 64, "b" * 64)},
    )
    service = AgentService(root=tmp_path / "operations", model_port=provider, tool_port=tools)
    run_id = _start_waiting(service)

    with pytest.raises(ModelIntegrityError, match="tool_result_hash_mismatch"):
        service.run(run_id)

    observation = service.runtime.store.model_observations(run_id)[0]
    assert observation.status == "integrity_error"
    assert observation.metadata["claimed_input_hash"] == "a" * 64


def test_model_loop_cancel_propagates_to_active_provider_request(tmp_path: Path) -> None:
    entered = threading.Event()
    provider: FakeModelProvider

    def blocking(request: Any) -> BaseException:
        entered.set()
        deadline = time.monotonic() + 3
        while request.request_id not in provider.cancelled and time.monotonic() < deadline:
            time.sleep(0.01)
        return RuntimeError("provider-cancelled")

    provider = FakeModelProvider([blocking])
    service = AgentService(
        root=tmp_path / "operations",
        model_port=provider,
        model_max_retries=0,
    )
    run_id = _start_waiting(service)
    errors: list[BaseException] = []

    def run_loop() -> None:
        try:
            service.run(run_id)
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=run_loop)
    thread.start()
    assert entered.wait(timeout=3)
    assert service.model_loop is not None
    service.model_loop.cancel(run_id)
    thread.join(timeout=5)

    assert not thread.is_alive()
    assert provider.cancelled
    assert errors and isinstance(errors[0], ModelLoopError)


def test_provider_switch_does_not_change_goal_evidence_or_terminal_semantics(
    tmp_path: Path,
) -> None:
    target = tmp_path / "provider-switch.txt"
    target.write_text("provider-switch", encoding="utf-8")
    outcomes = []
    for name in ("provider-a", "provider-b"):
        capabilities = ModelCapabilities(
            native_system_role=True,
            native_tool_calls=True,
            parallel_tool_calls=False,
            structured_output=True,
            usage_reporting=True,
            metadata={"provider": name, "model": f"{name}-model"},
        )
        provider = FakeModelProvider(
            [_tool_response({"id": "discover", "name": "fixture:discover", "arguments": {}}, provider=name)],
            provider=name,
            model=f"{name}-model",
            capabilities=capabilities,
        )
        tools = ScriptedToolPort({"fixture:discover": {"target": str(target)}})
        service = AgentService(
            root=tmp_path / name,
            model_port=provider,
            tool_port=tools,
        )
        completed = service.run(_start_waiting(service))
        outcomes.append(
            (
                completed.goal.objective,
                completed.run.status,
                completed.terminal.success,
                tuple(sorted(item.artifact_type for item in completed.evidence)),
                tuple(sorted(completed.terminal.satisfied)),
                tuple(sorted(completed.terminal.missing)),
            )
        )

    assert outcomes[0] == outcomes[1]
