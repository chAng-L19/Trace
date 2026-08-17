from __future__ import annotations

import json
from pathlib import Path

from scripts.phase6_snapshot import SNAPSHOT_FILE, generate_document


ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "phase6" / SNAPSHOT_FILE


def test_phase6_snapshot_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase6_snapshot_matches_fixture() -> None:
    assert generate_document() == json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_phase6_snapshot_freezes_thin_tactical_invariants() -> None:
    document = generate_document()
    assert document["schema"]["version"] == 9
    assert set(document["schema"]["tables"]) == {
        "exploration_records",
        "recon_digests",
        "tactical_attempts",
    }
    assert document["vertical_fixture"]["hidden_route_state"] == "reopened"
    assert document["vertical_fixture"]["automatic_reopen"] is True
    assert document["vertical_fixture"]["evidence_count"] == 0
    assert "observed_miss_never_closes_a_global_direction" in document["invariants"]
