from __future__ import annotations

import json
from pathlib import Path

from scripts.phase2_snapshot import SNAPSHOT_FILE, generate_document


REPO_ROOT = Path(__file__).resolve().parents[1]
SNAPSHOT = REPO_ROOT / "tests" / "fixtures" / "phase2" / SNAPSHOT_FILE


def test_phase2_snapshot_generation_is_deterministic() -> None:
    assert generate_document() == generate_document()


def test_phase2_snapshot_matches_authoritative_fixture() -> None:
    assert generate_document() == json.loads(SNAPSHOT.read_text(encoding="utf-8"))


def test_phase2_snapshot_covers_canonical_service_and_lifecycle() -> None:
    document = generate_document()
    assert tuple(document["service"]["methods"]) == (
        "start",
        "run",
        "submit_observation",
        "status",
        "cancel",
        "events",
    )
    assert document["lifecycle"]["statuses"] == [
        "cancelled",
        "cancelling",
        "completed",
        "created",
        "failed",
        "paused_budget",
        "running",
        "waiting_worker",
    ]
