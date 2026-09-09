from __future__ import annotations

import ast
import hashlib
import json
import re
import shutil
import socket
import subprocess
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import TYPE_CHECKING, Any, Mapping, Sequence

if TYPE_CHECKING:
    from .tool_broker import ToolBroker


MAX_HTTP_CAPTURE_BYTES = 4 * 1024 * 1024
MAX_BINARY_READ_BYTES = 16 * 1024 * 1024
MAX_PREVIEW_CHARS = 256 * 1024
MAX_RESULTS = 500


def _required_text(arguments: Mapping[str, Any], name: str) -> str:
    value = str(arguments.get(name) or "").strip()
    if not value:
        raise ValueError(f"{name}_required")
    return value


def _bounded(value: str, limit: int = MAX_PREVIEW_CHARS) -> tuple[str, bool]:
    text = str(value)
    bounded = max(1, int(limit))
    return (text[:bounded], len(text) > bounded)


def _sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_path(arguments: Mapping[str, Any]) -> tuple[Path, bytes]:
    raw = str(arguments.get("path") or arguments.get("target") or "").strip()
    if not raw:
        raise ValueError("path_required")
    path = Path(raw).expanduser().resolve(strict=True)
    if not path.is_file():
        raise ValueError("path_must_be_file")
    with path.open("rb") as handle:
        data = handle.read(MAX_BINARY_READ_BYTES + 1)
    return path, data


