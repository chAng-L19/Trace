from __future__ import annotations

import argparse
import configparser
import importlib
import importlib.metadata
import json
import os
import shutil
import socket
import subprocess
import sysconfig
import time
import zipfile
from email.parser import BytesParser
from pathlib import Path, PurePosixPath
from urllib.request import ProxyHandler, build_opener


ENTRY_POINTS = {
    "trace": "redteam_agent.cli:main",
    "trace-mcp": "redteam_agent.runtime.mcp_transport:main",
    "trace-web": "redteam_agent.adapters.web:main",
    "redteam-agent": "redteam_agent.cli:main",
    "redteam-agent-mcp": "redteam_agent.runtime.mcp_transport:main",
    "redteam-agent-web": "redteam_agent.adapters.web:main",
}
PUBLIC_TOOLS = {
    "redteam_run",
    "redteam_status",
    "redteam_evidence",
    "redteam_cancel",
    "redteam_events",
}


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def audit_wheel(path: Path) -> None:
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        bad = []
        for name in names:
            parts = PurePosixPath(name).parts
            if (
                "tests" in parts
                or "__pycache__" in parts
                or "build" in parts
                or name.endswith((".pyc", ".pyo"))
            ):
                bad.append(name)
        _require(not bad, f"wheel_contains_build_residue:{bad[:10]}")

        metadata_names = [name for name in names if name.endswith(".dist-info/METADATA")]
        entry_names = [name for name in names if name.endswith(".dist-info/entry_points.txt")]
        _require(len(metadata_names) == 1, "wheel_metadata_missing")
        _require(len(entry_names) == 1, "wheel_entry_points_missing")

        metadata = BytesParser().parsebytes(archive.read(metadata_names[0]))
        _require(metadata.get("Name") == "trace-agent", "wheel_name_mismatch")
        _require("Trace" in str(metadata.get("Summary") or ""), "wheel_summary_mismatch")

        entries = configparser.ConfigParser(interpolation=None)
        entries.read_string(archive.read(entry_names[0]).decode("utf-8"))
        actual = dict(entries["console_scripts"])
        _require(actual == ENTRY_POINTS, f"wheel_entry_points_mismatch:{actual}")
    print(json.dumps({"wheel": str(path), "entry_points": sorted(ENTRY_POINTS)}))


def _executable(name: str) -> str:
    scripts = Path(sysconfig.get_path("scripts"))
    executable = shutil.which(name, path=str(scripts))
    _require(bool(executable), f"entry_point_missing:{name}:{scripts}")
    return str(executable)


def _run(command: list[str], *, cwd: Path, input_text: str | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        command,
        cwd=cwd,
        input=input_text,
        text=True,
        capture_output=True,
        timeout=90,
        check=False,
    )
    _require(result.returncode == 0, f"command_failed:{command[0]}:{result.stderr[-2000:]}")
    return result


def _mcp_smoke(command: str, *, cwd: Path, root: Path) -> None:
    requests = (
        json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})
        + "\n"
        + json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        + "\n"
    )
    result = _run([command, "--root", str(root)], cwd=cwd, input_text=requests)
    responses = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
    by_id = {item.get("id"): item.get("result") for item in responses}
    _require(by_id.get(1, {}).get("serverInfo", {}).get("name") == "trace-agent-runtime", "mcp_identity_mismatch")
    tools = by_id.get(2, {}).get("tools", [])
    names = {str(item.get("name") or "") for item in tools}
    _require(names == PUBLIC_TOOLS, f"mcp_tools_mismatch:{sorted(names)}")


