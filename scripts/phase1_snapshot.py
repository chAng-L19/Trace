from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Mapping


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.core import (  # noqa: E402
    Asset,
    AttackPath,
    Budget,
    Event,
    Evidence,
    EvidenceProvenance,
    Finding,
    Goal,
    GoalCriterion,
    Intent,
    ModelCapabilities,
    ModelRequest,
    ModelResponse,
    ModelStreamEvent,
    Run,
    SearchNode,
    TerminalDecision,
    ToolCall,
    ToolDefinition,
    ToolResult,
    WorkerResult,
    WorkerTask,
)


SNAPSHOT_FILE = "core_contracts.json"


def sample_contracts() -> tuple[Any, ...]:
    criterion = GoalCriterion(
        criterion_id="criterion-1",
        statement="Produce verified reproduction evidence",
        target="fixture://web-api",
        required_artifacts=("reproduction_artifact",),
        metadata={"workflow_id": "generic-adaptive"},
    )
    goal = Goal(
        goal_id="goal-1",
        objective="Assess the Web API fixture and prove one attack path",
        targets=("fixture://web-api",),
        criteria=(criterion,),
        constraints={"scope": ["fixture://web-api"]},
        success_predicates=(
            {"kind": "artifact_verified", "subject": "reproduction_artifact"},
        ),
        max_actions=32,
        metadata={"workflow_hint": "generic-adaptive"},
    )
    intent = Intent(
        intent_id="intent-1",
        goal_id=goal.goal_id,
        statement="Test the highest-value authorization boundary",
        intent_type="controlled_validation",
        status="active",
        priority=90,
        target=goal.targets[0],
        required_evidence=("baseline", "request_diff", "negative_control"),
    )
    budget = Budget(
        action_limit=32,
        token_limit=100000,
        time_limit_seconds=1800.0,
        actions_used=4,
        input_tokens_used=1200,
        output_tokens_used=800,
        started_at="2026-08-10T00:00:00+00:00",
    )
    run = Run(
        run_id="run-1",
        session_id="session-1",
        goal_id=goal.goal_id,
        status="running",
        state_version=7,
        current_intent_id=intent.intent_id,
        current_search_node_id="search-1",
        budget=budget,
        evidence_ids=("evidence-1",),
        created_at="2026-08-10T00:00:00+00:00",
        updated_at="2026-08-10T00:05:00+00:00",
        metadata={"workflow_id": "generic-adaptive"},
    )
    search = SearchNode(
        node_id="search-1",
        run_id=run.run_id,
        intent_id=intent.intent_id,
        statement="Compare owner and non-owner object access",
        node_type="hypothesis",
        status="active",
        priority=90,
        target=goal.targets[0],
        required_capabilities=("controlled_validation", "browser_automation"),
        evidence_refs=("evidence-1",),
    )
    provenance = EvidenceProvenance(
        run_id=run.run_id,
        branch_id="main",
        plan_revision=2,
        action_id="validate-path",
        attempt_id="attempt-1",
        tool="fixture:http-client",
        tool_version="1.0",
        input_hash="a" * 64,
        output_hash="b" * 64,
        target=goal.targets[0],
        parent_ids=("evidence-1",),
    )
    evidence = Evidence(
        evidence_id="evidence-2",
        run_id=run.run_id,
        artifact_type="reproduction_artifact",
        target=goal.targets[0],
        action_id="validate-path",
        tool="fixture:http-client",
        payload={"baseline": 403, "observed": 200},
        content_hash="c" * 64,
        parent_ids=("evidence-1",),
        verifier="reproduction_artifact",
        confidence=0.95,
        verified=True,
        trust="runtime_verified",
        provenance=provenance,
        created_at="2026-08-10T00:04:00+00:00",
    )
    finding = Finding(
        finding_id="finding-1",
        run_id=run.run_id,
        title="Cross-account object access",
        severity="high",
        status="verified",
        target=goal.targets[0],
        asset_ids=("asset-1",),
        reproduction_evidence_ids=(evidence.evidence_id,),
        impact_evidence_ids=("evidence-3",),
        negative_control_evidence_ids=("evidence-4",),
        cleanup_evidence_ids=("evidence-5",),
    )
    asset = Asset(
        asset_id="asset-1",
        run_id=run.run_id,
        asset_type="api_endpoint",
        name="GET /objects/{id}",
        target=goal.targets[0],
        evidence_ids=("evidence-1",),
        attributes={"method": "GET", "authenticated": True},
    )
    attack_path = AttackPath(
        path_id="path-1",
        run_id=run.run_id,
        title="User token to foreign object",
        status="verified",
        asset_ids=(asset.asset_id,),
        finding_ids=(finding.finding_id,),
        evidence_ids=(evidence.evidence_id, "evidence-3"),
        impact="Read another account's object",
    )
    terminal = TerminalDecision(
        terminal=False,
        success=False,
        reason="impact_and_cleanup_missing",
        satisfied=("artifact_verified:reproduction_artifact",),
        missing=("artifact_verified:impact_proof", "artifact_verified:cleanup_proof"),
        evidence_refs=(evidence.evidence_id,),
    )
    model_capabilities = ModelCapabilities(
        parallel_tool_calls=True,
        structured_output=True,
        streaming=True,
        usage_reporting=True,
        max_context_tokens=200000,
        modalities=("text", "image"),
    )
    model_request = ModelRequest(
        request_id="model-request-1",
        run_id=run.run_id,
        messages=({"role": "user", "content": goal.objective},),
        tools=({"name": "http_request", "inputSchema": {"type": "object"}},),
        response_schema={"type": "object"},
        model="primary-model",
        allow_parallel_tools=True,
    )
    model_response = ModelResponse(
        request_id=model_request.request_id,
        status="completed",
        provider="fixture",
        model=model_request.model,
        text="Validate the authorization hypothesis.",
        structured_output={"next": "tool_call"},
        tool_calls=({"id": "call-1", "name": "http_request", "arguments": {}},),
        usage={"input_tokens": 100, "output_tokens": 50},
        finish_reason="tool_calls",
        response_hash="d" * 64,
    )
    stream_event = ModelStreamEvent(
        request_id=model_request.request_id,
        sequence=1,
        event_type="text_delta",
        payload={"text": "Validate"},
    )
    tool_definition = ToolDefinition(
        qualified_name="fixture:http-request",
        name="http-request",
        server="fixture",
        description="Execute an HTTP request against the fixture.",
        input_schema={"type": "object"},
        capabilities=("page_fetch", "controlled_validation"),
        version="1.0",
        side_effecting=False,
    )
    tool_call = ToolCall(
        call_id="call-1",
        run_id=run.run_id,
        tool_name=tool_definition.qualified_name,
        arguments={"method": "GET", "url": "fixture://web-api/objects/2"},
        idempotency_key="idempotency-1",
        timeout_seconds=30.0,
    )
    tool_result = ToolResult(
        call_id=tool_call.call_id,
        status="success",
        tool_name=tool_call.tool_name,
        output={"status": 200},
        input_hash="e" * 64,
        output_hash="f" * 64,
    )
    worker_task = WorkerTask(
        task_id="worker-task-1",
        run_id=run.run_id,
        capability="controlled_validation",
        payload={"tool_call": tool_call.to_dict()},
        idempotency_key="worker-idempotency-1",
        timeout_seconds=60.0,
        required_artifacts=("reproduction_artifact",),
    )
    worker_result = WorkerResult(
        task_id=worker_task.task_id,
        status="completed",
        output={"observation": tool_result.to_dict()},
        artifact_refs=(evidence.evidence_id,),
    )
    event = Event(
        run_id=run.run_id,
        event_type="observation_recorded",
        payload={"evidence_id": evidence.evidence_id},
        sequence=12,
        created_at="2026-08-10T00:04:00+00:00",
    )
    return (
        criterion,
        goal,
        intent,
        budget,
        run,
        terminal,
        search,
        provenance,
        evidence,
        finding,
        asset,
        attack_path,
        model_capabilities,
        model_request,
        model_response,
        stream_event,
        tool_definition,
        tool_call,
        tool_result,
        worker_task,
        worker_result,
        event,
    )


def generate_document() -> dict[str, Any]:
    contracts = sample_contracts()
    return {
        "core_schema_version": 1,
        "contracts": {
            item.KIND: item.to_dict()
            for item in sorted(contracts, key=lambda contract: contract.KIND)
        },
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 1 core contract snapshots.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path)
    group.add_argument("--check", type=Path)
    arguments = parser.parse_args(argv)
    document = generate_document()
    if arguments.write is not None:
        arguments.write.mkdir(parents=True, exist_ok=True)
        (arguments.write / SNAPSHOT_FILE).write_text(canonical_json(document), encoding="utf-8")
        return 0
    path = arguments.check / SNAPSHOT_FILE
    if not path.is_file():
        print(f"missing:{SNAPSHOT_FILE}")
        return 1
    if path.read_text(encoding="utf-8") != canonical_json(document):
        print(f"changed:{SNAPSHOT_FILE}")
        return 1
    print("phase1 core contract snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
