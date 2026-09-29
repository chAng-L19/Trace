from __future__ import annotations

import json
from typing import Any, Mapping

from ..core import ToolResult, contract_hash
from .contracts import AgentRunView, Observation
from .agent_loop_support import handle_tool_expand


class CycleLimit(Exception):
    def __init__(self, view: AgentRunView, progress_hash: str):
        self.view, self.progress_hash = view, progress_hash


def run_model_cycles(loop, run_id, *, max_actions=None, run_until_pause=True, max_cycles=32):
    bounded = max(1, min(128, int(max_cycles))) if run_until_pause else 1
    with loop.service.runtime.store.connection() as connection:
        row = connection.execute("SELECT payload_json FROM operation_events WHERE run_id=? AND event_type='model_cycle_completed' ORDER BY event_id DESC LIMIT 1", (run_id,)).fetchone()
    previous = json.loads(row[0]) if row else {}
    cycle = int(previous.get("cycle", 0))
    for index in range(bounded):
        cycle += 1
        try:
            view = run_cycle(loop, run_id, max_actions=max_actions)
            reason = "waiting_external_worker" if any(item.status == "waiting_worker" for item in loop.service.worker_records.records(run_id)) else view.run.status
            progress = ""
            continuing = False
        except CycleLimit as limit:
            view, progress = limit.view, limit.progress_hash
            continuing = index + 1 < bounded
            reason = "cycle_limit"
            if progress == previous.get("progress_hash"):
                continuing, reason = False, "model_no_progress"
            if loop._is_interrupted(run_id) or loop._is_cancelled(run_id):
                continuing, reason = False, "interrupted"
            if not continuing and reason in {"cycle_limit", "model_no_progress"}:
                loop.service.runtime.pause_run(run_id, reason="model_cycle_limit" if reason == "cycle_limit" else reason)
                view = loop.service.status(run_id)
        event = {"cycle": cycle, "turn_limit": loop.max_turns, "cycle_limit": bounded,
                 "reason": reason, "continue_reason": "cycle_limit_with_progress" if continuing else "",
                 "continuing": continuing, "progress_hash": progress, "pause_reason": view.run.budget.pause_reason}
        loop.service.runtime.store.append_event(run_id, "model_cycle_completed", event)
        if not continuing:
            return view
        previous = event
    return loop.service.status(run_id)


