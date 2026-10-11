"""Real SQLite/API lease races and conclusion provenance; temporary state only."""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
import json
import sqlite3
from pathlib import Path
import sys
import tempfile
from threading import Barrier
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi


def race(*actions):
    barrier = Barrier(len(actions))

    def run(action):
        barrier.wait(timeout=10)
        try:
            action()
            return "ok"
        except ValueError as error:
            return str(error)

    with ThreadPoolExecutor(max_workers=len(actions)) as pool:
        return list(pool.map(run, actions))


def rejects(action, message):
    try:
        action()
    except (ValueError, KeyError) as error:
        assert message in str(error), error
    else:
        raise AssertionError(f"expected {message}")


def main():
    with tempfile.TemporaryDirectory(prefix="trace-workspace-lifecycle-") as directory:
        with AgentService(root=Path(directory), load_external_configuration=False) as service:
            manager = service.project_workspaces
            store = service.runtime.store
            api = WebApi(service)
            headers = {"authorization": "Bearer " + api.control.login("trace", "admin@123")}

            def request(method, path, body=None, status=200):
                response = api.dispatch(method, path, body=body or {}, headers=headers)
                assert response.status == status, (method, path, response.status, response.body)
                return response.payload()

            request("GET", "/api/workspaces/missing", status=404)
            request("GET", "/api/workspaces/missing/export", status=404)
            data = request("POST", "/api/workspaces", {"title": "Lifecycle", "goal": "Check ownership"}, 201)["workspace"]
            wid = data["workspace"]["workspace_id"]
            iid = data["intents"][0]["id"]
            run_id = data["runs"][0]["run_id"]
            base = f"/api/workspaces/{wid}"
            intent = base + f"/intents/{iid}"
            request("POST", intent + "/heartbeat", {"worker": ""}, 400)
            request("POST", intent + "/release", {"worker": ""}, 400)
            request("POST", intent + "/claim", {"worker": "first"})
            request("POST", intent + "/heartbeat", {"worker": "first"})
            request("POST", intent + "/release", {"worker": "other"}, 400)
            request("POST", intent + "/conclude", {"worker": "first", "description": "premature"}, 400)
            request("POST", intent + "/release", {"worker": "first"})
            for _ in range(3):
                results = race(*(lambda worker=worker: manager.claim_intent(wid, iid, worker)
                                 for worker in ("a", "b", "c", "d")))
                assert results.count("ok") == 1, results
                winner = manager.detail(wid)["intents"][0]["worker"]
                manager.release_intent(wid, iid, winner)
            # Expiry is checked on writes even when there was no intervening GET.
            for timestamp in ("2000-01-01T00:00:00+00:00", "", "malformed"):
                manager.claim_intent(wid, iid, "old")
                manager.claim_reason(wid, "old", "inspect")
                with store.transaction(immediate=True) as connection:
                    connection.execute("UPDATE workspace_intents SET last_heartbeat_at=? WHERE intent_id=?", (timestamp, iid))
                    connection.execute("UPDATE workspace_meta SET reason_last_heartbeat_at=? WHERE workspace_id=?", (timestamp, wid))
                rejects(lambda: manager.heartbeat_intent(wid, iid, "old"), "not_owned")
                rejects(lambda: manager.release_intent(wid, iid, "old"), "not_owned")
                rejects(lambda: manager.conclude_intent(wid, iid, "old", "expired"), "not_owned")
                rejects(lambda: manager.heartbeat_reason(wid, "old"), "not_owned")
                rejects(lambda: manager.release_reason(wid, "old"), "not_owned")
                manager.claim_intent(wid, iid, "new")
                manager.claim_reason(wid, "new", "new trigger")
                results = race(lambda: manager.heartbeat_intent(wid, iid, "old"),
                               lambda: manager.release_intent(wid, iid, "old"),
                               lambda: manager.heartbeat_intent(wid, iid, "new"))
                assert results.count("ok") == 1, results
                manager.release_intent(wid, iid, "new")
                manager.release_reason(wid, "new")
            results = race(*(lambda worker=worker: manager.claim_reason(wid, worker, "inspect") for worker in ("a", "b", "c")))
            assert results.count("ok") == 1, results
            owner = manager.detail(wid)["workspace"]["reason_worker"]
            request("POST", base + "/reason/heartbeat", {"worker": owner})
            request("POST", base, {"action": "stopped"})
            assert manager.detail(wid)["workspace"]["reason_worker"] == ""
            request("POST", base + "/reason/claim", {"worker": owner}, 400)
            request("POST", base, {"action": "active"})
            manager.claim_reason(wid, owner, "finish")
            service.cancel(run_id)
            completed = request("POST", base, {"action": "completed"})["workspace"]
            assert completed["workspace"]["reason_worker"] == ""
            assert completed["intents"][0]["status"] == "open" and completed["intents"][0]["to"] is None
            manager.claim_intent(wid, iid, "operator")
            results = race(lambda: manager.conclude_intent(wid, iid, "operator", "Operator conclusion A"),
                           lambda: manager.conclude_intent(wid, iid, "operator", "Operator conclusion B"))
            assert results.count("ok") == 1, results
            concluded = manager.detail(wid)
            fact_id = concluded["intents"][0]["to"]
            fact = next(item for item in concluded["facts"] if item["id"] == fact_id)
            assert fact["source"] == "operator_conclusion" and fact["verified"] is False
            assert not concluded["hints"] and len(store.facts(run_id)) == 1
            assert store.facts(run_id)[0].source_evidence_ids == ()
            assert service.runtime.evidence_graph.list(run_id) == ()
            for action in (lambda: manager.claim_intent(wid, iid, "operator"),
                           lambda: manager.heartbeat_intent(wid, iid, "operator"),
                           lambda: manager.release_intent(wid, iid, "operator"),
                           lambda: manager.conclude_intent(wid, iid, "operator", "overwrite")):
                rejects(action, "concluded")
            assert manager.detail(wid)["intents"] == concluded["intents"]
            assert manager.export(wid)["facts"] == concluded["facts"]
            # A failed fact insert cannot leave a dangling conclusion edge.
            another = manager.add_intent(wid, {"description": "Rollback check"})
            second_run = another["intent"]["run_id"]
            second_id = next(item["id"] for item in another["intents"] if item["run_id"] == second_run)
            service.cancel(second_run)
            manager.claim_intent(wid, second_id, "operator")
            with patch("redteam_agent.application.workspaces.uuid4") as identity:
                identity.return_value.hex = fact_id.removeprefix("fact-")
                try:
                    manager.conclude_intent(wid, second_id, "operator", "Must roll back")
                except sqlite3.IntegrityError:
                    pass
                else:
                    raise AssertionError("expected duplicate fact insertion to fail")
            second = next(item for item in manager.detail(wid)["intents"] if item["id"] == second_id)
            assert second["status"] == "open" and second["to"] is None and not store.facts(second_run)
            service.delete_conversation(second_run)
            # Both materialized and legacy runs must be queried for verified evidence.
            with patch.object(service.runtime.evidence_graph, "list", wraps=service.runtime.evidence_graph.list) as evidence:
                manager.detail(wid)
                evidence.assert_called_once_with(run_id, include_unverified=False)
            legacy = service.start({"session_id": "legacy", "objective": "Legacy goal", "targets": ["local"]}).single.run.run_id
            detail = manager.detail("legacy")
            assert detail["workspace"]["title"] == "Legacy goal" and detail["workspace"]["origin"] == "local"
            assert detail["workspace"]["unclaimed_intent_count"] == 1
            assert manager._meta("legacy") is None
            manager.rename("legacy", "Renamed legacy")
            manager.add_hint("legacy", {"content": "legacy hint"})
            manager.claim_intent("legacy", "run:" + legacy, "legacy-worker")
            service.cancel(legacy)
            manager.conclude_intent("legacy", "run:" + legacy, "legacy-worker", "Legacy conclusion")
            assert manager.detail("legacy")["workspace"]["title"] == "Renamed legacy"
            # Persisted fact and intent survive a fresh manager/service read.
            with AgentService(root=Path(directory), load_external_configuration=False) as reopened:
                assert reopened.project_workspaces.detail(wid)["facts"] == concluded["facts"]
            request("DELETE", base)
            request("GET", base, status=404)
            manager.delete("legacy")
            active = manager.create({"title": "Delete active"})["workspace"]["workspace_id"]
            request("DELETE", f"/api/workspaces/{active}")
            with store.connection() as connection:
                for table in ("workspace_meta", "workspace_intents", "workspace_hints", "facts", "operations"):
                    assert connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0] == 0, table
    print(json.dumps({"ok": True, "checks": ["missing-404", "API-routes", "atomic-claim-races", "expired-ownership",
        "stale-worker-races", "reason-stop-complete", "no-fabricated-completion", "atomic-conclude",
        "immutable-conclusion", "fact-provenance", "fact-edge-rollback", "evidence-projection", "legacy-edit", "restart", "cascade-delete"]}))


if __name__ == "__main__":
    main()
