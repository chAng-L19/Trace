"""Run the bundled engines through Trace against local fixtures, without downloads."""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from threading import Thread
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from uuid import uuid4

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.core import ToolCall
from redteam_agent.runtime.managed_tools import resolve_executable


class Page(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        body = (b"<html><title>Bundled engines</title><body><input id='input'>"
                b"<button id='apply' onclick=\"document.querySelector('#result').textContent="
                b"document.querySelector('#input').value\">Apply</button><output id='result'></output></body></html>")
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    assert resolve_executable("rizin"), "Full bundle must supply Rizin, not the native fallback"
    server = ThreadingHTTPServer(("127.0.0.1", 0), Page)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="trace-bundled-engines-") as temporary:
            with AgentService(root=Path(temporary), load_external_configuration=False) as service:
                target = f"http://127.0.0.1:{server.server_port}"
                run_id = service.start({"session_id": "bundled", "objective": "Exercise local engines",
                                        "targets": [target], "max_actions": 0}).single.run.run_id
                service.tool_catalog(run_id)

                def invoke(name, arguments):
                    result = service.tools.invoke(ToolCall(
                        call_id=uuid4().hex, run_id=run_id, tool_name="builtin:" + name, arguments=arguments))
                    assert result.status == "success", result.to_dict()
                    return result.output

                arm = Path(temporary) / "arm64.bin"
                arm.write_bytes(bytes.fromhex("c0035fd6"))
                disassembly = invoke("binary-disassemble", {"path": str(arm), "architecture": "arm64"})
                assert disassembly["instructions"][0]["mnemonic"] == "ret", disassembly
                analysis = invoke("binary-radare2", {"path": sys.executable, "command": "ij"})
                assert analysis["tool"] == "rizin" and analysis["return_code"] == 0, analysis
                assert json.loads(analysis["output"])["bin"], analysis
                processes = invoke("frida-processes", {})
                assert any(item["pid"] == os.getpid() for item in processes["processes"]), processes
                assert "backend" not in processes, "Frida must execute, not fallback"

                created = invoke("browser-create", {"url": target})
                ids = {key: created[key] for key in ("session_id", "page_id")}
                assert created["title"] == "Bundled engines" and created["status_code"] == 200
                invoke("browser-fill", {**ids, "selector": "#input", "value": "Trace local engine"})
                invoke("browser-click", {**ids, "selector": "#apply"})
                value = invoke("browser-evaluate", {**ids, "expression": "document.querySelector('#result').textContent"})
                assert value["value"] == "Trace local engine", value
                assert "Trace local engine" in invoke("browser-snapshot", ids)["text"]
                screenshot = invoke("browser-screenshot", ids)
                assert screenshot["screenshot_artifact"], screenshot

                import frida
                child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
                session = None
                try:
                    session = frida.get_local_device().attach(child.pid)
                    script = session.create_script("rpc.exports = { echo: function (value) { return value; } };")
                    script.load()
                    assert script.exports_sync.echo("Trace local attach") == "Trace local attach"
                finally:
                    if session is not None:
                        session.detach()
                    child.terminate()
                    child.wait(timeout=10)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
    print(json.dumps({"ok": True, "checks": ["capstone-arm64", "rizin-real-analysis",
        "frida-processes", "frida-own-child-attach", "browser-create-fill-click-evaluate-snapshot-screenshot"]}))


if __name__ == "__main__":
    main()
