from pathlib import Path

from redteam_agent.runtime.model_common import utc_now
from redteam_agent.runtime.session_journal import ModelRequestRecord
from test_runtime_control_plane import _cancel_fixture


def test_budget_enforcement_preserves_pending_cancel_cleanup(tmp_path: Path) -> None:
    runtime, state, _, _ = _cancel_fixture(tmp_path, include_cleanup=False)
    current = runtime.store.load_operation(state.run_id)
    current.budget.actions_used = current.budget.action_limit
    runtime.store.save_operation(current)
    cancelling = runtime.cancel(state.run_id)
    assert cancelling.state.status == "cancelling"

    enforced = runtime.enforce_budget(state.run_id)

    assert enforced.state.status == "cancelling"
    assert enforced.state.cleanup_status == "unavailable"
    assert enforced.state.state_version == cancelling.state.state_version


def test_late_model_usage_accounts_tokens_without_reviving_cancelled_cleanup(tmp_path: Path) -> None:
    runtime, state, _, _ = _cancel_fixture(tmp_path, include_cleanup=False)
    current = runtime.store.load_operation(state.run_id)
    current.budget.token_limit = 1
    runtime.store.save_operation(current)
    runtime.store.save_model_request(ModelRequestRecord(
        request_id="late-usage", run_id=state.run_id, prompt_hash="fixture",
        provider="fixture", model="fixture", capabilities={}, request={}, created_at=utc_now(),
    ))
    assert runtime.cancel(state.run_id).state.status == "cancelling"

    recorded = runtime.record_model_usage(state.run_id, request_id="late-usage", usage={"total_tokens": 2})

    assert recorded.state.status == "cancelling"
    assert recorded.state.budget.tokens_used == 2
    assert recorded.state.cleanup_status == "unavailable"
