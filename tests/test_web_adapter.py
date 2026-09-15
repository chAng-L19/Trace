from __future__ import annotations

from pathlib import Path

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi


def _start_body(session_id: str, target: Path) -> dict[str, object]:
    return {
        "session_id": session_id,
        "objective": f"Inspect {target} and produce a report",
        "targets": [str(target)],
        "max_actions": 8,
    }


def test_web_start_and_run_projection_are_versioned(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("web-fixture", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)

    started = api.dispatch("POST", "/api/runs", body=_start_body("web-start", target))
    assert started.status == 201
    started_payload = started.payload()
    assert started_payload["schema_version"] == 1
    run_id = started_payload["runs"][0]["run"]["run_id"]

    status = api.dispatch("GET", f"/api/runs/{run_id}")
    assert status.status == 200
    assert status.payload()["run"]["run"]["run_id"] == run_id

    events = api.dispatch("GET", f"/api/runs/{run_id}/events?after_sequence=0")
    assert events.status == 200
    assert events.payload()["events"]
    assert events.payload()["next_sequence"] >= 1
    assert "state_snapshot" not in events.payload()["events"][0]["payload"]
    service.close()


def test_web_tools_and_transparency_routes_project_agent_service(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("web-fixture", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    started = api.dispatch("POST", "/api/runs", body=_start_body("web-inspection", target)).payload()
    run_id = started["runs"][0]["run"]["run_id"]
    tools = api.dispatch("GET", f"/api/runs/{run_id}/tools")
    transparency = api.dispatch("GET", f"/api/runs/{run_id}/transparency")
    assert tools.status == 200 and tools.payload()["run_id"] == run_id
    assert transparency.status == 200 and transparency.payload()["run_id"] == run_id
    service.close()


def test_web_run_projection_omits_evidence_payload_by_default() -> None:
    projected = WebApi._run_projection(
        {
            "run": {"run_id": "run-1"},
            "evidence": [
                {"evidence_id": "evidence-1", "payload": {"raw": "secret"}},
                {"evidence_id": "evidence-2", "content_hash": "a" * 64},
            ],
        }
    )

    assert projected["evidence"] == [
        {"evidence_id": "evidence-1"},
        {"evidence_id": "evidence-2", "content_hash": "a" * 64},
    ]


def test_web_command_receipt_replays_across_api_instances(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("web-idempotency", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    body = {**_start_body("web-command", target), "command_id": "start-command-1"}

    first = api.dispatch("POST", "/api/runs", body=body)
    second = WebApi(service).dispatch("POST", "/api/runs", body=body)

    assert first.status == second.status == 201
    assert first.body == second.body
    run_id = first.payload()["runs"][0]["run"]["run_id"]

    paused = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/pause",
        body={"command_id": "pause-command-1", "reason": "operator_pause"},
    )
    replayed = WebApi(service).dispatch(
        "POST",
        f"/api/runs/{run_id}/pause",
        body={"command_id": "pause-command-1", "reason": "operator_pause"},
    )
    assert paused.status == replayed.status == 200
    assert paused.body == replayed.body
    assert paused.payload()["run"]["run"]["status"] == "paused_budget"

    resumed = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/resume",
        body={"execute": False},
    )
    assert resumed.status == 200
    assert resumed.payload()["run"]["run"]["status"] == "running"

    conflict = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/pause",
        body={"command_id": "pause-command-1", "reason": "different"},
    )
    assert conflict.status == 409
    service.close()


def test_web_pause_resume_and_sse_are_delta_oriented(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("web-sse", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    started = api.dispatch("POST", "/api/runs", body=_start_body("web-sse", target)).payload()
    run_id = started["runs"][0]["run"]["run_id"]

    before = api.dispatch("GET", f"/api/runs/{run_id}/events").payload()
    paused = api.dispatch("POST", f"/api/runs/{run_id}/pause", body={})
    assert paused.payload()["run"]["run"]["status"] == "paused_budget"
    resumed = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/resume",
        body={"execute": False},
    )
    assert resumed.payload()["run"]["run"]["status"] == "running"

    after = api.sse_events(run_id, after_sequence=before["next_sequence"] or 0)
    assert [item["sequence"] for item in after] == sorted(item["sequence"] for item in after)
    assert {item["event_type"] for item in after} >= {"run_paused", "run_resumed"}
    assert all("state_snapshot" not in item["payload"] for item in after)
    service.close()


def test_web_graph_and_artifact_routes_keep_payloads_bounded(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("web-graph", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    started = api.dispatch("POST", "/api/runs", body=_start_body("web-graph", target)).payload()
    run_id = started["runs"][0]["run"]["run_id"]

    evidence = api.dispatch("GET", f"/api/runs/{run_id}/evidence-graph")
    assert evidence.status == 200
    assert evidence.payload()["nodes"] == []
    artifacts = api.dispatch("GET", f"/api/runs/{run_id}/artifacts")
    assert artifacts.status == 200
    assert isinstance(artifacts.payload()["artifacts"], list)
    service.close()


def test_web_budget_command_only_adjusts_budget(tmp_path: Path) -> None:
    """Budget updates must not execute model/workflow actions implicitly."""

    target = tmp_path / "target.txt"
    target.write_text("web-budget", encoding="utf-8")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    started = api.dispatch("POST", "/api/runs", body=_start_body("web-budget", target)).payload()
    run_id = started["runs"][0]["run"]["run_id"]

    before = api.dispatch("GET", f"/api/runs/{run_id}").payload()["run"]["run"]
    response = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/budget",
        body={"actions": 3, "tokens": 1000},
    )
    assert response.status == 200
    after = response.payload()["run"]["run"]
    assert after["budget"]["action_limit"] == before["budget"]["action_limit"] + 3
    assert after["budget"]["actions_used"] == before["budget"]["actions_used"]
    service.close()


def test_web_budget_command_rejects_cancelled_run_without_mutation(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    run_id = service.start(
        {"session_id": "web-cancelled-budget", "objective": "Prepare a plan"}
    ).single.run.run_id
    cancelled = service.cancel(run_id)

    response = api.dispatch(
        "POST",
        f"/api/runs/{run_id}/budget",
        body={"actions": 3},
    )

    assert response.status == 400
    assert "operation_terminal:cancelled" in response.payload()["error"]
    after = service.status(run_id)
    assert after.run.budget == cancelled.run.budget
    assert after.run.state_version == cancelled.run.state_version
    service.close()


def test_web_pending_receipt_is_single_flight_for_same_owner(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service, command_ttl_seconds=30)

    first = service.runtime.store.claim_web_command(
        "pending-command",
        "request-hash",
        owner=api.owner,
        ttl_seconds=30,
    )
    second = service.runtime.store.claim_web_command(
        "pending-command",
        "request-hash",
        owner=api.owner,
        ttl_seconds=30,
    )

    assert first["claimed"] is True
    assert second["claimed"] is False
    assert second["status"] == "pending"
    service.close()
