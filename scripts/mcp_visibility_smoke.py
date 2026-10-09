"""Offline MCP projection checks; no agent runs or external server processes."""
from __future__ import annotations

import sys
import os
import tempfile
import tomllib
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.runtime.mcp_config import PUBLIC_MCP_PRESETS


class CatalogClient:
    def list_tools(self):
        return [{"name": "browser_snapshot", "inputSchema": {"type": "object"},
                 "annotations": {"readOnlyHint": True}}]

    def close(self):
        pass


def main() -> None:
    sample = Path(__file__).resolve().parents[1] / "config.toml.example"
    example = tomllib.loads(sample.read_text(encoding="utf-8"))["mcp_servers"]["playwright"]
    assert list(PUBLIC_MCP_PRESETS["playwright"]["args"]) == example["args"]
    with tempfile.TemporaryDirectory(prefix="trace-mcp-visibility-") as temporary:
        root = Path(temporary)
        with AgentService(root=root / "runtime", load_external_configuration=False) as service:
            api = WebApi(service)
            token = api.control.login(os.environ.get("TRACE_ADMIN_USERNAME") or "trace", os.environ.get("TRACE_ADMIN_PASSWORD") or "admin@123")
            headers = {"Authorization": "Bearer " + token}
            response = api.dispatch("GET", "/api/mcp", headers=headers)
            assert response.status == 200
            initial = response.payload()
            assert initial["tools"] == 0  # Native adapters are not MCP tools.
            assert len(initial["servers"]) == 1
            available = initial["servers"][0]
            assert available["server_id"] == "playwright" and available["source"] == "catalog"
            assert available["status"]["status"] == "available"
            assert not available["status"]["configured"] and not available["status"]["callable"]

            config = root / "external.toml"
            config.write_text('''
[mcp_servers.external]
command = "fixture"
scope = "run"
preset = "playwright"
[mcp_servers.disabled]
command = "fixture"
args = ["disabled"]
enabled = false
[mcp_servers.broken]
command = "fixture"
args = ["broken"]
[mcp_servers.trace-agent-runtime]
command = "fixture"
''', encoding="utf-8")

            def client(spec, **_):
                if spec.name == "broken":
                    raise RuntimeError("fixture_connection_failed")
                return CatalogClient()

            broker = service.runtime.broker
            with patch.object(broker, "_create_client", side_effect=client) as factory:
                broker.discover_from_configs((config,))
                prior = factory.call_count
                result = api.dispatch("GET", "/api/mcp", headers=headers).payload()
                assert factory.call_count == prior  # Reading never starts/reloads a server.
            servers = {item["server_id"]: item for item in result["servers"]}
            assert set(servers) == {"external", "disabled", "broken"}
            assert servers["external"]["source"] == "config"
            assert servers["external"]["status"]["status"] == "catalogued"
            assert servers["external"]["status"]["callable"]
            assert servers["disabled"]["status"]["status"] == "disabled"
            assert not servers["disabled"]["status"]["callable"]
            assert servers["broken"]["status"]["status"] == "failed"
            assert not servers["broken"]["status"]["callable"]
            assert result["tools"] == 1
            service.control.save_mcp({"server_id": "external", "transport": "http",
                                      "url": "http://fixture.invalid/mcp", "enabled": False})
            updated = api.dispatch("GET", "/api/mcp", headers=headers).payload()["servers"]
            external = [item for item in updated if item["server_id"] == "external"]
            assert len(external) == 1 and external[0]["source"] == "managed"
            assert not external[0]["status"]["callable"]
    print("PASS: public preset, external/managed MCP, disabled/failed states, no builtins or GET startup")


if __name__ == "__main__":
    main()
