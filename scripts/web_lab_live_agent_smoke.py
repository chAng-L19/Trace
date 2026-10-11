"""Exercise both loopback Web labs with the persisted, real model provider.

Reads credentials in memory from a read-only SQLite connection. All runs and
captures go to a separate test state directory; production state is untouched.
"""
from __future__ import annotations

import argparse
from contextlib import closing, contextmanager
import json
import os
from pathlib import Path
import sqlite3
import sys
import threading
from http.server import ThreadingHTTPServer
from urllib.request import Request, urlopen
from uuid import uuid4

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from labs.web_authz_lab import AuthzHandler
from labs.web_ssti_lab import SstiHandler
from redteam_agent import AgentService
from redteam_agent.application.bootstrap import resolve_provider
from redteam_agent.adapters.web import WebApi
from redteam_agent.adapters.web_server import TraceHTTPServer


@contextmanager
def http_client(service):
    server = TraceHTTPServer(("127.0.0.1", 0), WebApi(service))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base, cookie = f"http://127.0.0.1:{server.server_port}", ""

    def request(method, path, body=None, *, expected=200, stream=False):
        nonlocal cookie
        headers = {"Content-Type": "application/json", "Cookie": cookie, "X-Command-ID": uuid4().hex}
        if stream:
            headers["Accept"] = "text/event-stream"
        query = Request(base + path, method=method, headers=headers,
                        data=json.dumps(body).encode() if body is not None else None)
        with urlopen(query, timeout=260) as response:
            assert response.status == expected, (path, response.status)
            if response.headers.get("Set-Cookie"):
                cookie = response.headers["Set-Cookie"].split(";", 1)[0]
            return response.read().decode() if stream else json.load(response)

    try:
        request("POST", "/api/auth/login", {
            "username": os.environ.get("TRACE_ADMIN_USERNAME", "trace"),
            "password": os.environ.get("TRACE_ADMIN_PASSWORD", "admin@123"),
        })
        assert request("GET", "/health/ready")["ready"]
        yield request
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def exercise(provider, root: Path, kind: str, handler, objective: str) -> dict:
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    target = f"http://127.0.0.1:{server.server_port}"
    try:
        with AgentService(root=root / kind, model_port=provider, model_streaming=True,
                          model_max_turns=12, model_max_retries=0,
                          load_external_configuration=False) as service:
            with http_client(service) as request:
                created = request("POST", "/api/runs", {"objective": objective, "targets": [target],
                    "session_id": "live-" + kind, "max_actions": 16, "time_limit_seconds": 240}, expected=201)
                run_id = created["runs"][0]["run"]["run_id"]
                print(json.dumps({"phase": "started", "lab": kind, "run_id": run_id}), flush=True)
                request("POST", f"/api/runs/{run_id}/run", {"run_until_pause": False, "max_cycles": 1})
                projected = request("GET", f"/api/runs/{run_id}")["run"]["run"]
                assert projected["budget"]["input_tokens_used"] > 0
                transcript = request("GET", f"/api/runs/{run_id}/transcript?latest=1")
                assert transcript["messages"]
                trajectory = request("GET", f"/api/runs/{run_id}/events?channel=trajectory")
                assert trajectory["events"]
                stream = request("GET", f"/api/runs/{run_id}/events?channel=trajectory", stream=True)
                streamed = [json.loads(line.removeprefix("data: ")) for line in stream.splitlines()
                            if line.startswith("data: ")]
                assert "event: trace-event" in stream
                assert any(item.get("trajectory", {}).get("kind") == "usage" for item in streamed)
                print(json.dumps({"phase": "http_verified", "lab": kind,
                    "checks": ["login", "ready", "create", "run", "tokens", "transcript", "trajectory_json", "trajectory_sse"]}), flush=True)
            view = service.status(run_id)
            observations = service.journal.model_observations(run_id)
            requests = []
            for row in service.runtime.store.events(run_id, limit=10000):
                if row["event_type"] == "model_request_started":
                    requests.append(row["payload"])
            with service.runtime.store.connection() as connection:
                responses = connection.execute(
                    "SELECT usage_json,status FROM model_responses WHERE run_id=?", (run_id,)
                ).fetchall()
            usages = [json.loads(row["usage_json"]) for row in responses]
            successes = [item for item in observations
                         if item.tool_name == "builtin:http-request" and item.status == "success"]
            outputs = [dict(item.observation.get("tool_result", {}).get("output", {})) for item in successes]
            summary = {"lab": kind, "run_id": run_id, "status": view.run.status,
                "reason": view.terminal.reason, "http_calls": len(successes),
                "model_responses": len(responses), "usages": usages,
                "tool_statuses": [{"name": item.tool_name, "status": item.status} for item in observations],
                "checks": [{"url": item.get("url"), "status": item.get("status_code"),
                            "body": item.get("body", "")[:500]} for item in outputs],
                "artifacts": len(service.artifacts(run_id))}
            print(json.dumps(summary, ensure_ascii=False), flush=True)
            assert len(successes) >= 3, "real model did not execute baseline/proof/control requests"
            assert any(int(item.get("total_tokens") or 0) > 0 for item in usages), "real usage missing"
            assert summary["artifacts"] >= 9, "capture artifacts missing"
            if kind == "authz":
                resources = [json.loads(item["body"]) for item in outputs if "/api/resource/" in str(item.get("url"))]
                assert any(item.get("resource_id") == "1" and item.get("allowed") for item in resources)
                assert any(item.get("resource_id") == "2" and item.get("request_user") == "1" and item.get("allowed") for item in resources)
                assert any(item.get("resource_id") == "2" and item.get("request_user") is None and not item.get("allowed") for item in resources)
            else:
                assert all(any(f"Hello, {expected}" in item.get("body", "") for item in outputs)
                           for expected in ("Trace", "49", "14"))
            return summary
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--provider-state", required=True, type=Path)
    parser.add_argument("--root", required=True, type=Path)
    args = parser.parse_args()
    database = args.provider_state.resolve() / "runtime.sqlite3"
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as connection:
        connection.row_factory = sqlite3.Row
        row = connection.execute("SELECT * FROM trace_providers WHERE enabled=1 AND active=1 LIMIT 1").fetchone()
        if row is None:
            raise RuntimeError("active provider missing")
        active = dict(row)
    provider, _ = resolve_provider(None, (), active=active, environ={},
                                  credential_override=str(active.get("api_key") or ""),
                                  options={"timeout_seconds": 60, "max_output_tokens": 2048})
    assert provider and provider.ready, "provider credentials missing"
    common = ("Use builtin:http-request to execute the checks yourself. Do not call prepare_tools, workers, "
              "or local shell commands. This task requires only the integrated HTTP tool. "
              "After collecting the observations stop with decision waiting_input and summarize the "
              "observed differences. Do not claim a verified report without evidence lifecycle artifacts. ")
    results = [
        exercise(provider, args.root, "authz", AuthzHandler, common +
            "Inspect the target root for routes. Compare /api/resource/1 and /api/resource/2 with X-User-ID: 1; "
            "then request /api/resource/2 anonymously as the negative control. Confirm whether user 1 can read user 2's data."),
        exercise(provider, args.root, "ssti", SstiHandler, common +
            "Compare /greet?name=Trace, a URL-encoded {{7*7}} input, and a URL-encoded {{7+7}} input. "
            "Confirm whether the name parameter evaluates template expressions using the actual response bodies."),
    ]
    print(json.dumps({"ok": True, "provider": provider.model, "labs": results}, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
