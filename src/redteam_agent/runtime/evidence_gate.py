from __future__ import annotations

"""Pure evidence promotion rules shared by verification and terminal judging.

The gate deliberately has no store, filesystem, model, or provider dependency.  It
only evaluates immutable records supplied by callers, which keeps promotion rules
consistent while leaving persistence and business predicates at their boundaries.
"""

import hashlib
import json
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from .evidence_trust import is_trusted_evidence, valid_evidence_trust
from .models import EvidenceNode, EvidenceProvenance, TaskAttempt, ToolCallResult


@dataclass(frozen=True)
class EvidenceGateDecision:
    """A deterministic validation result suitable for logs and tests."""

    passed: bool
    reason: str = ""


class EvidenceGate:
    """Single source of truth for evidence scope and lineage invariants."""

    @staticmethod
    def canonical_json(value: Any) -> str:
        return json.dumps(value, ensure_ascii=False, sort_keys=True, default=str)

    @classmethod
    def content_hash(cls, payload: Any) -> str:
        return hashlib.sha256(cls.canonical_json(payload).encode("utf-8")).hexdigest()

    @staticmethod
    def trusted(node: Any) -> bool:
        if isinstance(node, Mapping):
            return bool(
                node.get("verified", False)
                and str(node.get("evidence_trust") or node.get("trust") or "runtime_verified") in {"runtime_verified", "tool_verified"}
                and not str(node.get("tool") or "").startswith("host:")
            )
        return is_trusted_evidence(node)

    @staticmethod
    def valid_trust(node: Any) -> bool:
        if isinstance(node, Mapping):
            if EvidenceGate.trusted(node):
                return True
            return bool(
                node.get("verified") is False
                and str(node.get("trust") or node.get("evidence_trust") or "") == "host_asserted"
                and str(node.get("artifact_type") or "") == "host_observation"
                and str(node.get("tool") or "").startswith("host:")
            )
        return valid_evidence_trust(node)

    @classmethod
    def same_scope(
        cls,
        node: EvidenceNode,
        *,
        run_id: str,
        branch_id: str,
        target: str | None = None,
        max_plan_revision: int | None = None,
        require_trusted: bool = True,
    ) -> bool:
        provenance = node.provenance
        if require_trusted and not cls.trusted(node):
            return False
        if (
            node.run_id != run_id
            or not provenance
            or provenance.run_id != run_id
            or provenance.branch_id != branch_id
        ):
            return False
        if target not in (None, "") and node.target != target:
            return False
        if max_plan_revision is not None and provenance.plan_revision > max_plan_revision:
            return False
        return True

    @classmethod
    def valid_node_identity(
        cls,
        provenance: EvidenceProvenance | None,
        *,
        run_id: str,
        action_id: str,
        target: str,
        tool: str,
        attempt: TaskAttempt | None,
        final_required: bool,
    ) -> bool:
        if provenance is None or attempt is None or attempt.tool != tool:
            return False
        if (
            provenance.run_id,
            provenance.branch_id,
            provenance.plan_revision,
            provenance.action_id,
            provenance.attempt_id,
            provenance.tool,
            provenance.tool_version,
            provenance.input_hash,
            provenance.target,
        ) != (
            run_id,
            attempt.branch_id,
            attempt.plan_revision,
            action_id,
            attempt.attempt_id,
            tool,
            attempt.tool_version,
            attempt.input_hash,
            target,
        ):
            return False
        allowed = {"completed", "succeeded"} if final_required else {
            "running",
            "reconciling",
            "consumed",
            "completed",
            "succeeded",
        }
        return attempt.status in allowed

    @classmethod
    def valid_attempt_payload(
        cls,
        node_payload: Any,
        provenance: EvidenceProvenance | None,
        attempt: TaskAttempt,
        *,
        normalize_output: Callable[[Any], Mapping[str, Any]] | None = None,
    ) -> bool:
        if provenance is None or attempt.status not in {"completed", "succeeded"} or not isinstance(attempt.result, Mapping):
            return False
        try:
            result = ToolCallResult.from_dict(attempt.result)
        except (TypeError, ValueError, OverflowError):
            return False
        if result.status != "success":
            return False
        if result.input_hash and result.input_hash != provenance.input_hash:
            return False
        if result.output_hash and provenance.output_hash and result.output_hash != provenance.output_hash:
            return False
        if normalize_output is None:
            normalized = cls._normalize_output(result.output)
        else:
            normalized = normalize_output(result.output)
        return normalized == node_payload

    @staticmethod
    def _normalize_output(output: Any) -> Mapping[str, Any]:
        if isinstance(output, Mapping):
            structured = output.get("structuredContent")
            if isinstance(structured, Mapping):
                return dict(structured)
            artifact = output.get("artifact")
            if isinstance(artifact, Mapping):
                return dict(artifact)
            return dict(output)
        if isinstance(output, str):
            stripped = output.strip()
            if stripped:
                try:
                    decoded = json.loads(stripped)
                except json.JSONDecodeError:
                    return {"output": stripped}
                return dict(decoded) if isinstance(decoded, Mapping) else {"output": decoded}
            return {}
        if output is None:
            return {}
        return {"output": output}

    @classmethod
    def validate_parent_ids(
        cls,
        parent_ids: Sequence[str],
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        run_id: str,
        target: str,
        provenance: EvidenceProvenance | None = None,
        require_trusted: bool = True,
    ) -> EvidenceGateDecision:
        parents = tuple(str(item) for item in parent_ids)
        if len(set(parents)) != len(parents):
            return EvidenceGateDecision(False, "evidence_parent_duplicate")
        missing = set(parents) - set(evidence_by_id)
        if missing:
            return EvidenceGateDecision(False, f"evidence_parent_missing:{sorted(missing)}")
        for parent in (evidence_by_id[item] for item in parents):
            if parent.run_id != run_id:
                return EvidenceGateDecision(False, "evidence_parent_run_mismatch")
            if target and parent.target and parent.target != target:
                return EvidenceGateDecision(False, "evidence_parent_target_mismatch")
            if require_trusted and not cls.trusted(parent):
                return EvidenceGateDecision(False, "evidence_parent_untrusted")
            if provenance is not None:
                parent_provenance = parent.provenance
                if (
                    parent_provenance is None
                    or parent_provenance.branch_id != provenance.branch_id
                    or parent_provenance.plan_revision > provenance.plan_revision
                ):
                    return EvidenceGateDecision(False, "evidence_parent_branch_or_revision_mismatch")
        if provenance is not None and tuple(provenance.parent_ids) != parents:
            return EvidenceGateDecision(False, "evidence_provenance_parent_mismatch")
        return EvidenceGateDecision(True, "parents_valid")

    @classmethod
    def validate_promotion(
        cls,
        node: EvidenceNode,
        *,
        run_id: str,
        action_id: str | None = None,
        target: str | None = None,
        tool: str | None = None,
        attempt: TaskAttempt | None = None,
        evidence_by_id: Mapping[str, EvidenceNode] | None = None,
        normalize_output: Callable[[Any], Mapping[str, Any]] | None = None,
        final_required: bool = True,
    ) -> EvidenceGateDecision:
        if node.run_id != run_id or (action_id is not None and node.action_id != action_id):
            return EvidenceGateDecision(False, "evidence_attempt_identity_mismatch")
        if target not in (None, "") and node.target != target:
            return EvidenceGateDecision(False, "evidence_target_mismatch")
        if tool not in (None, "") and node.tool != tool:
            return EvidenceGateDecision(False, "evidence_attempt_identity_mismatch")
        if not valid_evidence_trust(node):
            return EvidenceGateDecision(False, "evidence_trust_invalid")
        if node.content_hash != cls.content_hash(node.payload):
            return EvidenceGateDecision(False, "evidence_content_hash_mismatch")
        provenance = node.provenance
        if not cls.valid_node_identity(
            provenance,
            run_id=run_id,
            action_id=node.action_id,
            target=node.target,
            tool=node.tool,
            attempt=attempt,
            final_required=final_required,
        ):
            return EvidenceGateDecision(False, "evidence_attempt_identity_mismatch")
        if final_required and (attempt is None or not cls.valid_attempt_payload(
            node.payload,
            provenance,  # type: ignore[arg-type]
            attempt,
            normalize_output=normalize_output,
        )):
            return EvidenceGateDecision(False, "evidence_attempt_result_mismatch")
        if evidence_by_id is not None:
            parent_result = cls.validate_parent_ids(
                node.parent_ids,
                evidence_by_id,
                run_id=run_id,
                target=node.target,
                provenance=provenance,
            )
            if not parent_result.passed:
                return parent_result
        return EvidenceGateDecision(True, "evidence_promotion_valid")

    @classmethod
    def lineage_error(
        cls,
        evidence: Sequence[EvidenceNode],
        *,
        require_trusted: bool = False,
    ) -> str:
        by_id = {node.evidence_id: node for node in evidence}
        for node in evidence:
            if require_trusted and not cls.trusted(node):
                continue
            if any(parent_id not in by_id for parent_id in node.parent_ids):
                return "evidence_lineage_missing_parent"
        visiting: set[str] = set()
        visited: set[str] = set()

        def visit(evidence_id: str) -> bool:
            if evidence_id in visiting:
                return False
            if evidence_id in visited:
                return True
            current = by_id.get(evidence_id)
            if current is None:
                return True
            visiting.add(evidence_id)
            for parent_id in current.parent_ids:
                if not visit(parent_id):
                    return False
            visiting.remove(evidence_id)
            visited.add(evidence_id)
            return True

        for evidence_id in by_id:
            if not visit(evidence_id):
                return "evidence_lineage_cycle"
        return ""

    @classmethod
    def eligible_evidence(
        cls,
        evidence: Sequence[EvidenceNode],
        *,
        run_id: str,
        branch_id: str,
        target: str | None = None,
        max_plan_revision: int | None = None,
        include_unverified: bool = False,
    ) -> tuple[EvidenceNode, ...]:
        result: list[EvidenceNode] = []
        for node in evidence:
            if not cls.same_scope(
                node,
                run_id=run_id,
                branch_id=branch_id,
                target=target,
                max_plan_revision=max_plan_revision,
                require_trusted=not include_unverified,
            ):
                continue
            if node.content_hash != cls.content_hash(node.payload):
                continue
            result.append(node)
        return tuple(result)

    @classmethod
    def clause_support_types(
        cls,
        evidence_ids: Sequence[str],
        clause_id: str,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        run_id: str,
        branch_id: str,
        target: str,
        max_plan_revision: int | None = None,
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
            if current is None or not cls.same_scope(
                current,
                run_id=run_id,
                branch_id=branch_id,
                target=target,
                max_plan_revision=max_plan_revision,
            ):
                continue
            raw_support = current.payload.get("clause_support") if isinstance(current.payload, Mapping) else None
            if isinstance(raw_support, Mapping) and clause_id in {str(key) for key in raw_support}:
                supported.add(current.artifact_type)
            stack.extend(current.parent_ids)
        return supported

    @classmethod
    def validate_finding(
        cls,
        finding: Any,
        evidence_by_id: Mapping[str, EvidenceNode],
        *,
        run_id: str,
        branch_id: str,
        target: str = "",
        max_plan_revision: int | None = None,
    ) -> EvidenceGateDecision:
        """Validate a Finding's evidence groups without persisting or promoting it."""

        def field(name: str) -> Any:
            return finding.get(name) if isinstance(finding, Mapping) else getattr(finding, name, None)

        if str(field("run_id") or "") != run_id:
            return EvidenceGateDecision(False, "finding_run_mismatch")
        finding_target = str(field("target") or target or "")
        if target and finding_target != target:
            return EvidenceGateDecision(False, "finding_target_mismatch")
        groups = {
            "reproduction_evidence_ids": "reproduction_evidence_missing",
            "impact_evidence_ids": "impact_evidence_missing",
            "negative_control_evidence_ids": "negative_control_evidence_missing",
            "cleanup_evidence_ids": "cleanup_evidence_missing",
        }
        for name, reason in groups.items():
            raw = field(name)
            refs = tuple(str(item) for item in raw if str(item)) if isinstance(raw, (list, tuple)) else ()
            if not refs:
                return EvidenceGateDecision(False, reason)
            if len(refs) != len(set(refs)):
                return EvidenceGateDecision(False, "finding_evidence_duplicate")
            for evidence_id in refs:
                node = evidence_by_id.get(evidence_id)
                if node is None or not cls.same_scope(
                    node,
                    run_id=run_id,
                    branch_id=branch_id,
                    target=finding_target,
                    max_plan_revision=max_plan_revision,
                ):
                    return EvidenceGateDecision(False, "finding_evidence_scope_mismatch")
        return EvidenceGateDecision(True, "finding_evidence_valid")


__all__ = ["EvidenceGate", "EvidenceGateDecision"]
