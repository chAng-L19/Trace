"""Offline Harness projection, durable pagination and real HTTP SSE regressions."""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError
from urllib.request import ProxyHandler, Request, build_opener

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.adapters.web_server import TraceHTTPServer
from redteam_agent.application.trajectory import project_event, text_summary, tool_request_summary, tool_result_summary
from redteam_agent.core import Event, ToolDefinition, ToolResult
from redteam_agent.providers import FakeModelProvider, ScriptedStream
from redteam_agent.runtime.security import secret_reference


class TrajectoryChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="trace-trajectory-")
        cls.environment = patch.dict(os.environ, {
            "TRACE_ADMIN_USERNAME": "trajectory", "TRACE_ADMIN_PASSWORD": "trajectory-test-pass",
        })
        cls.environment.start()
        cls.service = AgentService(root=Path(cls.temporary.name), load_external_configuration=False)
        cls.api = WebApi(cls.service)
        cls.token = cls.api.control.login("trajectory", "trajectory-test-pass")
        cls.headers = {"Authorization": "Bearer " + cls.token}
        cls.server = TraceHTTPServer(("127.0.0.1", 0), cls.api)
        cls.worker = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.worker.start()
        cls.base = f"http://127.0.0.1:{cls.server.server_port}"
        cls.opener = build_opener(ProxyHandler({}))

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.worker.join(5)
        cls.server.server_close()
        cls.service.close()
        cls.environment.stop()
        cls.temporary.cleanup()

    def fixture(self):
        run = self.service.runtime.start(session_id=self.id(), objective="Inspect trajectory fixture", model_led=True)
        events = self.service.events(run.run_id, limit=1000)
        return run.run_id, events[-1].sequence

    def append(self, run_id, event_type, payload=None):
        self.service.runtime.store.append_event(run_id, event_type, payload or {})
        return self.service.events(run_id, limit=1000)[-1].sequence

    def page(self, run_id, after, limit=200, channel="trajectory"):
        response = self.api.dispatch("GET", f"/api/runs/{run_id}/events?channel={channel}&after_sequence={after}&limit={limit}",
                                     headers=self.headers)
        self.assertEqual(response.status, 200, response.payload())
        return response.payload()

    def sse(self, run_id, after=0, last_id=None, channel="trajectory", wait=0, authenticated=True):
        headers = {**(self.headers if authenticated else {}), "Accept": "text/event-stream"}
        if last_id is not None:
            headers["Last-Event-ID"] = str(last_id)
        request = Request(f"{self.base}/api/runs/{run_id}/events?channel={channel}&after_sequence={after}&wait_seconds={wait}",
                          headers=headers)
        try:
            response = self.opener.open(request, timeout=5)
        except HTTPError as error:
            response = error
        with response:
            body = response.read().decode("utf-8")
            frames = []
            for block in body.split("\n\n"):
                fields = dict(line.split(": ", 1) for line in block.splitlines() if ": " in line and not line.startswith(":"))
                if "id" in fields:
                    frames.append({"id": int(fields["id"]), "event": fields.get("event"),
                                   "data": json.loads(fields["data"]) if "data" in fields else None})
            return response.status, frames, body

    def test_json_filtered_page_and_empty_tail(self):
        run, start = self.fixture()
        hidden = self.append(run, "internal_bookkeeping")
        page = self.page(run, start, limit=1)
        self.assertEqual(page["events"], [])
        self.assertEqual(page["next_sequence"], hidden)
        self.assertEqual(self.page(run, hidden)["next_sequence"], hidden)

    def test_json_mixed_pages_and_global_sequence_gaps(self):
        run, cursor = self.fixture()
        other, _ = self.fixture()
        expected = []
        for index in range(7):
            self.append(other, "unrelated")
            sequence = self.append(run, "harness_tool_started" if index % 2 else "internal", {"tool_name": "agent:artifacts"})
            if index % 2:
                expected.append(sequence)
        seen = []
        for _ in range(10):
            page = self.page(run, cursor, limit=2)
            seen.extend(item["sequence"] for item in page["events"])
            next_cursor = page["next_sequence"]
            self.assertIsNotNone(next_cursor)
            if next_cursor == cursor:
                break
            self.assertGreater(next_cursor, cursor)
            cursor = next_cursor
        self.assertEqual(seen, expected)
        self.assertEqual(cursor, sequence)

    def test_sse_hidden_page_reconnect_and_no_duplicate(self):
        run, start = self.fixture()
        # Fill one store page with events that the trajectory channel hides.
        with self.service.runtime.store.transaction(immediate=True) as connection:
            for _ in range(1000):
                self.service.runtime.store._insert_event(connection, run, "internal", {})
        hidden_tail = self.service.events(run, after_sequence=start, limit=10000)[-1].sequence
        status, frames, _ = self.sse(run, after=start)
        self.assertEqual(status, 200)
        self.assertTrue(frames, "An all-filtered page must advance EventSource's Last-Event-ID")
        self.assertEqual(frames[-1]["id"], hidden_tail)
        self.assertEqual([frame["event"] for frame in frames], ["trace-cursor"])
        self.append(run, "harness_tool_started", {"tool_name": "agent:artifacts"})
        status, frames, _ = self.sse(run, after=start, last_id=frames[-1]["id"])
        visible = [frame for frame in frames if frame["event"] == "trace-event"]
        self.assertEqual(status, 200)
        self.assertEqual(len(visible), 1)
        self.assertEqual(visible[0]["data"]["trajectory"]["tool_name"], "agent:artifacts")
        _, repeated, body = self.sse(run, after=start, last_id=frames[-1]["id"])
        self.assertEqual(repeated, [])
        self.assertIn(": keep-alive", body)

    def test_sse_filtered_tail(self):
        run, start = self.fixture()
        visible = self.append(run, "operation_completed")
        hidden = self.append(run, "internal")
        _, frames, _ = self.sse(run, start)
        self.assertEqual([(f["event"], f["id"]) for f in frames],
                         [("trace-event", visible), ("trace-cursor", hidden)])
        self.assertEqual(self.page(run, start)["next_sequence"], hidden)

    def test_raw_channels_and_http_errors(self):
        run, start = self.fixture()
        sequence = self.append(run, "internal")
        for channel, name in (("", "internal"), ("ui", "trace-event")):
            self.assertEqual(self.page(run, start, channel=channel)["events"][0]["event_type"], "internal")
            status, frames, _ = self.sse(run, start, channel=channel)
            self.assertEqual((status, frames[0]["event"], frames[0]["id"]), (200, name, sequence))
        self.assertEqual(self.sse(run, authenticated=False)[0], 401)
        self.assertEqual(self.sse("missing")[0], 404)
        for value in ("bad", "-1", str(2**63)):
            self.assertEqual(self.sse(run, last_id=value)[0], 400)
        self.assertEqual(self.sse(run, wait="nan")[0], 400)

    def test_long_poll_receives_appended_event(self):
        run, start = self.fixture()
        entered = threading.Event()
        original = self.service.events

        def read(*args, **kwargs):
            result = original(*args, **kwargs)
            entered.set()
            return result

        result = []
        with patch.object(self.service, "events", read):
            client = threading.Thread(target=lambda: result.append(self.sse(run, start, wait=1)))
            client.start()
            try:
                self.assertTrue(entered.wait(3))
                self.append(run, "operation_completed")
            finally:
                client.join(4)
        self.assertFalse(client.is_alive())
        self.assertEqual(result[0][0], 200)
        self.assertEqual(result[0][1][0]["data"]["trajectory"]["kind"], "run_end")

    def test_real_stream_projection_is_readable(self):
        stream = ScriptedStream((
            {"event_type": "text_delta", "payload": {"delta": "轨迹正文"}},
            {"event_type": "status", "payload": {"status": "running"}},
            {"event_type": "usage", "payload": {"input_tokens": 10, "output_tokens": 2}},
            {"event_type": "completed", "payload": {"text": "轨迹正文", "finish_reason": "stop"}},
        ))
        with tempfile.TemporaryDirectory(prefix="trace-stream-") as directory:
            with AgentService(root=Path(directory), model_port=FakeModelProvider(streams=(stream,)),
                              model_streaming=True, load_external_configuration=False) as service:
                run = service.start({"session_id": "stream", "objective": "Inspect fixture",
                                     "targets": [directory], "max_actions": 0}).single.run.run_id
                service.run(run, max_actions=0)
                events = service.events(run, limit=1000)
                projected = [project_event(e) for e in events if e.event_type == "model_stream"]
                steps = [step for step in projected if step is not None]
                text = next(step for step in steps if step["kind"] == "message_update")
                self.assertIn("12", text.get("text", ""))
                self.assertIn("字节", text["text"])
                self.assertNotIn("轨迹正文", json.dumps(steps, ensure_ascii=False))
                self.assertTrue(all(step.get("title") and step.get("step_id") for step in steps))
                self.assertTrue(all(step.get("step_id") for step in steps))
                finished = next(project_event(e) for e in events if e.event_type == "harness_step_finished")
                self.assertEqual(finished["text"], "轨迹正文")

    def test_model_summary_redacts_before_truncating(self):
        secret = "sk-trajectorySECRET0123456789012345"
        self.assertNotIn(secret[:8], text_summary("x" * 588 + " " + secret, 600))
        stream = ScriptedStream((
            {"event_type": "text_delta", "payload": {"delta": "已检查 password=opaque-fixture-credential"}},
            {"event_type": "completed", "payload": {"text": "已检查 password=opaque-fixture-credential",
                "finish_reason": "stop", "structured_output": {"decision": "waiting_input"}}},
        ))
        with tempfile.TemporaryDirectory(prefix="trace-trajectory-redaction-") as directory:
            with AgentService(root=Path(directory), model_port=FakeModelProvider(streams=(stream,)),
                              model_streaming=True, load_external_configuration=False) as service:
                run = service.start({"session_id": self.id(), "objective": "Inspect fixture", "targets": [directory], "max_actions": 0}).single.run.run_id
                service.run(run, max_cycles=1)
                events = service.events(run, limit=1000)
                completed = next(e for e in events if e.event_type == "harness_step_finished")
                self.assertIn("已检查", completed.payload["text"])
                self.assertNotIn("opaque-fixture-credential", json.dumps(completed.payload))
                self.assertIn("SECRET_REF", completed.payload["text"])

    def test_model_decision_json_uses_readable_reason(self):
        provider = FakeModelProvider([{"status": "completed", "text": '{"decision":"waiting_input","reason":"请补充目标"}',
                                      "structured_output": {"decision": "waiting_input", "reason": "请补充目标"}}])
        with tempfile.TemporaryDirectory(prefix="trace-trajectory-decision-") as directory:
            with AgentService(root=Path(directory), model_port=provider, load_external_configuration=False) as service:
                run = service.start({"session_id": self.id(), "objective": "Inspect fixture",
                                     "targets": [directory], "max_actions": 0}).single.run.run_id
                service.run(run, max_cycles=1)
                step = next(project_event(e) for e in service.events(run, limit=1000)
                            if e.event_type == "harness_step_finished")
                self.assertEqual(step["text"], "请补充目标")

    def test_tool_context_and_results_through_model_loop(self):
        secret = "opaque-trajectory-password"

        class Tools:
            def discover(self):
                return (ToolDefinition(qualified_name="fixture:http", name="http", server="fixture",
                    input_schema={"type": "object"}, capabilities=("http",), side_effecting=False),)

            def invoke(self, call):
                if call.call_id == "exception":
                    raise RuntimeError("connection refused " + secret)
                failed = call.call_id == "failure"
                # Match RuntimeToolAdapter's existing credential projection before hashing.
                result = self.project({"output": {"status_code": 502 if failed else 403,
                    "body": "网关返回错误" if failed else "匿名请求已拒绝 " + secret,
                    "headers": {"Set-Cookie": "session=hidden-fixture"}},
                    "error": "upstream timeout " + secret if failed else ""})
                return ToolResult(call_id=call.call_id, tool_name=call.tool_name,
                    status="failed" if failed else "success",
                    output=result["output"], error=result["error"])

            def reconcile(self, call):
                return None

            def cancel(self, call_id):
                return False

        provider = FakeModelProvider([
            {"status": "completed", "text": "比较匿名控制与错误响应", "tool_calls": [
                {"call_id": "control", "tool_name": "fixture:http", "arguments": {
                    "method": "GET", "url": "https://fixture.invalid/resource/2?token=hidden-query"}},
                {"call_id": "failure", "tool_name": "fixture:http", "arguments": {
                    "method": "POST", "target": "fixture.invalid", "path": "/fail"}},
            ]},
            {"status": "completed", "text": "已完成响应比较 " + secret,
             "structured_output": {"decision": "waiting_input"}},
        ])
        with tempfile.TemporaryDirectory(prefix="trace-trajectory-tools-") as directory:
            tools = Tools()
            with AgentService(root=Path(directory), model_port=provider, tool_port=tools,
                              load_external_configuration=False) as service:
                tools.project = service.runtime._credential_vault.project
                service.runtime._credential_vault.bind_many({secret_reference(secret): secret})
                run = service.start({"session_id": self.id(), "objective": "Inspect fixture",
                                     "targets": ["https://fixture.invalid"], "max_actions": 0}).single.run.run_id
                service.run(run, max_cycles=2)
                events = service.events(run, limit=1000)
                projected = [step for e in events if (step := project_event(e)) is not None]
                starts = {step["call_id"]: step for step in projected if step["kind"] == "tool_start"}
                results = {step["call_id"]: step for step in projected if step["kind"] == "tool_end"}
                self.assertEqual(starts["control"]["target"], "https://fixture.invalid")
                self.assertEqual(starts["control"]["method"], "GET")
                self.assertTrue(starts["control"]["path"].startswith("/resource/2?token="))
                self.assertEqual(starts["failure"]["path"], "/fail")
                self.assertEqual(results["control"]["http_status"], "403")
                self.assertIn("匿名请求已拒绝", results["control"]["text"])
                self.assertEqual(results["failure"]["http_status"], "502")
                self.assertIn("upstream timeout", results["failure"]["error"])
                self.assertEqual(results["failure"]["status"], "failed")
                serialized = json.dumps(projected, ensure_ascii=False)
                for value in (secret, "hidden-query", "hidden-fixture"):
                    self.assertNotIn(value, serialized)
                self.assertTrue(any("已完成响应比较" in step.get("text", "") for step in projected))
                api = WebApi(service)
                token = api.control.login("trajectory", "trajectory-test-pass")
                response = api.dispatch("GET", f"/api/runs/{run}/events?channel=trajectory", headers={"Authorization": "Bearer " + token})
                self.assertEqual(response.status, 200)
                self.assertIn("匿名请求已拒绝", response.body.decode("utf-8"))
                self.assertNotIn(secret, response.body.decode("utf-8"))
                provider._responses.append({"status": "completed", "tool_calls": [
                    {"call_id": "exception", "tool_name": "fixture:http", "arguments": {"path": "/offline"}},
                ]})
                failed_run = service.start({"session_id": "tool-exception", "objective": "Inspect failed tool",
                                            "targets": ["https://fixture.invalid"], "max_actions": 0}).single.run.run_id
                with self.assertRaisesRegex(RuntimeError, "connection refused"):
                    service.run(failed_run, max_cycles=1)
                failure = next(project_event(e) for e in service.events(failed_run, limit=1000)
                               if e.event_type == "harness_tool_finished")
                self.assertEqual(failure["status"], "failed")
                self.assertIn("connection refused", failure["error"])
                self.assertNotIn(secret, failure["error"])

    def test_nested_output_and_zero_exit(self):
        output = tool_result_summary({"result": {"http_status": 200, "body": {"preview": "响应内容"}},
                                      "exit_code": 0, "error": "", "stderr": "warning: retry exhausted"})
        self.assertEqual(output["text"], "响应内容")
        self.assertEqual(output["http_status"], "200")
        self.assertEqual(output["exit_code"], "0")
        self.assertEqual(output["error"], "warning: retry exhausted")
        self.assertEqual(tool_result_summary([1, 2, 3])["text"], "返回 3 条结果")
        self.assertEqual(tool_request_summary("builtin:http-request", {"url": "https://fixture.invalid"})["method"], "GET")
        self.assertNotIn("credential", tool_request_summary("builtin:http-request", {
            "url": "https://operator:credential@fixture.invalid/path"})["target"])

    def test_raw_stream_fields_never_enter_trajectory(self):
        raw = "RAW_STREAM_JSON_SENTINEL"
        for kind, delta in (("text_delta", {"delta": raw, "byte_count": 12}),
                            ("status", {"raw": raw, "content_hash": "hash"}),
                            ("usage", {"input_tokens": 0, "output_tokens": 1, "raw": raw})):
            event = Event("run", "model_stream", {"event_type": kind, "delta": delta})
            self.assertNotIn(raw, json.dumps(project_event(event)))
        self.assertEqual(project_event(Event("run", "model_stream", {
            "event_type": "usage", "delta": {"input_tokens": 0}}))["usage"], {"input_tokens": 0})

    def test_both_renderers_show_semantics_not_raw_payloads(self):
        script = r'''
const assert = require("node:assert/strict"), fs = require("node:fs"), vm = require("node:vm");
class Node {
  constructor() { this.children = []; this.value = ""; this.parentElement = {}; }
  append(...children) { this.children.push(...children); }
  replaceChildren() { this.children = []; this.value = ""; }
  set textContent(value) { this.value = String(value); }
  get textContent() { return this.value + this.children.map(node => node.textContent).join(""); }
  set innerHTML(value) { throw new Error("Trajectory must render text, not HTML"); }
}
const timeline = new Node(), terminal = new Node(), message = new Node();
const context = { document: { createElement: () => new Node(), querySelector: (selector) =>
  ({ "#terminal-events": terminal, "#terminal-message": message })[selector] } };
vm.createContext(context);
vm.runInContext(fs.readFileSync(process.argv[1], "utf8"), context);
const events = JSON.parse(fs.readFileSync(0, "utf8"));
context.TraceUI.renderEventTimeline(timeline, events);
for (const container of [timeline, terminal]) {
  for (const text of ["模型摘要", "目标：https://fixture.invalid", "请求：GET /resource", "HTTP 403",
    "匿名请求已拒绝", "错误：upstream timeout", "输入 0 token", "退出码 0", "<script>literal</script>"])
    assert.ok(container.textContent.includes(text), text + " missing: " + container.textContent);
  assert.doesNotMatch(container.textContent, /RAW_STREAM_JSON_SENTINEL|\[object Object\]/);
}
assert.equal(message.parentElement.hidden, true);
assert.ok(timeline.children[0].textContent.includes("工具返回结果"));
assert.ok(terminal.children[0].textContent.includes("模型完成推演"));
context.TraceUI.renderEventTimeline(timeline, []);
assert.equal(terminal.children.length, 0);
assert.equal(message.parentElement.hidden, false);
console.log("PASS: both trajectory renderers, errors, zero counters, text-only DOM, empty state");
'''
        steps = [
            project_event(Event("run", "harness_step_finished", {"text": "模型摘要 <script>literal</script>", "status": "completed", "usage": {"input_tokens": 0}})),
            project_event(Event("run", "harness_tool_started", {"tool_name": "fixture:http", "target": "https://fixture.invalid", "method": "GET", "path": "/resource"})),
            project_event(Event("run", "harness_tool_finished", {"status": "failed", "text": "匿名请求已拒绝", "http_status": 403, "exit_code": 0, "error": "upstream timeout"})),
        ]
        events = [{"sequence": index, "trajectory": step, "payload": {"raw": "RAW_STREAM_JSON_SENTINEL"}} for index, step in enumerate(steps)]
        result = subprocess.run(["node", "-e", script, str(Path(__file__).resolve().parents[1] / "src/redteam_agent/static/ui.js")],
                                input=json.dumps(events), capture_output=True, text=True, encoding="utf-8", timeout=20)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_projected_tool_call_is_not_an_anonymous_tool_start(self):
        event = Event("run", "model_stream", {"event_type": "tool_call", "delta": {"projected": True, "content_hash": "hash"}})
        self.assertIsNone(project_event(event))


if __name__ == "__main__":
    unittest.main(verbosity=2)
