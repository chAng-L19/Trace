"""Run the built-in Trace HTTP tool against both local Web labs."""
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

from redteam_agent.application.agent_service import AgentService
from redteam_agent.core import ToolCall
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.open_source_tools import register_open_source_tools

from labs.web_authz_lab import AuthzHandler
from labs.web_ssti_lab import SstiHandler


def call(service, run_id, call_id, arguments):
    result = service.tools.invoke(ToolCall(
        call_id=call_id, run_id=run_id, tool_name="builtin:http-request",
        arguments=arguments, timeout_seconds=10,
    ))
    assert result.status == "success", result.to_dict()
    return dict(result.output)


def run_lab(handler, checks):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    target = f"http://127.0.0.1:{server.server_port}"
    try:
        with tempfile.TemporaryDirectory(prefix="trace-web-lab-") as directory:
            runtime = OperationRuntime(root=Path(directory), register_builtins=False)
            register_open_source_tools(runtime.broker)
            state = runtime.start(session_id="web-lab", objective="Test local Web lab",
                                  targets=(target,), model_led=True)
            with AgentService(runtime=runtime, load_external_configuration=False) as service:
                run_id = state.run_id
                outputs = [call(service, run_id, name, {"url": target + path, **kwargs})
                           for name, path, kwargs in checks]
                assert all(item.get("artifact_refs") for item in outputs)
                return target, outputs, len(outputs), len(service.runtime.artifacts.refs(run_id))
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def main():
    authz = run_lab(AuthzHandler, [
        ("surface", "/", {}),
        ("baseline", "/api/resource/1", {"headers": {"X-User-ID": "1"}}),
        ("exploit", "/api/resource/2", {"headers": {"X-User-ID": "1"}}),
        ("negative", "/api/resource/2", {}),
    ])
    assert authz[1][1]["status_code"] == 200
    assert authz[1][2]["status_code"] == 200 and authz[1][2]["body"].find('"allowed":true') >= 0
    assert authz[1][3]["status_code"] == 403
    ssti = run_lab(SstiHandler, [
        ("baseline", "/greet?name=Trace", {}),
        ("exploit", "/greet?name=%7B%7B7%2A7%7D%7D", {}),
        ("negative", "/greet?name=%7B%7B7%2B7%7D%7D", {}),
    ])
    assert "Hello, Trace" in ssti[1][0]["body"]
    assert "Hello, 49" in ssti[1][1]["body"]
    assert "Hello, 14" in ssti[1][2]["body"]
    print(json.dumps({"ok": True, "labs": {
        "authz": {"target": authz[0], "observations": authz[2], "artifacts": authz[3], "checks": [200, 200, 403]},
        "ssti": {"target": ssti[0], "observations": ssti[2], "artifacts": ssti[3], "checks": ["Trace", "49", "14"]},
    }}))


if __name__ == "__main__":
    main()
