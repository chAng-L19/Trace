from __future__ import annotations

import argparse
import hashlib
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

from redteam_agent.runtime.builtins import (  # noqa: E402
    _artifact_clause_ids,
    _artifact_clause_support,
    register_builtin_tools,
)
from redteam_agent.runtime.mcp_server import (  # noqa: E402
    LEGACY_TOOL_NAMES,
    PUBLIC_TOOL_NAMES,
    TOOL_DEFINITIONS_BY_NAME,
)
from redteam_agent.runtime.operation_runtime import OperationRuntime  # noqa: E402
from redteam_agent.runtime.tool_broker import ToolBroker  # noqa: E402


SNAPSHOT_FILES = (
    "runtime_identity.json",
    "mcp_tools.json",
    "sqlite_schema.json",
    "goal_contract.json",
    "operation_trace.json",
    "evidence_terminal.json",
)
TARGET = "tests/fixtures/phase0/target.txt"
OBJECTIVE = f"Give me a plan for {TARGET}; do not make changes yet and no need to run tests"
BASELINE_SCHEMA_VERSION = 4
BASELINE_SCHEMA_OBJECTS = frozenset(
    {
        "idx_attempts_run",
        "idx_evidence_run",
        "idx_facts_run",
        "idx_handoffs_pending",
        "idx_operations_session",
        "idx_reviews_run",
        "action_leases",
        "action_results",
        "evidence_nodes",
        "facts",
        "host_handoffs",
        "lease_generations",
        "operation_events",
        "operations",
        "plan_revisions",
        "reviews",
        "schema_metadata",
        "session_bindings",
        "task_attempts",
    }
)


