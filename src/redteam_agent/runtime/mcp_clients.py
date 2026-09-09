from __future__ import annotations

import hashlib
import json
import math
import os
import re
import shlex
import queue
import shutil
import subprocess
import threading
import time
import tomllib
import urllib.error
import urllib.request
from collections.abc import Callable, Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .models import ToolCallResult, ToolDescriptor, utc_now
from .security import redact_sensitive, safe_error_text


TOKEN_RE = re.compile(r"[a-z0-9]+", re.IGNORECASE)
MAX_MCP_RESPONSE_BYTES = 8 * 1024 * 1024
MAX_MCP_PENDING_RESPONSES = 2048
MAX_TOOL_OUTPUT_BYTES = 16 * 1024 * 1024
MCP_READ_CHUNK_BYTES = 64 * 1024
MAX_ERROR_TEXT_BYTES = 1024


@dataclass
class ToolHealthState:
    successes: int = 0
    failures: int = 0
    semantic_failures: int = 0
    consecutive_failures: int = 0
    average_latency_ms: float = 0.0
    cooldown_until: float = 0.0
    last_error: str = ""

CAPABILITY_MARKERS: dict[str, frozenset[str]] = {
    "page_fetch": frozenset({"fetch", "http", "request", "curl", "webfetch", "web", "url", "uri"}),
    "browser_automation": frozenset({"browser", "playwright", "navigate", "click", "dom", "screenshot"}),
    "dns_resolve": frozenset({"dns", "resolve", "dig", "host"}),
    "subdomain_enum": frozenset({"subdomain", "subfinder", "amass"}),
    "port_scan": frozenset({"nmap", "masscan", "port", "scan"}),
    "http_fingerprint": frozenset({"fingerprint", "wappalyzer", "httpx", "technology"}),
    "cve_search": frozenset({"cve", "vulnerability", "nvd"}),
    "binary_reverse": frozenset({"ghidra", "radare", "rizin", "capstone", "frida", "disassemble", "decompile", "binary"}),
    "apk_decompile": frozenset({"jadx", "apk", "android", "decompile"}),
    "android_static_analysis": frozenset({"jadx", "apk", "android", "manifest"}),
    "code_analysis": frozenset({"code", "source", "repository", "audit", "search"}),
    "source_inventory": frozenset({"source", "repository", "tree", "files"}),
    "code_generation": frozenset({"codex", "agent", "generate", "code", "harness"}),
    "test_harness": frozenset({"test", "harness", "runner"}),
    "reasoning": frozenset({"codex", "agent", "reason", "analyze", "planner"}),
    "controlled_validation": frozenset({"validate", "reproduce", "exploit", "execute", "runner"}),
    "impact_analysis": frozenset({"impact", "analyze", "reason", "verify"}),
    "coverage_analysis": frozenset({"coverage", "review", "analyze", "reason"}),
    "cleanup": frozenset({"cleanup", "remove", "restore", "rollback"}),
    "rollback": frozenset({"rollback", "restore", "cleanup"}),
    "report_generation": frozenset({"report", "write", "generate", "codex", "agent"}),
    "model_probe": frozenset({"model", "prompt", "chat", "completion"}),
    "prompt_evaluation": frozenset({"prompt", "evaluation", "eval", "model"}),
    "evaluation_analysis": frozenset({"evaluation", "metrics", "analyze"}),
    "identity_inventory": frozenset({"identity", "directory", "ldap", "principal", "kerberos"}),
    "cloud_inventory": frozenset({"cloud", "aws", "azure", "gcp", "iam"}),
    "directory_inventory": frozenset({"directory", "ldap", "active", "inventory"}),
    "identity_validation": frozenset({"identity", "permission", "role", "validate"}),
    "graph_analysis": frozenset({"graph", "path", "analyze"}),
    "environment_inventory": frozenset({"environment", "inventory", "asset", "host"}),
    "technique_execution": frozenset({"atomic", "technique", "execute", "attack"}),
    "attack_mapping": frozenset({"attack", "mitre", "technique", "mapping"}),
    "telemetry_analysis": frozenset({"telemetry", "log", "event", "detection"}),
    "target_intake": frozenset({"target", "inspect", "inventory", "fetch", "analyze"}),
}

