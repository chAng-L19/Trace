"""Local regressions for intent contracts, bounded execution and stall recovery."""
from __future__ import annotations

import json
import sys
import threading
import time
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from model_recovery_smoke import response, scenario
from redteam_agent.application.model_loop import (
    MAX_PARALLEL_TOOL_CALLS, MAX_TOOL_CALLS_PER_TURN, ModelIntegrityError,
)
from redteam_agent.core import ModelRequest, ModelResponse, ToolResult
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.intent_rewriter import LEGACY_REWRITE_VERSION, rewrite_objective
from redteam_agent.runtime.workflow_registry import WorkflowRegistry


def contract_checks():
    informational = (
        "\u53ea\u7ed9\u65b9\u6848\uff0c\u5e76\u5206\u6790\u53ef\u884c\u6027\uff0c\u4e0d\u8981\u6267\u884c\u6216\u4fee\u6539",
        "\u53ea\u5206\u6790\u8fd9\u4e2a\u6f0f\u6d1e\u539f\u7406\uff0c\u4e0d\u8981\u6267\u884c\u6216\u4fee\u6539",
        "\u89e3\u91ca\u8fd9\u4e2a\u6f0f\u6d1e\u539f\u7406",
        "\u53ea\u5206\u6790\u4ee3\u7801\uff0c\u4e0d\u6267\u884c\uff0c\u4e0d\u4fee\u6539",
        "Only review the source code and give me a plan; do not execute or modify anything",
        "Explain the vulnerability mechanism without executing or modifying anything",
    )
    for objective in informational:
        goal = GoalCompiler().compile(objective)
        assert goal.intent_envelope["source_text"] == objective
        assert goal.intent_envelope["execution_required"] is False
        workflow = WorkflowRegistry().match(goal)
        assert [action.action_id for action in workflow.actions] == ["map-surface", "build-hypotheses", "report"]
        for clause in goal.intent_envelope["clause_contracts"]:
            assert set(clause["required_artifacts"]) <= {"surface_map", "hypothesis_queue", "final_report"}
    for objective in (
        "Explain the mechanism, then reproduce it",
        "Give me a plan and execute the tests",
        "Do not only give me a plan; implement the change",
        "Inspect local fixture",
        "\u5206\u6790\u5e76\u5b9e\u9645\u590d\u73b0\u6f0f\u6d1e",
    ):
        assert GoalCompiler().compile(objective).intent_envelope["execution_required"] is True
    # Existing durable v2 contracts remain verifiable with their original semantics.
    with scenario([]) as (service, _, run_id):
        state = service.runtime.store.load_operation(run_id)
        old = rewrite_objective(state.goal.objective, targets=state.goal.targets,
                                rewrite_version=LEGACY_REWRITE_VERSION)
        envelope = {**old.to_dict(), "fingerprint": old.fingerprint}
        for key in ("target_binding", "input_redaction", "credential_refs"):
            envelope[key] = state.goal.intent_envelope[key]
        state.goal = replace(state.goal, intent_envelope=envelope)
        assert service.runtime._goal_rewrite_integrity_error(state) == ""
        state.goal = replace(state.goal, intent_envelope={**envelope, "execution_required": False})
        assert service.runtime._goal_rewrite_integrity_error(state) == "prompt_rewrite_contract_mismatch"
    return "informational_constraints_and_legacy_contract_integrity"