def run_cycle(loop, run_id: str, *, max_actions: int | None = None) -> AgentRunView:
    view = loop.service._resume_runtime(
        run_id,
        max_actions=max_actions,
        model_led=True,
    )
    signatures = []
    for _ in range(loop.max_turns):
        pending = loop.service.worker_records.records(run_id)
        if any(item.status == "waiting_worker" for item in pending):
            return view
        if any(item.status == "unknown" for item in pending):
            loop.service.runtime.pause_run(run_id, reason="worker_result_unknown")
            return loop.service.status(run_id)
        if view.terminal.terminal or view.run.status in {"completed", "failed", "cancelled"}:
            return view
        if loop._is_cancelled(run_id):
            return loop.service.status(run_id)
        if loop._is_interrupted(run_id):
            return loop.service.status(run_id)
        if view.run.status == "paused_budget":
            return view
        budget_view = loop.service._enforce_runtime_budget(run_id)
        if budget_view.run.status == "paused_budget":
            return budget_view
        if view.run.status != "waiting_worker" or not view.next_action:
            return view
        recovered = loop._recover_pending_turn(view)
        if recovered is None:
            try:
                response, request = loop._model_turn(view)
            except loop._interrupted_error:
                return loop.service.status(run_id)
            existing: Mapping[str, ToolResult] = {}
            reconcile = False
        else:
            response, request, existing = recovered
            reconcile = True
            loop.service.conversation.record_model_response(run_id, response)
        if loop._is_cancelled(run_id):
            return loop.service.status(run_id)
        if loop._is_interrupted(run_id):
            return loop.service.status(run_id)
        current = loop.service.status(run_id)
        if current.run.status == "paused_budget":
            return current
        tactical_update = loop._record_tactical_update(view, request, response)
        budget_view = loop.service._record_model_usage(
            run_id,
            request.request_id,
            response.usage,
        )
        if budget_view.run.status == "paused_budget":
            return budget_view
        expanded_tools = handle_tool_expand(loop, run_id, response)
        if expanded_tools and not response.tool_calls:
            loop._mark_turn_consumed(view, request, "tool_catalog_expanded")
            view = loop.service._resume_runtime(run_id, max_actions=max_actions, model_led=True)
            continue
        if not response.tool_calls:
            loop._mark_turn_consumed(view, request, "model_response_no_tools")
            loop.service.runtime.pause_run(run_id, reason="waiting_input" if response.structured_output.get("decision") in {"waiting_input", "request_input", "need_input"} else "model_no_progress")
            return loop.service.status(run_id)
        try:
            results = loop._execute_tool_calls(
                view,
                request,
                response,
                existing=existing,
                reconcile=reconcile,
            )
        except loop._interrupted_error:
            return loop.service.status(run_id)
        if loop._is_cancelled(run_id):
            return loop.service.cancel(run_id, reason="model_loop_cancelled")
        if loop._is_interrupted(run_id):
            return loop.service.status(run_id)
        current = loop.service.status(run_id)
        if current.run.status == "paused_budget":
            return current
        artifact_ids = loop.service.conversation.record_tool_results(
            request.request_id,
            view.run.run_id,
            results,
        )
        loop._record_tactical_attempts(
            view,
            request,
            response,
            results,
            artifact_ids=artifact_ids,
            tactical_update=tactical_update,
        )
        signatures.append(contract_hash({"calls": [{k: v for k, v in dict(call).items() if k not in {"id", "call_id"}} for call in response.tool_calls], "results": [{"status": item.status, "output": item.output, "error": item.error} for item in results]}))
        if any(item.status == "waiting_worker" for item in loop.service.worker_records.records(run_id)):
            loop._mark_turn_consumed(view, request, "waiting_external_worker")
            return loop.service.status(run_id)
        successful = tuple(item for item in results if item.status == "success")
        if not successful:
            # A failed tool result is still a completed protocol turn.  It
            # must be visible to the model so it can change arguments,
            # choose another tool, or suspend the hypothesis.  Recovery
            # uses the durable observation/attempt records to avoid
            # repeating the failed side effect.
            loop._mark_turn_consumed(view, request, "tool_calls_failed")
            view = loop.service._resume_runtime(
                run_id,
                max_actions=max_actions,
                model_led=True,
            )
            continue
        if (view.next_action != "provide_target" and
                response.structured_output.get("commit_lifecycle_gate") is not True):
            loop._mark_turn_consumed(view, request, "lifecycle_gate_not_committed")
            view = loop.service._resume_runtime(run_id, max_actions=max_actions, model_led=True)
            continue
        if view.next_action == "provide_target":
            target = loop._target_from_results(successful)
            loop.service.runtime.provide_target(run_id, targets=(target,))
            loop._mark_turn_consumed(view, request, "target_provided")
            view = loop.service._resume_runtime(
                run_id,
                max_actions=max_actions,
                model_led=True,
            )
            continue
        output: Any
        if len(successful) == 1:
            output = successful[0].output
        else:
            output = {"tool_results": [item.to_dict() for item in successful]}
        receipt = dict(view.handoff)
        if receipt:
            loop.service.runtime.submit_model_observation(
                run_id=run_id,
                request_id=request.request_id,
                call_ids=tuple(item.call_id for item in successful),
                handoff_id=str(receipt.get("handoff_id") or ""),
                handoff_token=str(receipt.get("handoff_token") or ""),
                attempt_id=str(receipt.get("attempt_id") or ""),
                contract_hash=str(receipt.get("contract_hash") or ""),
                continue_run=False,
            )
        else:
            observation = Observation(
                action_id=view.next_action,
                output=output,
                tool="model-loop:" + ",".join(item.tool_name for item in successful),
                usage={"_accounted_request_id": request.request_id},
                idempotency_key=contract_hash(
                    {
                        "run_id": run_id,
                        "request_id": request.request_id,
                        "action_id": view.next_action,
                        "calls": [item.call_id for item in successful],
                    }
                ),
                continue_run=False,
            )
            loop.service._submit_runtime_observation(run_id, observation)
        loop._mark_turn_consumed(view, request, "lifecycle_observation_submitted")
        view = loop.service._resume_runtime(
            run_id,
            max_actions=max_actions,
            model_led=True,
        )
    if view.terminal.terminal or view.run.status in {"paused_budget", "completed", "failed", "cancelled", "cancelling"}:
        return view
    raise CycleLimit(view, contract_hash(signatures))
