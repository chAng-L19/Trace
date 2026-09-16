from __future__ import annotations

import ipaddress
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib.resources import files
from typing import Any, Mapping
from urllib.parse import parse_qs, unquote, urlsplit

from .web import (
    MAX_REQUEST_BYTES,
    _SECURITY_HEADERS as SECURITY_HEADERS,
    _STATIC_FILES as STATIC_FILES,
    WEB_SCHEMA_VERSION,
    WebApi,
    WebResponse,
)


class TraceRequestHandler(BaseHTTPRequestHandler):
    server_version = "TraceWeb/1"

    @property
    def api(self) -> WebApi:
        return self.server.api  # type: ignore[attr-defined]

    def _request_boundary(self) -> WebResponse | None:
        try:
            host = urlsplit(f"http://{self.headers.get('Host', '')}")
            hostname = host.hostname or ""
            if host.username is not None or host.password is not None or host.path or host.query or host.fragment:
                return self.api._error(400, "invalid_host")
            bound_host, bound_port = self.server.server_address[:2]
            if ipaddress.ip_address(bound_host).is_loopback:
                if hostname.casefold() not in {"localhost", "127.0.0.1", "::1"} or (host.port or 80) != bound_port:
                    return self.api._error(403, "host_not_allowed")
            origin_value = self.headers.get("Origin")
            if origin_value:
                origin = urlsplit(origin_value)
                if (
                    origin.scheme not in {"http", "https"}
                    or (origin.hostname or "").casefold() != hostname.casefold()
                    or (origin.port or 80) != (host.port or 80)
                    or origin.username is not None
                    or origin.password is not None
                    or origin.path
                    or origin.query
                    or origin.fragment
                ):
                    return self.api._error(403, "origin_not_allowed")
        except ValueError:
            return self.api._error(400, "invalid_request_origin")
        return None

    def _body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0") or 0)
        if length < 0:
            raise ValueError("invalid_content_length")
        if length > MAX_REQUEST_BYTES:
            raise ValueError("request_body_too_large")
        if length <= 0:
            return {}
        payload = json.loads(self.rfile.read(length).decode("utf-8"))
        if not isinstance(payload, Mapping):
            raise TypeError("request_body_must_be_object")
        return dict(payload)

    def _write(self, response: WebResponse) -> None:
        self.send_response(response.status)
        self.send_header("Content-Type", response.content_type)
        self.send_header("Content-Length", str(len(response.body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Trace-Schema-Version", str(WEB_SCHEMA_VERSION))
        for name, value in SECURITY_HEADERS.items():
            self.send_header(name, value)
        for name, value in response.headers.items():
            self.send_header(str(name), str(value))
        self.end_headers()
        self.wfile.write(response.body)

    def _reject_post(self, response: WebResponse) -> None:
        try:
            length = int(self.headers.get("Content-Length", "0") or 0)
        except ValueError:
            length = 0
        if 0 < length <= MAX_REQUEST_BYTES:
            previous_timeout = self.connection.gettimeout()
            try:
                self.connection.settimeout(5.0)
                self.rfile.read(length)
            except OSError:
                pass
            finally:
                self.connection.settimeout(previous_timeout)
        self._write(response)

    def do_GET(self) -> None:  # noqa: N802
        if (error := self._request_boundary()) is not None:
            self._write(error)
            return
        parsed = urlsplit(self.path)
        static_file = STATIC_FILES.get(parsed.path)
        if static_file is not None:
            try:
                name, content_type = static_file
                body = files("redteam_agent").joinpath("static", name).read_bytes()
            except (FileNotFoundError, OSError):
                self._write(self.api._error(404, "static_resource_not_found"))
                return
            self._write(WebResponse(200, body, content_type))
            return
        if "text/event-stream" in self.headers.get("Accept", "").casefold() and parsed.path.startswith("/api/runs/") and parsed.path.endswith("/events"):
            self._write_sse(parsed)
            return
        self._write(self.api.dispatch("GET", self.path, headers=self.headers))

    def do_POST(self) -> None:  # noqa: N802
        if (error := self._request_boundary()) is not None:
            self._reject_post(error)
            return
        if self.headers.get_content_type() != "application/json":
            self._reject_post(self.api._error(415, "application_json_required"))
            return
        try:
            headers = {str(key).casefold(): str(value) for key, value in self.headers.items()}
            headers["x-trace-client"] = str(self.client_address[0])
            self._write(self.api.dispatch("POST", self.path, body=self._body(), headers=headers))
        except (ValueError, TypeError, json.JSONDecodeError) as exc:
            self._write(self.api._error(400, str(exc)))

    def do_DELETE(self) -> None:  # noqa: N802
        if (error := self._request_boundary()) is not None:
            self._reject_post(error)
            return
        try:
            headers = {str(key).casefold(): str(value) for key, value in self.headers.items()}
            headers["x-trace-client"] = str(self.client_address[0])
            self._write(self.api.dispatch("DELETE", self.path, body=self._body(), headers=headers))
        except (ValueError, TypeError, json.JSONDecodeError) as exc:
            self._write(self.api._error(400, str(exc)))

    def _unsupported_method(self) -> None:
        if (error := self._request_boundary()) is not None:
            self._reject_post(error)
            return
        self._reject_post(self.api._error(405, "method_not_allowed"))

    def do_PATCH(self) -> None:  # noqa: N802
        self._unsupported_method()

    def do_PUT(self) -> None:  # noqa: N802
        self._unsupported_method()

    def do_OPTIONS(self) -> None:  # noqa: N802
        self._unsupported_method()

    def _write_sse(self, parsed: Any) -> None:
        if (self.api.control.auth_required or self.api.force_auth) and not self.api.control.authenticated(
            {str(key).casefold(): str(value) for key, value in self.headers.items()}, force=self.api.force_auth
        ):
            self._write(self.api._error(401, "authentication_required"))
            return
        segments = [unquote(item) for item in parsed.path.split("/") if item]
        if len(segments) != 4 or segments[:2] != ["api", "runs"] or segments[3] != "events":
            self._write(self.api._error(404, "route_not_found"))
            return
        query = {key: values[-1] for key, values in parse_qs(parsed.query).items() if values}
        headers_sent = False
        try:
            if self.headers.get("Last-Event-ID"):
                query["after_sequence"] = self.headers.get("Last-Event-ID", "0")
            after = self.api._int_query(query, "after_sequence", 0, 0, 2**63 - 1)
            events = self.api.sse_events(segments[2], after_sequence=after, wait_seconds=float(query.get("wait_seconds", "0") or 0))
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.send_header("X-Trace-Schema-Version", str(WEB_SCHEMA_VERSION))
            for name, value in SECURITY_HEADERS.items():
                self.send_header(name, value)
            self.end_headers()
            headers_sent = True
            event_name = "trace-event" if query.get("channel") == "ui" else ""
            for event in events:
                event_type = event_name or str(event["event_type"]).replace("\r", "").replace("\n", "")
                self.wfile.write((f"id: {event['sequence']}\n" f"event: {event_type}\n" f"data: {json.dumps(event, ensure_ascii=False, sort_keys=True, default=str)}\n\n").encode("utf-8"))
            if not events:
                self.wfile.write(b": keep-alive\n\n")
            self.wfile.flush()
        except Exception as exc:
            if headers_sent:
                try:
                    # The response headers are already committed, so the
                    # client receives an SSE error event. Keep that event
                    # bounded and redact exception text, which may contain
                    # filesystem paths, request data or provider details.
                    error_payload = {"error": f"sse_error:{type(exc).__name__}"}
                    self.wfile.write(("event: error\n" f"data: {json.dumps(error_payload, ensure_ascii=False)}\n\n").encode("utf-8"))
                    self.wfile.flush()
                except OSError:
                    return
            elif isinstance(exc, KeyError):
                self._write(self.api._error(404, str(exc)))
            elif isinstance(exc, (ValueError, TypeError)):
                self._write(self.api._error(400, str(exc)))
            else:
                self._write(self.api._error(500, f"internal_error:{type(exc).__name__}"))

    def log_message(self, format: str, *args: Any) -> None:
        return


class TraceHTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], api: WebApi) -> None:
        super().__init__(address, TraceRequestHandler)
        self.api = api


__all__ = ["TraceHTTPServer"]
