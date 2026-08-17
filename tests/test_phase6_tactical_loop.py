from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Mapping

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import ModelResponse, ToolCall, ToolDefinition, ToolResult
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime import ExplorationValidationError


class TacticalToolPort:
    def __init__(self, target: str) -> None:
        self.target = target
        self.calls: list[ToolCall] = []

    def discover(self) -> tuple[ToolDefinition, ...]:
        return (
            ToolDefinition(
                qualified_name="fixture:probe",
                name="probe",
                server="fixture",
                description="Expose a previously unknown route.",
                input_schema={"type": "object"},
                capabilities=("schema_discovery", "page_fetch"),
            ),
            ToolDefinition(
                qualified_name="fixture:surface",
                name="surface",
                server="fixture",
                description="Produce a verified surface-map artifact.",
                input_schema={"type": "object"},
                capabilities=("target_intake", "page_fetch"),
            ),
        )

    def invoke(self, call: ToolCall) -> ToolResult:
        self.calls.append(call)
        output: Any
        if call.tool_name == "fixture:probe":
            output = {
                "status_code": 200,
                "headers": {"content-type": "application/json", "x-fixture": "phase6"},
                "body": {"route": "/admin/info"},
                "routes": ["/", "/admin/info"],
                "timing": {"elapsed_ms": 12},
                "coverage": {"wordlist": "fixture", "tested": 2},
            }
        else:
            output = {
                "targets": [self.target],
                "routes": ["/", "/admin/info"],
                "surface": {"source": "schema-and-probe"},
            }
        return ToolResult(
            call_id=call.call_id,
            status="success",
            tool_name=call.tool_name,
            output=output,
        )

    def reconcile(self, call: ToolCall) -> ToolResult | None:
        del call
        return None

    def cancel(self, call_id: str) -> bool:
        del call_id
        return True


def _response(
    call_id: str,
    tool: str,
    *,
    commit: bool,
    hypothesis_id: str = "hidden-admin",
) -> ModelResponse:
    return ModelResponse(
        request_id="placeholder",
        status="completed",
        provider="phase6-fixture",
        model="primary-model",
        structured_output={
            "commit_lifecycle_gate": commit,
            "tactical_update": {
                "active_hypothesis_id": hypothesis_id,
                "records": [
                    {
                        "hypothesis_id": hypothesis_id,
                        "kind": "hypothesis",
                        "status": "active",
                        "statement": "Probe for an unlisted administrative information route",
                        "reopen_triggers": ["capability:schema_discovery"],
                    }
                ],
            },
        },
        tool_calls=(
            {"id": call_id, "name": tool, "arguments": {"path": "/admin/info"}},
        ),
        usage={"input_tokens": 20, "output_tokens": 8},
        finish_reason="tool_calls",
    )


def _start(service: AgentService, *, targets: tuple[str, ...] = ()) -> str:
    return service.start(
        StartRequest(
            session_id="phase6",
            objective="Assess the fixture, discover a real path, and preserve evidence",
            targets=targets,
            max_actions=32,
        )
    ).single.run.run_id


