from __future__ import annotations

from pathlib import Path

from redteam_agent.cli import _self_test
from redteam_agent.runtime.mcp_server import RuntimeMcpServer
from redteam_agent.runtime.mcp_transport import _default_config_paths
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.session_bridge import sync_session_summary
from redteam_agent.runtime.workflow_registry import WorkflowRegistry


def test_packaged_workflow_registry_loads_the_single_dag() -> None:
    registry = WorkflowRegistry()

    assert [workflow.workflow_id for workflow in registry.load()] == ["generic-adaptive"]
    assert registry.profile_ids


def test_agent_home_is_the_default_config_root(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("REDTEAM_AGENT_HOME", str(tmp_path / "agent-home"))
    monkeypatch.delenv("REDTEAM_AGENT_CONFIG", raising=False)
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))

    assert _default_config_paths([]) == [(tmp_path / "agent-home" / "config.toml").resolve()]


def test_codex_session_bridge_is_opt_in(monkeypatch) -> None:
    monkeypatch.delenv("CODEX_HOME", raising=False)

    assert sync_session_summary("standalone", {"run_id": "run-1"}) is False


def test_mcp_server_uses_independent_agent_identity(tmp_path: Path) -> None:
    server = RuntimeMcpServer(OperationRuntime(root=tmp_path / "operations", register_builtins=False))

    response = server.handle(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {"clientInfo": {"name": "pytest", "version": "1"}, "capabilities": {}},
        }
    )

    assert response["result"]["serverInfo"] == {"name": "redteam-agent-runtime", "version": "1"}


def test_isolated_cli_self_test_reaches_terminal_success(tmp_path: Path) -> None:
    assert _self_test(tmp_path / "self-test") == 0

