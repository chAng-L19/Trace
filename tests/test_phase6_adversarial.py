from __future__ import annotations

import json
from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.runtime import (
    ExplorationValidationError,
    ImmutableRecordError,
    TacticalAttemptRecord,
)


def _run(service: AgentService, session: str, target: str) -> str:
    return service.start(
        StartRequest(
            session_id=session,
            objective=f"Assess {target} and preserve exact evidence lineage",
            targets=(target,),
            max_actions=32,
        )
    ).single.run.run_id


def _scoped_negative(service: AgentService, run_id: str, artifact_id: str) -> None:
    service.record_exploration(
        run_id,
        {
            "record_id": "negative-scoped",
            "hypothesis_id": "hidden-route",
            "kind": "verified_negative",
            "status": "closed",
            "statement": "No match in the exact tested set",
            "artifact_refs": [artifact_id],
            "tested_domain": {"method": "GET", "entries": 16},
            "observations": {"matches": 0},
            "coverage": {"entries": 16, "recursive": False},
            "confidence": 0.7,
            "uncertainty": "Generated paths remain outside coverage",
            "reopen_triggers": ["capability:schema_discovery"],
        },
    )


def test_cross_run_artifact_and_parent_references_are_rejected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_a = _run(service, "phase6-a", "fixture://a")
    run_b = _run(service, "phase6-b", "fixture://b")
    artifact = service.runtime.artifacts.put_json(
        {"source": "a"}, run_id=run_a, artifact_type="raw_probe"
    )
    service.record_exploration(
        run_a,
        {
            "record_id": "run-a-parent",
            "hypothesis_id": "a",
            "kind": "lead",
            "status": "active",
            "statement": "Run A lead",
            "artifact_refs": [artifact.artifact_id],
        },
    )

    with pytest.raises(ExplorationValidationError, match="artifact_invalid"):
        service.record_exploration(
            run_b,
            {
                "record_id": "cross-run-artifact",
                "hypothesis_id": "b",
                "kind": "lead",
                "status": "active",
                "statement": "Attempt cross-run Artifact reference",
                "artifact_refs": [artifact.artifact_id],
            },
        )
    with pytest.raises(ExplorationValidationError, match="parent_missing"):
        service.record_exploration(
            run_b,
            {
                "record_id": "cross-run-parent",
                "hypothesis_id": "b",
                "kind": "lead",
                "status": "active",
                "statement": "Attempt cross-run parent reference",
                "parent_record_ids": ["run-a-parent"],
            },
        )


def test_exploration_sqlite_tampering_is_detected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "phase6-tamper", "fixture://tamper")
    service.record_exploration(
        run_id,
        {
            "record_id": "tamper-record",
            "hypothesis_id": "tamper",
            "kind": "hypothesis",
            "status": "active",
            "statement": "Original hypothesis",
        },
    )
    with service.runtime.store.transaction(immediate=True) as connection:
        row = connection.execute(
            "SELECT record_json FROM exploration_records WHERE record_id='tamper-record'"
        ).fetchone()
        payload = json.loads(row["record_json"])
        payload["statement"] = "forged conclusion"
        connection.execute(
            "UPDATE exploration_records SET record_json=? WHERE record_id='tamper-record'",
            (json.dumps(payload, sort_keys=True),),
        )

    with pytest.raises(ImmutableRecordError, match="exploration_record_integrity"):
        service.exploration_records(run_id)


def test_tactical_attempt_replay_is_idempotent_payload_bound_and_budgeted_once(
    tmp_path: Path,
) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "phase6-attempt", "fixture://attempt")
    attempt = TacticalAttemptRecord(
        attempt_id="tactical-attempt-fixed",
        run_id=run_id,
        request_id="request-fixed",
        call_id="call-fixed",
        lifecycle_action_id="map-surface",
        action_fingerprint="f" * 64,
        status="success",
        payload={"tool": "fixture:probe", "output_hash": "a" * 64},
        created_at="2026-08-17T00:00:00+00:00",
    )

    first = service.runtime.record_tactical_attempt(attempt)
    first_budget = service.status(run_id).run.budget.actions_used
    second = service.runtime.record_tactical_attempt(attempt)
    second_budget = service.status(run_id).run.budget.actions_used

    assert first == second
    assert first_budget == second_budget == 1
    assert len(service.runtime.store.tactical_attempts(run_id)) == 1

    changed = TacticalAttemptRecord(
        **{**attempt.to_dict(), "payload": {"tool": "fixture:other"}}
    )
    with pytest.raises(ImmutableRecordError, match="immutable_tactical_attempt"):
        service.runtime.record_tactical_attempt(changed)


def test_tactical_attempt_column_and_json_tampering_are_detected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "phase6-attempt-tamper", "fixture://attempt-tamper")
    attempt = TacticalAttemptRecord(
        attempt_id="tactical-attempt-tamper",
        run_id=run_id,
        request_id="request-tamper",
        call_id="call-tamper",
        lifecycle_action_id="map-surface",
        action_fingerprint="e" * 64,
        status="success",
        payload={"tool": "fixture:probe"},
        created_at="2026-08-17T00:00:00+00:00",
    )
    service.runtime.record_tactical_attempt(attempt)
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE tactical_attempts SET status='failed' WHERE attempt_id=?",
            (attempt.attempt_id,),
        )

    with pytest.raises(ImmutableRecordError, match="tactical_attempt_integrity"):
        service.runtime.store.tactical_attempts(run_id)


def test_recon_digest_tampering_and_missing_sources_are_detected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "phase6-digest-tamper", "fixture://digest")
    service.record_exploration(
        run_id,
        {
            "record_id": "digest-source",
            "hypothesis_id": "digest-source",
            "kind": "hypothesis",
            "status": "active",
            "statement": "Digest source",
        },
    )
    digest = service.recon_digest(run_id)
    with service.runtime.store.transaction(immediate=True) as connection:
        row = connection.execute(
            "SELECT digest_json FROM recon_digests WHERE digest_id=?", (digest.digest_id,)
        ).fetchone()
        payload = json.loads(row["digest_json"])
        payload["digest"]["run_status"] = "completed"
        connection.execute(
            "UPDATE recon_digests SET digest_json=? WHERE digest_id=?",
            (json.dumps(payload, sort_keys=True), digest.digest_id),
        )

    with pytest.raises(ImmutableRecordError, match="recon_digest_integrity"):
        service.recon_digests(run_id)


def test_restart_preserves_closed_state_and_new_capability_reopens_it(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    first = AgentService(root=root)
    run_id = _run(first, "phase6-restart", "fixture://restart")
    artifact = first.runtime.artifacts.put_json(
        {"matches": 0}, run_id=run_id, artifact_type="enumeration_transcript"
    )
    _scoped_negative(first, run_id, artifact.artifact_id)
    assert first.exploration.current(run_id)[0].status == "closed"

    recovered = AgentService(root=root)
    recovered.record_exploration(
        run_id,
        {
            "record_id": "restart-capability",
            "hypothesis_id": "new-capability",
            "kind": "lead",
            "status": "active",
            "statement": "Schema discovery is now available",
            "artifact_refs": [artifact.artifact_id],
            "capabilities": ["schema_discovery"],
        },
    )
    current = {item.hypothesis_id: item for item in recovered.exploration.current(run_id)}
    assert current["hidden-route"].status == "reopened"
    assert current["hidden-route"].parent_record_ids == (
        "negative-scoped",
        "restart-capability",
    )
