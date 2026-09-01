from __future__ import annotations

import json
import sys
import threading
import time
from pathlib import Path
from typing import Any, Mapping

import pytest

import redteam_agent.runtime.mcp_broker as mcp_broker_module
import redteam_agent.runtime.tool_broker as broker_module
from redteam_agent.runtime.mcp_config import parse_mcp_server_specs, profile_capabilities
from redteam_agent.runtime.mcp_clients import StdioMcpClient
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.tool_broker import ToolBroker


class _FakeProcess:
    def __init__(self) -> None:
        self.closed = False

    def poll(self) -> int | None:
        return 0 if self.closed else None


class _FakeMcpClient:
    instances: list["_FakeMcpClient"] = []
    tools_by_server: dict[str, list[Mapping[str, Any]]] = {}

    def __init__(
        self,
        server_name: str,
        command: str,
        args: tuple[str, ...],
        env: Mapping[str, str],
        *,
        cwd: Path | None = None,
        startup_timeout: float = 20.0,
        roots: tuple[Path, ...] = (),
    ) -> None:
        self.server_name = server_name
        self.command = command
        self.args = tuple(args)
        self.env = dict(env)
        self.cwd = cwd
        self.startup_timeout = startup_timeout
        self.roots = tuple(roots)
        self.process = _FakeProcess()
        self.calls: list[tuple[str, Mapping[str, Any], str]] = []
        self.cancelled: list[str] = []
        self.tools_changed = False
        type(self).instances.append(self)

    def list_tools(self):
        return tuple(type(self).tools_by_server.get(self.server_name, ()))

    def call_tool(
        self,
        name: str,
        arguments: Mapping[str, Any],
        *,
        timeout: float,
        cancellation_id: str = "",
    ) -> Mapping[str, Any]:
        del timeout
        self.calls.append((name, dict(arguments), cancellation_id))
        if name == "idb_open":
            return {
                "structuredContent": {
                    "success": True,
                    "session": {"session_id": str(arguments.get("preferred_session_id") or "db-1")},
                }
            }
        if name == "idb_close":
            return {
                "structuredContent": {
                    "success": True,
                    "session_id": str(arguments.get("database") or ""),
                }
            }
        return {"structuredContent": {"tool": name, "arguments": dict(arguments)}}

    def consume_tools_changed(self) -> bool:
        changed = self.tools_changed
        self.tools_changed = False
        return changed

    def cancel_request(self, cancellation_id: str) -> bool:
        self.cancelled.append(cancellation_id)
        return True

    def close(self) -> None:
        self.process.closed = True


@pytest.fixture
def fake_mcp(monkeypatch: pytest.MonkeyPatch):
    _FakeMcpClient.instances = []
    _FakeMcpClient.tools_by_server = {
        "playwright": [
            {
                "name": "browser_snapshot",
                "description": "Capture accessibility snapshot",
                "inputSchema": {"type": "object"},
                "annotations": {"readOnlyHint": True},
            },
            {
                "name": "browser_click",
                "description": "Click a snapshot ref",
                "inputSchema": {
                    "type": "object",
                    "required": ["target"],
                    "properties": {"target": {"type": "string"}},
                },
                "annotations": {"readOnlyHint": False},
            },
            {
                "name": "browser_install",
                "description": "Install a browser",
                "inputSchema": {"type": "object"},
            },
        ],
        "ida": [
            {
                "name": "idb_open",
                "description": "Open a binary database",
                "inputSchema": {
                    "type": "object",
                    "required": ["input_path"],
                    "properties": {"input_path": {"type": "string"}},
                },
            },
            {
                "name": "decompile",
                "description": "Decompile a function",
                "inputSchema": {"type": "object"},
            },
            {
                "name": "rename_local",
                "description": "Rename a local variable",
                "inputSchema": {"type": "object"},
            },
        ],
    }
    monkeypatch.setattr(broker_module, "StdioMcpClient", _FakeMcpClient)
    monkeypatch.setattr(mcp_broker_module, "StdioMcpClient", _FakeMcpClient)
    yield


def _config(path: Path) -> None:
    path.write_text(
        """
[mcp_servers.playwright]
preset = "playwright"
scope = "run"
command = "npx"
args = ["@playwright/mcp@0.0.79", "--isolated"]

[mcp_servers.ida]
preset = "ida"
scope = "run"
command = "uv"
args = ["run", "idalib-mcp", "--stdio"]
""".strip(),
        encoding="utf-8",
    )


