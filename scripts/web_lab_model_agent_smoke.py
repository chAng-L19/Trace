"""Drive both local Web labs through the real AgentService model loop.

The provider is deterministic, but tool discovery, model requests, tool broker
execution, capture artifacts, and durable observations are all production
paths. This keeps the regression offline while proving the agent, rather than
the test, selects and invokes Trace's integrated HTTP tool.
"""
from __future__ import annotations

import json
import sys
import tempfile
import threading
from http.server import ThreadingHTTPServer
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.open_source_tools import register_open_source_tools

from labs.web_authz_lab import AuthzHandler
from labs.web_ssti_lab import SstiHandler


def _response(*, tool_call=None, final=False):
    payload = {"status": "completed", "text": "", "usage": {"total_tokens": 7}}
    if tool_call is not None:
        payload["tool_calls"] = [{
            "call_id": f"agent-call-{tool_call['label']}",
            "tool_name": "builtin:http-request",
            "arguments": tool_call["arguments"],
        }]
    if final:
        payload.update(text="已完成本地验证：结果已写入观察记录和捕获工件。",
                       structured_output={"decision": "waiting_input"})
    return payload


def _run(handler, objective, calls):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    target = f"http://127.0.0.1:{server.server_port}"
    try:
        with tempfile.TemporaryDirectory(prefix="trace-model-lab-") as directory:
            runtime = OperationRuntime(root=Path(directory), register_builtins=False)
            register_open_source_tools(runtime.broker)
            position = {"index": 0}

            def next_response(_request):
                index = position["index"]
                position["index"] += 1
                if index < len(calls):
                    item = calls[index]
                    arguments = dict(item["arguments"])
                    arguments["url"] = target + str(arguments["path"])
                    arguments.pop("path", None)
                    return _response(tool_call={"label": item["label"], "arguments": arguments})
                return _response(final=True)

            provider = FakeModelProvider([next_response] * (len(calls) + 1), model="fixture-agent")
            with AgentService(runtime=runtime, model_port=provider,
                              model_max_turns=len(calls) + 2,
                              load_external_configuration=False) as service:
                run_id = service.start({
                    "session_id": "model-lab",
                    "objective": objective,
                    "targets": [target],
                    "max_actions": 0,
                }).single.run.run_id
                result = service.run(run_id, max_cycles=len(calls) + 2)
                observations = service.journal.model_observations(run_id)
                http_observations = [item for item in observations if item.tool_name == "builtin:http-request"]
                assert len(http_observations) == len(calls), [item.to_dict() for item in observations]
                assert all(item.status == "success" for item in http_observations)
                assert all("builtin:http-request" in {tool["name"] for tool in request.tools}
                           for request in provider.requests)
                assert len(service.artifacts(run_id)) >= len(calls) * 3
                # Tool execution alone must not be promoted to a completed
                # finding/report without the evidence lifecycle artifacts.
                assert not result.terminal.success
                return {
                    "run_id": run_id,
                    "status": result.run.status,
                    "model_requests": len(provider.requests),
                    "http_calls": len(http_observations),
                    "artifacts": len(service.artifacts(run_id)),
                    "tool_names": sorted({item.tool_name for item in http_observations}),
                }
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)


def main():
    authz = _run(
        AuthzHandler,
        "对本地目标实际验证资源授权边界，比较同一用户读取自身和其他资源，并保留匿名拒绝控制。",
        [
            {"label": "surface", "arguments": {"path": "/"}},
            {"label": "owner", "arguments": {"path": "/api/resource/1", "headers": {"X-User-ID": "1"}}},
            {"label": "cross-owner", "arguments": {"path": "/api/resource/2", "headers": {"X-User-ID": "1"}}},
            {"label": "anonymous-control", "arguments": {"path": "/api/resource/2"}},
        ],
    )
    ssti = _run(
        SstiHandler,
        "对本地目标实际验证模板表达式是否被执行，使用基线、乘法探针和加法负向差异。",
        [
            {"label": "baseline", "arguments": {"path": "/greet?name=Trace"}},
            {"label": "multiplication", "arguments": {"path": "/greet?name=%7B%7B7%2A7%7D%7D"}},
            {"label": "addition-control", "arguments": {"path": "/greet?name=%7B%7B7%2B7%7D%7D"}},
        ],
    )
    print(json.dumps({"ok": True, "agent": "real_agent_service_model_loop", "labs": {"authz": authz, "ssti": ssti}}))


if __name__ == "__main__":
    main()
