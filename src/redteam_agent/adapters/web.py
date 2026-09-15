from __future__ import annotations

import argparse
import ipaddress
import json
import math
import os
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from importlib.resources import files
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import parse_qs, unquote, urlsplit
from uuid import uuid4

from ..application import AgentService
from ..application.contracts import BudgetDelta
from ..core import ModelPort, contract_hash
from ..providers import OpenAICompatibleProvider
from ..runtime.store_common import ImmutableRecordError, StoreConflictError
from .web_control import ControlPlane
from .web_routes import ControlRoutesMixin

WEB_SCHEMA_VERSION = 1
MAX_REQUEST_BYTES = 2 * 1024 * 1024
DEFAULT_EVENT_LIMIT = 200
MAX_EVENT_PAYLOAD_BYTES = 16 * 1024
_RAW_EVENT_KEYS = frozenset({"state_snapshot", "payload", "output", "response", "request", "result", "stdout", "stderr"})
_STATIC_FILES = {
    "/": ("index.html", "text/html; charset=utf-8"),
    "/index.html": ("index.html", "text/html; charset=utf-8"),
    "/app.css": ("app.css", "text/css; charset=utf-8"),
    "/app.js": ("app.js", "text/javascript; charset=utf-8"),
}
_SECURITY_HEADERS = {
    "Content-Security-Policy": (
        "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "
        "connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; "
        "frame-ancestors 'none'"
    ),
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
}

def _event_value(value: Any, *, depth: int = 0) -> Any:
    """Project metadata recursively; preserve full content behind its hash."""

    def reference() -> dict[str, Any]:
        return {"omitted": True, "content_hash": contract_hash(value)}

    if depth > 4:
        return reference()
    if isinstance(value, Mapping):
        if len(value) > 64 or any(len(str(key)) > 256 for key in value):
            return reference()
        result = {
            str(key): _event_value(item, depth=depth + 1)
            for key, item in value.items()
            if str(key).casefold() not in _RAW_EVENT_KEYS
        }
    elif isinstance(value, (list, tuple)):
        if len(value) > 32:
            return reference()
        result = [_event_value(item, depth=depth + 1) for item in value]
    elif isinstance(value, str) and len(value.encode("utf-8")) > 2048:
        return reference()
    else:
        result = value
    if len(json.dumps(result, ensure_ascii=False, default=str).encode("utf-8")) > MAX_EVENT_PAYLOAD_BYTES:
        return reference()
    return result

def _jsonable(value: Any) -> Any:
    if hasattr(value, "to_dict"):
        return value.to_dict()
    if isinstance(value, Mapping):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value

@dataclass(frozen=True, slots=True)
class WebResponse:
    status: int
    body: bytes
    content_type: str = "application/json; charset=utf-8"
    headers: Mapping[str, str] = field(default_factory=dict)

    @classmethod
    def json(
        cls,
        payload: Mapping[str, Any],
        *,
        status: int = 200,
        headers: Mapping[str, str] | None = None,
    ) -> "WebResponse":
        return cls(
            status=status,
            body=json.dumps(
                _jsonable(dict(payload)),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                default=str,
            ).encode("utf-8"),
            headers=headers or {},
        )

    def payload(self) -> Mapping[str, Any]:
        value = json.loads(self.body.decode("utf-8"))
        return value if isinstance(value, Mapping) else {"value": value}

