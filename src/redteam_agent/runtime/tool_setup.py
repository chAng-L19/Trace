"""Explicit, pinned, user-local portable tool installation (stdlib only)."""
from __future__ import annotations

import contextlib
import hashlib
import importlib.metadata
import importlib.resources
import json
import math
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path
from typing import Any, Callable, Iterator
from uuid import uuid4

from .managed_tools import (_read_json, asset_identity, chromium_executable, contained_path,
                            managed_install, manifest, platform_key, resolve_executable, sha256, tools_root)


def remaining(deadline: float) -> float:
    value = deadline - time.monotonic()
    if value <= 0:
        raise TimeoutError("setup_timeout")
    return value


@contextlib.contextmanager
def setup_lock(root: Path, deadline: float) -> Iterator[None]:
    root.mkdir(parents=True, exist_ok=True)
    if os.name != "nt":
        root.chmod(0o700)
    # ponytail: one OS lock per tool directory; parallel downloads can come later.
    with (root / ".setup.lock").open("a+b") as stream:
        if os.fstat(stream.fileno()).st_size == 0:
            try:
                stream.write(b"0")
                stream.flush()
            except PermissionError:
                # Windows mandatory locking can race the first writer.
                pass
        while True:
            remaining(deadline)
            try:
                stream.seek(0)
                if os.name == "nt":
                    import msvcrt
                    msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except (BlockingIOError, OSError):
                time.sleep(min(0.1, remaining(deadline)))
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == "nt":
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream, fcntl.LOCK_UN)


def atomic_json(path: Path, value: Any) -> None:
    temporary = path.with_name(f".{path.name}.{uuid4().hex}.tmp")
    try:
        with temporary.open("x", encoding="utf-8") as stream:
            json.dump(value, stream, sort_keys=True, indent=2)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


class HttpsRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request: Any, fp: Any, code: int, msg: str, headers: Any, newurl: str) -> Any:
        if urllib.parse.urlsplit(newurl).scheme != "https":
            raise ValueError("download_redirect_requires_https")
        return super().redirect_request(request, fp, code, msg, headers, newurl)


def download(asset: dict[str, Any], root: Path, deadline: float, *, offline: bool,
             proxy: str | None, progress: Callable[[str], None]) -> Path:
    cache = root / "downloads"
    cache.mkdir(parents=True, exist_ok=True)
    index = _read_json(cache / "index.json")
    expected = asset.get("sha256") or index.get(asset_identity(asset), "")
    if expected and (len(expected) != 64 or any(c not in "0123456789abcdef" for c in expected)):
        raise ValueError("invalid_cached_digest")
    target = cache / expected if expected else cache / "missing"
    if expected and target.is_file() and target.stat().st_size == asset["size"] and sha256(target) == expected:
        publisher_matches = True
        if asset.get("publisher_md5"):
            with target.open("rb") as stream:
                publisher_matches = hashlib.file_digest(stream, lambda: hashlib.md5(usedforsecurity=False)).hexdigest() == asset["publisher_md5"]
        if publisher_matches:
            progress("verified cached archive")
            return target
    if offline:
        raise FileNotFoundError("offline_verified_archive_missing")
    if urllib.parse.urlsplit(asset["url"]).scheme != "https":
        raise ValueError("download_requires_https")
    handlers: list[Any] = [HttpsRedirect()]
    if proxy:
        if urllib.parse.urlsplit(proxy).scheme not in {"http", "https"}:
            raise ValueError("proxy_requires_http_or_https")
        handlers.append(urllib.request.ProxyHandler({"https": proxy, "http": proxy}))
    opener = urllib.request.build_opener(*handlers)
    temporary = cache / f".{uuid4().hex}.part"
    digest, publisher_md5, count, last = hashlib.sha256(), hashlib.md5(usedforsecurity=False), 0, 0.0
    progress(f"downloading {asset['size'] // (1024 * 1024)} MiB over HTTPS")
    try:
        request = urllib.request.Request(asset["url"], headers={"User-Agent": "trace-agent-setup/1"})
        with opener.open(request, timeout=min(15.0, remaining(deadline))) as response, temporary.open("xb") as stream:
            if urllib.parse.urlsplit(response.url).scheme != "https":
                raise ValueError("download_requires_https")
            while True:
                remaining(deadline)
                chunk = response.read1(256 * 1024)
                if not chunk:
                    break
                count += len(chunk)
                if count > asset["size"]:
                    raise ValueError("archive_size_mismatch")
                digest.update(chunk)
                publisher_md5.update(chunk)
                stream.write(chunk)
                if time.monotonic() - last >= 2:
                    progress(f"download {100 * count // asset['size']}% ({count // (1024 * 1024)} MiB)")
                    last = time.monotonic()
            stream.flush()
            os.fsync(stream.fileno())
        if (count != asset["size"] or (asset.get("sha256") and digest.hexdigest() != asset["sha256"])
                or (asset.get("publisher_md5") and publisher_md5.hexdigest() != asset["publisher_md5"])):
            raise ValueError("archive_sha256_or_size_mismatch")
        target = cache / digest.hexdigest()
        os.replace(temporary, target)
        index[asset_identity(asset)] = digest.hexdigest()
        atomic_json(cache / "index.json", index)
        return target
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"download_http_{exc.code}") from None
    except urllib.error.URLError:
        raise RuntimeError("download_network_error_check_proxy_or_offline_cache") from None
    finally:
        temporary.unlink(missing_ok=True)


