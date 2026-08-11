from __future__ import annotations

import sys
from pathlib import Path


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import OperationResult as PackageOperationResult
from redteam_agent.runtime import OperationRuntime as PackageOperationRuntime
from redteam_agent.runtime import operation_runtime as operation_runtime_module
from redteam_agent.runtime.operation_result import OperationResult
from redteam_agent.runtime.operation_runtime import OperationRuntime


def test_operation_runtime_facade_preserves_public_import_identity() -> None:
    assert PackageOperationRuntime is OperationRuntime
    assert PackageOperationResult is OperationResult
    assert {
        "start",
        "start_batch",
        "start_or_resume",
        "provide_target",
        "resume",
        "submit_handoff_observation",
        "validate_handoff_observation",
        "submit_observation",
        "apply_budget_delta",
        "apply_budget_delta_once",
        "apply_budget_delta_batch",
        "bind_credentials",
        "missing_credential_refs",
        "cancel",
        "status",
    } <= set(dir(OperationRuntime))


def test_operation_runtime_modules_remain_bounded() -> None:
    runtime_root = Path(operation_runtime_module.__file__).resolve().parent
    names = (
        "operation_runtime.py",
        "operation_result.py",
        "operation_contract.py",
        "operation_lifecycle.py",
        "operation_execution.py",
        "operation_handoff.py",
        "operation_cancellation.py",
    )
    counts = {
        name: len((runtime_root / name).read_text(encoding="utf-8").splitlines())
        for name in names
    }
    assert counts == {name: count for name, count in counts.items() if count <= 800}
