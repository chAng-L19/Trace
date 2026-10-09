from __future__ import annotations

import hashlib
import http.client
import io
import ipaddress
import json
import math
import os
import re
import socket
import threading
import time
from collections.abc import Iterator, Mapping
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from contextlib import contextmanager
from typing import Any
from urllib.parse import urlsplit

from ..core import ModelCapabilities, ModelRequest, ModelResponse, ModelStreamEvent
from .opaque import chat_continuation
from .openai_protocol import request_payload, responses_response, stream_events, token_limit
from ..runtime.model_common import _utc_datetime


MAX_RESPONSE_BYTES = 8 * 1024 * 1024
MAX_ERROR_BYTES = 64 * 1024
_TOOL_NAME = re.compile(r"[^A-Za-z0-9_-]")


class ProviderHTTPError(RuntimeError):
    def __init__(self, status: int, code: str, message: str, *, retry_after: float = 0.0) -> None:
        self.status = int(status)
        self.code = str(code)
        self.retry_after = retry_after
        super().__init__(f"provider_http_error:{self.status}:{self.code}:{message}")


def retry_after_seconds(response: Any) -> float:
    """Accept numeric delays or HTTP dates; the recovery policy bounds waiting."""
    for name, scale in (("retry-after-ms", 0.001), ("retry-after", 1.0)):
        raw = response.getheader(name)
        if raw is None:
            continue
        try:
            delay = float(raw) * scale
        except (TypeError, ValueError):
            if scale != 1.0:
                continue
            try:
                when = parsedate_to_datetime(raw)
                when = when if when.tzinfo else when.replace(tzinfo=timezone.utc)
                delay = (when - datetime.now(timezone.utc)).total_seconds()
            except (TypeError, ValueError, OverflowError):
                continue
        if math.isfinite(delay) and delay >= 0:
            return delay
    return 0.0


@dataclass
class _ActiveRequest:
    connection: http.client.HTTPConnection
    socket: socket.socket | None = None
    cancelled: threading.Event = field(default_factory=threading.Event)


class _CancellableReader(io.RawIOBase):
    def __init__(self, transport: _CancellableSocket) -> None:
        self._transport = transport
        # Retain the socket's normal makefile reference while HTTP/1.0 closes
        # the connection. Read directly so polling timeouts do not poison SocketIO.
        self._reference = transport.socket.makefile("rb", buffering=0)

    def readable(self) -> bool:
        return True

    def readinto(self, buffer: Any) -> int:
        return self._transport.readinto(buffer)

    def close(self) -> None:
        try:
            self._reference.close()
        finally:
            super().close()


class _CancellableSocket:
    def __init__(self, transport: socket.socket, cancelled: threading.Event, deadline: float) -> None:
        self.socket = transport
        self._cancelled = cancelled
        self._deadline = deadline

    def _prepare(self) -> None:
        if self._cancelled.is_set():
            raise RuntimeError("provider_request_cancelled")
        remaining = self._deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("provider_request_timed_out")
        self.socket.settimeout(min(0.2, remaining))

    def readinto(self, buffer: Any) -> int:
        while True:
            self._prepare()
            try:
                return self.socket.recv_into(buffer)
            except TimeoutError:
                continue

    def sendall(self, data: bytes) -> None:
        remaining = memoryview(data)
        while remaining:
            self._prepare()
            try:
                sent = self.socket.send(remaining)
            except TimeoutError:
                continue
            if not sent:
                raise ConnectionError("provider_connection_closed")
            remaining = remaining[sent:]

    def makefile(self, mode: str) -> io.BufferedReader:
        if mode != "rb":
            raise ValueError("provider_response_stream_mode_invalid")
        return io.BufferedReader(_CancellableReader(self))

    def close(self) -> None:
        self.socket.close()


