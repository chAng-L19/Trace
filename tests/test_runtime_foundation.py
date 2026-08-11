from __future__ import annotations

import hashlib
import json
import sqlite3
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.durable_store import (
    DurableStore,
    ImmutableRecordError,
    LeaseLostError,
    MAX_HANDOFF_OBSERVATION_BYTES,
    StateVersionConflict,
    StoreConflictError,
)
from redteam_agent.runtime.store_common import SCHEMA_VERSION
from redteam_agent.runtime.facts import FactLedger, FactValidationError
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.models import EvidenceNode, EvidenceProvenance, OperationState, TaskAttempt, utc_now
from redteam_agent.runtime.plan import PlanDelta, PlanRevision
from redteam_agent.runtime.workflow_registry import WorkflowRegistry


def _operation(tmp_path: Path) -> tuple[DurableStore, OperationState]:
    goal = GoalCompiler().compile("Analyze https://target.invalid")
    workflow = WorkflowRegistry().match(goal)
    state = OperationState.create(session_id="foundation", goal=goal, workflow=workflow)
    store = DurableStore(tmp_path / "operations")
    store.create_operation(state, event={"source": "foundation-test"})
    return store, state


def test_operation_state_uses_compare_and_swap(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    left = store.load_operation(state.run_id)
    right = store.load_operation(state.run_id)
    assert left is not None and right is not None and left.state_version == right.state_version == 1

    left.status = "left-won"
    store.save_operation(left)
    right.status = "stale-write"
    with pytest.raises(StateVersionConflict, match="state_version_conflict"):
        store.save_operation(right)

    loaded = store.load_operation(state.run_id)
    assert loaded is not None
    assert loaded.status == "left-won"
    assert loaded.state_version == 2
    assert right.state_version == 1


def test_fencing_token_rejects_expired_writer(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    first = store.acquire_lease(state.run_id, "action", "worker-a", ttl_seconds=30)
    assert first is not None
    with store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE action_leases SET expires_at=0 WHERE run_id=? AND action_id=?",
            (state.run_id, "action"),
        )
    second = store.acquire_lease(state.run_id, "action", "worker-b", ttl_seconds=30)
    assert second is not None and second.fencing_token > first.fencing_token
    assert store.assert_lease(first) is False
    assert store.assert_lease(second) is True

    loaded = store.load_operation(state.run_id)
    assert loaded is not None
    loaded.status = "must-not-commit"
    with pytest.raises(LeaseLostError, match="lease_lost"):
        store.compare_and_swap_operation(
            loaded,
            expected_version=loaded.state_version,
            lease_token=first,
        )
    assert store.release_lease(first) is False
    assert store.assert_lease(second) is True


def test_plan_revisions_are_immutable_and_delta_validated(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    workflow = WorkflowRegistry().get(state.workflow_id)
    first = PlanRevision.from_workflow(run_id=state.run_id, workflow=workflow, plan_id=state.plan_id)
    store.save_plan_revision(first)
    store.save_plan_revision(first)

    extra = replace(
        workflow.actions[-1],
        action_id="post-review",
        name="Post-execution review",
        depends_on=(workflow.actions[-1].action_id,),
    )
    second = PlanDelta(
        plan_id=first.plan_id,
        run_id=first.run_id,
        branch_id=first.branch_id,
        base_revision=first.revision,
        added_actions=(extra,),
        reason="add review",
    ).apply(first)
    store.save_plan_revision(second)
    assert [item.revision for item in store.plan_revisions(state.run_id)] == [1, 2]

    with pytest.raises(ImmutableRecordError, match="immutable_plan_conflict"):
        store.save_plan_revision(replace(second, reason="mutated after commit"))


def test_fact_invalidation_propagates_and_invalidates_overlay(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    records = []
    root = FactLedger.create(
        records,
        run_id=state.run_id,
        branch_id="main",
        plan_revision=1,
        key="service.version",
        value="1.0",
        source_evidence_ids=("e-root",),
    )
    records.append(root)
    child = FactLedger.create(
        records,
        run_id=state.run_id,
        branch_id="main",
        plan_revision=1,
        key="service.vulnerable",
        value=True,
        parent_fact_ids=(root.fact_id,),
        source_evidence_ids=("e-child",),
    )
    records.append(child)
    tombstones = FactLedger.invalidate(
        records,
        fact_id=root.fact_id,
        invalidated_by=("e-new-version",),
        reason="version changed",
        plan_revision=2,
    )
    all_records = [*records, *tombstones]
    assert {item.key for item in tombstones} == {"service.version", "service.vulnerable"}
    assert FactLedger.effective(all_records, run_id=state.run_id, branch_id="main") == {}
    with pytest.raises(FactValidationError, match="fact_overlay_invalid"):
        FactLedger.validate_overlay(all_records, {root.key: root.version})

    for fact in all_records:
        store.save_fact(fact)
    assert len(store.facts(state.run_id, branch_id="main")) == 4


def test_state_recovers_from_event_snapshot(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    state.status = "snapshot-status"
    store.save_operation(state, event_type="snapshot-test")
    with store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE operations SET state_json=? WHERE run_id=?",
            ("{corrupt-json", state.run_id),
        )

    recovered = store.load_operation(state.run_id)
    assert recovered is not None
    assert recovered.status == "snapshot-status"
    assert recovered.state_version == 2
    recovered.status = "healed"
    store.save_operation(recovered)
    assert store.load_operation(state.run_id).status == "healed"


def test_commit_action_outcome_is_atomic(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    action_id = next(iter(state.action_status))
    token = store.acquire_lease(state.run_id, action_id, "executor", ttl_seconds=30)
    assert token is not None
    attempt = TaskAttempt.create(
        run_id=state.run_id,
        branch_id=state.branch_id,
        plan_revision=state.plan_revision,
        action_id=action_id,
        tool="fixture:tool",
        tool_version="1",
        input_hash="input-hash",
        idempotency_key="idem-1",
        fencing_token=token.fencing_token,
    )
    store.create_task_attempt(attempt)
    completed = replace(attempt, status="succeeded", result={"ok": True}, finished_at=utc_now())
    payload = {"results": ["verified"]}
    node = EvidenceNode(
        evidence_id="evidence-atomic",
        run_id=state.run_id,
        action_id=action_id,
        artifact_type="surface_map",
        target=state.goal.targets[0],
        tool=attempt.tool,
        payload=payload,
        content_hash=hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest(),
        parent_ids=(),
        verifier="surface_map",
        confidence=1.0,
        verified=True,
        provenance=EvidenceProvenance(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            attempt_id=attempt.attempt_id,
            tool=attempt.tool,
            tool_version=attempt.tool_version,
            input_hash=attempt.input_hash,
            target=state.goal.targets[0],
        ),
    )
    state.plan_revision = attempt.plan_revision + 1
    state.action_status[action_id] = "completed"
    state.evidence_ids.append(node.evidence_id)
    assert store.commit_action_outcome(
        state=state,
        expected_state_version=1,
        attempt=completed,
        expected_attempt_status="prepared",
        lease_token=token,
        evidence=(node,),
    ) == 2
    assert store.task_attempts(state.run_id)[0].status == "succeeded"
    assert store.evidence(state.run_id) == (node,)

    assert store.release_lease(token) is True
    next_token = store.acquire_lease(state.run_id, action_id, "executor-2", ttl_seconds=30)
    assert next_token is not None
    second_attempt = TaskAttempt.create(
        run_id=state.run_id,
        branch_id=state.branch_id,
        plan_revision=state.plan_revision,
        action_id=action_id,
        tool="fixture:tool",
        tool_version="1",
        input_hash="input-hash-2",
        idempotency_key="idem-2",
        fencing_token=next_token.fencing_token,
    )
    store.create_task_attempt(second_attempt)
    conflicting = replace(
        node,
        payload={"results": ["different"]},
        content_hash=hashlib.sha256(
            json.dumps({"results": ["different"]}, sort_keys=True).encode()
        ).hexdigest(),
        provenance=replace(
            node.provenance,
            attempt_id=second_attempt.attempt_id,
            input_hash=second_attempt.input_hash,
            plan_revision=second_attempt.plan_revision,
        ),
    )
    before_version = state.state_version
    with pytest.raises(ImmutableRecordError, match="immutable_record_conflict"):
        store.commit_action_outcome(
            state=state,
            expected_state_version=before_version,
            attempt=replace(second_attempt, status="succeeded", finished_at=utc_now()),
            expected_attempt_status="prepared",
            lease_token=next_token,
            evidence=(conflicting,),
        )
    attempts = {item.attempt_id: item for item in store.task_attempts(state.run_id)}
    assert attempts[second_attempt.attempt_id].status == "prepared"
    assert store.load_operation(state.run_id).state_version == before_version


def test_schema_migrates_legacy_cas_fencing_and_evidence_identity(tmp_path: Path) -> None:
    root = tmp_path / "legacy"
    root.mkdir()
    path = root / "runtime.sqlite3"
    with sqlite3.connect(path) as connection:
        connection.executescript(
            """
            CREATE TABLE operations (
                run_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, goal_id TEXT NOT NULL,
                workflow_id TEXT NOT NULL, status TEXT NOT NULL, state_json TEXT NOT NULL,
                created_at TEXT NOT NULL, updated_at TEXT NOT NULL
            );
            CREATE TABLE action_leases (
                run_id TEXT NOT NULL, action_id TEXT NOT NULL, owner TEXT NOT NULL,
                expires_at REAL NOT NULL, PRIMARY KEY(run_id, action_id)
            );
            CREATE TABLE evidence_nodes (
                evidence_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, action_id TEXT NOT NULL,
                artifact_type TEXT NOT NULL, tool TEXT NOT NULL, content_hash TEXT NOT NULL,
                node_json TEXT NOT NULL, created_at TEXT NOT NULL,
                UNIQUE(run_id, action_id, artifact_type, tool, content_hash)
            );
            """
        )
    store = DurableStore(root)
    with store.connection() as connection:
        operation_columns = {row["name"] for row in connection.execute("PRAGMA table_info(operations)")}
        lease_columns = {row["name"] for row in connection.execute("PRAGMA table_info(action_leases)")}
        evidence_sql = connection.execute(
            "SELECT sql FROM sqlite_master WHERE type='table' AND name='evidence_nodes'"
        ).fetchone()["sql"]
        schema_version = connection.execute(
            "SELECT value FROM schema_metadata WHERE key='schema_version'"
        ).fetchone()["value"]
    assert "version" in operation_columns
    assert "fencing_token" in lease_columns
    assert "UNIQUE(run_id" not in evidence_sql
    assert schema_version == str(SCHEMA_VERSION)


def test_host_handoff_receipt_is_hashed_bound_and_single_use(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    action_id = next(iter(state.action_status))
    state.status = "waiting_host"
    state.current_action_id = action_id
    store.save_operation(state)
    attempt = replace(TaskAttempt.create(
        run_id=state.run_id,
        branch_id=state.branch_id,
        plan_revision=state.plan_revision,
        action_id=action_id,
        tool="host-agent",
        tool_version="1",
        input_hash="input",
        idempotency_key="handoff-attempt",
    ), status="waiting_host")
    store.create_task_attempt(attempt)
    identity = {
        "run_id": state.run_id,
        "branch_id": state.branch_id,
        "plan_revision": state.plan_revision,
        "action_id": action_id,
        "attempt_id": attempt.attempt_id,
        "contract_hash": "contract-v1",
    }
    old_handoff_id, old_raw_token = store.create_handoff(**identity)
    handoff_id, raw_token = store.create_handoff(**identity)
    with store.connection() as connection:
        row = connection.execute(
            "SELECT token_hash FROM host_handoffs WHERE handoff_id=?", (handoff_id,)
        ).fetchone()
    assert row["token_hash"] == hashlib.sha256(raw_token.encode()).hexdigest()
    assert raw_token not in row["token_hash"]
    assert store.get_handoff(old_handoff_id).status == "superseded"
    assert store.consume_handoff(handoff_id=old_handoff_id, raw_token=old_raw_token, **identity) is False
    assert store.consume_handoff(handoff_id=handoff_id, raw_token="forged", **identity) is False
    assert store.consume_handoff(
        handoff_id=handoff_id,
        raw_token=raw_token,
        **{**identity, "run_id": "run-other"},
    ) is False
    assert store.consume_handoff(handoff_id=handoff_id, raw_token=raw_token, **identity) is True
    assert store.consume_handoff(handoff_id=handoff_id, raw_token=raw_token, **identity) is False
    assert store.task_attempts(state.run_id, action_id=action_id)[0].status == "consumed"

    next_attempt = replace(
        TaskAttempt.create(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            tool="host-agent",
            tool_version="1",
            input_hash="input-2",
            idempotency_key="handoff-attempt-2",
        ),
        status="waiting_host",
    )
    store.create_task_attempt(next_attempt)
    next_identity = {**identity, "attempt_id": next_attempt.attempt_id}
    stale_handoff_id, stale_raw_token = store.create_handoff(**next_identity)
    state.plan_revision = 2
    store.save_operation(state)
    assert store.consume_handoff(
        handoff_id=stale_handoff_id,
        raw_token=stale_raw_token,
        **next_identity,
    ) is False


def test_claim_task_attempt_refences_recovery_and_rejects_stale_writer(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    action_id = next(iter(state.action_status))
    first_lease = store.acquire_lease(state.run_id, action_id, "worker-a", ttl_seconds=30)
    assert first_lease is not None
    attempt = replace(
        TaskAttempt.create(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            tool="fixture:side-effect",
            tool_version="1",
            input_hash="input",
            idempotency_key="recoverable",
            fencing_token=first_lease.fencing_token,
        ),
        status="uncertain",
    )
    store.create_task_attempt(attempt)
    assert store.release_lease(first_lease)
    recovery_lease = store.acquire_lease(state.run_id, action_id, "worker-b", ttl_seconds=30)
    assert recovery_lease is not None and recovery_lease.fencing_token > first_lease.fencing_token

    claimed = store.claim_task_attempt(
        attempt,
        expected_statuses=("running", "uncertain", "reconciling"),
        lease_token=recovery_lease,
    )
    assert claimed.status == "reconciling"
    assert claimed.fencing_token == recovery_lease.fencing_token
    with pytest.raises(LeaseLostError, match="lease_lost"):
        store.update_task_attempt(
            replace(attempt, status="completed"),
            expected_status="uncertain",
            lease_token=first_lease,
        )
    with pytest.raises(StoreConflictError, match="attempt_claim_status_conflict"):
        store.claim_task_attempt(
            claimed,
            expected_statuses=("running", "uncertain"),
            lease_token=recovery_lease,
        )


def test_session_binding_compare_and_swap_preserves_winner(tmp_path: Path) -> None:
    store, _ = _operation(tmp_path)
    first = store.save_session_binding(
        "session-cas",
        {"session_id": "session-cas", "run_id": "run-a", "batch_session_id": ""},
        expected_version=0,
    )
    assert first == 1
    second = store.save_session_binding(
        "session-cas",
        {"session_id": "session-cas", "run_id": "run-b", "batch_session_id": ""},
        expected_version=1,
    )
    assert second == 2
    with pytest.raises(StateVersionConflict, match="session_binding_version_conflict"):
        store.save_session_binding(
            "session-cas",
            {"session_id": "session-cas", "run_id": "stale", "batch_session_id": ""},
            expected_version=1,
        )
    binding = store.session_binding("session-cas")
    assert binding is not None
    assert binding["run_id"] == "run-b"
    assert binding["_version"] == 2


def test_concurrent_session_binding_creation_has_one_cas_winner(tmp_path: Path) -> None:
    store, _ = _operation(tmp_path)
    workers = 6
    barrier = threading.Barrier(workers)

    def write(index: int) -> str:
        barrier.wait(timeout=5)
        try:
            store.save_session_binding(
                "session-race",
                {
                    "session_id": "session-race",
                    "run_id": f"run-{index}",
                    "batch_session_id": "",
                },
                expected_version=0,
            )
        except StateVersionConflict:
            return "conflict"
        return "winner"

    with ThreadPoolExecutor(max_workers=workers) as pool:
        outcomes = list(pool.map(write, range(workers)))
    assert outcomes.count("winner") == 1
    assert outcomes.count("conflict") == workers - 1
    binding = store.session_binding("session-race")
    assert binding is not None and binding["_version"] == 1


def test_expired_handoff_is_rejected_and_placeholder_closed(tmp_path: Path) -> None:
    store, state = _operation(tmp_path)
    action_id = next(iter(state.action_status))
    state.status = "waiting_host"
    state.current_action_id = action_id
    store.save_operation(state)
    attempt = replace(
        TaskAttempt.create(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            tool="host-agent",
            tool_version="1",
            input_hash="expiry",
            idempotency_key="expiry",
        ),
        status="waiting_host",
    )
    store.create_task_attempt(attempt)
    identity = {
        "run_id": state.run_id,
        "branch_id": state.branch_id,
        "plan_revision": state.plan_revision,
        "action_id": action_id,
        "attempt_id": attempt.attempt_id,
        "contract_hash": "expiry-contract",
    }
    handoff_id, raw_token = store.create_handoff(**identity)
    with store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE host_handoffs SET expires_at='2000-01-01T00:00:00+00:00' WHERE handoff_id=?",
            (handoff_id,),
        )

    record = store.get_handoff(handoff_id)
    assert record is not None and record.status == "expired"
    assert store.consume_handoff(
        handoff_id=handoff_id,
        raw_token=raw_token,
        **identity,
    ) is False
    attempts = {item.attempt_id: item for item in store.task_attempts(state.run_id)}
    assert attempts[attempt.attempt_id].status == "expired"


def test_handoff_store_rejects_oversized_direct_observation(tmp_path: Path) -> None:
    """The durable inbox has the same bound as the MCP-facing entrypoint."""
    store, state = _operation(tmp_path)
    assert store.receive_handoff_observation(
        handoff_id="not-reached",
        raw_token="token",
        run_id=state.run_id,
        branch_id=state.branch_id,
        plan_revision=state.plan_revision,
        action_id="action",
        attempt_id="attempt",
        contract_hash="contract",
        output={"payload": "x" * MAX_HANDOFF_OBSERVATION_BYTES},
    ) is None


def test_legacy_handoff_schema_migrates_expiry_without_plaintext_token(tmp_path: Path) -> None:
    root = tmp_path / "legacy-operations"
    root.mkdir(parents=True)
    database = root / "runtime.sqlite3"
    with sqlite3.connect(database) as connection:
        connection.executescript(
            """
            CREATE TABLE host_handoffs (
                handoff_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, branch_id TEXT NOT NULL,
                plan_revision INTEGER NOT NULL, action_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
                token_hash TEXT NOT NULL, contract_hash TEXT NOT NULL, status TEXT NOT NULL,
                created_at TEXT NOT NULL, consumed_at TEXT NOT NULL
            );
            INSERT INTO host_handoffs VALUES(
                'legacy-handoff', 'legacy-run', 'main', 1, 'action', 'attempt',
                'hash-only', 'contract', 'pending', '2000-01-01T00:00:00+00:00', ''
            );
            """
        )

    store = DurableStore(root)
    with store.connection() as connection:
        columns = {
            str(row["name"])
            for row in connection.execute("PRAGMA table_info(host_handoffs)").fetchall()
        }
        row = connection.execute(
            "SELECT token_hash, expires_at FROM host_handoffs WHERE handoff_id='legacy-handoff'"
        ).fetchone()
    assert "expires_at" in columns
    assert row is not None
    assert row["token_hash"] == "hash-only"
    assert row["expires_at"] == "2000-01-01T00:15:00+00:00"
