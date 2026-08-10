from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from .models import ActionSpec, EvidenceNode, GoalContract, ToolCallResult
from .evidence_trust import is_trusted_evidence
from .security import redact_sensitive


@dataclass(frozen=True)
class VerificationDecision:
    passed: bool
    payload: Mapping[str, Any]
    confidence: float
    reason: str
    parent_ids: tuple[str, ...] = ()


DERIVED_ARTIFACTS = {
    "hypothesis_queue",
    "reproduction_artifact",
    "impact_proof",
    "coverage_report",
    "cleanup_proof",
    "final_report",
}
MAX_EVIDENCE_BYTES = 4 * 1024 * 1024
VERIFIER_VERSION = "semantic-v2"
OUTPUT_CONTRACTS: dict[str, Mapping[str, Any]] = {
    "surface_map": {"any_of": ["targets", "assets", "hosts", "routes", "services", "files", "components", "results"]},
    "hypothesis_queue": {
        "required": ["evidence_refs", "hypotheses"],
        "hypotheses": "non-empty array of {id, statement, priority, evidence_refs, negative_control}",
    },
    "reproduction_artifact": {
        "required": ["reproducible", "evidence_refs", "negative_controls", "side_effects"],
        "reproducible": True,
        "any_of": ["request_response", "commands", "tests", "observations", "results", "transcript"],
    },
    "impact_proof": {
        "required": ["evidence_refs"],
        "one_of": [{"verified": True}, {"measured": True}],
        "any_of": ["impact", "outcomes", "before_after", "observations", "results", "metrics"],
    },
    "coverage_report": {
        "required": ["evidence_refs"],
        "any_of": ["checked", "coverage", "matrix", "metrics"],
        "control_any_of": ["negative_controls", "false_positive_controls", "excluded"],
    },
    "cleanup_proof": {
        "required": ["evidence_refs", "verified", "outstanding_changes"],
        "verified": True,
        "outstanding_changes": [],
        "any_of": ["actions", "rollback", "results", "checks"],
    },
    "final_report": {
        "required": ["evidence_refs", "goal_result", "criteria", "clause_results"],
        "goal_result": ["achieved", "not_achieved", "partial"],
        "criteria": "exact GoalContract criteria with status and evidence_refs",
        "clause_results": "exact PromptRewrite clause IDs with achieved status, target, and evidence_refs",
        "any_of": ["report", "findings", "summary"],
    },
}

