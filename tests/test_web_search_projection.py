from __future__ import annotations

import json
from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.adapters.web import MAX_JSON_RESPONSE_BYTES, WebApi
from redteam_agent.adapters.web_projection import (
    MAX_SEARCH_RECORD_BYTES,
    search_graph_projection,
    search_record_projection,
)
from redteam_agent.core import ExplorationRecord, contract_hash


@pytest.fixture
def service(tmp_path: Path):
    instance = AgentService(root=tmp_path / "runtime")
    yield instance
    instance.close()


def _run(service: AgentService, name: str) -> str:
    return service.start(StartRequest(
        session_id=name, objective="Inspect fixture and provide a plan",
        targets=(f"fixture://{name}",),
    )).single.run.run_id


def _record(service: AgentService, run_id: str, record_id: str, **fields):
    return service.record_exploration(run_id, {
        "record_id": record_id, "hypothesis_id": record_id, "kind": "lead",
        "status": "active", "statement": "Inspect the fixture", **fields,
    })


def _get(api: WebApi, run_id: str):
    return api.dispatch("GET", f"/api/runs/{run_id}/search-graph")


def _bytes(value) -> int:
    return len(json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def test_search_graph_small_records_preserve_existing_contract(service: AgentService) -> None:
    run_id = _run(service, "small-search")
    parent = _record(service, run_id, "parent", observations={"status_code": 200})
    child = _record(service, run_id, "child", parent_record_ids=["parent"],
                    metadata={"source": "fixture"}, capabilities=["http_request"])
    response = _get(WebApi(service), run_id)
    payload = response.payload()
    assert response.status == 200
    assert payload["records"] == [parent.to_dict(), child.to_dict()]
    assert payload["search_graph"] == service.exploration_state(run_id)
    assert payload["records_truncated"] is False


def test_single_large_raw_observation_keeps_graph_available(service: AgentService) -> None:
    run_id = _run(service, "large-search")
    raw = {"stdout": "x" * MAX_JSON_RESPONSE_BYTES}
    _record(service, run_id, "large", observations=raw)
    response = _get(WebApi(service), run_id)
    assert response.status == 200
    node = response.payload()["records"][0]
    assert node["record_id"] == "large"
    assert node["observations"] == {"omitted": True, "content_hash": contract_hash(raw)}
    assert "observations" in node["projection_omissions"]["fields"]
    assert _bytes(node) <= MAX_SEARCH_RECORD_BYTES
    assert len(response.body) < MAX_JSON_RESPONSE_BYTES


def test_many_medium_observations_do_not_exhaust_response_budget(service: AgentService) -> None:
    run_id = _run(service, "medium-search")
    for index in range(129):
        service.runtime.store.save_exploration_record(ExplorationRecord.from_dict({
            "run_id": run_id, "record_id": f"record-{index:03}",
            "hypothesis_id": f"hypothesis-{index:03}", "kind": "lead",
            "status": "active", "statement": "Inspect the fixture",
            "observations": {"stdout": "x" * 65536},
            "created_at": f"2026-09-16T00:{index // 60:02}:{index % 60:02}Z",
        }))
    response = _get(WebApi(service), run_id)
    assert response.status == 200
    records = response.payload()["records"]
    assert len(records) == 129
    assert all(_bytes(record) <= MAX_SEARCH_RECORD_BYTES for record in records)
    assert all(record["observations"]["omitted"] for record in records)
    assert len(response.body) < MAX_JSON_RESPONSE_BYTES


def test_search_graph_keeps_latest_256_records(service: AgentService) -> None:
    run_id = _run(service, "many-search")
    for index in range(257):
        service.runtime.store.save_exploration_record(ExplorationRecord.from_dict({
            "run_id": run_id, "record_id": f"record-{index:03}",
            "hypothesis_id": f"hypothesis-{index:03}", "kind": "lead",
            "status": "active", "statement": "Inspect the fixture",
            "created_at": f"2026-09-16T00:{index // 60:02}:{index % 60:02}Z",
        }))
    response = _get(WebApi(service), run_id)
    payload = response.payload()
    assert response.status == 200
    assert len(payload["records"]) == 256
    assert payload["records"][0]["record_id"] == "record-001"
    assert payload["records"][-1]["record_id"] == "record-256"
    assert payload["records_truncated"] is True
    assert payload["search_graph"]["record_count"] == 257


def test_graph_summary_and_long_parent_references_use_same_projection(service: AgentService) -> None:
    run_id = _run(service, "long-search")
    parent_id = "parent-" + "x" * 1024
    _record(service, run_id, parent_id)
    _record(service, run_id, "child", parent_record_ids=[parent_id],
            statement="探索\n" * 10000, coverage={"raw": "x" * MAX_JSON_RESPONSE_BYTES})
    response = _get(WebApi(service), run_id)
    payload = response.payload()
    assert response.status == 200
    parent, child = payload["records"]
    assert parent["record_id"].startswith("sha256:")
    assert child["parent_record_ids"] == [parent["record_id"]]
    assert isinstance(child["statement"], str) and child["statement"].endswith("…")
    assert child["coverage"]["omitted"] is True
    assert all(_bytes(record) <= MAX_SEARCH_RECORD_BYTES for record in payload["records"])
    assert payload["search_graph"]["active"][0]["record_id"] == parent["record_id"]
    assert payload["search_graph"]["active"][1]["coverage"]["omitted"] is True
    assert len(response.body) < MAX_JSON_RESPONSE_BYTES


def test_search_graph_respects_run_branch_and_authentication_boundaries(service: AgentService) -> None:
    run_a, run_b = _run(service, "scope-a"), _run(service, "scope-b")
    _record(service, run_a, "shared-ancestor")
    branch_point = service.journal.leaf_id(run_a)
    _record(service, run_a, "main-only")
    service.fork_session(run_a, branch_point, "alternate")
    _record(service, run_a, "alternate-only")
    _record(service, run_b, "other-run")
    api = WebApi(service)
    assert [item["record_id"] for item in _get(api, run_a).payload()["records"]] == [
        "shared-ancestor", "alternate-only",
    ]
    assert [item["record_id"] for item in _get(api, run_b).payload()["records"]] == ["other-run"]
    assert _get(api, "unknown-search-run").status == 404
    api.force_auth = True
    assert _get(api, run_a).status == 401


@pytest.mark.parametrize("text", ["界" * 2000, "\x00\n\t\"\\" * 2000])
def test_record_byte_cap_includes_utf8_and_json_escaping(text: str) -> None:
    source = ExplorationRecord.from_dict({
        "record_id": text, "run_id": text, "hypothesis_id": text,
        "kind": "lead", "status": "active", "statement": text,
        "target": text, "tool": text, "action_fingerprint": text,
        "uncertainty": text, "created_at": text,
        "parent_record_ids": [text + str(i) for i in range(20)],
        "evidence_refs": [text + str(i) for i in range(20)],
        "artifact_refs": [text + str(i) for i in range(20)],
        "capabilities": [text + str(i) for i in range(20)],
        "reopen_triggers": [text + str(i) for i in range(20)],
        "observations": {"raw": text}, "metadata": {"raw": text},
        "coverage": {"raw": text}, "tested_domain": {"raw": text},
    }).to_dict()
    projected = search_record_projection(source)
    assert _bytes(projected) <= MAX_SEARCH_RECORD_BYTES
    assert isinstance(projected["statement"], str)
    assert isinstance(projected["parent_record_ids"], list)
    assert projected["projection_omissions"]["content_hash"] == contract_hash(source)
    graph = search_graph_projection({
        "authority": "navigation_only_not_evidence", "record_count": 256,
        "active": [source] * 32, "suspended": [source] * 32,
        "unresolved_contradictions": [source] * 32,
        "recent_attempts": [source] * 8, "repeated_action_signals": [source] * 40,
    })
    assert isinstance(graph["recent_attempts"], list)
    assert isinstance(graph["repeated_action_signals"], list)
    assert _bytes({"search_graph": graph, "records": [projected] * 256}) < MAX_JSON_RESPONSE_BYTES
