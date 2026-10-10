"""Offline regression for real usage accounting and unlimited action budgets."""
from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.runtime import apply_core_run, run_from_runtime
from redteam_agent.application.contracts import BudgetDelta, Observation, StartRequest
from redteam_agent.core import Budget, Goal, ModelRequest
from redteam_agent.providers import FakeModelProvider, OpenAICompatibleProvider, ScriptedStream
from redteam_agent.providers.anthropic_protocol import usage_values
from redteam_agent.providers.openai_protocol import responses_response, stream_events
from redteam_agent.runtime.mcp_server import RuntimeMcpServer
from redteam_agent.runtime.model_contracts import GoalContract
from redteam_agent.runtime.model_state import RunBudget
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.settings import _runtime_settings
from phase0_snapshot import OBJECTIVE, TARGET, _snapshot_broker
from provider_protocol_smoke import sse


def contracts():
    assert StartRequest.from_value({"session_id": "s", "objective": "Inspect fixture"}).max_actions == 64
    request = StartRequest.from_value({"session_id": "s", "objective": "Inspect fixture", "max_actions": 0})
    assert request.runtime_arguments()["max_actions"] == 0
    goal = GoalContract.create(objective=request.objective, targets=("fixture://budget",), max_actions=0)
    assert GoalContract.from_dict(goal.to_dict()).max_actions == 0
    assert Goal.from_dict({"goal_id": "g", "objective": request.objective, "max_actions": 0}).max_actions == 0
    assert Observation.from_value({"action_id": "a", "output": {}, "max_actions": 0}).max_actions == 0
    assert BudgetDelta.from_value({"action_limit": 0}).changes_budget
    assert BudgetDelta.from_value({"unlimited_actions": True}).action_limit == 0
    assert BudgetDelta.from_value({"unlimited_actions": False, "actions": 10}).action_limit is None
    budget = RunBudget.create(action_limit=1)
    budget.record_action()
    assert budget.exhaustion_reason() == "action_limit_exhausted"
    assert budget.extend(action_limit=0) and not budget.exhaustion_reason()
    budget.apply_delta(actions=10)
    assert budget.action_limit == 0
    assert not budget.extend(action_limit=64)
    assert RunBudget.from_dict(budget.to_dict()).action_limit == 0
    assert Budget.from_dict(budget.to_dict()).remaining_actions is None
    budget.apply_delta(action_limit=80, actions=2)
    assert budget.action_limit == 82
    budget.apply_delta(action_limit=0)
    budget.token_limit = 5
    budget.record_token_usage({"prompt_tokens": 3, "completion_tokens": 2}, required=True)
    assert budget.exhaustion_reason() == "token_limit_exhausted"
    unknown = RunBudget.create(action_limit=0, token_limit=100)
    unknown.record_token_usage({}, required=True)
    assert unknown.exhaustion_reason() == "token_usage_unknown"
    expired = RunBudget.create(action_limit=0, deadline="2000-01-01T00:00:00+00:00")
    assert expired.exhaustion_reason() == "time_limit_exhausted"
    total = Budget.from_dict({"tokens_used": 11})
    assert total.tokens_used == 11 and total.output_tokens_used is None
    assert Budget.from_dict({"input_tokens_used": 10, "output_tokens_used": 5}).tokens_used == 15
    assert Budget.from_dict({"input_tokens_used": 10}).tokens_used is None


