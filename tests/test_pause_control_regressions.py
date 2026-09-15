from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.durable_store import ImmutableRecordError
from test_conversation_context_budget import TargetToolPort, _request, _tool_response


def test_paused_run_without_target_never_calls_model_or_tools(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("pause-control-fixture", encoding="utf-8")
    provider = FakeModelProvider([_tool_response({"input_tokens": 4, "output_tokens": 2})])
    tools = TargetToolPort(str(target))
    service = AgentService(root=tmp_path / "runtime", model_port=provider, tool_port=tools)
    try:
        run_id = service.start(_request()).single.run.run_id
        paused = service.pause(run_id, reason="manual_review")

        result = service.run(run_id)

        assert provider.requests == []
        assert tools.calls == []
        assert result.run.status == "paused_budget"
        assert result.run.budget.pause_reason == "manual_review"
        assert result.run.state_version == paused.run.state_version
    finally:
        service.close()


def test_resume_without_execution_rejects_budget_change_on_cancelled_run(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        run_id = service.start({"session_id": "cancel-budget", "objective": "Prepare a plan"}).single.run.run_id
        cancelled = service.cancel(run_id)

        with pytest.raises(ValueError, match="operation_terminal:cancelled"):
            service.resume(run_id, {"actions": 1}, execute=False)

        result = service.status(run_id)
        assert result.run.status == "cancelled"
        assert result.run.budget == cancelled.run.budget
        assert result.run.state_version == cancelled.run.state_version
    finally:
        service.close()


def test_conflicting_budget_delta_does_not_clear_operator_pause(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        run_id = service.start({"session_id": "resume-conflict", "objective": "Prepare a plan"}).single.run.run_id
        service.apply_budget_delta_once(run_id, idempotency_key="same-budget-command", actions=1)
        paused = service.pause(run_id, reason="manual_review")

        with pytest.raises(ImmutableRecordError, match="budget_delta_idempotency_conflict"):
            service.resume(
                run_id,
                {"actions": 2, "idempotency_key": "same-budget-command"},
                execute=False,
            )

        result = service.status(run_id)
        assert result.run.status == "paused_budget"
        assert result.run.budget == paused.run.budget
        assert result.run.state_version == paused.run.state_version
    finally:
        service.close()


def test_batch_budget_delta_is_atomic_when_one_run_is_cancelled(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        active_id = service.start(
            {"session_id": "batch-active", "objective": "Prepare a plan"}
        ).single.run.run_id
        cancelled_id = service.start(
            {"session_id": "batch-cancelled", "objective": "Prepare another plan"}
        ).single.run.run_id
        service.cancel(cancelled_id)
        active_before = service.status(active_id)
        cancelled_before = service.status(cancelled_id)

        with pytest.raises(ValueError, match="operation_terminal:cancelled"):
            service.apply_budget_delta_batch([active_id, cancelled_id], actions=2)

        active_after = service.status(active_id)
        cancelled_after = service.status(cancelled_id)
        assert active_after.run.budget == active_before.run.budget
        assert active_after.run.state_version == active_before.run.state_version
        assert cancelled_after.run.budget == cancelled_before.run.budget
        assert cancelled_after.run.state_version == cancelled_before.run.state_version
    finally:
        service.close()