def unpack(archive: Path, target: Path, kind: str, deadline: float) -> None:
    budget = 3 * 1024 * 1024 * 1024
    total = 0
    if kind == "zip":
        with zipfile.ZipFile(archive) as package:
            if len(package.infolist()) > 50000:
                raise ValueError("archive_entry_limit")
            for member in package.infolist():
                remaining(deadline)
                output = contained_path(target, member.filename.rstrip("/"))
                mode = member.external_attr >> 16
                if stat.S_ISLNK(mode):
                    raise ValueError("zip_symlink_not_supported")
                total += member.file_size
                if total > budget:
                    raise ValueError("archive_expansion_limit")
                if member.is_dir():
                    output.mkdir(parents=True, exist_ok=True)
                    continue
                output.parent.mkdir(parents=True, exist_ok=True)
                with package.open(member) as source, output.open("xb") as destination:
                    while chunk := source.read(256 * 1024):
                        remaining(deadline)
                        destination.write(chunk)
                if os.name != "nt":
                    output.chmod(0o755 if mode & 0o111 else 0o644)
    elif kind == "tar.xz":
        links = []
        with tarfile.open(archive, "r:xz") as package:
            for index, member in enumerate(package):
                remaining(deadline)
                if index >= 50000:
                    raise ValueError("archive_entry_limit")
                output = contained_path(target, member.name.rstrip("/"))
                total += member.size
                if total > budget:
                    raise ValueError("archive_expansion_limit")
                if member.isdir():
                    output.mkdir(parents=True, exist_ok=True)
                elif member.issym() or member.islnk():
                    links.append((member, output))
                elif member.isfile():
                    output.parent.mkdir(parents=True, exist_ok=True)
                    with package.extractfile(member) as source, output.open("xb") as destination:
                        while chunk := source.read(256 * 1024):
                            remaining(deadline)
                            destination.write(chunk)
                    output.chmod(0o755 if member.mode & 0o111 else 0o644)
                else:
                    raise ValueError("archive_special_file")
        for member, output in links:
            link = contained_path(target, member.linkname) if member.islnk() else output.parent / member.linkname
            if Path(member.linkname).is_absolute() or not link.resolve().is_relative_to(target.resolve()):
                raise ValueError("archive_link_outside_directory")
            output.parent.mkdir(parents=True, exist_ok=True)
            if member.islnk():
                os.link(link, output)
            else:
                output.symlink_to(member.linkname)
    else:
        raise ValueError("unsupported_archive_format")


def run_probe(command: list[str], timeout: float = 20.0) -> tuple[int, str]:
    """Bounded local validation; terminate the entire child group on cancellation."""
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=output,
                                   stderr=output, start_new_session=os.name != "nt",
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
        try:
            process.wait(timeout=timeout)
        except BaseException:
            if os.name == "nt":
                subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=False)
            else:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
            raise
        output.seek(0)
        return process.returncode, output.read(8192).decode("utf-8", errors="replace")


def validate(name: str, executable: Path, version: str, deadline: float) -> dict[str, Any]:
    if name == "chromium":
        expected = manifest()["python_packages"]["playwright"]
        if importlib.metadata.version("playwright") != expected:
            raise RuntimeError("playwright_version_mismatch_run_python_m_pip_install_playwright_" + expected)
        code = (
            "import sys; from playwright.sync_api import sync_playwright; "
            "p=sync_playwright().start(); b=p.chromium.launch(executable_path=sys.argv[1],headless=True); "
            "page=b.new_page(); page.set_content('<title>trace-setup</title>'); "
            "assert page.title()=='trace-setup'; print(b.version); b.close(); p.stop()"
        )
        command = [sys.executable, "-c", code, str(executable)]
    else:
        command = [str(executable), "-v"]
    status, output = run_probe(command, timeout=min(30.0, remaining(deadline)))
    if status or version not in output:
        if name == "chromium" and ("error while loading shared libraries" in output
                                   or "Host system is missing dependencies" in output):
            raise RuntimeError("chromium_system_libraries_missing")
        # Never include arbitrary child output (which may contain credentials).
        raise RuntimeError(f"{name}_launch_validation_failed_run_trace_doctor")
    return {"status": "passed", "version": version,
            "probe": "playwright_launch_and_page" if name == "chromium" else "executable_version"}