def _web_smoke(command: str, *, cwd: Path) -> None:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    opener = build_opener(ProxyHandler({}))
    with (cwd / "web-smoke.log").open("w", encoding="utf-8") as log:
        process = subprocess.Popen(
            [command, "--root", str(cwd / "web-state"), "--port", str(port)],
            cwd=cwd, stdout=log, stderr=log,
        )
        try:
            deadline = time.monotonic() + 30
            while True:
                _require(process.poll() is None, "web_exited_before_ready")
                try:
                    response = opener.open(f"http://127.0.0.1:{port}/api/auth/status", timeout=2)
                    break
                except OSError:
                    _require(time.monotonic() < deadline, "web_start_timeout")
                    time.sleep(0.1)
            with response:
                payload = json.load(response)
                _require(response.status == 200 and payload.get("ok") is True, "web_api_not_ready")
                _require(isinstance(payload.get("authenticated"), bool), "web_auth_status_invalid")
            with opener.open(f"http://127.0.0.1:{port}/", timeout=2) as page:
                _require(page.status == 200 and b"Trace" in page.read(), "wheel_static_ui_missing")
        finally:
            process.terminate()
            try:
                code = process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                raise RuntimeError("web_shutdown_timeout") from None
        if os.name != "nt":
            _require(code == 0, f"web_sigterm_not_graceful:{code}")


def _mcp_shutdown_smoke(command: str, *, cwd: Path) -> None:
    if os.name == "nt":
        return  # Windows TerminateProcess is not POSIX SIGTERM.
    output = cwd / "mcp-shutdown.jsonl"
    with output.open("w", encoding="utf-8") as log:
        process = subprocess.Popen(
            [command, "--root", str(cwd / "mcp-shutdown-state")],
            stdin=subprocess.PIPE, stdout=log, stderr=log, text=True, cwd=cwd,
        )
        try:
            process.stdin.write('{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n')
            process.stdin.flush()
            deadline = time.monotonic() + 30
            while '"serverInfo"' not in output.read_text(encoding="utf-8"):
                _require(process.poll() is None and time.monotonic() < deadline, "mcp_shutdown_start_timeout")
                time.sleep(0.1)
            # Keep stdin open and idle: SIGTERM must interrupt its blocking read.
            process.terminate()
            _require(process.wait(timeout=15) == 0, "mcp_sigterm_not_graceful")
        finally:
            process.stdin.close()
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)


def installed_smoke(root: Path, source_root: Path) -> None:
    distribution = importlib.metadata.distribution("trace-agent")
    entries = {
        item.name: item.value
        for item in distribution.entry_points
        if item.group == "console_scripts" and item.name in ENTRY_POINTS
    }
    _require(entries == ENTRY_POINTS, f"installed_entry_points_mismatch:{entries}")

    package = importlib.import_module("redteam_agent")
    package_path = Path(package.__file__).resolve()
    try:
        package_path.relative_to(source_root.resolve())
    except ValueError:
        pass
    else:
        raise RuntimeError(f"source_tree_imported:{package_path}")

    commands = {name: _executable(name) for name in ENTRY_POINTS}
    _run([commands["trace"], "self-test"], cwd=root)
    _run([commands["redteam-agent"], "self-test"], cwd=root)
    _run([commands["trace-web"], "--help"], cwd=root)
    _run([commands["redteam-agent-web"], "--help"], cwd=root)
    _mcp_smoke(commands["trace-mcp"], cwd=root, root=root / "trace-mcp-state")
    _mcp_smoke(commands["redteam-agent-mcp"], cwd=root, root=root / "legacy-mcp-state")
    _mcp_shutdown_smoke(commands["trace-mcp"], cwd=root)
    _web_smoke(commands["trace-web"], cwd=root)
    print(json.dumps({"package": str(package_path), "mcp_tools": sorted(PUBLIC_TOOLS), "web_http": "ok"}))


def main() -> int:
    parser = argparse.ArgumentParser(description="Trace release smoke checks")
    subcommands = parser.add_subparsers(dest="command", required=True)
    wheel = subcommands.add_parser("audit-wheel")
    wheel.add_argument("wheel", type=Path)
    installed = subcommands.add_parser("installed-smoke")
    installed.add_argument("--root", type=Path, required=True)
    installed.add_argument("--source-root", type=Path, required=True)
    arguments = parser.parse_args()
    if arguments.command == "audit-wheel":
        audit_wheel(arguments.wheel)
    else:
        arguments.root.mkdir(parents=True, exist_ok=True)
        installed_smoke(arguments.root.resolve(), arguments.source_root.resolve())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
