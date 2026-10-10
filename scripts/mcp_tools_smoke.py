"""Offline MCP tools route checks using real broker discovery and account auth."""
from __future__ import annotations

import json
import os
import sys
import tempfile
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import patch
from urllib.parse import quote

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.application.bootstrap import reload_mcp


class CatalogClient:
    def __init__(self, name):
        self.name = name

    def list_tools(self):
        return [
            {"name": "browser_snapshot", "description": f"读取快照 / {self.name}",
             "inputSchema": {"type": "object"}, "annotations": {"readOnlyHint": True}},
            {"name": "excluded", "description": "Must remain outside the broker catalog"},
        ]

    def close(self):
        pass


def fixture_client(spec, **_):
    if spec.name == "failed":
        raise RuntimeError("fixture_connection_failed")
    return CatalogClient(spec.name)


def exercise(service, root):
    api = WebApi(service)
    broker = service.runtime.broker
    token = api.control.login("trace", "tools-pass")
    headers = {"Authorization": "Bearer " + token}

    def request(path, *, auth=headers, method="GET", status=200):
        response = api.dispatch(method, path, headers=auth)
        assert response.status == status, (path, response.status, response.payload())
        payload = response.payload()
        assert payload["schema_version"] == 1 and payload["ok"] == (status < 400)
        return payload

    def tools(server_id, **kwargs):
        return request(f"/api/mcp/{quote(server_id, safe='')}/tools", **kwargs)

    available = tools("playwright")
    assert available["server"]["source"] == "catalog"
    assert available["server"]["status"]["status"] == "available"
    assert available["tools"] == [] and available["tool_count"] == 0
    assert not available["server"]["status"]["configured"]
    assert tools("missing", status=404)["error"] == "mcp_server_not_found:missing"
    tools("builtin", status=404)

    config = root / "external.toml"
    config.write_text('''
[automation.tool_capabilities]
"config.server/节点:browser_snapshot" = ["browser_snapshot", "read_page"]
[mcp_servers."config.server/节点"]
transport = "http"
url = "http://fixture.invalid/config"
include_tools = ["browser_*"]
headers = {Authorization = "config-secret"}
[mcp_servers.run]
command = "fixture"
scope = "run"
exclude_tools = ["excluded"]
[mcp_servers.disabled]
command = "fixture"
args = ["disabled"]
enabled = false
[mcp_servers.failed]
command = "fixture"
args = ["failed"]
[mcp_servers.duplicate]
command = "fixture"
[mcp_servers.empty]
command = "fixture"
args = ["empty"]
include_tools = ["absent"]
''', encoding="utf-8")
    service.config_paths = [config]
    service.control.save_mcp({"server_id": "managed", "command": "fixture",
        "args": ["managed"], "exclude_tools": ["excluded"],
        "env": {"FIXTURE_TOKEN": "managed-secret"}})
    with patch.object(broker, "_create_client", side_effect=fixture_client):
        reload_mcp(service)
    service.control.save_mcp({"server_id": "pending", "command": "fixture"})
    # Even a native adapter sharing the node's server name is not an MCP tool.
    broker.register_adapter(name="native", server="managed", capabilities=("local",),
                            adapter=lambda _: {"unexpected": "execution"})
    descriptors = broker.descriptors()
    statuses = broker.server_statuses()
    shared_clients, run_clients = dict(broker._clients), dict(broker._run_clients)
    assert not run_clients and not service.list_runs(summary_only=True)

    # Only setup may discover tools. Every route below must use the cached catalog.
    with ExitStack() as stack:
        forbidden = [stack.enter_context(patch.object(target, name,
            side_effect=AssertionError(f"view must not call {name}")))
            for target, name in (
                (broker, "_create_client"), (broker, "discover_from_configs"),
                (broker, "refresh"), (broker, "_client_for"), (broker, "call"),
                (service, "start"), (api, "_reload_control_plane"),
            )]
        for client in shared_clients.values():
            forbidden.append(stack.enter_context(patch.object(client, "list_tools",
                side_effect=AssertionError("view must not rediscover tools"))))
        listed = request("/api/mcp")
        servers = {item["server_id"]: item for item in listed["servers"]}
        assert listed["tools"] == 3
        for server_id, source, state in (
            ("managed", "managed", "connected"),
            ("config.server/节点", "config", "connected"),
            ("run", "config", "catalogued"),
            ("empty", "config", "connected"),
            ("disabled", "config", "disabled"),
            ("failed", "config", "failed"),
            ("duplicate", "config", "duplicate"),
            ("pending", "managed", "configured"),
            ("playwright", "catalog", "available"),
        ):
            result = tools(server_id)
            assert result["server"] == servers[server_id]
            assert result["server"]["source"] == source
            assert result["server"]["status"]["status"] == state
            expected = [
                {"name": item.name, "qualified_name": item.qualified_name,
                 "description": item.description, "capabilities": list(item.capabilities)}
                for item in descriptors
                if item.server == server_id and item.source.startswith("live-mcp")
            ]
            assert result["tools"] == expected, result
            assert result["tool_count"] == len(expected)
            assert "config-secret" not in json.dumps(result)
            assert "managed-secret" not in json.dumps(result)
        managed = tools("managed")
        assert tools("config.server/节点")["tools"][0]["capabilities"] == ["browser_snapshot", "read_page"]
        assert managed["tools"][0]["description"] == "读取快照 / managed"
        assert tools("failed")["server"]["status"]["error"] == "fixture_connection_failed"
        assert not tools("run")["server"]["status"]["active_run_clients"]

        # Disabled durable configuration wins over a still-cached connected catalog.
        service.control.set_mcp("managed", enabled=False)
        disabled = tools("managed")
        assert not disabled["server"]["enabled"] and disabled["tools"] == []
        assert disabled["server"]["status"]["status"] == "connected"
        assert not disabled["server"]["status"]["callable"]
        service.control.set_mcp("managed", enabled=True)
        api._mcp_restore_error = "mcp_restore_failed:RuntimeError"
        failed = tools("managed")
        assert failed["server"]["status"]["status"] == "failed" and failed["tools"] == []
        api._mcp_restore_error = ""
        assert tools("managed") == managed

        # Deleted nodes cannot expose residual broker entries or a public preset.
        service.control.delete_mcp("config.server/节点",
            service.control.mcp_source_spec("config.server/节点", service.config_paths))
        tools("config.server/节点", status=404)
        service.control.delete_mcp("playwright", service.control.mcp_source_spec("playwright", ()))
        tools("playwright", status=404)
        tools("missing", status=404)
        request("/api/mcp/managed/tools/extra", status=404)
        request("/api/mcp/managed/unknown", status=404)

        for auth in ({}, {"Authorization": "Bearer invalid"}):
            assert tools("managed", auth=auth, status=401)["error"] == "authentication_required"
            tools("missing", auth=auth, status=401)
        assert tools("managed", auth={"Cookie": f"trace_session={token}"}) == managed
        added = api.dispatch("POST", "/api/auth/users", headers=headers,
            body={"username": "reader", "password": "reader-pass"})
        assert added.status == 200, added.payload()
        reader = {"Authorization": "Bearer " + api.control.login("reader", "reader-pass")}
        assert tools("managed", auth=reader) == managed
        for method in ("POST", "DELETE"):
            tools("managed", method=method, status=405)
            tools("managed", method=method, auth=reader, status=403)
        tools("managed", method="PUT", status=405)
        api.control.logout({"authorization": reader["Authorization"]})
        tools("managed", auth=reader, status=401)
        for guard in forbidden:
            guard.assert_not_called()

    assert broker.server_statuses() == statuses
    assert broker._clients == shared_clients and broker._run_clients == run_clients
    assert not service.list_runs(summary_only=True)


def main():
    with tempfile.TemporaryDirectory(prefix="trace-mcp-tools-") as directory, patch.dict(
        os.environ, {"TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "tools-pass"}
    ):
        root = Path(directory)
        with AgentService(root=root / "runtime", load_external_configuration=False) as service:
            exercise(service, root)
    print(json.dumps({"ok": True, "checks": ["connected", "catalogued", "disabled",
        "failed", "duplicate", "empty", "configured", "catalog", "config", "managed",
        "missing-and-deleted", "exact-node-and-tool-filter", "capabilities", "secret-omission",
        "stale-catalog", "restore-error", "bearer-cookie-member-revocation", "method-boundaries",
        "no-discovery-connection-or-execution"]}))


if __name__ == "__main__":
    main()
