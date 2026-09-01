"""MCP bridge for IDA Free's batch IDAPython runtime.

IDA Free does not provide the ``idalib``/plugin surface used by
``idalib-mcp``. This bridge keeps the MCP contract outside IDA and drives the
installed Free executable through a read-only ``-A -S`` IDAPython session.
"""

from __future__ import annotations

import argparse
import json
import os
import socket
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping
from uuid import uuid4


TOOL_DEFINITIONS: tuple[Mapping[str, Any], ...] = (
    {"name": "idb_open", "description": "Open a binary in an isolated IDA Free session.", "inputSchema": {"type": "object", "required": ["input_path"], "properties": {"input_path": {"type": "string"}, "run_auto_analysis": {"type": "boolean"}, "preferred_session_id": {"type": "string"}}}},
    {"name": "idb_list", "description": "List IDA Free sessions owned by this bridge.", "inputSchema": {"type": "object"}, "annotations": {"readOnlyHint": True}},
    {"name": "idb_close", "description": "Close one IDA Free session.", "inputSchema": {"type": "object", "required": ["database"], "properties": {"database": {"type": "string"}, "save": {"type": "boolean"}}}},
    {"name": "server_health", "description": "Return IDA Free bridge and database health.", "inputSchema": {"type": "object", "properties": {"database": {"type": "string"}}}, "annotations": {"readOnlyHint": True}},
    {"name": "list_funcs", "description": "List discovered functions.", "inputSchema": {"type": "object", "required": ["database"], "properties": {"database": {"type": "string"}, "limit": {"type": "integer"}}}, "annotations": {"readOnlyHint": True}},
    {"name": "imports", "description": "List imported symbols.", "inputSchema": {"type": "object", "required": ["database"], "properties": {"database": {"type": "string"}, "limit": {"type": "integer"}}}, "annotations": {"readOnlyHint": True}},
    {"name": "decompile", "description": "Decompile a function by name or address.", "inputSchema": {"type": "object", "required": ["database", "function"], "properties": {"database": {"type": "string"}, "function": {"type": ["string", "integer"]}}}, "annotations": {"readOnlyHint": True}},
    {"name": "disasm", "description": "Return assembly lines for a function.", "inputSchema": {"type": "object", "required": ["database", "function"], "properties": {"database": {"type": "string"}, "function": {"type": ["string", "integer"]}, "limit": {"type": "integer"}}}, "annotations": {"readOnlyHint": True}},
    {"name": "xrefs_to", "description": "List cross references to a symbol or address.", "inputSchema": {"type": "object", "required": ["database", "target"], "properties": {"database": {"type": "string"}, "target": {"type": ["string", "integer"]}}}, "annotations": {"readOnlyHint": True}},
    {"name": "get_string", "description": "Read a string literal at an address.", "inputSchema": {"type": "object", "required": ["database", "address"], "properties": {"database": {"type": "string"}, "address": {"type": ["string", "integer"]}}}, "annotations": {"readOnlyHint": True}},
    {"name": "get_bytes", "description": "Read bytes at an address.", "inputSchema": {"type": "object", "required": ["database", "address"], "properties": {"database": {"type": "string"}, "address": {"type": ["string", "integer"]}, "size": {"type": "integer"}}}, "annotations": {"readOnlyHint": True}},
    {"name": "get_int", "description": "Read an integer at an address.", "inputSchema": {"type": "object", "required": ["database", "address"], "properties": {"database": {"type": "string"}, "address": {"type": ["string", "integer"]}, "width": {"type": "integer"}}}, "annotations": {"readOnlyHint": True}},
)


@dataclass
class _Session:
    session_id: str
    input_path: str
    process: subprocess.Popen[bytes]
    connection: socket.socket
    lock: threading.Lock = field(default_factory=threading.Lock)


