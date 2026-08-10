from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

from redteam_agent.runtime.durable_store import DurableStore
from redteam_agent.runtime.store_common import SCHEMA_VERSION
from redteam_agent.runtime import store_migrations
from redteam_agent.runtime.store_migrations import MIGRATIONS, Migration, SchemaMigrationError


def _schema_projection(store: DurableStore) -> tuple[tuple[str, str, str], ...]:
    with store.connection() as connection:
        rows = connection.execute(
            "SELECT type, name, sql FROM sqlite_master "
            "WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY type, name"
        ).fetchall()
    return tuple((str(row["type"]), str(row["name"]), " ".join(str(row["sql"]).split())) for row in rows)


def test_migration_registry_is_explicit_contiguous_and_current() -> None:
    assert tuple(item.version for item in MIGRATIONS) == tuple(range(1, SCHEMA_VERSION + 1))
    assert len({item.name for item in MIGRATIONS}) == SCHEMA_VERSION


def test_new_database_records_migration_history_and_reopen_is_idempotent(tmp_path: Path) -> None:
    root = tmp_path / "new-store"
    first = DurableStore(root)
    first_schema = _schema_projection(first)
    first_history = first.migration_history()

    assert first.migration_report.detected_version == 0
    assert first.migration_report.applied == (1, 2, 3, 4)
    assert first.migration_report.verified == (1, 2, 3, 4)
    assert first.schema_version() == SCHEMA_VERSION
    assert tuple(version for version, _ in first_history) == (1, 2, 3, 4)

    second = DurableStore(root)

    assert second.migration_report.detected_version == SCHEMA_VERSION
    assert second.migration_report.applied == ()
    assert second.migration_history() == first_history
    assert _schema_projection(second) == first_schema


def test_legacy_schema_migration_preserves_evidence_rows(tmp_path: Path) -> None:
    root = tmp_path / "legacy-store"
    root.mkdir()
    database = root / "runtime.sqlite3"
    node = {
        "evidence_id": "legacy-evidence",
        "run_id": "legacy-run",
        "action_id": "legacy-action",
        "artifact_type": "surface_map",
        "target": "fixture://legacy",
        "tool": "legacy-tool",
        "payload": {"legacy": True},
        "content_hash": "a" * 64,
        "parent_ids": [],
        "verifier": "surface_map",
        "confidence": 1.0,
        "verified": True,
    }
    with sqlite3.connect(database) as connection:
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
        connection.execute(
            "INSERT INTO operations VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
            (
                "legacy-run",
                "legacy-session",
                "legacy-goal",
                "legacy-workflow",
                "completed",
                "{}",
                "2026-08-10T00:00:00+00:00",
                "2026-08-10T00:00:00+00:00",
            ),
        )
        connection.execute(
            "INSERT INTO evidence_nodes VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
            (
                "legacy-evidence",
                "legacy-run",
                "legacy-action",
                "surface_map",
                "legacy-tool",
                "a" * 64,
                json.dumps(node),
                "2026-08-10T00:00:00+00:00",
            ),
        )

    store = DurableStore(root)

    with store.connection() as connection:
        row = connection.execute(
            "SELECT evidence_id, node_json FROM evidence_nodes WHERE evidence_id='legacy-evidence'"
        ).fetchone()
        evidence_sql = connection.execute(
            "SELECT sql FROM sqlite_master WHERE type='table' AND name='evidence_nodes'"
        ).fetchone()["sql"]
        operation_columns = {item["name"] for item in connection.execute("PRAGMA table_info(operations)")}
        lease_columns = {item["name"] for item in connection.execute("PRAGMA table_info(action_leases)")}

    assert row is not None
    assert json.loads(row["node_json"])["payload"] == {"legacy": True}
    assert "UNIQUE(run_id" not in evidence_sql
    assert "version" in operation_columns
    assert "fencing_token" in lease_columns
    assert store.migration_report.applied == (1, 2, 3, 4)


def test_database_from_newer_runtime_is_not_rewritten(tmp_path: Path) -> None:
    root = tmp_path / "future-store"
    root.mkdir()
    database = root / "runtime.sqlite3"
    with sqlite3.connect(database) as connection:
        connection.execute(f"PRAGMA user_version={SCHEMA_VERSION + 1}")

    with pytest.raises(SchemaMigrationError, match="schema_version_newer_than_runtime"):
        DurableStore(root)

    with sqlite3.connect(database) as connection:
        assert int(connection.execute("PRAGMA user_version").fetchone()[0]) == SCHEMA_VERSION + 1
        tables = connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
        ).fetchall()
    assert tables == []


def test_migration_failure_rolls_back_all_schema_changes(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    root = tmp_path / "failed-store"

    def fail_migration(context: object, connection: sqlite3.Connection) -> None:
        del context, connection
        raise RuntimeError("injected_migration_failure")

    migrations = (*MIGRATIONS[:-1], Migration(4, MIGRATIONS[-1].name, fail_migration))
    monkeypatch.setattr(store_migrations, "MIGRATIONS", migrations)

    with pytest.raises(RuntimeError, match="injected_migration_failure"):
        DurableStore(root)

    with sqlite3.connect(root / "runtime.sqlite3") as connection:
        tables = connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
        ).fetchall()
        version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    assert tables == []
    assert version == 0
