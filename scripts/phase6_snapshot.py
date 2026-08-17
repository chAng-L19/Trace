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
from redteam_agent.application import ToolObservationProjector  # noqa: E402
from redteam_agent.core import ExplorationRecord, ToolResult  # noqa: E402
from redteam_agent.runtime import ExplorationLedger  # noqa: E402
from redteam_agent.runtime.durable_store import DurableStore  # noqa: E402
from redteam_agent.runtime.store_common import SCHEMA_VERSION  # noqa: E402


SNAPSHOT_FILE = "thin_tactical_loop.json"
PHASE6_TABLES = ("exploration_records", "recon_digests", "tactical_attempts")


def _parameters(owner: Any, name: str) -> list[str]:
    return [item for item in inspect.signature(getattr(owner, name)).parameters if item != "self"]


def _schema() -> Mapping[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase6-schema-") as directory:
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
                for table in PHASE6_TABLES
            }
            indexes = [
                str(row["name"])
                for row in connection.execute(
                    "SELECT name FROM sqlite_master WHERE type='index' "
                    "AND name LIKE 'idx_%' AND (name LIKE '%exploration%' "
                    "OR name LIKE '%recon_digest%' OR name LIKE '%tactical_attempt%') "
                    "ORDER BY name"
                ).fetchall()
            ]
    return {"version": SCHEMA_VERSION, "tables": tables, "indexes": indexes}


def _contract() -> Mapping[str, Any]:
    return ExplorationRecord(
        record_id="exploration-phase6",
        run_id="run-phase6",
        hypothesis_id="hidden-route",
        kind="observed_miss",
        status="suspended",
        statement="The exact GET /admin request returned 404",
        target="fixture://phase6",
        artifact_refs=("artifact-phase6",),
        tested_domain={"method": "GET", "paths": ["/admin"]},
        observations={"status_code": 404},
        coverage={"paths_tested": 1, "recursive": False},
        uncertainty="Nested and generated routes remain untested",
        reopen_triggers=("capability:schema_discovery",),
        created_at="2026-08-17T00:00:00+00:00",
    ).to_dict()


def _tool_projection() -> Mapping[str, Any]:
    projection = ToolObservationProjector().project(
        ToolResult(
            call_id="phase6-http",
            status="success",
            tool_name="fixture:http",
            output={
                "status_code": 403,
                "headers": {"content-type": "application/json"},
                "body": "denied",
                "baseline": {"status": 403},
                "observed": {"status": 200},
                "routes": ["/", "/admin/info"],
                "coverage": {"source": "schema", "complete": True},
            },
            input_hash="a" * 64,
            output_hash="b" * 64,
        ),
        raw_artifact={
            "artifact_ref": "artifact-http",
            "content_hash": "c" * 64,
            "byte_count": 512,
            "artifact_type": "model_tool_result",
            "media_type": "application/json",
        },
    )
    return {
        "content": dict(projection.content),
        "raw_bytes": projection.raw_bytes,
        "semantic_fields": list(projection.semantic_fields),
    }


def _vertical_fixture() -> Mapping[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase6-run-") as directory:
        service = AgentService(root=Path(directory) / "runtime")
        run_id = service.start(
            StartRequest(
                session_id="phase6-snapshot",
                objective="Assess fixture://phase6 and preserve uncertainty",
                targets=("fixture://phase6",),
            )
        ).single.run.run_id
        artifact = service.runtime.artifacts.put_json(
            {"requests": 32, "matches": 0},
            run_id=run_id,
            artifact_type="enumeration_transcript",
        )
        service.record_exploration(
            run_id,
            {
                "record_id": "negative-scoped",
                "hypothesis_id": "hidden-route",
                "kind": "verified_negative",
                "status": "closed",
                "statement": "No match in the exact tested input set",
                "artifact_refs": [artifact.artifact_id],
                "tested_domain": {"method": "GET", "entries": 32},
                "observations": {"matches": 0},
                "coverage": {"entries": 32, "recursive": False},
                "confidence": 0.8,
                "uncertainty": "Generated paths remain untested",
                "reopen_triggers": ["capability:schema_discovery"],
            },
        )
        service.record_exploration(
            run_id,
            {
                "record_id": "capability-arrived",
                "hypothesis_id": "schema-lead",
                "kind": "lead",
                "status": "active",
                "statement": "Schema discovery became available",
                "artifact_refs": [artifact.artifact_id],
                "capabilities": ["schema_discovery"],
            },
        )
        current = {item.hypothesis_id: item for item in service.exploration.current(run_id)}
        digest = service.recon_digest(run_id)
        return {
            "record_count": len(service.exploration_records(run_id)),
            "hidden_route_state": current["hidden-route"].status,
            "hidden_route_kind": current["hidden-route"].kind,
            "automatic_reopen": bool(current["hidden-route"].metadata.get("automatic")),
            "evidence_count": len(service.status(run_id).evidence),
            "digest_authority": digest.digest["authority"],
            "digest_has_reopen_trigger": bool(digest.digest["reopen_triggers"]),
        }


def generate_document() -> dict[str, Any]:
    return {
        "schema": _schema(),
        "contracts": {
            "exploration_record": _contract(),
            "agent_service": {
                name: _parameters(AgentService, name)
                for name in (
                    "record_exploration",
                    "exploration_records",
                    "exploration_state",
                    "recon_digest",
                    "recon_digests",
                )
            },
            "exploration_ledger": {
                name: _parameters(ExplorationLedger, name)
                for name in (
                    "record",
                    "record_model_update",
                    "current",
                    "projection",
                    "repeated_actions",
                    "build_recon_digest",
                )
            },
            "tool_projector": {"project": _parameters(ToolObservationProjector, "project")},
        },
        "tool_projection": _tool_projection(),
        "vertical_fixture": _vertical_fixture(),
        "invariants": [
            "model_owns_tactical_node_generation_and_priority",
            "runtime_quality_gate_does_not_prescribe_tool_actions",
            "observed_miss_never_closes_a_global_direction",
            "verified_negative_requires_scoped_coverage_sources_and_reopen_triggers",
            "new_capability_or_evidence_can_reopen_a_branch",
            "exploration_records_never_promote_themselves_to_evidence",
            "complete_tool_results_live_in_cas_with_bounded_model_projections",
            "repeated_actions_emit_diagnostics_without_forced_termination",
            "recon_digest_is_a_traceable_navigation_projection",
        ],
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 6 tactical-loop snapshot.")
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
    print("phase6 thin tactical-loop snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