def stall_checks():
    plan = response(text="I will inspect the files and run the checks.")
    tool = response(tool_calls=[{"call_id": "read", "tool_name": "agent:artifacts", "arguments": {}}])
    with scenario([plan, tool, response(structured_output={"decision": "waiting_input"}), plan]) as (service, provider, run_id):
        result = service.run(run_id)
        messages = service.conversation.messages(run_id)
        assert len(provider.requests) == 3 and result.run.budget.pause_reason == "waiting_input"
        assert len([item for item in messages if item.source_type == "model_execution_stall_nudge"]) == 1
        assert any(item.tool_name == "agent:artifacts" and item.status == "success"
                   for item in service.journal.model_observations(run_id))
        service.configure_model(provider)
        result = service.resume(run_id)
        assert len(provider.requests) == 4 and result.run.budget.pause_reason == "model_no_progress"
        assert len([item for item in service.conversation.messages(run_id)
                    if item.source_type == "model_execution_stall_nudge"]) == 1
    for plain in (
        response(text="The result is inconclusive."),
        response(text="I will not execute that."),
        response(text="I will inspect it.", structured_output={"decision": "waiting_input"}),
        response(text="I will inspect it.", metadata={"refusal": True}),
    ):
        with scenario([plain]) as (service, provider, run_id):
            service.run(run_id)
            assert len(provider.requests) == 1
            assert not any(item.source_type == "model_execution_stall_nudge"
                           for item in service.conversation.messages(run_id))
    with scenario([plan], token_limit=1) as (service, provider, run_id):
        assert service.run(run_id).run.budget.pause_reason == "token_limit_exhausted"
        assert len(provider.requests) == 1
    with scenario([plan]) as (service, provider, _):
        run_id = service.start({"session_id": "analysis", "objective": "Only analyze the source code",
                                "targets": ["fixture://analysis"]}).single.run.run_id
        service.run(run_id)
        assert len(provider.requests) == 1
        assert not any(item.source_type == "model_execution_stall_nudge"
                       for item in service.conversation.messages(run_id))
    return "one_durable_execution_correction_preserves_stop_boundaries"


def batch_checks():
    calls = [{"call_id": str(index), "tool_name": "agent:artifacts", "arguments": {}}
             for index in range(MAX_TOOL_CALLS_PER_TURN + 1)]
    with scenario([response(tool_calls=calls), response(text="Paused for review.")]) as (service, provider, run_id):
        with patch.object(service.agent_loop, "_execute_tool_calls") as execute:
            try:
                service.run(run_id)
            except ModelIntegrityError as error:
                assert str(error) == "model_tool_batch_limit"
            else:
                raise AssertionError("oversized tool batch executed")
            execute.assert_not_called()
        assert service.status(run_id).run.budget.pause_reason == "model_tool_batch_limit"
        stored = service.journal.model_responses(run_id)[0]
        assert stored.status == "integrity_error" and stored.usage["total_tokens"] == 1
        service.configure_model(provider)
        assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
        service.resume(run_id)
        assert len(provider.requests) == 2
    with scenario([]) as (service, _, run_id):
        loop = service.agent_loop
        view = service._resume_runtime(run_id, model_led=True)
        peak = active = 0
        lock = threading.Lock()

        def invoke(view, request, call, *, reconcile=False):
            nonlocal peak, active
            with lock:
                active += 1
                peak = max(peak, active)
            time.sleep(0.03)
            with lock:
                active -= 1
            return ToolResult(call.call_id, "success", call.tool_name, output=call.call_id)

        request = ModelRequest("parallel", run_id, (), allow_parallel_tools=True)
        reply = ModelResponse("parallel", "completed", tool_calls=tuple(calls[:16]))
        with patch.object(loop, "_invoke_tool", side_effect=invoke):
            results = loop._execute_tool_calls(view, request, reply)
        assert len(results) == 16 and [item.call_id for item in results] == [str(index) for index in range(16)]
        assert 1 < peak <= MAX_PARALLEL_TOOL_CALLS, peak
        active = peak = 0
        with patch.object(loop, "_invoke_tool", side_effect=invoke):
            loop._execute_tool_calls(view, replace(request, allow_parallel_tools=False), reply)
        assert peak == 1
    return "batch_rejection_is_accounted_and_parallelism_is_bounded"


def main():
    print(json.dumps({"ok": True, "checks": [contract_checks(), stall_checks(), batch_checks()]}))


if __name__ == "__main__":
    main()
