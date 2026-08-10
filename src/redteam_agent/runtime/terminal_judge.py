from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any

from .models import EvidenceNode, GoalContract, OperationState, SuccessPredicate, TerminalDecision, WorkflowSpec
from .evidence_trust import is_trusted_evidence


class TerminalJudge:
    def evaluate(
        self,
        *,
        state: OperationState,
        goal: GoalContract,
        workflow: WorkflowSpec,
        evidence: Sequence[EvidenceNode],
    ) -> TerminalDecision:
        # Evidence is scoped to the active plan branch before any predicate or
        # lineage calculation. Verified completion requires durable provenance;
        # unbound legacy rows remain inspectable in storage but cannot terminate.
        scoped_evidence = tuple(
            node
            for node in evidence
            if node.provenance is not None
            and node.run_id == state.run_id
            and node.provenance.run_id == state.run_id
            and node.provenance.branch_id == state.branch_id
            and node.provenance.plan_revision <= state.plan_revision
        )
        predicates = tuple(workflow.terminal_predicates) + tuple(goal.success_predicates)
        satisfied: list[str] = []
        missing: list[str] = []
        evidence_by_type: dict[str, list[EvidenceNode]] = {}
        for node in scoped_evidence:
            if is_trusted_evidence(node):
                evidence_by_type.setdefault(node.artifact_type, []).append(node)

        for predicate in predicates:
            key = self._predicate_key(predicate)
            if self._evaluate_predicate(predicate, state, workflow, evidence_by_type):
                satisfied.append(key)
            else:
                missing.append(key)

        for artifact_type in workflow.required_artifacts:
            if not evidence_by_type.get(artifact_type):
                missing.append(f"required_artifact:{artifact_type}")

        evidence_by_action: dict[str, list[EvidenceNode]] = {}
        for node in scoped_evidence:
            if is_trusted_evidence(node):
                evidence_by_action.setdefault(node.action_id, []).append(node)
        for action in workflow.actions:
            if action.optional:
                continue
            action_nodes = [
                node
                for node in evidence_by_action.get(action.action_id, ())
                if node.artifact_type == action.expected_artifact
            ]
            if state.action_status.get(action.action_id) == "completed" and not action_nodes:
                missing.append(f"required_action_evidence:{action.action_id}")
                continue
            for dependency in action.depends_on:
                dependency_ids = {node.evidence_id for node in evidence_by_action.get(dependency, ())}
                if dependency_ids and action_nodes and not any(
                    dependency_ids.intersection(node.parent_ids) for node in action_nodes
                ):
                    missing.append(f"action_lineage:{action.action_id}:{dependency}")

        final_reports = evidence_by_type.get("final_report", ())
        if final_reports:
            achieved = any(
                isinstance(node.payload, Mapping) and node.payload.get("goal_result") == "achieved"
                for node in final_reports
            )
            if not achieved:
                missing.append("final_report.goal_result:achieved")
            if not self._report_lineage_complete(final_reports, goal, scoped_evidence):
                missing.append("final_report_lineage_complete")
            if not self._goal_criteria_complete(final_reports, goal, scoped_evidence, state):
                missing.append("goal_criteria_complete")
            if not self._goal_clauses_complete(final_reports, goal, scoped_evidence, state):
                missing.append("goal_clause_results_complete")

        lineage_error = self._lineage_error(scoped_evidence)
        if lineage_error:
            missing.append(lineage_error)
        if not goal.targets:
            missing.append("goal_targets_present")
        else:
            covered_targets = {node.target for node in scoped_evidence if is_trusted_evidence(node) and node.target}
            for target in goal.targets:
                if target not in covered_targets:
                    missing.append(f"target_evidence:{target}")

        if missing:
            return TerminalDecision(
                terminal=False,
                success=False,
                reason="goal_predicates_pending",
                satisfied=tuple(dict.fromkeys(satisfied)),
                missing=tuple(dict.fromkeys(missing)),
            )
        return TerminalDecision(
            terminal=True,
            success=True,
            reason="goal_contract_satisfied",
            satisfied=tuple(dict.fromkeys(satisfied)),
        )

    @staticmethod
    def _predicate_key(predicate: SuccessPredicate) -> str:
        return f"{predicate.kind}:{predicate.subject}" if predicate.subject else predicate.kind

    def _evaluate_predicate(
        self,
        predicate: SuccessPredicate,
        state: OperationState,
        workflow: WorkflowSpec,
        evidence_by_type: Mapping[str, Sequence[EvidenceNode]],
    ) -> bool:
        if predicate.kind == "workflow_actions_complete":
            required = [action for action in workflow.actions if not action.optional]
            return all(state.action_status.get(action.action_id) == "completed" for action in required)
        if predicate.kind == "artifact_verified":
            return bool(evidence_by_type.get(predicate.subject))
        if predicate.kind == "artifact_count":
            count = len(evidence_by_type.get(predicate.subject, ()))
            return self._compare(count, predicate.operator, predicate.value)
        if predicate.kind == "artifact_field":
            artifact_type, separator, field_name = predicate.subject.partition(".")
            if not separator:
                return False
            for node in evidence_by_type.get(artifact_type, ()):
                if isinstance(node.payload, Mapping) and self._compare(node.payload.get(field_name), predicate.operator, predicate.value):
                    return True
            return False
        return False

    @staticmethod
    def _compare(actual: Any, operator: str, expected: Any) -> bool:
        if operator == "exists":
            return actual not in (None, False, "", [], {}, ())
        if operator == "eq":
            return actual == expected
        if operator == "ne":
            return actual != expected
        try:
            if operator == "gte":
                return actual >= expected
            if operator == "gt":
                return actual > expected
            if operator == "lte":
                return actual <= expected
            if operator == "lt":
                return actual < expected
        except TypeError:
            return False
        if operator == "contains":
            try:
                return expected in actual
            except TypeError:
                return False
        return False

    @staticmethod
    def _lineage_error(evidence: Sequence[EvidenceNode]) -> str:
        by_id = {node.evidence_id: node for node in evidence}
        for node in evidence:
            if any(parent_id not in by_id for parent_id in node.parent_ids):
                return "evidence_lineage_missing_parent"
        visiting: set[str] = set()
        visited: set[str] = set()

        def visit(evidence_id: str) -> bool:
            if evidence_id in visiting:
                return False
            if evidence_id in visited:
                return True
            visiting.add(evidence_id)
            for parent_id in by_id[evidence_id].parent_ids:
                if not visit(parent_id):
                    return False
            visiting.remove(evidence_id)
            visited.add(evidence_id)
            return True

        for evidence_id in by_id:
            if not visit(evidence_id):
                return "evidence_lineage_cycle"
        return ""

    @staticmethod
    def _completion_artifacts(goal: GoalContract) -> set[str]:
        envelope = goal.intent_envelope if isinstance(goal.intent_envelope, Mapping) else {}
        if envelope.get("action_kind") == "plan" and envelope.get("execution_required") is False:
            return {"surface_map", "hypothesis_queue"}
        return {"reproduction_artifact", "impact_proof", "coverage_report", "cleanup_proof"}

    @staticmethod
    def _report_lineage_complete(
        final_reports: Sequence[EvidenceNode],
        goal: GoalContract,
        evidence: Sequence[EvidenceNode],
    ) -> bool:
        by_id = {node.evidence_id: node for node in evidence}
        required = TerminalJudge._completion_artifacts(goal)
        for report in final_reports:
            stack = list(report.parent_ids)
            visited: set[str] = set()
            artifact_types: set[str] = set()
            while stack:
                evidence_id = stack.pop()
                if evidence_id in visited or evidence_id not in by_id:
                    continue
                visited.add(evidence_id)
                node = by_id[evidence_id]
                artifact_types.add(node.artifact_type)
                stack.extend(node.parent_ids)
            if required.issubset(artifact_types):
                return True
        return False

    @staticmethod
    def _goal_criteria_complete(
        final_reports: Sequence[EvidenceNode],
        goal: GoalContract,
        evidence: Sequence[EvidenceNode],
        state: OperationState,
    ) -> bool:
        expected = {criterion.criterion_id: criterion for criterion in goal.success_criteria}
        by_id = {node.evidence_id: node for node in evidence if is_trusted_evidence(node)}
        required = TerminalJudge._completion_artifacts(goal)
        for report in final_reports:
            if not isinstance(report.payload, Mapping):
                continue
            reported = {
                str(item.get("criterion_id") or ""): item
                for item in report.payload.get("criteria", ())
                if isinstance(item, Mapping)
            }
            if not expected or set(reported) != set(expected):
                continue
            valid = True
            for criterion_id, criterion in expected.items():
                item = reported[criterion_id]
                raw_refs = item.get("evidence_refs")
                if not isinstance(raw_refs, list) or not raw_refs:
                    valid = False
                    break
                refs = [str(value) for value in raw_refs if str(value)]
                if (
                    len(refs) != len(raw_refs)
                    or len(set(refs)) != len(refs)
                    or not set(refs).issubset(by_id)
                    or not set(refs).issubset(report.parent_ids)
                ):
                    valid = False
                    break
                nodes = [by_id[value] for value in refs]
                if any(
                    node.run_id != state.run_id
                    or node.provenance is None
                    or node.provenance.run_id != state.run_id
                    or node.provenance.branch_id != state.branch_id
                    or node.provenance.plan_revision > state.plan_revision
                    for node in nodes
                ):
                    valid = False
                    break
                if criterion.target and any(node.target != criterion.target for node in nodes):
                    valid = False
                    break
                if criterion.workflow_id and (
                    len(goal.workflow_hints) > 1 or goal.workflow_hint != criterion.workflow_id
                ):
                    prefix = f"{criterion.workflow_id}__"
                    nodes = [node for node in nodes if node.action_id.startswith(prefix)]
                if item.get("status") != "achieved" or not required.issubset({node.artifact_type for node in nodes}):
                    valid = False
                    break
            if valid:
                return True
        return False

    @staticmethod
    def _goal_clauses_complete(
        final_reports: Sequence[EvidenceNode],
        goal: GoalContract,
        evidence: Sequence[EvidenceNode],
        state: OperationState,
    ) -> bool:
        """Require each original PromptRewrite clause to be evidence-bound.

        Generic reproduction/impact/coverage/cleanup artifacts are necessary,
        but they do not demonstrate that every user clause was completed.
        """
        raw_ids = goal.intent_envelope.get("clause_ids") if isinstance(goal.intent_envelope, Mapping) else ()
        expected_ids = tuple(str(item) for item in raw_ids if str(item)) if isinstance(raw_ids, (list, tuple)) else ()
        if not expected_ids:
            return False
        raw_contracts = goal.intent_envelope.get("clause_contracts") if isinstance(goal.intent_envelope, Mapping) else ()
        required_by_clause = {
            str(item.get("clause_id")): {str(value) for value in item.get("required_artifacts", ()) if str(value)}
            for item in raw_contracts
            if isinstance(item, Mapping) and item.get("clause_id") and isinstance(item.get("required_artifacts"), list)
        } if isinstance(raw_contracts, (list, tuple)) else {}
        by_id = {node.evidence_id: node for node in evidence if is_trusted_evidence(node)}
        for report in final_reports:
            if not isinstance(report.payload, Mapping):
                continue
            raw_results = report.payload.get("clause_results")
            if not isinstance(raw_results, list) or len(raw_results) != len(expected_ids):
                continue
            results = [item for item in raw_results if isinstance(item, Mapping)]
            ids = [str(item.get("clause_id") or "") for item in results]
            if len(results) != len(raw_results) or len(set(ids)) != len(ids) or set(ids) != set(expected_ids):
                continue
            raw_report_support = report.payload.get("clause_support")
            report_clause_support = (
                {str(item) for item in raw_report_support}
                if isinstance(raw_report_support, Mapping)
                else set()
            )
            valid = True
            for item in results:
                target = str(item.get("target") or "")
                refs = item.get("evidence_refs")
                if item.get("status") != "achieved" or target not in goal.targets or not isinstance(refs, list) or not refs:
                    valid = False
                    break
                ref_ids = [str(ref) for ref in refs if str(ref)]
                if (
                    len(ref_ids) != len(refs)
                    or len(set(ref_ids)) != len(ref_ids)
                    or not set(ref_ids).issubset(by_id)
                    or not set(ref_ids).issubset(report.parent_ids)
                ):
                    valid = False
                    break
                for ref_id in ref_ids:
                    node = by_id[ref_id]
                    if (
                        node.run_id != state.run_id
                        or node.target != target
                        or node.provenance is None
                        or node.provenance.run_id != state.run_id
                        or node.provenance.branch_id != state.branch_id
                    ):
                        valid = False
                        break
                clause_id = str(item.get("clause_id") or "")
                supported_types = TerminalJudge._evidence_clause_support_types(
                    ref_ids,
                    clause_id,
                    by_id,
                    state=state,
                    target=target,
                )
                required_types = required_by_clause.get(clause_id, set())
                non_report_required = required_types - {"final_report"}
                report_supported = "final_report" not in required_types or clause_id in report_clause_support
                legacy_supported = not required_types and bool(supported_types)
                if valid and not (
                    (required_types and non_report_required.issubset(supported_types) and report_supported)
                    or legacy_supported
                ):
                    valid = False
                if not valid:
                    break
            if valid:
                return True
        return False

    @staticmethod
    def _evidence_supports_clause(
        node: EvidenceNode,
        clause_id: str,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        state: OperationState,
        target: str,
    ) -> bool:
        return bool(
            TerminalJudge._evidence_clause_support_types(
                (node.evidence_id,),
                clause_id,
                evidence_by_id,
                state=state,
                target=target,
            )
        )

    @staticmethod
    def _evidence_clause_support_types(
        evidence_ids: Sequence[str],
        clause_id: str,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        state: OperationState,
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
            if (
                current is None
                or not is_trusted_evidence(current)
                or current.run_id != state.run_id
                or current.target != target
                or current.provenance is None
                or current.provenance.run_id != state.run_id
                or current.provenance.branch_id != state.branch_id
            ):
                continue
            raw_support = current.payload.get("clause_support") if isinstance(current.payload, Mapping) else None
            if isinstance(raw_support, Mapping) and clause_id in {str(item) for item in raw_support}:
                supported.add(current.artifact_type)
            stack.extend(current.parent_ids)
        return supported
