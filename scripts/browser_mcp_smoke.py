"""Real stdio + installed Chrome/Playwright regression; only a loopback fixture."""
from __future__ import annotations

import fnmatch
import json
import os
import os
import subprocess
import sys
import tempfile
import threading
import tomllib
from contextlib import contextmanager
from dataclasses import replace
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1] / "src"
sys.path.insert(0, str(SOURCE))

from redteam_agent.runtime.browser_mcp import TOOLS
from redteam_agent.runtime.mcp_clients import StdioMcpClient, _child_environment
from redteam_agent.runtime.mcp_config import PRESET_DEFAULTS, PUBLIC_MCP_PRESETS, parse_mcp_server_specs
from redteam_agent.runtime.tool_broker import ToolBroker
from redteam_agent import AgentService
from redteam_agent.application.bootstrap import reload_mcp


class Fixture(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/broken":
            self.connection.close()
            return
        status = 404 if self.path == "/missing" else 200
        body = b"""<!doctype html><meta charset=utf-8><title>Browser MCP fixture</title>
        <h1>Browser fixture</h1><label>Name <input id=name></label>
        <input id=agree type=checkbox><select id=choice><option>a</option><option>b</option></select>
        <button id=submit onclick="document.querySelector('#result').textContent='Saved'; console.log('saved')">Save</button>
        <p id=result>Pending</p><a href=/second>Next page</a><script>console.log('fixture ready');</script>"""
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


def specification(root):
    spec, = parse_mcp_server_specs(root / "fixture.toml", {"mcp_servers": {"playwright": {"preset": "playwright"}}})
    assert spec.command == sys.executable
    assert spec.args == PUBLIC_MCP_PRESETS["playwright"]["args"]
    return replace(spec.render(run_id="smoke", workspace=root), env={
        "PYTHONPATH": str(SOURCE), "PYTHONDONTWRITEBYTECODE": "1"})


@contextmanager
def client_for(spec, root):
    root.mkdir(parents=True, exist_ok=True)
    client = StdioMcpClient(spec.name, spec.command, spec.args, spec.env, cwd=root)
    try:
        yield client
    finally:
        # EOF exercises the server's real cleanup, rather than killing Python.
        try:
            client.process.stdin.close()
            assert client.process.wait(timeout=20) == 0, client.stderr_snapshot()
        finally:
            client.close()


def protocol_smoke(root, spec):
    requests = ["not json", "[]", json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"})]
    for identity, method, params in ((1, "initialize", {}), (2, "tools/list", {}), (3, "ping", {}),
                                     (4, "unknown", {}), (5, "tools/call", {"name": "missing"}),
                                     (6, "tools/call", {"name": "browser_navigate", "arguments": {}}),
                                     (7, "tools/call", {"name": "browser_click", "arguments": []})):
        requests.append(json.dumps({"jsonrpc": "2.0", "id": identity, "method": method, "params": params}))
    result = subprocess.run([spec.command, *spec.args], input="\n".join(requests) + "\n", capture_output=True,
                            encoding="utf-8", env=_child_environment(spec.env), cwd=root, timeout=20)
    assert result.returncode == 0, result.stderr
    frames = [json.loads(line) for line in result.stdout.splitlines()]
    assert len(frames) == 9, frames
    assert [frame["error"]["code"] for frame in frames[:2]] == [-32700, -32600]
    assert frames[2]["result"]["serverInfo"]["name"] == "trace-browser-mcp"
    assert len(frames[3]["result"]["tools"]) == len(TOOLS)
    assert frames[4]["result"] == {}
    assert [frame["error"]["code"] for frame in frames[5:]] == [-32601, -32602, -32602, -32602]
    assert not (root / "playwright-output").exists(), "Discovery must not launch a browser"


def process_parents():
    if os.name == "nt":
        powershell = Path(os.environ["SYSTEMROOT"]) / "System32/WindowsPowerShell/v1.0/powershell.exe"
        result = subprocess.run([str(powershell), "-NoProfile", "-Command",
            "Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId | ConvertTo-Json -Compress"],
            capture_output=True, text=True, check=True)
        return {item["ProcessId"]: item["ParentProcessId"] for item in json.loads(result.stdout)}
    result = subprocess.run(["ps", "-eo", "pid=,ppid="], capture_output=True, text=True, check=True)
    return dict(tuple(map(int, line.split())) for line in result.stdout.splitlines())


def descendants(pid, parents):
    found = {pid}
    while children := {child for child, parent in parents.items() if parent in found} - found:
        found.update(children)
    return found


def broker_lifecycle(root, base):
    spec = specification(root)
    config = root / "broker.toml"
    config.write_text('[mcp_servers.playwright]\npreset = "playwright"\n'
                      '[mcp_servers.playwright.env]\nPYTHONPATH = ' + json.dumps(str(SOURCE)) +
                      '\nPYTHONDONTWRITEBYTECODE = "1"\n', encoding="utf-8")
    with ToolBroker() as broker:
        broker.bind_workspace_root(root / "runs")
        catalog = broker.discover_from_configs((config,))
        assert len(catalog) == len(TOOLS), broker.discovery_errors
        assert not broker._clients and not broker._run_clients, "Discovery must not retain a shared process"
        navigate = next(item for item in catalog if item.name == "browser_navigate")
        for run_id in ("run-a", "run-b"):
            result = broker.call(navigate, {"url": base}, run_id=run_id)
            assert result.status == "success", result.error
        first, second = (broker._run_clients[("playwright", run)] for run in ("run-a", "run-b"))
        parents = process_parents()
        first_pids, second_pids = descendants(first.process.pid, parents), descendants(second.process.pid, parents)
        assert len(first_pids) > 2 and len(second_pids) > 2
        assert first_pids.isdisjoint(second_pids)
        broker.close_run("run-a")
        assert first.process.returncode == 0
        assert not first_pids.intersection(process_parents()), "run-a left child processes"
        assert second.request("ping") == {}
        assert not (broker._workspace_for("run-a") / "playwright-output/.playwright-session").exists()
    assert second.process.returncode == 0
    assert not second_pids.intersection(process_parents()), "broker.close left child processes"
    assert not (broker._workspace_for("run-b") / "playwright-output/.playwright-session").exists()

    # Missing workspace must use an isolated temporary directory, never /playwright-output.
    empty = replace(spec, args=PUBLIC_MCP_PRESETS["playwright"]["args"])
    empty = empty.render(workspace=None)
    with client_for(empty, root) as client:
        result = client.call_tool("browser_take_screenshot", {}, timeout=45)
        assert not result["isError"], result
        output = Path(result["structuredContent"]["screenshot_path"])
        assert output.parent.parent == Path(tempfile.gettempdir()).resolve()
        assert output.parent.name.startswith("trace-browser-mcp-")
    assert not output.parent.exists()


def legacy_reopen(root):
    old = {"preset": "playwright", "scope": "run", "command": "npx", "args": [
        "-y", "@playwright/mcp@0.0.79", "--isolated", "--headless", "--image-responses=omit",
        "--codegen=none", "--block-service-workers", "--output-dir={workspace}/playwright-output",
        "--output-max-size=52428800"], "env": {"PYTHONPATH": str(SOURCE), "PYTHONDONTWRITEBYTECODE": "1"}}
    # A saved default survives upgrades in SQLite. Test the real export/restore path.
    state = root / "legacy-state"
    with AgentService(root=state, load_external_configuration=False) as service:
        service.control.save_mcp({"server_id": "playwright", **old})
        path = service.control.write_mcp_config()
        config = root / "config.toml"
        config.write_text(path.read_text(encoding="utf-8"), encoding="utf-8")
    with AgentService(root=state, load_external_configuration=False) as service:
        service.config_paths = [config]
        reload_mcp(service)
        spec = service.runtime.broker._server_configs["playwright"]
        assert spec.command == sys.executable and spec.args == PUBLIC_MCP_PRESETS["playwright"]["args"]
        assert service.runtime.broker.server_statuses()["playwright"]["tool_count"] == len(TOOLS)
        # Loading changes the effective launcher, not the user's persisted JSON.
        with service.runtime.store.connection() as connection:
            saved = json.loads(connection.execute(
                "SELECT spec_json FROM trace_mcp_servers WHERE server_id='playwright'").fetchone()[0])
        assert saved["command"] == "npx" and saved["args"] == old["args"]
        for source in (config, path):
            restored, = parse_mcp_server_specs(source, tomllib.loads(source.read_text(encoding="utf-8")))
            assert restored.command == sys.executable and restored.args == spec.args
        for overrides in ({"command": "npx.cmd"}, {"preset": ""},
                          {"args": [*old["args"], "--custom-flag"]},
                          {"args": ["-y", "@playwright/mcp@0.0.80", *old["args"][2:]]}):
            custom = {**old, **overrides}
            parsed, = parse_mcp_server_specs(config, {"mcp_servers": {"custom": custom}})
            assert parsed.command == custom["command"] and parsed.args == tuple(custom["args"])


def browser_smoke(root, spec, base):
    called = set()
    with client_for(spec, root) as client:
        catalog = client.list_tools()
        assert {tool["name"] for tool in catalog} == {tool["name"] for tool in TOOLS}
        for pattern in PRESET_DEFAULTS["playwright"]["include_tools"]:
            assert any(fnmatch.fnmatchcase(tool["name"], pattern) for tool in catalog), pattern
        with ToolBroker() as broker:
            broker._register_mcp_tools(spec, catalog)
            assert len(broker.descriptors()) == len(TOOLS)
            for tool in catalog:
                assert spec.accepts_tool(tool["name"])
                assert spec.tool_is_read_only(tool["name"], tool["annotations"]) == tool["annotations"]["readOnlyHint"]
                descriptor = next(item for item in broker.descriptors() if item.name == tool["name"])
                assert descriptor.side_effecting != tool["annotations"]["readOnlyHint"]

        def call(name, **arguments):
            called.add(name)
            result = client.call_tool(name, arguments, timeout=45)
            assert not result.get("isError"), (name, result)
            assert json.loads(result["content"][0]["text"]) == result["structuredContent"]
            return result["structuredContent"]

        first = call("browser_navigate", url=base)
        assert first["status_code"] == 200 and first["title"] == "Browser MCP fixture"
        assert "Browser fixture" in call("browser_snapshot", url=base + "/missing")["snapshot"]
        assert call("browser_snapshot")["url"] == base + "/", "Snapshot must not navigate"
        call("browser_type", selector="#name", text="用户")
        call("browser_type", selector="#name", text=" A", slowly=True, submit=True)
        assert call("browser_find", selector="#name")["elements"][0]["value"] == "用户 A"
        call("browser_fill_form", fields=[{"selector": "#name", "value": "form"},
             {"selector": "#agree", "type": "checkbox", "value": True},
             {"selector": "#choice", "type": "select", "value": "b"}])
        assert call("browser_evaluate", expression="[document.querySelector('#agree').checked, document.querySelector('#choice').value]")["value"] == [True, "b"]
        assert call("browser_select_option", selector="#choice", values=["a"])["values"] == ["a"]
        call("browser_type", selector="#name", text="keyboard")
        call("browser_press_key", key="Backspace")
        assert call("browser_find", selector="#name")["elements"][0]["value"] == "keyboar"
        call("browser_click", selector="#submit")
        call("browser_wait_for", text="Saved", textGone="Pending", time=0)
        assert any(item["text"] == "saved" for item in call("browser_console_messages")["messages"])
        call("browser_evaluate", expression="async () => { await fetch('/missing'); try { await fetch('/broken'); } catch {} console.error('console error'); localStorage.setItem('isolation', 'first'); return 'done'; }")
        requests = call("browser_network_requests")["requests"]
        assert any(item["url"] == base + "/missing" and item["status"] == 404 for item in requests)
        assert any(item["url"] == base + "/broken" and item.get("error") for item in requests)
        assert any(item["type"] == "error" and item["text"] == "console error"
                   for item in call("browser_console_messages")["messages"])
        shot = Path(call("browser_take_screenshot", output_path="fixture.png", full_page=True)["screenshot_path"])
        assert shot.is_relative_to(root) and shot.read_bytes().startswith(b"\x89PNG\r\n\x1a\n")
        call("browser_navigate", url=base + "/second")
        assert call("browser_navigate_back")["url"] == base + "/"
        assert call("browser_navigate_forward")["url"] == base + "/second"
        assert len(call("browser_tabs", action="new")["tabs"]) == 2
        call("browser_navigate", url=base + "/missing")
        assert len(call("browser_tabs", action="list")["tabs"]) == 2
        assert call("browser_tabs", action="select", index=0)["url"] == base + "/second"
        assert len(call("browser_tabs", action="close", index=1)["tabs"]) == 1
        error = client.call_tool("browser_tabs", {"action": "select", "index": 99}, timeout=10)
        assert error["isError"] and "browser_tab_not_found" in error["content"][0]["text"]
        error = client.call_tool("browser_evaluate", {"expression": "() => {throw new Error('fixture execution error')}"}, timeout=10)
        assert error["isError"] and "fixture execution error" in error["content"][0]["text"]

        other_root = root / "other"
        with client_for(specification(other_root), other_root) as other:
            second = other.call_tool("browser_navigate", {"url": base}, timeout=45)["structuredContent"]
            assert second["session_id"] != first["session_id"]
            value = other.call_tool("browser_evaluate", {"expression": "localStorage.getItem('isolation')"}, timeout=10)
            assert value["structuredContent"]["value"] is None
        assert not (other_root / "playwright-output/.playwright-session").exists()
        closed = call("browser_close")
        assert closed["closed"] and closed["cleanup"][0]["resources_closed"] == ["page", "context", "browser"]
        assert not (root / "playwright-output/.playwright-session").exists()
        fresh = call("browser_navigate", url=base)
        assert fresh["session_id"] != first["session_id"]
        assert call("browser_evaluate", expression="localStorage.getItem('isolation')")["value"] is None
        assert call("browser_tabs", action="close", index=0)["url"] == "about:blank"
    assert not (root / "playwright-output/.playwright-session").exists(), "EOF must close the browser"
    assert called == {tool["name"] for tool in TOOLS}, called


def main():
    with patch.dict(os.environ, {"TRACE_TOOLS_HOME": "fixture-tools-home"}):
        assert _child_environment({})["TRACE_TOOLS_HOME"] == "fixture-tools-home"
    with tempfile.TemporaryDirectory(prefix="trace-browser-mcp-") as temporary:
        root = Path(temporary)
        spec = specification(root)
        custom, = parse_mcp_server_specs(root / "custom.toml", {"mcp_servers": {"custom": {
            "preset": "playwright", "command": "npx", "args": ["custom-mcp"], "env": {"FIXTURE": "unchanged"}}}})
        assert (custom.command, custom.args, custom.env) == ("npx", ("custom-mcp",), {"FIXTURE": "unchanged"})
        remote, = parse_mcp_server_specs(root / "remote.toml", {"mcp_servers": {"remote": {
            "preset": "playwright", "url": "http://fixture.invalid/mcp"}}})
        assert remote.transport == "http" and not remote.command and not remote.args
        protocol_smoke(root, spec)
        legacy_reopen(root)
        missing = replace(spec, args=(*spec.args, "--executable-path", str(root / "missing-chrome")))
        with client_for(missing, root) as client:
            assert len(client.list_tools()) == len(TOOLS)
            result = client.call_tool("browser_navigate", {"url": "about:blank"}, timeout=15)
            assert result["isError"] and "missing-chrome" in result["content"][0]["text"]
        server = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            browser_smoke(root, spec, f"http://127.0.0.1:{server.server_port}")
            broker_lifecycle(root, f"http://127.0.0.1:{server.server_port}")
        finally:
            server.shutdown()
            worker.join(5)
            server.server_close()
    print(json.dumps({"status": "passed", "browser_tools": len(TOOLS), "checks": [
        "python_preset", "custom_command_unchanged", "http_unchanged", "stdio_protocol", "catalog_hints",
        "all_browser_tools", "http_status_and_failure", "console", "screenshot", "unicode",
        "execution_errors", "missing_browser_no_download", "process_isolation", "browser_close_reopen", "eof_cleanup",
        "broker_run_scope", "broker_shutdown_descendants", "empty_workspace_temporary_output",
        "legacy_database_reopen", "legacy_managed_and_config", "custom_near_matches_unchanged", "bundled_tools_home"]}))


if __name__ == "__main__":
    main()
