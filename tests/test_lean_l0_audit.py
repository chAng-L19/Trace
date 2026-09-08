from __future__ import annotations

from scripts.lean_l0_audit import build_report, canonical_json


def test_l0_audit_is_deterministic() -> None:
    first = build_report()
    second = build_report()
    assert canonical_json(first) == canonical_json(second)


def test_l0_audit_freezes_complexity_and_candidate_references() -> None:
    report = build_report()
    metrics = report["metrics"]
    assert metrics["production_files"] > 0
    assert metrics["production_lines"] > 0
    assert all(item["lines"] <= 800 for item in metrics["files_over_800_lines"])
    candidates = report["deletion_candidates"]
    assert set(candidates) >= {
        "AdaptivePlanner",
        "NextActionPolicy",
        "WorkflowRegistry",
    }
