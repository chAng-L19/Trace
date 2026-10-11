"""Cairn-style workspace workflow over Trace's existing run ledger."""
from __future__ import annotations

import json
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.application.workspaces import WorkspaceManager


def multi_target_regression(service, call):
    manager, store = service.project_workspaces, service.runtime.store
    targets = ["https://one.invalid", "https://two.invalid"]
    created = call("POST", "/api/workspaces", {
        "workspace_id": "multi", "title": "Batch review", "goal": "Inspect targets",
        "targets": targets, "hints": ["Shared batch hint"],
    }).payload()["workspace"]
    assert len(created["runs"]) == len(created["intents"]) == 2
    batch_id = created["started"]["batch_id"]
    states = store.operations_for_batch(batch_id)
    assert len(states) == 2
    assert {state.session_id for state in states} == {f"{batch_id}:1", f"{batch_id}:2"}
    assert all(state.goal.starting_context["parent_session_id"] == "multi" for state in states)
    assert all(state.goal.starting_context["workspace_id"] == "multi" for state in states)
    assert {tuple(state.goal.targets) for state in states} == {(target,) for target in targets}
    assert [item["workspace_id"] for item in manager.list()["workspaces"]] == ["multi"]

    extra = {"description": "Inspect more targets", "targets": ["https://three.invalid", "https://four.invalid"]}
    added = call("POST", "/api/workspaces/multi/intents", extra).payload()
    assert len(added["intent"]["run_ids"]) == 2 and added["workspace"]["run_count"] == 4
    assert added["intent"]["run_id"] == added["intent"]["run_ids"][0]
    replay = manager.add_intent("multi", extra)
    assert replay["intent"]["run_ids"] == added["intent"]["run_ids"]
    assert replay["workspace"]["intent_count"] == 4
    inherited = manager.add_intent("multi", {"description": "Review all workspace targets"})
    assert len(inherited["intent"]["run_ids"]) == 4
    assert {target for rid in inherited["intent"]["run_ids"] for target in service.status(rid).goal.targets} == set(targets + extra["targets"])
    ids = {item["run_id"] for item in inherited["runs"]}
    assert len(ids) == inherited["workspace"]["intent_count"] == 8
    assert all(not item.get("legacy") for item in inherited["intents"])

    stopped = manager.set_status("multi", "stopped")
    assert all(item["status"] == "paused_budget" for item in stopped["runs"])
    resumed = manager.set_status("multi", "active")
    assert all(item["status"] == "running" for item in resumed["runs"])
    # The shared fact board and legacy run: claim path must also resolve children.
    first = created["intents"][0]
    manager.claim_intent("multi", "run:" + first["run_id"], "operator")
    service.cancel(first["run_id"])
    manager.conclude_intent("multi", first["id"], "operator", "Batch conclusion")
    for run_id in ids:
        board = service.context_selector.prepare_model_context(service.status(run_id)).protected_context["workspace_context"]
        assert board["workspace_id"] == "multi" and board["hints"][0]["content"] == "Shared batch hint"
        assert len(board["intents"]) == 8
        assert any(fact["value"]["description"] == "Batch conclusion" for fact in board["facts"])
    with AgentService(root=service.runtime.root, load_external_configuration=False) as reopened:
        assert {item["run_id"] for item in reopened.project_workspaces.detail("multi")["runs"]} == ids
        assert reopened.project_workspaces.model_context(states[0].session_id)["workspace_id"] == "multi"
    exported = call("GET", "/api/workspaces/multi/export?format=json").payload()
    assert {item["run"]["run_id"] for item in exported["runs"]} == ids
    legacy = service.start({"session_id": "legacy-outside", "objective": "Separate review", "targets": ["local"]}).single
    call("DELETE", "/api/workspaces/multi")
    assert [item["workspace_id"] for item in manager.list()["workspaces"]] == ["legacy-outside"]
    assert {item.run_id for item in store.operations(limit=100)} == {legacy.run.run_id}
    manager.delete("legacy-outside")


