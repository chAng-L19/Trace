from __future__ import annotations

import math
import os
import sys
import tomllib
from pathlib import Path
from typing import Any, Mapping

from .handoff import DEFAULT_HANDOFF_TTL_SECONDS


def _default_config_paths(explicit: list[str]) -> list[Path]:
    paths = [Path(item).expanduser().resolve(strict=False) for item in explicit]
    configured = os.environ.get("REDTEAM_AGENT_CONFIG", "").strip()
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or (Path.home() / ".redteam-agent"))
    default = Path(configured).expanduser().resolve(strict=False) if configured else agent_home.expanduser().resolve(strict=False) / "config.toml"
    if default not in paths:
        paths.append(default)
    return paths


def _settings_warning(path: Path, key: str, value: Any, reason: str) -> None:
    sys.stderr.write(f"trace-agent-runtime: ignored automation.{key}: {reason} (type={type(value).__name__})\n")


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


def _runtime_settings(paths: list[Path], *, environ: Mapping[str, str] | None = None) -> dict[str, Any]:
    settings: dict[str, Any] = {
        "tool_priority": (),
        "max_actions_per_cycle": 64,
        "action_timeout_seconds": None,
        "max_retries_per_action": 2,
        "max_domains": 7,
        "max_hypothesis_branches": 4,
        "handoff_ttl_seconds": DEFAULT_HANDOFF_TTL_SECONDS,
    }
    sources = {key: "default" for key in settings}
    supplied = set()
    for path in paths:
        if not path.is_file():
            continue
        try:
            payload = tomllib.loads(path.read_text(encoding="utf-8-sig"))
        except (OSError, tomllib.TOMLDecodeError) as exc:
            sys.stderr.write(f"trace-agent-runtime: skipped invalid config ({type(exc).__name__})\n")
            continue
        automation = payload.get("automation") if isinstance(payload.get("automation"), Mapping) else {}
        automation = {key: value for key, value in automation.items() if key not in supplied}
        supplied.update(automation)
        sources.update({key: f"toml:{path}" for key in automation if key in settings})
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
    env = os.environ if environ is None else environ
    for key, current in tuple(settings.items()):
        name = "TRACE_" + key.upper()
        raw = env.get(name)
        if raw is None or key == "tool_priority":
            continue
        limits = {"max_retries_per_action": (0, 8), "max_domains": (1, 7), "max_hypothesis_branches": (1, 8)}
        minimum, maximum = limits.get(key, (1, 512))
        value = (_bounded_float_setting({key: raw}, key, current,
                    minimum=1.0 if key == "handoff_ttl_seconds" else 0.1, maximum=86400, path=Path("environment"))
                 if key.endswith("seconds") else _bounded_int_setting(
                    {key: raw}, key, current, minimum=minimum, maximum=maximum, path=Path("environment")))
        settings[key] = value
        sources[key] = f"env:{name}"
    settings["sources"] = sources
    return settings
