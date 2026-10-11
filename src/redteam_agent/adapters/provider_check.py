from __future__ import annotations

"""One explicit model request, without an agent loop or configuration writes."""

import os
import time
from typing import Any, Mapping
from uuid import uuid4

from ..application.bootstrap import resolve_provider
from ..core import ModelRequest
from ..runtime.security import safe_error_text


def test_provider(control: Any, payload: Mapping[str, Any]) -> dict[str, Any]:
    """Overlay saved provider fields by provider_id (or id), then call complete once.

    No id means a new, unsaved configuration. Blank api_key reuses the saved
    credential only for the same endpoint, protocol and api_key_env. Endpoint
    or protocol changes require api_key or a different explicit api_key_env.
    clear_api_key suppresses credential reuse; an explicit nonempty key wins.
    Global TRACE_* settings, default key environments and TOML are not loaded.
    Returns ok/reply/model/latency_ms/error, including on validation/HTTP failure.
    """
    started = time.perf_counter()
    result = {"ok": False, "reply": "", "model": "", "latency_ms": 0.0, "error": ""}
    secrets = {str(payload.get("api_key") or "")}
    try:
        provider_id = str(payload.get("provider_id") or payload.get("id") or "").strip()
        with control._lock:
            saved = control.provider(provider_id) if provider_id else {}
            stored_key = control.provider_secret(provider_id, include_environment=False) if saved else ""
            saved_env = str(saved.get("api_key_env") or "")
            secrets.update((stored_key, os.environ.get(saved_env, "")))
        options = {**saved, **payload}
        kind = str(options.get("provider") or "openai-compatible").strip().lower()
        default_url = "https://api.anthropic.com/v1" if kind == "anthropic" else "https://api.openai.com/v1"
        base_url = str(options.get("base_url") or default_url).strip().rstrip("/")
        model = str(options.get("model") or "").strip()
        result["model"] = model
        if not model:
            raise ValueError("provider_model_required")
        key_env = str(options.get("api_key_env") or "").strip()
        same_endpoint = bool(saved and saved["base_url"] == base_url and saved["provider"] == kind)
        secret = str(payload.get("api_key") or "")
        if not secret and not payload.get("clear_api_key"):
            if key_env and (same_endpoint or not saved or key_env != saved_env):
                secret = os.environ.get(key_env, "")
            if not secret and same_endpoint and key_env == saved_env:
                secret = stored_key
        secrets.add(secret)
        options.update(provider=kind, base_url=base_url, model=model)
        provider, _ = resolve_provider(None, (), options, environ={}, credential_override=secret)
        request = ModelRequest(
            request_id=uuid4().hex, run_id="provider-check", model=model,
            messages=({"role": "user", "content": "Reply with OK."},),
        )
        response = provider.complete(request)
        result.update(reply=response.text, model=response.model or model,
                      ok=response.status == "completed" and not response.error and not response.tool_calls,
                      error=response.error or ("provider_test_tool_calls" if response.tool_calls else
                            "" if response.status == "completed" else f"provider_test_{response.status}"))
    except Exception as exc:
        result["error"] = str(exc) or type(exc).__name__
    # Providers can echo credentials in successful text/model as well as errors.
    for field in ("reply", "model", "error"):
        for secret in sorted(filter(None, secrets), key=len, reverse=True):
            result[field] = result[field].replace(secret, "[REDACTED]")
    result["error"] = safe_error_text(result["error"])
    result["latency_ms"] = round((time.perf_counter() - started) * 1000, 3)
    return result
