"""Offline history search/pagination and latest transcript integrity regressions."""
from __future__ import annotations

import hashlib
import json
import os
import sys
import tempfile
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import patch
from urllib.parse import urlencode

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.runtime import LEGACY_TO_CORE_STATUS
from redteam_agent.adapters.web import WebApi
from redteam_agent.runtime.model_contracts import GoalContract
from redteam_agent.runtime.model_state import OperationState


def seed(service):
    states = []
    statuses = ("running", "completed", "failed_integrity", "waiting_host", "failed")
    for index in range(1005):
        goal = GoalContract(goal_id=f"goal-{index:04d}",
            objective=(f"旧名称 Needle {index:04d}" if index < 15 else f"Recent task {index:04d}"),
            targets=(f"target-{index:04d}.example",), constraints={"note": "unsearchable-metadata"})
        state = OperationState(run_id=f"history-{index:04d}", session_id=f"session-{index:04d}",
            goal=goal, workflow_id="fixture", status=statuses[index % len(statuses)], state_version=1,
            created_at="2026-01-01T00:00:00+00:00", updated_at="2026-01-01T00:00:00+00:00")
        states.append(state)
    states[0].goal = GoalContract(goal_id="goal-0000", objective="旧名称 Needle 100%_done",
        targets=("https://LEGACY.example/a", "https://second.example/路径"),
        constraints={"note": "unsearchable-metadata"})
    with service.runtime.store.transaction() as db:
        db.executemany("INSERT INTO operations(run_id,session_id,goal_id,workflow_id,status,state_json,"
            "version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", [
                (s.run_id, s.session_id, s.goal.goal_id, s.workflow_id, s.status,
                 json.dumps(s.to_dict(), ensure_ascii=True), 1, s.created_at, s.updated_at)
                for s in states])
    for index in range(450):
        service.conversation.append(run_id=states[0].run_id, role="assistant",
            content={"text": f"message-{index:03d}"}, protected=False,
            source_type="fixture", source_id=str(index))
    return states


def database_hash(store):
    with store.connection() as db:
        return hashlib.sha256("\n".join(db.iterdump()).encode()).hexdigest()


