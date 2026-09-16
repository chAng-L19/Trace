from __future__ import annotations

import json
from contextlib import contextmanager
from http.client import HTTPConnection
from pathlib import Path
from threading import Thread

import pytest

from redteam_agent import AgentService
from redteam_agent.adapters.web import MAX_JSON_RESPONSE_BYTES, TraceHTTPServer, WebApi, WebResponse
from redteam_agent.core import Event, contract_hash


@contextmanager
def _server(tmp_path: Path):
    service = AgentService(root=tmp_path / "runtime")
    run_id = service.start({"session_id": "protocol", "objective": "Prepare a plan"}).single.run.run_id
    server = TraceHTTPServer(("127.0.0.1", 0), WebApi(service))
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server, service, run_id
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        service.close()


def test_default_event_projection_recursively_omits_raw_and_large_values() -> None:
    payload = {
        "action_id": "probe",
        "diagnostics": {"request": {"raw": "RAW_REQUEST"}, "output": "RAW_OUTPUT", "status": "ok"},
        "notes": "LARGE_CONTENT" * 50_000,
        "many": {f"key_{index}": "value" * 500 for index in range(100)},
    }
    event = Event("run-1", "probe_completed", payload, sequence=3)
    projected = WebApi._event_projection(event)
    serialized = json.dumps(projected)
    assert "RAW_REQUEST" not in serialized
    assert "RAW_OUTPUT" not in serialized
    assert "LARGE_CONTENT" not in serialized
    assert len(serialized.encode()) < 20_000
    assert projected["payload_hash"] == contract_hash(payload)
    assert WebApi._event_projection(event, include_payload=True)["payload"] == payload


def test_json_responses_are_bounded_at_the_protocol_boundary() -> None:
    response = WebResponse.json({"error": "response_too_large", "blob": "x" * (MAX_JSON_RESPONSE_BYTES + 1)})

    assert response.status == 413
    assert response.payload()["error"] == "response_too_large"
    assert response.payload()["max_bytes"] == MAX_JSON_RESPONSE_BYTES


def test_http_events_negotiate_json_and_sse_and_close_completed_batch(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service, run_id):
        connection = HTTPConnection(*server.server_address, timeout=2)
        connection.request("GET", f"/api/runs/{run_id}/events")
        response = connection.getresponse()
        assert response.status == 200
        assert response.getheader("Content-Type").startswith("application/json")
        last = json.loads(response.read())["next_sequence"]
        connection.close()

        service.pause(run_id)
        connection = HTTPConnection(*server.server_address, timeout=2)
        connection.request("GET", f"/api/runs/{run_id}/events", headers={"Accept": "text/event-stream", "Last-Event-ID": str(last)})
        response = connection.getresponse()
        assert response.status == 200
        assert response.getheader("Content-Type").startswith("text/event-stream")
        frames = response.read().decode()
        assert "event: run_paused\n" in frames
        assert "event: operation_started\n" not in frames
        assert response.getheader("Connection") == "close"
        connection.close()


def test_sse_and_evidence_unknown_runs_return_404(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, _, _):
        for resource, headers in (("events", {"Accept": "text/event-stream"}), ("evidence-graph", {})):
            connection = HTTPConnection(*server.server_address, timeout=2)
            connection.request("GET", f"/api/runs/missing/{resource}", headers=headers)
            response = connection.getresponse()
            assert response.status == 404
            assert json.loads(response.read())["ok"] is False
            connection.close()


@pytest.mark.parametrize(
    ("headers", "expected"),
    [
        ({"Content-Type": "text/plain"}, 415),
        ({"Content-Type": "application/json", "Origin": "https://unrelated.example"}, 403),
        ({"Content-Type": "application/json", "Host": "rebound.example"}, 403),
        ({"Content-Type": "application/json", "Content-Length": "-1"}, 400),
        ({"Content-Type": "application/json"}, 200),
    ],
)
def test_http_control_commands_enforce_browser_boundary(tmp_path: Path, headers: dict[str, str], expected: int) -> None:
    with _server(tmp_path) as (server, service, run_id):
        before = service.status(run_id).run.state_version
        connection = HTTPConnection(*server.server_address, timeout=2)
        body = "" if headers.get("Content-Length") == "-1" else "{}"
        connection.request("POST", f"/api/runs/{run_id}/pause", body=body, headers=headers)
        response = connection.getresponse()
        assert response.status == expected
        assert json.loads(response.read())["ok"] is (expected == 200)
        connection.close()
        after = service.status(run_id).run.state_version
        assert (after > before) is (expected == 200)


def test_empty_sse_batch_terminates_with_comment(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service, run_id):
        last = service.events(run_id)[-1].sequence
        connection = HTTPConnection(*server.server_address, timeout=2)
        connection.request("GET", f"/api/runs/{run_id}/events?after_sequence={last}", headers={"Accept": "text/event-stream"})
        response = connection.getresponse()
        assert response.read() == b": keep-alive\n\n"
        connection.close()


def test_unsupported_http_methods_return_structured_errors(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, _, _):
        connection = HTTPConnection(*server.server_address, timeout=2)
        connection.request("PUT", "/api/runs")
        response = connection.getresponse()
        body = json.loads(response.read())

    assert response.status == 405
    assert body["error"] == "method_not_allowed"
    assert response.getheader("X-Content-Type-Options") == "nosniff"


def test_in_process_adapter_rejects_unsupported_methods_consistently(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        api = WebApi(service)
        for method in ("PUT", "PATCH", "OPTIONS", "TRACE"):
            response = api.dispatch(method, "/api/runs")
            assert response.status == 405
            assert response.payload()["error"] == "method_not_allowed"
    finally:
        service.close()
