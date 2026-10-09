from __future__ import annotations

from collections.abc import Iterator, Mapping
from dataclasses import replace
from urllib.parse import urlsplit

from ..core import ModelRequest, ModelResponse, ModelStreamEvent
from .anthropic_protocol import request_payload, message_response, stream_events
from .openai_compatible import MAX_RESPONSE_BYTES, OpenAICompatibleProvider
from .openai_protocol import token_limit


class AnthropicProvider(OpenAICompatibleProvider):
    """Native Messages protocol over the shared bounded, cancellable transport."""

    def __init__(self, base_url: str, model: str, api_key: str = "", *,
                 timeout_seconds: float = 120.0, max_context_tokens: int = 128_000,
                 api_key_env: str = "", environ: Mapping[str, str] | None = None,
                 max_output_tokens: int = 0, reasoning_effort: str = "",
                 thinking_type: str = "", thinking_budget_tokens: int = 0) -> None:
        super().__init__(base_url, model, api_key, timeout_seconds=timeout_seconds,
                         max_context_tokens=max_context_tokens, api_key_env=api_key_env, environ=environ,
                         max_output_tokens=max_output_tokens, reasoning_effort=reasoning_effort)
        path = urlsplit(base_url.strip().rstrip("/")).path.rstrip("/")
        self._path = path if path.endswith("/messages") else f"{path}/messages" if path else "/v1/messages"
        self._responses_api = False
        self._continuation_base = "anthropic\0" + self._continuation_base
        self.max_output_tokens = int(max_output_tokens)
        self.reasoning_effort = str(reasoning_effort).strip().lower()
        self.thinking_type = str(thinking_type).strip().lower()
        self.thinking_budget_tokens = token_limit(thinking_budget_tokens)
        if self.max_output_tokens < 0 or self.thinking_budget_tokens < 0:
            raise ValueError("provider_token_limit_invalid")
        if self.reasoning_effort not in {"", "low", "medium", "high", "xhigh", "max"}:
            raise ValueError("provider_reasoning_effort_invalid")
        if self.thinking_type not in {"", "enabled", "disabled", "adaptive"}:
            raise ValueError("provider_thinking_type_invalid")
        if self.thinking_type == "enabled" and self.thinking_budget_tokens < 1024:
            raise ValueError("provider_thinking_budget_too_small")
        if self.thinking_type != "enabled" and self.thinking_budget_tokens:
            raise ValueError("provider_thinking_budget_requires_enabled")
        if self.max_output_tokens and self.thinking_budget_tokens >= self.max_output_tokens:
            raise ValueError("provider_thinking_budget_exceeds_output")
        self._capabilities = replace(self._capabilities, structured_output=False,
            metadata={"provider": "anthropic", "model": self.model, "opaque_continuation": True,
                      "max_output_tokens": self.max_output_tokens})

    def _headers(self, credential: str, *, streaming: bool) -> dict[str, str]:
        headers = {"Content-Type": "application/json", "anthropic-version": "2023-06-01",
                   "Accept": "text/event-stream" if streaming else "application/json"}
        if credential:
            headers["x-api-key"] = credential
        return headers

    def complete(self, request: ModelRequest) -> ModelResponse:
        payload, names = request_payload(self, request)
        with self._exchange(request, payload) as response:
            raw = response.read(MAX_RESPONSE_BYTES + 1)
            if len(raw) > MAX_RESPONSE_BYTES:
                raise RuntimeError("provider_response_too_large")
            return message_response(self, request, self._document(raw), names)

    def stream(self, request: ModelRequest) -> Iterator[ModelStreamEvent]:
        payload, names = request_payload(self, request)
        payload["stream"] = True
        with self._exchange(request, payload) as response:
            if "text/event-stream" not in response.getheader("Content-Type", "").lower():
                raise RuntimeError("provider_stream_content_type_invalid")
            yield from stream_events(self, request, response, names, byte_limit=MAX_RESPONSE_BYTES)