def exercise(service, states):
    api, store = WebApi(service), service.runtime.store
    headers = {"Authorization": "Bearer " + api.control.login("trace", "history-pass")}

    def request(route, *, expected_status=200, auth=headers, **query):
        response = api.dispatch("GET", route + "?" + urlencode(query), headers=auth)
        assert response.status == expected_status, (route, query, response.status, response.payload())
        return response.payload()

    def listing(kind, **query):
        return request("/api/" + kind, **query)

    def ids(result, kind):
        return [item["run"]["run_id"] if kind == "runs" else item["run"]["run"]["run_id"]
                for item in result[kind]]

    ordered = [s.run_id for s in reversed(states)]
    run_id = states[0].run_id
    transcript_route = f"/api/runs/{run_id}/transcript"
    original_messages = service.transcript(run_id)
    message_ids = [item.message_id for item in original_messages]
    original_session = service.export_session(run_id)["session"]
    before = database_hash(store)
    with ExitStack() as stack:
        for target, name in ((service, "status"), (service.runtime, "status"),
                             (service, "start"), (service, "export_session"),
                             (store, "conversation_messages"), (service.runtime.broker, "refresh")):
            stack.enter_context(patch.object(target, name, side_effect=AssertionError(f"unexpected {name}")))
        for kind, page_size in (("runs", 200), ("conversations", 100)):
            default = listing(kind)
            assert ids(default, kind) == ordered[:100] and default["truncated"]
            collected = []
            for offset in range(0, len(states), page_size):
                result = listing(kind, limit=page_size, offset=offset)
                assert ids(result, kind) == ordered[offset:offset + page_size]
                assert result["truncated"] == (offset + page_size < len(states))
                collected.extend(ids(result, kind))
            assert collected == ordered and len(set(collected)) == len(states)
            maximum = listing(kind, limit=1000)
            assert len(maximum[kind]) == 1000 and maximum["truncated"]
            assert ids(listing(kind, limit=1000, offset=1000), kind) == ordered[1000:]
            assert listing(kind, offset=1005)[kind] == []
            assert not listing(kind, offset=1005)["truncated"]

            for keyword in ("100%_done", "legacy.EXAMPLE", "路径", "session-0000", "history-0000"):
                result = listing(kind, search=keyword, limit=1)
                assert ids(result, kind) == [run_id] and not result["truncated"], (keyword, result)
            assert ids(listing(kind, search="  NEEDLE  ", limit=20), kind) == ordered[-15:]
            for keyword in ("unsearchable-metadata", "objective", "no-match", "' OR 1=1 --", "100%xdone"):
                result = listing(kind, search=keyword)
                assert result[kind] == [] and not result["truncated"], (keyword, result)
            for status_filter in ("failed", "waiting_worker", "running", "completed", "unknown"):
                matching = [s.run_id for s in reversed(states[:15])
                    if LEGACY_TO_CORE_STATUS.get(s.status, s.status) == status_filter]
                for offset in range(0, len(matching) + 1, 2):
                    result = listing(kind, search="needle", limit=2, offset=offset, status=status_filter)
                    assert ids(result, kind) == matching[offset:offset + 2]
                    assert result["truncated"] == (offset + 2 < len(matching))
            for query in ({"limit": 0}, {"limit": 1001}, {"limit": "bad"}, {"offset": -1}, {"offset": 1000001}):
                listing(kind, expected_status=400, **query)
            listing(kind, expected_status=401, auth={}, search="needle")
            # Only limit+1 matching states are deserialized, not the complete history.
            with patch.object(store, "_state_from_row", wraps=store._state_from_row) as loaded:
                listing(kind, search="needle", limit=2)
                assert loaded.call_count == 3, (kind, loaded.call_count)

        assert service.list_runs(search="LEGACY.example", summary_only=True)[0].run.run_id == run_id
        assert service.conversation_summaries(search="LEGACY.example")[0]["session"]["run_id"] == run_id
        assert {view.run.status for view in service.list_runs(search="needle", status="failed", summary_only=True)} == {"failed"}
        assert {item["run"]["run"]["status"] for item in service.conversation_summaries(search="needle", status="failed")} == {"failed"}
        summary = listing("conversations", search=run_id)["conversations"][0]
        assert summary["message_count"] == 450
        for key in ("session_id", "run_id", "active_branch_id", "branches"):
            assert summary["session"][key] == original_session[key], key
        assert summary["run"] == listing("runs", search=run_id)["runs"][0]

        # Summary queries remain batched (one page, one heads query, one count query).
        queries = []
        connect = store._connect
        def traced_connection():
            db = connect()
            db.set_trace_callback(queries.append)
            return db
        with patch.object(store, "_connect", side_effect=traced_connection):
            service.conversation_summaries(limit=100, search="recent")
        reads = [q for q in queries if q.lstrip().upper().startswith(("SELECT", "WITH"))]
        assert len(reads) == 3, reads

        chunks = []
        for offset, expected, more in ((0, message_ids[250:], True), (200, message_ids[50:250], True),
                                        (400, message_ids[:50], False)):
            result = request(transcript_route, latest=1, limit=200, offset=offset)
            assert [m["message_id"] for m in result["messages"]] == expected
            assert result["truncated"] is more
            chunks.insert(0, [m["message_id"] for m in result["messages"]])
        rebuilt = [mid for chunk in chunks for mid in chunk]
        assert rebuilt == message_ids and len(set(rebuilt)) == 450
        assert request(transcript_route, latest=1, limit=200, offset=450)["messages"] == []
        assert not request(transcript_route, latest=1, limit=200, offset=450)["truncated"]
        for offset in (0, 200, 400):
            result = request(transcript_route, limit=200, offset=offset)
            assert [m["message_id"] for m in result["messages"]] == message_ids[offset:offset + 200]
            assert result["truncated"] == (offset + 200 < 450)
        for limit in (1, 200, 400, 450, 1000):
            result = request(transcript_route, latest=1, limit=limit)
            assert [m["message_id"] for m in result["messages"]] == message_ids[-limit:]
            assert result["truncated"] == (limit < 450)
        assert service.transcript(run_id, latest=True, limit=200) == original_messages[-200:]
        assert service.transcript(run_id, latest=True) == original_messages
        request("/api/runs/missing/transcript", latest=1, expected_status=404)
        request(transcript_route, latest=1, auth={}, expected_status=401)
    assert database_hash(store) == before, "GET mutated fixture history"

    # Fail closed using the existing message hash and journal/raw checks, in both directions.
    with store.connection() as db:
        row = db.execute("SELECT * FROM conversation_messages WHERE message_id=?", (message_ids[-1],)).fetchone()
    tampered = json.loads(row["message_json"])
    tampered["content"] = {"text": "tampered"}
    with store.transaction() as db:
        db.execute("UPDATE conversation_messages SET message_json=? WHERE message_id=?",
                   (json.dumps(tampered), message_ids[-1]))
    for latest in (0, 1):
        assert "conversation_record_integrity" in request(transcript_route, latest=latest, limit=450, expected_status=409)["error"]
    with store.transaction() as db:
        db.execute("UPDATE conversation_messages SET message_json=? WHERE message_id=?", (row["message_json"], message_ids[-1]))
        db.execute("DELETE FROM conversation_messages WHERE message_id=?", (message_ids[-1],))
    assert "journal_raw_missing" in request(transcript_route, latest=1, expected_status=409)["error"]
    with store.transaction() as db:
        db.execute("INSERT INTO conversation_messages(" + ",".join(row.keys()) + ") VALUES(" +
            ",".join("?" for _ in row) + ")", tuple(row))
        head = db.execute("SELECT leaf_entry_id FROM session_journal_heads WHERE run_id=? AND branch_id='main'", (run_id,)).fetchone()[0]
        parent = db.execute("SELECT parent_entry_id FROM session_journal_entries WHERE entry_id=?", (head,)).fetchone()[0]
        db.execute("UPDATE session_journal_entries SET parent_entry_id=? WHERE entry_id=?", (head, head))
    assert "journal_parent_integrity" in request(transcript_route, latest=1, expected_status=409)["error"]
    with store.transaction() as db:
        db.execute("UPDATE session_journal_entries SET parent_entry_id=? WHERE entry_id=?", (parent, head))
    assert database_hash(store) == before

    # Active branches, rather than all raw messages, define both summary and transcript.
    fork_entry = next(e for e in service.session_entries(run_id) if e.raw_id == message_ids[19])
    service.fork_session(run_id, fork_entry.entry_id, "history-fork")
    message = service.conversation.append(run_id=run_id, role="assistant", content={"text": "fork-only"},
        protected=False, source_type="fixture", source_id="fork")
    result = request(transcript_route, latest=1, limit=200)
    assert [m["message_id"] for m in result["messages"]] == message_ids[:20] + [message.message_id]
    assert not result["truncated"]
    summary = listing("conversations", search=run_id)["conversations"][0]
    assert summary["message_count"] == 21 and summary["session"]["active_branch_id"] == "history-fork"
    service.checkout_session(run_id, "main")
    assert listing("conversations", search=run_id)["conversations"][0]["message_count"] == 450


def main():
    with tempfile.TemporaryDirectory(prefix="trace-history-search-") as directory, patch.dict(os.environ,
        {"TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "history-pass"}):
        with AgentService(root=Path(directory), load_external_configuration=False) as service:
            exercise(service, seed(service))
    print(json.dumps({"ok": True, "runs": 1005, "messages": 450, "checks": [
        "search-before-pagination", "objective-target-session-run-search", "literal-search",
        "status-aliases", "lookahead-at-1000", "batched-summaries", "read-only-history", "auth",
        "latest-200-200-50", "ascending-compatibility", "message-journal-raw-integrity", "active-branch"]}))


if __name__ == "__main__":
    main()
