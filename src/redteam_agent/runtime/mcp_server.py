from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import sys
import threading
import tomllib
from pathlib import Path
from typing import Any, Mapping

from .operation_runtime import OperationRuntime
from .adaptive_planner import AdaptivePlanner
from .handoff import DEFAULT_HANDOFF_TTL_SECONDS
from .mcp_limits import MAX_OBSERVATION_BYTES, MAX_REQUEST_BYTES, MAX_TOOL_ARGUMENT_BYTES
from .mcp_tool_dispatch import RuntimeMcpToolDispatchMixin
from .session_bridge import sync_session_summary
from .security import safe_error_text
from .tool_broker import ToolBroker




_ALL_TOOL_DEFINITIONS = [
    {
        "name": "redteam_run",
        "description": "Single autonomous entrypoint: start or resume one operation or a multi-target batch, accept Host Agent observations, and continue to the next durable or terminal state.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "session_id": {"type": "string"},
                "objective": {"type": "string"},
                "targets": {"type": "array", "items": {"type": "string"}},
                "workflow_hint": {"type": "string"},
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
                "observation": {
                    "type": "object",
                    "properties": {
                        "action_id": {"type": "string"},
                        "handoff_id": {"type": "string", "minLength": 1},
                        "handoff_token": {"type": "string", "minLength": 1},
                        "attempt_id": {"type": "string", "minLength": 1},
                        "contract_hash": {"type": "string", "minLength": 1},
                        "output": {},
                        "tool": {"type": "string"},
                        "usage": {
                            "type": "object",
                            "properties": {
                                "total_tokens": {"type": "integer", "minimum": 0},
                                "input_tokens": {"type": "integer", "minimum": 0},
                                "output_tokens": {"type": "integer", "minimum": 0},
                            },
                            "additionalProperties": False,
                        },
                    },
                    "required": ["handoff_id", "handoff_token", "attempt_id", "contract_hash", "output"],
                    "additionalProperties": False,
                },
                "observations": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "run_id": {"type": "string"},
                            "action_id": {"type": "string"},
                            "handoff_id": {"type": "string", "minLength": 1},
                            "handoff_token": {"type": "string", "minLength": 1},
                            "attempt_id": {"type": "string", "minLength": 1},
                            "contract_hash": {"type": "string", "minLength": 1},
                            "output": {},
                            "tool": {"type": "string"},
                            "usage": {
                                "type": "object",
                                "properties": {
                                    "total_tokens": {"type": "integer", "minimum": 0},
                                    "input_tokens": {"type": "integer", "minimum": 0},
                                    "output_tokens": {"type": "integer", "minimum": 0},
                                },
                                "additionalProperties": False,
                            },
                        },
                        "required": [
                            "run_id",
                            "handoff_id",
                            "handoff_token",
                            "attempt_id",
                            "contract_hash",
                            "output"
                        ],
                        "additionalProperties": False,
                    },
                },
                "starting_context": {"type": "object"},
                "constraints": {"type": "object"},
                "success_predicates": {"type": "array", "items": {"type": "object"}},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "max_total_actions": {"type": "integer", "minimum": 1, "maximum": 4096},
                "max_tokens": {"type": "integer", "minimum": 1},
                "max_time_seconds": {"type": "number", "minimum": 0.1},
                "deadline": {"type": "string"},
                "budget_delta": {
                    "type": "object",
                    "properties": {
                        "actions": {"type": "integer", "minimum": 0},
                        "tokens": {"type": "integer", "minimum": 0},
                        "time_seconds": {"type": "number", "minimum": 0},
                        "deadline": {"type": "string"},
                    },
                    "additionalProperties": False,
                },
                "credential_bindings": {
                    "type": "object",
                    "additionalProperties": {"type": "string"},
                    "description": "Process-local mapping of required SECRET_REF values to raw tool-channel credentials.",
                },
                "auto_continue": {"type": "boolean"},
                "max_cycles": {"type": "integer", "minimum": 1, "maximum": 64},
                "max_retries_per_action": {"type": "integer", "minimum": 0, "maximum": 8},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_start",
        "description": "Compile a red-team goal, select a typed workflow, execute available tools, and persist the operation until its next durable state.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "session_id": {"type": "string"},
                "objective": {"type": "string"},
                "targets": {"type": "array", "items": {"type": "string"}},
                "workflow_hint": {"type": "string"},
                "starting_context": {"type": "object"},
                "constraints": {"type": "object"},
                "success_predicates": {"type": "array", "items": {"type": "object"}},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "max_total_actions": {"type": "integer", "minimum": 1, "maximum": 4096},
                "max_tokens": {"type": "integer", "minimum": 1},
                "max_time_seconds": {"type": "number", "minimum": 0.1},
                "deadline": {"type": "string"},
                "max_retries_per_action": {"type": "integer", "minimum": 0, "maximum": 8},
            },
            "required": ["session_id", "objective"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_resume",
        "description": "Resume a persisted operation without requiring copied tool output from the user.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "credential_bindings": {
                    "type": "object",
                    "additionalProperties": {"type": "string"},
                },
            },
            "required": ["run_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_status",
        "description": "Return non-advancing operation or batch state, verified evidence, missing predicates, and the next executable action.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_submit_observation",
        "description": "Submit host-agent tool output to the current typed action; semantic verification and lineage checks run before the workflow advances automatically.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "action_id": {"type": "string"},
                "output": {},
                "tool": {"type": "string"},
                "usage": {
                    "type": "object",
                    "properties": {
                        "total_tokens": {"type": "integer", "minimum": 0},
                        "input_tokens": {"type": "integer", "minimum": 0},
                        "output_tokens": {"type": "integer", "minimum": 0},
                    },
                    "additionalProperties": False,
                },
                "continue_run": {"type": "boolean"},
            },
            "required": ["run_id", "action_id", "output"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_evidence",
        "description": "Fetch one verified evidence node by operation and evidence ID when its payload was omitted from a compact status response.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "evidence_id": {"type": "string"},
            },
            "required": ["run_id", "evidence_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_cancel",
        "description": "Cancel an active operation, run an available cleanup action, and persist the cleanup outcome.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
                "reason": {"type": "string"},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_events",
        "description": "Return the durable event trace for an operation.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "after_event_id": {"type": "integer", "minimum": 0},
                "limit": {"type": "integer", "minimum": 1, "maximum": 1000},
            },
            "required": ["run_id"],
            "additionalProperties": False,
        },
    },
]
PUBLIC_TOOL_NAMES = (
    "redteam_run",
    "redteam_status",
    "redteam_evidence",
    "redteam_cancel",
    "redteam_events",
)
LEGACY_TOOL_NAMES = (
    "redteam_start",
    "redteam_resume",
    "redteam_submit_observation",
)
TOOL_DEFINITIONS_BY_NAME = {str(item["name"]): item for item in _ALL_TOOL_DEFINITIONS}
TOOL_DEFINITIONS = [TOOL_DEFINITIONS_BY_NAME[name] for name in PUBLIC_TOOL_NAMES]
PUBLIC_TOOL_DEFINITIONS_BY_NAME = {
    name: TOOL_DEFINITIONS_BY_NAME[name] for name in PUBLIC_TOOL_NAMES
}


