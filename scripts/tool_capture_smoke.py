"""Real local HTTP, worker recovery, private CAS and browser capture regression."""
from __future__ import annotations

import json
import sys
import tempfile
import threading
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.evidence_records import EvidenceRecords
from redteam_agent.application.execution_steps import ExecutionSteps
from redteam_agent.core import ModelRequest, ToolCall, WorkerTask
from redteam_agent.providers import FakeModelProvider, OpenAICompatibleProvider
from redteam_agent.providers.openai_protocol import request_payload
from redteam_agent.runtime.tool_capture import ToolCapture
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.open_source_tools import register_open_source_tools


RAW = b"\xff\xfe\x00A"
SECRET = "capture-fixture-sensitive-value"


class Handler(BaseHTTPRequestHandler):
    counts: dict[str, int] = {}
    release = threading.Event()

    def log_message(self, *args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.counts[self.path] = self.counts.get(self.path, 0) + 1
        if self.path != "/cas-failure":
            self.release.wait(5)
        try:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"written")
        except OSError:
            pass

    def do_GET(self):
        self.counts[self.path] = self.counts.get(self.path, 0) + 1
        body = b"A" * (4 * 1024 * 1024 + 1) if self.path == "/large" else RAW
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("X-Repeat", "one")
        self.send_header("X-Repeat", "two")
        self.send_header("Set-Cookie", "session=" + SECRET)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except OSError:
            pass


def invoke(service, run_id, name, arguments, call_id):
    result = service.tools.invoke(ToolCall(
        call_id=call_id, run_id=run_id, tool_name="builtin:" + name,
        arguments=arguments, timeout_seconds=20,
        metadata={"request_id": "capture-model-request"},
    ))
    assert result.status == "success", result.to_dict()
    return result.output


def raw_bytes(runtime, run_id, part):
    identity = part["raw_capture"]["private_artifact_ref"]
    return identity, runtime.artifacts.read(identity, run_id=run_id)


def check_privacy(service, run_id, output):
    assert SECRET not in json.dumps(output)
    for part in output["exchange_artifacts"].values():
        private_id = part["raw_capture"]["private_artifact_ref"]
        try:
            service.read_artifact(run_id, private_id)
        except KeyError:
            pass
        else:
            raise AssertionError("private_raw_capture_publicly_readable")
    for artifact in service.artifacts(run_id):
        assert not artifact.metadata.get("provider_private")
        assert SECRET.encode() not in service.read_artifact(run_id, artifact.artifact_id)


def check_chat_binding():
    provider = OpenAICompatibleProvider("http://127.0.0.1:1/v1", "fixture", environ={})
    call = {"id": "reused", "function": {"name": "fixture", "arguments": "{}"}}
    request = ModelRequest("now", "run", (
        {"role": "assistant", "source_request_id": "old", "content": None, "tool_calls": [call]},
        {"role": "tool", "tool_call_id": "reused", "content": "old-result"},
        {"role": "assistant", "source_request_id": "new", "content": None, "tool_calls": [call]},
    ), continuation={"chain": [{"assistant_request_id": "old", "assistant_call_ids": ["reused"],
                                "assistant": {"tool_calls": [{"id": "reused", "extra_content": {
                                    "google": {"thought_signature": "old-signature"}}}]}}]})
    payload, _ = request_payload(provider, request)
    assert payload["messages"][0]["tool_calls"][0]["extra_content"]["google"]["thought_signature"] == "old-signature"
    assert "extra_content" not in payload["messages"][2]["tool_calls"][0]


