from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any, Mapping, Sequence

from .durable_store import DurableStore
from .evidence_trust import RUNTIME_VERIFIED, is_trusted_evidence, valid_evidence_trust
from .models import EvidenceNode, EvidenceProvenance, TaskAttempt, ToolCallResult
from .security import redact_sensitive, secure_directory, secure_file
from .verifier import SemanticVerifier


class EvidenceGraph:
    def __init__(self, store: DurableStore, artifact_root: Path) -> None:
        self.store = store
        self.artifact_root = artifact_root
        secure_directory(self.artifact_root)

    @staticmethod
    def content_hash(payload: Any) -> str:
        serialized = json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
        return hashlib.sha256(serialized).hexdigest()

    @staticmethod
    def _attempt_identity_valid(
        provenance: EvidenceProvenance,
        *,
        run_id: str,
        action_id: str,
        target: str,
        tool: str,
        attempt: TaskAttempt | None,
        final_required: bool,
    ) -> bool:
        if attempt is None:
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

    @staticmethod
    def _attempt_payload_valid(node_payload: Any, provenance: EvidenceProvenance, attempt: TaskAttempt) -> bool:
        if attempt.status not in {"completed", "succeeded"} or not isinstance(attempt.result, Mapping):
            return False
        result = ToolCallResult.from_dict(attempt.result)
        if result.status != "success":
            return False
        if result.input_hash and result.input_hash != provenance.input_hash:
            return False
        if result.output_hash and provenance.output_hash and result.output_hash != provenance.output_hash:
            return False
        normalized = SemanticVerifier().normalize_output(result.output)
        return normalized == node_payload

    def add(
        self,
        *,
        run_id: str,
        action_id: str,
        artifact_type: str,
        target: str,
        tool: str,
        payload: Any,
        parent_ids: Sequence[str],
        verifier: str,
        confidence: float,
        provenance: EvidenceProvenance | None = None,
        persist: bool = True,
        verified: bool = True,
        trust: str = RUNTIME_VERIFIED,
    ) -> EvidenceNode:
        if provenance is None:
            raise ValueError("evidence_provenance_required")
        payload = redact_sensitive(payload)
        state = self.store.load_operation(run_id)
        if state is None:
            raise ValueError(f"evidence_operation_missing:{run_id}")
        if target not in state.goal.targets:
            raise ValueError("evidence_target_outside_goal")
        attempts = {
            item.attempt_id: item
            for item in self.store.task_attempts(run_id, action_id=action_id)
        }
        attempt = attempts.get(provenance.attempt_id)
        if not self._attempt_identity_valid(
            provenance,
            run_id=run_id,
            action_id=action_id,
            target=target,
            tool=tool,
            attempt=attempt,
            final_required=persist,
        ):
            raise ValueError("evidence_attempt_identity_mismatch")
        if persist and (attempt is None or not self._attempt_payload_valid(payload, provenance, attempt)):
            raise ValueError("evidence_attempt_result_mismatch")

        stored_nodes = self.store.evidence(run_id)
        existing = {node.evidence_id: node for node in stored_nodes}
        missing_parents = set(parent_ids) - set(existing)
        if missing_parents:
            raise ValueError(f"evidence_parent_missing:{sorted(missing_parents)}")
        parents = tuple(existing[parent_id] for parent_id in parent_ids)
        if any(parent.run_id != run_id for parent in parents):
            raise ValueError("evidence_parent_run_mismatch")
        if target and any(parent.target and parent.target != target for parent in parents):
            raise ValueError("evidence_parent_target_mismatch")
        if tuple(provenance.parent_ids) != tuple(parent_ids):
            raise ValueError("evidence_provenance_parent_mismatch")
        if any(
            parent.provenance is None
            or parent.provenance.branch_id != provenance.branch_id
            or parent.provenance.plan_revision > provenance.plan_revision
            for parent in parents
        ):
            raise ValueError("evidence_parent_branch_or_revision_mismatch")
        digest = self.content_hash(payload)
        identity_payload = {
            "run_id": run_id,
            "action_id": action_id,
            "artifact_type": artifact_type,
            "target": target,
            "tool": tool,
            "content_hash": digest,
            "parent_ids": list(parent_ids),
            "verifier": verifier,
            "verified": bool(verified),
            "trust": trust,
            "provenance": provenance.to_dict() if provenance is not None else None,
        }
        identity = self.content_hash(identity_payload)
        node = EvidenceNode(
            evidence_id=f"evidence-{identity[:32]}",
            run_id=run_id,
            action_id=action_id,
            artifact_type=artifact_type,
            target=target,
            tool=tool,
            payload=payload,
            content_hash=digest,
            parent_ids=tuple(parent_ids),
            verifier=verifier,
            confidence=max(0.0, min(1.0, float(confidence))),
            verified=bool(verified),
            provenance=provenance,
            trust=trust,
        )
        if not valid_evidence_trust(node):
            raise ValueError("evidence_trust_invalid")
        for stored in stored_nodes:
            if stored.evidence_id == node.evidence_id:
                if not valid_evidence_trust(stored):
                    raise ValueError("stored_evidence_trust_invalid")
                self._write_artifact(stored)
                return stored
        self._write_artifact(node)
        if not persist:
            return node
        self.store.save_evidence(node)
        persisted = next((item for item in self.list(run_id) if item.evidence_id == node.evidence_id), None)
        return persisted or node

    def _write_artifact(self, node: EvidenceNode) -> Path:
        path = self._artifact_path(node)
        secure_directory(path.parent)
        temporary = path.with_suffix(".json.tmp")
        temporary.write_text(json.dumps(node.to_dict(), ensure_ascii=False, indent=2, default=str), encoding="utf-8")
        secure_file(temporary)
        temporary.replace(path)
        secure_file(path)
        return path

    def _artifact_path(self, node: EvidenceNode) -> Path:
        safe_run = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.run_id)
        safe_action = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.action_id)
        safe_type = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.artifact_type)
        tool_digest = hashlib.sha256(node.tool.encode("utf-8", errors="replace")).hexdigest()[:8]
        return self.artifact_root / safe_run / f"{safe_action}-{safe_type}-{node.content_hash[:12]}-{tool_digest}.json"

    def list(self, run_id: str, *, include_unverified: bool = False) -> tuple[EvidenceNode, ...]:
        nodes = self.store.evidence(run_id)
        state = self.store.load_operation(run_id)
        if state is None:
            return ()
        attempts = {item.attempt_id: item for item in self.store.task_attempts(run_id)}
        integrity_valid: list[EvidenceNode] = []
        for node in nodes:
            if (
                node.run_id != run_id
                or not valid_evidence_trust(node)
                or node.content_hash != self.content_hash(node.payload)
            ):
                continue
            path = self._artifact_path(node)
            source_path = path
            if not source_path.is_file():
                legacy_path = self._legacy_artifact_path(node)
                if legacy_path.is_file():
                    source_path = legacy_path
            try:
                persisted = json.loads(source_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                continue
            if not isinstance(persisted, dict):
                continue
            persisted_node = EvidenceNode.from_dict(persisted)
            if persisted_node.to_dict() != node.to_dict():
                continue
            if source_path != path:
                self._write_artifact(node)
            provenance = node.provenance
            attempt = attempts.get(provenance.attempt_id) if provenance is not None else None
            if (
                provenance is None
                or node.target not in state.goal.targets
                or tuple(provenance.parent_ids) != tuple(node.parent_ids)
                or not self._attempt_identity_valid(
                    provenance,
                    run_id=node.run_id,
                    action_id=node.action_id,
                    target=node.target,
                    tool=node.tool,
                    attempt=attempt,
                    final_required=True,
                )
                or attempt is None
                or not self._attempt_payload_valid(node.payload, provenance, attempt)
            ):
                continue
            integrity_valid.append(node)
        by_id = {node.evidence_id: node for node in integrity_valid}
        valid: list[EvidenceNode] = []
        for node in integrity_valid:
            if not set(node.parent_ids).issubset(by_id):
                continue
            parents = tuple(by_id[parent_id] for parent_id in node.parent_ids)
            if node.target and any(parent.target and parent.target != node.target for parent in parents):
                continue
            if any(
                parent.provenance is None
                or node.provenance is None
                or parent.provenance.branch_id != node.provenance.branch_id
                or parent.provenance.plan_revision > node.provenance.plan_revision
                or not is_trusted_evidence(parent)
                for parent in parents
            ):
                continue
            valid.append(node)
        return tuple(
            node for node in valid if include_unverified or is_trusted_evidence(node)
        )

    def find_by_content(
        self,
        run_id: str,
        action_id: str,
        artifact_type: str,
        digest: str,
        *,
        tool: str = "",
    ) -> EvidenceNode | None:
        for node in self.list(run_id):
            if (
                node.action_id == action_id
                and node.artifact_type == artifact_type
                and node.content_hash == digest
                and (not tool or node.tool == tool)
            ):
                return node
        return None

    def _legacy_artifact_path(self, node: EvidenceNode) -> Path:
        safe_run = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.run_id)
        safe_action = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.action_id)
        safe_type = "".join(character if character.isalnum() or character in "._-" else "_" for character in node.artifact_type)
        return self.artifact_root / safe_run / f"{safe_action}-{safe_type}-{node.content_hash[:12]}.json"

    def by_type(self, run_id: str, artifact_type: str) -> tuple[EvidenceNode, ...]:
        return tuple(node for node in self.list(run_id) if node.artifact_type == artifact_type)
