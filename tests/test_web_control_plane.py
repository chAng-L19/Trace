from __future__ import annotations

import json
import os
from pathlib import Path

from redteam_agent.adapters.web import WebApi
from redteam_agent.application import AgentService


def _payload(response):
    assert response.status < 400, response.payload()
    return response.payload()


def test_provider_control_plane_is_redacted_and_switches_model(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    saved = _payload(api.dispatch("POST", "/api/providers", body={
        "provider_id": "fixture",
        "name": "Fixture",
        "base_url": "http://127.0.0.1:9/v1",
        "model": "trace-model",
        "api_key": "secret-not-returned",
    }))
    assert saved["provider"]["api_key_set"] is True
    assert "secret-not-returned" not in str(saved)
    activated = _payload(api.dispatch("POST", "/api/providers/active", body={"provider_id": "fixture"}))
    assert activated["provider"]["active"] is True
    assert service.model_loop is not None
    service.close()


def test_skill_and_mcp_settings_round_trip(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    skill = root / "skills" / "web"
    skill.mkdir(parents=True)
    (skill / "SKILL.md").write_text("# web\n", encoding="utf-8")
    service = AgentService(root=root)
    api = WebApi(service)
    skills = _payload(api.dispatch("GET", "/api/skills"))["skills"]
    skill_id = next(item["resource_id"] for item in skills if item["source"].endswith("SKILL.md"))
    assert _payload(api.dispatch("POST", f"/api/skills/{skill_id}", body={"enabled": False}))["skill"]["enabled"] is False
    server = _payload(api.dispatch("POST", "/api/mcp", body={"server_id": "fixture", "transport": "stdio", "command": "python", "args": ["-c", "pass"], "headers": {"Authorization": "Bearer fixture-secret"}}))
    assert server["server"]["server_id"] == "fixture"
    assert (root / "managed-mcp.toml").is_file()
    assert "fixture-secret" not in (root / "runtime.sqlite3").read_bytes().decode("utf-8", "ignore")
    assert "fixture-secret" not in (root / "managed-mcp.toml").read_text(encoding="utf-8")
    assert _payload(api.dispatch("GET", "/api/mcp"))["servers"][0]["server_id"] == "fixture"
    service.close()


def test_auth_cookie_gates_control_routes(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("TRACE_ADMIN_PASSWORD", "fixture-password")
    monkeypatch.setenv("TRACE_AUTH_REQUIRED", "1")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    assert api.dispatch("GET", "/api/runs").status == 401
    assert _payload(api.dispatch("GET", "/api/auth/status"))["required"] is True
    login = api.dispatch("POST", "/api/auth/login", body={"password": "fixture-password"})
    assert login.status == 200
    token = login.headers["Set-Cookie"].split("=", 1)[1].split(";", 1)[0]
    assert api.dispatch("GET", "/api/runs", headers={"cookie": f"trace_session={token}"}).status == 200
    service.close()


def test_force_auth_cannot_be_disabled_by_trace_auth_required(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("TRACE_ADMIN_PASSWORD", "fixture-password")
    monkeypatch.setenv("TRACE_AUTH_REQUIRED", "0")
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    api.force_auth = True
    assert api.dispatch("GET", "/api/runs").status == 401
    login = api.dispatch("POST", "/api/auth/login", body={"password": "fixture-password"})
    assert login.status == 200
    assert "Secure" not in login.headers["Set-Cookie"]
    service.close()


def test_mcp_secrets_are_scoped_to_server_and_not_process_environment(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    first = _payload(api.dispatch("POST", "/api/mcp", body={
        "server_id": "first", "transport": "http", "url": "http://127.0.0.1:1", "headers": {"Authorization": "Bearer first"},
    }))
    second = _payload(api.dispatch("POST", "/api/mcp", body={
        "server_id": "second", "transport": "http", "url": "http://127.0.0.1:2", "headers": {"Authorization": "Bearer second"},
    }))
    assert first["server"]["server_id"] == "first"
    assert second["server"]["server_id"] == "second"
    bindings = api.control.mcp_secret_bindings()
    assert set(bindings) == {"first", "second"}
    assert set(bindings["first"].values()) == {"Bearer first"}
    assert set(bindings["second"].values()) == {"Bearer second"}
    for name in (*bindings["first"], *bindings["second"]):
        assert name not in os.environ
    service.close()


def test_existing_mcp_plaintext_is_migrated_to_process_binding(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    service = AgentService(root=root)
    WebApi(service)
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "INSERT INTO trace_mcp_servers(server_id,spec_json,enabled,updated_at) VALUES(?,?,1,?)",
            ("legacy", json.dumps({"transport": "http", "url": "http://127.0.0.1:1", "headers": {"Authorization": "Bearer legacy"}}), "now"),
        )
    api = WebApi(service)
    with service.runtime.store.connection() as connection:
        stored = str(connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id='legacy'").fetchone()[0])
    assert "Bearer legacy" not in stored
    assert set(api.control.mcp_secret_bindings()["legacy"].values()) == {"Bearer legacy"}
    service.close()
