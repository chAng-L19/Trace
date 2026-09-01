from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from redteam_agent.runtime.ida_free_bridge import IdaFreeBridge, TOOL_DEFINITIONS
from redteam_agent.runtime.mcp_config import parse_mcp_server_specs, profile_capabilities


def test_ida_free_preset_is_read_only_and_token_bounded(tmp_path: Path) -> None:
    config = tmp_path / "config.toml"
    payload = {
        "mcp_servers": {
            "ida-free": {
                "preset": "ida_free",
                "command": "{python}",
                "args": ["-m", "redteam_agent.runtime.ida_free_bridge", "--ida", "idat64.exe"],
            }
        }
    }
    spec = parse_mcp_server_specs(config, payload)[0]
    assert spec.scope == "run"
    assert spec.accepts_tool("decompile")
    assert not spec.accepts_tool("rename_local")
    assert spec.tool_is_read_only("decompile") is True
    assert spec.tool_is_read_only("idb_open") is False
    assert profile_capabilities("ida_free", "decompile") == ("binary_reverse", "decompile")
    assert len(TOOL_DEFINITIONS) <= 12
    assert spec.render().command == sys.executable


def test_ida_free_bridge_mcp_initialize_and_tools_list(tmp_path: Path) -> None:
    executable = tmp_path / "idat64.exe"
    executable.write_bytes(b"fixture runtime")
    bridge = IdaFreeBridge(executable, startup_timeout=1)
    try:
        initialized = bridge.handle(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {"protocolVersion": "2025-06-18"},
            }
        )
        assert initialized is not None
        assert initialized["result"]["serverInfo"]["name"] == "ida-free-mcp-bridge"
        listed = bridge.handle({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        assert listed is not None
        assert {item["name"] for item in listed["result"]["tools"]} >= {
            "idb_open",
            "decompile",
            "get_bytes",
        }
    finally:
        bridge.close()


def test_ida_free_installer_path_is_rejected_without_launching(tmp_path: Path) -> None:
    installer = tmp_path / "ida-free-pc_94_x64win.exe"
    installer.write_bytes(b"installer fixture")
    with pytest.raises(ValueError, match="ida_free_installer_not_runtime"):
        IdaFreeBridge(installer)


def test_ida_free_bridge_reports_invalid_tool_as_mcp_error(tmp_path: Path) -> None:
    executable = tmp_path / "idat64.exe"
    executable.write_bytes(b"fixture runtime")
    bridge = IdaFreeBridge(executable, startup_timeout=1)
    try:
        response = bridge.handle(
            {
                "jsonrpc": "2.0",
                "id": 7,
                "method": "tools/call",
                "params": {"name": "unsupported", "arguments": {}},
            }
        )
        assert response is not None
        assert response["result"]["isError"] is True
        assert "ida_free_database_required" in json.dumps(response)
    finally:
        bridge.close()


def test_ida_free_bridge_rejects_duplicate_session_ids(tmp_path: Path) -> None:
    executable = tmp_path / "idat64.exe"
    executable.write_bytes(b"fixture runtime")
    binary = tmp_path / "fixture.bin"
    binary.write_bytes(b"MZ")
    bridge = IdaFreeBridge(executable, startup_timeout=1)
    try:
        bridge._pending["duplicate"] = (None, {})  # type: ignore[assignment]
        with pytest.raises(ValueError, match="ida_free_session_exists"):
            bridge._launch(str(binary), "duplicate")
    finally:
        bridge.close()
