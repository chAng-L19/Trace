from __future__ import annotations

import sys
from pathlib import Path


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.operation_runtime import OperationRuntime


def test_status_does_not_report_completed_when_terminal_evidence_is_missing(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="terminal-integrity",
        objective="Inspect https://target.invalid and write the report",
    )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    persisted.status = "completed"
    persisted.terminal_reason = "goal_contract_satisfied"
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    observed = runtime.status(state.run_id)

    assert observed.state.status == "failed_integrity"
    assert observed.terminal.terminal is True
    assert observed.terminal.success is False
    assert observed.terminal.reason == "terminal_evidence_integrity_failed"
    stored = runtime.store.load_operation(state.run_id)
    assert stored is not None and stored.status == "completed"

    resumed = runtime.resume(state.run_id)

    assert resumed.state.status == "failed_integrity"
    assert resumed.terminal.reason == "terminal_evidence_integrity_failed"


def test_plan_row_hash_column_tampering_is_rejected_by_status_and_resume(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="plan-row-integrity",
        objective="Inspect https://target.invalid and write the report",
    )
    with runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE plan_revisions SET plan_hash=? WHERE run_id=? AND revision=1",
            ("0" * 64, state.run_id),
        )

    observed = runtime.status(state.run_id)

    assert observed.state.status == "failed_integrity"
    assert observed.terminal.terminal is True
    assert observed.terminal.success is False
    assert "plan_record_column_mismatch" in observed.terminal.reason

    resumed = runtime.resume(state.run_id)

    assert resumed.state.status == "failed_integrity"
    assert "plan_record_column_mismatch" in resumed.terminal.reason
