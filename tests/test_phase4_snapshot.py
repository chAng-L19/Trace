from __future__ import annotations

import json
from pathlib import Path

from scripts.phase4_snapshot import SNAPSHOT_FILE, generate_document

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "phase4" / SNAPSHOT_FILE


def test_phase4_snapshot_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase4_snapshot_matches_fixture() -> None:
    assert generate_document() == json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_phase4_snapshot_freezes_budget_and_context_invariants() -> None:
    document = generate_document()
    assert document["schema"]["version"] == 6
    assert set(document["schema"]["tables"]) == {
        "conversation_messages",
        "context_summaries",
        "context_snapshots",
        "diagnostic_artifacts",
        "model_budget_usage",
    }
    assert "unknown_token_usage_is_never_fabricated" in document["invariants"]
