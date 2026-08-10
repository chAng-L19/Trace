from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, ClassVar, Protocol, runtime_checkable

from ..contracts import (
    contract_version,
    json_mapping,
    json_value,
    optional_positive_float,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


@dataclass(frozen=True, slots=True)
class ToolDefinition:
    KIND: ClassVar[str] = "tool_definition"

    qualified_name: str
    name: str
    server: str
    description: str = ""
    input_schema: Mapping[str, Any] = field(default_factory=dict)
    capabilities: tuple[str, ...] = ()
    version: str = "unknown"
    side_effecting: bool = False
    supports_reconcile: bool = False
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "qualified_name": self.qualified_name,
                "name": self.name,
                "server": self.server,
                "description": self.description,
                "input_schema": dict(self.input_schema),
                "capabilities": list(self.capabilities),
                "version": self.version,
                "side_effecting": self.side_effecting,
                "supports_reconcile": self.supports_reconcile,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ToolDefinition":
        contract_version(payload, kind=cls.KIND)
        name = required_text(payload.get("name"), "tool_name")
        server = optional_text(payload.get("server")) or "local"
        return cls(
            qualified_name=optional_text(payload.get("qualified_name")) or f"{server}:{name}",
            name=name,
            server=server,
            description=str(payload.get("description") or ""),
            input_schema=json_mapping(payload.get("input_schema"), field="tool.input_schema"),
            capabilities=unique_strings(payload.get("capabilities")),
            version=optional_text(payload.get("version")) or "unknown",
            side_effecting=bool(payload.get("side_effecting", False)),
            supports_reconcile=bool(payload.get("supports_reconcile", False)),
            metadata=json_mapping(payload.get("metadata"), field="tool.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ToolCall:
    KIND: ClassVar[str] = "tool_call"

    call_id: str
    run_id: str
    tool_name: str
    arguments: Mapping[str, Any]
    idempotency_key: str = ""
    timeout_seconds: float | None = None
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "call_id": self.call_id,
                "run_id": self.run_id,
                "tool_name": self.tool_name,
                "arguments": dict(self.arguments),
                "idempotency_key": self.idempotency_key,
                "timeout_seconds": self.timeout_seconds,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ToolCall":
        contract_version(payload, kind=cls.KIND)
        return cls(
            call_id=required_text(payload.get("call_id") or payload.get("id"), "tool_call_id"),
            run_id=required_text(payload.get("run_id"), "tool_call_run_id"),
            tool_name=required_text(payload.get("tool_name") or payload.get("tool"), "tool_call_name"),
            arguments=json_mapping(payload.get("arguments"), field="tool_call.arguments"),
            idempotency_key=optional_text(payload.get("idempotency_key")),
            timeout_seconds=optional_positive_float(payload.get("timeout_seconds"), field="tool_timeout"),
            metadata=json_mapping(payload.get("metadata"), field="tool_call.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ToolResult:
    KIND: ClassVar[str] = "tool_result"

    call_id: str
    status: str
    tool_name: str
    output: Any = None
    error: str = ""
    retryable: bool = False
    input_hash: str = ""
    output_hash: str = ""
    started_at: str = ""
    finished_at: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "call_id": self.call_id,
                "status": self.status,
                "tool_name": self.tool_name,
                "output": json_value(self.output, field="tool_result.output"),
                "error": self.error,
                "retryable": self.retryable,
                "input_hash": self.input_hash,
                "output_hash": self.output_hash,
                "started_at": self.started_at,
                "finished_at": self.finished_at,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ToolResult":
        contract_version(payload, kind=cls.KIND)
        return cls(
            call_id=required_text(payload.get("call_id") or payload.get("id"), "tool_result_call_id"),
            status=required_text(payload.get("status"), "tool_result_status"),
            tool_name=required_text(payload.get("tool_name") or payload.get("tool"), "tool_result_name"),
            output=json_value(payload.get("output"), field="tool_result.output"),
            error=str(payload.get("error") or ""),
            retryable=bool(payload.get("retryable", False)),
            input_hash=optional_text(payload.get("input_hash")),
            output_hash=optional_text(payload.get("output_hash")),
            started_at=optional_text(payload.get("started_at")),
            finished_at=optional_text(payload.get("finished_at")),
            metadata=json_mapping(payload.get("metadata"), field="tool_result.metadata"),
        )


@runtime_checkable
class ToolPort(Protocol):
    def discover(self) -> tuple[ToolDefinition, ...]: ...

    def invoke(self, call: ToolCall) -> ToolResult: ...

    def reconcile(self, call: ToolCall) -> ToolResult | None: ...

    def cancel(self, call_id: str) -> bool: ...
