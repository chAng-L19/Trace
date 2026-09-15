from __future__ import annotations

import json
import platform
from typing import Any, Mapping

from ..application.contracts import CANONICAL_RUN_STATUSES
from ..providers import OpenAICompatibleProvider


class ControlRoutesMixin:
    """Management routes kept separate so the HTTP adapter stays small."""

    def _load_active_provider(self) -> None:
        if self.service.model_loop is not None:
            return
        active = next((item for item in self.control.providers() if item["active"]), None)
        if active is None:
            return
        self.service.configure_model(
            OpenAICompatibleProvider(
                active["base_url"], active["model"], self.control.provider_secret(active["provider_id"]),
                timeout_seconds=active["timeout_seconds"], max_context_tokens=active["max_context_tokens"],
            ),
            model_name=active["model"],
        )

    def _safe_control(self, method: str, domain: str, tail: list[str], body: Mapping[str, Any]):
        try:
            return self._control_dispatch(method, domain, tail, body)
        except KeyError as exc:
            return self._error(404, str(exc))
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            return self._error(400, str(exc))
        except Exception as exc:  # pragma: no cover
            return self._error(500, f"internal_error:{type(exc).__name__}")

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
                return self._ok({"provider": self.control.save_provider(body)}, status=201)
            if method == "POST" and identifier == "active":
                saved = self.control.activate_provider(str(body.get("provider_id") or ""))
                provider = OpenAICompatibleProvider(saved["base_url"], saved["model"], self.control.provider_secret(saved["provider_id"]), timeout_seconds=saved["timeout_seconds"], max_context_tokens=saved["max_context_tokens"])
                self.service.configure_model(provider, model_name=saved["model"])
                return self._ok({"provider": saved})
            if identifier and method == "DELETE":
                was_active = self.control.provider(identifier)["active"]
                self.control.delete_provider(identifier)
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
                return self._ok({"skill": self.control.set_skill(identifier, enabled=bool(body.get("enabled", True)), config=config)})
        if domain == "mcp":
            if method == "GET" and not identifier:
                statuses = self.service.runtime.broker.server_statuses()
                items = self.control.mcp_servers()
                for item in items:
                    item["status"] = statuses.get(item["server_id"], item["status"])
                return self._ok({"servers": items, "tools": len(self.service.runtime.broker.descriptors())})
            if method == "POST" and not identifier:
                saved = self.control.save_mcp(body)
                self._reload_control_plane()
                return self._ok({"server": saved}, status=201)
            if method == "POST" and identifier == "refresh":
                return self._ok(self._reload_control_plane())
            if method == "DELETE" and identifier:
                self.control.delete_mcp(identifier)
                self._reload_control_plane()
                return self._ok({"deleted": identifier})
        if domain == "conversations":
            if method == "GET" and not identifier:
                runs = self.service.list_runs(limit=100, offset=0)
                return self._ok({"conversations": [{"run": self._run_projection(item), "message_count": len(self.service.transcript(item.run.run_id))} for item in runs]})
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
                return self._ok({"entry": _jsonable(entry)})
        return self._error(404, "resource_not_found")

    def _reload_control_plane(self) -> dict[str, Any]:
        path = self.control.write_mcp_config()
        self.service.runtime.broker.set_secret_bindings(self.control.mcp_secret_bindings())
        self.service.runtime.broker.register_config_paths((path,))
        self.service.runtime.broker.refresh(force=True)
        return {"mcp": self.service.runtime.broker.server_statuses(), "tool_count": len(self.service.runtime.broker.descriptors())}

    def system_info(self) -> dict[str, Any]:
        loop = self.service.model_loop
        if loop is None:
            active = next((item for item in self.control.providers() if item["active"]), None)
            provider = {"configured": active is not None, "name": str(active["name"]) if active else "", "model": str(active["model"]) if active else ""}
        else:
            capabilities = loop.model.capabilities()
            provider = {
                "configured": True,
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
        return {"service": "Trace", "platform": platform.system().lower(), "control_plane": self.control.system(), "auth": {"required": self.control.auth_required or self.force_auth}, "providers": self.control.providers(), "provider": provider, "run_statuses": sorted(CANONICAL_RUN_STATUSES)}


def _jsonable(value: Any) -> Any:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    if isinstance(value, Mapping):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value


__all__ = ["ControlRoutesMixin"]