class IdaFreeBridge:
    def __init__(self, ida_executable: Path, script_path: Path | None = None, *, startup_timeout: float = 180.0) -> None:
        self.ida_executable = ida_executable.expanduser().resolve(strict=False)
        self.script_path = (script_path or Path(__file__).with_name("ida_free_agent.py")).resolve(strict=False)
        self.startup_timeout = max(1.0, float(startup_timeout))
        self._validate_executable()
        self.token = uuid4().hex
        self._listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._listener.bind(("127.0.0.1", 0))
        self._listener.listen(16)
        self._listener.settimeout(0.5)
        self._port = int(self._listener.getsockname()[1])
        self._sessions: dict[str, _Session] = {}
        self._pending: dict[str, tuple[threading.Event, dict[str, Any]]] = {}
        self._active_requests: dict[int, str] = {}
        self._lock = threading.RLock()
        self._stdout_lock = threading.Lock()
        self._accept_thread = threading.Thread(target=self._accept_loop, name="ida-free-bridge", daemon=True)
        self._accept_thread.start()

    def _accept_loop(self) -> None:
        while True:
            try:
                connection, _ = self._listener.accept()
            except socket.timeout:
                if self._listener.fileno() < 0:
                    return
                continue
            except OSError:
                return
            threading.Thread(target=self._accept_connection, args=(connection,), daemon=True).start()

    def _accept_connection(self, connection: socket.socket) -> None:
        try:
            connection.settimeout(self.startup_timeout)
            line = self._recv_line(connection)
            hello = json.loads(line.decode("utf-8"))
            if hello.get("token") != self.token or not str(hello.get("session_id") or ""):
                connection.close()
                return
            session_id = str(hello["session_id"])
            with self._lock:
                pending = self._pending.get(session_id)
                if pending is None:
                    connection.close()
                    return
                event, result = pending
                result["connection"] = connection
                event.set()
        except Exception:
            try:
                connection.close()
            except OSError:
                pass

    @staticmethod
    def _recv_line(connection: socket.socket) -> bytes:
        chunks = bytearray()
        while True:
            chunk = connection.recv(1)
            if not chunk:
                raise ConnectionError("ida_free_bridge_eof")
            if chunk == b"\n":
                return bytes(chunks)
            chunks.extend(chunk)
            if len(chunks) > 8 * 1024 * 1024:
                raise ValueError("ida_free_bridge_frame_too_large")

    @staticmethod
    def _send_line(connection: socket.socket, payload: Mapping[str, Any]) -> None:
        connection.sendall((json.dumps(dict(payload), ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8"))

    def _validate_executable(self) -> None:
        if not self.ida_executable.is_file():
            raise FileNotFoundError(f"ida_free_executable_missing:{self.ida_executable}")
        name = self.ida_executable.name.casefold()
        if "setup" in name or "installer" in name or name.startswith("ida-free-pc"):
            raise ValueError("ida_free_installer_not_runtime:configure_ida64_or_idat64")
        if name not in {"ida64.exe", "idat64.exe", "ida.exe", "idat.exe"}:
            raise ValueError("ida_free_runtime_name_unrecognized:expected_ida64_or_idat64")

    def _launch(self, input_path: str, session_id: str) -> _Session:
        self._validate_executable()
        binary = Path(input_path).expanduser().resolve(strict=False)
        if not binary.is_file():
            raise FileNotFoundError(f"ida_free_input_missing:{binary}")
        event = threading.Event()
        result: dict[str, Any] = {}
        with self._lock:
            if session_id in self._sessions or session_id in self._pending:
                raise ValueError(f"ida_free_session_exists:{session_id}")
            self._pending[session_id] = (event, result)
        environment = dict(os.environ)
        environment.update(
            {
                "REDTEAM_IDA_FREE_BRIDGE_HOST": "127.0.0.1",
                "REDTEAM_IDA_FREE_BRIDGE_PORT": str(self._port),
                "REDTEAM_IDA_FREE_BRIDGE_TOKEN": self.token,
                "REDTEAM_IDA_FREE_BRIDGE_SESSION": session_id,
            }
        )
        process = subprocess.Popen(
            [str(self.ida_executable), "-A", f"-S{self.script_path}", str(binary)],
            cwd=str(self.ida_executable.parent),
            env=environment,
            stdin=subprocess.DEVNULL,
            # IDA writes substantial startup diagnostics. The bridge's socket
            # channel is authoritative; discard console streams so a full
            # pipe can never wedge an otherwise healthy analysis session.
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
            start_new_session=os.name != "nt",
        )
        if not event.wait(self.startup_timeout):
            process.kill()
            process.wait(timeout=5)
            with self._lock:
                self._pending.pop(session_id, None)
            raise TimeoutError(f"ida_free_session_start_timeout:{session_id}")
        with self._lock:
            self._pending.pop(session_id, None)
        connection = result.get("connection")
        if not isinstance(connection, socket.socket):
            process.kill()
            process.wait(timeout=5)
            raise RuntimeError(f"ida_free_session_handshake_failed:{session_id}")
        connection.settimeout(self.startup_timeout)
        session = _Session(session_id, str(binary), process, connection)
        with self._lock:
            self._sessions[session_id] = session
        return session

    def _session(self, database: str) -> _Session:
        with self._lock:
            session = self._sessions.get(database)
        if session is None:
            raise KeyError(f"ida_free_database_not_found:{database}")
        if session.process.poll() is not None:
            raise RuntimeError(f"ida_free_session_exited:{database}:{session.process.returncode}")
        return session

    def _call(self, session: _Session, tool: str, arguments: Mapping[str, Any], *, timeout: float) -> Mapping[str, Any]:
        with session.lock:
            session.connection.settimeout(max(0.1, timeout))
            request_id = uuid4().hex
            self._send_line(session.connection, {"id": request_id, "tool": tool, "arguments": dict(arguments)})
            while True:
                response = json.loads(self._recv_line(session.connection).decode("utf-8", errors="replace"))
                if response.get("id") != request_id:
                    continue
                if response.get("error"):
                    raise RuntimeError(str(response["error"]))
                result = response.get("result")
                return result if isinstance(result, Mapping) else {"result": result}

    def call_tool(self, name: str, arguments: Mapping[str, Any], *, timeout: float = 180.0) -> Mapping[str, Any]:
        if name == "idb_open":
            session_id = str(arguments.get("preferred_session_id") or uuid4().hex[:12])
            session = self._launch(str(arguments.get("input_path") or ""), session_id)
            return {"success": True, "session": {"session_id": session.session_id, "input_path": session.input_path, "backend": "ida_free", "owned": True}}
        if name == "idb_list":
            with self._lock:
                sessions = [
                    {"session_id": item.session_id, "input_path": item.input_path, "backend": "ida_free", "owned": True, "is_active": item.process.poll() is None}
                    for item in self._sessions.values()
                ]
            return {"sessions": sessions, "count": len(sessions)}
        if name == "idb_close":
            database = str(arguments.get("database") or "")
            session = self._session(database)
            try:
                self._call(session, "shutdown", {"save": bool(arguments.get("save", True))}, timeout=timeout)
            finally:
                try:
                    session.connection.close()
                except OSError:
                    pass
                if session.process.poll() is None:
                    session.process.terminate()
                    try:
                        session.process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        session.process.kill()
                with self._lock:
                    self._sessions.pop(database, None)
            return {"success": True, "session_id": database, "backend": "ida_free", "owned": True, "saved": bool(arguments.get("save", True))}
        if name == "server_health":
            database = str(arguments.get("database") or "")
            if database:
                session = self._session(database)
                return {"healthy": session.process.poll() is None, "database": database, "backend": "ida_free"}
            with self._lock:
                return {"healthy": True, "backend": "ida_free", "session_count": len(self._sessions)}
        database = str(arguments.get("database") or "")
        if not database:
            raise ValueError("ida_free_database_required")
        session = self._session(database)
        return self._call(session, name, arguments, timeout=timeout)

    def cancel(self, request_id: int) -> bool:
        with self._lock:
            database = self._active_requests.get(request_id)
        if not database:
            return False
        try:
            session = self._session(database)
            self._send_line(session.connection, {"id": uuid4().hex, "tool": "cancel", "arguments": {}})
            return True
        except Exception:
            return False

    def handle(self, payload: Mapping[str, Any]) -> Mapping[str, Any] | None:
        request_id = payload.get("id")
        method = str(payload.get("method") or "")
        if method == "notifications/cancelled":
            params = payload.get("params") if isinstance(payload.get("params"), Mapping) else {}
            try:
                self.cancel(int(params.get("requestId")))
            except (TypeError, ValueError):
                pass
            return None
        if method == "initialize":
            return {"jsonrpc": "2.0", "id": request_id, "result": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {"listChanged": False}}, "serverInfo": {"name": "ida-free-mcp-bridge", "version": "1"}}}
        if method == "notifications/initialized":
            return None
        if method == "tools/list":
            return {"jsonrpc": "2.0", "id": request_id, "result": {"tools": list(TOOL_DEFINITIONS)}}
        if method != "tools/call":
            return {"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method_not_found"}}
        params = payload.get("params") if isinstance(payload.get("params"), Mapping) else {}
        name = str(params.get("name") or "")
        arguments = params.get("arguments") if isinstance(params.get("arguments"), Mapping) else {}
        try:
            numeric_request_id = int(request_id)
        except (TypeError, ValueError):
            numeric_request_id = None
        if numeric_request_id is not None:
            with self._lock:
                self._active_requests[numeric_request_id] = str(arguments.get("database") or "")
        try:
            result = self.call_tool(name, arguments)
            return {"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": json.dumps(result, ensure_ascii=False)}], "structuredContent": result}}
        except Exception as exc:
            return {"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": str(exc)}], "isError": True}}
        finally:
            if numeric_request_id is not None:
                with self._lock:
                    self._active_requests.pop(numeric_request_id, None)

    def serve(self) -> int:
        try:
            for line in sys.stdin:
                if not line.strip():
                    continue
                payload = json.loads(line)
                if not isinstance(payload, Mapping):
                    continue
                if payload.get("method") == "tools/call":
                    threading.Thread(target=self._serve_call, args=(payload,), daemon=True).start()
                    continue
                response = self.handle(payload)
                self._write_response(response)
        finally:
            self.close()
        return 0

    def _serve_call(self, payload: Mapping[str, Any]) -> None:
        self._write_response(self.handle(payload))

    def _write_response(self, response: Mapping[str, Any] | None) -> None:
        if response is None:
            return
        with self._stdout_lock:
            sys.stdout.write(json.dumps(response, ensure_ascii=False, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    def close(self) -> None:
        with self._lock:
            sessions = tuple(self._sessions.values())
            self._sessions.clear()
        for session in sessions:
            try:
                self._call(session, "shutdown", {"save": True}, timeout=5.0)
            except Exception:
                pass
            try:
                session.connection.close()
            except OSError:
                pass
            if session.process.poll() is None:
                session.process.terminate()
                try:
                    session.process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    session.process.kill()
        try:
            self._listener.close()
        except OSError:
            pass


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="MCP bridge for IDA Free batch IDAPython")
    parser.add_argument("--ida", required=True, type=Path, help="Path to ida64.exe or idat64.exe")
    parser.add_argument("--script", type=Path, default=None, help="Optional IDAPython bridge script")
    parser.add_argument("--startup-timeout", type=float, default=180.0)
    args = parser.parse_args(argv)
    return IdaFreeBridge(args.ida, args.script, startup_timeout=args.startup_timeout).serve()


if __name__ == "__main__":
    raise SystemExit(main())