def test_playwright_and_ida_presets_expose_high_value_bounded_catalog(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    _config(config)
    broker = ToolBroker()
    broker.bind_workspace_root(tmp_path / "runtime" / "workspaces")
    descriptors = {item.qualified_name: item for item in broker.discover_from_configs((config,))}

    assert "playwright:browser_snapshot" in descriptors
    assert "playwright:browser_click" in descriptors
    assert "playwright:browser_install" not in descriptors
    assert descriptors["playwright:browser_snapshot"].side_effecting is False
    assert descriptors["playwright:browser_click"].side_effecting is True
    assert {"browser_automation", "dom_snapshot"} <= set(
        descriptors["playwright:browser_snapshot"].capabilities
    )

    assert "ida:idb_open" in descriptors
    assert "ida:decompile" in descriptors
    assert "ida:rename_local" not in descriptors
    assert descriptors["ida:idb_open"].side_effecting is True
    assert descriptors["ida:decompile"].side_effecting is False
    assert {"binary_reverse", "decompile"} <= set(descriptors["ida:decompile"].capabilities)
    assert broker.server_statuses()["playwright"]["status"] == "catalogued"
    assert broker.server_statuses()["ida"]["scope"] == "run"


def test_run_scoped_mcp_clients_are_isolated_reused_and_closed(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    _config(config)
    broker = ToolBroker()
    broker.bind_workspace_root(tmp_path / "runtime" / "workspaces")
    descriptor = next(
        item
        for item in broker.discover_from_configs((config,))
        if item.qualified_name == "playwright:browser_click"
    )
    catalogue_clients = len(_FakeMcpClient.instances)

    missing = broker.call(descriptor, {"target": "ref"})
    assert missing.status == "failed"
    assert missing.error == "mcp_run_id_required:playwright"

    first = broker.call(
        descriptor,
        {"target": "ref-a"},
        run_id="run-a",
        external_call_id="call-a",
    )
    second = broker.call(descriptor, {"target": "ref-b"}, run_id="run-a")
    third = broker.call(descriptor, {"target": "ref-c"}, run_id="run-b")
    assert first.status == second.status == third.status == "success"
    assert len(_FakeMcpClient.instances) == catalogue_clients + 2
    run_a, run_b = _FakeMcpClient.instances[-2:]
    assert run_a is not run_b
    assert run_a.cwd != run_b.cwd
    assert run_a.roots == (run_a.cwd,)
    assert run_b.roots == (run_b.cwd,)
    assert len(run_a.calls) == 2
    assert len(run_b.calls) == 1

    broker.close_run("run-a")
    assert run_a.process.closed is True
    assert run_b.process.closed is False
    broker.close()
    assert run_b.process.closed is True


def test_ida_run_cleanup_only_closes_sessions_opened_by_that_run(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    _config(config)
    broker = ToolBroker()
    broker.bind_workspace_root(tmp_path / "runtime" / "workspaces")
    descriptors = {item.qualified_name: item for item in broker.discover_from_configs((config,))}

    opened = broker.call(
        descriptors["ida:idb_open"],
        {"input_path": "fixture.bin", "preferred_session_id": "run-db"},
        run_id="run-ida",
    )
    assert opened.status == "success"
    ida_client = _FakeMcpClient.instances[-1]
    reports = broker.close_run("run-ida")

    assert reports == (
        {
            "server": "ida",
            "preset": "ida",
            "resources_discovered": 1,
            "resources_closed": ["run-db"],
            "errors": [],
            "status": "closed",
        },
    )
    assert ("idb_close", {"database": "run-db", "save": True}, "") in ida_client.calls
    assert ida_client.process.closed is True


def test_run_client_tool_change_notification_refreshes_catalog(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    _config(config)
    broker = ToolBroker()
    broker.bind_workspace_root(tmp_path / "runtime" / "workspaces")
    descriptor = next(
        item
        for item in broker.discover_from_configs((config,))
        if item.qualified_name == "playwright:browser_snapshot"
    )
    assert broker.call(descriptor, {}, run_id="run-refresh").status == "success"
    client = _FakeMcpClient.instances[-1]
    _FakeMcpClient.tools_by_server["playwright"].append(
        {
            "name": "browser_console_messages",
            "description": "Read console messages",
            "inputSchema": {"type": "object"},
            "annotations": {"readOnlyHint": True},
        }
    )
    client.tools_changed = True

    assert broker.call(descriptor, {}, run_id="run-refresh").status == "success"
    refreshed = {item.qualified_name: item for item in broker.descriptors()}
    assert "playwright:browser_console_messages" in refreshed
    assert refreshed["playwright:browser_console_messages"].side_effecting is False


def test_duplicate_mcp_process_signature_is_not_started_twice(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    config.write_text(
        """
[mcp_servers.first]
command = "fixture"
args = ["same"]

[mcp_servers.second]
command = "fixture"
args = ["same"]
""".strip(),
        encoding="utf-8",
    )
    broker = ToolBroker()
    broker.discover_from_configs((config,))
    statuses = broker.server_statuses()
    assert statuses["first"]["status"] == "connected"
    assert statuses["second"]["status"] == "duplicate"
    assert "server:second:duplicate_server_signature" in broker.discovery_errors
    assert len(_FakeMcpClient.instances) == 1


def test_same_command_with_distinct_environment_is_not_deduplicated(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    config.write_text(
        """
[mcp_servers.first]
command = "fixture"
args = ["same"]
env = { PROFILE = "one" }

[mcp_servers.second]
command = "fixture"
args = ["same"]
env = { PROFILE = "two" }
""".strip(),
        encoding="utf-8",
    )
    broker = ToolBroker()
    broker.discover_from_configs((config,))
    statuses = broker.server_statuses()
    assert statuses["first"]["status"] == "connected"
    assert statuses["second"]["status"] == "connected"
    assert len(_FakeMcpClient.instances) == 2


def test_installer_command_is_rejected_before_process_launch(
    tmp_path: Path,
    fake_mcp: None,
) -> None:
    config = tmp_path / "config.toml"
    config.write_text(
        """
[mcp_servers.ida]
preset = "ida_free"
command = "ida-free-pc_94_x64win.exe"
""".strip(),
        encoding="utf-8",
    )
    broker = ToolBroker()
    broker.discover_from_configs((config,))
    assert broker.server_statuses()["ida"]["status"] == "failed"
    assert broker.server_statuses()["ida"]["error"].startswith("ida_free_installer_not_runtime")
    assert _FakeMcpClient.instances == []


def test_mcp_spec_expands_environment_and_run_placeholders(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MCP_TOKEN", "fixture-token")
    config = tmp_path / "config.toml"
    payload = {
        "mcp_servers": {
            "fixture": {
                "command": "fixture",
                "args": ["--run={run_id}", "--workspace={workspace}"],
                "env": {"TOKEN": "${MCP_TOKEN}"},
                "scope": "run",
            }
        }
    }
    spec = parse_mcp_server_specs(config, payload)[0].render(
        run_id="run-1",
        workspace=tmp_path / "workspace",
    )
    assert spec.args[0] == "--run=run-1"
    assert str(tmp_path / "workspace") in spec.args[1]
    assert spec.env["TOKEN"] == "fixture-token"
    assert profile_capabilities("ida", "decompile") == ("binary_reverse", "decompile")


def test_stdio_mcp_roots_and_cancellation_protocol(tmp_path: Path) -> None:
    server = tmp_path / "fixture_mcp.py"
    server.write_text(
        """
import json
import sys

def read():
    return json.loads(sys.stdin.readline())

def write(value):
    sys.stdout.write(json.dumps(value, separators=(",", ":")) + "\\n")
    sys.stdout.flush()

initialize = read()
write({"jsonrpc": "2.0", "id": 900, "method": "roots/list", "params": {}})
roots_response = read()
write({"jsonrpc": "2.0", "id": initialize["id"], "result": {
    "protocolVersion": "2025-06-18",
    "capabilities": {"tools": {"listChanged": True}},
    "serverInfo": {"name": "fixture", "version": "1"},
}})
read()  # notifications/initialized

while True:
    message = read()
    if message["method"] == "tools/list":
        write({"jsonrpc": "2.0", "id": message["id"], "result": {"tools": [{
            "name": "slow",
            "description": "wait for cancellation",
            "inputSchema": {"type": "object"},
        }]}})
    elif message["method"] == "tools/call":
        cancelled = read()
        write({"jsonrpc": "2.0", "id": message["id"], "result": {
            "structuredContent": {
                "roots": roots_response["result"]["roots"],
                "cancel_method": cancelled["method"],
                "cancel_request_id": cancelled["params"]["requestId"],
                "tool_request_id": message["id"],
            }
        }})
        break
""".strip(),
        encoding="utf-8",
    )
    root = tmp_path / "run-workspace"
    root.mkdir()
    client = StdioMcpClient(
        "fixture",
        sys.executable,
        (str(server),),
        roots=(root,),
    )
    try:
        assert [item["name"] for item in client.list_tools()] == ["slow"]
        outcome: dict[str, Any] = {}

        def invoke() -> None:
            outcome.update(
                client.call_tool(
                    "slow",
                    {},
                    timeout=5.0,
                    cancellation_id="external-call",
                )
            )

        thread = threading.Thread(target=invoke, daemon=True)
        thread.start()
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            with client._condition:
                if "external-call" in client._active_request_ids:
                    break
            time.sleep(0.01)
        assert client.cancel_request("external-call") is True
        thread.join(timeout=5.0)
        assert not thread.is_alive()
        structured = outcome["structuredContent"]
        assert structured["roots"] == [{"uri": root.as_uri(), "name": root.name}]
        assert structured["cancel_method"] == "notifications/cancelled"
        assert structured["cancel_request_id"] == structured["tool_request_id"]
        with client._condition:
            assert "external-call" not in client._active_request_ids
            assert not client._pending
    finally:
        client.close()


def test_mcp_cleanup_report_is_persisted_as_event_not_evidence(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "state")
    state = runtime.start(
        session_id="mcp-cleanup-event",
        objective="Create a plan for the fixture and do not modify it",
        targets=("fixture",),
    )
    runtime.broker.close_run = lambda run_id: (  # type: ignore[method-assign]
        {
            "server": "playwright",
            "preset": "playwright",
            "resources_discovered": 0,
            "resources_closed": [],
            "errors": [],
            "status": "closed",
        },
    )
    runtime._close_run_resources(state.run_id)

    events = runtime.store.events(state.run_id)
    cleanup_events = [item for item in events if item["event_type"] == "mcp_run_resources_closed"]
    assert len(cleanup_events) == 1
    assert cleanup_events[0]["payload"]["servers"][0]["status"] == "closed"
    assert runtime.store.evidence(state.run_id) == ()
