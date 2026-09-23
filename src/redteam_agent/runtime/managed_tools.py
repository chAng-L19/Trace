"""Read-only tool resolution. Downloads belong exclusively to ``trace setup``."""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import platform
import shutil
from importlib.resources import files
from pathlib import Path
from typing import Any


def manifest() -> dict[str, Any]:
    return json.loads(files("redteam_agent").joinpath("tool_manifest.json").read_text(encoding="utf-8"))


def tools_root(root: Path | None = None) -> Path:
    if root is not None:
        return root.expanduser().resolve()
    if os.environ.get("TRACE_TOOLS_HOME"):
        return Path(os.environ["TRACE_TOOLS_HOME"]).expanduser().resolve()
    if os.environ.get("TRACE_HOME"):
        return (Path(os.environ["TRACE_HOME"]).expanduser() / "tools").resolve()
    base = Path(os.environ.get("XDG_CACHE_HOME") or (
        os.environ.get("LOCALAPPDATA", str(Path.home() / "AppData/Local"))
        if os.name == "nt" else str(Path.home() / ".cache")))
    return (base.expanduser() / "trace" / "tools").resolve()


def platform_key() -> str:
    machine = platform.machine().lower()
    arch = {"amd64": "x86_64", "x64": "x86_64", "aarch64": "arm64"}.get(machine, machine)
    return f"{platform.system().lower()}-{arch}"


def sha256(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def asset_identity(asset: dict[str, Any]) -> str:
    return hashlib.sha256(json.dumps(asset, sort_keys=True).encode()).hexdigest()


def _read_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        return value if isinstance(value, dict) else {}
    except (OSError, ValueError):
        return {}


def contained_path(root: Path, relative: str) -> Path:
    # Reject Windows separators/drives even when inspecting an archive on Linux.
    if not relative or "\\" in relative or ":" in relative:
        raise ValueError("invalid_relative_path")
    path = root / relative
    if Path(relative).is_absolute() or ".." in Path(relative).parts:
        raise ValueError("invalid_relative_path")
    if not path.resolve().is_relative_to(root.resolve()):
        raise ValueError("path_outside_tool_directory")
    return path


def managed_install(name: str, root: Path | None = None, *, verify: bool = False,
                    allow_previous: bool = False) -> dict[str, Any]:
    base = tools_root(root)
    tool = manifest()["tools"][name]
    asset = tool["platforms"].get(platform_key())
    result = {"name": name, "installed": False, "source": "missing", "path": "",
              "version": "", "expected_version": tool["version"], "checksum_status": "not_installed"}
    if asset is None:
        return {**result, "checksum_status": "unsupported_platform"}
    pointer = _read_json(base / "active.json").get(name)
    if not isinstance(pointer, str):
        return result
    try:
        directory = contained_path(base / "installed" / name, pointer)
        receipt = _read_json(directory / "receipt.json")
        current_identity = asset_identity(asset)
        recorded_asset = receipt.get("asset")
        if allow_previous and isinstance(recorded_asset, dict):
            asset = recorded_asset
        relative_executable = receipt.get("executable", asset["executable"])
        if not isinstance(relative_executable, str):
            raise ValueError("invalid_executable_path")
        if relative_executable != asset["executable"]:
            raise ValueError("unexpected_executable_path")
        executable = contained_path(directory, relative_executable)
        version_matches = receipt.get("version") == tool["version"] and asset_identity(asset) == current_identity
        valid = (receipt.get("name") == name and receipt.get("asset_identity") == asset_identity(asset)
                 and (not asset.get("sha256") or receipt.get("archive_sha256") == asset["sha256"])
                 and receipt.get("platform") == platform_key()
                 and (version_matches or allow_previous) and executable.is_file())
        if valid and verify:
            hashes = receipt.get("files")
            actual = {path.relative_to(directory).as_posix() for path in directory.rglob("*")
                      if path.is_file() and path != directory / "receipt.json"}
            valid = isinstance(hashes, dict) and actual == set(hashes) and relative_executable in hashes and all(
                contained_path(directory, relative).is_file()
                and sha256(contained_path(directory, relative)) == digest
                for relative, digest in hashes.items())
        return {**result, "installed": bool(valid), "source": "managed" if valid else "missing",
                "version": receipt.get("version", "") if valid else "", "version_matches": version_matches,
                "path": str(executable) if valid else "", "directory": str(directory),
                "checksum_status": "verified" if valid and verify else "recorded" if valid else "mismatch",
                "archive_sha256": receipt.get("archive_sha256", ""),
                "source_verification": "publisher_sha256" if asset.get("sha256") else "playwright_release_metadata_and_publisher_md5",
                "validation": receipt.get("validation", {})}
    except (OSError, ValueError, TypeError, KeyError):
        return {**result, "checksum_status": "mismatch"}


def resolve_executable(*names: str, root: Path | None = None, path: str | None = None,
                       include_managed: bool = True) -> str:
    """Search managed tools before PATH; never change the caller's environment."""
    for tool, definition in (manifest()["tools"].items() if include_managed else ()):
        if any(name in definition["commands"] for name in names):
            managed = managed_install(tool, root, allow_previous=True)
            if managed["installed"]:
                return str(managed["path"])
    return next((found for name in names if (found := shutil.which(name, path=path))), "")


def chromium_executable(root: Path | None = None, *, include_managed: bool = True) -> str:
    if include_managed:
        managed = managed_install("chromium", root, allow_previous=True)
        if managed["installed"]:
            return str(managed["path"])
    cached = playwright_chromium()
    if cached:
        return cached
    found = resolve_executable("chromium", "chromium-browser", "google-chrome", "chrome", root=root,
                               include_managed=False)
    if found:
        return found
    candidates = (
        Path(os.environ.get("PROGRAMFILES", "C:/Program Files")) / "Google/Chrome/Application/chrome.exe",
        Path(os.environ.get("PROGRAMFILES(X86)", "C:/Program Files (x86)")) / "Google/Chrome/Application/chrome.exe",
        Path(os.environ.get("LOCALAPPDATA", str(Path.home()))) / "Google/Chrome/Application/chrome.exe",
        Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
    )
    return next((str(item) for item in candidates if item.is_file()), "")


def playwright_chromium() -> str:
    """Inspect Playwright's existing cache without spawning a driver or downloading."""
    tool = manifest()["tools"]["chromium"]
    asset = tool["platforms"].get(platform_key())
    if asset is None:
        return ""
    configured = os.environ.get("PLAYWRIGHT_BROWSERS_PATH", "")
    if configured == "0":
        spec = importlib.util.find_spec("playwright")
        if spec is None or not spec.origin:
            return ""
        base = Path(spec.origin).parent / "driver/package/.local-browsers"
    elif configured:
        base = Path(configured).expanduser()
    elif os.name == "nt":
        base = Path(os.environ.get("LOCALAPPDATA", str(Path.home() / "AppData/Local"))) / "ms-playwright"
    else:
        base = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "ms-playwright"
    executable = base / f"chromium-{tool['revision']}" / asset["executable"]
    return str(executable) if executable.is_file() else ""
