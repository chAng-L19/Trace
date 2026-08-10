from __future__ import annotations

import sqlite3

from .store_common import SCHEMA_VERSION, _load


class StoreSchemaMixin:
    @staticmethod
    def _columns(connection: sqlite3.Connection, table: str) -> set[str]:
        return {str(row["name"]) for row in connection.execute(f"PRAGMA table_info({table})").fetchall()}

    def _initialize(self) -> None:
        with self.transaction(immediate=True) as connection:
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS operations (
                    run_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, goal_id TEXT NOT NULL,
                    workflow_id TEXT NOT NULL, status TEXT NOT NULL, state_json TEXT NOT NULL,
                    version INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_operations_session ON operations(session_id, updated_at DESC);
                CREATE TABLE IF NOT EXISTS operation_events (
                    event_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL,
                    event_type TEXT NOT NULL, payload_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS action_leases (
                    run_id TEXT NOT NULL, action_id TEXT NOT NULL, owner TEXT NOT NULL,
                    fencing_token INTEGER NOT NULL DEFAULT 0, expires_at REAL NOT NULL,
                    PRIMARY KEY(run_id, action_id),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS lease_generations (
                    run_id TEXT NOT NULL, action_id TEXT NOT NULL, generation INTEGER NOT NULL,
                    PRIMARY KEY(run_id, action_id),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS action_results (
                    run_id TEXT NOT NULL, action_id TEXT NOT NULL, idempotency_key TEXT NOT NULL,
                    result_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    PRIMARY KEY(run_id, idempotency_key),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS evidence_nodes (
                    evidence_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, action_id TEXT NOT NULL,
                    artifact_type TEXT NOT NULL, tool TEXT NOT NULL, content_hash TEXT NOT NULL,
                    node_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS plan_revisions (
                    run_id TEXT NOT NULL, plan_id TEXT NOT NULL, branch_id TEXT NOT NULL,
                    revision INTEGER NOT NULL, parent_revision INTEGER NOT NULL, plan_hash TEXT NOT NULL,
                    plan_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    PRIMARY KEY(run_id, plan_id, branch_id, revision),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS task_attempts (
                    attempt_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, branch_id TEXT NOT NULL,
                    plan_revision INTEGER NOT NULL, action_id TEXT NOT NULL, tool TEXT NOT NULL,
                    tool_version TEXT NOT NULL, input_hash TEXT NOT NULL, idempotency_key TEXT NOT NULL,
                    status TEXT NOT NULL, fencing_token INTEGER NOT NULL, attempt_json TEXT NOT NULL,
                    started_at TEXT NOT NULL, finished_at TEXT NOT NULL,
                    UNIQUE(run_id, branch_id, plan_revision, idempotency_key),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS facts (
                    fact_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, branch_id TEXT NOT NULL,
                    fact_key TEXT NOT NULL, version INTEGER NOT NULL, valid INTEGER NOT NULL,
                    fact_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    UNIQUE(run_id, branch_id, fact_key, version),
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS reviews (
                    review_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, branch_id TEXT NOT NULL,
                    plan_revision INTEGER NOT NULL, scope TEXT NOT NULL, subject_id TEXT NOT NULL,
                    decision TEXT NOT NULL, review_json TEXT NOT NULL, created_at TEXT NOT NULL,
                    FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
                );
                CREATE TABLE IF NOT EXISTS session_bindings (
                    session_id TEXT PRIMARY KEY, run_id TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL,
                    binding_json TEXT NOT NULL, updated_at TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS schema_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
                """
            )
            if "version" not in self._columns(connection, "operations"):
                connection.execute("ALTER TABLE operations ADD COLUMN version INTEGER NOT NULL DEFAULT 1")
            if "fencing_token" not in self._columns(connection, "action_leases"):
                connection.execute("ALTER TABLE action_leases ADD COLUMN fencing_token INTEGER NOT NULL DEFAULT 0")
            self._migrate_evidence_table(connection)
            from .handoff import ensure_handoff_schema
            ensure_handoff_schema(connection)
            connection.executescript(
                """
                CREATE INDEX IF NOT EXISTS idx_evidence_run ON evidence_nodes(run_id, created_at, evidence_id);
                CREATE INDEX IF NOT EXISTS idx_attempts_run ON task_attempts(run_id, action_id, started_at);
                CREATE INDEX IF NOT EXISTS idx_facts_run ON facts(run_id, branch_id, fact_key, version);
                CREATE INDEX IF NOT EXISTS idx_reviews_run ON reviews(run_id, branch_id, plan_revision);
                """
            )
            connection.execute(
                "INSERT INTO schema_metadata(key, value) VALUES('schema_version', ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (str(SCHEMA_VERSION),),
            )
            connection.execute(f"PRAGMA user_version={SCHEMA_VERSION}")

    def _migrate_evidence_table(self, connection: sqlite3.Connection) -> None:
        row = connection.execute(
            "SELECT sql FROM sqlite_master WHERE type='table' AND name='evidence_nodes'"
        ).fetchone()
        table_sql = str(row["sql"] or "") if row else ""
        normalized = "".join(table_sql.casefold().split())
        columns = self._columns(connection, "evidence_nodes")
        if "unique(run_id,action_id,artifact_type,tool,content_hash)" not in normalized and "tool" in columns:
            return
        legacy = "evidence_nodes_schema2"
        connection.execute(f"DROP TABLE IF EXISTS {legacy}")
        connection.execute(f"ALTER TABLE evidence_nodes RENAME TO {legacy}")
        connection.execute(
            """
            CREATE TABLE evidence_nodes (
                evidence_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, action_id TEXT NOT NULL,
                artifact_type TEXT NOT NULL, tool TEXT NOT NULL, content_hash TEXT NOT NULL,
                node_json TEXT NOT NULL, created_at TEXT NOT NULL,
                FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
            )
            """
        )
        legacy_columns = self._columns(connection, legacy)
        for legacy_row in connection.execute(f"SELECT * FROM {legacy}").fetchall():
            payload = _load(legacy_row["node_json"], {})
            tool = str(legacy_row["tool"]) if "tool" in legacy_columns else str((payload or {}).get("tool") or "legacy")
            connection.execute(
                "INSERT OR IGNORE INTO evidence_nodes VALUES(?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    legacy_row["evidence_id"], legacy_row["run_id"], legacy_row["action_id"],
                    legacy_row["artifact_type"], tool, legacy_row["content_hash"],
                    legacy_row["node_json"], legacy_row["created_at"],
                ),
            )
        connection.execute(f"DROP TABLE {legacy}")

