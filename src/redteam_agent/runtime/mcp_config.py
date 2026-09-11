from __future__ import annotations

import fnmatch
import hashlib
import json
import math
import os
import re
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Mapping, Sequence


MAX_REQUEST_BYTES = 8 * 1024 * 1024
MAX_TOOL_ARGUMENT_BYTES = 4 * 1024 * 1024
MAX_OBSERVATION_BYTES = 2 * 1024 * 1024


_ENV_PATTERN = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")


@dataclass(frozen=True, slots=True)
class McpServerSpec:
    name: str
    transport: str
    enabled: bool = True
    preset: str = ""
    scope: str = "shared"
    command: str = ""
    args: tuple[str, ...] = ()
    env: Mapping[str, str] = field(default_factory=dict)
    cwd: Path | None = None
    url: str = ""
    headers: Mapping[str, str] = field(default_factory=dict)
    startup_timeout_seconds: float = 30.0
    tool_timeout_seconds: float = 60.0
    include_tools: tuple[str, ...] = ()
    exclude_tools: tuple[str, ...] = ()
    read_only_tools: tuple[str, ...] = ()
    write_tools: tuple[str, ...] = ()
    read_only: bool = False

    @property
    def signature(self) -> str:
        if self.transport == "stdio":
            payload: Mapping[str, Any] = {
                "transport": self.transport,
                "command": self.command,
                "args": list(self.args),
                "env": dict(sorted(self.env.items())),
                "cwd": str(self.cwd or ""),
            }
        else:
            payload = {
                "transport": self.transport,
                "url": self.url,
                "headers": dict(sorted(self.headers.items())),
            }
        encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()

    def accepts_tool(self, name: str) -> bool:
        if self.include_tools and not _matches(name, self.include_tools):
            return False
        return not _matches(name, self.exclude_tools)

    def tool_is_read_only(self, name: str, annotations: Mapping[str, Any] | None = None) -> bool:
        if _matches(name, self.write_tools):
            return False
        if self.read_only or _matches(name, self.read_only_tools):
            return True
        hints = annotations or {}
        return hints.get("readOnlyHint") is True

    def render(self, *, run_id: str = "", workspace: Path | None = None) -> "McpServerSpec":
        values = {
            "run_id": run_id,
            "workspace": str(workspace or ""),
        }

        def render_text(value: str) -> str:
            text = _ENV_PATTERN.sub(lambda match: os.environ.get(match.group(1), ""), str(value))
            for key, replacement in values.items():
                text = text.replace("{" + key + "}", replacement)
            return text

        return replace(
            self,
            command=render_text(self.command),
            args=tuple(render_text(item) for item in self.args),
            env={key: render_text(value) for key, value in self.env.items()},
            cwd=Path(render_text(str(self.cwd))) if self.cwd is not None else None,
            url=render_text(self.url),
            headers={key: render_text(value) for key, value in self.headers.items()},
        )


PRESET_DEFAULTS: dict[str, Mapping[str, Any]] = {
    "playwright": {
        "scope": "run",
        "startup_timeout_seconds": 90.0,
        "tool_timeout_seconds": 120.0,
        "include_tools": (
            "browser_navigate*",
            "browser_snapshot",
            "browser_find",
            "browser_click",
            "browser_type",
            "browser_fill_form",
            "browser_select_option",
            "browser_press_key",
            "browser_wait_for",
            "browser_tabs",
            "browser_network*",
            "browser_console*",
            "browser_take_screenshot",
            "browser_evaluate",
            "browser_close",
        ),
        "read_only_tools": (
            "browser_snapshot",
            "browser_find",
            "browser_network*",
            "browser_console*",
            "browser_take_screenshot",
        ),
    },
}


PLAYWRIGHT_CAPABILITIES: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("browser_navigate*", ("browser_automation", "page_fetch")),
    ("browser_snapshot", ("browser_automation", "dom_snapshot", "page_fetch")),
    ("browser_find", ("browser_automation", "dom_snapshot")),
    ("browser_network*", ("browser_automation", "http_fingerprint", "page_fetch")),
    ("browser_take_screenshot", ("browser_automation", "screenshot")),
    ("browser_console*", ("browser_automation", "telemetry_analysis")),
    ("browser_*", ("browser_automation",)),
)


