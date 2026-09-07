from __future__ import annotations

from pathlib import Path
from typing import Any

from redteam_agent.application import AgentService
from redteam_agent.adapters import OperationRuntimeAdapter
from redteam_agent.runtime.mcp_server import RuntimeMcpServer
from redteam_agent.runtime.operation_runtime import OperationRuntime


class _RecordingService:
    def __init__(self, delegate: AgentService) -> None:
        self.delegate = delegate
        self.calls: list[str] = []

    def __getattr__(self, name: str) -> Any:
        return getattr(self.delegate, name)

    def start(self, request: Any):
        self.calls.append("start")
        return self.delegate.start(request)

    def run(self, run_id: str, *args: Any, **kwargs: Any):
        self.calls.append("run")
        return self.delegate.run(run_id, *args, **kwargs)

    def status(self, run_id: str):
        self.calls.append("status")
        return self.delegate.status(run_id)

    def summary(self, run_id: str):
        self.calls.append("summary")
        return self.delegate.summary(run_id)


def test_mcp_public_run_routes_through_agent_service(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "runtime")
    recording = _RecordingService(AgentService(runtime=runtime))
    server = RuntimeMcpServer(runtime, service=recording)
    target = tmp_path / "target.txt"
    target.write_text("fixture\n", encoding="utf-8")

    result = server._call_tool(
        "redteam_run",
        {
            "session_id": "lean-l1",
            "objective": f"Give me a plan for {target}; do not make changes yet",
            "targets": [str(target)],
            "max_actions": 16,
        },
    )["structuredContent"]

    assert result["status"] == "completed"
    assert "start" in recording.calls
    assert "run" in recording.calls
    assert "summary" in recording.calls


def test_runtime_mcp_server_constructs_canonical_service(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "runtime")
    server = RuntimeMcpServer(runtime)

    assert isinstance(server.service, AgentService)
    assert server.service.runtime is runtime


def test_legacy_runtime_adapter_is_an_agent_service_shim(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "runtime")
    adapter = OperationRuntimeAdapter(runtime)

    assert isinstance(adapter.service, AgentService)
    assert adapter.service.runtime is runtime
    assert adapter.runtime is runtime
