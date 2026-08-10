from __future__ import annotations

import json
from pathlib import Path

from scripts.phase0_snapshot import SNAPSHOT_FILES, generate_documents


REPO_ROOT = Path(__file__).resolve().parents[1]
SNAPSHOT_ROOT = REPO_ROOT / "tests" / "fixtures" / "phase0" / "snapshots"


def test_phase0_snapshot_generation_is_deterministic() -> None:
    assert generate_documents() == generate_documents()


def test_phase0_snapshots_match_authoritative_fixtures() -> None:
    generated = generate_documents()
    expected = {
        name: json.loads((SNAPSHOT_ROOT / name).read_text(encoding="utf-8"))
        for name in SNAPSHOT_FILES
    }
    assert generated == expected


def test_phase0_snapshot_covers_required_contracts() -> None:
    generated = generate_documents()
    assert tuple(generated) == SNAPSHOT_FILES
    assert generated["runtime_identity.json"]["public_tool_count"] == 5
    assert generated["sqlite_schema.json"]["user_version"] >= 1
    assert generated["operation_trace.json"]["completed"]["status"] == "completed"
    assert generated["evidence_terminal.json"]["terminal"] == {
        "terminal": True,
        "success": True,
        "reason": "goal_contract_satisfied",
        "satisfied": ["workflow_actions_complete:required", "artifact_verified:final_report"],
        "missing": [],
    }
