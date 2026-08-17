from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from typing import Any, Callable

from .store_common import SCHEMA_VERSION


MigrationAction = Callable[[Any, sqlite3.Connection], None]


class SchemaMigrationError(RuntimeError):
    pass


@dataclass(frozen=True, slots=True)
class Migration:
    version: int
    name: str
    apply: MigrationAction


@dataclass(frozen=True, slots=True)
class MigrationReport:
    detected_version: int
    current_version: int
    applied: tuple[int, ...]
    verified: tuple[int, ...]


def execute_sql_script(connection: sqlite3.Connection, script: str) -> None:
    """Execute a fixed DDL script without sqlite3.executescript's implicit commit."""

    pending: list[str] = []
    for line in script.splitlines():
        pending.append(line)
        statement = "\n".join(pending).strip()
        if statement and sqlite3.complete_statement(statement):
            connection.execute(statement)
            pending.clear()
    if "\n".join(pending).strip():
        raise SchemaMigrationError("migration_sql_incomplete")


def _table_exists(connection: sqlite3.Connection, table: str) -> bool:
    row = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
        (table,),
    ).fetchone()
    return row is not None


def detected_schema_version(connection: sqlite3.Connection) -> int:
    pragma_version = int(connection.execute("PRAGMA user_version").fetchone()[0])
    metadata_version = 0
    if _table_exists(connection, "schema_metadata"):
        row = connection.execute(
            "SELECT value FROM schema_metadata WHERE key='schema_version'"
        ).fetchone()
        if row is not None:
            try:
                metadata_version = int(row[0])
            except (TypeError, ValueError, OverflowError):
                metadata_version = 0
    detected = max(pragma_version, metadata_version)
    if detected < 0:
        raise SchemaMigrationError(f"schema_version_invalid:{detected}")
    if detected > SCHEMA_VERSION:
        raise SchemaMigrationError(f"schema_version_newer_than_runtime:{detected}:{SCHEMA_VERSION}")
    return detected


