from __future__ import annotations

import sqlite3

from .store_common import _load
from .store_migrations import MigrationReport, apply_migrations, migration_history


class StoreSchemaMixin:
    @staticmethod
    def _columns(connection: sqlite3.Connection, table: str) -> set[str]:
        return {str(row["name"]) for row in connection.execute(f"PRAGMA table_info({table})").fetchall()}

    def _initialize(self) -> None:
        with self.transaction(immediate=True) as connection:
            self._migration_report = apply_migrations(self, connection)

    @property
    def migration_report(self) -> MigrationReport:
        return self._migration_report

    def schema_version(self) -> int:
        with self.connection() as connection:
            return int(connection.execute("PRAGMA user_version").fetchone()[0])

    def migration_history(self) -> tuple[tuple[int, str], ...]:
        with self.connection() as connection:
            return migration_history(connection)

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
