from __future__ import annotations

import hashlib
import json
import math
import re
import threading
import time
from collections.abc import Mapping, Sequence
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .models import ToolCallResult, ToolDescriptor, utc_now
from .mcp_broker import McpBrokerMixin
from .mcp_config import McpServerSpec
from .security import redact_sensitive, safe_error_text


TOKEN_RE = re.compile(r"[a-z0-9]+", re.IGNORECASE)
MAX_TOOL_OUTPUT_BYTES = 16 * 1024 * 1024

from .mcp_clients import (
    Adapter,
    CAPABILITY_ALIASES,
    CAPABILITY_MARKERS,
    HttpMcpClient,
    StdioMcpClient,
    ToolHealthState,
)

class ToolBroker(McpBrokerMixin):
    def __init__(self, *, tool_priority: Sequence[str] = ()) -> None:
        self.tool_priority = tuple(
            (re.sub(r"[^a-z0-9]+", "", name.casefold()), index)
            for index, name in enumerate(tool_priority)
            if str(name).strip()
        )
        self._descriptors: dict[str, ToolDescriptor] = {}
        self._adapters: dict[str, Adapter] = {}
        self._reconcilers: dict[str, Adapter] = {}
        self._clients: dict[str, StdioMcpClient | HttpMcpClient] = {}
        self._run_clients: dict[tuple[str, str], StdioMcpClient | HttpMcpClient] = {}
        self._run_resources: dict[tuple[str, str], set[str]] = {}
        self._active_call_clients: dict[str, StdioMcpClient | HttpMcpClient] = {}
        self._server_configs: dict[str, McpServerSpec] = {}
        self._server_status: dict[str, dict[str, Any]] = {}
        self._config_paths: list[Path] = []
        self._workspace_root: Path | None = None
        self._last_refresh = 0.0
        self._lifecycle_lock = threading.RLock()
        self._active_calls = 0
        self._health: dict[str, ToolHealthState] = {}
        self._discovery_errors: list[str] = []
        self._capability_overrides: dict[str, tuple[str, ...]] = {}

    @property
    def discovery_errors(self) -> tuple[str, ...]:
        with self._lifecycle_lock:
            return tuple(self._discovery_errors)

    def health_snapshot(self) -> dict[str, dict[str, Any]]:
        with self._lifecycle_lock:
            now = time.monotonic()
            return {
                name: {**asdict(state), "cooling_down": state.cooldown_until > now}
                for name, state in self._health.items()
            }

    def _health_for(self, qualified_name: str) -> ToolHealthState:
        return self._health.setdefault(qualified_name, ToolHealthState())

    def _record_discovery_error(self, source: str, error: Any) -> None:
        self._discovery_errors.append(safe_error_text(f"{source}:{error}"))

    def _record_result(self, qualified_name: str, *, success: bool, latency_ms: float, error: str = "") -> None:
        with self._lifecycle_lock:
            state = self._health_for(qualified_name)
            total = state.successes + state.failures
            state.average_latency_ms = ((state.average_latency_ms * total) + latency_ms) / (total + 1)
            if success:
                state.successes += 1
                state.consecutive_failures = 0
                state.cooldown_until = 0.0
                state.last_error = ""
                return
            state.failures += 1
            state.consecutive_failures += 1
            state.last_error = error
            if state.consecutive_failures >= 3:
                state.cooldown_until = time.monotonic() + min(300.0, 15.0 * state.consecutive_failures)

    def record_semantic_failure(self, descriptor: ToolDescriptor, reason: str) -> None:
        with self._lifecycle_lock:
            state = self._health_for(descriptor.qualified_name)
            state.semantic_failures += 1
            state.consecutive_failures += 1
            state.last_error = reason
            if state.consecutive_failures >= 3:
                state.cooldown_until = time.monotonic() + min(300.0, 15.0 * state.consecutive_failures)

    def record_semantic_success(self, descriptor: ToolDescriptor) -> None:
        with self._lifecycle_lock:
            state = self._health_for(descriptor.qualified_name)
            state.consecutive_failures = 0
            state.semantic_failures = max(0, state.semantic_failures - 1)
            state.cooldown_until = 0.0

    def _priority_for(self, *names: str) -> int:
        candidates = tuple(re.sub(r"[^a-z0-9]+", "", name.casefold()) for name in names if name)
        for preferred, priority in self.tool_priority:
            if any(preferred == candidate or preferred in candidate or candidate in preferred for candidate in candidates):
                return priority
        return 100

    @staticmethod
    def infer_capabilities(name: str, description: str = "", schema: Mapping[str, Any] | None = None) -> tuple[str, ...]:
        explicit = (schema or {}).get("x-capabilities")
        if isinstance(explicit, list) and explicit:
            return tuple(dict.fromkeys(str(item).strip().casefold().replace("-", "_") for item in explicit if str(item).strip()))
        schema_text = json.dumps(schema or {}, ensure_ascii=False, default=str)
        tokens = set(TOKEN_RE.findall(f"{name} {description} {schema_text}".casefold()))
        capabilities: list[str] = []
        for capability, markers in CAPABILITY_MARKERS.items():
            if tokens & markers:
                capabilities.append(capability)
        return tuple(capabilities)

    def _capabilities_for(
        self,
        server_name: str,
        name: str,
        description: str,
        schema: Mapping[str, Any],
    ) -> tuple[str, ...]:
        for key in (f"{server_name}:{name}", name, server_name):
            override = self._capability_overrides.get(key.casefold())
            if override:
                return override
        return self.infer_capabilities(name, description, schema)

    def register_adapter(
        self,
        *,
        name: str,
        capabilities: Sequence[str],
        adapter: Adapter,
        description: str = "",
        server: str = "builtin",
        priority: int = 10,
        input_schema: Mapping[str, Any] | None = None,
        version: str = "builtin-v1",
        side_effecting: bool = False,
        reconciler: Adapter | None = None,
    ) -> ToolDescriptor:
        schema = dict(input_schema or {"type": "object"})
        descriptor = ToolDescriptor(
            server=server,
            name=name,
            description=description,
            input_schema=schema,
            capabilities=tuple(dict.fromkeys(str(item) for item in capabilities)),
            source="registered-adapter",
            healthy=True,
            priority=priority,
            version=version,
            schema_hash=self.canonical_hash(schema),
            side_effecting=side_effecting,
            supports_reconcile=reconciler is not None,
        )
        with self._lifecycle_lock:
            self._descriptors[descriptor.qualified_name] = descriptor
            self._adapters[descriptor.qualified_name] = adapter
            if reconciler is not None:
                self._reconcilers[descriptor.qualified_name] = reconciler
        return descriptor

    @staticmethod
    def canonical_json(value: Any) -> bytes:
        return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str).encode("utf-8")

    @classmethod
    def canonical_hash(cls, value: Any) -> str:
        return hashlib.sha256(cls.canonical_json(value)).hexdigest()

    @classmethod
    def output_size(cls, value: Any) -> int:
        return len(cls.canonical_json(value))

    @staticmethod
    def _invoke_adapter(adapter: Adapter, arguments: Mapping[str, Any], *, timeout: float) -> Any:
        """Run an in-process adapter without allowing it to hold the caller.

        Python cannot safely terminate an arbitrary adapter thread.  The worker
        is therefore daemonized and an elapsed deadline means "outcome
        unknown", which callers already map to reconciliation for side-effecting
        descriptors.  The adapter may still complete after this method returns.
        """

        completed = threading.Event()
        result: dict[str, Any] = {}

        def run() -> None:
            try:
                result["output"] = adapter(dict(arguments))
            except BaseException as exc:  # returned through the broker's normal error path
                result["error"] = exc
            finally:
                completed.set()

        worker = threading.Thread(target=run, name="codex-runtime-adapter", daemon=True)
        worker.start()
        if not completed.wait(timeout=max(0.001, float(timeout))):
            raise TimeoutError("adapter_timeout")
        error = result.get("error")
        if isinstance(error, BaseException):
            raise error
        return result.get("output")

    def descriptors(self) -> tuple[ToolDescriptor, ...]:
        with self._lifecycle_lock:
            return tuple(sorted(self._descriptors.values(), key=lambda item: (item.priority, item.qualified_name.casefold())))

    @staticmethod
    def _supports(descriptor: ToolDescriptor, capability: str) -> bool:
        normalized = capability.casefold().replace("-", "_")
        accepted = CAPABILITY_ALIASES.get(normalized, frozenset({normalized})) | frozenset({normalized})
        offered = {item.casefold().replace("-", "_") for item in descriptor.capabilities}
        return bool(accepted & offered)

    @classmethod
    def _schema_error(cls, schema: Mapping[str, Any], value: Any, path: str = "arguments") -> str:
        expected_type = schema.get("type")
        type_checks = {
            "object": lambda item: isinstance(item, Mapping),
            "array": lambda item: isinstance(item, list),
            "string": lambda item: isinstance(item, str),
            "integer": lambda item: isinstance(item, int) and not isinstance(item, bool),
            "number": lambda item: isinstance(item, (int, float)) and not isinstance(item, bool),
            "boolean": lambda item: isinstance(item, bool),
            "null": lambda item: item is None,
        }
        if isinstance(expected_type, str) and expected_type in type_checks and not type_checks[expected_type](value):
            return f"{path}:expected_{expected_type}"
        if "enum" in schema and isinstance(schema["enum"], list) and value not in schema["enum"]:
            return f"{path}:enum"
        if isinstance(value, Mapping):
            required = schema.get("required") if isinstance(schema.get("required"), list) else []
            missing = [str(item) for item in required if str(item) not in value]
            if missing:
                return f"tool_schema_required_missing:{','.join(missing)}"
            properties = schema.get("properties") if isinstance(schema.get("properties"), Mapping) else {}
            if schema.get("additionalProperties") is False:
                unknown = [str(key) for key in value if key not in properties]
                if unknown:
                    return f"{path}:additional_properties:{','.join(unknown)}"
            for key, item in value.items():
                child_schema = properties.get(key)
                if isinstance(child_schema, Mapping):
                    error = cls._schema_error(child_schema, item, f"{path}.{key}")
                    if error:
                        return error
        if isinstance(value, list) and isinstance(schema.get("items"), Mapping):
            for index, item in enumerate(value):
                error = cls._schema_error(schema["items"], item, f"{path}[{index}]")
                if error:
                    return error
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            if not math.isfinite(float(value)):
                return f"{path}:finite"
            if schema.get("minimum") is not None and value < schema["minimum"]:
                return f"{path}:minimum"
            if schema.get("maximum") is not None and value > schema["maximum"]:
                return f"{path}:maximum"
        if isinstance(value, str):
            if schema.get("minLength") is not None and len(value) < int(schema["minLength"]):
                return f"{path}:minLength"
            if schema.get("maxLength") is not None and len(value) > int(schema["maxLength"]):
                return f"{path}:maxLength"
        return ""

    def select(self, capabilities: Sequence[str], *, exclude: Sequence[str] = ()) -> ToolDescriptor | None:
        with self._lifecycle_lock:
            excluded = set(exclude)
            candidates: list[tuple[int, int, str, ToolDescriptor]] = []
            for descriptor in self.descriptors():
                if descriptor.qualified_name in excluded or not descriptor.healthy:
                    continue
                health = self._health_for(descriptor.qualified_name)
                if health.cooldown_until > time.monotonic():
                    continue
                for capability_index, capability in enumerate(capabilities):
                    if self._supports(descriptor, capability):
                        health_penalty = health.consecutive_failures * 25 + health.semantic_failures * 5
                        latency_penalty = min(100, int(health.average_latency_ms / 100))
                        candidates.append((capability_index, descriptor.priority + health_penalty + latency_penalty, descriptor.qualified_name, descriptor))
                        break
            candidates.sort(key=lambda item: (item[0], item[1], item[2].casefold()))
            return candidates[0][3] if candidates else None

    def explain_selection(self, capabilities: Sequence[str], *, exclude: Sequence[str] = ()) -> dict[str, Any]:
        excluded = set(exclude)
        candidates: list[dict[str, Any]] = []
        for descriptor in self.descriptors():
            matches = [capability for capability in capabilities if self._supports(descriptor, capability)]
            if not matches:
                continue
            health = self._health_for(descriptor.qualified_name)
            candidates.append(
                {
                    "tool": descriptor.qualified_name,
                    "capability_match": matches,
                    "source": descriptor.source,
                    "priority": descriptor.priority,
                    "excluded": descriptor.qualified_name in excluded,
                    "cooling_down": health.cooldown_until > time.monotonic(),
                    "consecutive_failures": health.consecutive_failures,
                    "average_latency_ms": round(health.average_latency_ms, 2),
                }
            )
        selected = self.select(capabilities, exclude=exclude)
        return {
            "selected_tool": selected.qualified_name if selected else "",
            "required_capabilities": list(capabilities),
            "fallback_reason": "prior_tool_failed" if excluded and selected else "",
            "candidates": candidates,
        }

    def call(
        self,
        descriptor: ToolDescriptor,
        arguments: Mapping[str, Any],
        *,
        timeout: float = 60.0,
        run_id: str = "",
        external_call_id: str = "",
    ) -> ToolCallResult:
        started_at = utc_now()
        started_clock = time.monotonic()
        qualified = descriptor.qualified_name
        call_id = f"call-{hashlib.sha256(f'{qualified}\0{time.time_ns()}'.encode()).hexdigest()[:24]}"
        input_hash = self.canonical_hash(arguments)
        schema_error = self._schema_error(descriptor.input_schema, arguments)
        if schema_error:
            return ToolCallResult(
                status="failed",
                error=safe_error_text(schema_error),
                tool=qualified,
                started_at=started_at,
                retryable=False,
                call_id=call_id,
                input_hash=input_hash,
                tool_version=descriptor.version,
            )
        with self._lifecycle_lock:
            self._active_calls += 1
            adapter = self._adapters.get(qualified)
        try:
            if adapter is not None:
                output = self._invoke_adapter(adapter, arguments, timeout=timeout)
            else:
                with self._lifecycle_lock:
                    client = self._client_for(descriptor, run_id=run_id)
                if (
                    isinstance(client, StdioMcpClient)
                    and client.process.poll() is not None
                    and str(descriptor.metadata.get("mcp_scope") or "shared") == "shared"
                ):
                    self._restart_server(descriptor.server)
                    with self._lifecycle_lock:
                        client = self._clients.get(descriptor.server)
                if client is None:
                    raise RuntimeError(safe_error_text(f"mcp_client_missing:{descriptor.server}"))
                configured_timeout = descriptor.metadata.get("tool_timeout_seconds")
                effective_timeout = timeout
                if isinstance(configured_timeout, (int, float)) and configured_timeout > 0:
                    effective_timeout = min(timeout, float(configured_timeout))
                if external_call_id:
                    with self._lifecycle_lock:
                        self._active_call_clients[external_call_id] = client
                if isinstance(client, StdioMcpClient):
                    output = client.call_tool(
                        descriptor.name,
                        arguments,
                        timeout=effective_timeout,
                        cancellation_id=external_call_id,
                    )
                else:
                    output = client.call_tool(descriptor.name, arguments, timeout=effective_timeout)
                spec = self._server_configs.get(descriptor.server)
                if spec is not None:
                    self._track_run_resource(
                        spec,
                        run_id=run_id,
                        tool_name=descriptor.name,
                        arguments=arguments,
                        output=output,
                    )
            if isinstance(output, Mapping) and output.get("isError") is True:
                self._record_result(qualified, success=False, latency_ms=(time.monotonic() - started_clock) * 1000, error="mcp_tool_error")
                return ToolCallResult(
                    status="failed",
                    output=redact_sensitive(output),
                    error="mcp_tool_error",
                    tool=qualified,
                    started_at=started_at,
                    retryable=False,
                    call_id=call_id,
                    input_hash=input_hash,
                    output_hash=self.canonical_hash(output),
                    tool_version=descriptor.version,
                )
            if self.output_size(output) > MAX_TOOL_OUTPUT_BYTES:
                self._record_result(qualified, success=False, latency_ms=(time.monotonic() - started_clock) * 1000, error="tool_output_too_large")
                return ToolCallResult(
                    status="failed",
                    error="tool_output_too_large",
                    tool=qualified,
                    started_at=started_at,
                    retryable=False,
                    call_id=call_id,
                    input_hash=input_hash,
                    tool_version=descriptor.version,
                )
            self._record_result(qualified, success=True, latency_ms=(time.monotonic() - started_clock) * 1000)
            return ToolCallResult(
                status="success",
                output=output,
                tool=qualified,
                started_at=started_at,
                call_id=call_id,
                input_hash=input_hash,
                output_hash=self.canonical_hash(output),
                tool_version=descriptor.version,
            )
        except (TimeoutError, OSError, ConnectionError, BrokenPipeError) as exc:
            error = safe_error_text(exc)
            self._record_result(qualified, success=False, latency_ms=(time.monotonic() - started_clock) * 1000, error=error)
            return ToolCallResult(
                status="failed",
                error=error,
                tool=qualified,
                started_at=started_at,
                retryable=True,
                call_id=call_id,
                input_hash=input_hash,
                tool_version=descriptor.version,
            )
        except Exception as exc:
            error = safe_error_text(exc)
            self._record_result(qualified, success=False, latency_ms=(time.monotonic() - started_clock) * 1000, error=error)
            return ToolCallResult(
                status="failed",
                error=error,
                tool=qualified,
                started_at=started_at,
                retryable=False,
                call_id=call_id,
                input_hash=input_hash,
                tool_version=descriptor.version,
            )
        finally:
            with self._lifecycle_lock:
                if external_call_id:
                    self._active_call_clients.pop(external_call_id, None)
                self._active_calls = max(0, self._active_calls - 1)

    def cancel(self, call_id: str) -> bool:
        with self._lifecycle_lock:
            client = self._active_call_clients.get(call_id)
        if isinstance(client, StdioMcpClient):
            return client.cancel_request(call_id)
        return False

    def reconcile(
        self,
        descriptor: ToolDescriptor,
        *,
        idempotency_key: str,
        arguments: Mapping[str, Any],
        timeout: float = 60.0,
    ) -> ToolCallResult | None:
        with self._lifecycle_lock:
            reconciler = self._reconcilers.get(descriptor.qualified_name)
        if reconciler is None:
            return None
        started_at = utc_now()
        input_hash = self.canonical_hash(arguments)
        try:
            output = self._invoke_adapter(
                reconciler,
                {"idempotency_key": idempotency_key, "arguments": dict(arguments)},
                timeout=timeout,
            )
            if self.output_size(output) > MAX_TOOL_OUTPUT_BYTES:
                raise ValueError("tool_output_too_large")
        except Exception as exc:
            return ToolCallResult(
                status="failed",
                error=safe_error_text(f"reconcile_failed:{exc}"),
                tool=descriptor.qualified_name,
                started_at=started_at,
                retryable=True,
                input_hash=input_hash,
                tool_version=descriptor.version,
            )
        return ToolCallResult(
            status="success",
            output=output,
            tool=descriptor.qualified_name,
            started_at=started_at,
            input_hash=input_hash,
            output_hash=self.canonical_hash(output),
            tool_version=descriptor.version,
        )

    def __enter__(self) -> "ToolBroker":
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.close()