def check_native_unknown(root, base):
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    provider = FakeModelProvider([{"status": "completed", "text": "local fixture",
                                  "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
                                  "structured_output": {"commit_lifecycle_gate": True}, "tool_calls": [
        {"call_id": "success-before", "tool_name": "builtin:http-request", "arguments": {"url": base + "/before"}},
        {"call_id": "native-write", "tool_name": "builtin:http-request", "arguments": {
            "url": base + "/native-write", "method": "POST", "body": "change", "timeout": 0.05}},
        {"call_id": "never-dispatched", "tool_name": "builtin:http-request", "arguments": {"url": base + "/after"}},
    ]}])
    with AgentService(runtime=runtime, model_port=provider, load_external_configuration=False) as service:
        run_id = service.start({"session_id": "native", "objective": "Inspect local HTTP fixture",
                                "targets": [base]}).runs[0].run.run_id
        result = service.run(run_id)
        assert result.run.budget.pause_reason == "worker_result_unknown", result
        assert len(provider.requests) == 1 and Handler.counts["/native-write"] == 1
        assert result.run.budget.input_tokens_used == 3 and result.run.budget.output_tokens_used == 2
        observations = service.journal.model_observations(run_id)
        assert [item.call_id for item in observations] == ["success-before", "native-write"]
        failed = observations[1].observation["tool_result"]
        assert failed["status"] == "unknown" and failed["retryable"] and failed["error"]
        assert failed["output"]["exchange_artifacts"]["http_request"]["artifact_ref"]
        transcript = service.conversation.messages(run_id)
        assert any(item.source_type == "tool_result" and item.source_id.endswith(":native-write") for item in transcript)
        consumed = [event["payload"] for event in runtime.store.events(run_id)
                    if event["event_type"] == "model_turn_consumed"]
        assert any(item["reason"] == "worker_result_unknown" for item in consumed), consumed
        assert not any(event["event_type"] == "model_observation_submitted" for event in runtime.store.events(run_id))
        assert service.agent_loop._recover_pending_turn(service.status(run_id)) is None
        uncertain = next(item for item in service.worker_records.records(run_id) if item.status == "unknown")
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    restarted = FakeModelProvider([])
    with AgentService(runtime=runtime, model_port=restarted, load_external_configuration=False) as service:
        result = service.resume(run_id)
        assert result.run.budget.pause_reason == "worker_result_unknown"
        assert not restarted.requests and Handler.counts["/native-write"] == 1
        assert service.worker_records.get(uncertain.task.task_id).status == "unknown"


def check_native_success(root, base):
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    provider = FakeModelProvider([
        {"status": "completed", "text": "fixture response", "usage": {"total_tokens": 1},
         "structured_output": {"commit_lifecycle_gate": True}, "tool_calls": [{
             "call_id": "native-success", "tool_name": "builtin:http-request", "arguments": {"url": base + "/success"}}]},
        {"status": "completed", "text": "Following lifecycle action", "usage": {"total_tokens": 1}},
    ])
    with AgentService(runtime=runtime, model_port=provider, load_external_configuration=False) as service:
        run_id = service.start({"session_id": "native-success", "objective": "Inspect local fixture",
                                "targets": [base]}).runs[0].run.run_id
        service.run(run_id)
        assert len(provider.requests) == 2
        assert any(item.status == "completed" for item in service.worker_records.records(run_id))
        assert any(event["event_type"] == "model_turn_consumed" and
                   event["payload"]["reason"] == "lifecycle_observation_submitted"
                   for event in runtime.store.events(run_id))


def check_native_cas_failure(root, base):
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    run_id = runtime.start(session_id="cas-failure", objective="Inspect local HTTP fixture",
                           targets=(base,), model_led=True).run_id
    call = ToolCall("cas-failure", run_id, "builtin:http-request",
                    {"url": base + "/cas-failure", "method": "POST", "body": "change"},
                    idempotency_key="cas-failure")
    with AgentService(runtime=runtime, load_external_configuration=False) as service:
        with patch.object(runtime.artifacts, "put_file", side_effect=OSError("fixture-cas-failure")):
            result = service.tools.invoke(call)
        assert result.status == "unknown" and "fixture-cas-failure" in result.error, result
        assert Handler.counts["/cas-failure"] == 1
        assert service.tools.invoke(call).status == "unknown"
        assert Handler.counts["/cas-failure"] == 1
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    with AgentService(runtime=runtime, load_external_configuration=False) as service:
        assert service.tools.reconcile(call).status == "unknown"
        assert Handler.counts["/cas-failure"] == 1


def check_queued_deadline(root, base):
    runtime = OperationRuntime(root=root, register_builtins=False)
    register_open_source_tools(runtime.broker)
    provider = FakeModelProvider([{"status": "completed", "text": "fixture", "usage": {"total_tokens": 1},
                                  "tool_calls": [{"call_id": path, "tool_name": "builtin:http-request",
                                                  "arguments": {"url": base + path}}
                                                 for path in ("/deadline-first", "/deadline-never")]}])
    with AgentService(runtime=runtime, model_port=provider, load_external_configuration=False) as service:
        run_id = service.start({"session_id": "deadline", "objective": "Inspect local fixture",
                                "targets": [base]}).runs[0].run.run_id
        original = service.tools.delegate.invoke

        def invoke_and_expire(call):
            result = original(call)
            if call.metadata.get("worker_kind") != "mcp":
                state = runtime.store.load_operation(run_id)
                state.budget.deadline = (datetime.now(timezone.utc) - timedelta(seconds=1)).isoformat()
                runtime.store.save_operation(state, event_type="fixture_deadline_expired", event={},
                                             expected_version=state.state_version)
            return result

        with patch.object(service.tools.delegate, "invoke", side_effect=invoke_and_expire):
            result = service.run(run_id)
        assert result.run.budget.pause_reason == "time_limit_exhausted", result
        assert Handler.counts["/deadline-first"] == 1 and "/deadline-never" not in Handler.counts


