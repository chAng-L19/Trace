from __future__ import annotations

from collections.abc import Iterator, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, ClassVar, Protocol, runtime_checkable

from ..contracts import (
    bounded_int,
    contract_version,
    json_mapping,
    optional_text,
    required_text,
    unique_strings,
    versioned_payload,
)


def _objects(value: Any, field_name: str) -> tuple[Mapping[str, Any], ...]:
    if isinstance(value, str) or not isinstance(value, Sequence):
        return ()
    return tuple(
        json_mapping(item, field=f"{field_name}[]")
        for item in value
        if isinstance(item, Mapping)
    )


@dataclass(frozen=True, slots=True)
class ModelCapabilities:
    KIND: ClassVar[str] = "model_capabilities"

    native_system_role: bool = True
    native_tool_calls: bool = True
    parallel_tool_calls: bool = False
    structured_output: bool = False
    streaming: bool = False
    usage_reporting: bool = False
    max_context_tokens: int = 0
    modalities: tuple[str, ...] = ("text",)
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "native_system_role": self.native_system_role,
                "native_tool_calls": self.native_tool_calls,
                "parallel_tool_calls": self.parallel_tool_calls,
                "structured_output": self.structured_output,
                "streaming": self.streaming,
                "usage_reporting": self.usage_reporting,
                "max_context_tokens": self.max_context_tokens,
                "modalities": list(self.modalities),
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelCapabilities":
        contract_version(payload, kind=cls.KIND)
        return cls(
            native_system_role=bool(payload.get("native_system_role", True)),
            native_tool_calls=bool(payload.get("native_tool_calls", True)),
            parallel_tool_calls=bool(payload.get("parallel_tool_calls", False)),
            structured_output=bool(payload.get("structured_output", False)),
            streaming=bool(payload.get("streaming", False)),
            usage_reporting=bool(payload.get("usage_reporting", False)),
            max_context_tokens=bounded_int(
                payload.get("max_context_tokens", 0),
                default=0,
                minimum=0,
                maximum=2**31 - 1,
                field="max_context_tokens",
            ),
            modalities=unique_strings(payload.get("modalities")) or ("text",),
            metadata=json_mapping(payload.get("metadata"), field="model_capabilities.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ModelRequest:
    KIND: ClassVar[str] = "model_request"

    request_id: str
    run_id: str
    messages: tuple[Mapping[str, Any], ...]
    tools: tuple[Mapping[str, Any], ...] = ()
    response_schema: Mapping[str, Any] = field(default_factory=dict)
    model: str = ""
    allow_parallel_tools: bool = False
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "request_id": self.request_id,
                "run_id": self.run_id,
                "messages": [dict(item) for item in self.messages],
                "tools": [dict(item) for item in self.tools],
                "response_schema": dict(self.response_schema),
                "model": self.model,
                "allow_parallel_tools": self.allow_parallel_tools,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelRequest":
        contract_version(payload, kind=cls.KIND)
        return cls(
            request_id=required_text(payload.get("request_id") or payload.get("id"), "model_request_id"),
            run_id=required_text(payload.get("run_id"), "model_request_run_id"),
            messages=_objects(payload.get("messages"), "model_request.messages"),
            tools=_objects(payload.get("tools"), "model_request.tools"),
            response_schema=json_mapping(payload.get("response_schema"), field="model_request.response_schema"),
            model=optional_text(payload.get("model")),
            allow_parallel_tools=bool(payload.get("allow_parallel_tools", False)),
            metadata=json_mapping(payload.get("metadata"), field="model_request.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ModelResponse:
    KIND: ClassVar[str] = "model_response"

    request_id: str
    status: str
    provider: str = ""
    model: str = ""
    text: str = ""
    structured_output: Mapping[str, Any] = field(default_factory=dict)
    tool_calls: tuple[Mapping[str, Any], ...] = ()
    usage: Mapping[str, Any] = field(default_factory=dict)
    finish_reason: str = ""
    error: str = ""
    response_hash: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "request_id": self.request_id,
                "status": self.status,
                "provider": self.provider,
                "model": self.model,
                "text": self.text,
                "structured_output": dict(self.structured_output),
                "tool_calls": [dict(item) for item in self.tool_calls],
                "usage": dict(self.usage),
                "finish_reason": self.finish_reason,
                "error": self.error,
                "response_hash": self.response_hash,
                "metadata": dict(self.metadata),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelResponse":
        contract_version(payload, kind=cls.KIND)
        return cls(
            request_id=required_text(payload.get("request_id"), "model_response_request_id"),
            status=required_text(payload.get("status"), "model_response_status"),
            provider=optional_text(payload.get("provider")),
            model=optional_text(payload.get("model")),
            text=str(payload.get("text") or ""),
            structured_output=json_mapping(
                payload.get("structured_output"),
                field="model_response.structured_output",
            ),
            tool_calls=_objects(payload.get("tool_calls"), "model_response.tool_calls"),
            usage=json_mapping(payload.get("usage"), field="model_response.usage"),
            finish_reason=optional_text(payload.get("finish_reason")),
            error=str(payload.get("error") or ""),
            response_hash=optional_text(payload.get("response_hash")),
            metadata=json_mapping(payload.get("metadata"), field="model_response.metadata"),
        )


@dataclass(frozen=True, slots=True)
class ModelStreamEvent:
    KIND: ClassVar[str] = "model_stream_event"

    request_id: str
    sequence: int
    event_type: str
    payload: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return versioned_payload(
            self.KIND,
            {
                "request_id": self.request_id,
                "sequence": self.sequence,
                "event_type": self.event_type,
                "payload": dict(self.payload),
            },
        )

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelStreamEvent":
        contract_version(payload, kind=cls.KIND)
        return cls(
            request_id=required_text(payload.get("request_id"), "stream_request_id"),
            sequence=bounded_int(
                payload.get("sequence", 0),
                default=0,
                minimum=0,
                maximum=2**63 - 1,
                field="stream_sequence",
            ),
            event_type=required_text(payload.get("event_type"), "stream_event_type"),
            payload=json_mapping(payload.get("payload"), field="stream_event.payload"),
        )


@runtime_checkable
class ModelPort(Protocol):
    def capabilities(self) -> ModelCapabilities: ...

    def complete(self, request: ModelRequest) -> ModelResponse: ...

    def stream(self, request: ModelRequest) -> Iterator[ModelStreamEvent]: ...

    def cancel(self, request_id: str) -> bool: ...
