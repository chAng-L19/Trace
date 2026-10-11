"""Read-only capability doctor for Trace's project-native tool plane."""
from __future__ import annotations

import os
import subprocess
import sys
import tomllib
from pathlib import Path
from typing import Any

from .managed_tools import chromium_executable, managed_install, platform_key, tools_root, resolve_executable
from .tool_prepare import TOOL_NAMES, module_available, prepare
from .tool_setup import run_probe


def _module(name: str) -> dict[str, Any]:
    available = module_available(name)
    return {"name": name, "installed": available, "status": "available" if available else "not_installed",
            "source": "python_package", "repair": "从完整离线 bundle 安装所需引擎；运行时不下载安装。"}


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

    optional = [_module("playwright"), _module("capstone"), _module("frida")]
    browser_path = chromium_executable(root)
    chromium = bool(browser_path)
    browser_validation = "not_probed"
    if probe_browser and chromium and module_available("playwright"):
        code = ("import sys; from playwright.sync_api import sync_playwright; "
                "p=sync_playwright().start(); b=p.chromium.launch(executable_path=sys.argv[1],headless=True); "
                "page=b.new_page(); page.set_content('<title>trace-doctor</title>'); "
                "assert page.title()=='trace-doctor'; b.close(); p.stop()")
        try:
            exit_code, _ = run_probe([sys.executable, "-c", code, browser_path], timeout=20)
            browser_validation = "passed" if exit_code == 0 else "failed"
        except (OSError, subprocess.SubprocessError):
            browser_validation = "failed"
    managed_browser = managed_install("chromium", root, allow_previous=True)
    optional.append({"name": "chromium", "installed": chromium,
                     "status": "failed" if browser_validation == "failed" else "available" if chromium else "not_installed",
                     "source": "managed" if chromium and browser_path == managed_browser.get("path") else "system" if chromium else "missing",
                     "path": browser_path, "runtime_validation": browser_validation,
                     "repair": "使用随包 Chromium，并预先准备宿主系统库；--probe-browser 可验证本地启动，运行时不安装系统库。"})
    tools = optional + [item for item in prepare(list(dict.fromkeys(TOOL_NAMES)), root=root)["tools"]
                        if item["name"] not in {"playwright", "capstone", "frida", "chromium"}] + [
        {"name": "trace-native", "installed": True, "status": "installed", "source": "project",
         "capabilities": ["http", "dns", "tcp", "page_fetch", "code_search", "python_ast",
                          "binary_metadata", "binary_strings", "x86_x64_disassembly", "process_inventory"]},
    ]
    playwright_ready = module_available("playwright") and chromium
    reverse = bool(resolve_executable("rizin", "r2", "radare2", root=root))
    frida = module_available("frida")
    clouds = [item for item in tools if item["name"] in {
        "aws", "azure", "tencent", "gcp", "aliyun", "huawei", "volcengine", "baidu", "jdcloud"}]
    caps = [
        {"name": "http_dns_tcp_code_search_binary_metadata", "status": "ready", "provider": "trace-native"},
        {"name": "browser_page_fetch", "status": "ready", "provider": "trace-native",
         "note": "HTML 文本、链接和表单解析完全由项目内置。"},
        {"name": "disassembly", "status": "ready" if module_available("capstone") else "partial", "provider": "capstone" if module_available("capstone") else "trace-native-x86",
         "fallback_capabilities": [] if module_available("capstone") else ["x86", "x64"]},
        {"name": "browser_automation", "status": "ready" if browser_validation == "passed" else "failed" if browser_validation == "failed" else "installed_unprobed" if playwright_ready else "optional_unavailable",
         "provider": "playwright" if playwright_ready else "trace-native:page-fetch",
         "note": "使用随包或主机已有引擎；启动失败时检查宿主系统库，运行时不下载浏览器。"},
        {"name": "graph_analysis", "status": "installed_unprobed" if reverse else "optional_unavailable", "provider": "rizin/radare2" if reverse else "trace-native",
         "missing_capabilities": ["decompile"] if reverse else ["decompile", "rizin_commands"]},
        {"name": "frida_local_processes", "status": "installed_unprobed" if frida else "partial", "provider": "frida" if frida else "trace-native-process-inventory",
         "fallback_capabilities": [] if frida else ["process_inventory"],
         "missing_capabilities": [] if frida else ["attach", "injection"]},
        {"name": "cloud_inventory", "status": "external_credentials_required" if all(item["installed"] for item in clouds)
         else "partial" if any(item["installed"] for item in clouds) else "optional_unavailable", "provider": "read-only adapter",
         "note": "使用随包或主机已有 CLI/SDK；各提供商是否安装见 tools，云凭据及查询权限需单独配置。"},
    ]
    return {"schema_version": 1, "tools_root": str(root), "platform": platform_key(),
            "tools": tools, "runtime_root": str(state_root.expanduser().resolve()),
            "configuration": configuration, "capabilities": caps,
            "mcp_servers": _mcp_status(configs or [], root), "mcp_note": "MCP 服务按配置启停；Trace 不下载第三方服务。",
            "ready": configuration.get("status") != "invalid" and (not probe_browser or browser_validation == "passed"),
            "downloads_allowed": False, "installers_enabled": False}


def _mcp_status(configs: list[Path], root: Path) -> list[dict[str, Any]]:
    from .mcp_config import parse_mcp_server_specs
    rows = []
    for path in configs:
        if not path.is_file():
            continue
        try:
            specs = parse_mcp_server_specs(path, tomllib.loads(path.read_text(encoding="utf-8-sig")))
        except (OSError, ValueError):
            rows.append({"config": str(path), "status": "invalid_config"})
            continue
        for spec in specs:
            rendered = spec.render()
            executable = resolve_executable(rendered.command, root=root, path=rendered.env.get("PATH")) if rendered.command else ""
            rows.append({"name": spec.name, "config": str(path), "enabled": spec.enabled,
                "transport": spec.transport, "launcher_path": executable,
                "status": "disabled" if not spec.enabled else "configured_unprobed" if executable or spec.transport == "http" else "missing_launcher",
                "capabilities": "not_discovered"})
    return rows


__all__ = ["doctor"]
