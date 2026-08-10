from __future__ import annotations

import sys
from dataclasses import replace
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
CODEX_ROOT = REPO_ROOT / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime import GoalCompiler
from redteam_agent.runtime.operation_runtime import OperationRuntime


class _FingerprintCompiler:
    def __init__(self, fingerprint: str) -> None:
        self._delegate = GoalCompiler()
        self._fingerprint = fingerprint

    def compile(self, objective: str, **kwargs: object):
        goal = self._delegate.compile(objective, **kwargs)
        envelope = dict(goal.intent_envelope)
        envelope["fingerprint"] = self._fingerprint
        return replace(goal, intent_envelope=envelope)


def test_same_rewrite_fingerprint_reuses_the_deterministic_run(tmp_path: Path) -> None:
    runtime = OperationRuntime(
        root=tmp_path / "operations",
        compiler=_FingerprintCompiler("rewrite-v2:same"),
        register_builtins=False,
    )

    first = runtime.start(session_id="session-1", objective="Inspect TARGET and report findings")
    second = runtime.start(session_id="session-1", objective="Inspect TARGET and report findings")

    assert second.run_id == first.run_id
    assert second.goal.goal_id == first.goal.goal_id
    assert runtime.store.load_operation(first.run_id) is not None


def test_rewrite_fingerprint_change_creates_a_new_run(tmp_path: Path) -> None:
    root = tmp_path / "operations"
    old_runtime = OperationRuntime(
        root=root,
        compiler=_FingerprintCompiler("rewrite-v1:old"),
        register_builtins=False,
    )
    new_runtime = OperationRuntime(
        root=root,
        compiler=_FingerprintCompiler("rewrite-v2:new"),
        register_builtins=False,
    )

    old_state = old_runtime.start(session_id="session-1", objective="Inspect TARGET and report findings")
    new_state = new_runtime.start(session_id="session-1", objective="Inspect TARGET and report findings")

    assert new_state.run_id != old_state.run_id
    assert new_state.goal.goal_id != old_state.goal.goal_id
    assert new_runtime.store.load_operation(old_state.run_id) is not None
    assert new_runtime.store.load_operation(new_state.run_id) is not None