def http_request(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    url = _required_text(arguments, "url")
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme.casefold() not in {"http", "https"} or not parsed.netloc:
        raise ValueError("http_url_required")
    method = str(arguments.get("method") or "GET").upper().strip()
    if not re.fullmatch(r"[A-Z][A-Z0-9_-]{0,15}", method):
        raise ValueError("http_method_invalid")
    raw_headers = arguments.get("headers")
    headers = {
        str(key): str(value)
        for key, value in raw_headers.items()
    } if isinstance(raw_headers, Mapping) else {}
    body_value = arguments.get("body")
    if isinstance(body_value, (Mapping, list)):
        body = json.dumps(body_value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        headers.setdefault("Content-Type", "application/json")
    elif body_value is None:
        body = None
    else:
        body = str(body_value).encode("utf-8")
    try:
        timeout = max(0.1, min(300.0, float(arguments.get("timeout", 30.0))))
    except (TypeError, ValueError, OverflowError):
        timeout = 30.0
    request = urllib.request.Request(url, data=body, headers=headers, method=method)
    status = 0
    response_headers: Mapping[str, str] = {}
    response_body = b""
    error = ""
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status = int(response.status)
            response_headers = {str(k).lower(): str(v) for k, v in response.headers.items()}
            response_body = response.read(MAX_HTTP_CAPTURE_BYTES + 1)
    except urllib.error.HTTPError as exc:
        status = int(exc.code)
        response_headers = {str(k).lower(): str(v) for k, v in exc.headers.items()}
        response_body = exc.read(MAX_HTTP_CAPTURE_BYTES + 1)
        error = f"http_status:{status}"
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise RuntimeError(f"http_request_failed:{type(exc).__name__}:{exc}") from exc
    truncated = len(response_body) > MAX_HTTP_CAPTURE_BYTES
    if truncated:
        response_body = response_body[:MAX_HTTP_CAPTURE_BYTES]
    content_type = response_headers.get("content-type", "")
    charset = "utf-8"
    match = re.search(r"charset=([\w-]+)", content_type, re.IGNORECASE)
    if match:
        charset = match.group(1)
    decoded = response_body.decode(charset, errors="replace")
    return {
        "url": url,
        "method": method,
        "status_code": status,
        "headers": dict(response_headers),
        "body": decoded,
        "body_bytes": len(response_body),
        "body_sha256": _sha256_bytes(response_body),
        "truncated": truncated,
        "error": error,
    }


def dns_resolve(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    value = _required_text(arguments, "host")
    parsed = urllib.parse.urlsplit(value if "://" in value else f"//{value}")
    host = parsed.hostname or value.split(":", 1)[0]
    try:
        records = socket.getaddrinfo(host, None, type=socket.SOCK_STREAM)
    except socket.gaierror as exc:
        return {"host": host, "addresses": [], "error": f"dns_error:{exc}"}
    addresses = sorted({str(item[4][0]) for item in records if item[4]})
    return {"host": host, "addresses": addresses, "count": len(addresses)}


def port_probe(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    host = _required_text(arguments, "host")
    try:
        port = int(arguments.get("port"))
        timeout = max(0.1, min(30.0, float(arguments.get("timeout", 3.0))))
    except (TypeError, ValueError, OverflowError) as exc:
        raise ValueError("port_and_timeout_invalid") from exc
    if not 1 <= port <= 65535:
        raise ValueError("port_out_of_range")
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return {"host": host, "port": port, "open": True}
    except (OSError, TimeoutError) as exc:
        return {"host": host, "port": port, "open": False, "error": type(exc).__name__}


def _chrome_path() -> str:
    candidates = (
        r"C:\Program Files\Google\Chrome\Application\chrome.exe",
        r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
        "/usr/bin/google-chrome",
        "/usr/bin/chromium",
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    )
    return next((item for item in candidates if Path(item).is_file()), "")


def _browser(arguments: Mapping[str, Any], operation: str) -> Mapping[str, Any]:
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise RuntimeError("playwright_python_package_missing") from exc
    url = _required_text(arguments, "url")
    executable = str(arguments.get("executable_path") or _chrome_path()).strip()
    try:
        timeout = max(1_000, min(300_000, int(float(arguments.get("timeout", 30.0)) * 1000)))
    except (TypeError, ValueError, OverflowError):
        timeout = 30_000
    headless = bool(arguments.get("headless", True))
    launch_options: dict[str, Any] = {"headless": headless}
    if executable:
        launch_options["executable_path"] = executable
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(**launch_options)
        try:
            context = browser.new_context(ignore_https_errors=bool(arguments.get("ignore_https_errors", False)))
            page = context.new_page()
            response = page.goto(url, wait_until=str(arguments.get("wait_until") or "domcontentloaded"), timeout=timeout)
            selector = str(arguments.get("selector") or "").strip()
            if operation == "click":
                if not selector:
                    raise ValueError("selector_required")
                page.locator(selector).first.click(timeout=timeout)
            elif operation == "fill":
                if not selector:
                    raise ValueError("selector_required")
                page.locator(selector).first.fill(str(arguments.get("value") or ""), timeout=timeout)
            output_path: Path | None = None
            if operation == "screenshot":
                output_path = Path(str(arguments.get("output_path") or "browser-screenshot.png")).expanduser().resolve()
                output_path.parent.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(output_path), full_page=bool(arguments.get("full_page", False)))
            try:
                title = page.title()
            except Exception:
                title = ""
            try:
                text = page.locator("body").inner_text(timeout=timeout)
            except Exception:
                text = ""
            bounded_text, text_truncated = _bounded(text)
            links: list[Mapping[str, str]] = []
            try:
                links = list(page.locator("a").evaluate_all(
                    "els => els.slice(0, 100).map(e => ({text: (e.innerText || '').trim(), href: e.href}))"
                ))
            except Exception:
                links = []
            result: dict[str, Any] = {
                "operation": operation,
                "requested_url": url,
                "url": page.url,
                "title": title,
                "status_code": int(response.status) if response is not None else 0,
                "text": bounded_text,
                "text_truncated": text_truncated,
                "links": links,
            }
            if output_path is not None:
                result["screenshot_path"] = str(output_path)
            return result
        finally:
            browser.close()


def browser_navigate(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    return _browser(arguments, "navigate")


def browser_snapshot(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    return _browser(arguments, "snapshot")


def browser_click(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    return _browser(arguments, "click")


def browser_fill(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    return _browser(arguments, "fill")


def browser_screenshot(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    return _browser(arguments, "screenshot")


def binary_info(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    path, data = _read_path(arguments)
    magic = data[:16]
    fmt = "unknown"
    architecture = "unknown"
    details: dict[str, Any] = {}
    if magic.startswith(b"MZ"):
        fmt = "PE"
        pe_offset = int.from_bytes(data[0x3C:0x40], "little") if len(data) >= 0x40 else 0
        if 0 < pe_offset <= len(data) - 24 and data[pe_offset:pe_offset + 4] == b"PE\0\0":
            machine = int.from_bytes(data[pe_offset + 4:pe_offset + 6], "little")
            architecture = {0x014C: "x86", 0x8664: "x64", 0x01C4: "arm", 0xAA64: "arm64"}.get(machine, f"machine-0x{machine:04x}")
            details["machine"] = machine
    elif magic[:4] == b"\x7fELF":
        fmt = "ELF"
        machine = int.from_bytes(data[18:20], "little" if data[5] == 1 else "big") if len(data) >= 20 else 0
        architecture = {3: "x86", 62: "x64", 40: "arm", 183: "arm64", 8: "mips"}.get(machine, f"machine-{machine}")
        details.update({"class": 32 if len(data) > 4 and data[4] == 1 else 64, "machine": machine})
    elif magic[:4] in {b"\xfe\xed\xfa\xce", b"\xce\xfa\xed\xfe", b"\xfe\xed\xfa\xcf", b"\xcf\xfa\xed\xfe"}:
        fmt = "Mach-O"
        architecture = "arm64" if magic[:4] in {b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf"} else "x86"
    elif magic[:4] == b"\0asm":
        fmt = "WASM"
    return {
        "path": str(path),
        "format": fmt,
        "architecture": architecture,
        "size": path.stat().st_size,
        "read_bytes": len(data),
        "read_truncated": len(data) > MAX_BINARY_READ_BYTES,
        "sha256": _sha256_file(path),
        "magic_hex": magic.hex(),
        "details": details,
    }


def binary_strings(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    path, data = _read_path(arguments)
    try:
        minimum = max(2, min(64, int(arguments.get("minimum_length", 4))))
        limit = max(1, min(MAX_RESULTS, int(arguments.get("max_results", 200))))
    except (TypeError, ValueError, OverflowError) as exc:
        raise ValueError("strings_limits_invalid") from exc
    pattern = re.compile(rb"[ -~]{%d,}" % minimum)
    matches: list[Mapping[str, Any]] = [
        {"offset": item.start(), "encoding": "ascii", "value": item.group().decode("ascii", errors="replace")}
        for item in pattern.finditer(data)
    ]
    utf16_pattern = re.compile((rb"(?:[ -~]\x00){%d,}" % minimum))
    matches.extend(
        {"offset": item.start(), "encoding": "utf-16le", "value": item.group().decode("utf-16le", errors="replace").rstrip("\x00")}
        for item in utf16_pattern.finditer(data)
    )
    matches.sort(key=lambda item: (int(item["offset"]), str(item["encoding"])))
    return {"path": str(path), "strings": matches[:limit], "count": min(len(matches), limit), "truncated": len(matches) > limit}


def _capstone_architecture(name: str) -> tuple[Any, Any, str]:
    try:
        from capstone import (
            CS_ARCH_ARM,
            CS_ARCH_ARM64,
            CS_ARCH_MIPS,
            CS_ARCH_PPC,
            CS_ARCH_X86,
            CS_MODE_16,
            CS_MODE_32,
            CS_MODE_64,
            CS_MODE_ARM,
            CS_MODE_BIG_ENDIAN,
            CS_MODE_LITTLE_ENDIAN,
        )
    except ImportError as exc:
        raise RuntimeError("capstone_python_package_missing") from exc
    aliases = {
        "i386": (CS_ARCH_X86, CS_MODE_32, "x86"),
        "x86": (CS_ARCH_X86, CS_MODE_32, "x86"),
        "x64": (CS_ARCH_X86, CS_MODE_64, "x64"),
        "amd64": (CS_ARCH_X86, CS_MODE_64, "x64"),
        "x86_16": (CS_ARCH_X86, CS_MODE_16, "x86_16"),
        "arm": (CS_ARCH_ARM, CS_MODE_ARM, "arm"),
        "arm64": (CS_ARCH_ARM64, CS_MODE_ARM, "arm64"),
        "aarch64": (CS_ARCH_ARM64, CS_MODE_ARM, "arm64"),
        "mips": (CS_ARCH_MIPS, CS_MODE_32 + CS_MODE_LITTLE_ENDIAN, "mips"),
        "mipsbe": (CS_ARCH_MIPS, CS_MODE_32 + CS_MODE_BIG_ENDIAN, "mipsbe"),
        "ppc": (CS_ARCH_PPC, CS_MODE_32 + CS_MODE_LITTLE_ENDIAN, "ppc"),
    }
    return aliases.get(name.casefold(), aliases["x64"])


def binary_disassemble(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    path, data = _read_path(arguments)
    requested = str(arguments.get("architecture") or arguments.get("arch") or "x64")
    arch, mode, normalized = _capstone_architecture(requested)
    try:
        offset = max(0, int(arguments.get("offset", 0)))
        max_bytes = max(1, min(1_048_576, int(arguments.get("max_bytes", 4096))))
        max_instructions = max(1, min(MAX_RESULTS, int(arguments.get("max_instructions", 200))))
    except (TypeError, ValueError, OverflowError) as exc:
        raise ValueError("disassembly_limits_invalid") from exc
    from capstone import Cs

    engine = Cs(arch, mode)
    engine.detail = False
    instructions = [
        {
            "address": int(item.address + offset),
            "bytes": bytes(item.bytes).hex(),
            "mnemonic": item.mnemonic,
            "operands": item.op_str,
        }
        for item in list(engine.disasm(data[offset:offset + max_bytes], offset))[:max_instructions]
    ]
    return {"path": str(path), "architecture": normalized, "offset": offset, "instructions": instructions, "count": len(instructions)}


def binary_radare2(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    path, _ = _read_path(arguments)
    executable = next((shutil.which(name) for name in ("r2", "radare2", "rizin") if shutil.which(name)), "")
    if not executable:
        raise RuntimeError("open_source_disassembler_missing:install_radare2_or_rizin")
    command = str(arguments.get("command") or "aaa;afl").strip()
    try:
        timeout = max(1.0, min(300.0, float(arguments.get("timeout", 60.0))))
    except (TypeError, ValueError, OverflowError):
        timeout = 60.0
    process = subprocess.run(
        (executable, "-2", "-q", "-c", command, str(path)),
        capture_output=True,
        text=True,
        errors="replace",
        timeout=timeout,
        check=False,
    )
    output, truncated = _bounded(process.stdout)
    return {"path": str(path), "tool": Path(executable).name, "return_code": process.returncode, "output": output, "truncated": truncated}


def frida_processes(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    del arguments
    try:
        import frida
    except ImportError as exc:
        raise RuntimeError("frida_python_package_missing") from exc
    # Frida 17 exposes process enumeration on the local Device; older
    # releases kept a module-level helper. Support both without shelling out.
    device = getattr(frida, "get_local_device", lambda: None)()
    enumerate_processes = getattr(device, "enumerate_processes", None)
    if not callable(enumerate_processes):
        enumerate_processes = getattr(frida, "enumerate_processes", None)
    if not callable(enumerate_processes):
        raise RuntimeError("frida_process_inventory_unavailable")
    processes = enumerate_processes()
    return {"processes": [{"pid": int(item.pid), "name": str(item.name)} for item in processes[:MAX_RESULTS]], "count": len(processes)}


def code_search(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    root = Path(_required_text(arguments, "root")).expanduser().resolve(strict=True)
    if not root.is_dir():
        raise ValueError("root_must_be_directory")
    pattern = _required_text(arguments, "pattern")
    try:
        expression = re.compile(pattern, 0 if bool(arguments.get("case_sensitive", False)) else re.IGNORECASE)
        limit = max(1, min(MAX_RESULTS, int(arguments.get("max_results", 100))))
    except (re.error, TypeError, ValueError, OverflowError) as exc:
        raise ValueError("search_pattern_or_limit_invalid") from exc
    glob = str(arguments.get("glob") or "*")
    matches: list[Mapping[str, Any]] = []
    for path in root.rglob(glob):
        relative_parts = path.relative_to(root).parts
        if len(matches) >= limit or not path.is_file() or any(item in {".git", ".tmp", "__pycache__"} for item in relative_parts):
            continue
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for line_number, line in enumerate(text.splitlines(), start=1):
            found = expression.search(line)
            if found is None:
                continue
            matches.append({"path": str(path), "line": line_number, "column": found.start() + 1, "text": line[:2048]})
            if len(matches) >= limit:
                break
    return {"root": str(root), "pattern": pattern, "matches": matches, "count": len(matches), "truncated": len(matches) >= limit}


_DANGEROUS_CALLS = frozenset({"eval", "exec", "system", "popen", "loads", "yaml.load", "pickle.loads", "subprocess.run", "subprocess.Popen"})


def python_ast_audit(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    root = Path(_required_text(arguments, "root")).expanduser().resolve(strict=True)
    if not root.is_dir():
        raise ValueError("root_must_be_directory")
    findings: list[Mapping[str, Any]] = []
    for path in root.rglob("*.py"):
        if any(item in {".git", ".tmp", "__pycache__"} for item in path.relative_to(root).parts):
            continue
        try:
            tree = ast.parse(path.read_text(encoding="utf-8", errors="replace"), filename=str(path))
        except (OSError, SyntaxError):
            continue
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            if isinstance(node.func, ast.Name):
                name = node.func.id
            elif isinstance(node.func, ast.Attribute):
                owner = node.func.value.id if isinstance(node.func.value, ast.Name) else ""
                name = f"{owner}.{node.func.attr}" if owner else node.func.attr
            else:
                name = ""
            if name in _DANGEROUS_CALLS or name.endswith(".loads"):
                findings.append({"path": str(path), "line": int(getattr(node, "lineno", 0)), "call": name})
                if len(findings) >= MAX_RESULTS:
                    return {"root": str(root), "findings": findings, "count": len(findings), "truncated": True}
    return {"root": str(root), "findings": findings, "count": len(findings), "truncated": False}


def cloud_inventory(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    requested = str(arguments.get("provider") or "auto").casefold()
    commands: dict[str, tuple[str, ...]] = {
        "aws": ("aws", "sts", "get-caller-identity", "--output", "json"),
        "azure": ("az", "account", "show", "--output", "json"),
        "gcp": ("gcloud", "auth", "list", "--format=json"),
    }
    providers = (requested,) if requested in commands else tuple(commands)
    available = []
    for provider in providers:
        executable = shutil.which(commands[provider][0])
        if not executable:
            continue
        try:
            process = subprocess.run(commands[provider], capture_output=True, text=True, errors="replace", timeout=30.0, check=False)
            output, truncated = _bounded(process.stdout or process.stderr, 64 * 1024)
            available.append({"provider": provider, "executable": executable, "return_code": process.returncode, "output": output, "truncated": truncated})
        except (OSError, subprocess.TimeoutExpired) as exc:
            available.append({"provider": provider, "executable": executable, "error": type(exc).__name__})
    return {"requested_provider": requested, "providers": available, "count": len(available)}


def _schema(required: Sequence[str], properties: Mapping[str, Any]) -> Mapping[str, Any]:
    return {"type": "object", "required": list(required), "properties": dict(properties), "additionalProperties": False}


def register_open_source_tools(broker: ToolBroker) -> None:
    text = {"type": "string"}
    broker.register_adapter(
        name="http-request", capabilities=("page_fetch", "http_fingerprint", "controlled_validation"), adapter=http_request,
        description="Direct Python standard-library HTTP request with bounded, hashable response capture.", priority=620,
        input_schema=_schema(("url",), {"url": text, "method": text, "headers": {"type": "object"}, "body": {}, "timeout": {"type": "number"}}),
    )
    broker.register_adapter(
        name="dns-resolve", capabilities=("dns_resolve", "target_intake"), adapter=dns_resolve,
        description="Direct socket DNS resolution using the Python standard library.", priority=620,
        input_schema=_schema(("host",), {"host": text}),
    )
    broker.register_adapter(
        name="port-probe", capabilities=("port_scan", "controlled_validation"), adapter=port_probe,
        description="Direct bounded TCP connect probe without a shell or third-party scanner.", priority=620,
        input_schema=_schema(("host", "port"), {"host": text, "port": {"type": "integer", "minimum": 1, "maximum": 65535}, "timeout": {"type": "number"}}),
    )
    browser_props = {"url": text, "selector": text, "value": text, "timeout": {"type": "number"}, "headless": {"type": "boolean"}, "ignore_https_errors": {"type": "boolean"}, "wait_until": text, "executable_path": text, "output_path": text, "full_page": {"type": "boolean"}}
    for name, operation, adapter, required in (
        ("browser-navigate", "navigate", browser_navigate, ("url",)),
        ("browser-snapshot", "snapshot", browser_snapshot, ("url",)),
        ("browser-click", "click", browser_click, ("url", "selector")),
        ("browser-fill", "fill", browser_fill, ("url", "selector", "value")),
        ("browser-screenshot", "screenshot", browser_screenshot, ("url",)),
    ):
        broker.register_adapter(
            name=name, capabilities=("browser_automation", "page_fetch", "dom_snapshot"), adapter=adapter,
            description=f"Direct Microsoft Playwright browser {operation} adapter; no MCP configuration required.", priority=620,
            input_schema=_schema(required, browser_props),
            side_effecting=operation in {"click", "fill"},
        )
    broker.register_adapter(
        name="binary-info", capabilities=("binary_reverse", "binary_inventory"), adapter=binary_info,
        description="Direct open-source binary format, architecture, size and hash inspection.", priority=620,
        input_schema=_schema(("path",), {"path": text}),
    )
    broker.register_adapter(
        name="binary-strings", capabilities=("binary_reverse", "binary_inventory"), adapter=binary_strings,
        description="Direct bounded ASCII and UTF-16LE string extraction from a binary.", priority=620,
        input_schema=_schema(("path",), {"path": text, "minimum_length": {"type": "integer"}, "max_results": {"type": "integer"}}),
    )
    broker.register_adapter(
        name="binary-disassemble", capabilities=("binary_reverse", "disassemble"), adapter=binary_disassemble,
        description="Direct Capstone disassembly adapter for x86/ARM/MIPS/PowerPC samples.", priority=620,
        input_schema=_schema(("path",), {"path": text, "architecture": text, "offset": {"type": "integer"}, "max_bytes": {"type": "integer"}, "max_instructions": {"type": "integer"}}),
    )
    broker.register_adapter(
        name="binary-radare2", capabilities=("binary_reverse", "decompile", "graph_analysis"), adapter=binary_radare2,
        description="Direct read-only radare2/Rizin command adapter when the open-source executable is installed.", priority=620,
        input_schema=_schema(("path",), {"path": text, "command": text, "timeout": {"type": "number"}}),
    )
    broker.register_adapter(
        name="frida-processes", capabilities=("binary_reverse", "binary_debug"), adapter=frida_processes,
        description="Direct Frida process inventory adapter when the open-source Python package is installed.", priority=620,
        input_schema={"type": "object", "additionalProperties": False},
    )
    broker.register_adapter(
        name="code-search", capabilities=("code_analysis", "source_inventory"), adapter=code_search,
        description="Direct regex source search with bounded results and repository exclusions.", priority=620,
        input_schema=_schema(("root", "pattern"), {"root": text, "pattern": text, "glob": text, "case_sensitive": {"type": "boolean"}, "max_results": {"type": "integer"}}),
    )
    broker.register_adapter(
        name="python-ast-audit", capabilities=("code_analysis", "source_inventory", "data_flow"), adapter=python_ast_audit,
        description="Direct Python AST audit for dangerous call sites with file and line provenance.", priority=620,
        input_schema=_schema(("root",), {"root": text}),
    )
    broker.register_adapter(
        name="cloud-inventory", capabilities=("cloud_inventory", "identity_inventory", "environment_inventory"), adapter=cloud_inventory,
        description="Direct read-only inventory through locally installed open-source cloud CLIs; never writes credentials to context.", priority=620,
        input_schema={"type": "object", "properties": {"provider": {"type": "string", "enum": ["auto", "aws", "azure", "gcp"]}}, "additionalProperties": False},
    )


__all__ = [
    "binary_disassemble",
    "binary_info",
    "binary_radare2",
    "binary_strings",
    "browser_click",
    "browser_fill",
    "browser_navigate",
    "browser_screenshot",
    "browser_snapshot",
    "cloud_inventory",
    "code_search",
    "dns_resolve",
    "frida_processes",
    "http_request",
    "port_probe",
    "python_ast_audit",
    "register_open_source_tools",
]
