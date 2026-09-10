from __future__ import annotations

import argparse
import inspect
import json
import sys
import tempfile
from pathlib import Path
from typing import Any, Mapping


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent import AgentService, StartRequest  # noqa: E402
from redteam_agent.application import ContextSelector  # noqa: E402
from redteam_agent.core import WorkerTask  # noqa: E402
from redteam_agent.runtime.artifact_store import ArtifactStore  # noqa: E402
from redteam_agent.runtime.durable_store import DurableStore  # noqa: E402
from redteam_agent.workers import (  # noqa: E402
    CodexHandoffWorker,
    DockerWorkerAdapter,
    LocalWorker,
    McpWorker,
    WorkerManager,
)


SNAPSHOT_FILE = "worker_artifacts.json"
PHASE5_SCHEMA_VERSION = 8
PHASE5_TABLES = (
    "artifact_blobs",
    "artifact_refs",
    "artifact_links",
    "artifact_fts",
    "run_workspaces",
    "worker_tasks",
)


def _parameters(owner: Any, name: str) -> list[str]:
    return [item for item in inspect.signature(getattr(owner, name)).parameters if item != "self"]


def _schema() -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase5-schema-") as directory:
        store = DurableStore(Path(directory))
        with store.connection() as connection:
            tables = {
                table: [
                    {
                        "name": str(row["name"]),
                        "type": str(row["type"]),
                        "not_null": bool(row["notnull"]),
                        "primary_key": int(row["pk"]),
                    }
                    for row in connection.execute(f"PRAGMA table_info({table})").fetchall()
                ]
                for table in PHASE5_TABLES
            }
            indexes = [
                str(row["name"])
                for row in connection.execute(
                    "SELECT name FROM sqlite_master WHERE type='index' "
                    "AND (name LIKE 'idx_artifact_%' OR name LIKE 'idx_worker_%') "
                    "AND name NOT LIKE 'sqlite_%' ORDER BY name"
                ).fetchall()
            ]
    return {"version": PHASE5_SCHEMA_VERSION, "tables": tables, "indexes": indexes}


def _vertical_fixture() -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase5-run-") as directory:
        service = AgentService(root=Path(directory) / "runtime")
        run_id = service.start(
            StartRequest(
                session_id="phase5-snapshot",
                objective="Execute a local fixture and preserve complete content-addressed output",
            )
        ).single.run.run_id
        task = WorkerTask(
            task_id="phase5-local-task",
            run_id=run_id,
            capability="local.command",
            payload={"argv": [sys.executable, "-c", "print('phase5-worker')"]},
            idempotency_key="phase5-local-idempotency",
            metadata={"worker_kind": "local"},
        )
        result = service.execute_worker(task)
        refs = service.artifacts(run_id)
        return {
            "status": result.status,
            "artifact_types": sorted(item.artifact_type for item in refs),
            "artifact_count": len(refs),
            "stdout_verified": service.read_artifact(run_id, result.artifact_refs[0])
            .decode("utf-8")
            .strip(),
            "replay_equal": service.execute_worker(task) == result,
            "workspace_key_length": len(service.workspaces.ensure(run_id).workspace_key),
            "sqlite_result_is_bounded_projection": len(
                json.dumps(result.output, ensure_ascii=False).encode("utf-8")
            ) < 80_000,
        }


def generate_document() -> dict[str, Any]:
    worker_classes = (LocalWorker, McpWorker, CodexHandoffWorker, DockerWorkerAdapter)
    return {
        "schema": _schema(),
        "contracts": {
            "agent_service": {
                name: _parameters(AgentService, name)
                for name in (
                    "execute_worker",
                    "worker_status",
                    "worker_results",
                    "cancel_worker",
                    "artifact",
                    "read_artifact",
                    "artifacts",
                    "search_artifacts",
                )
            },
            "artifact_store": {
                name: _parameters(ArtifactStore, name)
                for name in (
                    "put_bytes",
                    "put_json",
                    "put_file",
                    "get_ref",
                    "read",
                    "verify",
                    "refs",
                    "search",
                    "project",
                )
            },
            "context_selector": {
                "select": _parameters(ContextSelector, "select"),
                "prepare_model_context": _parameters(ContextSelector, "prepare_model_context"),
                "metrics": [
                    "estimated_context_tokens",
                    "provider_context_tokens",
                    "selected_tokens",
                    "reserved_output_tokens",
                    "projection_bytes",
                    "cache_read_tokens",
                    "cache_write_tokens",
                    "context_overflow_tokens",
                    "compaction_ids",
                    "overflow_retry",
                ],
            },
            "worker_adapters": {
                item.__name__: {
                    name: _parameters(item, name)
                    for name in ("capabilities", "execute", "reconcile", "cancel")
                }
                for item in worker_classes
            },
            "worker_manager": {
                name: _parameters(WorkerManager, name)
                for name in ("capabilities", "execute", "reconcile", "cancel")
            },
        },
        "vertical_fixture": _vertical_fixture(),
        "invariants": [
            "complete_bytes_live_in_sha256_cas",
            "sqlite_stores_bounded_projection_and_lineage",
            "artifact_references_are_run_bound",
            "workspace_paths_and_environment_are_run_isolated",
            "worker_idempotency_is_payload_bound_and_restart_recoverable",
            "timeout_and_cancel_propagate_to_process_tree",
            "tool_call_and_tool_result_are_atomic_context_groups",
            "token_optimization_never_deletes_source_messages_or_evidence",
            "protected_context_has_precedence_over_window_fit",
            "stable_system_and_tool_prefix_supports_provider_cache",
        ],
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 5 worker/artifact snapshot.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path)
    group.add_argument("--check", type=Path)
    arguments = parser.parse_args(argv)
    document = canonical_json(generate_document())
    if arguments.write is not None:
        arguments.write.mkdir(parents=True, exist_ok=True)
        (arguments.write / SNAPSHOT_FILE).write_text(document, encoding="utf-8")
        return 0
    path = arguments.check / SNAPSHOT_FILE
    if not path.is_file():
        print(f"missing:{SNAPSHOT_FILE}")
        return 1
    if path.read_text(encoding="utf-8") != document:
        print(f"changed:{SNAPSHOT_FILE}")
        return 1
    print("phase5 worker/artifact snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
