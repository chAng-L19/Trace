"""Local dependency inventory. No downloads, cloud login, or MCP server startup."""
from __future__ import annotations

import importlib.metadata
import os
import re
import subprocess
import sys
import tomllib
from pathlib import Path
from typing import Any

from .managed_tools import (chromium_executable, managed_install, manifest,
                            platform_key, playwright_chromium, resolve_executable, tools_root)
from .tool_setup import run_probe


def _version(command: list[str]) -> tuple[str, str]:
    try:
        code, output = run_probe(command, timeout=5)
        match = re.search(r"\b\d+\.\d+(?:\.\d+){0,2}\b", output)
        return (match.group() if match else "", "passed" if code == 0 else "failed")
    except (OSError, subprocess.SubprocessError):
        return "", "failed"


def _package(name: str, pinned: str) -> dict[str, Any]:
    row: dict[str, Any] = {"name": name, "status": "missing", "installed": False, "source": "missing",
                           "version": "", "expected_version": pinned, "path": "",
                           "checksum_status": "package_manager", "repair": f'python -m pip install "{name}=={pinned}"'}
    try:
        distribution = importlib.metadata.distribution(name)
        row.update(version=distribution.version, path=str(distribution.locate_file(name)))
        code, _ = run_probe([sys.executable, "-c", f"import {name}"], timeout=5)
        row.update(installed=code == 0, status="installed" if code == 0 else "missing",
                   source="system" if code == 0 else "missing", validation="passed" if code == 0 else "import_failed",
                   version_matches=distribution.version == pinned)
    except (importlib.metadata.PackageNotFoundError, OSError, subprocess.SubprocessError):
        pass
    return row


def _mcp_status(configs: list[Path], root: Path) -> list[dict[str, Any]]:
    from .mcp_config import parse_mcp_server_specs

    rows = []
    for path in configs:
        if not path.is_file():
            continue
        try:
            specs = parse_mcp_server_specs(path, tomllib.loads(path.read_text(encoding="utf-8-sig")))
        except (OSError, ValueError):
            rows.append({"config": str(path), "status": "invalid_config", "repair": "trace mcp-doctor"})
            continue
        for spec in specs:
            rendered = spec.render()
            executable = resolve_executable(rendered.command, root=root, path=rendered.env.get("PATH")) if rendered.command else ""
            # Intentionally omit argv, URL, headers, and environment: they can contain credentials.
            rows.append({"name": spec.name, "config": str(path), "enabled": spec.enabled,
                         "transport": spec.transport, "status": "disabled" if not spec.enabled else
                         "configured_unprobed" if executable or spec.transport == "http" else "missing_launcher",
                         "launcher_path": executable, "capabilities": "not_discovered",
                         "repair": "trace mcp-doctor --config " + subprocess.list2cmdline([str(path)])})
    return rows


