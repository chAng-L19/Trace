from __future__ import annotations

import threading
import sys
from types import SimpleNamespace
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from redteam_agent.runtime.operation_runtime import OperationRuntime


class _FixtureHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - stdlib protocol name
        body = b"<!doctype html><html><body><h1>Fixture</h1><button id='probe' onclick=\"document.body.dataset.clicked='yes'; document.querySelector('h1').textContent='Clicked'\">Probe</button></body></html>"
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def _broker(tmp_path: Path):
    runtime = OperationRuntime(root=tmp_path / "runtime")
    return runtime, {item.qualified_name: item for item in runtime.broker.descriptors()}


def test_builtin_open_source_catalog(tmp_path: Path) -> None:
    runtime, descriptors = _broker(tmp_path)
    expected = {
        "builtin:http-request",
        "builtin:dns-resolve",
        "builtin:port-probe",
        "builtin:browser-navigate",
        "builtin:browser-snapshot",
        "builtin:browser-click",
        "builtin:browser-fill",
        "builtin:browser-screenshot",
        "builtin:binary-info",
        "builtin:binary-strings",
        "builtin:binary-disassemble",
        "builtin:binary-radare2",
        "builtin:frida-processes",
        "builtin:code-search",
        "builtin:python-ast-audit",
        "builtin:cloud-inventory",
    }
    assert expected <= set(descriptors)
    assert all(descriptors[name].source == "registered-adapter" for name in expected)
    assert descriptors["builtin:browser-click"].side_effecting is True
    runtime.broker.close()


def test_http_request_fixture(tmp_path: Path) -> None:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _FixtureHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        runtime, descriptors = _broker(tmp_path)
        url = f"http://127.0.0.1:{server.server_port}/fixture"
        result = runtime.broker.call(descriptors["builtin:http-request"], {"url": url})
        assert result.status == "success"
        assert result.output["status_code"] == 200
        assert "Fixture" in result.output["body"]
        assert len(result.output["body_sha256"]) == 64
        runtime.broker.close()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_browser_snapshot_and_click_fixture(tmp_path: Path) -> None:
    server = ThreadingHTTPServer(("127.0.0.1", 0), _FixtureHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        runtime, descriptors = _broker(tmp_path)
        url = f"http://127.0.0.1:{server.server_port}/fixture"
        snapshot = runtime.broker.call(descriptors["builtin:browser-snapshot"], {"url": url})
        assert snapshot.status == "success"
        assert snapshot.output["title"] == ""
        assert "Probe" in snapshot.output["text"]
        clicked = runtime.broker.call(
            descriptors["builtin:browser-click"],
            {"url": url, "selector": "#probe"},
        )
        assert clicked.status == "success"
        assert "Clicked" in clicked.output["text"]
        runtime.broker.close()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_binary_tools_fixture(tmp_path: Path) -> None:
    runtime, descriptors = _broker(tmp_path)
    sample = tmp_path / "sample.bin"
    sample.write_bytes(b"\x90\x90\xc3\x00HELLO\x00SAMPLE\x00")
    info = runtime.broker.call(descriptors["builtin:binary-info"], {"path": str(sample)})
    strings = runtime.broker.call(descriptors["builtin:binary-strings"], {"path": str(sample)})
    disasm = runtime.broker.call(
        descriptors["builtin:binary-disassemble"],
        {"path": str(sample), "architecture": "x86", "max_instructions": 3},
    )
    assert info.status == strings.status == disasm.status == "success"
    assert info.output["format"] == "unknown"
    assert any(item["value"] == "HELLO" for item in strings.output["strings"])
    assert disasm.output["count"] >= 1
    runtime.broker.close()


def test_code_audit_tools_fixture(tmp_path: Path) -> None:
    runtime, descriptors = _broker(tmp_path)
    source = tmp_path / "source.py"
    source.write_text("import subprocess\nvalue = eval(user_input)\nsubprocess.run(['x'])\n", encoding="utf-8")
    search = runtime.broker.call(
        descriptors["builtin:code-search"],
        {"root": str(tmp_path), "pattern": "eval\\(", "glob": "*.py"},
    )
    audit = runtime.broker.call(descriptors["builtin:python-ast-audit"], {"root": str(tmp_path)})
    assert search.status == audit.status == "success"
    assert search.output["matches"][0]["line"] == 2
    calls = {item["call"] for item in audit.output["findings"]}
    assert {"eval", "subprocess.run"} <= calls
    runtime.broker.close()


def test_frida_process_inventory_uses_local_device_api(tmp_path: Path, monkeypatch) -> None:
    class _Device:
        def enumerate_processes(self):
            return [SimpleNamespace(pid=1234, name="fixture.exe")]

    monkeypatch.setitem(sys.modules, "frida", SimpleNamespace(get_local_device=lambda: _Device()))
    runtime, descriptors = _broker(tmp_path)
    result = runtime.broker.call(descriptors["builtin:frida-processes"], {})
    assert result.status == "success"
    assert result.output == {
        "processes": [{"pid": 1234, "name": "fixture.exe"}],
        "count": 1,
    }
    runtime.broker.close()
