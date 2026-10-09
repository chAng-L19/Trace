"""On-demand preparation through existing portable setup and standard package managers."""
from __future__ import annotations

import importlib
import importlib.util
import math
import os
import subprocess
import sys
import time
from pathlib import Path

from .managed_tools import chromium_executable, manifest, resolve_executable, tools_root
from .tool_setup import remaining, run_probe, setup, setup_cancellation, setup_lock
from .security import redact_sensitive, safe_error_text

# Only dependencies consumed by existing adapters belong here.
CLOUD_PACKAGES = {
    "aws": ("awscli>=1,<2", "awscli", "aws"),
    "azure": ("azure-cli>=2,<3", "azure.cli", "az"),
    "tencent": ("tccli", "tccli", "tccli"),
    "huawei": ("huaweicloudsdkecs==3.1.217", "huaweicloudsdkecs", ""),
    "volcengine": ("volcengine-python-sdk==5.0.50", "volcenginesdkecs", ""),
    "baidu": ("bce-python-sdk==0.9.79", "baidubce", ""),
    "jdcloud": ("jdcloud-sdk==1.6.346", "jdcloud_sdk", ""),
}
TOOL_NAMES = (*manifest()["python_packages"], *manifest()["tools"], *CLOUD_PACKAGES)


def module_available(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def adapter_dependencies(name: str) -> tuple[str, ...]:
    if name.startswith("browser-"):
        return tuple(item for item, ready in (("playwright", module_available("playwright")),
                                              ("chromium", bool(chromium_executable()))) if not ready)
    if name == "binary-disassemble" and not module_available("capstone"):
        return ("capstone",)
    if name == "frida-processes" and not module_available("frida"):
        return ("frida",)
    return ()


def _pip_command(requirement: str, *, offline: bool, proxy: str | None) -> list[str]:
    target = os.environ.get("PIP_TARGET")
    uv = resolve_executable("uv", include_managed=False) if not proxy and not target else ""
    command = ([uv, "pip", "install", "--python", sys.executable] if uv else
               [sys.executable, "-m", "pip", "install"])
    if target:
        command.append("--upgrade")
    if offline:
        command.append("--offline" if uv else "--no-index")
    if proxy:
        command.extend(("--proxy", proxy))
    return [*command, requirement]


def _package(name: str, deadline: float, *, offline: bool, proxy: str | None) -> dict:
    pinned = manifest()["python_packages"].get(name)
    requirement, module, executable = ((f"{name}=={pinned}", name, "") if pinned else CLOUD_PACKAGES[name])
    check = [sys.executable, "-c", f"import {module}"]
    if pinned:
        check[-1] += f"; import importlib.metadata; assert importlib.metadata.version('{name}') == '{pinned}'"
    code, _ = run_probe(check, timeout=remaining(deadline))
    action = "unchanged"
    if code:
        code, output = run_probe(_pip_command(requirement, offline=offline, proxy=proxy), timeout=remaining(deadline))
        if code:
            return {"name": name, "installed": False, "action": "failed", "error": "package_install_failed",
                    "exit_code": code, "output": redact_sensitive(output)}
        importlib.invalidate_caches()
        action = "installed"
        code, output = run_probe(check, timeout=remaining(deadline))
        if code:
            return {"name": name, "installed": False, "action": "failed", "error": "package_import_failed",
                    "exit_code": code, "output": redact_sensitive(output)}
    path = resolve_executable(executable) if executable else ""
    if executable:
        code, output = run_probe([path, "version" if name == "tencent" else "--version"], timeout=remaining(deadline)) if path else (1, "launcher_missing")
        if code:
            return {"name": name, "installed": False, "action": "failed", "path": path,
                    "error": "launcher_validation_failed", "output": redact_sensitive(output)}
    return {"name": name, "installed": True, "action": action, "path": path, "validation": "passed"}


def _prepare(names: list[str], *, root: Path | None = None, offline: bool = False,
            proxy: str | None = None, timeout: float = 600, progress=lambda message: None) -> dict:
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("timeout_must_be_positive_and_finite")
    deadline, results = time.monotonic() + timeout, []
    requested = list(dict.fromkeys(names))
    if "chromium" in requested and "playwright" not in requested:
        requested.insert(0, "playwright")
    for name in requested:
        try:
            progress(f"{name}: preparing")
            if name in manifest()["python_packages"] or name in CLOUD_PACKAGES:
                with setup_lock(tools_root(root), deadline):
                    result = _package(name, deadline, offline=offline, proxy=proxy)
            elif name in manifest()["tools"]:
                result = setup([name], root=root, offline=offline, proxy=proxy,
                               timeout=remaining(deadline), progress=progress)["tools"][0]
            else:
                result = {"name": name, "installed": False, "action": "failed", "error": "unsupported_dependency"}
        except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as exc:
            result = {"name": name, "installed": False, "action": "failed", "error": safe_error_text(exc)}
        results.append(result)
        if result.get("error") == "tool_preparation_cancelled":
            break
    importlib.invalidate_caches()
    return {"tools_root": str(tools_root(root)), "success": all(item["installed"] for item in results),
            "offline": offline, "tools": results}


def prepare(names: list[str], *, root: Path | None = None, offline: bool = False,
            proxy: str | None = None, timeout: float = 600, progress=lambda message: None,
            cancel_event=None) -> dict:
    with setup_cancellation(cancel_event):
        return _prepare(names, root=root, offline=offline, proxy=proxy, timeout=timeout, progress=progress)
