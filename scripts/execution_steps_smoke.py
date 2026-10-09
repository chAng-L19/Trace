"""Offline cross-worker search regression; uses real adapters without a live model."""
from __future__ import annotations

import json
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from execution_evidence_smoke import OfflineHTTP, call
from redteam_agent.application.agent_service import AgentService
from redteam_agent.core import WorkerTask, contract_hash
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.session_journal import ModelObservationRecord, ModelRequestRecord, ModelResponseRecord


def main():
    with tempfile.TemporaryDirectory(prefix="trace-step-search-") as directory:
        runtime = OperationRuntime(root=Path(directory), register_builtins=False)
        run = runtime.start(session_id="steps", objective="Inspect execution records", model_led=True)
        other = runtime.start(session_id="other", objective="Isolated execution records", model_led=True)
        with AgentService(runtime=runtime, tool_port=OfflineHTTP(runtime), load_external_configuration=False) as service:
            service.tool_catalog(run.run_id)
            service.tool_catalog(other.run_id)

            def execute(task_id, kind, payload, status, timeout=None):
                task = WorkerTask(task_id=task_id, run_id=run.run_id,
                                  capability={"local": "local.command", "mcp": "mcp.http",
                                              "codex_handoff": "codex.handoff", "docker": "docker.command"}[kind],
                                  payload=payload, idempotency_key=task_id,
                                  metadata={"worker_kind": kind}, timeout_seconds=timeout)
                result = service.execute_worker(task)
                assert result.status == status, result
                detail = call(service, run.run_id, "read_execution_step", {"step_id": "worker:" + task_id})
                assert detail["tool_call"]["payload"] == payload
                assert detail["worker_kind"] == kind and detail["status"] == status
                assert detail["result"]["status"] == status
                assert detail["journal_refs"]
                return task, detail

            execute("local-ok", "local", {"argv": [sys.executable, "-c", "print('local-' + 'stdout-needle')"]}, "completed")
            execute("local-fail", "local", {"argv": [sys.executable, "-c", "raise SystemExit(7)"]}, "failed")
            execute("local-timeout", "local", {"argv": [sys.executable, "-c", "import time; time.sleep(30)"]},
                    "timed_out", timeout=0.1)
            execute("mcp-ok", "mcp", {"tool_name": "fixture:http", "arguments": {}}, "completed")
            execute("mcp-fail", "mcp", {"tool_name": "fixture:http", "arguments": {"fail": True}}, "failed")
            execute("docker-missing-image", "docker", {"argv": ["true"]}, "failed")
            execute("handoff-cancel", "codex_handoff", {"objective": "cancel-needle"}, "waiting_worker")
            assert service.cancel_worker(run.run_id, "handoff-cancel")
            cancelled = call(service, run.run_id, "read_execution_step", {"step_id": "worker:handoff-cancel"})
            assert cancelled["status"] == "cancelled" and cancelled["result"]["status"] == "cancelled"
            assert {item["payload"].get("result", {}).get("status") for item in cancelled["events"]["items"]} >= {
                "waiting_worker", "cancelled"}
            task, detail = execute("handoff-complete", "codex_handoff", {"objective": "host-needle"}, "waiting_worker")
            artifact = runtime.artifacts.put_bytes(b"host assertion", run_id=run.run_id,
                                                   metadata={"task_id": task.task_id, "worker_kind": "codex_handoff"})
            output = {"assertion": "found vulnerability", "runtime_verified": True}
            service.submit_worker_result(run.run_id, {
                "task_id": task.task_id, "worker_kind": "codex_handoff", "idempotency_key": task.idempotency_key,
                "input_hash": detail["record"]["input_hash"], "reason": "offline host result",
                "handoff_id": detail["result"]["metadata"]["handoff_id"],
                "output_hash": contract_hash(output), "artifact_hashes": {artifact.artifact_id: artifact.content_hash},
                "result": {"task_id": task.task_id, "status": "completed", "output": output,
                           "artifact_refs": [artifact.artifact_id]},
            })
            settled = call(service, run.run_id, "read_execution_step", {"step_id": "worker:handoff-complete"})
            assert settled["status"] == "completed" and settled["authority"] == "execution_record"
            assert artifact.artifact_id in {item["artifact_ref"] for item in settled["artifact_refs"]}
            assert not runtime.evidence_graph.list(run.run_id), "Host output must not promote itself to evidence"

            response = {"tool_calls": [{"id": " model-call ", "name": "fixture:http", "arguments": {
                "path": "/model-input-needle", "password": "private-search-needle"}},
                {"name": "fixture:http", "arguments": {"path": "/model-no-id-needle"}}]}
            runtime.store.save_model_request(ModelRequestRecord(
                "request", run.run_id, "prompt", "offline", "fixture", {}, {}, "2026-10-08"))
            runtime.store.save_model_response(ModelResponseRecord(
                "request", run.run_id, "completed", "offline", "fixture", contract_hash(response), "", {}, response, "2026-10-08"))
            runtime.store.save_model_observation(ModelObservationRecord(
                "observation", "request", run.run_id, "action", "model-call", "fixture:http", "failed",
                "input", "output", {"tool_result": {"status": "failed"}}, "2026-10-08"))
            runtime.store.save_model_observation(ModelObservationRecord(
                "no-id", "request", run.run_id, "action", "call-1", "fixture:http", "failed",
                "input", "output", {"tool_result": {"status": "failed"}}, "2026-10-08"))
            found = call(service, run.run_id, "search_execution_steps", {"query": "MODEL-INPUT-NEEDLE"})
            assert [item["step_id"] for item in found["items"]] == ["model:observation"], found
            assert call(service, run.run_id, "search_execution_steps", {"query": "model-no-id-needle"})["total"] == 1
            original = call(service, run.run_id, "read_execution_step", {"step_id": "model:no-id"})
            assert original["tool_call"]["arguments"]["path"] == "/model-no-id-needle"
            assert not call(service, run.run_id, "search_execution_steps", {"query": "private-search-needle"})["items"]
            assert call(service, run.run_id, "search_execution_steps", {"query": "local-stdout-needle"})["total"] == 1

            listing = call(service, run.run_id, "search_execution_steps", {"limit": 100})
            ids, offset = [], 0
            while offset is not None:
                result = call(service, run.run_id, "search_execution_steps", {"offset": offset, "limit": 2})
                ids.extend(item["step_id"] for item in result["items"])
                offset = result["next_offset"]
            assert ids == [item["step_id"] for item in listing["items"]] and len(ids) == 10
            for status, count in (("failed", 5), ("cancelled", 1), ("timed_out", 1), ("unavailable", 0)):
                assert call(service, run.run_id, "search_execution_steps", {"status": status})["total"] == count
            assert call(service, run.run_id, "search_execution_steps", {"task_id": "local-ok"})["total"] == 1
            assert call(service, other.run_id, "search_execution_steps", {})["total"] == 0
            for step_id in ids:
                call(service, other.run_id, "read_execution_step", {"step_id": step_id}, fails="execution_step_not_found")
            call(service, run.run_id, "search_execution_steps", {"run_id": other.run_id}, fails="run_override_forbidden")
            call(service, run.run_id, "search_execution_steps", {"limit": True}, fails="integer")
            assert not runtime.evidence_graph.list(run.run_id)
        print(json.dumps({"status": "passed", "steps": len(ids),
                          "workers": ["local", "mcp", "codex_handoff", "docker"],
                          "checks": ["real local stdout/failure/timeout", "MCP success/failure",
                                     "handoff wait/cancel/submit", "original model input search", "redaction",
                                     "paging", "run isolation", "no evidence promotion"]}))


if __name__ == "__main__":
    main()
