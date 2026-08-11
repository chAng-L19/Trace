from __future__ import annotations

import json
from pathlib import Path

from scripts.phase3_snapshot import SNAPSHOT_FILE, generate_document


REPO_ROOT = Path(__file__).resolve().parents[1]
SNAPSHOT = REPO_ROOT / "tests" / "fixtures" / "phase3" / SNAPSHOT_FILE


def test_phase3_snapshot_generation_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase3_snapshot_matches_authoritative_fixture() -> None:
    assert generate_document() == json.loads(SNAPSHOT.read_text(encoding="utf-8"))


def test_phase3_snapshot_covers_provider_boundary_and_observation_invariant() -> None:
    document = generate_document()
    assert document["schema"]["version"] == 5
    assert set(document["schema"]["tables"]) == {
        "model_requests",
        "model_responses",
        "model_stream_events",
        "model_observations",
    }
    assert "tool_results_enter_model_observations_before_evidence" in document["invariants"]