CAPABILITY_ALIASES: dict[str, frozenset[str]] = {
    "page_fetch": frozenset({"page_fetch", "content_extract", "http_fetch"}),
    "browser_automation": frozenset({"browser_automation", "dom_snapshot", "screenshot"}),
    "binary_reverse": frozenset({"binary_reverse", "protocol_analysis", "decompile", "disassemble"}),
    "apk_decompile": frozenset({"apk_decompile", "android_static_analysis"}),
    "reasoning": frozenset({"reasoning", "code_generation", "ai_coding"}),
    "report_generation": frozenset({"report_generation", "reasoning", "code_generation"}),
    "impact_analysis": frozenset({"impact_analysis", "reasoning"}),
    "coverage_analysis": frozenset({"coverage_analysis", "reasoning"}),
    "cleanup": frozenset({"cleanup", "rollback"}),
}


class StdioMcpClient:
    def __init__(
        self,
        server_name: str,
        command: str,
        args: Sequence[str],
        env: Mapping[str, str] | None = None,
        *,
        cwd: Path | None = None,
        startup_timeout: float = 20.0,
        roots: Sequence[Path] = (),
    ) -> None:
        self.server_name = server_name
        self.roots = tuple(path.expanduser().resolve(strict=False) for path in roots)
        environment = dict(os.environ)
        environment.update({str(key): str(value) for key, value in dict(env or {}).items()})
        executable = shutil.which(command) or command
        self.process = subprocess.Popen(
            [executable, *args],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=False,
            bufsize=0,
            env=environment,
            cwd=cwd,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
            start_new_session=os.name != "nt",
        )
        self._next_id = 1
        self._responses: dict[int, Mapping[str, Any]] = {}
        self._abandoned: set[int] = set()
        self._pending: set[int] = set()
        self._active_request_ids: dict[str, int] = {}
        self._reader_error = ""
        self._tools_changed = False
        self._condition = threading.Condition()
        self._write_lock = threading.Lock()
        self._stderr: queue.Queue[str] = queue.Queue(maxsize=128)
        self._reader = threading.Thread(target=self._read_stdout, daemon=True)
        self._error_reader = threading.Thread(target=self._read_stderr, daemon=True)
        self._reader.start()
        self._error_reader.start()
        self._initialize(timeout=startup_timeout)

    def _read_stdout(self) -> None:
        if self.process.stdout is None:
            return
        buffer = bytearray()
        discarding = False
        while True:
            chunk = self.process.stdout.read(MCP_READ_CHUNK_BYTES)
            if not chunk:
                return
            if discarding:
                if b"\n" in chunk:
                    _, remainder = chunk.split(b"\n", 1)
                    buffer.extend(remainder)
                    discarding = False
                else:
                    continue
            else:
                buffer.extend(chunk)
            while True:
                newline = buffer.find(b"\n")
                if newline < 0:
                    if len(buffer) <= MAX_MCP_RESPONSE_BYTES:
                        break
                    buffer.clear()
                    discarding = True
                    with self._condition:
                        self._reader_error = safe_error_text(
                            f"mcp_stdio_response_too_large:{self.server_name}"
                        )
                        self._condition.notify_all()
                    break
                raw_line = bytes(buffer[:newline])
                del buffer[: newline + 1]
                if len(raw_line) > MAX_MCP_RESPONSE_BYTES:
                    with self._condition:
                        self._reader_error = safe_error_text(
                            f"mcp_stdio_response_too_large:{self.server_name}"
                        )
                        self._condition.notify_all()
                    continue
                stripped = raw_line.strip()
                if not stripped:
                    continue
                try:
                    payload = json.loads(stripped.decode("utf-8", errors="replace"))
                except json.JSONDecodeError:
                    continue
                if isinstance(payload, Mapping) and isinstance(payload.get("method"), str):
                    self._handle_server_message(payload)
                    continue
                response_id = payload.get("id") if isinstance(payload, Mapping) else None
                if isinstance(response_id, int):
                    with self._condition:
                        if response_id in self._abandoned:
                            self._abandoned.discard(response_id)
                        elif response_id in self._pending:
                            self._responses[response_id] = payload
                            if len(self._responses) > MAX_MCP_PENDING_RESPONSES:
                                oldest = min(self._responses)
                                self._responses.pop(oldest, None)
                                self._pending.discard(oldest)
                        self._condition.notify_all()

    def _handle_server_message(self, payload: Mapping[str, Any]) -> None:
        method = str(payload.get("method") or "")
        if method in {"notifications/tools/list_changed", "notifications/tools/changed"}:
            with self._condition:
                self._tools_changed = True
            return
        request_id = payload.get("id")
        if request_id is None:
            return
        if method == "roots/list":
            roots = [{"uri": path.as_uri(), "name": path.name or str(path)} for path in self.roots]
            response: Mapping[str, Any] = {"jsonrpc": "2.0", "id": request_id, "result": {"roots": roots}}
        else:
            response = {
                "jsonrpc": "2.0",
                "id": request_id,
                "error": {"code": -32601, "message": "method_not_supported"},
            }
        try:
            self._send(response)
        except Exception:
            pass

    def _read_stderr(self) -> None:
        if self.process.stderr is None:
            return
        buffer = bytearray()
        while True:
            chunk = self.process.stderr.read(MCP_READ_CHUNK_BYTES)
            if not chunk:
                return
            buffer.extend(chunk)
            while True:
                newline = buffer.find(b"\n")
                if newline < 0:
                    if len(buffer) > MAX_ERROR_TEXT_BYTES:
                        del buffer[:-MAX_ERROR_TEXT_BYTES]
                    break
                line = bytes(buffer[:newline])
                del buffer[: newline + 1]
                text = safe_error_text(line.decode("utf-8", errors="replace"), limit=MAX_ERROR_TEXT_BYTES)
                try:
                    self._stderr.put_nowait(text)
                except queue.Full:
                    try:
                        self._stderr.get_nowait()
                        self._stderr.put_nowait(text)
                    except queue.Empty:
                        pass

    def _send(self, payload: Mapping[str, Any]) -> None:
        if self.process.poll() is not None:
            if self._error_reader.is_alive():
                self._error_reader.join(timeout=0.05)
            detail = self.stderr_snapshot()
            suffix = f":{detail}" if detail else ""
            raise RuntimeError(
                safe_error_text(
                    f"mcp_server_exited:{self.server_name}:{self.process.returncode}{suffix}"
                )
            )
        if self.process.stdin is None:
            raise RuntimeError(safe_error_text(f"mcp_server_stdin_missing:{self.server_name}"))
        encoded = (json.dumps(dict(payload), ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")
        # Multiple request threads share one stdio stream.  Keep each JSON-RPC
        # frame intact; interleaved writes otherwise corrupt both requests.
        with self._write_lock:
            self.process.stdin.write(encoded)
            self.process.stdin.flush()

    def request(
        self,
        method: str,
        params: Mapping[str, Any] | None = None,
        *,
        timeout: float = 30.0,
        cancellation_id: str = "",
    ) -> Mapping[str, Any]:
        with self._condition:
            request_id = self._next_id
            self._next_id += 1
            self._pending.add(request_id)
            try:
                self._send(
                    {
                        "jsonrpc": "2.0",
                        "id": request_id,
                        "method": method,
                        "params": dict(params or {}),
                    }
                )
            except BaseException:
                self._pending.discard(request_id)
                raise
            if cancellation_id:
                # Publish cancellation only after the request frame is on the
                # transport. Otherwise a concurrent cancel can overtake the
                # tools/call frame and be ignored by a conforming server.
                self._active_request_ids[cancellation_id] = request_id
        deadline = time.monotonic() + max(0.1, timeout)
        with self._condition:
            while request_id not in self._responses:
                if self._reader_error:
                    self._pending.discard(request_id)
                    if cancellation_id:
                        self._active_request_ids.pop(cancellation_id, None)
                    raise ValueError(safe_error_text(self._reader_error))
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    self._abandoned.add(request_id)
                    self._pending.discard(request_id)
                    if cancellation_id:
                        self._active_request_ids.pop(cancellation_id, None)
                    if len(self._abandoned) > 1024:
                        self._abandoned.pop()
                    try:
                        self.notify(
                            "notifications/cancelled",
                            {"requestId": request_id, "reason": f"timeout:{method}"},
                        )
                    except Exception:
                        pass
                    raise TimeoutError(safe_error_text(f"mcp_request_timeout:{self.server_name}:{method}"))
                self._condition.wait(timeout=min(remaining, 0.25))
                if self.process.poll() is not None and request_id not in self._responses:
                    self._pending.discard(request_id)
                    if cancellation_id:
                        self._active_request_ids.pop(cancellation_id, None)
                    if self._error_reader.is_alive():
                        self._error_reader.join(timeout=0.05)
                    detail = self.stderr_snapshot()
                    suffix = f":{detail}" if detail else ""
                    raise RuntimeError(
                        safe_error_text(
                            f"mcp_server_exited:{self.server_name}:{self.process.returncode}{suffix}"
                        )
                    )
            response = self._responses.pop(request_id)
            self._pending.discard(request_id)
            if cancellation_id:
                self._active_request_ids.pop(cancellation_id, None)
        if "error" in response:
            raise RuntimeError(safe_error_text(f"mcp_error:{self.server_name}:{method}:{response['error']}"))
        result = response.get("result")
        return result if isinstance(result, Mapping) else {"value": result}

    def notify(self, method: str, params: Mapping[str, Any] | None = None) -> None:
        self._send({"jsonrpc": "2.0", "method": method, "params": dict(params or {})})

    def stderr_snapshot(self) -> str:
        lines: list[str] = []
        while True:
            try:
                lines.append(self._stderr.get_nowait())
            except queue.Empty:
                break
        # The actionable reason is normally the final traceback line; keeping
        # only the tail avoids truncating it behind a long Python traceback.
        return safe_error_text(lines[-1] if lines else "", limit=MAX_ERROR_TEXT_BYTES)

    def _initialize(self, *, timeout: float = 20.0) -> None:
        self.request(
            "initialize",
            {
                "protocolVersion": "2025-06-18",
                "capabilities": {"roots": {"listChanged": False}},
                "clientInfo": {"name": "redteam-agent-runtime", "version": "1"},
            },
            timeout=timeout,
        )
        self.notify("notifications/initialized")

    def list_tools(self) -> Sequence[Mapping[str, Any]]:
        collected: list[Mapping[str, Any]] = []
        cursor = ""
        for _ in range(100):
            result = self.request("tools/list", {"cursor": cursor} if cursor else {}, timeout=30.0)
            tools = result.get("tools")
            if isinstance(tools, list):
                collected.extend(item for item in tools if isinstance(item, Mapping))
            next_cursor = result.get("nextCursor")
            if not isinstance(next_cursor, str) or not next_cursor or next_cursor == cursor:
                break
            cursor = next_cursor
        return tuple(collected)

    def consume_tools_changed(self) -> bool:
        with self._condition:
            changed = self._tools_changed
            self._tools_changed = False
            return changed

    def call_tool(
        self,
        name: str,
        arguments: Mapping[str, Any],
        *,
        timeout: float,
        cancellation_id: str = "",
    ) -> Mapping[str, Any]:
        return self.request(
            "tools/call",
            {"name": name, "arguments": dict(arguments)},
            timeout=timeout,
            cancellation_id=cancellation_id,
        )

    def cancel_request(self, cancellation_id: str) -> bool:
        with self._condition:
            request_id = self._active_request_ids.get(cancellation_id)
        if request_id is None:
            return False
        self.notify(
            "notifications/cancelled",
            {"requestId": request_id, "reason": "runtime_cancelled"},
        )
        return True

    def close(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=3.0)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=3.0)


class HttpMcpClient:
    def __init__(
        self,
        server_name: str,
        url: str,
        headers: Mapping[str, str] | None = None,
        *,
        startup_timeout: float = 20.0,
    ) -> None:
        self.server_name = server_name
        self.url = url
        self.headers = {str(key): str(value) for key, value in dict(headers or {}).items()}
        self.session_id = ""
        self._next_id = 1
        self._request_lock = threading.RLock()
        self._initialize(timeout=startup_timeout)

    def _decode_response(self, response: Any) -> Mapping[str, Any]:
        content_length = response.headers.get("Content-Length")
        try:
            declared_length = int(content_length) if content_length else 0
        except (TypeError, ValueError):
            declared_length = 0
        if declared_length > MAX_MCP_RESPONSE_BYTES:
            raise ValueError(safe_error_text(f"mcp_http_response_too_large:{self.server_name}"))
        encoded = response.read(MAX_MCP_RESPONSE_BYTES + 1)
        if len(encoded) > MAX_MCP_RESPONSE_BYTES:
            raise ValueError(safe_error_text(f"mcp_http_response_too_large:{self.server_name}"))
        raw = encoded.decode("utf-8", errors="replace")
        content_type = str(response.headers.get("Content-Type") or "").casefold()
        session_id = response.headers.get("Mcp-Session-Id")
        if session_id:
            self.session_id = str(session_id)
        if "text/event-stream" in content_type or raw.lstrip().startswith("event:"):
            data_lines = [line[5:].strip() for line in raw.splitlines() if line.startswith("data:")]
            if not data_lines:
                raise ValueError(safe_error_text(f"mcp_sse_data_missing:{self.server_name}"))
            raw = data_lines[-1]
        payload = json.loads(raw)
        if not isinstance(payload, Mapping):
            raise ValueError(safe_error_text(f"mcp_http_invalid_response:{self.server_name}"))
        return payload

    def request(self, method: str, params: Mapping[str, Any] | None = None, *, timeout: float = 30.0) -> Mapping[str, Any]:
        # HTTP MCP session state is mutable (request IDs and Mcp-Session-Id).
        # Serialize requests to avoid accepting a response under a session that
        # another concurrent call has just replaced.
        with self._request_lock:
            request_id = self._next_id
            self._next_id += 1
            body = json.dumps(
                {"jsonrpc": "2.0", "id": request_id, "method": method, "params": dict(params or {})},
                ensure_ascii=False,
            ).encode("utf-8")
            headers = {
                "Content-Type": "application/json",
                "Accept": "application/json, text/event-stream",
                **self.headers,
            }
            if self.session_id:
                headers["Mcp-Session-Id"] = self.session_id
            request = urllib.request.Request(self.url, data=body, headers=headers, method="POST")
            try:
                with urllib.request.urlopen(request, timeout=max(0.1, timeout)) as response:
                    payload = self._decode_response(response)
            except urllib.error.HTTPError as exc:
                detail = exc.read(MAX_ERROR_TEXT_BYTES + 1)
                if len(detail) > MAX_ERROR_TEXT_BYTES:
                    detail = detail[:MAX_ERROR_TEXT_BYTES] + b"...[truncated]"
                raise RuntimeError(
                    safe_error_text(
                        f"mcp_http_error:{self.server_name}:{exc.code}:{detail.decode('utf-8', errors='replace')}",
                        limit=MAX_ERROR_TEXT_BYTES,
                    )
                ) from exc
        if payload.get("id") != request_id:
            raise ValueError(safe_error_text(f"mcp_http_response_id_mismatch:{self.server_name}"))
        if "error" in payload:
            raise RuntimeError(safe_error_text(f"mcp_error:{self.server_name}:{method}:{payload['error']}"))
        result = payload.get("result")
        return result if isinstance(result, Mapping) else {"value": result}

    def notify(self, method: str, params: Mapping[str, Any] | None = None) -> None:
        with self._request_lock:
            body = json.dumps(
                {"jsonrpc": "2.0", "method": method, "params": dict(params or {})},
                ensure_ascii=False,
            ).encode("utf-8")
            headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream", **self.headers}
            if self.session_id:
                headers["Mcp-Session-Id"] = self.session_id
            request = urllib.request.Request(self.url, data=body, headers=headers, method="POST")
            try:
                with urllib.request.urlopen(request, timeout=10.0):
                    return
            except urllib.error.HTTPError as exc:
                if exc.code not in {202, 204}:
                    raise

    def _initialize(self, *, timeout: float = 20.0) -> None:
        self.request(
            "initialize",
            {
                "protocolVersion": "2025-06-18",
                "capabilities": {"roots": {"listChanged": False}},
                "clientInfo": {"name": "redteam-agent-runtime", "version": "1"},
            },
            timeout=timeout,
        )
        self.notify("notifications/initialized")

    def list_tools(self) -> Sequence[Mapping[str, Any]]:
        collected: list[Mapping[str, Any]] = []
        cursor = ""
        for _ in range(100):
            result = self.request("tools/list", {"cursor": cursor} if cursor else {}, timeout=30.0)
            tools = result.get("tools")
            if isinstance(tools, list):
                collected.extend(item for item in tools if isinstance(item, Mapping))
            next_cursor = result.get("nextCursor")
            if not isinstance(next_cursor, str) or not next_cursor or next_cursor == cursor:
                break
            cursor = next_cursor
        return tuple(collected)

    def call_tool(self, name: str, arguments: Mapping[str, Any], *, timeout: float) -> Mapping[str, Any]:
        return self.request("tools/call", {"name": name, "arguments": dict(arguments)}, timeout=timeout)

    def close(self) -> None:
        return


Adapter = Callable[[Mapping[str, Any]], Any]
