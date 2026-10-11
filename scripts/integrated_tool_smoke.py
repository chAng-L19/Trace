"""Regression for project-native optional-tool fallbacks."""
from __future__ import annotations

import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.runtime.open_source_tools import (
    _native_process_inventory,
    binary_disassemble,
    page_fetch,
    register_open_source_tools,
)
from redteam_agent.runtime.operation_runtime import OperationRuntime


def main():
    class PageHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            body = b"<html><head><title>Trace page</title></head><body><a href='/next'>Next</a><form action='/login' method='post'><input name='user' /></form></body></html>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = ThreadingHTTPServer(("127.0.0.1", 0), PageHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        page = page_fetch({"url": f"http://127.0.0.1:{server.server_port}/"})
        assert page["title"] == "Trace page"
        assert page["links"] == [{"text": "Next", "href": f"http://127.0.0.1:{server.server_port}/next"}]
        assert page["forms"][0]["method"] == "POST" and page["forms"][0]["fields"][0]["name"] == "user"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
    with tempfile.TemporaryDirectory(prefix="trace-native-tools-") as directory:
        sample = Path(directory) / "sample.bin"
        sample.write_bytes(bytes.fromhex("5589e590c3"))
        with patch("redteam_agent.runtime.open_source_tools._capstone_architecture",
                   side_effect=RuntimeError("capstone_python_package_missing")):
            result = binary_disassemble({"path": str(sample), "architecture": "x64"})
        assert result["backend"] == "trace-native-x86"
        assert [item["mnemonic"] for item in result["instructions"]] == ["push", "mov", "nop", "ret"]

        native = _native_process_inventory()
        assert native["backend"] == "trace-native-process-inventory"
        assert native["instrumentation_available"] is False

        runtime = OperationRuntime(root=Path(directory), register_builtins=False)
        register_open_source_tools(runtime.broker)
        with patch("redteam_agent.runtime.tool_prepare.module_available", return_value=False):
            descriptors = {item.qualified_name: item for item in runtime.broker.descriptors()}
        assert descriptors["builtin:binary-disassemble"].healthy
        assert descriptors["builtin:frida-processes"].healthy
    print("integrated_tool_smoke: PASS (native page fetch, disassembly, process inventory, catalog visibility)")


if __name__ == "__main__":
    main()
