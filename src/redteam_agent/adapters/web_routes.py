from __future__ import annotations

import json
import logging
import os
import platform
from typing import Any, Mapping
from uuid import uuid4

from ..application.contracts import CANONICAL_RUN_STATUSES
from ..runtime.security import safe_error_text, SENSITIVE_KEY_RE
from ..application.bootstrap import resolve_provider, reload_mcp


class ControlRoutesMixin:
    """Management routes kept separate so the HTTP adapter stays small."""

    def _internal_error(self, exc: Exception):
        from .web import WebResponse, WEB_SCHEMA_VERSION

        correlation_id = uuid4().hex
        reason = str(exc)
        secrets = [value for name, value in os.environ.items() if SENSITIVE_KEY_RE.search(name)]
        secrets.extend(self.control._secrets.values())
        secrets.extend(self.control._mcp_secret_values.values())
        loop = self.service.model_loop
        if loop is not None:
            secrets.append(str(getattr(loop.model, "_api_key", "")))
        for secret in sorted((str(value) for value in secrets if value), key=len, reverse=True):
            reason = reason.replace(secret, "[REDACTED]")
        reason = safe_error_text(reason).replace("\r", " ").replace("\n", " ")
        logging.getLogger(__name__).error("request_failed id=%s type=%s reason=%s", correlation_id, type(exc).__name__, reason)
        return WebResponse.json({"ok": False, "schema_version": WEB_SCHEMA_VERSION,
            "error": f"internal_error:{type(exc).__name__}", "correlation_id": correlation_id},
            status=500, headers={"X-Trace-Error-ID": correlation_id})

    def _implicit_command_id(
        self,
        route: list[str] | tuple[str, ...],
        body: Mapping[str, Any],
        *,
        run_id: str = "",
    ) -> str:
        """Assign a receipt identity; retries must echo the returned command ID.

        Identical bodies can be distinct operator commands (e.g. A→B→A), so
        request contents or the run state version cannot identify intent.
        """
        return "compat-" + uuid4().hex

    def _activate_model(self, saved) -> None:
        provider, sources = resolve_provider(self.control, self.service.config_paths, saved)
        if not saved["enabled"]:
            provider = None
        self.service.configure_model(provider, model_name=saved["model"], streaming=bool(provider and provider.capabilities().streaming))
        self.service.configuration_projection["provider_sources"] = sources

    def _safe_control(self, method: str, domain: str, tail: list[str], body: Mapping[str, Any]):
        try:
            return self._control_dispatch(method, domain, tail, body)
        except KeyError as exc:
            return self._error(404, str(exc))
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            return self._error(400, str(exc))
        except Exception as exc:  # pragma: no cover
            return self._internal_error(exc)

    def _auth(self, method: str, tail: list[str], body: Mapping[str, Any], headers: Mapping[str, str]):
        route = tail[0] if tail else "status"
        if route == "status" and method == "GET":
            required = bool(self.control.auth_required or self.force_auth)
            return self._ok({"required": required, "authenticated": self.control.authenticated(headers, force=self.force_auth) if required else True})
        if route == "login" and method == "POST":
            try:
                token = self.control.login(
                    str(body.get("password") or body.get("token") or ""),
                    client_key=str(headers.get("x-trace-client") or "direct"),
                )
            except ValueError:
                return self._error(401, "invalid_credentials")
            if (self.control.auth_required or self.force_auth) and not token:
                return self._error(401, "invalid_credentials")
            from .web import WebResponse
            secure = "; Secure" if self.tls_enabled else ""
            cookie = {"Set-Cookie": f"trace_session={token}; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200{secure}"} if token else {}
            return WebResponse.json({"authenticated": True}, headers=cookie)
        if route == "logout" and method == "POST":
            self.control.logout(headers)
            from .web import WebResponse
            secure = "; Secure" if self.tls_enabled else ""
            return WebResponse.json({"authenticated": False}, headers={"Set-Cookie": f"trace_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0{secure}"})
        known = {"status", "login", "logout"}
        return self._error(405 if route in known else 404, "method_not_allowed" if route in known else "route_not_found")

    def _control_dispatch(self, method: str, domain: str, tail: list[str], body: Mapping[str, Any]):
        identifier = "/".join(tail) if domain == "skills" else (tail[0] if tail else "")
        if domain == "providers":
            if method == "GET" and not identifier:
                return self._ok({"providers": self.control.providers()})
            if method == "POST" and not identifier:
                saved = self.service.control_write(self.control.save_provider, body)
                # Provider persistence and the active model loop must move as
                # one control-plane operation.  Otherwise editing an active
                # key/model silently leaves future turns on the old instance.
                if saved["active"]:
                    self._activate_model(saved)
                return self._ok({"provider": saved}, status=201)
            if method == "POST" and identifier == "active":
                saved = self.service.control_write(self.control.activate_provider, str(body.get("provider_id") or ""))
                self._activate_model(saved)
                return self._ok({"provider": saved})
            if identifier and method == "DELETE":
                was_active = self.control.provider(identifier)["active"]
                self.service.control_write(self.control.delete_provider, identifier)
                if was_active:
                    self.service.configure_model(None)
                return self._ok({"deleted": identifier})
            if identifier and method == "GET":
                return self._ok({"provider": self.control.provider(identifier)})
        if domain == "skills":
            if method == "GET" and not identifier:
                return self._ok({"skills": self.control.skills()})
            if method == "POST" and identifier:
                config = body.get("config") if isinstance(body.get("config"), Mapping) else {}
                return self._ok({"skill": self.service.control_write(self.control.set_skill, identifier, enabled=bool(body.get("enabled", True)), config=config)})
        if domain == "mcp":
            if method == "GET" and not identifier:
                statuses = self.service.runtime.broker.server_statuses()
                items = self.control.mcp_servers()
                for item in items:
                    status = dict(statuses.get(item["server_id"], item["status"]))
                    if self._mcp_restore_error:
                        status = {"status": "failed", "error": self._mcp_restore_error}
                    discovered = status.get("status") in {"connected", "catalogued"}
                    item["status"] = {
                        **status, "configured": True, "discovered": discovered,
                        "callable": bool(item["enabled"] and discovered and status.get("tool_count", 0)),
                    }
                return self._ok({"servers": items, "tools": len(self.service.runtime.broker.descriptors())})
            if method == "POST" and not identifier:
                saved = self.service.control_write(self.control.save_mcp, body)
                self._reload_control_plane()
                return self._ok({"server": saved}, status=201)
            if method == "POST" and identifier == "refresh":
                return self._ok(self._reload_control_plane())
            if method == "DELETE" and identifier:
                self.service.control_write(self.control.delete_mcp, identifier)
                self._reload_control_plane()
                return self._ok({"deleted": identifier})
        if domain == "conversations":
            if method == "GET" and not identifier:
                runs = self.service.list_runs(limit=100, offset=0)
                conversations = []
                for item in runs:
                    session = _jsonable(self.service.export_session(item.run.run_id))
                    conversations.append({
                        "run": self._run_projection(item),
                        "message_count": len(self.service.transcript(item.run.run_id)),
                        "session": session.get("session", {}),
                    })
                return self._ok({"conversations": conversations})
            if identifier and method == "GET":
                self.service.status(identifier)
                # Include the journal projection alongside the tree so clients can
                # render the active branch and branch heads without a second,
                # provider-specific query.  The tree remains unchanged for
                # backwards compatibility.
                session = _jsonable(self.service.export_session(identifier))
                return self._ok({
                    "run_id": identifier,
                    "session": session.get("session", {}),
                    "tree": session.get("tree", {}),
                    "messages": [_jsonable(item) for item in self.service.transcript(identifier)],
                })
            if identifier and method == "POST" and tail[1:] and tail[1] == "fork":
                entry = self.service.fork_session(identifier, str(body.get("from_entry_id") or ""), str(body.get("branch_id") or ""))
                session = _jsonable(self.service.export_session(identifier))
                metadata = session.get("session", {})
                active = str(metadata.get("active_branch_id") or "")
                branches = metadata.get("branches", {})
                return self._ok({
                    "run_id": identifier,
                    "entry": _jsonable(entry),
                    "branch_id": active,
                    "active_branch_id": active,
                    "branches": branches,
                    "leaf_entry_id": branches.get(active),
                })
            if identifier and method == "POST" and tail[1:] and tail[1] == "branch":
                entry = self.service.branch_session(
                    identifier,
                    str(body.get("from_entry_id") or ""),
                    expected_leaf_id=str(body.get("expected_leaf_id") or "") or None,
                )
                session = _jsonable(self.service.export_session(identifier))
                metadata = session.get("session", {})
                active = str(metadata.get("active_branch_id") or "")
                branches = metadata.get("branches", {})
                return self._ok({
                    "run_id": identifier,
                    "entry": _jsonable(entry),
                    "branch_id": active,
                    "active_branch_id": active,
                    "branches": branches,
                    "leaf_entry_id": branches.get(active),
                })
            if identifier and method == "POST" and tail[1:] and tail[1] == "checkout":
                branch_id = str(body.get("branch_id") or "").strip()
                leaf_id = self.service.checkout_session(identifier, branch_id)
                session = _jsonable(self.service.export_session(identifier))
                metadata = session.get("session", {})
                return self._ok({
                    "run_id": identifier,
                    "branch_id": branch_id,
                    "active_branch_id": metadata.get("active_branch_id", branch_id),
                    "branches": metadata.get("branches", {}),
                    "leaf_entry_id": leaf_id,
                })
        return self._error(404, "resource_not_found")

    def _reload_control_plane(self) -> dict[str, Any]:
        try:
            result = self.service.control_write(reload_mcp, self.service)
        except Exception as exc:
            self._mcp_restore_error = f"mcp_restore_failed:{type(exc).__name__}"
            raise
        self._mcp_restore_error = ""
        return result

    def system_info(self) -> dict[str, Any]:
        loop = self.service.model_loop
        if loop is None:
            active = next((item for item in self.control.providers() if item["active"]), None)
            provider = {"configured": active is not None, "ready": False, "reason": "provider_not_configured", "name": str(active["name"]) if active else "", "model": str(active["model"]) if active else ""}
        else:
            capabilities = loop.model.capabilities()
            provider = {
                "configured": True,
                "ready": bool(getattr(loop.model, "ready", True)) and not self.service.closing,
                "reason": "service_closing" if self.service.closing else ("" if getattr(loop.model, "ready", True) else "missing_credentials"),
                "name": str(capabilities.metadata.get("provider") or type(loop.model).__name__),
                "model": loop.model_name or str(capabilities.metadata.get("model") or ""),
                "capabilities": {
                    "native_system_role": capabilities.native_system_role,
                    "native_tool_calls": capabilities.native_tool_calls,
                    "parallel_tool_calls": capabilities.parallel_tool_calls,
                    "structured_output": capabilities.structured_output,
                    "streaming": capabilities.streaming,
                    "usage_reporting": capabilities.usage_reporting,
                    "max_context_tokens": capabilities.max_context_tokens,
                    "modalities": list(capabilities.modalities),
                },
            }
        return {"service": "Trace", "platform": platform.system().lower(), "control_plane": self.control.system(), "auth": {"required": self.control.auth_required or self.force_auth}, "providers": self.control.providers(), "provider": provider, "run_statuses": sorted(CANONICAL_RUN_STATUSES), "configuration": self.service.configuration_projection}


def _jsonable(value: Any) -> Any:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    if isinstance(value, Mapping):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value


__all__ = ["ControlRoutesMixin"]
