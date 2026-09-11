from __future__ import annotations

import json
from pathlib import Path

from scripts.phase11_snapshot import SNAPSHOT_FILE, generate_document


ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "phase11" / SNAPSHOT_FILE


def test_phase11_snapshot_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase11_snapshot_matches_fixture() -> None:
    assert generate_document() == json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_phase11_cost_and_clean_target_gates() -> None:
    document = generate_document()
    metrics = document["metrics"]
    assert metrics["tool_input_token_reduction_ge_35_percent"] is True
    assert metrics["large_output_median_reduction_ge_40_percent"] is True
    assert metrics["goal_contract_completion_rate"] == 1.0
    assert metrics["clean_target_false_successes"] == 0
    assert sum(not item["expected_finding"] for item in document["cases"]) == 5
    assert all(item["projection_kind"] == "ai_friendly_tool_observation_v1" for item in document["cases"])
