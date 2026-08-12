from __future__ import annotations

import json
from pathlib import Path

from scripts.phase5_snapshot import SNAPSHOT_FILE, generate_document


ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "phase5" / SNAPSHOT_FILE


def test_phase5_snapshot_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase5_snapshot_matches_fixture() -> None:
    assert generate_document() == json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_phase5_snapshot_freezes_worker_artifact_and_token_invariants() -> None:
    document = generate_document()
    assert document["schema"]["version"] == 8
    assert set(document["schema"]["tables"]) == {
        "artifact_blobs",
        "artifact_refs",
        "artifact_links",
        "artifact_fts",
        "run_workspaces",
        "worker_tasks",
    }
    assert document["vertical_fixture"]["status"] == "completed"
    assert document["vertical_fixture"]["replay_equal"] is True
    assert document["vertical_fixture"]["sqlite_result_is_bounded_projection"] is True
    assert "token_optimization_never_deletes_source_messages_or_evidence" in document["invariants"]