class OpenAICompatibleProvider:
    """Small production ModelPort for OpenAI-compatible chat-completions APIs."""

    def __init__(
        self,
        base_url: str,
        model: str,
        api_key: str = "",
        *,
        timeout_seconds: float = 120.0,
        max_context_tokens: int = 128_000,
        api_key_env: str = "",
        environ: Mapping[str, str] | None = None,
        max_output_tokens: int = 0,
        reasoning_effort: str = "",
    ) -> None:
        parsed = urlsplit(str(base_url).strip().rstrip("/"))
        if (
            parsed.scheme not in {"http", "https"}
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError("provider_base_url_invalid")
        if not str(model).strip():
            raise ValueError("provider_model_required")
        if not math.isfinite(float(timeout_seconds)) or float(timeout_seconds) <= 0:
            raise ValueError("provider_timeout_must_be_positive")
        if int(max_context_tokens) <= 0:
            raise ValueError("provider_context_tokens_must_be_positive")
        max_output_tokens = token_limit(max_output_tokens)
        if reasoning_effort not in {"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}:
            raise ValueError("provider_reasoning_effort_invalid")
        self._scheme = parsed.scheme
        self._host = parsed.hostname
        self._port = parsed.port
        base_path = parsed.path.rstrip("/")
        self._responses_api = base_path.endswith("/responses")
        self._path = (
            base_path
            if self._responses_api or base_path.endswith("/chat/completions")
            else f"{base_path}/chat/completions"
        ) or "/chat/completions"
        self.model = str(model).strip()
        self.max_output_tokens = int(max_output_tokens)
        self.reasoning_effort = reasoning_effort
        self._api_key = str(api_key)
        self._api_key_env = str(api_key_env)
        self._environ = os.environ if environ is None else environ
        self._continuation_base = str(base_url).rstrip("/")
        try:
            self._allow_no_key = ipaddress.ip_address(self._host).is_loopback
        except ValueError:
            self._allow_no_key = self._host.casefold() == "localhost"
        self._timeout = float(timeout_seconds)
        self._capabilities = ModelCapabilities(
            native_system_role=True,
            native_tool_calls=True,
            parallel_tool_calls=True,
            structured_output=True,
            streaming=True,
            usage_reporting=True,
            max_context_tokens=int(max_context_tokens),
            metadata={"provider": "openai-compatible", "model": self.model,
                      "opaque_continuation": True, "max_output_tokens": self.max_output_tokens},
        )
        self._active: dict[str, _ActiveRequest] = {}
        self._lock = threading.RLock()

    def capabilities(self) -> ModelCapabilities:
        return replace(self._capabilities, metadata={
            **self._capabilities.metadata,
            "continuation_scope": hashlib.sha256(
                (self._continuation_base + "\0" + self._credential()).encode()
            ).hexdigest(),
        })

    @property
    def ready(self) -> bool:
        return bool(self._credential() or self._allow_no_key)

    def _credential(self) -> str:
        return self._environ.get(self._api_key_env, "") or self._api_key

    def complete(self, request: ModelRequest) -> ModelResponse:
        payload, name_map = request_payload(self, request)
        with self._exchange(request, payload) as response:
            raw = response.read(MAX_RESPONSE_BYTES + 1)
            if len(raw) > MAX_RESPONSE_BYTES:
                raise RuntimeError("provider_response_too_large")
            document = self._document(raw)
            return (responses_response(self, request, document, name_map) if self._responses_api
                    else self._response(request, document, name_map))

    def stream(self, request: ModelRequest) -> Iterator[ModelStreamEvent]:
        payload, name_map = request_payload(self, request)
        payload["stream"] = True
        if not self._responses_api:
            payload["stream_options"] = {"include_usage": True}
        with self._exchange(request, payload) as response:
            if "text/event-stream" not in response.getheader("Content-Type", "").lower():
                raise RuntimeError("provider_stream_content_type_invalid")
            yield from stream_events(self, request, response, name_map, byte_limit=MAX_RESPONSE_BYTES)

    @contextmanager
    def _exchange(self, request: ModelRequest, payload: Mapping[str, Any]):
        credential = self._credential()
        if not credential and not self._allow_no_key:
            raise RuntimeError("missing_credentials")
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        connection = self._connection()
        active = _ActiveRequest(connection)
        deadline = time.monotonic() + self._timeout
        runtime_deadline = _utc_datetime(request.metadata.get("runtime_deadline"))
        if "runtime_deadline" in request.metadata and runtime_deadline is None:
            raise ValueError("provider_runtime_deadline_invalid")
        if runtime_deadline is not None:
            remaining = (runtime_deadline - datetime.now(timezone.utc)).total_seconds()
            if remaining <= 0:
                raise TimeoutError("provider_runtime_deadline_exhausted")
            deadline = min(deadline, time.monotonic() + remaining)
            connection.timeout = min(self._timeout, remaining)
        response: http.client.HTTPResponse | None = None
        with self._lock:
            if request.request_id in self._active:
                raise ValueError("provider_request_already_active")
            self._active[request.request_id] = active
        try:
            headers = self._headers(credential, streaming=bool(payload.get("stream")))
            if active.cancelled.is_set():
                raise RuntimeError("provider_request_cancelled")
            self._connect(connection, active, deadline)
            connection.sock = _CancellableSocket(
                active.socket, active.cancelled, deadline
            )
            connection.request("POST", self._path, body=body, headers=headers)
            response = connection.getresponse()
            if not 200 <= response.status < 300:
                raw = response.read(MAX_ERROR_BYTES + 1)
                if len(raw) > MAX_ERROR_BYTES:
                    raise RuntimeError("provider_response_too_large")
                try:
                    error_document = self._document(raw)
                except RuntimeError:
                    error_document = {}
                code, message = self._error(error_document, raw, credential=credential)
                raise ProviderHTTPError(response.status, code, message,
                                        retry_after=retry_after_seconds(response))
            yield response
            if active.cancelled.is_set():
                raise RuntimeError("provider_request_cancelled")
        except Exception:
            if active.cancelled.is_set():
                raise RuntimeError("provider_request_cancelled") from None
            raise
        finally:
            if response is not None:
                response.close()
            connection.close()
            with self._lock:
                if self._active.get(request.request_id) is active:
                    self._active.pop(request.request_id, None)

    def _headers(self, credential: str, *, streaming: bool) -> dict[str, str]:
        headers = {"Content-Type": "application/json", "Accept": "text/event-stream" if streaming else "application/json"}
        if credential:
            headers["Authorization"] = f"Bearer {credential}"
        return headers

    def cancel(self, request_id: str) -> bool:
        with self._lock:
            active = self._active.get(str(request_id))
            if active is None or active.cancelled.is_set():
                return False
            active.cancelled.set()
            transport = active.socket or active.connection.sock
        if transport is not None:
            try:
                transport.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                transport.close()
            except OSError:
                pass
        # The request thread owns close(); cross-thread close() can block on
        # the buffered response reader, and close alone does not interrupt it.
        return True

    def _connection(self) -> http.client.HTTPConnection:
        connection_type = (
            http.client.HTTPSConnection if self._scheme == "https" else http.client.HTTPConnection
        )
        return connection_type(self._host, self._port, timeout=self._timeout)

    def _connect(
        self,
        connection: http.client.HTTPConnection,
        active: _ActiveRequest,
        deadline: float,
    ) -> None:
        if isinstance(connection, http.client.HTTPSConnection):
            # HTTPSConnection.connect() performs a blocking TLS handshake before
            # exposing its socket. Split those steps so cancel can close it.
            http.client.HTTPConnection.connect(connection)
            with self._lock:
                active.socket = connection.sock
                if active.cancelled.is_set():
                    raise RuntimeError("provider_request_cancelled")
                server_hostname = connection._tunnel_host or connection.host
                secure = connection._context.wrap_socket(
                    active.socket,
                    server_hostname=server_hostname,
                    do_handshake_on_connect=False,
                )
                connection.sock = secure
                active.socket = secure
            while True:
                if active.cancelled.is_set():
                    raise RuntimeError("provider_request_cancelled")
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("provider_request_timed_out")
                secure.settimeout(min(0.2, remaining))
                try:
                    secure.do_handshake()
                    break
                except TimeoutError:
                    continue
        else:
            connection.connect()
            with self._lock:
                active.socket = connection.sock
        if active.cancelled.is_set():
            raise RuntimeError("provider_request_cancelled")

    @classmethod
    def _message(
        cls,
        message: Mapping[str, Any],
        encoded_names: Mapping[str, str],
    ) -> dict[str, Any]:
        role = str(message.get("role") or "user")
        content = message.get("content", "")
        if role == "assistant":
            ledger_content = content if isinstance(content, Mapping) else None
            raw_calls = (
                ledger_content.get("tool_calls")
                if ledger_content is not None
                else message.get("tool_calls")
            )
            projected: dict[str, Any] = {
                "role": role,
                "content": (
                    cls._assistant_text(ledger_content)
                    if ledger_content is not None
                    else cls._message_content(content)
                ),
            }
            if isinstance(raw_calls, list) and raw_calls:
                projected["tool_calls"] = [
                    cls._assistant_tool_call(item, index, encoded_names)
                    for index, item in enumerate(raw_calls)
                    if isinstance(item, Mapping)
                ]
            if projected.get("tool_calls") or ledger_content is not None:
                return projected
        projected: dict[str, Any] = {
            "role": role,
            "content": cls._message_content(content),
        }
        for key in ("name", "tool_call_id"):
            value = message.get(key)
            if value:
                projected[key] = str(value)
        if role == "tool" and "tool_call_id" not in projected:
            if isinstance(content, Mapping) and content.get("call_id"):
                projected["tool_call_id"] = str(content["call_id"])
            else:
                raise ValueError("provider_tool_message_call_id_required")
        return projected

    @staticmethod
    def _message_content(content: Any) -> Any:
        if content is None or isinstance(content, str):
            return content
        if isinstance(content, list):
            return content
        return json.dumps(content, ensure_ascii=False, separators=(",", ":"), default=str)

    @staticmethod
    def _assistant_text(content: Mapping[str, Any]) -> str | None:
        text = content.get("text")
        if isinstance(text, str) and text:
            return text
        structured = content.get("structured_output")
        if isinstance(structured, Mapping) and structured:
            return json.dumps(structured, ensure_ascii=False, separators=(",", ":"))
        return None

    @classmethod
    def _assistant_tool_call(
        cls,
        item: Mapping[str, Any],
        index: int,
        encoded_names: Mapping[str, str],
    ) -> dict[str, Any]:
        function = item.get("function") if isinstance(item.get("function"), Mapping) else {}
        name = str(
            item.get("tool_name")
            or item.get("name")
            or item.get("tool")
            or function.get("name")
            or ""
        ).strip()
        if not name:
            raise ValueError("provider_tool_name_required")
        arguments = item.get("arguments", function.get("arguments", {})) or {}
        if isinstance(arguments, str):
            try:
                parsed = json.loads(arguments)
            except json.JSONDecodeError as exc:
                raise ValueError("provider_tool_arguments_invalid_json") from exc
            if not isinstance(parsed, Mapping):
                raise TypeError("provider_tool_arguments_must_be_object")
            encoded_arguments = arguments
        elif isinstance(arguments, Mapping):
            encoded_arguments = json.dumps(
                arguments, ensure_ascii=False, separators=(",", ":")
            )
        else:
            raise TypeError("provider_tool_arguments_must_be_object")
        return {
            "id": str(item.get("call_id") or item.get("id") or f"call-{index}"),
            "type": "function",
            "function": {
                "name": encoded_names.get(name) or cls._encoded_tool_name(name),
                "arguments": encoded_arguments,
            },
        }

    @classmethod
    def _tool(cls, tool: Mapping[str, Any], name_map: dict[str, str]) -> dict[str, Any]:
        original = str(tool.get("name") or tool.get("qualified_name") or "").strip()
        if not original:
            raise ValueError("provider_tool_name_required")
        safe = cls._encoded_tool_name(original)
        if safe in name_map and name_map[safe] != original:
            raise ValueError("provider_tool_name_collision")
        name_map[safe] = original
        schema = tool.get("input_schema") or tool.get("parameters") or {}
        if not isinstance(schema, Mapping):
            raise TypeError("provider_tool_schema_must_be_object")
        return {
            "type": "function",
            "function": {
                "name": safe,
                "description": str(tool.get("description") or "")[:1024],
                "parameters": dict(schema),
            },
        }

    @staticmethod
    def _encoded_tool_name(original: str) -> str:
        safe = _TOOL_NAME.sub("_", original)[:64]
        if safe != original:
            suffix = hashlib.sha256(original.encode("utf-8")).hexdigest()[:8]
            safe = f"{safe[:55]}_{suffix}"
        return safe

    @staticmethod
    def _document(raw: bytes) -> Mapping[str, Any]:
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise RuntimeError("provider_response_not_json") from exc
        if not isinstance(value, Mapping):
            raise RuntimeError("provider_response_must_be_object")
        return value

    def _error(self, document: Mapping[str, Any], raw: bytes, *, credential: str = "") -> tuple[str, str]:
        value = document.get("error")
        if isinstance(value, Mapping):
            code = str(value.get("code") or value.get("type") or "error")
            message = str(value.get("message") or "provider request failed")
        else:
            code, message = "error", raw.decode("utf-8", errors="replace")
        # Redact before truncation; a key crossing the boundary otherwise leaks
        # its prefix. Some gateways echo credentials in code as well as message.
        for candidate in {credential, self._credential(), self._api_key} - {""}:
            for secret in (candidate, json.dumps(candidate)[1:-1]):
                code = code.replace(secret, "[REDACTED]")
                message = message.replace(secret, "[REDACTED]")
        return code[:256], message[:2048]

    @classmethod
    def _response(
        cls,
        request: ModelRequest,
        document: Mapping[str, Any],
        name_map: Mapping[str, str],
    ) -> ModelResponse:
        choices = document.get("choices")
        if not isinstance(choices, list) or not choices or not isinstance(choices[0], Mapping):
            raise RuntimeError("provider_response_choice_missing")
        choice = choices[0]
        message = choice.get("message")
        if not isinstance(message, Mapping):
            raise RuntimeError("provider_response_message_missing")
        refusal = cls._text(message.get("refusal"))
        text = cls._text(message.get("content")) or refusal
        structured: Mapping[str, Any] = {}
        if text and not refusal:
            try:
                parsed = json.loads(text)
            except json.JSONDecodeError:
                parsed = None
            if isinstance(parsed, Mapping):
                structured = dict(parsed)
        finish_reason = str(choice.get("finish_reason") or "")
        status = "completed"
        error = ""
        if finish_reason in {"length", "max_tokens"}:
            status, error = "interrupted", f"finish_reason:{finish_reason}"
        elif finish_reason not in {"", "stop", "tool_calls", "function_call"}:
            status, error = "failed", f"finish_reason:{finish_reason}"
        calls = () if status != "completed" else cls._tool_calls(message.get("tool_calls"), name_map)
        usage = cls._usage(document.get("usage"))
        return ModelResponse(
            request_id=request.request_id,
            status=status,
            provider="openai-compatible",
            model=str(document.get("model") or request.model or ""),
            text=text,
            structured_output=structured,
            tool_calls=calls,
            usage=usage,
            finish_reason=finish_reason,
            error=error,
            metadata={"provider_response_id": str(document.get("id") or ""),
                      **({"refusal": True, "refusal_text": refusal,
                          "response_category": "refusal"} if refusal else {})},
            response_id=str(document.get("id") or ""),
            continuation={"assistant": opaque, "assistant_text_hash": hashlib.sha256(text.encode()).hexdigest(),
                          "assistant_call_ids": [str(call.get("id") or "") for call in message.get("tool_calls") or ()]}
                         if (opaque := chat_continuation(message)) else {},
        )

    @staticmethod
    def _text(content: Any) -> str:
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return "".join(
                str(item.get("text") or "")
                for item in content
                if isinstance(item, Mapping) and item.get("type") in {None, "text"}
            )
        return "" if content is None else json.dumps(content, ensure_ascii=False, default=str)

    @staticmethod
    def _tool_calls(value: Any, name_map: Mapping[str, str]) -> tuple[Mapping[str, Any], ...]:
        if value is None:
            return ()
        if not isinstance(value, list):
            raise RuntimeError("provider_tool_calls_must_be_array")
        calls: list[Mapping[str, Any]] = []
        for index, item in enumerate(value):
            if not isinstance(item, Mapping) or not isinstance(item.get("function"), Mapping):
                raise RuntimeError("provider_tool_call_invalid")
            function = item["function"]
            encoded = str(function.get("name") or "")
            if encoded not in name_map:
                raise RuntimeError("provider_tool_call_unknown")
            raw_arguments = function.get("arguments") or "{}"
            try:
                arguments = (
                    json.loads(raw_arguments) if isinstance(raw_arguments, str) else raw_arguments
                )
            except json.JSONDecodeError as exc:
                raise RuntimeError("provider_tool_arguments_invalid_json") from exc
            if not isinstance(arguments, Mapping):
                raise RuntimeError("provider_tool_arguments_must_be_object")
            calls.append(
                {
                    "call_id": str(item.get("id") or f"call-{index}"),
                    "tool_name": name_map[encoded],
                    "arguments": dict(arguments),
                }
            )
        return tuple(calls)

    @staticmethod
    def _usage(value: Any) -> Mapping[str, Any]:
        if value is None:
            return {}
        if not isinstance(value, Mapping):
            raise RuntimeError("provider_usage_invalid")
        usage: dict[str, Any] = {}
        aliases = {
            "prompt_tokens": "input_tokens",
            "completion_tokens": "output_tokens",
            "total_tokens": "total_tokens",
        }
        for source, target in aliases.items():
            if source in value:
                usage[target] = value[source]
        for field, source, target in (("prompt_tokens_details", "cached_tokens", "cache_read_tokens"),
                                      ("completion_tokens_details", "reasoning_tokens", "reasoning_tokens")):
            details = value.get(field)
            if details is None:
                continue
            if not isinstance(details, Mapping):
                raise RuntimeError("provider_usage_invalid")
            if source in details:
                usage[target] = details[source]
        for number in usage.values():
            if isinstance(number, bool) or not isinstance(number, int) or not 0 <= number <= 2**63 - 1:
                raise RuntimeError("provider_usage_invalid")
        return usage


__all__ = ["MAX_ERROR_BYTES", "MAX_RESPONSE_BYTES", "OpenAICompatibleProvider", "ProviderHTTPError"]