def _migration_1_base(context: Any, connection: sqlite3.Connection) -> None:
    del context
    execute_sql_script(
        connection,
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


def _migration_2_cas_and_fencing(context: Any, connection: sqlite3.Connection) -> None:
    if "version" not in context._columns(connection, "operations"):
        connection.execute("ALTER TABLE operations ADD COLUMN version INTEGER NOT NULL DEFAULT 1")
    if "fencing_token" not in context._columns(connection, "action_leases"):
        connection.execute("ALTER TABLE action_leases ADD COLUMN fencing_token INTEGER NOT NULL DEFAULT 0")


def _migration_3_evidence_identity(context: Any, connection: sqlite3.Connection) -> None:
    context._migrate_evidence_table(connection)


def _migration_4_handoff_and_indexes(context: Any, connection: sqlite3.Connection) -> None:
    del context
    from .handoff import ensure_handoff_schema

    ensure_handoff_schema(connection)
    execute_sql_script(
        connection,
        """
        CREATE INDEX IF NOT EXISTS idx_evidence_run ON evidence_nodes(run_id, created_at, evidence_id);
        CREATE INDEX IF NOT EXISTS idx_attempts_run ON task_attempts(run_id, action_id, started_at);
        CREATE INDEX IF NOT EXISTS idx_facts_run ON facts(run_id, branch_id, fact_key, version);
        CREATE INDEX IF NOT EXISTS idx_reviews_run ON reviews(run_id, branch_id, plan_revision);
        """
    )


def _migration_5_model_loop_records(context: Any, connection: sqlite3.Connection) -> None:
    del context
    execute_sql_script(
        connection,
        """
        CREATE TABLE IF NOT EXISTS model_requests (
            request_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, prompt_hash TEXT NOT NULL,
            provider TEXT NOT NULL, model TEXT NOT NULL, capabilities_json TEXT NOT NULL,
            request_json TEXT NOT NULL, created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS model_responses (
            request_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, status TEXT NOT NULL,
            provider TEXT NOT NULL, model TEXT NOT NULL, response_hash TEXT NOT NULL,
            claimed_response_hash TEXT NOT NULL, usage_json TEXT NOT NULL,
            response_json TEXT NOT NULL, created_at TEXT NOT NULL,
            FOREIGN KEY(request_id) REFERENCES model_requests(request_id) ON DELETE CASCADE,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS model_stream_events (
            request_id TEXT NOT NULL, sequence INTEGER NOT NULL, event_type TEXT NOT NULL,
            event_json TEXT NOT NULL, PRIMARY KEY(request_id, sequence),
            FOREIGN KEY(request_id) REFERENCES model_requests(request_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS model_observations (
            observation_id TEXT PRIMARY KEY, request_id TEXT NOT NULL, run_id TEXT NOT NULL,
            action_id TEXT NOT NULL, call_id TEXT NOT NULL, tool_name TEXT NOT NULL,
            status TEXT NOT NULL, input_hash TEXT NOT NULL, output_hash TEXT NOT NULL,
            observation_json TEXT NOT NULL, created_at TEXT NOT NULL,
            UNIQUE(request_id, call_id),
            FOREIGN KEY(request_id) REFERENCES model_requests(request_id) ON DELETE CASCADE,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_model_requests_run
            ON model_requests(run_id, created_at, request_id);
        CREATE INDEX IF NOT EXISTS idx_model_observations_run
            ON model_observations(run_id, action_id, created_at);
        """,
    )


def _migration_6_conversation_context_budget(context: Any, connection: sqlite3.Connection) -> None:
    del context
    execute_sql_script(
        connection,
        """
        CREATE TABLE IF NOT EXISTS conversation_messages (
            message_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, sequence INTEGER NOT NULL,
            role TEXT NOT NULL, content_hash TEXT NOT NULL, protected INTEGER NOT NULL,
            source_type TEXT NOT NULL, source_id TEXT NOT NULL, message_json TEXT NOT NULL,
            created_at TEXT NOT NULL, UNIQUE(run_id, sequence),
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS context_summaries (
            summary_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, source_hash TEXT NOT NULL,
            summary_hash TEXT NOT NULL, source_ids_json TEXT NOT NULL, summary_json TEXT NOT NULL,
            created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS context_snapshots (
            snapshot_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, source_hash TEXT NOT NULL,
            protected_hash TEXT NOT NULL, context_hash TEXT NOT NULL, context_json TEXT NOT NULL,
            created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS diagnostic_artifacts (
            artifact_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, artifact_type TEXT NOT NULL,
            source_id TEXT NOT NULL, content_hash TEXT NOT NULL, artifact_json TEXT NOT NULL,
            created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS model_budget_usage (
            request_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, usage_hash TEXT NOT NULL,
            input_tokens INTEGER, output_tokens INTEGER, total_tokens INTEGER,
            usage_missing INTEGER NOT NULL, usage_json TEXT NOT NULL, created_at TEXT NOT NULL,
            FOREIGN KEY(request_id) REFERENCES model_requests(request_id) ON DELETE CASCADE,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_conversation_run
            ON conversation_messages(run_id, sequence);
        CREATE INDEX IF NOT EXISTS idx_context_summary_run
            ON context_summaries(run_id, created_at);
        CREATE INDEX IF NOT EXISTS idx_diagnostic_run
            ON diagnostic_artifacts(run_id, artifact_type, created_at);
        """,
    )


def _migration_7_artifact_cas(context: Any, connection: sqlite3.Connection) -> None:
    del context
    execute_sql_script(
        connection,
        """
        CREATE TABLE IF NOT EXISTS artifact_blobs (
            content_hash TEXT PRIMARY KEY, byte_count INTEGER NOT NULL,
            storage_key TEXT NOT NULL, created_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS artifact_refs (
            artifact_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, content_hash TEXT NOT NULL,
            byte_count INTEGER NOT NULL, media_type TEXT NOT NULL, artifact_type TEXT NOT NULL,
            storage_key TEXT NOT NULL, preview_json TEXT NOT NULL, metadata_json TEXT NOT NULL,
            artifact_json TEXT NOT NULL, created_at TEXT NOT NULL,
            UNIQUE(artifact_id, run_id),
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE,
            FOREIGN KEY(content_hash) REFERENCES artifact_blobs(content_hash)
        );
        CREATE TABLE IF NOT EXISTS artifact_links (
            artifact_id TEXT NOT NULL, parent_id TEXT NOT NULL, run_id TEXT NOT NULL,
            PRIMARY KEY(artifact_id, parent_id),
            FOREIGN KEY(artifact_id, run_id) REFERENCES artifact_refs(artifact_id, run_id) ON DELETE CASCADE,
            FOREIGN KEY(parent_id, run_id) REFERENCES artifact_refs(artifact_id, run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_artifacts_run ON artifact_refs(run_id, created_at, artifact_id);
        CREATE INDEX IF NOT EXISTS idx_artifact_links_parent ON artifact_links(run_id, parent_id);
        CREATE VIRTUAL TABLE IF NOT EXISTS artifact_fts USING fts5(
            artifact_id UNINDEXED, run_id UNINDEXED, artifact_type, preview, metadata
        );
        """,
    )


def _migration_8_worker_plane(context: Any, connection: sqlite3.Connection) -> None:
    del context
    execute_sql_script(
        connection,
        """
        CREATE TABLE IF NOT EXISTS run_workspaces (
            run_id TEXT PRIMARY KEY, workspace_key TEXT NOT NULL,
            manifest_hash TEXT NOT NULL, manifest_json TEXT NOT NULL,
            created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE TABLE IF NOT EXISTS worker_tasks (
            task_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, worker_kind TEXT NOT NULL,
            capability TEXT NOT NULL, idempotency_key TEXT NOT NULL, input_hash TEXT NOT NULL,
            status TEXT NOT NULL, task_json TEXT NOT NULL, result_json TEXT NOT NULL,
            owner TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
            UNIQUE(run_id, worker_kind, idempotency_key),
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_worker_tasks_run
            ON worker_tasks(run_id, status, created_at, task_id);
        """,
    )


def _migration_9_tactical_exploration(context: Any, connection: sqlite3.Connection) -> None:
    execute_sql_script(
        connection,
        """
        CREATE TABLE IF NOT EXISTS exploration_records (
            record_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, hypothesis_id TEXT NOT NULL,
            kind TEXT NOT NULL, status TEXT NOT NULL, record_hash TEXT NOT NULL,
            record_json TEXT NOT NULL, created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_exploration_run
            ON exploration_records(run_id, hypothesis_id, created_at, record_id);
        CREATE INDEX IF NOT EXISTS idx_exploration_fingerprint
            ON exploration_records(run_id, kind, record_hash);
        CREATE TABLE IF NOT EXISTS recon_digests (
            digest_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, source_hash TEXT NOT NULL,
            digest_hash TEXT NOT NULL, source_ids_json TEXT NOT NULL,
            digest_json TEXT NOT NULL, created_at TEXT NOT NULL,
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_recon_digest_run
            ON recon_digests(run_id, created_at, digest_id);
        CREATE TABLE IF NOT EXISTS tactical_attempts (
            attempt_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, request_id TEXT NOT NULL,
            call_id TEXT NOT NULL, lifecycle_action_id TEXT NOT NULL,
            action_fingerprint TEXT NOT NULL, status TEXT NOT NULL,
            attempt_hash TEXT NOT NULL, attempt_json TEXT NOT NULL, created_at TEXT NOT NULL,
            UNIQUE(run_id, request_id, call_id),
            FOREIGN KEY(run_id) REFERENCES operations(run_id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS idx_tactical_attempt_run
            ON tactical_attempts(run_id, action_fingerprint, created_at, attempt_id);
        """,
    )
    if "attempt_hash" not in context._columns(connection, "tactical_attempts"):
        from ..core import contract_hash
        from .store_common import _load

        connection.execute(
            "ALTER TABLE tactical_attempts ADD COLUMN attempt_hash TEXT NOT NULL DEFAULT ''"
        )
        for row in connection.execute(
            "SELECT attempt_id, attempt_json FROM tactical_attempts"
        ).fetchall():
            connection.execute(
                "UPDATE tactical_attempts SET attempt_hash=? WHERE attempt_id=?",
                (contract_hash(_load(row["attempt_json"], {})), row["attempt_id"]),
            )


MIGRATIONS = (
    Migration(1, "base_runtime_schema", _migration_1_base),
    Migration(2, "operation_cas_and_lease_fencing", _migration_2_cas_and_fencing),
    Migration(3, "evidence_identity_without_uniqueness_collapse", _migration_3_evidence_identity),
    Migration(4, "durable_handoff_and_query_indexes", _migration_4_handoff_and_indexes),
    Migration(5, "provider_agnostic_model_loop_records", _migration_5_model_loop_records),
    Migration(6, "conversation_context_and_model_budget", _migration_6_conversation_context_budget),
    Migration(7, "content_addressed_artifact_store", _migration_7_artifact_cas),
    Migration(8, "isolated_worker_plane", _migration_8_worker_plane),
    Migration(9, "thin_tactical_exploration_ledger", _migration_9_tactical_exploration),
)


def _validate_registry() -> None:
    versions = tuple(item.version for item in MIGRATIONS)
    expected = tuple(range(1, SCHEMA_VERSION + 1))
    if versions != expected:
        raise SchemaMigrationError(f"migration_registry_non_contiguous:{versions}:{expected}")
    if len({item.name for item in MIGRATIONS}) != len(MIGRATIONS):
        raise SchemaMigrationError("migration_registry_duplicate_name")


def apply_migrations(context: Any, connection: sqlite3.Connection) -> MigrationReport:
    _validate_registry()
    detected = detected_schema_version(connection)
    applied: list[int] = []
    for migration in MIGRATIONS:
        migration.apply(context, connection)
        if migration.version > detected:
            applied.append(migration.version)
        connection.execute(
            "INSERT OR IGNORE INTO schema_metadata(key, value) VALUES(?, ?)",
            (f"migration:{migration.version}", migration.name),
        )
    connection.execute(
        "INSERT INTO schema_metadata(key, value) VALUES('schema_version', ?) "
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        (str(SCHEMA_VERSION),),
    )
    connection.execute(f"PRAGMA user_version={SCHEMA_VERSION}")
    return MigrationReport(
        detected_version=detected,
        current_version=SCHEMA_VERSION,
        applied=tuple(applied),
        verified=tuple(item.version for item in MIGRATIONS),
    )


def migration_history(connection: sqlite3.Connection) -> tuple[tuple[int, str], ...]:
    if not _table_exists(connection, "schema_metadata"):
        return ()
    rows = connection.execute(
        "SELECT key, value FROM schema_metadata WHERE key LIKE 'migration:%' ORDER BY key"
    ).fetchall()
    history: list[tuple[int, str]] = []
    for row in rows:
        try:
            version = int(str(row[0]).partition(":")[2])
        except (TypeError, ValueError, OverflowError):
            continue
        history.append((version, str(row[1])))
    return tuple(sorted(history))
