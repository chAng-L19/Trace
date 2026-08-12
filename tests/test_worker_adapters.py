from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import ToolDefinition, ToolResult, WorkerTask
from redteam_agent.runtime.store_common import ImmutableRecordError


class FixtureToolPort:
    def __init__(self, *, error: BaseException | None = None) -> None:
        self.error = error
        self.calls = []

    def discover(self):
        return (
            ToolDefinition(
                qualified_name="fixture:inspect",
                name="inspect",
                server="fixture",
                input_schema={"type": "object"},
            ),
        )

    def invoke(self, call):
        self.calls.append(call)
        if self.error is not None:
            raise self.error
        return ToolResult(
            call_id=call.call_id,
            status="success",
            tool_name=call.tool_name,
            output={"results": ["verified"]},
        )

    def reconcile(self, call):
        return None

    def cancel(self, call_id):
        return True


def _run(service: AgentService, session_id: str) -> str:
    return service.start(
        StartRequest(session_id=session_id, objective="Exercise the configured worker adapter")
    ).single.run.run_id


def test_mcp_worker_persists_full_result_artifact_and_replays(tmp_path: Path) -> None:
    tools = FixtureToolPort()
    service = AgentService(root=tmp_path / "runtime", tool_port=tools)
    run_id = _run(service, "mcp-success")
    task = WorkerTask(
        task_id="mcp-task",
        run_id=run_id,
        capability="mcp.fixture:inspect",
        payload={"tool_name": "fixture:inspect", "arguments": {"target": "fixture"}},
        idempotency_key="mcp-idempotency",
        metadata={"worker_kind": "mcp"},
    )

    first = service.execute_worker(task)
    second = service.execute_worker(task)

    assert first == second
    assert first.status == "completed"
    assert len(tools.calls) == 1
    stored = service.runtime.artifacts.read_json(first.artifact_refs[0], run_id=run_id)
    assert stored["output"] == {"results": ["verified"]}


def test_mcp_worker_exception_becomes_durable_failed_result(tmp_path: Path) -> None:
    tools = FixtureToolPort(error=ConnectionError("api_key=super-secret-value"))
    service = AgentService(root=tmp_path / "runtime", tool_port=tools)
    run_id = _run(service, "mcp-failure")
    task = WorkerTask(
        task_id="mcp-failure-task",
        run_id=run_id,
        capability="mcp.fixture:inspect",
        payload={"tool_name": "fixture:inspect", "arguments": {}},
        idempotency_key="mcp-failure-idempotency",
        metadata={"worker_kind": "mcp"},
    )

    result = service.execute_worker(task)

    assert result.status == "failed"
    assert result.retryable is True
    assert "super-secret-value" not in result.error
    assert service.worker_status(task.task_id).status == "failed"


def test_codex_handoff_replay_and_cancel_are_persistent(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "codex-handoff")
    task = WorkerTask(
        task_id="codex-task",
        run_id=run_id,
        capability="codex.handoff",
        payload={"phase": "controlled_validation", "command": ["fixture", "--check"]},
        idempotency_key="codex-idempotency",
        metadata={"worker_kind": "codex_handoff"},
    )

    first = service.execute_worker(task)
    second = service.execute_worker(task)

    assert first == second
    assert first.status == "waiting_worker"
    assert service.cancel_worker(task.task_id) is True
    assert service.worker_status(task.task_id).status == "cancelled"


def test_docker_adapter_reports_configured_capability_gap_without_success(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "docker-unavailable")
    task = WorkerTask(
        task_id="docker-task",
        run_id=run_id,
        capability="docker.command",
        payload={"image": "fixture:latest", "argv": ["true"]},
        idempotency_key="docker-idempotency",
        metadata={"worker_kind": "docker"},
    )

    result = service.execute_worker(task)

    assert result.status == "unavailable"
    assert result.retryable is True
    assert service.worker_status(task.task_id).status == "unavailable"


def test_worker_task_id_cannot_be_rebound_to_another_run(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    first_run = _run(service, "identity-first")
    second_run = _run(service, "identity-second")
    first = WorkerTask(
        task_id="global-task-id",
        run_id=first_run,
        capability="docker.command",
        payload={},
        idempotency_key="first-key",
        metadata={"worker_kind": "docker"},
    )
    second = WorkerTask(
        task_id="global-task-id",
        run_id=second_run,
        capability="docker.command",
        payload={},
        idempotency_key="second-key",
        metadata={"worker_kind": "docker"},
    )
    service.execute_worker(first)

    with pytest.raises(ImmutableRecordError, match="worker_task_identity_conflict"):
        service.execute_worker(second)