def main():
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{server.server_port}"
    checks = []
    try:
        check_chat_binding()
        checks.append("chat_reused_call_id_source_binding")
        with tempfile.TemporaryDirectory(prefix="trace-tool-capture-") as temporary:
            root = Path(temporary)
            runtime = OperationRuntime(root=root, register_builtins=False)
            register_open_source_tools(runtime.broker)
            state = runtime.start(session_id="capture", objective="Inspect a local HTTP fixture",
                                  targets=(base,), model_led=True,
                                  starting_context={"headers": {"Authorization": "Bearer " + SECRET}})
            run_id = state.run_id
            runtime._credential_vault.project({"password": SECRET})
            boundary_secret = "boundary-sensitive-" + "s" * 512
            runtime._credential_vault.project({"password": boundary_secret})
            capture = ToolCapture(runtime.artifacts, run_id, {}, runtime._credential_vault.project)
            projected = capture.project_output({"body": "A" * (16 * 1024 - 4) + boundary_secret})
            assert "boun" not in projected["body"]
            checks.append("secret_projected_before_truncation")
            with AgentService(runtime=runtime, load_external_configuration=False) as service:
                descriptor = next(item for item in runtime.broker.descriptors()
                                  if item.qualified_name == "builtin:http-request")
                assert descriptor.side_effecting and not descriptor.supports_reconcile
                task = WorkerTask(task_id="capture-http", run_id=run_id, capability="mcp.http-request",
                                  payload={"tool_name": "builtin:http-request", "arguments": runtime._credential_vault.project({
                                      "url": base + "/bytes", "headers": {"Authorization": "Bearer " + SECRET},
                                  })}, idempotency_key="capture-http", metadata={"worker_kind": "mcp"})
                result = service.execute_worker(task)
                assert result.status == "completed", result.to_dict()
                recorded = runtime.artifacts.read_json(result.artifact_refs[0], run_id=run_id)
                output = recorded["output"]
                parts = output["exchange_artifacts"]
                assert raw_bytes(runtime, run_id, parts["http_response_body"])[1] == RAW
                response = json.loads(raw_bytes(runtime, run_id, parts["http_response"])[1])
                repeated = [value for name, value in response["headers"] if name == "X-Repeat"]
                assert repeated == ["one", "two"], response
                request = json.loads(raw_bytes(runtime, run_id, parts["http_request"])[1])
                assert any(SECRET in value for name, value in request["headers"])
                check_privacy(service, run_id, output)
                step = ExecutionSteps(service).read(run_id, "worker:" + task.task_id)
                assert set(output["artifact_refs"]).issubset({item["artifact_ref"] for item in step["artifact_refs"]})
                manifest = EvidenceRecords(service).manifest(run_id, {
                    "mode": "record", "manifest_id": "captured", "target": base,
                    "exchanges": [{"role": "proof", "source_step_id": "worker:" + task.task_id,
                                   "request_artifact_id": parts["http_request"]["artifact_ref"],
                                   "response_artifact_id": parts["http_response"]["artifact_ref"],
                                   "response_body_artifact_id": parts["http_response_body"]["artifact_ref"]}],
                })
                assert manifest["cas_integrity"] == "verified_at_recording"
                checks.extend(("binary_bytes_exact", "duplicate_headers", "secret_boundary", "worker_manifest_link"))

                large = invoke(service, run_id, "http-request", {"url": base + "/large"}, "capture-large")
                large_part = large["exchange_artifacts"]["http_response_body"]
                assert large_part["capture_truncated"]
                assert len(raw_bytes(runtime, run_id, large_part)[1]) == 4 * 1024 * 1024
                assert large["body_projection_truncated"] and len(large["body"]) == 16 * 1024
                checks.append("bounded_capture")

                encoded = invoke(service, run_id, "http-request", {
                    "url": base + "/bytes?api%5Fkey=encoded-fixture-value"
                }, "capture-encoded-url")
                assert "encoded-fixture-value" not in json.dumps(encoded)
                for reference in encoded["artifact_refs"]:
                    assert b"encoded-fixture-value" not in service.read_artifact(run_id, reference)
                checks.append("encoded_url_secret_projection")

                too_big = service.tools.invoke(ToolCall(
                    call_id="request-too-big", run_id=run_id, tool_name="builtin:http-request",
                    arguments={"url": base + "/request-too-big", "method": "POST", "body": "A" * (4 * 1024 * 1024 + 1)},
                ))
                assert too_big.status == "failed" and not too_big.retryable, too_big.to_dict()
                assert "/request-too-big" not in Handler.counts
                checks.append("oversized_request_stops_before_dispatch")

                writing = WorkerTask(task_id="capture-writing", run_id=run_id, capability="mcp.http-request",
                                     payload={"tool_name": "builtin:http-request", "arguments": {
                                         "url": base + "/worker-write", "method": "POST", "body": "change", "timeout": 0.05,
                                     }}, idempotency_key="capture-writing", metadata={"worker_kind": "mcp"})
                unknown = service.execute_worker(writing)
                assert unknown.status == "unknown", unknown.to_dict()
                assert service.execute_worker(writing) == unknown
                assert Handler.counts["/worker-write"] == 1
                checks.append("worker_write_timeout_no_replay")

                invoke(service, run_id, "browser-create", {
                    "url": "data:text/html,<html><body style='background:white'>capture</body></html>"
                }, "capture-browser-create")
                first = invoke(service, run_id, "browser-screenshot", {}, "capture-screen-one")
                first_part = first["screenshot_artifact"]
                first_id, first_pixels = raw_bytes(runtime, run_id, first_part)
                assert first_pixels.startswith(b"\x89PNG\r\n\x1a\n")
                invoke(service, run_id, "browser-evaluate", {
                    "expression": "document.body.style.background='rgb(240,0,0)'"
                }, "capture-browser-change")
                second = invoke(service, run_id, "browser-screenshot", {}, "capture-screen-two")
                second_id, second_pixels = raw_bytes(runtime, run_id, second["screenshot_artifact"])
                assert "screenshot_path" not in first and "screenshot_path" not in second
                screenshot_path = service.workspaces.ensure(run_id).path / "browser-screenshot.png"
                assert not screenshot_path.exists()
                assert first_pixels != second_pixels and first_id != second_id
                assert runtime.artifacts.read(first_id, run_id=run_id) == first_pixels
                check_privacy(service, run_id, second)
                checks.append("screenshot_overwrite_preserves_cas")

            # Recreate both Runtime and worker manager against the durable state.
            runtime = OperationRuntime(root=root, register_builtins=False)
            register_open_source_tools(runtime.broker)
            with AgentService(runtime=runtime, load_external_configuration=False) as service:
                assert service.execute_worker(writing) == unknown
                assert Handler.counts["/worker-write"] == 1
                assert runtime.artifacts.read(first_id, run_id=run_id) == first_pixels
                checks.append("restart_worker_no_replay")

                runtime_state = runtime.start(session_id="runtime-writing", objective="Inspect local HTTP",
                                              targets=(base,))
                workflow = runtime._workflow_for(runtime_state)
                action = replace(workflow.actions[0], parameters={"url": base + "/runtime-write",
                                 "method": "POST", "body": "change", "timeout": 0.05})
                workflow = replace(workflow, actions=(action,))
                descriptor = next(item for item in runtime.broker.descriptors()
                                  if item.qualified_name == "builtin:http-request")
                outcome = runtime.executor.execute(runtime_state, workflow, action, descriptor, timeout=1)
                assert outcome.event_type == "action_result_uncertain", outcome
                attempt = runtime.store.task_attempts(runtime_state.run_id)[0]
                assert attempt.status == "uncertain"
                current = runtime.store.load_operation(runtime_state.run_id)
                recovered = runtime.executor.reconcile_attempt(current, workflow, attempt, timeout=1)
                assert recovered.event_type == "action_reconcile_host_required", recovered
                assert Handler.counts["/runtime-write"] == 1
                checks.append("runtime_uncertain_no_replay")
            check_native_unknown(root / "native", base)
            checks.extend(("native_unknown_mixed_batch_stops", "native_unknown_usage_and_failure_fidelity",
                           "native_unknown_consumed_and_restart_no_replay"))
            check_native_success(root / "native-success", base)
            checks.append("native_success_preserves_lifecycle")
            check_native_cas_failure(root / "native-cas-failure", base)
            checks.append("native_success_cas_failure_unknown_no_replay")
            check_queued_deadline(root / "queued-deadline", base)
            checks.append("queued_tool_rechecks_runtime_deadline")
        print(json.dumps({"checks": checks, "passed": len(checks)}, sort_keys=True))
    finally:
        Handler.release.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


if __name__ == "__main__":
    main()
