from __future__ import annotations

import argparse
import inspect
import json
import sqlite3
import sys
import tempfile
from pathlib import Path
from typing import Any, Mapping

PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.application import AgentService, ContextSelector, TraceableCompactor  # noqa: E402
from redteam_agent.application.context import ConversationLedger  # noqa: E402
from redteam_agent.application.contracts import BudgetDelta  # noqa: E402
from redteam_agent.core import contract_hash  # noqa: E402
from redteam_agent.runtime.durable_store import DurableStore  # noqa: E402
SNAPSHOT_FILE = "context_budget.json"
PHASE4_SCHEMA_VERSION = 6
PHASE4_TABLES = (
    "conversation_messages",
    "context_summaries",
    "context_snapshots",
    "diagnostic_artifacts",
    "model_budget_usage",
)


def _parameters(owner: Any, name: str) -> list[str]:
    return [p for p in inspect.signature(getattr(owner, name)).parameters if p != "self"]


def _schema() -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase4-") as directory:
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
                for table in PHASE4_TABLES
            }
            indexes = [
                str(row["name"])
                for row in connection.execute(
                    "SELECT name FROM sqlite_master WHERE type='index' "
                    "AND (name LIKE 'idx_conversation_%' OR name LIKE 'idx_context_%' "
                    "OR name LIKE 'idx_diagnostic_%' OR tbl_name='model_budget_usage') "
                    "AND name NOT LIKE 'sqlite_%' ORDER BY name"
                ).fetchall()
            ]
    return {"version": PHASE4_SCHEMA_VERSION, "tables": tables, "indexes": indexes}


def generate_document() -> dict[str, Any]:
    delta = BudgetDelta(
        actions=2,
        tokens=128,
        time_seconds=30.0,
        deadline="2026-08-11T00:00:00+00:00",
        idempotency_key="fixture-budget-extension",
        acknowledge_missing_usage=True,
    )
    return {
        "contracts": {
            "agent_service": {
                name: _parameters(AgentService, name)
                for name in ("start", "run", "submit_observation", "status", "cancel", "events")
            },
            "conversation": {
                "append": _parameters(ConversationLedger, "append"),
                "compact": _parameters(TraceableCompactor, "compact"),
                "select": ["view", "max_messages"],
            },
            "budget_delta": delta.to_dict(),
        },
        "schema": _schema(),
        "record_hashes": {
            "protected_context_keys": [
                "original_goal",
                "unsatisfied_clauses",
                "active_plan",
                "critical_evidence_refs",
                "irreversible_state",
            ],
            "hash_algorithm": "sha256",
            "content_hash_contract": "contract_hash(canonical_json(value))",
        },
        "invariants": [
            "conversation_transcript_is_append_only",
            "compaction_adds_traceable_source_hash_summary",
            "protected_context_survives_any_selection_limit",
            "unknown_token_usage_is_never_fabricated",
            "budget_exhaustion_is_recoverable_paused_state",
            "partial_stream_is_diagnostic_artifact_only",
            "context_snapshots_are_immutable",
        ],
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 4 snapshot.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path)
    group.add_argument("--check", type=Path)
    args = parser.parse_args(argv)
    document = canonical_json(generate_document())
    if args.write is not None:
        args.write.mkdir(parents=True, exist_ok=True)
        (args.write / SNAPSHOT_FILE).write_text(document, encoding="utf-8")
        return 0
    path = args.check / SNAPSHOT_FILE
    if not path.is_file():
        print(f"missing:{SNAPSHOT_FILE}")
        return 1
    if path.read_text(encoding="utf-8") != document:
        print(f"changed:{SNAPSHOT_FILE}")
        return 1
    print("phase4 context/budget snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
