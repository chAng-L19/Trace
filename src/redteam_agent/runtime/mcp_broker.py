from __future__ import annotations

import hashlib
import time
import tomllib
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from .mcp_clients import HttpMcpClient, StdioMcpClient
from .mcp_config import McpServerSpec, parse_mcp_server_specs, profile_capabilities
from .models import ToolDescriptor
from .security import safe_error_text, secure_directory


class McpBrokerMixin:
    def bind_workspace_root(self, root: Path) -> None:
        resolved = root.expanduser().resolve(strict=False)
        secure_directory(resolved)
        self._workspace_root = resolved

    def server_statuses(self) -> dict[str, dict[str, Any]]:
        with self._lifecycle_lock:
            active_by_server: dict[str, int] = {}
            for server_name, _run_id in self._run_clients:
                active_by_server[server_name] = active_by_server.get(server_name, 0) + 1
            return {
                name: {**status, "active_run_clients": active_by_server.get(name, 0)}
                for name, status in sorted(self._server_status.items())
            }

    def discover_from_configs(self, paths: Sequence[Path]) -> tuple[ToolDescriptor, ...]:
        with self._lifecycle_lock:
            return self._discover_from_configs_locked(paths)

    def _discover_from_configs_locked(self, paths: Sequence[Path]) -> tuple[ToolDescriptor, ...]:
        for path in paths:
            resolved_path = path.expanduser().resolve(strict=False)
            if resolved_path not in self._config_paths:
                self._config_paths.append(resolved_path)
            if not path.is_file():
                continue
            try:
                config = tomllib.loads(path.read_text(encoding="utf-8-sig"))
            except (OSError, tomllib.TOMLDecodeError) as exc:
                self._record_discovery_error(f"config:{path}", exc)
                continue
            automation = config.get("automation") if isinstance(config.get("automation"), Mapping) else {}
            raw_overrides = automation.get("tool_capabilities")
            if isinstance(raw_overrides, Mapping):
                for tool_name, raw_capabilities in raw_overrides.items():
                    if not isinstance(raw_capabilities, list):
                        continue
                    capabilities = tuple(
                        dict.fromkeys(
                            str(item).strip().casefold().replace("-", "_")
                            for item in raw_capabilities
                            if str(item).strip()
                        )
                    )
                    if capabilities:
                        self._capability_overrides.setdefault(str(tool_name).casefold(), capabilities)
            signatures = {item.signature for item in self._server_configs.values()}
            for spec in parse_mcp_server_specs(path, config):
                server_name = spec.name
                if server_name.casefold() in {
                    "codex-redteam-orchestrator",
                    "codex-redteam-runtime",
                    "redteam-agent-runtime",
                }:
                    continue
                if not spec.enabled:
                    self._server_status[server_name] = {
                        "status": "disabled",
                        "transport": spec.transport,
                        "scope": spec.scope,
                        "preset": spec.preset,
                    }
                    continue
                command_name = Path(spec.command).name.casefold() if spec.command else ""
                if spec.preset == "ida" and command_name.startswith("ida-free-pc"):
                    error = "ida_installer_not_mcp_server"
                    self._record_discovery_error(f"server:{server_name}", error)
                    self._server_status[server_name] = {
                        "status": "failed",
                        "transport": spec.transport,
                        "scope": spec.scope,
                        "preset": spec.preset,
                        "error": error,
                    }
                    continue
                if spec.transport not in {"stdio", "http"} or (
                    spec.transport == "stdio" and not spec.command
                ) or (spec.transport == "http" and not spec.url):
                    self._record_discovery_error(f"server:{server_name}", "unsupported_transport")
                    self._server_status[server_name] = {
                        "status": "failed",
                        "transport": spec.transport,
                        "scope": spec.scope,
                        "preset": spec.preset,
                        "error": "unsupported_transport",
                    }
                    continue
                if spec.signature in signatures:
                    self._record_discovery_error(f"server:{server_name}", "duplicate_server_signature")
                    self._server_status[server_name] = {
                        "status": "duplicate",
                        "transport": spec.transport,
                        "scope": spec.scope,
                        "preset": spec.preset,
                    }
                    continue
                signatures.add(spec.signature)
                self._server_configs[server_name] = spec
                self._discover_server(spec)
        return self.descriptors()

    def refresh(self, *, force: bool = False) -> tuple[ToolDescriptor, ...]:
        with self._lifecycle_lock:
            now = time.monotonic()
            if self._active_calls or (not force and now - self._last_refresh < 10.0):
                return self.descriptors()
            self._last_refresh = now
            for run_id in sorted({run_id for _server_name, run_id in self._run_clients}):
                self.close_run(run_id)
            for client in self._clients.values():
                client.close()
            self._clients.clear()
            self._run_resources.clear()
            self._server_configs.clear()
            self._server_status.clear()
            self._capability_overrides.clear()
            self._discovery_errors.clear()
            for qualified in [
                name
                for name, descriptor in self._descriptors.items()
                if descriptor.source.startswith("live-mcp")
            ]:
                self._descriptors.pop(qualified, None)
            return self.discover_from_configs(tuple(self._config_paths))

    def _create_client(
        self,
        spec: McpServerSpec,
        *,
        run_id: str = "",
    ) -> StdioMcpClient | HttpMcpClient:
        workspace = self._workspace_for(run_id) if run_id else None
        rendered = spec.render(run_id=run_id, workspace=workspace)
        if rendered.transport == "stdio":
            return StdioMcpClient(
                rendered.name,
                rendered.command,
                rendered.args,
                rendered.env,
                cwd=rendered.cwd or workspace,
                startup_timeout=rendered.startup_timeout_seconds,
                roots=(workspace,) if workspace is not None else (),
            )
        return HttpMcpClient(
            rendered.name,
            rendered.url,
            rendered.headers,
            startup_timeout=rendered.startup_timeout_seconds,
        )

    def _workspace_for(self, run_id: str) -> Path | None:
        if not run_id or self._workspace_root is None:
            return None
        key = hashlib.sha256(run_id.encode("utf-8")).hexdigest()
        workspace = self._workspace_root / key[:2] / key[2:]
        secure_directory(workspace)
        return workspace

    def _discover_server(self, spec: McpServerSpec) -> None:
        if spec.transport == "stdio":
            self._discover_stdio(spec)
        else:
            self._discover_http(spec)

    def _discover_stdio(self, spec: McpServerSpec) -> None:
        server_name = spec.name
        existing = self._clients.get(server_name)
        if isinstance(existing, StdioMcpClient) and existing.process.poll() is None:
            return
        if existing is not None:
            existing.close()
            self._clients.pop(server_name, None)
        try:
            client = self._create_client(spec)
            tools = client.list_tools()
        except Exception as exc:
            failed_client = locals().get("client")
            if isinstance(failed_client, (StdioMcpClient, HttpMcpClient)):
                failed_client.close()
            self._record_discovery_error(f"server:{server_name}", exc)
            self._server_status[server_name] = {
                "status": "failed",
                "transport": spec.transport,
                "scope": spec.scope,
                "preset": spec.preset,
                "error": safe_error_text(exc),
            }
            return
        if spec.scope == "shared":
            self._clients[server_name] = client
        else:
            client.close()
        self._register_mcp_tools(spec, tools)

    def _discover_http(self, spec: McpServerSpec) -> None:
        server_name = spec.name
        if server_name in self._clients:
            return
        try:
            client = self._create_client(spec)
            tools = client.list_tools()
        except Exception as exc:
            failed_client = locals().get("client")
            if isinstance(failed_client, (StdioMcpClient, HttpMcpClient)):
                failed_client.close()
            self._record_discovery_error(f"server:{server_name}", exc)
            self._server_status[server_name] = {
                "status": "failed",
                "transport": spec.transport,
                "scope": spec.scope,
                "preset": spec.preset,
                "error": safe_error_text(exc),
            }
            return
        if spec.scope == "shared":
            self._clients[server_name] = client
        else:
            client.close()
        self._register_mcp_tools(spec, tools)

    def _register_mcp_tools(
        self,
        spec: McpServerSpec,
        tools: Sequence[Mapping[str, Any]],
    ) -> None:
        registered = 0
        for item in tools:
            name = str(item.get("name") or "").strip()
            if not name or not spec.accepts_tool(name):
                continue
            description = str(item.get("description") or "")
            schema = item.get("inputSchema") if isinstance(item.get("inputSchema"), Mapping) else {"type": "object"}
            annotations = item.get("annotations") if isinstance(item.get("annotations"), Mapping) else {}
            capabilities = tuple(
                dict.fromkeys(
                    (
                        *profile_capabilities(spec.preset, name),
                        *self._capabilities_for(spec.name, name, description, schema),
                    )
                )
            )
            read_only = spec.tool_is_read_only(name, annotations)
            descriptor = ToolDescriptor(
                server=spec.name,
                name=name,
                description=description,
                input_schema=dict(schema),
                capabilities=capabilities,
                source="live-mcp-run" if spec.scope == "run" else "live-mcp",
                healthy=True,
                priority=self._priority_for(spec.name, name, f"{spec.name}:{name}"),
                version=str(item.get("version") or self.canonical_hash({"description": description, "schema": schema})[:16]),
                schema_hash=self.canonical_hash(schema),
                side_effecting=not read_only,
                supports_reconcile=False,
                metadata={
                    "mcp_scope": spec.scope,
                    "mcp_preset": spec.preset,
                    "mcp_annotations": dict(annotations),
                    "tool_timeout_seconds": spec.tool_timeout_seconds,
                },
            )
            self._descriptors[descriptor.qualified_name] = descriptor
            registered += 1
        self._server_status[spec.name] = {
            "status": "connected" if spec.scope == "shared" else "catalogued",
            "transport": spec.transport,
            "scope": spec.scope,
            "preset": spec.preset,
            "tool_count": registered,
        }

    def _refresh_changed_catalog(
        self,
        spec: McpServerSpec,
        client: StdioMcpClient | HttpMcpClient,
    ) -> None:
        consume = getattr(client, "consume_tools_changed", None)
        if not callable(consume) or not consume():
            return
        tools = client.list_tools()
        for qualified in [
            name for name, item in self._descriptors.items() if item.server == spec.name
        ]:
            self._descriptors.pop(qualified, None)
        self._register_mcp_tools(spec, tools)

    @staticmethod
    def _structured_content(output: Any) -> Mapping[str, Any]:
        if not isinstance(output, Mapping):
            return {}
        structured = output.get("structuredContent")
        return structured if isinstance(structured, Mapping) else output

    def _track_run_resource(
        self,
        spec: McpServerSpec,
        *,
        run_id: str,
        tool_name: str,
        arguments: Mapping[str, Any],
        output: Any,
    ) -> None:
        if spec.preset != "ida" or not run_id:
            return
        key = (spec.name, run_id)
        if tool_name == "idb_open":
            session = self._structured_content(output).get("session")
            if isinstance(session, Mapping):
                session_id = str(session.get("session_id") or "").strip()
                if session_id:
                    self._run_resources.setdefault(key, set()).add(session_id)
        elif tool_name == "idb_close":
            session_id = str(arguments.get("database") or "").strip()
            if session_id:
                resources = self._run_resources.get(key)
                if resources is not None:
                    resources.discard(session_id)
                    if not resources:
                        self._run_resources.pop(key, None)

    def _restart_server(self, server_name: str) -> None:
        with self._lifecycle_lock:
            spec = self._server_configs.get(server_name)
            if spec is None:
                return
            client = self._clients.pop(server_name, None)
            if client is not None:
                client.close()
            for qualified in [name for name, item in self._descriptors.items() if item.server == server_name]:
                self._descriptors.pop(qualified, None)
            self._discover_server(spec)

    def _client_for(
        self,
        descriptor: ToolDescriptor,
        *,
        run_id: str,
    ) -> StdioMcpClient | HttpMcpClient | None:
        spec = self._server_configs.get(descriptor.server)
        if spec is None or spec.scope == "shared":
            return self._clients.get(descriptor.server)
        if not run_id:
            raise ValueError(f"mcp_run_id_required:{descriptor.server}")
        key = (descriptor.server, run_id)
        client = self._run_clients.get(key)
        if isinstance(client, StdioMcpClient) and client.process.poll() is not None:
            client.close()
            self._run_clients.pop(key, None)
            client = None
        if client is None:
            client = self._create_client(spec, run_id=run_id)
            self._run_clients[key] = client
        self._refresh_changed_catalog(spec, client)
        return client

    def close_run(self, run_id: str) -> tuple[Mapping[str, Any], ...]:
        reports: list[Mapping[str, Any]] = []
        with self._lifecycle_lock:
            detached = [
                (
                    key[0],
                    self._run_clients.pop(key),
                    self._server_configs.get(key[0]),
                    sorted(self._run_resources.pop(key, set())),
                )
                for key in [key for key in self._run_clients if key[1] == run_id]
            ]
        for server_name, client, spec, resources in detached:
            closed: list[str] = []
            errors: list[Mapping[str, str]] = []
            if spec is not None and spec.preset == "ida":
                for session_id in resources:
                    try:
                        result = client.call_tool(
                            "idb_close",
                            {"database": session_id, "save": True},
                            timeout=spec.tool_timeout_seconds,
                        )
                        structured = self._structured_content(result)
                        if structured.get("error"):
                            errors.append(
                                {
                                    "resource": session_id,
                                    "error": safe_error_text(structured.get("error")),
                                }
                            )
                        else:
                            closed.append(session_id)
                    except Exception as exc:
                        errors.append(
                            {"resource": session_id, "error": safe_error_text(exc)}
                        )
            client.close()
            reports.append(
                {
                    "server": server_name,
                    "preset": spec.preset if spec is not None else "",
                    "resources_discovered": len(resources),
                    "resources_closed": closed,
                    "errors": errors,
                    "status": "closed" if not errors else "closed_with_errors",
                }
            )
        return tuple(reports)

    def close(self) -> None:
        with self._lifecycle_lock:
            run_ids = sorted({run_id for _server_name, run_id in self._run_clients})
        for run_id in run_ids:
            self.close_run(run_id)
        with self._lifecycle_lock:
            shared = tuple(self._clients.values())
            self._clients.clear()
            leftovers = tuple(self._run_clients.values())
            self._run_clients.clear()
            self._run_resources.clear()
        for client in (*shared, *leftovers):
            client.close()

