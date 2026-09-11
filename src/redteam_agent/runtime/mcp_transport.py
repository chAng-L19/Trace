from __future__ import annotations

import argparse
import json
import math
import os
import sys
import tomllib
from pathlib import Path
from typing import TYPE_CHECKING, Any, Mapping

from .adaptive_planner import AdaptivePlanner
from .handoff import DEFAULT_HANDOFF_TTL_SECONDS
from .mcp_config import MAX_REQUEST_BYTES
from .operation_runtime import OperationRuntime
from .tool_broker import ToolBroker

if TYPE_CHECKING:
    from .mcp_server import RuntimeMcpServer


def _default_config_paths(explicit: list[str]) -> list[Path]:
    paths = [Path(item).expanduser().resolve(strict=False) for item in explicit]
    configured = os.environ.get("REDTEAM_AGENT_CONFIG", "").strip()
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or (Path.home() / ".redteam-agent"))
    default = Path(configured).expanduser().resolve(strict=False) if configured else agent_home.expanduser().resolve(strict=False) / "config.toml"
    if default not in paths:
        paths.append(default)
    return paths


def _settings_warning(path: Path, key: str, value: Any, reason: str) -> None:
    rendered = repr(value)
    if len(rendered) > 120:
        rendered = f"{rendered[:117]}..."
    sys.stderr.write(
        f"redteam-agent-runtime: ignored automation.{key} from {path}: "
        f"{reason} (value={rendered})\n"
    )


def _bounded_int_setting(
    automation: Mapping[str, Any],
    key: str,
    default: int,
    *,
    minimum: int,
    maximum: int,
    path: Path,
) -> int:
    if key not in automation:
        return default
    value = automation.get(key)
    try:
        if isinstance(value, bool):
            raise ValueError
        if isinstance(value, float):
            if not math.isfinite(value) or not value.is_integer():
                raise ValueError
        parsed = int(value)
    except (TypeError, ValueError, OverflowError):
        _settings_warning(path, key, value, "expected a finite integer; using default")
        return default
    bounded = max(minimum, min(maximum, parsed))
    if bounded != parsed:
        _settings_warning(
            path,
            key,
            value,
            f"outside [{minimum}, {maximum}]; clamped to {bounded}",
        )
    return bounded


def _bounded_float_setting(
    automation: Mapping[str, Any],
    key: str,
    default: float | None,
    *,
    minimum: float,
    maximum: float,
    path: Path,
) -> float | None:
    if key not in automation:
        return default
    value = automation.get(key)
    try:
        if isinstance(value, bool):
            raise ValueError
        parsed = float(value)
        if not math.isfinite(parsed):
            raise ValueError
    except (TypeError, ValueError, OverflowError):
        _settings_warning(path, key, value, "expected a finite number; using default")
        return default
    bounded = max(minimum, min(maximum, parsed))
    if bounded != parsed:
        _settings_warning(
            path,
            key,
            value,
            f"outside [{minimum}, {maximum}]; clamped to {bounded}",
        )
    return bounded


def _runtime_settings(paths: list[Path]) -> dict[str, Any]:
    settings: dict[str, Any] = {
        "tool_priority": (),
        "max_actions_per_cycle": 64,
        "action_timeout_seconds": None,
        "max_retries_per_action": 2,
        "max_domains": 7,
        "max_hypothesis_branches": 4,
        "handoff_ttl_seconds": DEFAULT_HANDOFF_TTL_SECONDS,
    }
    for path in paths:
        if not path.is_file():
            continue
        try:
            payload = tomllib.loads(path.read_text(encoding="utf-8-sig"))
        except (OSError, tomllib.TOMLDecodeError) as exc:
            sys.stderr.write(f"redteam-agent-runtime: skipped invalid config {path}: {exc}\n")
            continue
        automation = payload.get("automation") if isinstance(payload.get("automation"), Mapping) else {}
        raw_priority = automation.get("tool_priority")
        if isinstance(raw_priority, list):
            settings["tool_priority"] = tuple(str(item) for item in raw_priority if str(item).strip())
        elif raw_priority is not None:
            _settings_warning(path, "tool_priority", raw_priority, "expected an array; using default")
        settings["max_actions_per_cycle"] = _bounded_int_setting(
            automation,
            "max_actions_per_cycle",
            settings["max_actions_per_cycle"],
            minimum=1,
            maximum=512,
            path=path,
        )
        settings["action_timeout_seconds"] = _bounded_float_setting(
            automation,
            "action_timeout_seconds",
            settings["action_timeout_seconds"],
            minimum=0.1,
            maximum=86_400.0,
            path=path,
        )
        settings["max_retries_per_action"] = _bounded_int_setting(
            automation,
            "max_retries_per_action",
            settings["max_retries_per_action"],
            minimum=0,
            maximum=8,
            path=path,
        )
        settings["max_domains"] = _bounded_int_setting(
            automation,
            "max_domains",
            settings["max_domains"],
            minimum=1,
            maximum=7,
            path=path,
        )
        settings["max_hypothesis_branches"] = _bounded_int_setting(
            automation,
            "max_hypothesis_branches",
            settings["max_hypothesis_branches"],
            minimum=1,
            maximum=8,
            path=path,
        )
        settings["handoff_ttl_seconds"] = _bounded_float_setting(
            automation,
            "handoff_ttl_seconds",
            settings["handoff_ttl_seconds"],
            minimum=1.0,
            maximum=86_400.0,
            path=path,
        )
        break
    return settings


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

    parser = argparse.ArgumentParser(description="Host-independent durable red-team Agent MCP runtime")
    parser.add_argument("--root", default="", help="Durable state root")
    parser.add_argument("--config", action="append", default=[], help="Codex config.toml path")
    arguments = parser.parse_args(argv)
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or (Path.home() / ".redteam-agent")).expanduser().resolve(strict=False)
    root = Path(arguments.root).expanduser().resolve(strict=False) if arguments.root else agent_home / "operations"
    config_paths = _default_config_paths(arguments.config)
    settings = _runtime_settings(config_paths)
    broker = ToolBroker(tool_priority=settings["tool_priority"])
    broker.discover_from_configs(config_paths)
    runtime = OperationRuntime(
        root=root,
        broker=broker,
        action_timeout_cap=settings["action_timeout_seconds"],
        planner=AdaptivePlanner(
            max_domains=settings["max_domains"],
            max_hypothesis_branches=settings["max_hypothesis_branches"],
        ),
    )
    from ..application.agent_service import AgentService

    service = AgentService(runtime=runtime)
    server = RuntimeMcpServer(
        runtime,
        service=service,
        default_max_actions=settings["max_actions_per_cycle"],
        default_max_retries_per_action=settings["max_retries_per_action"],
        handoff_ttl_seconds=settings["handoff_ttl_seconds"],
    )
    try:
        _serve_stdio(server, sys.stdin, sys.stdout)
    finally:
        service.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
