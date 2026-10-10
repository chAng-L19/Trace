"""Offline CRUD, branch reads, cancellation and shared-data regressions."""
from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from urllib.parse import quote

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService, StartRequest
from redteam_agent.adapters.web import WebApi
from redteam_agent.application.bootstrap import reload_mcp
from redteam_agent.runtime.durable_store import ImmutableRecordError


def start(service, session_id):
    return service.start(StartRequest(session_id=session_id, objective="Inspect local fixture",
        targets=("fixture.invalid",))).single.run.run_id


def client(*args, **kwargs):
    return SimpleNamespace(list_tools=lambda: [
        {"name": "read", "inputSchema": {"type": "object"}},
        {"name": "write", "inputSchema": {"type": "object"}},
        {"name": "browser_snapshot", "inputSchema": {"type": "object"}},
    ], close=lambda: None)


def exercise(service):
    api = WebApi(service)
    headers = {"authorization": "Bearer " + api.control.login("trace", "crud-pass")}

    def request(method, path, body=None, status=200, command_id=""):
        response = api.dispatch(method, path, body=body,
            headers={**headers, **({"x-command-id": command_id} if command_id else {})})
        assert response.status == status, (method, path, response.status, response.payload())
        return response.payload()

    run_id = start(service, "crud")
    builtin = request("GET", "/api/skills")["skills"][0]["resource_id"]
    route = "/api/skills/" + quote(builtin, safe="")
    original = request("GET", route)["skill"]
    request("POST", route, {"enabled": True, "config": {"requested": [builtin]}})
    assert builtin in service.resource_selection(run_id).resource_ids
    request("DELETE", route)
    assert builtin not in {item["resource_id"] for item in request("GET", "/api/skills")["skills"]}
    assert builtin not in {item.resource_id for item in service.resource_index(run_id).resources}
    assert builtin not in service.resource_selection(run_id, extra_requested=(builtin,)).resource_ids
    request("GET", route, status=404)
    request("POST", "/api/skills", {"resource_id": builtin, "content": original["content"],
        "config": {"requested": [builtin]}}, status=201)
    assert builtin in service.resource_selection(run_id).resource_ids

    skill_id = "skill:custom/demo"
    custom = "/api/skills/" + quote(skill_id, safe="")
    request("POST", "/api/skills", {"resource_id": skill_id, "content": "custom instructions",
        "config": {"requested": [skill_id]}}, status=201)
    request("POST", custom, {"enabled": False})
    assert request("GET", custom)["skill"]["config"] == {"requested": [skill_id]}
    request("POST", custom, {"content": "updated instructions"})
    assert request("GET", custom)["skill"]["enabled"] is False
    request("POST", custom, {"enabled": True})
    assert request("GET", custom)["skill"]["content"] == "updated instructions"
    selected = service.resource_selection(run_id)
    assert next(item.content for item in selected.selected if item.resource_id == skill_id) == "updated instructions"
    request("DELETE", custom)

    config = service.runtime.root / "external-mcp.toml"
    config.write_text('''[mcp_servers."config.server"]
transport = "http"
url = "http://fixture.invalid/mcp"
include_tools = ["read"]
exclude_tools = ["write"]
read_only = true
startup_timeout_seconds = 73
tool_timeout_seconds = 91
headers = {Authorization = "fixture-secret"}
''', encoding="utf-8")
    service.config_paths = [config]
    broker = service.runtime.broker
    with patch.object(broker, "_create_client", side_effect=client):
        reload_mcp(service)
        assert any(item["source"] == "config" for item in request("GET", "/api/mcp")["servers"])
        listed = next(item for item in request("GET", "/api/mcp")["servers"] if item["server_id"] == "config.server")
        assert listed["url"] == "http://fixture.invalid/mcp" and "headers" not in listed
        request("POST", "/api/mcp", {"server_id": "config.server", "transport": "http",
            "url": "http://fixture.invalid/edited", "enabled": True}, status=201)
        assert broker._server_configs["config.server"].url == "http://fixture.invalid/edited"
        assert broker._server_configs["config.server"].render(environment=api.control.mcp_secret_bindings()["config.server"]).headers["Authorization"] == "fixture-secret"
        request("POST", "/api/mcp/config.server", {"enabled": False})
        assert broker.server_statuses()["config.server"]["status"] == "disabled"
        request("POST", "/api/mcp/config.server", {"enabled": True})
        spec = broker._server_configs["config.server"]
        assert spec.include_tools == ("read",) and spec.exclude_tools == ("write",)
        assert spec.read_only and spec.startup_timeout_seconds == 73 and spec.tool_timeout_seconds == 91
        assert spec.render(environment=api.control.mcp_secret_bindings()["config.server"]).headers["Authorization"] == "fixture-secret"
        assert "fixture-secret" not in json.dumps(request("GET", "/api/mcp"))
        request("DELETE", "/api/mcp/config.server")
        reload_mcp(service)
        assert "config.server" not in {item["server_id"] for item in request("GET", "/api/mcp")["servers"]}
        assert not any(item.server == "config.server" for item in broker.descriptors())
        assert config.read_text(encoding="utf-8").find("fixture-secret") >= 0

        request("POST", "/api/mcp", {"server_id": "managed", "transport": "http",
            "url": "http://fixture.invalid/managed", "headers": {"Authorization": "managed-secret"}}, status=201)
        request("POST", "/api/mcp/managed", {"enabled": False})
        request("POST", "/api/mcp/managed", {"enabled": True})
        request("DELETE", "/api/mcp/managed")
        assert "managed" not in api.control.mcp_secret_bindings()
        request("DELETE", "/api/mcp/playwright")
        assert "playwright" not in {item["server_id"] for item in request("GET", "/api/mcp")["servers"]}
        request("POST", "/api/mcp/playwright", {"enabled": True})
        assert "playwright" in broker._server_configs
        request("DELETE", "/api/mcp/playwright")

    first = service.transcript(run_id)[0]
    entry = next(item for item in service.session_entries(run_id) if item.raw_id == first.message_id)
    for number in range(6):
        service.conversation.append(run_id=run_id, role="assistant", content={"text": str(number)},
            protected=False, source_type="fixture", source_id=str(number))
    main_messages = service.conversation.messages(run_id)
    assert service.transcript(run_id) == main_messages
    service.fork_session(run_id, entry.entry_id, "fork")
    branch_message = service.conversation.append(run_id=run_id, role="assistant", content={"text": "fork"},
        protected=False, source_type="fixture", source_id="fork")
    expected = service.conversation.messages(run_id)
    assert service.transcript(run_id) == expected == (first, branch_message)
    with patch.object(service, "status", side_effect=AssertionError("full status on list")), \
         patch.object(service, "export_session", side_effect=AssertionError("full journal on list")), \
         patch.object(service.runtime.store, "conversation_messages", side_effect=AssertionError("full transcript read")):
        listed = request("GET", "/api/conversations")["conversations"]
        summary = next(item for item in listed if item["run"]["run"]["run_id"] == run_id)
        assert summary["message_count"] == 2
        assert summary["session"]["active_branch_id"] == "fork"
        assert request("GET", "/api/runs")["runs"]
        assert len(request("GET", f"/api/runs/{run_id}/transcript?limit=1")["messages"]) == 1
        assert request("GET", f"/api/runs/{run_id}/transcript?limit=1")["truncated"]
        assert not request("GET", f"/api/runs/{run_id}/transcript?limit=1&offset=1")["truncated"]
    service.checkout_session(run_id, "main")
    assert service.transcript(run_id) == main_messages

    with service.runtime.store.transaction(immediate=True) as db:
        original_json = db.execute("SELECT message_json FROM conversation_messages WHERE message_id=?", (first.message_id,)).fetchone()[0]
        changed = {**json.loads(original_json), "content": {"text": "tampered"}}
        db.execute("UPDATE conversation_messages SET message_json=? WHERE message_id=?", (json.dumps(changed), first.message_id))
    try:
        service.transcript(run_id)
        raise AssertionError("message integrity check bypassed")
    except ImmutableRecordError:
        pass
    with service.runtime.store.transaction(immediate=True) as db:
        db.execute("UPDATE conversation_messages SET message_json=? WHERE message_id=?", (original_json, first.message_id))
        leaf = db.execute("SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id='main'", (run_id,)).fetchone()[0]
        parent = db.execute("SELECT parent_entry_id FROM session_journal_entries WHERE entry_id=?", (entry.entry_id,)).fetchone()[0]
        db.execute("UPDATE session_journal_entries SET parent_entry_id=? WHERE entry_id=?", (leaf, entry.entry_id))
    try:
        service.transcript(run_id)
        raise AssertionError("journal integrity check bypassed")
    except ImmutableRecordError:
        pass
    with service.runtime.store.transaction(immediate=True) as db:
        db.execute("UPDATE session_journal_entries SET parent_entry_id=? WHERE entry_id=?", (parent, entry.entry_id))

    other = start(service, "retained")
    retained = service.runtime.artifacts.put_bytes(b"shared-blob", run_id=other)
    service.runtime.artifacts.put_bytes(b"shared-blob", run_id=run_id)
    service.runtime.store.save_session_binding("crud", {"run_id": run_id})
    service.runtime.store.save_session_binding("retained", {"run_id": other})
    lease = service.runtime.store.acquire_lease(run_id, "__model_loop__", "external-process", ttl_seconds=60)
    request("DELETE", f"/api/conversations/{run_id}", status=409)
    assert service.runtime.store.load_operation(run_id).status == "cancelling"
    service.runtime.store.release_lease(lease)
    command = "delete-crud"
    request("DELETE", f"/api/conversations/{run_id}", command_id=command)
    request("DELETE", f"/api/conversations/{run_id}", command_id=command)
    request("GET", f"/api/conversations/{run_id}", status=404)
    request("GET", f"/api/runs/{run_id}", status=404)
    assert service.runtime.store.load_operation(other) is not None
    assert service.runtime.artifacts.read(retained.artifact_id, run_id=other) == b"shared-blob"
    with service.runtime.store.connection() as db:
        assert not db.execute("PRAGMA foreign_key_check").fetchall()
        for table in ("operation_events", "conversation_messages", "session_journal_entries", "session_journal_heads",
                      "session_journal_state", "artifact_refs", "worker_tasks", "session_bindings"):
            assert db.execute(f"SELECT COUNT(*) FROM {table} WHERE run_id=?", (run_id,)).fetchone()[0] == 0
        assert db.execute("SELECT COUNT(*) FROM artifact_blobs").fetchone()[0] > 0
    assert service.runtime.store.session_binding("retained") is not None
    return builtin, skill_id, config


def main():
    with tempfile.TemporaryDirectory(prefix="trace-crud-") as directory, patch.dict(os.environ, {
        "TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "crud-pass",
    }):
        root = Path(directory)
        with AgentService(root=root, load_external_configuration=False) as service:
            builtin, skill_id, config = exercise(service)
        with AgentService(root=root, load_external_configuration=False) as service:
            assert service.control.skill(builtin)["content"]
            assert skill_id not in {item["resource_id"] for item in service.control.skills()}
            service.config_paths = [config]
            with patch.object(service.runtime.broker, "_create_client", side_effect=client):
                reload_mcp(service)
                assert service.runtime.broker.server_statuses()["config.server"]["status"] == "disabled"
                assert not service.control.mcp_servers()
            assert len(service.list_runs(summary_only=True)) == 1
    print(json.dumps({"ok": True, "checks": ["skill-content-crud", "builtin-delete-restore",
        "skill-injection", "mcp-all-sources", "mcp-secrets-and-options", "restart-persistence",
        "branch-transcript-pagination", "message-and-journal-integrity", "summary-reads", "cancel-before-delete", "delete-idempotency",
        "cascade-and-shared-artifacts"]}))


if __name__ == "__main__":
    main()
