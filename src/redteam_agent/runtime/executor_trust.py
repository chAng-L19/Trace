from __future__ import annotations

import json
from dataclasses import replace
from typing import Any, Mapping, Sequence

from .evidence_trust import HOST_ASSERTED, is_host_assertion
from .executor_common import ExecutionOutcome
from .models import (
    ActionSpec,
    EvidenceNode,
    EvidenceProvenance,
    LeaseToken,
    OperationState,
    ReviewRecord,
    TaskAttempt,
    ToolCallResult,
    ToolDescriptor,
    WorkflowSpec,
    utc_now,
)
from .verifier import MAX_EVIDENCE_BYTES, VerificationDecision


class ExecutorTrustMixin:
    """Fail-closed ingestion for observations carried by the Host Agent."""

    def host_assertions_for_action(
        self,
        state: OperationState,
        action: ActionSpec,
    ) -> tuple[EvidenceNode, ...]:
        return tuple(
            node
            for node in self.evidence_graph.list(state.run_id, include_unverified=True)
            if is_host_assertion(node)
            and node.action_id == action.action_id
            and node.provenance is not None
            and node.provenance.branch_id == state.branch_id
            and node.provenance.plan_revision <= state.plan_revision
        )

    def verification_decision(
        self,
        *,
        state: OperationState,
        action: ActionSpec,
        descriptor: ToolDescriptor,
        result: ToolCallResult,
        available_evidence: Sequence[EvidenceNode],
    ) -> VerificationDecision:
        if descriptor.source != "host-receipt":
            return self.verifier.verify(
                action=action,
                result=result,
                goal=state.goal,
                available_evidence=available_evidence,
                run_id=state.run_id,
                branch_id=state.branch_id,
            )
        if result.status != "success":
            return VerificationDecision(False, {}, 0.0, result.error or "host_observation_failed")
        payload = self.verifier.normalize_output(result.output)
        if not payload:
            return VerificationDecision(False, {}, 0.0, "empty_host_observation")
        size = len(json.dumps(payload, ensure_ascii=False, default=str).encode("utf-8"))
        if size > MAX_EVIDENCE_BYTES:
            return VerificationDecision(False, {}, 0.0, "host_observation_too_large")
        return VerificationDecision(True, payload, 0.0, "host_observation_asserted")

    def _finish_host_assertion(
        self,
        *,
        state: OperationState,
        action: ActionSpec,
        descriptor: ToolDescriptor,
        attempt: TaskAttempt,
        lease_token: LeaseToken,
        result: ToolCallResult,
        decision: VerificationDecision,
        expected_attempt_status: str,
    ) -> ExecutionOutcome:
        expected_version = state.state_version
        target = state.goal.targets[0] if state.goal.targets else ""
        provenance = EvidenceProvenance(
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=attempt.plan_revision,
            action_id=action.action_id,
            attempt_id=attempt.attempt_id,
            tool=descriptor.qualified_name,
            tool_version=descriptor.version,
            input_hash=result.input_hash or attempt.input_hash,
            output_hash=result.output_hash or self.broker.canonical_hash(result.output),
            verifier="host-assertion-v1",
            verifier_version=self.verifier.version,
            fact_versions=self._fact_versions(state),
            parent_ids=decision.parent_ids,
            target=target,
        )
        node = self.evidence_graph.add(
            run_id=state.run_id,
            action_id=action.action_id,
            artifact_type="host_observation",
            target=target,
            tool=descriptor.qualified_name,
            payload=dict(decision.payload),
            parent_ids=decision.parent_ids,
            verifier="host-assertion-v1",
            confidence=0.0,
            provenance=provenance,
            persist=False,
            verified=False,
            trust=HOST_ASSERTED,
        )
        if node.evidence_id not in state.evidence_ids:
            state.evidence_ids.append(node.evidence_id)
        tried = state.action_tools_tried.setdefault(action.action_id, [])
        if descriptor.qualified_name not in tried:
            tried.append(descriptor.qualified_name)
        state.action_status[action.action_id] = "pending"
        state.current_action_id = action.action_id
        state.status = "running"
        finished = replace(
            attempt,
            status="completed",
            result=result.to_dict(),
            error="independent_verification_required",
            finished_at=utc_now(),
        )
        review = ReviewRecord(
            review_id=f"review-{node.evidence_id.removeprefix('evidence-')}",
            run_id=state.run_id,
            branch_id=state.branch_id,
            plan_revision=attempt.plan_revision,
            scope="task",
            subject_id=action.action_id,
            decision="defer",
            gate_results=(
                {
                    "gate_id": f"{action.action_id}:evidence-trust",
                    "predicate": "independent_tool_evidence",
                    "passed": False,
                    "observed": HOST_ASSERTED,
                },
            ),
            evidence_ids=(node.evidence_id,),
            reason="host_observation_requires_independent_verification",
            reviewer="evidence-trust-gate-v1",
        )
        self.store.commit_action_outcome(
            state=state,
            expected_state_version=expected_version,
            attempt=finished,
            expected_attempt_status=expected_attempt_status,
            lease_token=lease_token,
            evidence=(node,),
            reviews=(review,),
            event_type="host_observation_asserted",
            event={
                "action_id": action.action_id,
                "attempt_id": attempt.attempt_id,
                "evidence_id": node.evidence_id,
                "asserted_artifact_type": action.expected_artifact,
                "trust": HOST_ASSERTED,
                "next_action": "verify-observation",
            },
        )
        return ExecutionOutcome(
            True,
            "host_observation_requires_independent_verification",
            "host_observation_asserted",
            evidence=node,
            review=review,
        )


__all__ = ["ExecutorTrustMixin"]