class WebApi(ControlRoutesMixin):
    def __init__(self, service: AgentService, *, command_ttl_seconds: float = 30.0) -> None:
        self.service = service
        self.command_ttl_seconds = max(1.0, float(command_ttl_seconds))
        self.owner = f"web-{uuid4().hex}"
        self.control = ControlPlane(service.runtime.store, service.runtime.root)
        self.force_auth = False
        self.tls_enabled = False
        self.service.runtime.broker.set_secret_bindings(self.control.mcp_secret_bindings())
        self._load_active_provider()

    def dispatch(
        self,
        method: str,
        path: str,
        *,
        body: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> WebResponse:
        method = str(method or "GET").upper()
        parsed = urlsplit(path)
        segments = [unquote(item) for item in parsed.path.split("/") if item]
        query = {key: values[-1] for key, values in parse_qs(parsed.query).items() if values}
        payload = dict(body or {})
        request_headers = {str(key).casefold(): str(value) for key, value in (headers or {}).items()}
        if segments[:2] == ["api", "auth"]:
            return self._auth(method, segments[2:], payload, request_headers)
        if (self.control.auth_required or self.force_auth) and segments[:2] != ["api", "auth"]:
            if not self.control.authenticated(request_headers, force=self.force_auth):
                return self._error(401, "authentication_required")
        if segments == ["api", "system"]:
            if method != "GET":
                return self._error(405, "method_not_allowed")
            return self._ok(self.system_info())
        if segments[:2] == ["api", "providers"]:
            return self._safe_control(method, "providers", segments[2:], payload)
        if segments[:2] == ["api", "skills"]:
            return self._safe_control(method, "skills", segments[2:], payload)
        if segments[:2] == ["api", "mcp"]:
            return self._safe_control(method, "mcp", segments[2:], payload)
        if segments[:2] == ["api", "conversations"]:
            return self._safe_control(method, "conversations", segments[2:], payload)
        if segments == ["api", "system", "reload"]:
            if method != "POST":
                return self._error(405, "method_not_allowed")
            return self._ok(self._reload_control_plane())
        if segments[:2] != ["api", "runs"]:
            return self._error(404, "route_not_found")
        try:
            if method == "GET":
                return self._get(segments[2:], query)
            if method == "POST":
                return self._post(segments[2:], payload, request_headers)
            return self._error(405, "method_not_allowed")
        except KeyError as exc:
            return self._error(404, str(exc))
        except (ImmutableRecordError, StoreConflictError) as exc:
            return self._error(409, str(exc))
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            return self._error(400, str(exc))
        except Exception as exc:  # pragma: no cover - defensive protocol boundary
            return self._error(500, f"internal_error:{type(exc).__name__}")

    def _get(self, tail: list[str], query: Mapping[str, str]) -> WebResponse:
        if not tail:
            views = self.service.list_runs(
                limit=self._int_query(query, "limit", 100, 1, 1000),
                offset=self._int_query(query, "offset", 0, 0, 1000000),
                status=str(query.get("status") or ""),
            )
            return self._ok({"runs": [self._run_projection(view) for view in views]})
        run_id = tail[0]
        if len(tail) == 1:
            return self._ok({"run": self._run_projection(self.service.status(run_id))})
        resource = tail[1]
        if resource == "events":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            events = self.service.events(
                run_id,
                after_sequence=self._int_query(query, "after_sequence", 0, 0, 2**63 - 1),
                limit=self._int_query(query, "limit", DEFAULT_EVENT_LIMIT, 1, 1000),
            )
            items = [
                self._event_projection(item, include_payload=query.get("include_payload") == "1")
                for item in events
            ]
            return self._ok(
                {
                    "run_id": run_id,
                    "events": items,
                    "next_sequence": items[-1]["sequence"] if items else None,
                }
            )
        if resource == "search-graph":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok({"run_id": run_id, "search_graph": _jsonable(self.service.exploration_state(run_id))})
        if resource == "evidence-graph":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok(self._evidence_graph(
                run_id,
                include_payload=query.get("include_payload") == "1",
                limit=self._int_query(query, "limit", 1000, 1, 10000),
                offset=self._int_query(query, "offset", 0, 0, 1_000_000),
            ))
        if resource == "transparency":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok(self.service.inspect_session(run_id, event_limit=self._int_query(query, "limit", 1000, 1, 10000)))
        if resource == "tools":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok(self.service.explain_tool_visibility(run_id, tool_name=str(query.get("tool") or "")))
        if resource == "transcript":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            offset = self._int_query(query, "offset", 0, 0, 1_000_000)
            limit = self._int_query(query, "limit", 200, 1, 1000)
            messages = self.service.transcript(run_id)
            page = messages[offset : offset + limit]
            return self._ok({"run_id": run_id, "messages": [_jsonable(item) for item in page], "truncated": offset + limit < len(messages)})
        if resource == "artifacts":
            if len(tail) == 2:
                offset = self._int_query(query, "offset", 0, 0, 1_000_000)
                limit = self._int_query(query, "limit", 200, 1, 1000)
                artifacts = self.service.artifacts(run_id)
                page = artifacts[offset : offset + limit]
                return self._ok({"run_id": run_id, "artifacts": [_jsonable(item) for item in page], "truncated": offset + limit < len(artifacts)})
            artifact_id = tail[2]
            if len(tail) == 4 and tail[3] == "content":
                return WebResponse(200, self.service.read_artifact(run_id, artifact_id), "application/octet-stream")
            if len(tail) != 3:
                return self._error(404, "resource_not_found")
            return self._ok({"artifact": _jsonable(self.service.artifact(run_id, artifact_id))})
        if resource == "report":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            projection = self.service.inspect_session(run_id, event_limit=self._int_query(query, "limit", 1000, 1, 10000))
            return self._ok(
                {
                    "run": projection.get("run", {}),
                    "terminal": projection.get("terminal", {}),
                    "evidence": projection.get("evidence", {}),
                    "artifacts": projection.get("artifacts", []),
                    "report_hash": projection.get("report_hash", ""),
                }
            )
        return self._error(404, "resource_not_found")

    def _post(
        self,
        tail: list[str],
        body: Mapping[str, Any],
        headers: Mapping[str, str],
    ) -> WebResponse:
        command_id = str(headers.get("x-command-id") or body.get("command_id") or "").strip()
        command_body = {key: value for key, value in body.items() if key != "command_id"}
        request_hash = contract_hash({"route": tail, "body": command_body})
        run_id = tail[0] if tail else ""
        claim_owner = f"{self.owner}:{uuid4().hex}"
        fencing_token = 0
        if command_id:
            receipt = self.service.runtime.store.claim_web_command(
                command_id,
                request_hash,
                owner=claim_owner,
                run_id=run_id,
                ttl_seconds=self.command_ttl_seconds,
            )
            if receipt["status"] == "completed":
                saved = receipt["response"]
                if isinstance(saved.get("payload"), Mapping):
                    return WebResponse.json(saved["payload"], status=int(saved.get("status", 200)))
                return WebResponse.json(saved, status=200)
            if not receipt.get("claimed") or receipt["owner"] != claim_owner or receipt["status"] != "pending":
                return self._error(409, "command_in_progress")
            fencing_token = int(receipt["fencing_token"])
            if receipt.get("reclaimed") and self._command_replay_is_uncertain(tail):
                response = self._error(409, "command_result_uncertain")
                self.service.runtime.store.complete_web_command(
                    command_id,
                    {"status": response.status, "payload": response.payload()},
                    owner=claim_owner,
                    fencing_token=fencing_token,
                    run_id=run_id,
                )
                return response
        try:
            response = self._post_once(tail, command_body, command_id=command_id)
        except Exception:
            raise
        if command_id and response.status < 500:
            response_payload = response.payload()
            response_run = response_payload.get("run", {})
            if not isinstance(response_run, Mapping):
                response_run = {}
            runs = response_payload.get("runs", [])
            first_run = runs[0].get("run", {}) if isinstance(runs, list) and runs and isinstance(runs[0], Mapping) else {}
            saved_run_id = response_run.get("run_id") or (first_run.get("run_id") if isinstance(first_run, Mapping) else "")
            self.service.runtime.store.complete_web_command(
                command_id,
                {"status": response.status, "payload": response_payload},
                owner=claim_owner,
                fencing_token=fencing_token,
                run_id=run_id or str(saved_run_id or ""),
            )
        return response

    def _post_once(
        self,
        tail: list[str],
        body: Mapping[str, Any],
        *,
        command_id: str = "",
    ) -> WebResponse:
        if not tail:
            if not body:
                return self._error(400, "start_request_required")
            started = self.service.start(body)
            return self._ok(started.to_dict(), status=201)
        run_id = tail[0]
        if len(tail) > 2:
            return self._error(404, "command_not_found")
        command = tail[1] if len(tail) > 1 else "run"
        if command == "run":
            budget_delta = self._command_budget_delta(body.get("budget_delta"), command_id)
            return self._run_ok(
                self.service.run(
                    run_id,
                    budget_delta,
                    max_actions=self._optional_int(body.get("max_actions")),
                )
            )
        if command == "pause":
            return self._run_ok(self.service.pause(run_id, str(body.get("reason") or "user_requested")))
        if command == "resume":
            budget_delta = self._command_budget_delta(body.get("budget_delta"), command_id)
            return self._run_ok(
                self.service.resume(
                    run_id,
                    budget_delta,
                    max_actions=self._optional_int(body.get("max_actions")),
                    execute=bool(body.get("execute", True)),
                )
            )
        if command == "cancel":
            return self._run_ok(self.service.cancel(run_id, str(body.get("reason") or "user_requested")))
        if command == "budget":
            delta = body.get("budget_delta", body)
            resolved = BudgetDelta.from_value(delta)
            fields = {
                "actions": resolved.actions,
                "tokens": resolved.tokens,
                "time_seconds": resolved.time_seconds,
                "deadline": resolved.deadline,
                "acknowledge_missing_usage": resolved.acknowledge_missing_usage,
            }
            idempotency_key = resolved.idempotency_key or (
                f"web-command:{command_id}" if command_id else ""
            )
            if idempotency_key:
                view = self.service.apply_budget_delta_once(
                    run_id, idempotency_key=idempotency_key, **fields
                )
            else:
                view = self.service.apply_budget_delta(run_id, **fields)
            return self._run_ok(view)
        if command == "observation":
            observation = body.get("observation", body)
            if command_id and isinstance(observation, Mapping) and not observation.get("idempotency_key"):
                observation = {**observation, "idempotency_key": f"web-command:{command_id}"}
            return self._run_ok(self.service.submit_observation(run_id, observation))
        if command == "fork":
            entry = self.service.fork_session(
                run_id,
                str(body.get("from_entry_id") or ""),
                str(body.get("branch_id") or ""),
            )
            return self._ok({"entry": _jsonable(entry)})
        return self._error(404, "command_not_found")

    @staticmethod
    def _command_budget_delta(value: Any, command_id: str) -> Any:
        if not command_id or value is None or not isinstance(value, Mapping):
            return value
        if value.get("idempotency_key"):
            return value
        return {**value, "idempotency_key": f"web-command:{command_id}"}

    @staticmethod
    def _command_replay_is_uncertain(tail: list[str]) -> bool:
        if not tail:
            return False
        command = tail[1] if len(tail) > 1 else "run"
        return command in {"run", "resume", "observation", "fork"}

    def _evidence_graph(self, run_id: str, *, include_payload: bool, limit: int = 1000, offset: int = 0) -> dict[str, Any]:
        self.service.status(run_id)
        nodes = self.service.runtime.evidence_graph.list(run_id, include_unverified=True)
        bounded = max(1, min(10000, int(limit)))
        truncated = len(nodes) > offset + bounded
        nodes = nodes[offset : offset + bounded]
        projected = []
        edges = []
        for node in nodes:
            item = node.to_dict()
            if not include_payload:
                item.pop("payload", None)
            projected.append(item)
            edges.extend({"from": parent, "to": node.evidence_id, "kind": "parent"} for parent in node.parent_ids)
        return {"run_id": run_id, "nodes": projected, "edges": edges, "truncated": truncated, "next_offset": offset + len(nodes) if truncated else None}

    def _run_ok(self, view: Any, *, status: int = 200) -> WebResponse:
        return self._ok({"run": self._run_projection(view)}, status=status)

    @staticmethod
    def _run_projection(view: Any) -> dict[str, Any]:
        projected = _jsonable(view)
        if not isinstance(projected, Mapping):
            return {"value": projected}
        result = dict(projected)
        evidence = result.get("evidence", [])
        if isinstance(evidence, list):
            result["evidence"] = [
                {key: value for key, value in item.items() if key != "payload"}
                if isinstance(item, Mapping)
                else item
                for item in evidence
            ]
        return result

    @staticmethod
    def _payload_ref(payload: Mapping[str, Any]) -> str:
        reference = str(payload.get("artifact_ref") or payload.get("artifact_id") or "")
        return reference if reference and len(reference) <= 512 else contract_hash(payload)

    @classmethod
    def _event_projection(cls, event: Any, *, include_payload: bool = False) -> dict[str, Any]:
        payload = dict(event.payload)
        payload_hash = contract_hash(payload)
        if include_payload:
            projected = payload
        else:
            # Raw output and state snapshots remain in SQLite/CAS.  The default
            # Web/SSE projection is intentionally bounded and delta-oriented.
            projected = _event_value(payload)
        return {
            "run_id": event.run_id,
            "sequence": event.sequence,
            "event_type": event.event_type,
            # Some journal events do not carry a state snapshot/version.  A
            # deterministic event cursor still gives clients a usable revision
            # without leaking the full state.
            "revision": str(payload.get("state_version") or f"event:{event.sequence}"),
            "payload_ref": cls._payload_ref(payload),
            "payload_hash": payload_hash,
            "payload": projected,
            "created_at": event.created_at,
        }

    @staticmethod
    def _optional_int(value: Any) -> int | None:
        return None if value in (None, "") else int(value)

    @staticmethod
    def _int_query(query: Mapping[str, str], key: str, default: int, minimum: int, maximum: int) -> int:
        value = int(query.get(key, default))
        if value < minimum or value > maximum:
            raise ValueError(f"query_{key}_out_of_range:{minimum}:{maximum}")
        return value

    @staticmethod
    def _ok(value: Any, *, status: int = 200) -> WebResponse:
        normalized = _jsonable(value)
        payload = normalized if isinstance(normalized, Mapping) else {"value": normalized}
        return WebResponse.json({"schema_version": WEB_SCHEMA_VERSION, "ok": True, **dict(payload)}, status=status)

    @staticmethod
    def _error(status: int, error: str) -> WebResponse:
        return WebResponse.json(
            {"schema_version": WEB_SCHEMA_VERSION, "ok": False, "error": str(error)},
            status=status,
        )

    def sse_events(self, run_id: str, *, after_sequence: int = 0, wait_seconds: float = 0.0) -> list[dict[str, Any]]:
        """Return event deltas for an SSE connection, with bounded polling."""

        wait = float(wait_seconds)
        if not math.isfinite(wait):
            raise ValueError("wait_seconds_must_be_finite")
        deadline = time.monotonic() + max(0.0, min(30.0, wait))
        while True:
            events = self.service.events(run_id, after_sequence=after_sequence, limit=1000)
            if events or time.monotonic() >= deadline:
                return [self._event_projection(item) for item in events]
            time.sleep(0.1)

class _TraceHandler(BaseHTTPRequestHandler):
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
        raw = self.rfile.read(length)
        payload = json.loads(raw.decode("utf-8"))
        if not isinstance(payload, Mapping):
            raise TypeError("request_body_must_be_object")
        return dict(payload)

    def _write(self, response: WebResponse) -> None:
        self.send_response(response.status)
        self.send_header("Content-Type", response.content_type)
        self.send_header("Content-Length", str(len(response.body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Trace-Schema-Version", str(WEB_SCHEMA_VERSION))
        for name, value in _SECURITY_HEADERS.items():
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
        boundary_error = self._request_boundary()
        if boundary_error is not None:
            self._write(boundary_error)
            return
        parsed = urlsplit(self.path)
        static_file = _STATIC_FILES.get(parsed.path)
        if static_file is not None:
            name, content_type = static_file
            try:
                body = files("redteam_agent").joinpath("static", name).read_bytes()
            except (FileNotFoundError, OSError):
                self._write(self.api._error(404, "static_resource_not_found"))
                return
            self._write(WebResponse(200, body, content_type))
            return
        accepts_sse = "text/event-stream" in self.headers.get("Accept", "").casefold()
        if accepts_sse and parsed.path.startswith("/api/runs/") and parsed.path.endswith("/events"):
            self._write_sse(parsed)
            return
        self._write(self.api.dispatch("GET", self.path, headers=self.headers))

    def do_POST(self) -> None:  # noqa: N802
        boundary_error = self._request_boundary()
        if boundary_error is not None:
            self._reject_post(boundary_error)
            return
        if self.headers.get_content_type() != "application/json":
            self._reject_post(self.api._error(415, "application_json_required"))
            return
        try:
            payload = self._body()
            headers = {str(key).casefold(): str(value) for key, value in self.headers.items()}
            headers["x-trace-client"] = str(self.client_address[0])
            self._write(self.api.dispatch("POST", self.path, body=payload, headers=headers))
        except (ValueError, TypeError, json.JSONDecodeError) as exc:
            self._write(self.api._error(400, str(exc)))

    def _write_sse(self, parsed: Any) -> None:
        if (self.api.control.auth_required or self.api.force_auth) and not self.api.control.authenticated(
            {str(key).casefold(): str(value) for key, value in self.headers.items()},
            force=self.api.force_auth,
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
                query = {**query, "after_sequence": self.headers.get("Last-Event-ID", "0")}
            after = self.api._int_query(query, "after_sequence", 0, 0, 2**63 - 1)
            wait = float(query.get("wait_seconds", "0") or 0)
            events = self.api.sse_events(segments[2], after_sequence=after, wait_seconds=wait)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            # This endpoint emits a bounded batch. EOF terminates the batch so
            # EventSource can reconnect with Last-Event-ID instead of hanging.
            self.send_header("Connection", "close")
            self.send_header("X-Trace-Schema-Version", str(WEB_SCHEMA_VERSION))
            for name, value in _SECURITY_HEADERS.items():
                self.send_header(name, value)
            self.end_headers()
            headers_sent = True
            event_name = "trace-event" if query.get("channel") == "ui" else ""
            for event in events:
                event_type = event_name or str(event["event_type"]).replace("\r", "").replace("\n", "")
                self.wfile.write(
                    (
                        f"id: {event['sequence']}\n"
                        f"event: {event_type}\n"
                        f"data: {json.dumps(event, ensure_ascii=False, sort_keys=True, default=str)}\n\n"
                    ).encode("utf-8")
                )
            if not events:
                self.wfile.write(b": keep-alive\n\n")
            self.wfile.flush()
        except Exception as exc:
            if headers_sent:
                # The HTTP framing is already committed; keep the stream valid
                # instead of appending a JSON response after SSE data.
                try:
                    self.wfile.write(
                        (
                            "event: error\n"
                            f"data: {json.dumps({'error': str(exc)}, ensure_ascii=False)}\n\n"
                        ).encode("utf-8")
                    )
                    self.wfile.flush()
                except OSError:
                    return
            else:
                if isinstance(exc, KeyError):
                    self._write(self.api._error(404, str(exc)))
                elif isinstance(exc, (ValueError, TypeError)):
                    self._write(self.api._error(400, str(exc)))
                else:
                    self._write(self.api._error(500, f"internal_error:{type(exc).__name__}"))

    def log_message(self, format: str, *args: Any) -> None:
        return

class TraceHTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], api: WebApi) -> None:
        super().__init__(address, _TraceHandler)
        self.api = api

def model_provider_from_environment(
    environ: Mapping[str, str] | None = None,
    *,
    model: str = "",
    base_url: str = "",
    api_key_env: str = "",
    timeout_seconds: float | None = None,
    max_context_tokens: int | None = None,
) -> OpenAICompatibleProvider | None:
    values = environ if environ is not None else os.environ
    resolved_model = str(model or values.get("TRACE_MODEL") or "").strip()
    if not resolved_model:
        return None
    resolved_base = str(
        base_url or values.get("TRACE_API_BASE_URL") or "https://api.openai.com/v1"
    ).strip()
    key_name = str(api_key_env or values.get("TRACE_API_KEY_ENV") or "OPENAI_API_KEY").strip()
    resolved_timeout = (
        float(timeout_seconds)
        if timeout_seconds is not None
        else float(values.get("TRACE_API_TIMEOUT_SECONDS") or 120)
    )
    resolved_context = (
        int(max_context_tokens)
        if max_context_tokens is not None
        else int(values.get("TRACE_MODEL_CONTEXT_TOKENS") or 128_000)
    )
    return OpenAICompatibleProvider(
        resolved_base,
        resolved_model,
        str(values.get(key_name) or ""),
        timeout_seconds=resolved_timeout,
        max_context_tokens=resolved_context,
    )

def serve(
    root: Path,
    *,
    host: str = "127.0.0.1",
    port: int = 8765,
    model_port: ModelPort | None = None,
    model_name: str = "",
    api_base_url: str = "",
    api_key_env: str = "",
    api_timeout_seconds: float | None = None,
    model_context_tokens: int | None = None,
) -> None:
    provider = model_port or model_provider_from_environment(
        model=model_name,
        base_url=api_base_url,
        api_key_env=api_key_env,
        timeout_seconds=api_timeout_seconds,
        max_context_tokens=model_context_tokens,
    )
    resolved_model = model_name or str(
        provider.capabilities().metadata.get("model") if provider is not None else ""
    )
    service = AgentService(root=root, model_port=provider, model_name=resolved_model)
    try:
        is_loopback = ipaddress.ip_address(host).is_loopback
    except ValueError:
        is_loopback = host.casefold() == "localhost"
    if not is_loopback and not (os.environ.get("TRACE_ADMIN_PASSWORD") or os.environ.get("TRACE_ADMIN_TOKEN")):
        service.close()
        raise ValueError("non_loopback_requires_trace_admin_credentials")
    api = WebApi(service)
    api.force_auth = not is_loopback
    server = TraceHTTPServer((host, int(port)), api)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        service.close()

def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="redteam-agent-web")
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument("--model", default="")
    parser.add_argument("--api-base-url", default="")
    parser.add_argument("--api-key-env", default="")
    parser.add_argument("--api-timeout-seconds", type=float)
    parser.add_argument("--model-context-tokens", type=int)
    arguments = parser.parse_args(argv)
    serve(
        arguments.root.expanduser().resolve(),
        host=arguments.host,
        port=arguments.port,
        model_name=arguments.model,
        api_base_url=arguments.api_base_url,
        api_key_env=arguments.api_key_env,
        api_timeout_seconds=arguments.api_timeout_seconds,
        model_context_tokens=arguments.model_context_tokens,
    )
    return 0

__all__ = ["DEFAULT_EVENT_LIMIT", "MAX_REQUEST_BYTES", "WEB_SCHEMA_VERSION", "TraceHTTPServer", "WebApi", "WebResponse", "main", "model_provider_from_environment", "serve"]
