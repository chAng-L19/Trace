from __future__ import annotations

import sys
from pathlib import Path


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import durable_store, executor, executor_actions, executor_common, models
from redteam_agent import runtime as runtime_package
from redteam_agent.runtime.model_common import utc_now
from redteam_agent.runtime.model_contracts import GoalContract
from redteam_agent.runtime.model_state import OperationState
from redteam_agent.runtime.store_common import StoreConflictError


def test_legacy_model_facade_preserves_public_type_identity() -> None:
    assert models.GoalContract is GoalContract
    assert models.OperationState is OperationState
    assert models.utc_now is utc_now


def test_legacy_store_facade_preserves_public_error_identity() -> None:
    assert durable_store.StoreConflictError is StoreConflictError
    assert hasattr(durable_store.DurableStore, "save_operation")
    assert hasattr(durable_store.DurableStore, "save_evidence")
    assert hasattr(durable_store.DurableStore, "receive_handoff_observation")


def test_executor_facade_preserves_split_contract_identity() -> None:
    assert executor.ExecutionOutcome is executor_common.ExecutionOutcome
    assert issubclass(executor.ActionExecutor, executor_actions.ExecutorActionsMixin)
    for name in ("execute", "reconcile_attempt", "accept_host_observation", "accept_external_observation"):
        assert hasattr(executor.ActionExecutor, name)


def test_all_production_python_modules_remain_bounded() -> None:
    package_root = Path(runtime_package.__file__).resolve().parent.parent
    production_roots = (package_root,)
    counts = {
        path.relative_to(package_root.parent).as_posix(): len(path.read_text(encoding="utf-8").splitlines())
        for root in production_roots
        for path in root.rglob("*.py")
        if "__pycache__" not in path.parts
    }
    oversized = {name: count for name, count in counts.items() if count > 800}
    assert oversized == {}