def _canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def _deterministic_inspector(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    target_path = PROJECT_ROOT / TARGET
    content = target_path.read_bytes()
    return {
        "artifact_type": str(arguments.get("expected_artifact") or "surface_map"),
        "target": str(arguments.get("target") or TARGET),
        "confidence": 1.0,
        "clause_ids": _artifact_clause_ids(arguments, "surface_map"),
        "clause_support": _artifact_clause_support(arguments, "surface_map"),
        "files": [
            {
                "path": TARGET,
                "size": len(content),
                "sha256": hashlib.sha256(content).hexdigest(),
            }
        ],
    }


def _snapshot_broker() -> ToolBroker:
    broker = ToolBroker()
    register_builtin_tools(broker)
    broker.register_adapter(
        name="local-target-inspector",
        capabilities=("target_intake", "code_analysis", "source_inventory", "environment_inventory"),
        adapter=_deterministic_inspector,
        description="Deterministic Phase 0 target inventory fixture.",
        server="builtin",
        priority=400,
        version="phase0-fixture-v1",
    )
    return broker


def _runtime_identity() -> dict[str, Any]:
    return {
        "package": "codex-redteam-agent",
        "package_version": "0.1.0",
        "python_baseline": "3.12",
        "requires_python": ">=3.11",
        "sqlite_schema_version": BASELINE_SCHEMA_VERSION,
        "mcp_server": "redteam-agent-runtime",
        "mcp_protocol": "2025-06-18",
        "public_tool_count": len(PUBLIC_TOOL_NAMES),
    }


def _mcp_tools() -> dict[str, Any]:
    names = tuple(PUBLIC_TOOL_NAMES) + tuple(LEGACY_TOOL_NAMES)
    return {
        "public_tool_names": list(PUBLIC_TOOL_NAMES),
        "legacy_tool_names": list(LEGACY_TOOL_NAMES),
        "definitions": [TOOL_DEFINITIONS_BY_NAME[name] for name in names],
    }


def _sqlite_schema(path: Path) -> dict[str, Any]:
    runtime = OperationRuntime(root=path, broker=_snapshot_broker(), register_builtins=False)
    del runtime
    database = path / "runtime.sqlite3"
    connection = sqlite3.connect(database)
    try:
        rows = connection.execute(
            "SELECT type, name, tbl_name, sql FROM sqlite_master "
            "WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL "
            "ORDER BY type, name"
        ).fetchall()
        objects = [
            {
                "type": str(row[0]),
                "name": str(row[1]),
                "table": str(row[2]),
                "sql": " ".join(str(row[3]).split()),
            }
            for row in rows
            if str(row[1]) in BASELINE_SCHEMA_OBJECTS
        ]
    finally:
        connection.close()
    return {"user_version": BASELINE_SCHEMA_VERSION, "objects": objects}


def _goal_contract(goal: Any) -> dict[str, Any]:
    return {
        "goal_id": goal.goal_id,
        "objective": goal.objective,
        "targets": list(goal.targets),
        "workflow_hint": goal.workflow_hint,
        "workflow_hints": list(goal.workflow_hints),
        "starting_context": dict(goal.starting_context),
        "constraints": dict(goal.constraints),
        "success_criteria": [criterion.__dict__ for criterion in goal.success_criteria],
        "success_predicates": [predicate.__dict__ for predicate in goal.success_predicates],
        "intent_envelope": dict(goal.intent_envelope),
        "stop_conditions": list(goal.stop_conditions),
        "evidence_standard": goal.evidence_standard,
        "max_actions": goal.max_actions,
        "max_retries_per_action": goal.max_retries_per_action,
    }


def _state_projection(state: Any) -> dict[str, Any]:
    budget = state.budget
    return {
        "run_id": state.run_id,
        "session_id": state.session_id,
        "workflow_id": state.workflow_id,
        "workflow_version": state.workflow_version,
        "state_version": state.state_version,
        "status": state.status,
        "branch_id": state.branch_id,
        "plan_id": state.plan_id,
        "plan_revision": state.plan_revision,
        "current_action_id": state.current_action_id,
        "action_status": dict(sorted(state.action_status.items())),
        "action_attempts": dict(sorted(state.action_attempts.items())),
        "evidence_count": len(state.evidence_ids),
        "cleanup_status": state.cleanup_status,
        "terminal_reason": state.terminal_reason,
        "budget": {
            "action_limit": budget.action_limit,
            "actions_used": budget.actions_used,
            "token_limit": budget.token_limit,
            "tokens_used": budget.tokens_used,
            "time_limit_seconds": budget.time_limit_seconds,
            "pause_reason": budget.pause_reason,
            "exhausted": bool(budget.exhaustion_reason()),
        },
    }


def _event_projection(event: Mapping[str, Any]) -> dict[str, Any]:
    payload = event.get("payload") if isinstance(event.get("payload"), Mapping) else {}
    snapshot = payload.get("state_snapshot") if isinstance(payload.get("state_snapshot"), Mapping) else {}
    projected = {
        "event_id": int(event.get("event_id") or 0),
        "event_type": str(event.get("event_type") or ""),
        "state_version": int(payload.get("state_version") or 0),
        "state_status": str(snapshot.get("status") or ""),
        "action_id": str(payload.get("action_id") or snapshot.get("current_action_id") or ""),
        "payload_keys": sorted(str(key) for key in payload if key != "state_snapshot"),
    }
    return projected


def _semantic_evidence(node: Any, by_id: Mapping[str, Any]) -> dict[str, Any]:
    payload = node.payload if isinstance(node.payload, Mapping) else {}
    semantic: dict[str, Any]
    if node.artifact_type == "surface_map":
        semantic = {
            "files": payload.get("files", []),
            "clause_ids": payload.get("clause_ids", []),
            "clause_support": payload.get("clause_support", {}),
        }
    elif node.artifact_type == "hypothesis_queue":
        semantic = {
            "hypotheses": [
                {
                    key: item.get(key)
                    for key in (
                        "id",
                        "statement",
                        "priority",
                        "status",
                        "recommended_capabilities",
                        "negative_control",
                    )
                }
                for item in payload.get("hypotheses", [])
                if isinstance(item, Mapping)
            ],
            "clause_ids": payload.get("clause_ids", []),
            "clause_support": payload.get("clause_support", {}),
        }
    elif node.artifact_type == "final_report":
        semantic = {
            "goal_result": payload.get("goal_result"),
            "criteria": [
                {key: item.get(key) for key in ("criterion_id", "statement", "target", "workflow_id", "status")}
                for item in payload.get("criteria", [])
                if isinstance(item, Mapping)
            ],
            "clause_results": [
                {key: item.get(key) for key in ("clause_id", "target", "status")}
                for item in payload.get("clause_results", [])
                if isinstance(item, Mapping)
            ],
            "findings": sorted(
                (
                    {key: item.get(key) for key in ("artifact_type", "target", "result")}
                    for item in payload.get("findings", [])
                    if isinstance(item, Mapping)
                ),
                key=lambda item: (str(item.get("artifact_type") or ""), str(item.get("target") or "")),
            ),
            "report": payload.get("report", {}),
            "summary": payload.get("summary", ""),
        }
    else:
        semantic = {"payload_keys": sorted(str(key) for key in payload)}
    return {
        "action_id": node.action_id,
        "artifact_type": node.artifact_type,
        "target": node.target,
        "tool": node.tool,
        "verifier": node.verifier,
        "verified": node.verified,
        "trust": node.trust,
        "confidence": node.confidence,
        "parent_artifact_types": sorted(by_id[parent_id].artifact_type for parent_id in node.parent_ids),
        "semantic_payload": semantic,
    }


def _operation_documents(path: Path) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    runtime = OperationRuntime(root=path, broker=_snapshot_broker(), register_builtins=False)
    state = runtime.start(
        session_id="phase0-baseline",
        objective=OBJECTIVE,
        targets=(TARGET,),
        max_actions=16,
    )
    started = _state_projection(state)
    result = runtime.resume(state.run_id, max_actions=16)
    completed = _state_projection(result.state)
    events = [_event_projection(event) for event in runtime.store.events(state.run_id, limit=1000)]
    nodes = runtime.evidence_graph.list(state.run_id)
    by_id = {node.evidence_id: node for node in nodes}
    action_rank = {action.action_id: index for index, action in enumerate(result.workflow.actions)}
    ordered_nodes = sorted(
        nodes,
        key=lambda node: (action_rank.get(node.action_id, len(action_rank)), node.artifact_type, node.tool),
    )
    evidence = [_semantic_evidence(node, by_id) for node in ordered_nodes]
    trace = {"started": started, "completed": completed, "events": events}
    terminal = {
        "evidence": evidence,
        "terminal": {
            "terminal": result.terminal.terminal,
            "success": result.terminal.success,
            "reason": result.terminal.reason,
            "satisfied": list(result.terminal.satisfied),
            "missing": list(result.terminal.missing),
        },
        "missing_capabilities": list(result.missing_capabilities),
        "next_action": result.next_action,
    }
    return _goal_contract(result.state.goal), trace, terminal


def generate_documents() -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase0-") as directory:
        root = Path(directory)
        goal, trace, terminal = _operation_documents(root / "operation")
        return {
            "runtime_identity.json": _runtime_identity(),
            "mcp_tools.json": _mcp_tools(),
            "sqlite_schema.json": _sqlite_schema(root / "schema"),
            "goal_contract.json": goal,
            "operation_trace.json": trace,
            "evidence_terminal.json": terminal,
        }


def write_documents(output: Path, documents: Mapping[str, Any]) -> None:
    output.mkdir(parents=True, exist_ok=True)
    for name in SNAPSHOT_FILES:
        (output / name).write_text(_canonical_json(documents[name]), encoding="utf-8")


def check_documents(expected: Path, documents: Mapping[str, Any]) -> list[str]:
    mismatches: list[str] = []
    for name in SNAPSHOT_FILES:
        path = expected / name
        if not path.is_file():
            mismatches.append(f"missing:{name}")
            continue
        if path.read_text(encoding="utf-8") != _canonical_json(documents[name]):
            mismatches.append(f"changed:{name}")
    return mismatches


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify the deterministic Phase 0 baseline snapshots.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path, help="Write snapshots to this directory.")
    group.add_argument("--check", type=Path, help="Compare generated snapshots with this directory.")
    arguments = parser.parse_args(argv)
    documents = generate_documents()
    if arguments.write is not None:
        write_documents(arguments.write, documents)
        return 0
    mismatches = check_documents(arguments.check, documents)
    if mismatches:
        for mismatch in mismatches:
            print(mismatch)
        return 1
    print(f"phase0 snapshots verified: {len(SNAPSHOT_FILES)} files")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