def profile_capabilities(preset: str, tool_name: str) -> tuple[str, ...]:
    rules = PLAYWRIGHT_CAPABILITIES if preset.casefold() == "playwright" else ()
    capabilities: list[str] = []
    for pattern, offered in rules:
        if fnmatch.fnmatchcase(tool_name.casefold(), pattern.casefold()):
            capabilities.extend(offered)
    return tuple(dict.fromkeys(capabilities))


def parse_mcp_server_specs(path: Path, payload: Mapping[str, Any]) -> tuple[McpServerSpec, ...]:
    servers = payload.get("mcp_servers") or payload.get("mcpServers") or {}
    if not isinstance(servers, Mapping):
        return ()
    specs: list[McpServerSpec] = []
    for raw_name, raw in servers.items():
        if not isinstance(raw, Mapping):
            continue
        name = str(raw_name).strip()
        if not name:
            continue
        preset = str(raw.get("preset") or "").strip().casefold()
        defaults = PRESET_DEFAULTS.get(preset, {})
        command = str(raw.get("command") or "").strip()
        url = str(raw.get("url") or raw.get("http_url") or "").strip()
        transport = str(raw.get("transport") or raw.get("type") or ("stdio" if command else "http" if url else "")).casefold()
        if transport == "local":
            transport = "stdio"
        if transport == "remote":
            transport = "http"
        args = _strings(raw.get("args"))
        env = _string_mapping(raw.get("env") or raw.get("environment"))
        headers = _string_mapping(raw.get("headers"))
        token_env = str(raw.get("bearer_token_env_var") or "").strip()
        if token_env and os.environ.get(token_env):
            headers.setdefault("Authorization", f"Bearer {os.environ[token_env]}")
        raw_cwd = str(raw.get("cwd") or "").strip()
        cwd = None
        if raw_cwd:
            candidate = Path(raw_cwd).expanduser()
            cwd = candidate if candidate.is_absolute() else (path.parent / candidate).resolve(strict=False)
        scope = str(raw.get("scope") or defaults.get("scope") or "shared").strip().casefold()
        if scope not in {"shared", "run"}:
            scope = "shared"
        specs.append(
            McpServerSpec(
                name=name,
                transport=transport,
                enabled=raw.get("enabled") is not False and raw.get("disabled") is not True,
                preset=preset,
                scope=scope,
                command=command,
                args=args,
                env=env,
                cwd=cwd,
                url=url,
                headers=headers,
                startup_timeout_seconds=_positive_float(
                    raw.get("startup_timeout_seconds") or raw.get("startup_timeout"),
                    float(defaults.get("startup_timeout_seconds") or 30.0),
                ),
                tool_timeout_seconds=_positive_float(
                    raw.get("tool_timeout_seconds") or raw.get("tool_timeout"),
                    float(defaults.get("tool_timeout_seconds") or 60.0),
                ),
                include_tools=_strings(
                    raw.get("include_tools")
                    or raw.get("tools")
                    or defaults.get("include_tools")
                ),
                exclude_tools=_strings(raw.get("exclude_tools")),
                read_only_tools=_strings(
                    raw.get("read_only_tools") or defaults.get("read_only_tools")
                ),
                write_tools=_strings(raw.get("write_tools")),
                read_only=bool(raw.get("read_only", False)),
            )
        )
    return tuple(specs)


def _matches(name: str, patterns: Sequence[str]) -> bool:
    return any(fnmatch.fnmatchcase(name.casefold(), pattern.casefold()) for pattern in patterns)


def _strings(value: Any) -> tuple[str, ...]:
    if isinstance(value, str):
        return (value,) if value.strip() else ()
    if not isinstance(value, Sequence) or isinstance(value, (bytes, bytearray)):
        return ()
    return tuple(dict.fromkeys(str(item).strip() for item in value if str(item).strip()))


def _string_mapping(value: Any) -> dict[str, str]:
    if not isinstance(value, Mapping):
        return {}
    return {str(key): str(item) for key, item in value.items()}


def _positive_float(value: Any, default: float) -> float:
    try:
        parsed = float(value) if value is not None else default
    except (TypeError, ValueError, OverflowError):
        return default
    return parsed if math.isfinite(parsed) and parsed > 0 else default


__all__ = [
    "McpServerSpec",
    "parse_mcp_server_specs",
    "profile_capabilities",
]