class SemanticVerifier:
    version = VERIFIER_VERSION

    @staticmethod
    def output_contract(verifier: str) -> Mapping[str, Any]:
        contract = dict(OUTPUT_CONTRACTS.get(verifier, {"any_of": ["results", "output", "findings", "observations"]}))
        if verifier in DERIVED_ARTIFACTS:
            contract["clause_binding"] = (
                "When declaring clause_ids not present in parent evidence, include a non-empty "
                "clause_support mapping for every newly declared clause_id."
            )
        return contract

    @classmethod
    def _redact(cls, value: Any, key: str = "") -> Any:
        return redact_sensitive(value, key)

    def normalize_output(self, output: Any) -> Mapping[str, Any]:
        if isinstance(output, Mapping):
            structured = output.get("structuredContent")
            if isinstance(structured, Mapping):
                return self._redact(dict(structured))
            if "artifact" in output and isinstance(output.get("artifact"), Mapping):
                return self._redact(dict(output["artifact"]))
            content = output.get("content")
            if isinstance(content, list):
                texts = [str(item.get("text")) for item in content if isinstance(item, Mapping) and item.get("type") == "text"]
                joined = "\n".join(texts).strip()
                if joined:
                    try:
                        decoded = json.loads(joined)
                    except json.JSONDecodeError:
                        return self._redact({"output": joined, "raw_mcp": dict(output)})
                    if isinstance(decoded, Mapping):
                        return self._redact(dict(decoded))
                    return self._redact({"output": decoded, "raw_mcp": dict(output)})
            return self._redact(dict(output))
        if isinstance(output, str):
            stripped = output.strip()
            if not stripped:
                return {}
            try:
                decoded = json.loads(stripped)
            except json.JSONDecodeError:
                return self._redact({"output": stripped})
            return self._redact(dict(decoded) if isinstance(decoded, Mapping) else {"output": decoded})
        if output is None:
            return {}
        return self._redact({"output": output})

    @staticmethod
    def _nonempty(payload: Mapping[str, Any], keys: Sequence[str]) -> bool:
        for key in keys:
            value = payload.get(key)
            if isinstance(value, (str, list, tuple, dict, set)) and len(value) > 0:
                return True
            if value not in (None, False, "", [], {}, ()):
                return True
        return False

    @staticmethod
    def _evidence_refs(payload: Mapping[str, Any]) -> tuple[str, ...]:
        raw = payload.get("evidence_refs")
        if not isinstance(raw, list):
            return ()
        return tuple(dict.fromkeys(str(item) for item in raw if str(item).strip()))

    @staticmethod
    def _clause_ids(goal: GoalContract) -> tuple[str, ...]:
        raw = goal.intent_envelope.get("clause_ids") if isinstance(goal.intent_envelope, Mapping) else ()
        return tuple(str(item) for item in raw if str(item)) if isinstance(raw, (list, tuple)) else ()

    @staticmethod
    def _clause_required_artifacts(goal: GoalContract) -> dict[str, set[str]]:
        raw = goal.intent_envelope.get("clause_contracts") if isinstance(goal.intent_envelope, Mapping) else ()
        if not isinstance(raw, (list, tuple)):
            return {}
        return {
            str(item.get("clause_id")): {str(value) for value in item.get("required_artifacts", ()) if str(value)}
            for item in raw
            if isinstance(item, Mapping) and item.get("clause_id") and isinstance(item.get("required_artifacts"), list)
        }

    @staticmethod
    def _same_scope(node: EvidenceNode, *, run_id: str, branch_id: str, target: str) -> bool:
        return (
            is_trusted_evidence(node)
            and node.run_id == run_id
            and node.target == target
            and node.provenance is not None
            and node.provenance.run_id == run_id
            and node.provenance.branch_id == branch_id
        )

    @classmethod
    def _normalize_clause_ids(cls, payload: Mapping[str, Any], goal: GoalContract) -> tuple[tuple[str, ...], str]:
        raw = payload.get("clause_ids")
        if raw is None:
            return (), ""
        if not isinstance(raw, list):
            return (), "clause_ids_invalid"
        clause_ids = tuple(str(item) for item in raw if str(item))
        if len(clause_ids) != len(raw) or len(set(clause_ids)) != len(clause_ids):
            return (), "clause_ids_duplicate_or_invalid"
        expected = set(cls._clause_ids(goal))
        if not set(clause_ids).issubset(expected):
            return (), "clause_ids_unknown"
        return clause_ids, ""

    @staticmethod
    def _payload_clause_ids(node: EvidenceNode) -> tuple[str, ...]:
        raw = node.payload.get("clause_ids") if isinstance(node.payload, Mapping) else ()
        return tuple(str(item) for item in raw if str(item)) if isinstance(raw, list) else ()

    @staticmethod
    def _clause_contracts(goal: GoalContract) -> dict[str, Mapping[str, Any]]:
        envelope = goal.intent_envelope if isinstance(goal.intent_envelope, Mapping) else {}
        raw = envelope.get("clause_contracts")
        if not isinstance(raw, (list, tuple)):
            return {}
        return {
            str(item["clause_id"]): item
            for item in raw
            if isinstance(item, Mapping) and item.get("clause_id")
        }

    @classmethod
    def _eligible_clause_ids(cls, goal: GoalContract, artifact_type: str) -> set[str]:
        contracts = cls._clause_contracts(goal)
        if not contracts:
            return set(cls._clause_ids(goal))
        return {
            clause_id
            for clause_id, contract in contracts.items()
            if isinstance(contract.get("required_artifacts"), (list, tuple))
            and artifact_type in {str(item) for item in contract["required_artifacts"]}
        }

    @classmethod
    def _validate_clause_support(
        cls,
        payload: Mapping[str, Any],
        new_clause_ids: Sequence[str],
        *,
        goal: GoalContract,
        artifact_type: str,
    ) -> str:
        raw = payload.get("clause_support")
        if raw is None:
            support: dict[str, Any] = {}
        elif isinstance(raw, Mapping):
            support = {str(key): value for key, value in raw.items() if str(key)}
        else:
            return "clause_support_invalid"
        declared_ids = set(cls._clause_ids_from_payload(payload))
        support_ids = set(support)
        if not support_ids.issubset(declared_ids):
            return "derived_clause_support_mismatch"
        eligible = cls._eligible_clause_ids(goal, artifact_type)
        if not support_ids.issubset(eligible) or not set(new_clause_ids).issubset(eligible):
            return "clause_artifact_contract_mismatch"
        if not set(new_clause_ids).issubset(support_ids):
            return "derived_clause_support_required"
        contracts = cls._clause_contracts(goal)
        for clause_id, value in support.items():
            if not cls._nonempty({"support": value}, ("support",)):
                return "derived_clause_support_empty"
            contract = contracts.get(clause_id)
            if contract and isinstance(value, Mapping):
                source_text = value.get("source_text")
                if source_text is not None and str(source_text) != str(contract.get("source_text") or ""):
                    return "clause_support_source_mismatch"
        return ""

    @staticmethod
    def _clause_ids_from_payload(payload: Mapping[str, Any]) -> tuple[str, ...]:
        raw = payload.get("clause_ids")
        return tuple(str(item) for item in raw if str(item)) if isinstance(raw, list) else ()

    def _evidence_supports_clause(
        self,
        node: EvidenceNode,
        clause_id: str,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        run_id: str,
        branch_id: str,
        target: str,
    ) -> bool:
        return bool(
            self._evidence_clause_support_types(
                (node.evidence_id,),
                clause_id,
                evidence_by_id,
                run_id=run_id,
                branch_id=branch_id,
                target=target,
            )
        )

    def _evidence_clause_support_types(
        self,
        evidence_ids: Sequence[str],
        clause_id: str,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        run_id: str,
        branch_id: str,
        target: str,
    ) -> set[str]:
        stack = [str(item) for item in evidence_ids]
        visited: set[str] = set()
        supported: set[str] = set()
        while stack:
            evidence_id = stack.pop()
            if evidence_id in visited:
                continue
            visited.add(evidence_id)
            current = evidence_by_id.get(evidence_id)
            if current is None or not self._same_scope(current, run_id=run_id, branch_id=branch_id, target=target):
                continue
            raw_support = current.payload.get("clause_support") if isinstance(current.payload, Mapping) else None
            if isinstance(raw_support, Mapping) and clause_id in {str(key) for key in raw_support}:
                supported.add(current.artifact_type)
            stack.extend(current.parent_ids)
        return supported

    def _validate_schema(self, verifier: str, payload: Mapping[str, Any]) -> tuple[bool, str]:
        if verifier == "surface_map":
            passed = self._nonempty(
                payload,
                ("targets", "assets", "hosts", "addresses", "routes", "services", "files", "components", "surface", "results", "output"),
            )
            return passed, "surface_evidence_missing" if not passed else "surface_verified"
        if verifier == "hypothesis_queue":
            hypotheses = payload.get("hypotheses")
            passed = (
                isinstance(hypotheses, list)
                and bool(hypotheses)
                and all(
                    isinstance(item, Mapping)
                    and bool(item.get("id"))
                    and bool(item.get("statement"))
                    and item.get("priority") in {"critical", "high", "medium", "low"}
                    and isinstance(item.get("evidence_refs"), list)
                    and bool(item.get("evidence_refs"))
                    and bool(item.get("negative_control"))
                    for item in hypotheses
                )
            )
            return passed, "hypotheses_missing" if not passed else "hypotheses_verified"
        if verifier == "reproduction_artifact":
            reproducible = payload.get("reproducible") is True
            concrete = self._nonempty(payload, ("request_response", "commands", "tests", "observations", "results", "transcript"))
            controls = self._nonempty(payload, ("negative_controls", "false_positive_controls"))
            side_effects_declared = "side_effects" in payload
            passed = reproducible and concrete and controls and side_effects_declared
            return passed, "reproduction_requires_concrete_replay" if not passed else "reproduction_verified"
        if verifier == "impact_proof":
            measured = payload.get("verified") is True or payload.get("measured") is True
            concrete = self._nonempty(payload, ("impact", "outcomes", "before_after", "observations", "results", "metrics"))
            passed = measured and concrete
            return passed, "impact_requires_measured_outcome" if not passed else "impact_verified"
        if verifier == "coverage_report":
            checked = self._nonempty(payload, ("checked", "coverage", "matrix", "metrics"))
            controls = self._nonempty(payload, ("negative_controls", "false_positive_controls", "excluded"))
            passed = checked and controls
            return passed, "coverage_requires_checks_and_negative_controls" if not passed else "coverage_verified"
        if verifier == "cleanup_proof":
            actions = self._nonempty(payload, ("actions", "rollback", "results", "checks"))
            verified = payload.get("verified") is True and not payload.get("outstanding_changes")
            passed = actions and verified
            return passed, "cleanup_requires_verified_rollback" if not passed else "cleanup_verified"
        if verifier == "final_report":
            report = self._nonempty(payload, ("report", "findings", "summary"))
            goal_result = payload.get("goal_result") in {"achieved", "not_achieved", "partial"}
            criteria = payload.get("criteria")
            criteria_valid = (
                isinstance(criteria, list)
                and bool(criteria)
                and all(
                    isinstance(item, Mapping)
                    and bool(item.get("criterion_id"))
                    and item.get("status") in {"achieved", "not_achieved"}
                    and isinstance(item.get("evidence_refs"), list)
                    and bool(item.get("evidence_refs"))
                    for item in criteria
                )
            )
            clause_results = payload.get("clause_results")
            clauses_valid = (
                isinstance(clause_results, list)
                and bool(clause_results)
                and all(
                    isinstance(item, Mapping)
                    and bool(item.get("clause_id"))
                    and item.get("status") in {"achieved", "not_achieved"}
                    and bool(item.get("target"))
                    and isinstance(item.get("evidence_refs"), list)
                    and bool(item.get("evidence_refs"))
                    for item in clause_results
                )
            )
            passed = report and goal_result and criteria_valid and clauses_valid
            return passed, "report_requires_goal_criteria_and_findings" if not passed else "report_verified"
        passed = self._nonempty(payload, ("results", "output", "findings", "observations"))
        return passed, "generic_evidence_missing" if not passed else "generic_verified"

    @staticmethod
    def _declares_unexecuted(value: Any) -> bool:
        markers = {
            "claim_only",
            "not_executed",
            "not run",
            "not_run",
            "simulated_only",
            "placeholder_only",
            "unverified_claim",
        }
        phrases = (
            "no tool was executed",
            "no command was executed",
            "not actually executed",
            "not actually run",
            "claimed success",
            "claim only",
            "self reported",
            "self-reported",
            "unverified claim",
            "simulated only",
            "placeholder only",
            "未执行",
            "没有执行",
            "未运行",
            "仅声明",
            "自报",
            "仅模拟",
        )

        def visit(item: Any) -> bool:
            if isinstance(item, Mapping):
                for key, child in item.items():
                    normalized = str(key).strip().casefold().replace("-", "_")
                    if normalized in markers and child not in (False, None, "", 0, [], {}):
                        return True
                    if visit(child):
                        return True
                return False
            if isinstance(item, (list, tuple, set)):
                return any(visit(child) for child in item)
            if isinstance(item, str):
                normalized = " ".join(item.strip().casefold().replace("-", "_").split())
                phrase_text = " ".join(item.strip().casefold().split())
                return normalized in markers or any(phrase in phrase_text for phrase in phrases)
            return False

        return visit(value)

    @staticmethod
    def _confidence(result: ToolCallResult, parent_count: int) -> float:
        confidence = 0.72
        if result.input_hash:
            confidence += 0.05
        if result.output_hash:
            confidence += 0.08
        if result.call_id:
            confidence += 0.03
        confidence += min(0.08, parent_count * 0.02)
        return max(0.0, min(0.95, confidence))

    def verify(
        self,
        *,
        action: ActionSpec,
        result: ToolCallResult,
        goal: GoalContract,
        available_evidence: Sequence[EvidenceNode],
        run_id: str = "",
        branch_id: str = "",
    ) -> VerificationDecision:
        if result.status != "success":
            return VerificationDecision(False, {}, 0.0, result.error or "tool_call_failed")
        payload = self.normalize_output(result.output)
        if not payload:
            return VerificationDecision(False, {}, 0.0, "empty_tool_output")
        serialized_size = len(json.dumps(payload, ensure_ascii=False, default=str).encode("utf-8"))
        if serialized_size > MAX_EVIDENCE_BYTES:
            return VerificationDecision(False, {}, 0.0, "evidence_payload_too_large")
        declared_artifact = str(payload.get("artifact_type") or payload.get("kind") or "")
        if declared_artifact and declared_artifact != action.expected_artifact:
            return VerificationDecision(False, payload, 0.0, "artifact_type_mismatch")
        declared_target = str(payload.get("target") or "")
        if goal.targets and not declared_target:
            return VerificationDecision(False, payload, 0.0, "evidence_target_required")
        if declared_target and goal.targets and declared_target not in goal.targets:
            return VerificationDecision(False, payload, 0.0, "evidence_target_mismatch")
        if self._declares_unexecuted(payload):
            return VerificationDecision(False, payload, 0.0, "unexecuted_claim_rejected")
        clause_ids, clause_error = self._normalize_clause_ids(payload, goal)
        if clause_error:
            return VerificationDecision(False, payload, 0.0, clause_error)
        if "clause_ids" in payload:
            payload = dict(payload)
            payload["clause_ids"] = list(clause_ids)
        passed, reason = self._validate_schema(action.verifier, payload)
        if not passed:
            return VerificationDecision(False, payload, 0.0, reason)
        evidence_by_id = {node.evidence_id: node for node in available_evidence}
        parent_ids = self._evidence_refs(payload)
        confidence = self._confidence(result, len(parent_ids))
        inherited_clause_ids: set[str] = set()
        if action.expected_artifact in DERIVED_ARTIFACTS:
            if not parent_ids:
                return VerificationDecision(False, payload, confidence, "derived_evidence_requires_parents")
            if not set(parent_ids).issubset(evidence_by_id):
                return VerificationDecision(False, payload, confidence, "unknown_evidence_reference")
            if any(not is_trusted_evidence(evidence_by_id[parent_id]) for parent_id in parent_ids):
                return VerificationDecision(False, payload, confidence, "unverified_evidence_reference")
            if goal.targets and any(evidence_by_id[parent_id].target != declared_target for parent_id in parent_ids):
                return VerificationDecision(False, payload, confidence, "evidence_parent_target_mismatch")
            inherited_clause_ids = {
                clause_id
                for parent_id in parent_ids
                for clause_id in self._payload_clause_ids(evidence_by_id[parent_id])
            }
        new_clause_ids = tuple(clause_id for clause_id in clause_ids if clause_id not in inherited_clause_ids)
        support_error = self._validate_clause_support(
            payload,
            new_clause_ids,
            goal=goal,
            artifact_type=action.expected_artifact,
        )
        if support_error:
            return VerificationDecision(False, payload, confidence, support_error)
        if action.expected_artifact == "final_report":
            if not run_id or not branch_id:
                return VerificationDecision(False, payload, confidence, "clause_binding_scope_required")
            expected = {item.criterion_id: item for item in goal.success_criteria}
            reported = {
                str(item.get("criterion_id") or ""): item
                for item in payload.get("criteria", ())
                if isinstance(item, Mapping)
            }
            if set(reported) != set(expected):
                return VerificationDecision(False, payload, confidence, "goal_criteria_mismatch")
            for criterion_id, criterion in expected.items():
                result_item = reported[criterion_id]
                refs = tuple(str(item) for item in result_item.get("evidence_refs", ()) if str(item))
                if not refs or not set(refs).issubset(evidence_by_id):
                    return VerificationDecision(False, payload, confidence, "goal_criterion_evidence_invalid")
                scoped_nodes = [evidence_by_id[item] for item in refs]
                if criterion.target and any(node.target != criterion.target for node in scoped_nodes):
                    return VerificationDecision(False, payload, confidence, "goal_criterion_target_mismatch")
                if not set(refs).issubset(parent_ids):
                    return VerificationDecision(False, payload, confidence, "report_evidence_ref_not_parent")
                if criterion.workflow_id and (
                    len(goal.workflow_hints) > 1 or goal.workflow_hint != criterion.workflow_id
                ):
                    prefix = f"{criterion.workflow_id}__"
                    scoped_nodes = [node for node in scoped_nodes if node.action_id.startswith(prefix)]
                scoped_types = {node.artifact_type for node in scoped_nodes}
                plan_only = (
                    goal.intent_envelope.get("action_kind") == "plan"
                    and goal.intent_envelope.get("execution_required") is False
                )
                required = (
                    {"surface_map", "hypothesis_queue"}
                    if plan_only
                    else {"reproduction_artifact", "impact_proof", "coverage_report", "cleanup_proof"}
                )
                achieved = result_item.get("status") == "achieved"
                if achieved != required.issubset(scoped_types):
                    return VerificationDecision(False, payload, confidence, "goal_criterion_status_unproven")
            if payload.get("goal_result") == "achieved" and any(
                item.get("status") != "achieved" for item in reported.values()
            ):
                return VerificationDecision(False, payload, confidence, "goal_result_exceeds_criteria")
            expected_clause_ids = self._clause_ids(goal)
            clause_results = payload.get("clause_results")
            if not expected_clause_ids or not isinstance(clause_results, list):
                return VerificationDecision(False, payload, confidence, "clause_results_missing")
            reported_clauses = [item for item in clause_results if isinstance(item, Mapping)]
            reported_clause_ids = [str(item.get("clause_id") or "") for item in reported_clauses]
            if len(reported_clauses) != len(clause_results) or len(set(reported_clause_ids)) != len(reported_clause_ids):
                return VerificationDecision(False, payload, confidence, "clause_results_duplicate_or_invalid")
            if set(reported_clause_ids) != set(expected_clause_ids):
                return VerificationDecision(False, payload, confidence, "clause_results_mismatch")
            required_by_clause = self._clause_required_artifacts(goal)
            raw_report_support = payload.get("clause_support")
            report_clause_support = (
                {str(key) for key in raw_report_support}
                if isinstance(raw_report_support, Mapping)
                else set()
            )
            for item in reported_clauses:
                target = str(item.get("target") or "")
                refs = item.get("evidence_refs")
                if item.get("status") != "achieved" or target not in goal.targets or not isinstance(refs, list) or not refs:
                    return VerificationDecision(False, payload, confidence, "clause_result_unachieved_or_unbound")
                ref_ids = [str(ref) for ref in refs if str(ref)]
                if len(ref_ids) != len(refs) or len(set(ref_ids)) != len(ref_ids) or not set(ref_ids).issubset(evidence_by_id):
                    return VerificationDecision(False, payload, confidence, "clause_result_evidence_invalid")
                if any(
                    not self._same_scope(evidence_by_id[ref_id], run_id=run_id, branch_id=branch_id, target=target)
                    for ref_id in ref_ids
                ):
                    return VerificationDecision(False, payload, confidence, "clause_result_evidence_scope_mismatch")
                if not set(ref_ids).issubset(parent_ids):
                    return VerificationDecision(False, payload, confidence, "report_evidence_ref_not_parent")
                clause_id = str(item.get("clause_id"))
                supported_types = self._evidence_clause_support_types(
                    ref_ids,
                    clause_id,
                    evidence_by_id,
                    run_id=run_id,
                    branch_id=branch_id,
                    target=target,
                )
                required_types = required_by_clause.get(clause_id, set())
                non_report_required = required_types - {"final_report"}
                report_supported = "final_report" not in required_types or clause_id in report_clause_support
                legacy_supported = not required_types and bool(supported_types)
                if not (
                    (required_types and non_report_required.issubset(supported_types) and report_supported)
                    or legacy_supported
                ):
                    return VerificationDecision(False, payload, confidence, "clause_result_evidence_unsupported")
        return VerificationDecision(True, payload, confidence, reason, parent_ids)