def test_observed_miss_never_closes_a_hypothesis_or_becomes_evidence(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _start(service, targets=("fixture://phase6",))
    artifact = service.runtime.artifacts.put_json(
        {"status": 404, "path": "/admin"},
        run_id=run_id,
        artifact_type="raw_probe",
    )

    with pytest.raises(ExplorationValidationError, match="observed_miss_cannot_close"):
        service.record_exploration(
            run_id,
            {
                "record_id": "miss-closed",
                "hypothesis_id": "admin-route",
                "kind": "observed_miss",
                "status": "closed",
                "statement": "One request returned 404",
            },
        )

    saved = service.record_exploration(
        run_id,
        {
            "record_id": "miss-scoped",
            "hypothesis_id": "admin-route",
            "kind": "observed_miss",
            "status": "suspended",
            "statement": "The exact /admin request returned 404",
            "artifact_refs": [artifact.artifact_id],
            "tested_domain": {"method": "GET", "paths": ["/admin"]},
            "observations": {"status_code": 404, "response_length": 18},
            "coverage": {"paths_tested": 1, "recursive": False},
            "uncertainty": "Nested routes and alternate methods remain untested",
            "reopen_triggers": ["capability:schema_discovery", "signal:nested-route"],
        },
    )

    assert saved.status == "suspended"
    assert service.status(run_id).evidence == ()


def test_verified_negative_is_scoped_and_new_capability_reopens_it(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _start(service, targets=("fixture://phase6",))
    artifact = service.runtime.artifacts.put_json(
        {"requests": 128, "matches": 0},
        run_id=run_id,
        artifact_type="enumeration_transcript",
    )

    with pytest.raises(ExplorationValidationError, match="requires_domain_coverage"):
        service.record_exploration(
            run_id,
            {
                "record_id": "negative-invalid",
                "hypothesis_id": "hidden-route",
                "kind": "verified_negative",
                "status": "closed",
                "statement": "No hidden route exists",
            },
        )

    service.record_exploration(
        run_id,
        {
            "record_id": "negative-scoped",
            "hypothesis_id": "hidden-route",
            "kind": "verified_negative",
            "status": "closed",
            "statement": "No match in the exact tested dictionary and method set",
            "artifact_refs": [artifact.artifact_id],
            "tested_domain": {"methods": ["GET"], "wordlist": "small-fixture"},
            "observations": {"requests": 128, "matches": 0},
            "coverage": {"dictionary_entries": 128, "recursive": False},
            "confidence": 0.8,
            "uncertainty": "Generated and schema-derived paths remain outside coverage",
            "reopen_triggers": ["capability:schema_discovery"],
        },
    )
    service.record_exploration(
        run_id,
        {
            "record_id": "new-schema-capability",
            "hypothesis_id": "schema-lead",
            "kind": "lead",
            "status": "active",
            "statement": "An API schema discovery capability became available",
            "artifact_refs": [artifact.artifact_id],
            "capabilities": ["schema_discovery"],
        },
    )

    current = {item.hypothesis_id: item for item in service.exploration.current(run_id)}
    assert current["hidden-route"].kind == "reopen"
    assert current["hidden-route"].status == "reopened"
    assert current["hidden-route"].metadata["matched_triggers"] == [
        "capability:schema_discovery"
    ]


def test_model_led_tactical_loop_can_probe_without_advancing_the_quality_gate(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("phase6", encoding="utf-8")
    provider = FakeModelProvider(
        [
            _response("probe-1", "fixture:probe", commit=False),
            _response("probe-2", "fixture:probe", commit=False),
            _response("surface-1", "fixture:surface", commit=True),
        ]
    )
    tools = TacticalToolPort(str(target))
    service = AgentService(
        root=tmp_path / "runtime",
        model_port=provider,
        tool_port=tools,
        model_max_turns=3,
    )
    run_id = _start(service, targets=(str(target),))

    view = service.run(run_id)

    assert [item.tool_name for item in tools.calls] == [
        "fixture:probe",
        "fixture:probe",
        "fixture:surface",
    ]
    assert view.goal.targets == (str(target),)
    assert len(service.runtime.store.tactical_attempts(run_id)) == 3
    repeated = service.exploration.repeated_actions(run_id)
    assert len(repeated) == 1
    assert repeated[0]["count"] == 2
    assert view.terminal.terminal is False
    assert all(item.kind != "verified_negative" for item in service.exploration_records(run_id))


def test_tool_projection_is_bounded_and_complete_raw_result_is_retrievable(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _start(service, targets=("fixture://phase6",))
    result = ToolResult(
        call_id="http-call",
        status="success",
        tool_name="fixture:http",
        output={
            "request": {"method": "GET", "url": "https://fixture/api/items"},
            "status_code": 403,
            "headers": {"content-type": "application/json", "x-authz": "denied"},
            "body": "x" * 100_000,
            "elapsed_ms": 31,
            "baseline": {"status": 403, "length": 20},
            "observed": {"status": 200, "length": 400},
            "routes": [f"/route/{index}" for index in range(100)],
            "coverage": {"source": "schema", "complete": True},
        },
        input_hash="a" * 64,
        output_hash="b" * 64,
    )

    artifact_ids = service.conversation.record_tool_results(
        "phase6-http-request", run_id, (result,)
    )
    message = service.transcript(run_id)[-1]
    artifact_id = artifact_ids[result.call_id]
    raw = service.runtime.artifacts.read_json(artifact_id, run_id=run_id)

    assert raw["output"] == result.output
    assert message.content["projection"]["status_code"] == 403
    assert message.content["projection"]["body"]["byte_count"] == 100_000
    assert message.content["projection"]["comparison"]["different"] is True
    assert message.content["projection"]["enumeration"]["routes"]["count"] == 100
    assert message.content["raw"]["artifact"]["artifact_ref"] == artifact_id
    assert len(json.dumps(message.content, ensure_ascii=False).encode("utf-8")) < 32_000


def test_context_degradation_builds_traceable_recon_digest_and_recovers(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    service = AgentService(root=root)
    run_id = _start(service, targets=("fixture://phase6",))
    service.record_exploration(
        run_id,
        {
            "record_id": "digest-hypothesis",
            "hypothesis_id": "digest-hypothesis",
            "kind": "hypothesis",
            "status": "active",
            "statement": "Inspect a route exposed by schema metadata",
            "reopen_triggers": ["capability:schema_discovery"],
        },
    )
    for index in range(55):
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"note": index, "raw_reference": f"artifact-{index}"},
            protected=False,
            source_type="phase6-pressure",
            source_id=str(index),
        )

    service.select_context(run_id, max_messages=2)
    digests = service.recon_digests(run_id)

    assert len(digests) == 1
    assert digests[0].source_message_ids
    assert digests[0].digest["authority"] == "navigation_only_sources_remain_authoritative"
    assert digests[0].digest["unverified_hypotheses"][0]["hypothesis_id"] == "digest-hypothesis"

    recovered = AgentService(root=root)
    assert recovered.recon_digests(run_id) == digests
    assert recovered.exploration_state(run_id) == service.exploration_state(run_id)