def creation_rollback_regression(service):
    manager, store = service.project_workspaces, service.runtime.store
    targets = ["https://one.invalid", "https://two.invalid"]
    original_start = type(service.runtime).start_or_resume
    original_intent = WorkspaceManager._ensure_intent

    def snapshot():
        with store.connection() as connection:
            return tuple(connection.iterdump())

    def fail_second(original):
        calls = 0

        def invoke(instance, *args, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise ValueError("injected_second_write_failure")
            return original(instance, *args, **kwargs)
        return invoke

    def unchanged_after_failure(action, message):
        before = snapshot()
        try:
            action()
        except ValueError as error:
            assert message in str(error), error
        else:
            raise AssertionError("expected creation failure")
        assert snapshot() == before, "failed creation changed durable records"

    # Failure inside start_batch and after start() returns must roll back all tables.
    for owner, method, original in ((type(service.runtime), "start_or_resume", original_start),
                                    (WorkspaceManager, "_ensure_intent", original_intent)):
        with patch.object(owner, method, fail_second(original)):
            unchanged_after_failure(lambda: manager.create({
                "workspace_id": "failed-create", "title": "Rollback", "targets": targets,
            }), "injected_second_write_failure")
    unchanged_after_failure(lambda: manager.create({
        "workspace_id": "failed-hint", "title": "Rollback hints", "targets": targets,
        "hints": ["This hint must also roll back", ""],
    }), "workspace_hint_required")

    manager.create({"workspace_id": "existing", "title": "Keep existing", "targets": ["local"]})
    extra = {"description": "Existing batch", "targets": targets}
    manager.add_intent("existing", extra)
    manager.set_status("existing", "stopped")
    # Replaying an existing first child may extend its budget/resume it: roll that back too.
    with patch.object(type(service.runtime), "start_or_resume", fail_second(original_start)):
        unchanged_after_failure(lambda: manager.add_intent("existing", {**extra, "max_actions": 20}),
                                "injected_second_write_failure")
    with patch.object(WorkspaceManager, "_ensure_intent", fail_second(original_intent)):
        unchanged_after_failure(lambda: manager.add_intent("existing", {
            "description": "New batch must roll back", "targets": targets,
        }), "injected_second_write_failure")
    # Materializing legacy workspace metadata belongs to the same transaction.
    service.start({"session_id": "legacy", "objective": "Legacy", "targets": ["local"]})
    with patch.object(type(service.runtime), "start_or_resume", fail_second(original_start)):
        unchanged_after_failure(lambda: manager.add_intent("legacy", {
            "description": "Failed legacy batch", "targets": targets,
        }), "injected_second_write_failure")
    assert manager._meta("legacy") is None
    # A successful retry after rollback must create a complete batch.
    retry = manager.create({"workspace_id": "failed-create", "title": "Rollback", "targets": targets})
    assert len(retry["runs"]) == len(retry["intents"]) == 2
    for workspace_id in ("existing", "legacy", "failed-create"):
        manager.delete(workspace_id)
    with store.connection() as connection:
        assert connection.execute("PRAGMA foreign_key_check").fetchall() == []
        for table in ("operations", "workspace_meta", "workspace_intents", "workspace_hints", "conversation_messages", "session_journal_entries"):
            assert connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0] == 0, table


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="trace-workspace-smoke-") as directory:
        with AgentService(root=Path(directory), load_external_configuration=False) as service:
            api = WebApi(service)
            headers = {"authorization": "Bearer " + api.control.login("trace", "admin@123")}

            def call(method, path, body=None):
                response = api.dispatch(method, path, body=body or {}, headers=headers)
                assert response.status < 300, (method, path, response.status, response.body)
                return response

            created = call("POST", "/api/workspaces", {
                "title": "本地授权审计", "origin": "http://127.0.0.1:8781",
                "goal": "验证资源授权边界", "targets": ["http://127.0.0.1:8781"],
                "max_actions": 0,
                "hints": [{"content": "保留匿名拒绝控制", "creator": "operator"}],
            }).payload()["workspace"]
            workspace_id = created["workspace"]["workspace_id"]
            assert created["workspace"]["run_count"] == 1
            assert created["workspace"]["hint_count"] == 1
            assert call("GET", "/api/workspaces?search=missing").payload()["workspaces"] == []
            assert call("GET", "/api/workspaces?status=active").payload()["workspaces"][0]["workspace_id"] == workspace_id

            detail = call("GET", f"/api/workspaces/{workspace_id}").payload()
            assert detail["workspace"]["goal"] == "验证资源授权边界"
            assert detail["hints"][0]["content"] == "保留匿名拒绝控制"
            assert detail["intents"][0]["status"] == "open"

            intent = call("POST", f"/api/workspaces/{workspace_id}/intents", {
                "description": "比较同一用户读取不同资源",
                "max_actions": 0,
            }).payload()
            assert intent["intent"]["run_id"]
            assert service.status(intent["intent"]["run_id"]).goal.targets == ("http://127.0.0.1:8781",)
            assert call("GET", "/api/workspaces").payload()["workspaces"][0]["run_count"] == 2
            for item in service.project_workspaces.detail(workspace_id)["runs"]:
                selection = service.context_selector.prepare_model_context(service.status(item["run_id"]))
                board = selection.protected_context["workspace_context"]
                assert board["hints"][0]["content"] == "保留匿名拒绝控制"
                assert len(board["intents"]) == 2
            assert not service.project_workspaces.model_context("unrelated")

            stopped = call("POST", f"/api/workspaces/{workspace_id}", {"action": "stopped"}).payload()["workspace"]
            assert stopped["workspace"]["status"] == "stopped"
            assert all(item["status"] == "paused_budget" for item in stopped["runs"])
            resumed = call("POST", f"/api/workspaces/{workspace_id}", {"action": "active"}).payload()["workspace"]
            assert resumed["workspace"]["status"] == "active"
            assert all(item["status"] != "completed" for item in resumed["runs"])

            renamed = call("POST", f"/api/workspaces/{workspace_id}", {"action": "rename", "title": "授权边界复核"}).payload()["workspace"]
            assert renamed["workspace"]["title"] == "授权边界复核"
            markdown = api.dispatch("GET", f"/api/workspaces/{workspace_id}/export?format=markdown", headers=headers)
            assert (
                markdown.status == 200
                and "授权边界复核".encode("utf-8") in markdown.body
                and "匿名拒绝控制".encode("utf-8") in markdown.body
                and "事实与结论".encode("utf-8") in markdown.body
                and "比较同一用户读取不同资源".encode("utf-8") in markdown.body
            )
            exported = call("GET", f"/api/workspaces/{workspace_id}/export?format=json").payload()
            assert exported["workspace"]["title"] == "授权边界复核"
            assert len(exported["runs"]) == 2

            call("POST", f"/api/workspaces/{workspace_id}", {"action": "stopped"})
            call("DELETE", f"/api/workspaces/{workspace_id}")
            assert call("GET", "/api/workspaces").payload()["workspaces"] == []
            assert service.runtime.store.operations(limit=10) == ()
            multi_target_regression(service, call)
            creation_rollback_regression(service)
    print(json.dumps({"ok": True, "checks": ["create", "search-status-filter", "hints", "intents-as-runs", "stop-resume", "rename", "markdown-json-export", "cascade-delete",
        "multi-target-create-add", "batch-session-identity", "batch-replay", "inherited-targets", "shared-context-facts", "batch-stop-resume-export-delete", "batch-restart",
        "partial-start-rollback", "intent-write-rollback", "hint-write-rollback", "existing-budget-rollback", "legacy-materialization-rollback", "retry-after-rollback"]}))


if __name__ == "__main__":
    main()