def doctor(*, root: Path | None = None, configs: list[Path] | None = None,
           probe_browser: bool = False, runtime_root: Path | None = None) -> dict[str, Any]:
    from ..application.bootstrap import configuration_projection

    root = tools_root(root)
    state_root = runtime_root or (Path(os.environ["TRACE_HOME"]) if os.environ.get("TRACE_HOME") else
                                 Path(os.environ.get("REDTEAM_AGENT_HOME", str(Path.home() / ".redteam-agent"))) / "operations")
    try:
        configuration = configuration_projection(state_root, configs)
    except (OSError, ValueError, TypeError) as exc:
        configuration = {"status": "invalid", "error": type(exc).__name__}
    rows = [_package(name, version) for name, version in manifest()["python_packages"].items()]
    browser = managed_install("chromium", root, verify=True, allow_previous=True)
    external = chromium_executable(root, include_managed=False)
    if not browser["installed"] and external:
        browser.update(installed=True, source="fallback" if external == playwright_chromium() else "system",
                       managed_checksum_status=browser["checksum_status"],
                       path=external, version="", checksum_status="external_unverified")
    browser.update(status="installed" if browser["installed"] else "missing", repair="trace setup chromium",
                   runtime_validation="not_probed", dependency_repair="python -m playwright install-deps chromium")
    if browser["installed"] and probe_browser:
        code = ("import sys; from playwright.sync_api import sync_playwright; p=sync_playwright().start(); "
                "b=p.chromium.launch(executable_path=sys.argv[1],headless=True); page=b.new_page(); "
                "page.set_content('<title>trace-doctor</title>'); assert page.title()=='trace-doctor'; "
                "print(b.version); b.close(); p.stop()")
        browser["version"], browser["runtime_validation"] = _version([sys.executable, "-c", code, browser["path"]])
    elif browser["installed"] and browser["source"] != "managed":
        # Launching Chrome --version on Windows can open a visible user window.
        browser["version"] = (manifest()["tools"]["chromium"]["version"]
                                if external == playwright_chromium() else "not_probed")
    rows.append(browser)
    reverse = managed_install("rizin", root, verify=True, allow_previous=True)
    reverse.update(repair="trace setup rizin", status="installed" if reverse["installed"] else "missing")
    if not reverse["installed"]:
        executable = resolve_executable("r2", "radare2", "rizin", root=root, include_managed=False)
        if executable:
            version, validation = _version([executable, "-v"])
            reverse.update(installed=validation == "passed", path=executable, version=version,
                           managed_checksum_status=reverse["checksum_status"],
                           source="system", status="installed" if validation == "passed" else "missing",
                           checksum_status="external_unverified", runtime_validation=validation)
        else:
            reverse.update(source="fallback", fallback="trace-binary-query",
                           fallback_capabilities=["binary_metadata", "binary_strings", "disassembly"],
                           missing_capabilities=["graph_analysis", "radare2_commands", "decompile"])
    rows.append(reverse)
    cloud = (
        ("aws", "aws", 'python -m pip install "awscli>=1,<2"'),
        ("azure", "az", 'python -m pip install "azure-cli>=2,<3"'),
        ("gcp", "gcloud", "conda install -c conda-forge google-cloud-sdk"),
    )
    for name, command, repair in cloud:
        executable = resolve_executable(command, root=root)
        version, validation = _version([executable, "--version"]) if executable else ("", "not_installed")
        rows.append({"name": name, "installed": bool(executable), "status": "installed" if executable else "missing",
                     "source": "system" if executable else "missing", "path": executable, "version": version,
                     "checksum_status": "external_unverified" if executable else "not_installed",
                     "runtime_validation": validation, "authentication": "not_checked", "repair": repair})
    by_name = {item["name"]: item for item in rows}
    caps = [
        {"name": "http_dns_tcp_code_search_binary_metadata", "status": "ready", "provider": "python_stdlib"},
        {"name": "disassembly", "status": "ready" if by_name["capstone"]["installed"] else "missing", "provider": "capstone"},
        {"name": "browser_automation", "status": "ready" if browser["runtime_validation"] == "passed" else
         "installed_unprobed" if browser["installed"] and by_name["playwright"]["installed"] else "missing", "provider": browser["source"]},
        {"name": "graph_analysis", "status": "ready" if reverse["installed"] else "missing", "provider": reverse["source"]},
        {"name": "decompile", "status": "plugin_unprobed", "provider": "rizin/radare2", "repair": "rz-pm -i rz-ghidra"},
        {"name": "frida_local_processes", "status": "installed_unprobed" if by_name["frida"]["installed"] else "missing",
         "provider": "frida", "repair": by_name["frida"]["repair"]},
        {"name": "frida_device", "status": "device_unprobed", "provider": "external",
         "repair": "python -m pip install frida-tools; frida-ls-devices",
         "note": "Device-side frida-server must match the Python Frida version and device architecture; Trace does not deploy it."},
        *({"name": f"cloud_{name}", "status": "credentials_unprobed" if by_name[name]["installed"] else "missing",
           "provider": name, "repair": by_name[name]["repair"]} for name, _, _ in cloud),
    ]
    return {"schema_version": 1, "tools_root": str(root), "platform": platform_key(), "tools": rows,
            "runtime_root": str(state_root.expanduser().resolve()), "configuration": configuration,
            "capabilities": caps, "mcp_servers": _mcp_status(configs or [], root),
            "mcp_note": "Third-party servers are configured separately; run trace mcp-doctor for live discovery.",
            "ready": browser["runtime_validation"] != "failed" and all(
                by_name[name]["installed"] and by_name[name].get("version_matches", True)
                for name in ("capstone", "frida", "playwright", "chromium", "rizin"))}
