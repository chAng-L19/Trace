"""Immutable evidence packaging and report source revisions over Runtime events/CAS.

Packaging is a model declaration. Only the existing EvidenceGraph/EvidenceGate
may certify a finding; a valid blob hash never grants semantic evidence trust.
"""
from __future__ import annotations

from typing import Any

from ..core import contract_hash
from ..runtime.evidence_gate import EvidenceGate
from ..runtime.security import redact_sensitive
from .execution_steps import ExecutionSteps, event_pages, page


MANIFEST_EVENT = "http_evidence_manifest_recorded"
REPORT_EVENT = "report_source_revision_recorded"


class EvidenceRecords:
    def __init__(self, service) -> None:
        self.service = service
        self.runtime = service.runtime
        self.steps = ExecutionSteps(service)

    def _state(self, run_id: str):
        state = self.runtime.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        return state

    def _records(self, run_id: str, event_type: str) -> list[dict]:
        self._state(run_id)
        return [event["payload"] for event in event_pages(self.runtime.store, run_id)
                if event["event_type"] == event_type]

    def _existing(self, run_id: str, event_type: str, identity: str, value: str) -> dict | None:
        return next((record for record in self._records(run_id, event_type)
                     if record[identity] == value), None)

    def _artifact(self, run_id: str, artifact_id: str) -> dict:
        ref = self.service.artifact(run_id, artifact_id)
        self.runtime.artifacts.verify(artifact_id, run_id=run_id)
        return {"artifact_ref": artifact_id, "content_hash": ref.content_hash,
                "byte_count": ref.byte_count, "media_type": ref.media_type}

    def _append(self, run_id: str, event_type: str, identity: str, record: dict) -> dict:
        durable = redact_sensitive(record)
        durable["record_hash"] = contract_hash(durable)
        self.runtime.store.append_event_once(run_id, event_type, durable,
                                             identity_field=identity, fingerprint_field="record_hash")
        return durable

    def _verified(self, run_id: str, target: str) -> list[dict]:
        state = self._state(run_id)
        return [{"evidence_id": node.evidence_id, "content_hash": node.content_hash,
                 "attempt_id": node.provenance.attempt_id, "trust": node.trust}
                for node in self.runtime.evidence_graph.list(run_id)
                if EvidenceGate.same_scope(node, run_id=run_id, branch_id=state.branch_id,
                                           target=target, max_plan_revision=state.plan_revision)]

    def manifest(self, run_id: str, arguments: dict) -> dict:
        mode = arguments.get("mode", "list")
        if mode == "list":
            return page(self._records(run_id, MANIFEST_EVENT), offset=arguments.get("offset", 0),
                        limit=arguments.get("limit", 20))
        identity = arguments.get("manifest_id", "")
        if not identity:
            raise ValueError("manifest_id_required")
        existing = self._existing(run_id, MANIFEST_EVENT, "manifest_id", identity)
        if mode == "read":
            if existing is None:
                raise KeyError(f"manifest_not_found:{identity}")
            return {"record": existing, "runtime_verified_evidence": self._verified(run_id, existing["target"])}
        if mode != "record":
            raise ValueError("manifest_mode_invalid")
        request_hash = contract_hash(redact_sensitive(arguments))
        if existing is not None:
            if existing["request_hash"] != request_hash:
                raise ValueError("manifest_identity_conflict")
            return existing
        state = self._state(run_id)
        target = arguments.get("target", "")
        if not target or target not in state.goal.targets:
            raise ValueError("manifest_target_outside_run")
        exchanges = arguments.get("exchanges", [])
        if not isinstance(exchanges, list) or not 1 <= len(exchanges) <= 30:
            raise ValueError("manifest_exchanges_required")
        records = []
        for exchange in exchanges:
            role = exchange.get("role")
            if role not in {"baseline", "proof", "control"}:
                raise ValueError("manifest_exchange_role_invalid")
            step = self.steps.read(run_id, exchange.get("source_step_id", ""))
            allowed_refs = {ref["artifact_ref"] for ref in step["artifact_refs"]}
            parts = {}
            for field in ("request_artifact_id", "response_artifact_id",
                          "request_body_artifact_id", "response_body_artifact_id"):
                artifact_id = exchange.get(field)
                if not artifact_id:
                    if field in {"request_artifact_id", "response_artifact_id"}:
                        raise ValueError(f"manifest_{field}_required")
                    continue
                if artifact_id not in allowed_refs:
                    raise ValueError(f"manifest_artifact_not_in_step:{artifact_id}")
                parts[field.removesuffix("_id")] = self._artifact(run_id, artifact_id)
            records.append({"role": role, "source_step_id": step["step_id"],
                            "source_hash": step["source_hash"], "status": step["status"],
                            "attempt_id": step["attempt_id"], "task_id": step["task_id"],
                            "request_id": step["request_id"], "call_id": step["call_id"],
                            "target": target, **parts})
        return self._append(run_id, MANIFEST_EVENT, "manifest_id", {
            "manifest_id": identity, "schema_version": 1, "run_id": run_id,
            "target": target, "branch_id": state.branch_id, "plan_revision": state.plan_revision,
            "session_branch_id": self.service.journal.active_branch_id(run_id),
            "exchanges": records, "request_hash": request_hash,
            "association_trust": "model_declared", "cas_integrity": "verified_at_recording",
            "runtime_verified": False,
        })

    def sources(self, run_id: str, target: str) -> dict:
        state = self._state(run_id)
        if target not in state.goal.targets:
            raise ValueError("report_target_outside_run")
        manifests = [record for record in self._records(run_id, MANIFEST_EVENT) if record["target"] == target]
        steps, artifacts, problems = {}, {}, []
        for manifest in manifests:
            for exchange in manifest["exchanges"]:
                step_id = exchange["source_step_id"]
                try:
                    steps[step_id] = self.steps.read(run_id, step_id)["source_hash"]
                except (KeyError, ValueError, RuntimeError):
                    problems.append(f"source_unavailable:{step_id}")
                for field, part in exchange.items():
                    if field.endswith("_artifact") and isinstance(part, dict):
                        artifact_id = part["artifact_ref"]
                        try:
                            artifacts[artifact_id] = self._artifact(run_id, artifact_id)["content_hash"]
                        except (KeyError, ValueError, RuntimeError, OSError):
                            problems.append(f"artifact_integrity_unavailable:{artifact_id}")
        snapshot = {"target": target, "branch_id": state.branch_id, "plan_revision": state.plan_revision,
                    "session_branch_id": self.service.journal.active_branch_id(run_id),
                    "manifests": {record["manifest_id"]: record["record_hash"] for record in manifests},
                    "steps": steps, "artifacts": artifacts, "runtime_verified_evidence": self._verified(run_id, target),
                    "problems": sorted(set(problems))}
        return {"source_hash": contract_hash(snapshot), "snapshot": snapshot}

    def report(self, run_id: str, arguments: dict) -> dict:
        mode = arguments.get("mode", "list")
        if mode == "sources":
            return self.sources(run_id, arguments.get("target", ""))
        if mode == "list":
            records = self._records(run_id, REPORT_EVENT)
            if arguments.get("report_id"):
                records = [item for item in records if item["report_id"] == arguments["report_id"]]
            return page(records, offset=arguments.get("offset", 0), limit=arguments.get("limit", 20))
        revision_id = arguments.get("revision_id", "")
        if not revision_id:
            raise ValueError("report_revision_id_required")
        existing = self._existing(run_id, REPORT_EVENT, "revision_id", revision_id)
        if mode == "read":
            if existing is None:
                raise KeyError(f"report_revision_not_found:{revision_id}")
            return self._report_view(run_id, existing)
        if mode != "record":
            raise ValueError("report_mode_invalid")
        request_hash = contract_hash(redact_sensitive(arguments))
        if existing is not None:
            if existing["request_hash"] != request_hash:
                raise ValueError("report_revision_identity_conflict")
            return self._report_view(run_id, existing)
        if not arguments.get("report_id"):
            raise ValueError("report_id_required")
        sources = self.sources(run_id, arguments.get("target", ""))
        if not sources["snapshot"]["manifests"] or sources["snapshot"]["problems"]:
            raise ValueError("report_sources_missing_or_invalid")
        if arguments.get("expected_source_hash") != sources["source_hash"]:
            raise ValueError("report_source_stale")
        content = self._artifact(run_id, arguments.get("content_artifact_id", ""))
        record = self._append(run_id, REPORT_EVENT, "revision_id", {
            "revision_id": revision_id, "report_id": arguments["report_id"], "schema_version": 1,
            "run_id": run_id, "target": arguments["target"], "content": content,
            "source_hash": sources["source_hash"], "source_snapshot": sources["snapshot"],
            "request_hash": request_hash, "content_trust": "model_declared", "runtime_verified": False,
        })
        return self._report_view(run_id, record)

    def _report_view(self, run_id: str, record: dict) -> dict:
        current = self.sources(run_id, record["target"])
        problems = list(current["snapshot"]["problems"])
        try:
            self._artifact(run_id, record["content"]["artifact_ref"])
        except (KeyError, ValueError, RuntimeError, OSError):
            problems.append("report_content_integrity_unavailable")
        return {"record": record, "current_source_hash": current["source_hash"],
                "stale": record["source_hash"] != current["source_hash"] or bool(problems),
                "problems": problems}
