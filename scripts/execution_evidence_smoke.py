"""Offline regression over real AgentService, Runtime, journal, worker store and CAS.

Run: python scripts/execution_evidence_smoke.py (no network or model required).
"""
from __future__ import annotations

import json
import sys
import tempfile
from copy import deepcopy
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.evidence_records import EvidenceRecords, REPORT_EVENT
from redteam_agent.core import ToolCall, ToolDefinition, ToolResult, WorkerResult, WorkerTask, contract_hash
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.model_state import TaskAttempt
from redteam_agent.runtime.session_journal import ModelObservationRecord, ModelRequestRecord, ModelResponseRecord


class OfflineHTTP:
    def __init__(self, runtime):
        self.runtime = runtime
        self.exchanges = {}

    def discover(self):
        return (ToolDefinition(qualified_name="fixture:http", name="http", server="fixture",
                               input_schema={"type": "object"}, capabilities=("http",)),)

    def invoke(self, call):
        refs = {}
        for field, data in (("request", b"GET /evidence HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"),
                            ("response", b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"),
                            ("response_body", b"OK")):
            refs[field] = self.runtime.artifacts.put_bytes(
                data, run_id=call.run_id, artifact_type="http_" + field,
                metadata={"task_id": call.call_id, "worker_kind": "mcp"},
            ).artifact_id
        self.exchanges[call.call_id] = refs
        return ToolResult(call_id=call.call_id, tool_name=call.tool_name,
                          status="failed" if call.arguments.get("fail") else "success",
                          output={"status_code": 200, "artifact_refs": list(refs.values())},
                          error="offline_failure" if call.arguments.get("fail") else "")

    def reconcile(self, call):
        return None

    def cancel(self, call_id):
        return False


def call(service, run_id, name, arguments, *, fails=""):
    result = service.tools.invoke(ToolCall(call_id="smoke-" + name, run_id=run_id,
                                          tool_name="agent:" + name, arguments=arguments))
    if fails:
        assert result.status == "failed" and fails in result.error, result.to_dict()
        return None
    assert result.status == "success", result.to_dict()
    output = result.output
    if isinstance(output, dict) and "artifact_ref" in output and "preview" in output:
        return service.runtime.artifacts.read_json(output["artifact_ref"], run_id=run_id)
    return output


def main():
    with tempfile.TemporaryDirectory(prefix="trace-execution-evidence-") as directory:
        runtime = OperationRuntime(root=Path(directory), register_builtins=False)
        first = runtime.start(session_id="first", objective="Inspect HTTP evidence", targets=("fixture.invalid",), model_led=True)
        other = runtime.start(session_id="other", objective="Inspect HTTP evidence", targets=("fixture.invalid",), model_led=True)
        fixture = OfflineHTTP(runtime)
        with AgentService(runtime=runtime, tool_port=fixture, load_external_configuration=False) as service:
            for run in (first, other):
                service.tool_catalog(run.run_id)
            for task_id, fail in (("proof-worker", False), ("failed-worker", True)):
                task = WorkerTask(task_id=task_id, run_id=first.run_id, capability="mcp.http",
                                  payload={"tool_name": "fixture:http", "arguments": {"fail": fail}},
                                  idempotency_key=task_id, metadata={"worker_kind": "mcp"})
                result = service.execute_worker(task)
                assert result.status == ("failed" if fail else "completed")
            cancelled = WorkerTask(task_id="cancelled-worker", run_id=first.run_id, capability="mcp.http",
                                   payload={}, idempotency_key="cancelled", metadata={"worker_kind": "mcp"})
            service.worker_records.prepare(cancelled, worker_kind="mcp", owner="smoke")
            service.worker_records.transition(cancelled.task_id, expected_statuses=("prepared",),
                                               status="cancelled", result=WorkerResult(cancelled.task_id, "cancelled"))
            service._record_worker_observation(cancelled, WorkerResult(cancelled.task_id, "cancelled"))
            attempt = TaskAttempt.create(run_id=first.run_id, branch_id=first.branch_id, plan_revision=1,
                                         action_id="fixture-action", tool="fixture:http", tool_version="1",
                                         input_hash="original-input-hash", idempotency_key="attempt-fixture")
            runtime.store.create_task_attempt(attempt)
            attempt_step = call(service, first.run_id, "read_execution_step", {"step_id": "attempt:" + attempt.attempt_id})
            assert attempt_step["input_availability"] == "hash_only" and attempt_step["tool_call"] is None

            # Runtime observation fixture retains the exact original model tool call and failure.
            response = {"tool_calls": [{"id": "model-call", "name": "fixture:http", "arguments": {"path": "/model"}}]}
            runtime.store.save_model_request(ModelRequestRecord(
                "request", first.run_id, "prompt", "offline", "fixture", {}, {}, "2026-10-08"))
            runtime.store.save_model_response(ModelResponseRecord(
                "request", first.run_id, "completed", "offline", "fixture", contract_hash(response), "", {}, response, "2026-10-08"))
            runtime.store.save_model_observation(ModelObservationRecord(
                "observation", "request", first.run_id, "action", "model-call", "fixture:http", "failed",
                "input", "output", {"tool_result": {"status": "failed", "error": "fixture_rejected"}}, "2026-10-08"))
            model_step = call(service, first.run_id, "read_execution_step", {"step_id": "model:observation"})
            assert model_step["tool_call"]["arguments"]["path"] == "/model"
            assert model_step["result"]["tool_result"]["error"] == "fixture_rejected"
            assert model_step["journal_refs"]

            listing = call(service, first.run_id, "search_execution_steps", {"limit": 1})
            assert listing["total"] == 5 and listing["next_offset"] == 1
            assert call(service, first.run_id, "search_execution_steps", {"kind": "worker", "status": "failed"})["total"] == 1
            assert call(service, first.run_id, "search_execution_steps", {"status": "cancelled"})["total"] == 1
            assert call(service, other.run_id, "search_execution_steps", {})["total"] == 0
            call(service, other.run_id, "read_execution_step", {"step_id": "worker:proof-worker"}, fails="execution_step_not_found")
            call(service, first.run_id, "search_execution_steps", {"run_id": other.run_id}, fails="run_override_forbidden")
            call(service, first.run_id, "search_execution_steps", {"limit": 0}, fails="minimum")

            with runtime.store.transaction(immediate=True) as connection:
                for number in range(1005):
                    runtime.store._insert_event(connection, first.run_id, "fixture_progress", {"number": number})
                runtime.store._insert_event(connection, first.run_id, "worker_cancel_requested", {"task_id": "proof-worker"})
            proof = call(service, first.run_id, "read_execution_step", {"step_id": "worker:proof-worker", "limit": 1})
            assert proof["tool_call"]["payload"]["tool_name"] == "fixture:http"
            events = call(service, first.run_id, "read_execution_step", {"step_id": "worker:proof-worker", "limit": 100})["events"]
            assert any(item["event_type"] == "worker_cancel_requested" for item in events["items"])

            refs = fixture.exchanges["proof-worker"]
            exchange = {"role": "proof", "source_step_id": "worker:proof-worker",
                        "request_artifact_id": refs["request"], "response_artifact_id": refs["response"],
                        "response_body_artifact_id": refs["response_body"]}
            arguments = {"mode": "record", "manifest_id": "manifest-1", "target": "fixture.invalid",
                         "exchanges": [{**exchange, "role": role} for role in ("baseline", "proof", "control")]}
            manifest = call(service, first.run_id, "evidence_manifest", arguments)
            assert not manifest["runtime_verified"] and manifest["association_trust"] == "model_declared"
            assert manifest["exchanges"][0]["response_body_artifact"]["content_hash"] == contract_hash_bytes(b"OK")
            assert call(service, first.run_id, "evidence_manifest", arguments) == manifest
            bad = deepcopy(arguments)
            bad["exchanges"][0]["role"] = "control"
            call(service, first.run_id, "evidence_manifest", bad, fails="manifest_identity_conflict")
            for artifact_id in ("artifact-forged", runtime.artifacts.put_bytes(b"unrelated", run_id=first.run_id).artifact_id,
                                runtime.artifacts.put_bytes(b"foreign", run_id=other.run_id).artifact_id):
                bad = {**arguments, "manifest_id": "bad-reference", "exchanges": [{**exchange, "response_artifact_id": artifact_id}]}
                call(service, first.run_id, "evidence_manifest", bad, fails="artifact_not_in_step")
            call(service, first.run_id, "evidence_manifest", {**arguments, "manifest_id": "bad-target", "target": "elsewhere.invalid"}, fails="target_outside_run")
            call(service, other.run_id, "evidence_manifest", {"mode": "read", "manifest_id": "manifest-1"}, fails="manifest_not_found")
            call(service, first.run_id, "evidence_manifest", {**arguments, "runtime_verified": True}, fails="additional")

            sources = call(service, first.run_id, "report_revision", {"mode": "sources", "target": "fixture.invalid"})
            content = runtime.artifacts.put_bytes(b"# Offline report\nEvidence requires semantic review.\n", run_id=first.run_id)
            report_args = {"mode": "record", "report_id": "report", "revision_id": "r1", "target": "fixture.invalid",
                           "content_artifact_id": content.artifact_id, "expected_source_hash": sources["source_hash"]}
            report = call(service, first.run_id, "report_revision", report_args)
            assert not report["stale"] and not report["record"]["runtime_verified"]
            assert call(service, first.run_id, "report_revision", report_args)["record"] == report["record"]
            call(service, other.run_id, "report_revision", {"mode": "read", "revision_id": "r1"}, fails="revision_not_found")
            call(service, first.run_id, "report_revision", {**report_args, "report_id": "forged"}, fails="identity_conflict")
            call(service, first.run_id, "evidence_manifest", {**arguments, "manifest_id": "manifest-2"})
            stale = call(service, first.run_id, "report_revision", {"mode": "read", "revision_id": "r1"})
            assert stale["stale"] and stale["record"] == report["record"]
            call(service, first.run_id, "report_revision", {**report_args, "revision_id": "r2"}, fails="source_stale")
            fresh = call(service, first.run_id, "report_revision", {"mode": "sources", "target": "fixture.invalid"})
            assert not call(service, first.run_id, "report_revision", {**report_args, "revision_id": "r2", "expected_source_hash": fresh["source_hash"]})["stale"]
            leaf = service.journal.leaf_id(first.run_id)
            service.fork_session(first.run_id, leaf, "evidence-branch")
            service.checkout_session(first.run_id, "evidence-branch")
            assert call(service, first.run_id, "report_revision", {"mode": "read", "revision_id": "r2"})["stale"]
            service.checkout_session(first.run_id, "main")
            assert not call(service, first.run_id, "report_revision", {"mode": "read", "revision_id": "r2"})["stale"]
            append = EvidenceRecords._append

            def source_race(records, run_id, event_type, identity, record):
                if event_type == REPORT_EVENT:
                    records.manifest(run_id, {**arguments, "manifest_id": "manifest-concurrent"})
                return append(records, run_id, event_type, identity, record)

            with patch.object(EvidenceRecords, "_append", source_race):
                raced = call(service, first.run_id, "report_revision", {
                    **report_args, "revision_id": "r-concurrent", "expected_source_hash": fresh["source_hash"]})
            assert raced["stale"], "A source change between check and append must never return fresh"
            assert not runtime.evidence_graph.list(first.run_id), "Packaging must never create trusted evidence"
            raw_ref = runtime.artifacts.get_ref(refs["response_body"], run_id=first.run_id)
            runtime.artifacts._path(raw_ref.content_hash).write_bytes(b"tampered")
            corrupt = call(service, first.run_id, "report_revision", {"mode": "read", "revision_id": "r2"})
            assert corrupt["stale"] and corrupt["problems"]
            call(service, first.run_id, "evidence_manifest", {**arguments, "manifest_id": "corrupt"}, fails="artifact_")

        restored = OperationRuntime(root=Path(directory), register_builtins=False)
        with AgentService(runtime=restored, tool_port=OfflineHTTP(restored), load_external_configuration=False) as service:
            service.tool_catalog(first.run_id)
            saved = call(service, first.run_id, "report_revision", {"mode": "read", "revision_id": "r1"})
            assert saved["stale"] and saved["record"] == report["record"]
        print(json.dumps({"status": "passed", "execution_steps": 5, "event_paging": ">1000",
                          "checks": ["worker/model results", "failure/cancel", "run isolation", "forged references",
                                     "immutable replay", "source/branch stale", "CAS tamper", "restart", "no evidence promotion"]}))


def contract_hash_bytes(raw):
    import hashlib
    return hashlib.sha256(raw).hexdigest()


if __name__ == "__main__":
    main()
