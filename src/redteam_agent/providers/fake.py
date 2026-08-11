from __future__ import annotations

from collections.abc import Iterable, Iterator, Mapping
from dataclasses import dataclass, replace
from threading import RLock
from typing import Any, Callable

from ..core import ModelCapabilities, ModelRequest, ModelResponse, ModelStreamEvent


@dataclass(frozen=True, slots=True)
class ScriptedStream:
    events: tuple[ModelStreamEvent | Mapping[str, Any], ...]
    error: BaseException | None = None
    error_after: int | None = None


class FakeModelProvider:
    """Deterministic ModelPort used for protocol and recovery tests."""

    def __init__(
        self,
        responses: Iterable[
            ModelResponse
            | Mapping[str, Any]
            | BaseException
            | Callable[[ModelRequest], ModelResponse | Mapping[str, Any] | BaseException]
        ] = (),
        *,
        streams: Iterable[ScriptedStream] = (),
        provider: str = "fake",
        model: str = "fake-model",
        capabilities: ModelCapabilities | None = None,
    ) -> None:
        self.provider = provider
        self.model = model
        self._capabilities = capabilities or ModelCapabilities(
            native_system_role=True,
            native_tool_calls=True,
            parallel_tool_calls=True,
            structured_output=True,
            streaming=True,
            usage_reporting=True,
            max_context_tokens=128_000,
            metadata={"provider": provider, "model": model},
        )
        self._responses = list(responses)
        self._streams = list(streams)
        self.requests: list[ModelRequest] = []
        self.cancelled: list[str] = []
        self._lock = RLock()

    def capabilities(self) -> ModelCapabilities:
        return self._capabilities

    def complete(self, request: ModelRequest) -> ModelResponse:
        with self._lock:
            self.requests.append(request)
            if not self._responses:
                raise RuntimeError("fake_provider_script_exhausted")
            scripted = self._responses.pop(0)
        if isinstance(scripted, BaseException):
            raise scripted
        if callable(scripted):
            scripted = scripted(request)
            if isinstance(scripted, BaseException):
                raise scripted
        if isinstance(scripted, Mapping):
            payload = dict(scripted)
            payload.setdefault("schema_version", 1)
            payload.setdefault("kind", "model_response")
            payload.setdefault("request_id", request.request_id)
            response = ModelResponse.from_dict(payload)
        else:
            response = scripted
        return replace(
            response,
            request_id=request.request_id,
            provider=response.provider or self.provider,
            model=response.model or request.model or self.model,
        )

    def stream(self, request: ModelRequest) -> Iterator[ModelStreamEvent]:
        with self._lock:
            self.requests.append(request)
            if not self._streams:
                raise RuntimeError("fake_provider_stream_script_exhausted")
            scripted = self._streams.pop(0)
        for index, item in enumerate(scripted.events):
            if scripted.error is not None and scripted.error_after == index:
                raise scripted.error
            if isinstance(item, ModelStreamEvent):
                yield replace(item, request_id=request.request_id)
            else:
                payload = dict(item)
                payload.setdefault("schema_version", 1)
                payload.setdefault("kind", "model_stream_event")
                payload.setdefault("request_id", request.request_id)
                payload.setdefault("sequence", index)
                yield ModelStreamEvent.from_dict(payload)
        if scripted.error is not None and scripted.error_after is None:
            raise scripted.error

    def cancel(self, request_id: str) -> bool:
        with self._lock:
            self.cancelled.append(request_id)
        return True