def install_tool(name: str, root: Path, deadline: float, *, offline: bool, proxy: str | None,
                 progress: Callable[[str], None]) -> dict[str, Any]:
    definition = manifest()["tools"][name]
    asset = definition["platforms"].get(platform_key())
    if asset is None:
        raise RuntimeError("unsupported_platform_" + platform_key())
    if name == "chromium":
        browsers = json.loads(importlib.resources.files("playwright").joinpath(
            "driver/package/browsers.json").read_text(encoding="utf-8"))["browsers"]
        release = next(item for item in browsers if item["name"] == "chromium")
        if (release["revision"] != definition["revision"] or release["browserVersion"] != definition["version"]
                or importlib.metadata.version("playwright") != manifest()["python_packages"]["playwright"]):
            raise RuntimeError("playwright_release_metadata_mismatch")
    installed = managed_install(name, root, verify=True)
    if installed["installed"]:
        progress("already installed; checksums verified")
        return {**installed, "action": "unchanged"}
    external = (chromium_executable(root, include_managed=False) if name == "chromium" else
                resolve_executable("rizin", root=root, include_managed=False))
    if installed["checksum_status"] == "mismatch":
        external = ""
    if external:
        try:
            validation = validate(name, Path(external), definition["version"], deadline)
        except (OSError, RuntimeError, subprocess.SubprocessError, importlib.metadata.PackageNotFoundError):
            pass
        else:
            progress("existing external installation passed validation")
            return {"name": name, "installed": True, "source": "system", "path": external,
                    "version": definition["version"], "checksum_status": "external_unverified",
                    "action": "unchanged", "validation": validation}
    archive = download(asset, root, deadline, offline=offline, proxy=proxy, progress=progress)
    staging = root / "staging"
    staging.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=f"{name}-", dir=staging) as temporary:
        payload = Path(temporary) / "payload"
        payload.mkdir()
        progress("extracting verified archive")
        unpack(archive, payload, asset["format"], deadline)
        executable = contained_path(payload, asset["executable"])
        progress("validating executable")
        validation = validate(name, executable, definition["version"], deadline)
        hashes = {}
        for path in sorted(payload.rglob("*")):
            remaining(deadline)
            if path.is_file():
                hashes[path.relative_to(payload).as_posix()] = sha256(path)
        atomic_json(payload / "receipt.json", {
            "name": name, "version": definition["version"], "platform": platform_key(),
            "archive_sha256": sha256(archive), "asset_identity": asset_identity(asset),
            "asset": asset,
            "url": asset["url"], "files": hashes,
            "executable": executable.relative_to(payload).as_posix(),
            "validation": validation,
        })
        directory = root / "installed" / name
        directory.mkdir(parents=True, exist_ok=True)
        generation = f"{definition['version']}-{platform_key()}-{archive.name[:16]}-{uuid4().hex[:8]}"
        destination = directory / generation
        os.replace(payload, destination)
        pointers = _read_json(root / "active.json")
        pointers[name] = generation
        # Old generations remain usable if validation or this atomic switch fails.
        try:
            atomic_json(root / "active.json", pointers)
        except BaseException:
            if destination.resolve().is_relative_to(directory.resolve()):
                shutil.rmtree(destination, ignore_errors=True)
            raise
    progress("installed and activated")
    return {**managed_install(name, root), "checksum_status": "verified", "action": "installed"}


def setup(names: list[str], *, root: Path | None = None, offline: bool = False,
          proxy: str | None = None, timeout: float = 600,
          progress: Callable[[str], None] = lambda message: None) -> dict[str, Any]:
    if not math.isfinite(timeout) or timeout <= 0:
        raise ValueError("timeout_must_be_positive_and_finite")
    root = tools_root(root)
    deadline, results = time.monotonic() + timeout, []
    with setup_lock(root, deadline):
        for name in dict.fromkeys(names):
            try:
                results.append(install_tool(name, root, deadline, offline=offline, proxy=proxy,
                                            progress=lambda message: progress(f"{name}: {message}")))
            except (OSError, ValueError, RuntimeError, subprocess.SubprocessError,
                    ImportError, importlib.metadata.PackageNotFoundError, zipfile.BadZipFile, tarfile.TarError) as exc:
                code = str(exc) if re.fullmatch(r"[a-z][a-z0-9_]*", str(exc)) else type(exc).__name__
                # Fixed error codes only; socket/proxy/subprocess exception text may contain secrets.
                results.append({"name": name, "installed": False, "action": "failed", "error": code,
                                "repair": "python -m playwright install-deps chromium" if code ==
                                "chromium_system_libraries_missing" else f"trace setup {name}"})
                progress(f"{name}: failed ({code}); previous active installation preserved")
    return {"tools_root": str(root), "platform": platform_key(), "offline": offline,
            "success": all(item["installed"] for item in results), "tools": results}