def protocols():
    provider = OpenAICompatibleProvider("http://127.0.0.1:1/v1", "fixture", environ={})
    request = ModelRequest("protocol", "run", ())
    chat = {"choices": [{"message": {"content": "Done"}, "finish_reason": "stop"}]}
    for usage in ({"prompt_tokens": 10, "completion_tokens": 5},
                  {"input_tokens": 10, "output_tokens": 5}):
        reply = provider._response(request, {**chat, "usage": usage}, {})
        assert RunBudget.normalized_token_usage(reply.usage) == (10, 5, 15)
    reply = responses_response(provider, request, {"status": "completed", "output": [],
        "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}, {})
    assert RunBudget.normalized_token_usage(reply.usage) == (10, 5, 15)
    events = list(stream_events(provider, request, sse(
        {"choices": [{"delta": {"content": "Done"}}], "usage": {"prompt_tokens": 10}},
        {"choices": [], "usage": {"completion_tokens": 5}},
        {"choices": [{"delta": {}, "finish_reason": "stop"}], "usage": {}}, done=True), {}, byte_limit=65536))
    assert RunBudget.normalized_token_usage(events[-1].payload["usage"]) == (10, 5, 15)
    assert usage_values({"input_tokens": 10, "output_tokens": 5,
                         "cache_read_input_tokens": 7, "cache_creation_input_tokens": 3})["total_tokens"] == 25
    assert provider._usage(None) == {}


def accounting(root):
    provider = FakeModelProvider()
    with AgentService(root=root, model_port=provider, load_external_configuration=False) as service:
        run_id = service.start({"session_id": "usage", "objective": "Inspect fixture", "targets": [str(root)],
                                "max_actions": 0}).single.run.run_id
        assert service.status(run_id).run.budget.tokens_used is None
        totals = service.inspect_session(run_id)["model"]["totals"]
        assert totals["requests"] == 0 and totals["total_tokens"] == 0
        for index, usage in enumerate(({"prompt_tokens": 10, "completion_tokens": 5}, {}, {"total_tokens": 7})):
            request = ModelRequest(f"usage-{index}", run_id, ())
            service.model_loop._save_request(request)
            service._record_model_usage(run_id, request.request_id, usage)
            service._record_model_usage(run_id, request.request_id, usage)
        current = service.status(run_id).run.budget
        assert current.tokens_used == 22 and current.input_tokens_used == 10 and current.output_tokens_used == 5
        assert current.token_usage_missing == 1
        assert len(service.runtime.store.model_budget_usage(run_id)) == 3
        totals = service.inspect_session(run_id)["model"]["totals"]
        assert totals["total_tokens"] is None and totals["known_totals"]["total_tokens"] == 22
        assert totals["missing_requests"] == 1 and totals["requests"] == 3
        state = service.runtime.store.load_operation(run_id)
        apply_core_run(state, run_from_runtime(state))
        assert state.budget.tokens_used == 22
        result = service.apply_budget_delta(run_id, action_limit=0)
        assert result.run.budget.action_limit == 0
    with AgentService(root=root, load_external_configuration=False) as reopened:
        assert reopened.status(run_id).run.budget.tokens_used == 22
        assert reopened.status(run_id).run.budget.action_limit == 0


def streaming(root):
    stream = ScriptedStream((
        {"event_type": "text_delta", "payload": {"delta": "Done"}},
        {"event_type": "usage", "payload": {"input_tokens": 10, "output_tokens": 1}},
        {"event_type": "usage", "payload": {"output_tokens": 5}},
        {"event_type": "completed", "payload": {"text": "Done", "usage": {}, "finish_reason": "stop"}},
    ))
    with AgentService(root=root, model_port=FakeModelProvider(streams=(stream,)), model_streaming=True,
                      load_external_configuration=False) as service:
        run_id = service.start({"session_id": "stream", "objective": "Inspect fixture",
                                "targets": [str(root)], "max_actions": 0}).single.run.run_id
        result = service.run(run_id, max_actions=0)
        assert result.run.budget.tokens_used == 15 and result.run.budget.token_usage_missing == 0
        assert service.journal.model_responses(run_id)[0].usage == {"input_tokens": 10, "output_tokens": 5}
        assert len(service.runtime.store.model_budget_usage(run_id)) == 1


def model_cycles(root):
    responses = [{"status": "completed", "usage": {"total_tokens": 3}, "tool_calls": [
        {"call_id": f"call-{index}", "tool_name": "agent:artifacts", "arguments": {"limit": index + 1}}]}
        for index in range(3)]
    responses.append({"status": "completed", "text": "Done", "usage": {}})
    provider = FakeModelProvider(responses)
    with AgentService(root=root, model_port=provider, model_max_turns=1,
                      load_external_configuration=False) as service:
        run_id = service.start({"session_id": "cycles", "objective": "Inspect fixture",
                                "targets": [str(root)], "max_actions": 0}).single.run.run_id
        result = service.run(run_id, max_actions=0, max_cycles=4)
        assert len(provider.requests) == 4
        assert result.run.budget.tokens_used == 9 and result.run.budget.token_usage_missing == 1
        assert len(service.runtime.store.model_budget_usage(run_id)) == 4
        cycles = [event for event in service.runtime.store.events(run_id)
                  if event["event_type"] == "model_cycle_completed"]
        assert len(cycles) == 4


def forwarding(root):
    """Run after the other owner applies budget_forwarding.patch."""
    with AgentService(root=root, load_external_configuration=False) as service:
        run_ids = [service.start({"session_id": f"forward-{index}", "objective": "Inspect fixture",
                                 "targets": [str(root)], "max_actions": 1}).single.run.run_id
                   for index in range(2)]
        run_id = run_ids[0]
        finite = service.status(run_id)
        service.apply_budget_delta_once(run_id, idempotency_key="legacy", actions=1)
        service.apply_budget_delta_once(run_id, idempotency_key="legacy", actions=1)
        assert service.status(run_id).run.budget.action_limit == finite.run.budget.action_limit + 1
        service.apply_budget_delta_once(run_id, idempotency_key="unlimited", action_limit=0)
        version = service.status(run_id).run.state_version
        service.apply_budget_delta_once(run_id, idempotency_key="unlimited", action_limit=0)
        assert service.status(run_id).run.state_version == version
        service.apply_budget_delta_batch(run_ids, action_limit=0)
        assert all(service.status(identity).run.budget.action_limit == 0 for identity in run_ids)
        service.resume(run_id, {"unlimited_actions": True}, execute=False)
        assert service.status(run_id).run.budget.action_limit == 0
        service.run(run_ids[1], {"unlimited_actions": True, "idempotency_key": "run-unlimited"}, max_actions=0)
        assert service.status(run_ids[1]).run.budget.action_limit == 0


def execution(root):
    runtime = OperationRuntime(root=root, broker=_snapshot_broker(), register_builtins=False)
    try:
        state = runtime.start(session_id="unlimited", objective=OBJECTIVE, targets=(TARGET,), max_actions=0)
        state.budget.actions_used = 64
        runtime.store.save_operation(state, expected_version=state.state_version)
        result = runtime.resume(state.run_id, max_actions=0)
        assert result.state.status in {"completed", "waiting_host"}, result.state.status
        assert result.state.budget.actions_used > 64 and result.summary()["budget"]["remaining_actions"] is None
        state = runtime.start(session_id="cycle", objective=OBJECTIVE, targets=(TARGET,), max_actions=0)
        result = runtime.resume(state.run_id, max_actions=1)
        assert result.state.budget.pause_reason == "cycle_action_limit"
        result = runtime.resume(state.run_id, max_actions=0)
        assert result.state.status in {"completed", "waiting_host"}, result.state.status
        assert result.state.budget.actions_used > 1 and not result.state.budget.pause_reason
    finally:
        runtime.broker.close()


def mcp_and_settings(root):
    config = root / "unlimited.toml"
    config.write_text("[automation]\nmax_actions_per_cycle = 0\n", encoding="utf-8")
    assert _runtime_settings([config], environ={})["max_actions_per_cycle"] == 0
    assert _runtime_settings([], environ={})["max_actions_per_cycle"] == 64
    with AgentService(root=root / "mcp", load_external_configuration=False) as service:
        server = RuntimeMcpServer(service.runtime, service=service, default_max_actions=0)
        assert server.default_max_actions == 0
        started = server._call_tool("redteam_run", {"session_id": "mcp-unlimited", "objective": "Inspect fixture",
            "targets": [str(root)], "max_actions": 0, "auto_continue": False})
        summary = started["structuredContent"]
        assert summary["budget"]["action_limit"] == 0, started
        server._apply_budget_delta([summary["run_id"]], {"unlimited_actions": True})
        assert service.status(summary["run_id"]).run.budget.action_limit == 0
        with patch.object(server, "_resume_summary", return_value=summary) as resume:
            server._call_tool("redteam_resume", {"run_id": summary["run_id"], "max_actions": 0})
            assert resume.call_args.kwargs["max_actions"] == 0


def cli(root):
    from redteam_agent.cli import _main
    environment = {"REDTEAM_AGENT_HOME": str(root / "home"), "REDTEAM_AGENT_CONFIG": str(root / "absent.toml"),
                   "USERPROFILE": str(root / "home"), "HOME": str(root / "home")}
    def invoke(args):
        output = io.StringIO()
        with patch.dict(os.environ, environment, clear=True), contextlib.redirect_stdout(output):
            assert _main([*args, "--root", str(root)]) == 0
        return json.loads(output.getvalue())
    started = invoke(["start", "Inspect fixture", "--session-id", "cli-unlimited", "--target", str(root), "--max-actions", "0"])
    run_id = started["runs"][0]["run"]["run_id"]
    assert started["runs"][0]["run"]["budget"]["action_limit"] == 0
    invoked = invoke(["run", run_id, "--max-actions", "0", "--action-limit", "0"])
    assert invoked["run"]["budget"]["action_limit"] == 0
    resumed = invoke(["resume", run_id, "--max-actions", "0"])
    assert resumed["run"]["budget"]["action_limit"] == 0


def main():
    contracts()
    protocols()
    checks = ["contracts", "provider_usage"]
    with tempfile.TemporaryDirectory(prefix="trace-budget-usage-") as temporary:
        root = Path(temporary)
        selected = [accounting, streaming, model_cycles, execution, mcp_and_settings, cli]
        if "--forwarding" in sys.argv:
            selected.append(forwarding)
        for check in selected:
            path = root / check.__name__
            path.mkdir()
            check(path)
            checks.append(check.__name__)
    print(json.dumps({"ok": True, "checks": checks}))


if __name__ == "__main__":
    main()
