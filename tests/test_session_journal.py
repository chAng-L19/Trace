from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.runtime.model_common import utc_now
from redteam_agent.runtime.model_records import (
    ModelObservationRecord,
    ModelRequestRecord,
    ModelResponseRecord,
)
from redteam_agent.runtime.store_common import ImmutableRecordError, StoreConflictError


def _service(tmp_path: Path, session_id: str = "journal") -> tuple[AgentService, str]:
    target = tmp_path / f"{session_id}.txt"
    target.write_text("journal fixture\n", encoding="utf-8")
    service = AgentService(root=tmp_path / session_id)
    run_id = service.start(
        StartRequest(
            session_id=session_id,
            objective=f"Give me a plan for {target}; do not make changes yet",
            targets=(str(target),),
        )
    ).single.run.run_id
    return service, run_id


def test_journal_unifies_run_bound_sources_without_copying_payloads(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    created_at = utc_now()
    service.runtime.store.save_model_request(
        ModelRequestRecord(
            request_id="journal-request",
            run_id=run_id,
            prompt_hash="a" * 64,
            provider="fixture",
            model="fixture-model",
            capabilities={},
            request={"messages": []},
            created_at=created_at,
        )
    )
    service.runtime.store.save_model_response(
        ModelResponseRecord(
            request_id="journal-request",
            run_id=run_id,
            status="completed",
            provider="fixture",
            model="fixture-model",
            response_hash="b" * 64,
            claimed_response_hash="",
            usage={},
            response={"text": "done"},
            created_at=created_at,
        )
    )
    service.runtime.store.save_model_observation(
        ModelObservationRecord(
            observation_id="journal-observation",
            request_id="journal-request",
            run_id=run_id,
            action_id="inspect-target",
            call_id="journal-call",
            tool_name="fixture:tool",
            status="success",
            input_hash="c" * 64,
            output_hash="d" * 64,
            observation={"status": "success"},
            created_at=created_at,
        )
    )
    service.record_exploration(
        run_id,
        {
            "record_id": "journal-hypothesis",
            "hypothesis_id": "journal-hypothesis",
            "kind": "hypothesis",
            "status": "active",
            "statement": "Inspect the fixture",
        },
    )
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content={"note": "retain raw message"},
        protected=False,
        source_type="journal-test",
        source_id="assistant-1",
    )
    summary = service.compact_context(run_id)

    entries = service.session_entries(run_id)
    entry_types = {entry.entry_type for entry in entries}
    exported = service.export_session(run_id)

    assert summary is not None
    assert {
        "event:operation_started",
        "message",
        "model_request",
        "model_response",
        "model_observation",
        "exploration",
        "compaction",
    } <= entry_types
    assert len(exported["tree"]["nodes"]) == len(entries)
    assert exported["session"]["run_id"] == run_id
    assert all("raw_ref" in node["entry"] for node in exported["tree"]["nodes"].values())
    assert all("payload" not in node["entry"] for node in exported["tree"]["nodes"].values())
    request_entry = next(entry for entry in entries if entry.entry_type == "model_request")
    assert service.journal.raw(run_id, request_entry.entry_id)["request_id"] == "journal-request"
    assert service.status(run_id).evidence == ()


def test_branch_fork_checkout_and_restart_preserve_active_path(tmp_path: Path) -> None:
    root = tmp_path / "journal-branches"
    service = AgentService(root=root)
    run_id = service.start(
        StartRequest(session_id="branches", objective="Give me a plan; do not make changes yet")
    ).single.run.run_id
    branch_point = service.journal.leaf_id(run_id)
    assert branch_point is not None
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content="main-only",
        protected=False,
        source_type="branch-test",
        source_id="main",
    )
    main_leaf = service.journal.leaf_id(run_id)
    assert main_leaf is not None

    service.fork_session(run_id, branch_point, "alternate")
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content="alternate-only",
        protected=False,
        source_type="branch-test",
        source_id="alternate",
    )
    alternate_leaf = service.journal.leaf_id(run_id)
    active = [message.content for message in service.transcript(run_id)]

    assert alternate_leaf is not None
    assert "alternate-only" in active
    assert "main-only" not in active
    assert main_leaf not in {entry.entry_id for entry in service.replay_session(run_id)}

    recovered = AgentService(root=root)
    assert recovered.journal.leaf_id(run_id) == alternate_leaf
    assert [message.content for message in recovered.transcript(run_id)] == active
    recovered.checkout_session(run_id, "main")
    main = [message.content for message in recovered.transcript(run_id)]
    assert "main-only" in main
    assert "alternate-only" not in main


