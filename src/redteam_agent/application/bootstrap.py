from __future__ import annotations

"""Shared process assembly. Explicit options > environment > durable > TOML > defaults."""

import os
import sqlite3
import tomllib
from contextlib import closing
from pathlib import Path
from typing import Any, Mapping, Sequence

from ..providers import OpenAICompatibleProvider
from ..runtime.adaptive_planner import AdaptivePlanner
from ..runtime.operation_runtime import OperationRuntime
from ..runtime.settings import _default_config_paths, _runtime_settings
from ..runtime.tool_broker import ToolBroker

PROVIDER_ENV = {
    "model": "TRACE_MODEL", "base_url": "TRACE_API_BASE_URL",
    "api_key_env": "TRACE_API_KEY_ENV", "timeout_seconds": "TRACE_API_TIMEOUT_SECONDS",
    "max_context_tokens": "TRACE_MODEL_CONTEXT_TOKENS",
}
PROVIDER_DEFAULTS = {
    "model": "", "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY",
    "timeout_seconds": 120.0, "max_context_tokens": 128000,
}


def config_paths(paths: Sequence[Path | str] | None = None) -> list[Path]:
    return _default_config_paths([str(path) for path in (paths or ())])


def build_runtime(root: Path, paths: Sequence[Path], *,
                  environ: Mapping[str, str] | None = None) -> tuple[OperationRuntime, dict[str, Any]]:
    settings = _runtime_settings(list(paths), environ=environ)
    broker = ToolBroker(tool_priority=settings["tool_priority"])
    runtime = OperationRuntime(
        root=root, broker=broker, action_timeout_cap=settings["action_timeout_seconds"],
        planner=AdaptivePlanner(max_domains=settings["max_domains"],
                                max_hypothesis_branches=settings["max_hypothesis_branches"]),
    )
    return runtime, settings


def resolve_provider(control: Any, paths: Sequence[Path], options: Mapping[str, Any] | None = None,
                     environ: Mapping[str, str] | None = None,
                     active: Mapping[str, Any] | None = None) -> tuple[Any, dict[str, str]]:
    values, sources = dict(PROVIDER_DEFAULTS), {key: "default" for key in PROVIDER_DEFAULTS}
    for path in reversed(paths):
        try:
            payload = tomllib.loads(path.read_text(encoding="utf-8-sig"))
        except (OSError, tomllib.TOMLDecodeError):
            continue
        provider = payload.get("provider", {})
        if isinstance(provider, Mapping):
            for key in values.keys() & provider.keys():
                values[key], sources[key] = provider[key], f"toml:{path}"
    if control:
        active = next((item for item in control.providers() if item["active"] and item["enabled"]), None)
    if active:
        for key in values:
            values[key], sources[key] = active[key], "persisted:active_provider"
    env = os.environ if environ is None else environ
    for key, name in PROVIDER_ENV.items():
        if env.get(name):
            values[key], sources[key] = env[name], f"env:{name}"
    for key, value in (options or {}).items():
        if key in values and value not in (None, ""):
            values[key], sources[key] = value, "explicit"
    if not str(values["model"]).strip():
        return None, sources
    # An endpoint override must never forward a persisted key to another host.
    secret = control.provider_secret(active["provider_id"], include_environment=False) if control and active and values["base_url"] == active["base_url"] else ""
    key_name = str(values["api_key_env"])
    try:
        timeout, context = float(values["timeout_seconds"]), int(values["max_context_tokens"])
    except (TypeError, ValueError, OverflowError):
        raise ValueError("provider_limits_invalid") from None
    return OpenAICompatibleProvider(
        str(values["base_url"]), str(values["model"]), str(secret), api_key_env=key_name, environ=env,
        timeout_seconds=timeout, max_context_tokens=context,
    ), sources


def reload_mcp(service: Any) -> dict[str, Any]:
    control = service.control
    path = control.write_mcp_config()
    service.runtime.broker.set_secret_bindings(control.mcp_secret_bindings())
    # First definition wins: durable management overrides file defaults.
    service.runtime.broker.register_config_paths((path, *service.config_paths))
    service.runtime.broker.refresh(force=True)
    return {"mcp": service.runtime.broker.server_statuses(),
            "tool_count": len(service.runtime.broker.descriptors())}


def configure_service(service: Any, *, model_port: Any = None, model_name: str = "",
                      streaming: bool = False, options: Mapping[str, Any] | None = None,
                      max_retries: int = 2, max_turns: int = 8,
                      load_external_configuration: bool = True) -> None:
    from ..adapters.web_control import ControlPlane

    service.control = ControlPlane(service.runtime.store, service.runtime.root)
    if model_port is not None:
        provider, sources = model_port, {"provider": "explicit:model_port"}
    elif load_external_configuration:
        provider, sources = resolve_provider(
            service.control, service.config_paths, {"model": model_name, **(options or {})},
        )
    else:
        provider, sources = None, {"provider": "disabled:external_configuration"}
    service.configuration_projection = {
        "precedence": ["explicit", "environment", "persisted", "toml", "default"],
        "provider_sources": sources, "automation": service.runtime_settings,
        "config_paths": [str(path) for path in service.config_paths],
        "mcp_precedence": ["persisted", "explicit_config", "environment_config", "default_config"],
    }
    service.mcp_restore_error = ""
    if load_external_configuration:
        try:
            reload_mcp(service)
        except Exception as exc:
            service.mcp_restore_error = f"mcp_restore_failed:{type(exc).__name__}"
    if provider is not None:
        service.configure_model(provider, model_name=model_name or provider.capabilities().metadata.get("model", ""),
                                streaming=streaming if model_port is not None else provider.capabilities().streaming,
                                max_retries=max_retries, max_turns=max_turns)


def configuration_projection(root: Path, paths: Sequence[Path | str] | None = None) -> dict[str, Any]:
    """Read-only doctor projection: no migrations, discovery, writes or network calls."""
    resolved_paths = config_paths(paths)
    active = None
    database = root.expanduser().resolve() / "runtime.sqlite3"
    if database.is_file():
        try:
            with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as connection:
                connection.row_factory = sqlite3.Row
                row = connection.execute("SELECT * FROM trace_providers WHERE active=1 AND enabled=1 LIMIT 1").fetchone()
                active = dict(row) if row else None
        except sqlite3.Error:
            pass
    provider, sources = resolve_provider(None, resolved_paths, active=active)
    return {
        "precedence": ["explicit", "environment", "persisted", "toml", "default"],
        "provider_sources": sources, "automation": _runtime_settings(resolved_paths),
        "config_paths": [str(path) for path in resolved_paths],
        "provider": {"configured": provider is not None, "ready": bool(provider and provider.ready),
                     "model": provider.model if provider else "",
                     "reason": "" if provider and provider.ready else ("missing_credentials" if provider else "provider_not_configured")},
    }
