"""Report Trace's local capabilities without installing or downloading anything."""
from __future__ import annotations

import importlib.util
from pathlib import Path
from typing import Any

from .managed_tools import chromium_executable, managed_install, manifest, resolve_executable, tools_root


# Stable compatibility names. They describe capabilities, never packages to install.
TOOL_NAMES = (
    "page-fetch", "http-request", "dns-lookup", "tcp-connect", "code-search",
    "python-ast", "binary-disassemble", "process-inventory", "browser-automation",
    *manifest()["python_packages"], *manifest()["tools"],
    "aws", "azure", "tencent", "huawei", "volcengine", "baidu", "jdcloud",
)


def module_available(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def adapter_dependencies(name: str, *, root: Path | None = None) -> tuple[str, ...]:
    if name == "browser-automation" or name.startswith("browser-"):
        missing = []
        if not module_available("playwright"):
            missing.append("playwright")
        if not chromium_executable(root):
            missing.append("chromium")
        return tuple(missing)
    return ()


def _capability(name: str, root: Path | None = None) -> dict[str, Any]:
    if name not in TOOL_NAMES:
        return {"name": name, "installed": False, "action": "unavailable", "error": "unsupported_dependency"}
    if name in manifest()["python_packages"]:
        available = module_available(name)
        return {"name": name, "installed": available, "available": available,
                "action": "available" if available else "unavailable", "source": "python_package"}
    if name in manifest()["tools"] or name in {"aws", "azure", "tencent"}:
        commands = manifest()["tools"].get(name, {}).get("commands") or {
            "aws": ["aws"], "azure": ["az"], "tencent": ["tccli"],
        }.get(name, [])
        path = chromium_executable(root) if name == "chromium" else resolve_executable(*commands, root=root)
        managed = managed_install(name, root, allow_previous=True) if name in manifest()["tools"] else {}
        return {"name": name, "installed": bool(path), "available": bool(path),
                "action": "available" if path else "unavailable", "path": path,
                "source": "managed" if path and path == managed.get("path") else "system" if path else "missing"}
    if name in {"huawei", "volcengine", "baidu", "jdcloud"}:
        modules = {"huawei": "huaweicloudsdkecs", "volcengine": "volcenginesdkecs", "baidu": "baidubce", "jdcloud": "jdcloud_sdk"}
        available = module_available(modules[name])
        return {"name": name, "installed": available, "available": available,
                "action": "available" if available else "unavailable", "source": "python_sdk"}
    if name == "browser-automation":
        missing = adapter_dependencies(name, root=root)
        return {"name": name, "installed": not missing,
                "action": "available" if not missing else "not_installed",
                "available": not missing,
                "backend": "playwright" if not missing else "builtin:page-fetch",
                "missing": list(missing),
                "note": "页面抓取已内置；完整浏览器自动化使用离线 bundle 或本机已有的 Playwright/Chromium。"}
    return {"name": name, "installed": True, "action": "builtin", "available": True,
            "backend": "trace-native", "note": "项目内置，无需安装。"}


def prepare(names: list[str], *, root: Path | None = None, offline: bool = False,
            proxy: str | None = None, timeout: float = 600, progress=lambda message: None,
            cancel_event=None) -> dict[str, Any]:
    """Return a local capability report; this function never mutates the host."""
    del offline, proxy, timeout, cancel_event
    requested = list(dict.fromkeys(str(item) for item in names))
    results = []
    for name in requested:
        progress(f"{name}: inspecting local capability")
        results.append(_capability(name, root))
    return {"tools_root": str(tools_root(root)), "success": all(item["installed"] for item in results),
            "downloaded": False, "installed": False, "tools": results}


__all__ = ["TOOL_NAMES", "adapter_dependencies", "module_available", "prepare"]
