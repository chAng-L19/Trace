from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Mapping

from .models import ActionSpec, EvidenceNode, GoalContract, OperationState, TerminalDecision, WorkflowSpec
from .evidence_trust import is_host_assertion, is_trusted_evidence
from .verifier import SemanticVerifier


from .workflow_registry import WorkflowRegistry


MAX_INLINE_EVIDENCE_BYTES = 64 * 1024
TERMINAL_FAILURE_STATUSES = {"failed", "failed_integrity", "cancelled"}
ARTIFACT_PHASES = {
    "hypothesis_queue": "hypothesis",
    "reproduction_artifact": "validation",
    "impact_proof": "impact",
    "coverage_report": "coverage",
    "cleanup_proof": "cleanup",
    "final_report": "reporting",
}


def _goal_contract_payload(goal: GoalContract) -> dict[str, Any]:
    return {
        "goal_id": goal.goal_id,
        "objective": goal.objective,
        "targets": list(goal.targets),
        "constraints": dict(goal.constraints),
        "success_criteria": [
            {
                "criterion_id": criterion.criterion_id,
                "statement": criterion.statement,
                "target": criterion.target,
                "workflow_id": criterion.workflow_id,
            }
            for criterion in goal.success_criteria
        ],
        "success_predicates": [
            {
                "kind": predicate.kind,
                "subject": predicate.subject,
                "operator": predicate.operator,
                "value": predicate.value,
                "description": predicate.description,
            }
            for predicate in goal.success_predicates
        ],
        "stop_conditions": list(goal.stop_conditions),
        "evidence_standard": goal.evidence_standard,
        "prompt_rewrite": dict(goal.intent_envelope),
    }


@dataclass(frozen=True)
class OperationResult:
    state: OperationState
    workflow: WorkflowSpec
    evidence: tuple[EvidenceNode, ...]
    terminal: TerminalDecision
    next_action: str = ""
    missing_capabilities: tuple[str, ...] = ()
    handoff: Mapping[str, Any] = field(default_factory=dict)

    @staticmethod
    def _gate(action: ActionSpec) -> dict[str, Any]:
        return {
            "gate_id": f"{action.action_id}:semantic",
            "predicate": "semantic_verification",
            "inputs": [f"artifact:{action.expected_artifact}", "target", "lineage"],
            "expected": True,
            "operator": "eq",
            "on_pass": "advance",
            "on_fail": "replan",
        }

    def _goal_contract(self) -> dict[str, Any]:
        return _goal_contract_payload(self.state.goal)

    def summary(self) -> dict[str, Any]:
        action_id = self.next_action or self.state.current_action_id
        action = next((item for item in self.workflow.actions if item.action_id == action_id), None)
        next_action_spec: dict[str, Any] | None = None
        if action is not None:
            host_assertions = [
                node
                for node in self.evidence
                if is_host_assertion(node) and node.action_id == action.action_id
            ]
            verification_required = bool(host_assertions)
            next_action_spec = {
                "action_id": action.action_id,
                "name": f"Verify host observation: {action.name}" if verification_required else action.name,
                "run_id": self.state.run_id,
                "branch_id": self.state.branch_id,
                "plan_revision": self.state.plan_revision,
                "goal_contract": self._goal_contract(),
                "required_capabilities": list(action.required_capabilities),
                "expected_artifact": action.expected_artifact,
                "verifier": action.verifier,
                "risk": action.risk,
                "timeout_seconds": action.timeout_seconds,
                "target": self.state.goal.targets[0] if self.state.goal.targets else "",
                "evidence_refs": [node.evidence_id for node in self.evidence if is_trusted_evidence(node)],
                "host_assertion_refs": [node.evidence_id for node in host_assertions],
                "output_contract": SemanticVerifier.output_contract(action.verifier),
                "parameters": dict(action.parameters),
                "phase": "verify-observation" if verification_required else ARTIFACT_PHASES.get(action.expected_artifact, "discovery"),
                "trigger": (
                    "host_assertion_requires_independent_tool_execution"
                    if verification_required
                    else "dependencies_and_fact_overlay_valid"
                ),
                "feedback_gate": (
                    {
                        "gate_id": f"{action.action_id}:evidence-trust",
                        "predicate": "independent_tool_evidence",
                        "inputs": ["runtime_or_mcp_tool_result", "fresh_raw_output", "lineage"],
                        "expected": True,
                        "operator": "eq",
                        "on_pass": "semantic_verification",
                        "on_fail": "remain_waiting_host",
                    }
                    if verification_required
                    else self._gate(action)
                ),
                "exit_condition": {"artifact_verified": action.expected_artifact},
                "failure_policy": "retry_then_fallback_then_reconcile_or_host_handoff",
                "execution_channel": "host-agent" if self.handoff or self.missing_capabilities else "direct-mcp",
                "tool_strategy": action.tool_strategy,
                "min_tool_results": action.min_tool_results,
                "max_tool_results": action.max_tool_results,
                "successful_tools": list(self.state.action_tools_succeeded.get(action.action_id, ())),
                "verification_requirement": (
                    {
                        "required": True,
                        "mode": "independent_tool_execution",
                        "rule": "Do not reuse or cite host assertion IDs. Execute the action with a Runtime/MCP tool and return fresh raw output.",
                    }
                    if verification_required
                    else {"required": False}
                ),
            }
            if self.handoff:
                next_action_spec["handoff"] = dict(self.handoff)

        evidence_summary: list[dict[str, Any]] = []
        for node in self.evidence:
            item = node.to_dict()
            payload_bytes = len(json.dumps(item.get("payload"), ensure_ascii=False, default=str).encode("utf-8"))
            if is_host_assertion(node):
                item.pop("payload", None)
                item.update(
                    {
                        "payload_omitted": True,
                        "payload_bytes": payload_bytes,
                        "payload_reason": "unverified_host_assertion",
                    }
                )
            elif payload_bytes > MAX_INLINE_EVIDENCE_BYTES:
                item.pop("payload", None)
                item.update({"payload_omitted": True, "payload_bytes": payload_bytes})
            evidence_summary.append(item)

        budget = self.state.budget.to_dict()
        budget["remaining_actions"] = max(0, self.state.budget.action_limit - self.state.budget.actions_used)
        budget["remaining_tokens"] = (
            None
            if self.state.budget.token_limit is None or self.state.budget.tokens_used is None
            else max(0, self.state.budget.token_limit - self.state.budget.tokens_used)
        )
        budget["exhausted"] = bool(self.state.budget.exhaustion_reason())
        return {
            "run_id": self.state.run_id,
            "session_id": self.state.session_id,
            "status": self.state.status,
            "workflow_id": self.workflow.workflow_id,
            "branch_id": self.state.branch_id,
            "plan_id": self.state.plan_id,
            "plan_revision": self.state.plan_revision,
            "current_action": self.state.current_action_id,
            "next_action": self.next_action,
            "next_action_spec": next_action_spec,
            "missing_capabilities": list(self.missing_capabilities),
            "credential_refs": list(self.state.credential_refs),
            "dependencies": {key: dict(value) for key, value in self.state.dependencies.items()},
            "budget": budget,
            "cleanup_status": self.state.cleanup_status,
            "cancel_reason": self.state.cancel_reason,
            "evidence": evidence_summary,
            "terminal": {
                "terminal": self.terminal.terminal,
                "success": self.terminal.success,
                "reason": self.terminal.reason,
                "satisfied": list(self.terminal.satisfied),
                "missing": list(self.terminal.missing),
            },
        }

