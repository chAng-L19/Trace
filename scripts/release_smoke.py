from __future__ import annotations

import argparse
import configparser
import importlib
import importlib.metadata
import json
import shutil
import subprocess
import sysconfig
import zipfile
from email.parser import BytesParser
from pathlib import Path, PurePosixPath


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
    print(json.dumps({"package": str(package_path), "mcp_tools": sorted(PUBLIC_TOOLS)}))


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