class RuntimeMcpServer(RuntimeMcpToolDispatchMixin):
    def __init__(
        self,
        runtime: OperationRuntime,
        *,
        service: Any | None = None,
        default_max_actions: int = 64,
        default_max_retries_per_action: int = 2,
        handoff_ttl_seconds: float = DEFAULT_HANDOFF_TTL_SECONDS,
    ) -> None:
        self.runtime = runtime
        if service is None and isinstance(runtime, OperationRuntime):
            from ..application.agent_service import AgentService

            service = AgentService(runtime=runtime)
        self.service = service
        self.default_max_actions = max(1, min(512, int(default_max_actions)))
        self.default_max_retries_per_action = max(0, min(8, int(default_max_retries_per_action)))
        self.handoff_ttl_seconds = max(1.0, float(handoff_ttl_seconds))
        self._handoff_tokens: dict[str, str] = {}
        self._handoff_token_lock = threading.RLock()

    @staticmethod
    def _summary_of(result: Any) -> dict[str, Any]:
        if hasattr(result, "summary") and callable(result.summary):
            return dict(result.summary())
        if isinstance(result, Mapping):
            return dict(result)
        raise TypeError("runtime_result_not_mappable")

    @staticmethod
    def _encoded_size(value: Any) -> int:
        return len(
            json.dumps(value, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
        )

    @staticmethod
    def _contract_hash(
        *,
        run_id: str,
        branch_id: str,
        plan_revision: int,
        action_id: str,
        action_spec: Mapping[str, Any],
    ) -> str:
        payload = {
            "run_id": run_id,
            "branch_id": branch_id,
            "plan_revision": plan_revision,
            "action_id": action_id,
            "name": action_spec.get("name"),
            "target": action_spec.get("target"),
            "required_capabilities": action_spec.get("required_capabilities"),
            "expected_artifact": action_spec.get("expected_artifact"),
            "verifier": action_spec.get("verifier"),
            "output_contract": action_spec.get("output_contract"),
            "parameters": action_spec.get("parameters"),
            "evidence_refs": action_spec.get("evidence_refs"),
            "feedback_gate": action_spec.get("feedback_gate"),
            "exit_condition": action_spec.get("exit_condition"),
        }
        encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str)
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()

    @staticmethod
    def _handoff_complete(value: Any) -> bool:
        return isinstance(value, Mapping) and all(
            str(value.get(key) or "").strip()
            for key in ("handoff_id", "handoff_token", "attempt_id", "contract_hash")
        )

    def _ensure_host_handoff(self, summary: Mapping[str, Any]) -> dict[str, Any]:
        """Attach a fresh one-time receipt to a host-only next action.

        Newer OperationRuntime implementations create the receipt themselves.
        This adapter keeps the MCP contract intact while the facade remains
        backward-compatible with older runtimes.
        """

        prepared = dict(summary)
        if str(prepared.get("status") or "") != "waiting_host":
            return prepared
        run_id = str(prepared.get("run_id") or "").strip()
        raw_spec = prepared.get("next_action_spec")
        if not run_id or not isinstance(raw_spec, Mapping):
            return prepared
        spec = dict(raw_spec)
        existing = spec.get("handoff")
        if self._handoff_complete(existing):
            record = self.runtime.store.get_handoff(str(existing.get("handoff_id") or ""))
            if record is not None and record.status == "pending" and (
                record.run_id,
                record.branch_id,
                record.plan_revision,
                record.action_id,
                record.attempt_id,
                record.contract_hash,
            ) == (
                run_id,
                str(existing.get("branch_id") or record.branch_id),
                int(existing.get("plan_revision") or record.plan_revision),
                str(existing.get("action_id") or record.action_id),
                str(existing.get("attempt_id") or ""),
                str(existing.get("contract_hash") or ""),
            ):
                with self._handoff_token_lock:
                    self._handoff_tokens[record.handoff_id] = str(existing.get("handoff_token") or "")
                return prepared
        state = self.runtime.store.load_operation(run_id)
        if state is None or state.status != "waiting_host":
            return prepared
        action_id = str(spec.get("action_id") or prepared.get("next_action") or state.current_action_id or "").strip()
        if not action_id or state.current_action_id != action_id:
            return prepared
        contract_hash = self._contract_hash(
            run_id=run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
            action_spec=spec,
        )
        pending = self.runtime.store.pending_handoff(
            run_id=run_id,
            branch_id=state.branch_id,
            plan_revision=state.plan_revision,
            action_id=action_id,
        )
        if pending is not None and pending.contract_hash == contract_hash:
            with self._handoff_token_lock:
                cached_token = self._handoff_tokens.get(pending.handoff_id, "")
            if cached_token:
                spec["handoff"] = {
                    **pending.identity(),
                    "handoff_token": cached_token,
                }
                prepared["next_action_spec"] = spec
                return prepared
        try:
            handoff_id, handoff_token, placeholder = self.runtime.store.rotate_handoff(
                run_id=run_id,
                branch_id=state.branch_id,
                plan_revision=state.plan_revision,
                action_id=action_id,
                contract_hash=contract_hash,
                ttl_seconds=self.handoff_ttl_seconds,
            )
        except (KeyError, ValueError):
            # The operation advanced between the summary read and receipt
            # creation.  Returning the durable summary is preferable to
            # publishing a receipt for a stale action.
            return prepared
        spec["handoff"] = {
            "handoff_id": handoff_id,
            "handoff_token": handoff_token,
            "run_id": run_id,
            "branch_id": state.branch_id,
            "plan_revision": state.plan_revision,
            "action_id": action_id,
            "attempt_id": placeholder.attempt_id,
            "contract_hash": contract_hash,
        }
        with self._handoff_token_lock:
            if pending is not None:
                self._handoff_tokens.pop(pending.handoff_id, None)
            self._handoff_tokens[handoff_id] = handoff_token
        prepared["next_action_spec"] = spec
        return prepared

    def _submit_handoff_observation(
        self,
        *,
        run_id: str,
        observation: Mapping[str, Any],
        max_actions: int,
    ) -> dict[str, Any]:
        if self._encoded_size(observation.get("output")) > MAX_OBSERVATION_BYTES:
            raise ValueError("observation_output_too_large")
        handoff_id = str(observation.get("handoff_id") or "").strip()
        handoff_token = str(observation.get("handoff_token") or "").strip()
        attempt_id = str(observation.get("attempt_id") or "").strip()
        contract_hash = str(observation.get("contract_hash") or "").strip()
        if not all((run_id, handoff_id, handoff_token, attempt_id, contract_hash)):
            raise ValueError("complete_handoff_receipt_required")
        record = self.runtime.store.get_handoff(handoff_id)
        if record is None or record.status != "pending":
            raise ValueError("handoff_receipt_not_pending")
        if (
            record.run_id,
            record.attempt_id,
            record.contract_hash,
        ) != (run_id, attempt_id, contract_hash):
            raise ValueError("handoff_receipt_identity_mismatch")
        supplied_action = str(observation.get("action_id") or "").strip()
        if supplied_action and supplied_action != record.action_id:
            raise ValueError("handoff_action_mismatch")
        service = self._application_service()
        if service is not None:
            from ..application.contracts import Observation

            service.submit_observation(
                run_id,
                Observation(
                    action_id=record.action_id,
                    output=observation.get("output"),
                    tool=str(observation.get("tool") or "host-agent"),
                    usage=(
                        observation.get("usage")
                        if isinstance(observation.get("usage"), Mapping)
                        else {}
                    ),
                    continue_run=True,
                    max_actions=max_actions,
                    handoff_id=handoff_id,
                    handoff_token=handoff_token,
                    attempt_id=attempt_id,
                    contract_hash=contract_hash,
                ),
            )
            with self._handoff_token_lock:
                self._handoff_tokens.pop(handoff_id, None)
            return service.summary(run_id)
        submit = getattr(self.runtime, "submit_handoff_observation", None)
        if callable(submit):
            result = submit(
                run_id=run_id,
                handoff_id=handoff_id,
                handoff_token=handoff_token,
                attempt_id=attempt_id,
                contract_hash=contract_hash,
                output=observation.get("output"),
                tool=str(observation.get("tool") or "host-agent"),
                usage=observation.get("usage") if isinstance(observation.get("usage"), Mapping) else None,
                continue_run=True,
                max_actions=max_actions,
            )
            with self._handoff_token_lock:
                self._handoff_tokens.pop(handoff_id, None)
            return self._summary_of(result)
        receipt_identity = record.identity()
        receipt_identity.pop("handoff_id", None)
        consumed = self.runtime.store.consume_handoff(
            handoff_id=handoff_id,
            raw_token=handoff_token,
            **receipt_identity,
        )
        if not consumed:
            raise ValueError("handoff_receipt_rejected")
        with self._handoff_token_lock:
            self._handoff_tokens.pop(handoff_id, None)
        result = self.runtime.submit_observation(
            run_id=run_id,
            action_id=record.action_id,
            output=observation.get("output"),
            tool=str(observation.get("tool") or "host-agent"),
            continue_run=True,
            max_actions=max_actions,
        )
        return self._summary_of(result)

    def _apply_budget_delta(
        self,
        run_ids: list[str],
        raw_delta: Any,
    ) -> None:
        if raw_delta is None:
            return
        if not isinstance(raw_delta, Mapping):
            raise ValueError("budget_delta_must_be_object")
        delta = {
            "actions": int(raw_delta.get("actions") or 0),
            "tokens": int(raw_delta.get("tokens") or 0),
            "time_seconds": float(raw_delta.get("time_seconds") or 0.0),
            "deadline": str(raw_delta.get("deadline") or "").strip(),
        }
        if any(delta[key] < 0 for key in ("actions", "tokens", "time_seconds")):
            raise ValueError("budget_delta_must_be_non_negative")
        if not any((delta["actions"], delta["tokens"], delta["time_seconds"], delta["deadline"])):
            return
        apply_delta = getattr(self.runtime, "apply_budget_delta", None)
        service = self._application_service()
        if service is not None:
            apply_delta = service.apply_budget_delta
        if not callable(apply_delta):
            raise ValueError("runtime_budget_delta_unsupported")
        apply_batch = getattr(self.runtime, "apply_budget_delta_batch", None)
        if service is not None:
            apply_batch = service.apply_budget_delta_batch
        if len(run_ids) > 1 and callable(apply_batch):
            apply_batch(run_ids, **delta)
            return
        for run_id in run_ids:
            apply_delta(run_id, **delta)

    def _continue_summary(
        self,
        summary: Mapping[str, Any],
        *,
        cycle_actions: int,
        max_cycles: int,
        auto_continue: bool,
    ) -> dict[str, Any]:
        current = dict(summary)
        cycles = 1
        run_id = str(current.get("run_id") or "")
        while auto_continue and run_id and current.get("status") == "paused_budget" and cycles < max_cycles:
            current = self._resume_summary(run_id, max_actions=cycle_actions)
            cycles += 1
        current["automation_cycles"] = cycles
        return current

    @staticmethod
    def _batch_status(operations: list[Mapping[str, Any]]) -> str:
        statuses = [str(item.get("status") or "") for item in operations]
        if operations and all(status == "completed" for status in statuses):
            return "completed"
        if operations and all(status == "cancelled" for status in statuses):
            return "cancelled"
        if any(status in {"failed", "failed_integrity", "cancelled"} for status in statuses):
            return "failed"
        if any(status in {"waiting_tools", "waiting_host"} for status in statuses):
            return "waiting_host"
        if any(status == "cancelling" for status in statuses):
            return "cancelling"
        if any(status == "waiting_goal_input" for status in statuses):
            return "waiting_goal_input"
        if any(status == "paused_budget" for status in statuses):
            return "paused_budget"
        return "running"

    def _run_batch(
        self,
        *,
        batch_session_id: str,
        summaries: list[Mapping[str, Any]] | None,
        observations: list[Mapping[str, Any]],
        cycle_actions: int,
        max_cycles: int,
        auto_continue: bool,
    ) -> dict[str, Any]:
        states = self.runtime.store.operations_for_batch(batch_session_id)
        known_run_ids = {state.run_id for state in states}
        if not known_run_ids and summaries:
            known_run_ids = {str(item.get("run_id") or "") for item in summaries if item.get("run_id")}
        if not known_run_ids:
            raise KeyError(f"batch_not_found:{batch_session_id}")
        observation_by_run: dict[str, Mapping[str, Any]] = {}
        handoff_ids: set[str] = set()
        for observation in observations:
            run_id = str(observation.get("run_id") or "").strip()
            if run_id not in known_run_ids:
                raise ValueError(f"batch_observation_run_mismatch:{run_id}")
            if run_id in observation_by_run:
                raise ValueError(f"duplicate_batch_observation:{run_id}")
            handoff_id = str(observation.get("handoff_id") or "").strip()
            if handoff_id in handoff_ids:
                raise ValueError(f"duplicate_batch_handoff:{handoff_id}")
            handoff_ids.add(handoff_id)
            observation_by_run[run_id] = observation

        validate_receipt = getattr(self.runtime, "validate_handoff_observation", None)
        if callable(validate_receipt):
            for run_id, observation in observation_by_run.items():
                if self._encoded_size(observation.get("output")) > MAX_OBSERVATION_BYTES:
                    raise ValueError("observation_output_too_large")
                handoff_id = str(observation.get("handoff_id") or "").strip()
                handoff_token = str(observation.get("handoff_token") or "").strip()
                attempt_id = str(observation.get("attempt_id") or "").strip()
                contract_hash = str(observation.get("contract_hash") or "").strip()
                if not all((run_id, handoff_id, handoff_token, attempt_id, contract_hash)):
                    raise ValueError("complete_handoff_receipt_required")
                if not validate_receipt(
                    run_id=run_id,
                    handoff_id=handoff_id,
                    handoff_token=handoff_token,
                    attempt_id=attempt_id,
                    contract_hash=contract_hash,
                    action_id=str(observation.get("action_id") or "").strip(),
                ):
                    raise ValueError(f"batch_handoff_receipt_rejected:{run_id}")

        initial = {str(item.get("run_id") or ""): item for item in summaries or ()}
        results: list[dict[str, Any]] = []
        ordered_run_ids = [state.run_id for state in states] or sorted(known_run_ids)
        for run_id in ordered_run_ids:
            observation = observation_by_run.get(run_id)
            if observation is not None:
                summary = self._submit_handoff_observation(
                    run_id=run_id,
                    observation=observation,
                    max_actions=cycle_actions,
                )
            elif run_id in initial:
                summary = dict(initial[run_id])
            else:
                summary = self._resume_summary(run_id, max_actions=cycle_actions)
            results.append(
                self._ensure_host_handoff(
                    self._continue_summary(
                    summary,
                    cycle_actions=cycle_actions,
                    max_cycles=max_cycles,
                    auto_continue=auto_continue,
                    )
                )
            )
        success = bool(results) and all(
            item.get("status") == "completed" and item.get("terminal", {}).get("success") is True
            for item in results
        )
        parent_session_id = (
            str(states[0].goal.starting_context.get("parent_session_id") or "")
            if states
            else ""
        )
        return {
            "batch_session_id": batch_session_id,
            "session_id": parent_session_id,
            "status": self._batch_status(results),
            "run_ids": [str(item.get("run_id") or "") for item in results],
            "operations": results,
            "pending_operations": [
                {
                    "run_id": item.get("run_id"),
                    "next_action": item.get("next_action"),
                    "next_action_spec": item.get("next_action_spec"),
                    "missing_capabilities": item.get("missing_capabilities", []),
                }
                for item in results
                if item.get("status") != "completed"
            ],
            "terminal": {
                "terminal": all(item.get("terminal", {}).get("terminal") is True for item in results),
                "success": success,
                "reason": "batch_goal_contract_satisfied" if success else "batch_goal_predicates_pending",
            },
            "automation_cycles": sum(int(item.get("automation_cycles") or 0) for item in results),
        }

    def _status_batch(self, batch_session_id: str) -> dict[str, Any]:
        states = self.runtime.store.operations_for_batch(batch_session_id)
        if not states:
            raise KeyError(f"batch_not_found:{batch_session_id}")
        results = [
            self._ensure_host_handoff(self._status_summary(state.run_id))
            for state in states
        ]
        success = bool(results) and all(
            item.get("status") == "completed" and item.get("terminal", {}).get("success") is True
            for item in results
        )
        parent_session_id = str(states[0].goal.starting_context.get("parent_session_id") or "")
        return {
            "batch_session_id": batch_session_id,
            "session_id": parent_session_id,
            "status": self._batch_status(results),
            "run_ids": [state.run_id for state in states],
            "operations": results,
            "pending_operations": [
                {
                    "run_id": item.get("run_id"),
                    "next_action": item.get("next_action"),
                    "next_action_spec": item.get("next_action_spec"),
                    "missing_capabilities": item.get("missing_capabilities", []),
                }
                for item in results
                if item.get("status") != "completed"
            ],
            "terminal": {
                "terminal": all(item.get("terminal", {}).get("terminal") is True for item in results),
                "success": success,
                "reason": "batch_goal_contract_satisfied" if success else "batch_goal_predicates_pending",
            },
        }

    def handle(self, payload: Mapping[str, Any]) -> Mapping[str, Any] | None:
        method = str(payload.get("method") or "")
        request_id = payload.get("id")
        if not method:
            return self._error(request_id, -32600, "invalid_request")
        if request_id is None:
            return None
        try:
            if method == "initialize":
                result: Any = {
                    "protocolVersion": "2025-06-18",
                    "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "redteam-agent-runtime", "version": "1"},
                }
            elif method == "ping":
                result = {}
            elif method == "tools/list":
                result = {"tools": TOOL_DEFINITIONS}
            elif method == "tools/call":
                params = payload.get("params") if isinstance(payload.get("params"), Mapping) else {}
                tool_name = str(params.get("name") or "")
                # Legacy tools remain available only as private implementation
                # details behind redteam_run.  JSON-RPC callers may dispatch
                # exactly the surface advertised by tools/list.
                definition = PUBLIC_TOOL_DEFINITIONS_BY_NAME.get(tool_name)
                if definition is None:
                    raise ValueError(f"tool_not_found:{tool_name}")
                raw_arguments = params.get("arguments")
                arguments = raw_arguments if isinstance(raw_arguments, Mapping) else {}
                if self._encoded_size(arguments) > MAX_TOOL_ARGUMENT_BYTES:
                    raise ValueError("tool_arguments_too_large")
                schema_error = ToolBroker._schema_error(definition["inputSchema"], arguments)
                if schema_error:
                    raise ValueError(schema_error)
                result = self._call_tool(tool_name, arguments)
            else:
                return self._error(request_id, -32601, f"method_not_found:{method}")
            return {"jsonrpc": "2.0", "id": request_id, "result": result}
        except KeyError as exc:
            return self._error(request_id, -32004, safe_error_text(exc))
        except ValueError as exc:
            return self._error(request_id, -32602, safe_error_text(exc))
        except Exception as exc:
            return self._error(request_id, -32000, safe_error_text(f"runtime_error:{exc}"))

    @staticmethod
    def _error(request_id: Any, code: int, message: str) -> Mapping[str, Any]:
        return {
            "jsonrpc": "2.0",
            "id": request_id,
            "error": {"code": code, "message": safe_error_text(message)},
        }
from .mcp_transport import (
    _default_config_paths,
    _iter_request_lines,
    _runtime_settings,
    _serve_stdio,
    main,
)


if __name__ == "__main__":
    raise SystemExit(main())
