from __future__ import annotations

import argparse
import json
import os
import signal
import sys
from pathlib import Path
from typing import TYPE_CHECKING, Any, Mapping

from .mcp_config import MAX_REQUEST_BYTES

if TYPE_CHECKING:
    from .mcp_server import RuntimeMcpServer


from .settings import _default_config_paths, _runtime_settings


def _iter_request_lines(
    stream: Any,
    *,
    max_bytes: int = MAX_REQUEST_BYTES,
):
    """Yield bounded UTF-8 JSON lines without allocating an unbounded record."""

    reader = getattr(stream, "buffer", stream)
    limit = max(1, int(max_bytes))
    while True:
        chunk = reader.readline(limit + 1)
        if not chunk:
            return
        encoded = chunk.encode("utf-8") if isinstance(chunk, str) else bytes(chunk)
        too_large = len(encoded) > limit
        if too_large:
            has_newline = encoded.endswith((b"\n", b"\r"))
            while not has_newline:
                remainder = reader.readline(limit + 1)
                if not remainder:
                    break
                raw_remainder = (
                    remainder.encode("utf-8")
                    if isinstance(remainder, str)
                    else bytes(remainder)
                )
                has_newline = raw_remainder.endswith((b"\n", b"\r"))
            yield None, True
            continue
        if isinstance(chunk, str):
            yield chunk, False
            continue
        try:
            yield encoded.decode("utf-8"), False
        except UnicodeDecodeError:
            yield "\ufffd", False


def _serve_stdio(
    server: RuntimeMcpServer,
    stdin: Any,
    stdout: Any,
    *,
    max_request_bytes: int = MAX_REQUEST_BYTES,
) -> None:
    for line, too_large in _iter_request_lines(stdin, max_bytes=max_request_bytes):
        if too_large:
            response = server._error(None, -32600, "invalid_request:request_too_large")
        else:
            assert line is not None
            stripped = line.strip()
            if not stripped:
                continue
            try:
                payload = json.loads(stripped)
            except json.JSONDecodeError:
                response = {
                    "jsonrpc": "2.0",
                    "id": None,
                    "error": {"code": -32700, "message": "parse_error"},
                }
            else:
                response = (
                    server.handle(payload)
                    if isinstance(payload, Mapping)
                    else server._error(None, -32600, "invalid_request")
                )
        if response is not None:
            stdout.write(json.dumps(response, ensure_ascii=False, separators=(",", ":")) + "\n")
            stdout.flush()


def main(argv: list[str] | None = None) -> int:
    # Keep the transport importable from ``runtime.mcp_server`` when that
    # module is executed with ``python -m``.  Importing the server class at
    # module load time creates a second ``runtime.mcp_server`` identity beside
    # ``__main__`` and leaves both modules partially initialized.
    from .mcp_server import RuntimeMcpServer

    parser = argparse.ArgumentParser(prog="trace-mcp", description="Trace durable Agent MCP runtime")
    parser.add_argument("--root", default="", help="Durable state root")
    parser.add_argument("--config", action="append", default=[], help="MCP config.toml path")
    parser.add_argument("--model", default="")
    parser.add_argument("--api-base-url", default="")
    parser.add_argument("--api-key-env", default="")
    parser.add_argument("--api-timeout-seconds", type=float)
    parser.add_argument("--model-context-tokens", type=int)
    arguments = parser.parse_args(argv)
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or (Path.home() / ".redteam-agent")).expanduser().resolve(strict=False)
    root = Path(arguments.root or os.environ.get("TRACE_HOME") or (agent_home / "operations")).expanduser().resolve(strict=False)
    from ..application.agent_service import AgentService

    service = AgentService(root=root, config_paths=arguments.config, provider_options={
        "model": arguments.model, "base_url": arguments.api_base_url,
        "api_key_env": arguments.api_key_env, "timeout_seconds": arguments.api_timeout_seconds,
        "max_context_tokens": arguments.model_context_tokens,
    })
    runtime, settings = service.runtime, service.runtime_settings
    server = RuntimeMcpServer(
        runtime,
        service=service,
        default_max_actions=settings["max_actions_per_cycle"],
        default_max_retries_per_action=settings["max_retries_per_action"],
        handoff_ttl_seconds=settings["handoff_ttl_seconds"],
    )
    previous_sigterm = signal.signal(signal.SIGTERM, signal.default_int_handler)
    try:
        _serve_stdio(server, sys.stdin, sys.stdout)
    except KeyboardInterrupt:
        pass
    finally:
        signal.signal(signal.SIGTERM, previous_sigterm)
        service.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
