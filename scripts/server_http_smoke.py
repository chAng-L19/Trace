"""Exercise the installed package over HTTP with isolated temporary state."""
import json
import os
import tempfile
import threading
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen
from unittest.mock import patch

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.adapters.web_server import TraceHTTPServer


def exercise(base):
    cookie = ""

    def request(method, path, body=None, expected=200):
        nonlocal cookie
        headers = {"Content-Type": "application/json"}
        if cookie:
            headers["Cookie"] = cookie
        req = Request(base + path, data=json.dumps(body).encode() if body is not None else None,
                      method=method, headers=headers)
        try:
            response = urlopen(req, timeout=20)
        except HTTPError as error:
            response = error
        with response:
            if response.headers.get("Set-Cookie"):
                cookie = response.headers["Set-Cookie"].split(";", 1)[0]
            payload = json.loads(response.read())
            assert response.status == expected, (method, path, response.status, payload)
            return payload

    request("GET", "/api/runs", expected=401)
    request("POST", "/api/auth/login", {"username": "trace-test", "password": "http-test-pass"})
    started = request("POST", "/api/runs", {"session_id": "server-http", "objective": "Inspect isolated fixture",
                      "targets": ["fixture.invalid"], "max_actions": 0}, expected=201)
    run = started["runs"][0]["run"]
    run_id = run["run_id"]
    assert run["budget"]["action_limit"] == 0
    request("POST", f"/api/runs/{run_id}/budget", {"unlimited_actions": True})
    assert request("GET", f"/api/runs/{run_id}")["run"]["run"]["budget"]["action_limit"] == 0
    timings = {}
    for path in ("/api/runs", "/api/conversations", f"/api/conversations/{run_id}"):
        before = time.perf_counter()
        result = request("GET", path)
        timings[path] = round((time.perf_counter() - before) * 1000, 2)
    assert result["messages"]

    skill = "skill:server-http"
    route = "/api/skills/" + quote(skill, safe="")
    request("POST", "/api/skills", {"resource_id": skill, "content": "isolated instructions"}, expected=201)
    request("POST", route, {"enabled": False})
    assert request("GET", route)["skill"]["enabled"] is False
    request("POST", route, {"enabled": True, "content": "updated instructions"})
    assert request("GET", route)["skill"]["content"] == "updated instructions"
    request("DELETE", route)
    request("GET", route, expected=404)

    request("POST", "/api/mcp", {"server_id": "server-http", "transport": "http",
            "url": "http://127.0.0.1:1/mcp", "enabled": False}, expected=201)
    assert request("POST", "/api/mcp/server-http", {"enabled": True})["server"]["enabled"] is True
    assert request("POST", "/api/mcp/server-http", {"enabled": False})["server"]["enabled"] is False
    request("DELETE", "/api/mcp/server-http")
    assert "server-http" not in {item["server_id"] for item in request("GET", "/api/mcp")["servers"]}
    request("DELETE", f"/api/conversations/{run_id}")
    assert not request("GET", "/api/conversations")["conversations"]
    assert not request("GET", "/api/runs")["runs"]
    print(json.dumps({"ok": True, "checks": ["installed_http", "auth", "unlimited", "conversation_delete",
          "skill_crud", "mcp_crud"], "fixture_request_ms": timings}))


def main():
    with tempfile.TemporaryDirectory(prefix="trace-server-http-") as temporary:
        root = Path(temporary)
        with patch.dict(os.environ, {"HOME": str(root), "USERPROFILE": str(root),
                "REDTEAM_AGENT_HOME": str(root), "REDTEAM_AGENT_CONFIG": str(root / "absent.toml"),
                "TRACE_ADMIN_USERNAME": "trace-test", "TRACE_ADMIN_PASSWORD": "http-test-pass"}):
            service = AgentService(root=root / "state")
            server = TraceHTTPServer(("127.0.0.1", 0), WebApi(service))
            worker = threading.Thread(target=server.serve_forever)
            worker.start()
            try:
                exercise(f"http://127.0.0.1:{server.server_port}")
            finally:
                server.shutdown()
                worker.join()
                server.server_close()
                service.close()


if __name__ == "__main__":
    main()
