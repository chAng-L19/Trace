from __future__ import annotations

import json
from typing import Any, Mapping

from .mcp_limits import MAX_OBSERVATION_BYTES
from .session_bridge import sync_session_summary


class RuntimeMcpToolDispatchMixin:
    def _application_service(self) -> Any | None:
        service = getattr(self, "service", None)
        return service if service is not None else None

    def _bind_credentials(self, run_id: str, bindings: Mapping[str, Any]) -> None:
        service = self._application_service()
        if service is not None:
            service.bind_credentials(run_id, bindings)
        else:
            self.runtime.bind_credentials(run_id, bindings)

    def _resume_summary(self, run_id: str, *, max_actions: int) -> dict[str, Any]:
        service = self._application_service()
        if service is None:
            return self.runtime.resume(run_id, max_actions=max_actions).summary()
        service.run(run_id, max_actions=max_actions)
        return service.summary(run_id)

    def _status_summary(self, run_id: str) -> dict[str, Any]:
        service = self._application_service()
        return service.summary(run_id) if service is not None else self.runtime.status(run_id).summary()

    def _cancel_summary(self, run_id: str, reason: str) -> dict[str, Any]:
        service = self._application_service()
        if service is None:
            return self.runtime.cancel(run_id, reason=reason).summary()
        service.cancel(run_id, reason=reason)
        return service.summary(run_id)

    def _submit_summary(self, run_id: str, arguments: Mapping[str, Any], *, max_actions: int) -> dict[str, Any]:
        service = self._application_service()
        if service is None:
            return self.runtime.submit_observation(
                run_id=run_id,
                action_id=str(arguments.get("action_id") or ""),
                output=arguments.get("output"),
                tool=str(arguments.get("tool") or "host-agent"),
                usage=arguments.get("usage") if isinstance(arguments.get("usage"), Mapping) else None,
                continue_run=bool(arguments.get("continue_run", True)),
                max_actions=max_actions,
            ).summary()
        from ..application.contracts import Observation

        service.submit_observation(
            run_id,
            Observation.from_value({**dict(arguments), "max_actions": max_actions}),
        )
        return service.summary(run_id)

    def _call_tool(self, name: str, raw_arguments: Any) -> Mapping[str, Any]:
        arguments = raw_arguments if isinstance(raw_arguments, Mapping) else {}
        if name == "redteam_run":
            run_id = str(arguments.get("run_id") or "").strip()
            batch_session_id = str(arguments.get("batch_session_id") or "").strip()
            observation = arguments.get("observation") if isinstance(arguments.get("observation"), Mapping) else None
            raw_observations = arguments.get("observations")
            observations = [item for item in raw_observations if isinstance(item, Mapping)] if isinstance(raw_observations, list) else []
            budget_delta = arguments.get("budget_delta")
            credential_bindings = (
                arguments.get("credential_bindings")
                if isinstance(arguments.get("credential_bindings"), Mapping)
                else {}
            )
            auto_continue = bool(arguments.get("auto_continue", True))
            cycle_actions = int(arguments.get("max_actions") or self.default_max_actions)
            max_cycles = max(1, min(64, int(arguments.get("max_cycles") or 16)))
            if run_id and batch_session_id:
                raise ValueError("run_id_and_batch_session_id_are_mutually_exclusive")
            if observation is not None and observations:
                raise ValueError("observation_and_observations_are_mutually_exclusive")
            if observation is not None and not run_id:
                raise ValueError("single_observation_requires_run_id")
            if observations and not batch_session_id:
                raise ValueError("batch_observations_require_batch_session_id")
            if batch_session_id and observation is not None:
                raise ValueError("batch_requires_observations_array")
            if run_id and observations:
                raise ValueError("single_run_requires_observation_object")
            if budget_delta is not None and not (run_id or batch_session_id):
                raise ValueError("budget_delta_requires_existing_run_or_batch")
            if credential_bindings and not (run_id or batch_session_id):
                raise ValueError("credential_bindings_require_existing_run_or_batch")
            if arguments.get("max_total_actions") is not None and (run_id or batch_session_id):
                raise ValueError("use_budget_delta_actions_when_resuming")
            target_patch_requested = run_id and (
                "targets" in arguments or "starting_context" in arguments
            )
            if target_patch_requested:
                if observation is not None or observations:
                    raise ValueError("target_patch_and_observation_are_mutually_exclusive")
                raw_targets = arguments.get("targets")
                starting_context = (
                    arguments.get("starting_context")
                    if isinstance(arguments.get("starting_context"), Mapping)
                    else {}
                )
                supplied_targets = tuple(
                    dict.fromkeys(
                        str(item).strip()
                        for item in (
                            raw_targets
                            if isinstance(raw_targets, list)
                            else self.runtime.compiler.extract_context_targets(starting_context)
                        )
                        if str(item).strip()
                    )
                )
                if not supplied_targets:
                    raise ValueError("target_required_for_waiting_goal")
                service = self._application_service()
                if service is not None:
                    service.provide_target(run_id, supplied_targets)
                else:
                    self.runtime.provide_target(run_id, targets=supplied_targets)
            if batch_session_id:
                batch_states = self.runtime.store.operations_for_batch(batch_session_id)
                if not batch_states:
                    raise KeyError(f"batch_not_found:{batch_session_id}")
                known_refs = {
                    reference
                    for state in batch_states
                    for reference in state.credential_refs
                }
                if any(str(reference) not in known_refs for reference in credential_bindings):
                    raise ValueError("credential_binding_reference_unknown")
                for state in batch_states:
                    scoped = {
                        str(reference): value
                        for reference, value in credential_bindings.items()
                        if str(reference) in state.credential_refs
                    }
                    if scoped:
                        self._bind_credentials(state.run_id, scoped)
                self._apply_budget_delta([state.run_id for state in batch_states], budget_delta)
                summary = self._run_batch(
                    batch_session_id=batch_session_id,
                    summaries=None,
                    observations=observations,
                    cycle_actions=cycle_actions,
                    max_cycles=max_cycles,
                    auto_continue=auto_continue,
                )
            elif not run_id:
                started = self._call_tool("redteam_start", arguments)
                summary = started["structuredContent"]
                if isinstance(summary.get("operations"), list):
                    summary = self._run_batch(
                        batch_session_id=str(summary.get("batch_session_id") or ""),
                        summaries=[item for item in summary["operations"] if isinstance(item, Mapping)],
                        observations=observations,
                        cycle_actions=cycle_actions,
                        max_cycles=max_cycles,
                        auto_continue=auto_continue,
                    )
                    summary["session_state_synced"] = sync_session_summary(
                        str(summary.get("session_id") or summary.get("batch_session_id") or ""),
                        summary,
                        store=self.runtime.store,
                    )
                    return {
                        "content": [{"type": "text", "text": json.dumps(summary, ensure_ascii=False, default=str)}],
                        "structuredContent": summary,
                        "isError": False,
                    }
                run_id = str(summary.get("run_id") or "")
                if not run_id or not auto_continue:
                    if isinstance(summary, dict):
                        summary["session_state_synced"] = sync_session_summary(
                            str(arguments.get("session_id") or ""),
                            summary,
                            store=self.runtime.store,
                        )
                    return started
            elif observation is not None:
                if credential_bindings:
                    self._bind_credentials(run_id, credential_bindings)
                self._apply_budget_delta([run_id], budget_delta)
                summary = self._submit_handoff_observation(
                    run_id=run_id,
                    observation=observation,
                    max_actions=cycle_actions,
                )
            else:
                if credential_bindings:
                    self._bind_credentials(run_id, credential_bindings)
                self._apply_budget_delta([run_id], budget_delta)
                summary = self._resume_summary(run_id, max_actions=cycle_actions)
            if run_id:
                summary = self._continue_summary(
                    summary,
                    cycle_actions=cycle_actions,
                    max_cycles=max_cycles,
                    auto_continue=auto_continue,
                )
        elif name == "redteam_start":
            session_id = str(arguments.get("session_id") or "").strip()
            objective = str(arguments.get("objective") or "").strip()
            if not session_id or not objective:
                raise ValueError("session_id_and_objective_required")
            targets = arguments.get("targets")
            starting_context = arguments.get("starting_context") if isinstance(arguments.get("starting_context"), Mapping) else {}
            resolved_targets = (
                tuple(str(item) for item in targets if str(item).strip())
                if isinstance(targets, list)
                else self.runtime.compiler.extract_targets(objective)
                or self.runtime.compiler.extract_context_targets(starting_context)
            )
            predicates = arguments.get("success_predicates")
            cycle_actions = int(arguments.get("max_actions") or self.default_max_actions)
            total_actions = int(arguments.get("max_total_actions") or max(256, cycle_actions))
            budget_options: dict[str, Any] = {}
            if arguments.get("max_tokens") is not None:
                budget_options["token_limit"] = int(arguments["max_tokens"])
            if arguments.get("max_time_seconds") is not None:
                budget_options["time_limit_seconds"] = float(arguments["max_time_seconds"])
            if arguments.get("deadline") is not None:
                budget_options["deadline"] = str(arguments.get("deadline") or "")
            service = self._application_service()
            if service is not None:
                start_arguments = {
                    **dict(arguments),
                    "targets": resolved_targets,
                    "max_actions": total_actions,
                    "token_limit": budget_options.get("token_limit"),
                    "time_limit_seconds": budget_options.get("time_limit_seconds"),
                    "deadline": budget_options.get("deadline", ""),
                }
                start_result = service.start(start_arguments)
                run_ids = start_result.run_ids
                states = tuple(
                    self.runtime.store.load_operation(run_id)
                    for run_id in run_ids
                )
                results = [self._resume_summary(run_id, max_actions=cycle_actions) for run_id in run_ids]
            else:
                states = self.runtime.start_batch(
                    session_id=session_id,
                    objective=objective,
                    targets=resolved_targets,
                    workflow_hint=str(arguments.get("workflow_hint") or ""),
                    starting_context=starting_context,
                    constraints=arguments.get("constraints") if isinstance(arguments.get("constraints"), Mapping) else {},
                    success_predicates=predicates if isinstance(predicates, list) else (),
                    max_actions=total_actions,
                    max_retries_per_action=int(
                        arguments.get("max_retries_per_action", self.default_max_retries_per_action)
                    ),
                    **budget_options,
                )
                results = [self._resume_summary(state.run_id, max_actions=cycle_actions) for state in states]
            if len(results) == 1:
                summary = results[0]
            else:
                batch_session_id = str(states[0].goal.starting_context.get("batch_session_id") or "")
                summary = self._run_batch(
                    batch_session_id=batch_session_id,
                    summaries=results,
                    observations=[],
                    cycle_actions=cycle_actions,
                    max_cycles=1,
                    auto_continue=False,
                )
        elif name == "redteam_resume":
            run_id = str(arguments.get("run_id") or "")
            credential_bindings = (
                arguments.get("credential_bindings")
                if isinstance(arguments.get("credential_bindings"), Mapping)
                else {}
            )
            if credential_bindings:
                self._bind_credentials(run_id, credential_bindings)
            summary = self._resume_summary(
                run_id,
                max_actions=int(arguments.get("max_actions") or self.default_max_actions),
            )
        elif name == "redteam_status":
            run_id = str(arguments.get("run_id") or "").strip()
            batch_session_id = str(arguments.get("batch_session_id") or "").strip()
            if bool(run_id) == bool(batch_session_id):
                raise ValueError("exactly_one_of_run_id_or_batch_session_id_required")
            summary = (
                self._status_summary(run_id)
                if run_id
                else self._status_batch(batch_session_id)
            )
        elif name == "redteam_submit_observation":
            if self._encoded_size(arguments.get("output")) > MAX_OBSERVATION_BYTES:
                raise ValueError("observation_output_too_large")
            summary = self._submit_summary(
                str(arguments.get("run_id") or ""),
                arguments,
                max_actions=self.default_max_actions,
            )
        elif name == "redteam_evidence":
            run_id = str(arguments.get("run_id") or "")
            evidence_id = str(arguments.get("evidence_id") or "")
            node = next(
                (
                    item
                    for item in self.runtime.evidence_graph.list(run_id, include_unverified=True)
                    if item.evidence_id == evidence_id
                ),
                None,
            )
            if node is None:
                raise KeyError(f"evidence_not_found:{evidence_id}")
            summary = node.to_dict()
        elif name == "redteam_cancel":
            run_id = str(arguments.get("run_id") or "").strip()
            batch_session_id = str(arguments.get("batch_session_id") or "").strip()
            if bool(run_id) == bool(batch_session_id):
                raise ValueError("exactly_one_of_run_id_or_batch_session_id_required")
            reason = str(arguments.get("reason") or "user_requested")
            if run_id:
                summary = self._cancel_summary(run_id, reason)
            else:
                states = self.runtime.store.operations_for_batch(batch_session_id)
                if not states:
                    raise KeyError(f"batch_not_found:{batch_session_id}")
                results = []
                for state in states:
                    if state.status in {"completed", "failed", "failed_integrity"}:
                        results.append(self._status_summary(state.run_id))
                    else:
                        results.append(self._cancel_summary(state.run_id, reason))
                summary = self._run_batch(
                    batch_session_id=batch_session_id,
                    summaries=results,
                    observations=[],
                    cycle_actions=self.default_max_actions,
                    max_cycles=1,
                    auto_continue=False,
                )
                if all(item.get("status") == "cancelled" for item in summary["operations"]):
                    summary["status"] = "cancelled"
                    summary["terminal"] = {
                        "terminal": True,
                        "success": False,
                        "reason": "batch_cancelled",
                    }
        elif name == "redteam_events":
            run_id = str(arguments.get("run_id") or "")
            events = self.runtime.store.events(
                run_id,
                after_event_id=int(arguments.get("after_event_id") or 0),
                limit=int(arguments.get("limit") or 200),
            )
            summary = {
                "run_id": run_id,
                "events": list(events),
                "next_event_id": events[-1]["event_id"] if events else None,
            }
        else:
            raise ValueError(f"tool_not_found:{name}")
        if name in {
            "redteam_run",
            "redteam_start",
            "redteam_resume",
            "redteam_status",
            "redteam_submit_observation",
            "redteam_cancel",
        } and isinstance(summary, Mapping):
            if name != "redteam_cancel":
                if isinstance(summary.get("operations"), list):
                    summary = {
                        **dict(summary),
                        "operations": [
                            self._ensure_host_handoff(item)
                            for item in summary.get("operations", [])
                            if isinstance(item, Mapping)
                        ],
                    }
                else:
                    summary = self._ensure_host_handoff(summary)
            if summary.get("batch_session_id"):
                bridge_session_id = str(summary.get("session_id") or summary.get("batch_session_id") or "")
            else:
                bridge_run_id = str(summary.get("run_id") or arguments.get("run_id") or "")
                bridge_state = self.runtime.store.load_operation(bridge_run_id) if bridge_run_id else None
                bridge_session_id = bridge_state.session_id if bridge_state is not None else str(arguments.get("session_id") or "")
            if isinstance(summary, dict):
                summary["session_state_synced"] = sync_session_summary(
                    bridge_session_id,
                    summary,
                    store=self.runtime.store,
                )
        return {
            "content": [{"type": "text", "text": json.dumps(summary, ensure_ascii=False, default=str)}],
            "structuredContent": summary,
            "isError": False,
        }