def test_branch_uses_leaf_cas_and_keeps_both_children(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, "branch-cas")
    parent = service.journal.leaf_id(run_id)
    assert parent is not None
    service.runtime.store.append_event(run_id, "first-child")
    first_child = service.journal.leaf_id(run_id)
    assert first_child is not None

    with pytest.raises(StoreConflictError, match="journal_leaf_conflict"):
        service.branch_session(run_id, parent, expected_leaf_id="stale-leaf")
    service.branch_session(run_id, parent, expected_leaf_id=first_child)
    service.runtime.store.append_event(run_id, "second-child")
    second_child = service.journal.leaf_id(run_id)
    tree = service.session_tree(run_id)

    assert second_child is not None
    assert tree["nodes"][parent]["children"] == [first_child, second_child]
    assert first_child not in {entry.entry_id for entry in service.replay_session(run_id)}


def test_cross_run_entry_references_are_rejected(tmp_path: Path) -> None:
    service, first_run = _service(tmp_path, "shared")
    target = tmp_path / "second.txt"
    target.write_text("second\n", encoding="utf-8")
    second_run = service.start(
        StartRequest(
            session_id="second",
            objective=f"Give me a plan for {target}; do not make changes yet",
            targets=(str(target),),
        )
    ).single.run.run_id
    foreign_entry = service.session_entries(first_run)[0]

    with pytest.raises(ValueError, match="journal_entry_run_mismatch"):
        service.journal.entry(second_run, foreign_entry.entry_id)
    with pytest.raises(ValueError, match="journal_entry_run_mismatch"):
        service.branch_session(second_run, foreign_entry.entry_id)


def test_raw_hash_tampering_is_detected(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, "tamper")
    message_entry = next(
        entry for entry in service.session_entries(run_id) if entry.raw_table == "conversation_messages"
    )
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE conversation_messages SET message_json='{}' WHERE message_id=?",
            (message_entry.raw_id,),
        )

    with pytest.raises(ImmutableRecordError, match="journal_raw_hash_mismatch"):
        service.journal.raw(run_id, message_entry.entry_id)


def test_compaction_keeps_request_messages_atomic(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, "atomic-compaction")
    first = service.conversation.append(
        run_id=run_id,
        role="assistant",
        content="tool call",
        protected=False,
        source_type="model_response",
        source_id="atomic-request",
        metadata={"request_id": "atomic-request"},
    )
    second = service.conversation.append(
        run_id=run_id,
        role="tool",
        content="tool result",
        protected=False,
        source_type="tool_result",
        source_id="atomic-request:call",
        metadata={"request_id": "atomic-request"},
    )

    summary = service.compact_context(run_id, (second.message_id,))

    assert summary is not None
    assert summary.source_message_ids == (first.message_id, second.message_id)


def test_compaction_is_reused_only_on_the_active_branch(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path, "branch-compaction")
    message = service.conversation.append(
        run_id=run_id,
        role="assistant",
        content="shared source",
        protected=False,
        source_type="branch-compaction",
        source_id="source",
    )
    branch_point = service.journal.leaf_id(run_id)
    assert branch_point is not None
    main_summary = service.compact_context(run_id, (message.message_id,))
    assert main_summary is not None

    service.fork_session(run_id, branch_point, "alternate")
    alternate_summary = service.compact_context(run_id, (message.message_id,))

    assert alternate_summary is not None
    assert alternate_summary.source_hash == main_summary.source_hash
    assert alternate_summary.summary_id != main_summary.summary_id
    assert service.journal.context_summaries(run_id) == (alternate_summary,)


def test_schema_upgrade_backfills_existing_raw_records(tmp_path: Path) -> None:
    root = tmp_path / "upgrade"
    service = AgentService(root=root)
    run_id = service.start(
        StartRequest(session_id="upgrade", objective="Give me a plan; do not make changes yet")
    ).single.run.run_id
    expected_messages = len(service.runtime.store.conversation_messages(run_id))
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute("DROP TABLE session_journal_heads")
        connection.execute("DROP TABLE session_journal_state")
        connection.execute("DROP TABLE session_journal_entries")
        connection.execute("DELETE FROM schema_metadata WHERE key='migration:10'")
        connection.execute("UPDATE schema_metadata SET value='9' WHERE key='schema_version'")
        connection.execute("PRAGMA user_version=9")

    recovered = AgentService(root=root)
    entries = recovered.session_entries(run_id)

    assert recovered.runtime.store.schema_version() == 10
    assert len(recovered.transcript(run_id)) == expected_messages
    assert {entry.raw_table for entry in entries} >= {
        "operation_events",
        "conversation_messages",
    }
