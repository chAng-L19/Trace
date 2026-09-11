from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest


def _service(tmp_path: Path) -> tuple[AgentService, str]:
    service = AgentService(root=tmp_path / "runtime")
    run_id = service.start(
        StartRequest(
            session_id="l11",
            objective="Inspect fixture://l11 and preserve complete traceability",
            targets=("fixture://l11",),
        )
    ).single.run.run_id
    return service, run_id


def test_transparency_inspect_is_bounded_and_versioned(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    report = service.inspect_session(run_id)
    assert report["schema_version"] == 1
    assert report["run"]["run"]["run_id"] == run_id
    assert report["events"]["count"] >= 1
    assert "totals" in report["model"]
    assert "visibility" in report["tools"]
    assert report["evidence"]["nodes"] == []
    assert report["report_hash"]


def test_transparency_export_retains_raw_model_records_only_when_requested(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    inspect = service.inspect_session(run_id)
    exported = service.export_transparency(run_id)
    assert inspect["events"]["items"][0]["payload"] != exported["events"]["items"][0]["payload"]
    assert exported["session"]["session"]["run_id"] == run_id


def test_tool_visibility_explain_and_context_usage_are_run_scoped(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    explanation = service.explain_tool_visibility(run_id)
    context = service.context_usage(run_id)
    assert explanation["run_id"] == run_id
    assert explanation["revision"]
    assert isinstance(explanation["visibility"], list)
    assert set(context) >= {"snapshots", "compaction_boundaries", "latest"}
    with pytest.raises(KeyError, match="operation_not_found"):
        service.inspect_session("foreign-run")


def test_evidence_lineage_rejects_missing_or_invalid_direction(tmp_path: Path) -> None:
    service, run_id = _service(tmp_path)
    with pytest.raises(ValueError, match="evidence_lineage_direction_invalid"):
        service.evidence_lineage(run_id, "missing", direction="sideways")
    with pytest.raises(KeyError, match="evidence_not_found"):
        service.evidence_lineage(run_id, "missing")
