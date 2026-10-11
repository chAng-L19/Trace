from __future__ import annotations
import argparse
import base64
import ipaddress
import json
import math
import os
import signal
import ssl
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import parse_qs, unquote, urlsplit
from uuid import uuid4

from ..application import AgentService
from ..application.contracts import BudgetDelta
from ..core import ModelPort, contract_hash
from ..application.bootstrap import resolve_provider, add_model_options, model_options
from ..application.trajectory import project_event
from ..runtime.store_common import ImmutableRecordError, StoreConflictError
from .web_routes import ControlRoutesMixin
from .web_projection import MAX_SEARCH_RECORDS, search_graph_projection, search_record_projection
WEB_SCHEMA_VERSION = 1
MAX_REQUEST_BYTES = 2 * 1024 * 1024
MAX_JSON_RESPONSE_BYTES = 8 * 1024 * 1024
DEFAULT_EVENT_LIMIT = 200
MAX_EVENT_PAYLOAD_BYTES = 16 * 1024
_RAW_EVENT_KEYS = frozenset({"state_snapshot", "payload", "output", "response", "request", "result", "stdout", "stderr"})
_STATIC_FILES = {
    "/": ("index.html", "text/html; charset=utf-8"),
    "/index.html": ("index.html", "text/html; charset=utf-8"),
    **{f"/{name}.css": (f"{name}.css", "text/css; charset=utf-8")
       for name in ("app", "base", "registry", "workbench", "control")},
    **{f"/{name}.js": (f"{name}.js", "text/javascript; charset=utf-8")
       for name in ("ui", "presentation", "layout", "control", "session", "profile", "run-controls", "app", "workspace", "theme", "lucide.min")},
    "/trace-mark.png": ("trace-mark.png", "image/png"),
    "/barlow-condensed-bold.ttf": ("barlow-condensed-bold.ttf", "font/ttf"),
    **{f"/{name}.ttf": (f"{name}.ttf", "font/ttf")
       for name in ("inter-regular", "inter-medium", "inter-semibold", "inter-bold",
                    "jetbrains-mono-regular", "jetbrains-mono-medium", "jetbrains-mono-semibold")},
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
        body = json.dumps(
            _jsonable(dict(payload)),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            default=str,
        ).encode("utf-8")
        if len(body) > MAX_JSON_RESPONSE_BYTES:
            bounded_body = json.dumps(
                {
                    "schema_version": WEB_SCHEMA_VERSION,
                    "ok": False,
                    "error": "response_too_large",
                    "max_bytes": MAX_JSON_RESPONSE_BYTES,
                },
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
            return cls(status=413, body=bounded_body, headers=headers or {})
        return cls(status=status, body=body, headers=headers or {})
    def payload(self) -> Mapping[str, Any]:
        value = json.loads(self.body.decode("utf-8"))
        return value if isinstance(value, Mapping) else {"value": value}
class WebApi(ControlRoutesMixin):
    def __init__(self, service: AgentService, *, command_ttl_seconds: float = 30.0) -> None:
        self.service = service
        self.command_ttl_seconds = max(1.0, float(command_ttl_seconds))
        self.owner = f"web-{uuid4().hex}"
        self.control = service.control
        self.force_auth = False
        self.tls_enabled = False
        self._mcp_restore_error = service.mcp_restore_error
    def dispatch(self, method, path, *, body=None, headers=None):
        try:
            return self._dispatch(method, path, body=body, headers=headers)
        except Exception as exc:
            return self._internal_error(exc)

    def _dispatch(
        self,
        method: str,
        path: str,
        *,
        body: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
    ) -> WebResponse:
        method = str(method or "GET").upper()
        if method not in {"GET", "POST", "DELETE"}:
            # Keep the in-process adapter contract identical to the HTTP
            # handler.  Unsupported verbs must never reach a resource
            # dispatcher and be misreported as a missing route.
            return self._error(405, "method_not_allowed")
        parsed = urlsplit(path)
        segments = [unquote(item) for item in parsed.path.split("/") if item]
        query = {key: values[-1] for key, values in parse_qs(parsed.query).items() if values}
        payload = dict(body or {})
        request_headers = {str(key).casefold(): str(value) for key, value in (headers or {}).items()}
        if parsed.path in {"/health/live", "/health/ready", "/healthz", "/readyz"}:
            if method != "GET":
                return self._error(405, "method_not_allowed")
            loop = self.service.model_loop
            configured = loop is not None
            ready = configured and bool(getattr(loop.model, "ready", True)) and not self.service.closing
            live = parsed.path in {"/health/live", "/healthz"}
            return self._ok({"live": not self.service.closing, "configured": configured, "ready": ready},
                            status=200 if (not self.service.closing if live else ready) else 503)
        if segments[:2] == ["api", "auth"]:
            return self._auth(method, segments[2:], payload, request_headers)
        if (self.control.auth_required or self.force_auth) and segments[:2] != ["api", "auth"]:
            if not self.control.authenticated(request_headers, force=self.force_auth):
                return self._error(401, "authentication_required")
        if method != "GET" and segments[:2] in (["api", "providers"], ["api", "skills"], ["api", "mcp"], ["api", "system"]):
            user = self.control.accounts.user(request_headers)
            if user is None or user["role"] != "admin":
                return self._error(403, "admin_required")
        if segments == ["api", "system"]:
            if method != "GET":
                return self._error(405, "method_not_allowed")
            return self._ok(self.system_info())
        if segments[:2] == ["api", "providers"]:
            return self._control_request(method, "providers", segments[2:], payload, request_headers)
        if segments[:2] == ["api", "skills"]:
            return self._control_request(method, "skills", segments[2:], payload, request_headers)
        if segments[:2] == ["api", "mcp"]:
            return self._control_request(method, "mcp", segments[2:], payload, request_headers)
        if segments[:2] == ["api", "conversations"]:
            return self._control_request(method, "conversations", segments[2:], query if method == "GET" else payload, request_headers)
        if segments[:2] == ["api", "workspaces"]:
            try:
                return self._workspace_request(method, segments[2:], query if method == "GET" else payload)
            except KeyError as exc:
                return self._error(404, str(exc))
            except StoreConflictError as exc:
                return self._error(409, str(exc))
            except (TypeError, ValueError, json.JSONDecodeError) as exc:
                return self._error(400, str(exc))
        if segments == ["api", "system", "reload"]:
            if method != "POST":
                return self._error(405, "method_not_allowed")
            return self._control_request(method, "system", ["reload"], payload, request_headers)
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
            return self._internal_error(exc)

    def _workspace_request(self, method: str, tail: list[str], body: Mapping[str, Any]) -> WebResponse:
        manager = self.service.project_workspaces
        if not tail:
            if method == "GET":
                return self._ok(manager.list(
                    search=str(body.get("search") or ""), status=str(body.get("status") or ""),
                    limit=self._int_query(body, "limit", 100, 1, 1000),
                    offset=self._int_query(body, "offset", 0, 0, 1_000_000),
                ))
            if method == "POST":
                return self._ok({"workspace": manager.create(body)}, status=201)
            return self._error(405, "method_not_allowed")
        workspace_id = tail[0]
        if len(tail) == 1:
            if method == "GET":
                return self._ok(manager.detail(workspace_id))
            if method == "DELETE":
                manager.delete(workspace_id)
                return self._ok({"deleted": workspace_id})
            if method == "POST":
                action = str(body.get("action") or "").strip()
                if action == "rename":
                    return self._ok({"workspace": manager.rename(workspace_id, str(body.get("title") or ""))})
                if action in {"active", "stopped", "completed"}:
                    return self._ok({"workspace": manager.set_status(workspace_id, action)})
                return self._error(400, "workspace_action_required")
            return self._error(405, "method_not_allowed")
        if len(tail) == 2 and tail[1] == "hints" and method == "POST":
            return self._ok({"workspace": manager.add_hint(workspace_id, body)})
        if len(tail) == 2 and tail[1] == "intents" and method == "POST":
            return self._ok(manager.add_intent(workspace_id, body), status=201)
        if len(tail) == 4 and tail[1] == "intents" and tail[3] in {"claim", "heartbeat", "release", "conclude"} and method == "POST":
            intent_id, action = tail[2], tail[3]
            worker = str(body.get("worker") or "operator")
            if action == "claim":
                return self._ok(manager.claim_intent(workspace_id, intent_id, worker))
            if action == "heartbeat":
                return self._ok(manager.heartbeat_intent(workspace_id, intent_id, worker))
            if action == "release":
                return self._ok(manager.release_intent(workspace_id, intent_id, worker))
            return self._ok(manager.conclude_intent(workspace_id, intent_id, worker, str(body.get("description") or "")))
        if len(tail) == 3 and tail[1] == "reason" and tail[2] in {"claim", "heartbeat", "release"} and method == "POST":
            action, worker = tail[2], str(body.get("worker") or "operator")
            if action == "claim":
                return self._ok(manager.claim_reason(workspace_id, worker, str(body.get("trigger") or "operator")))
            if action == "heartbeat":
                return self._ok(manager.heartbeat_reason(workspace_id, worker))
            return self._ok(manager.release_reason(workspace_id, worker))
        if len(tail) == 2 and tail[1] == "export" and method == "GET":
            exported = manager.export(workspace_id)
            if str(body.get("format") or "") == "markdown":
                workspace = exported["workspace"]
                lines = [f"# {workspace['title']}", "", f"状态：{workspace['status']}",
                         f"目标：{workspace['goal']}", "", "## 线索"]
                lines.extend(f"- {item['content']}（{item['creator']}）" for item in exported["hints"])
                lines.append("\n## 事实与结论")
                lines.extend(f"- {item['description']}（来源：{item['source']}；"
                             f"{'已验证' if item.get('verified') else '未核验'}；编号：{item['id']}）"
                             for item in exported["facts"])
                lines.append("\n## 探索意图")
                lines.extend(f"- {item['description']}（{'已收敛' if item['status'] == 'concluded' else '待探索'}；"
                             f"负责人：{item.get('worker') or '未认领'}；结论：{item.get('to') or '尚无'}）"
                             for item in exported["intents"])
                lines.append("\n## 执行")
                lines.extend(f"- {item['run']['run_id']}：{item['run']['status']}，{item['run']['objective']}" for item in exported["runs"])
                return WebResponse(200, ("\n".join(lines) + "\n").encode("utf-8"), "text/markdown; charset=utf-8",
                                   {"Content-Disposition": 'attachment; filename="trace-workspace.md"'})
            return self._ok(exported)
        return self._error(404, "workspace_resource_not_found")

    def _control_request(
        self,
        method: str,
        domain: str,
        tail: list[str],
        body: Mapping[str, Any],
        headers: Mapping[str, str],
    ) -> WebResponse:
        """Apply durable single-flight semantics to browser control writes."""

        command_id = str(headers.get("x-command-id") or body.get("command_id") or "").strip()
        command_body = {key: value for key, value in body.items() if key != "command_id"}
        run_id = tail[0] if domain == "conversations" and tail else ""
        if method == "GET":
            if domain == "system" and tail == ["reload"]:
                return self._ok(self._reload_control_plane())
            return self._safe_control(method, domain, tail, body)
        if not command_id:
            command_id = self._implicit_command_id(["control", domain, *tail], command_body, run_id=run_id)
        request_hash = contract_hash({"method": method, "route": [domain, *tail], "body": command_body})
        owner = f"{self.owner}:{uuid4().hex}"
        try:
            receipt = self.service.runtime.store.claim_web_command(
                command_id, request_hash, owner=owner, run_id=run_id, ttl_seconds=self.command_ttl_seconds,
            )
            if receipt["status"] == "completed":
                saved = receipt["response"]
                replay = self._replay_receipt(saved)
                if replay is not None:
                    return self._command_response(replay, command_id)
                if isinstance(saved.get("payload"), Mapping):
                    return self._command_response(WebResponse.json(saved["payload"], status=int(saved.get("status", 200))), command_id)
                return self._command_response(WebResponse.json(saved, status=200), command_id)
            if not receipt.get("claimed") or receipt["owner"] != owner or receipt["status"] != "pending":
                return self._error(409, "command_in_progress")
            if receipt.get("reclaimed"):
                response = self._error(409, "command_result_uncertain")
                self.service.runtime.store.complete_web_command(
                    command_id, self._receipt_payload(response),
                    owner=owner, fencing_token=int(receipt["fencing_token"]), run_id=run_id,
                )
                return self._command_response(response, command_id)
            if domain == "system" and tail == ["reload"]:
                response = self._ok(self._reload_control_plane())
            else:
                response = self._safe_control(method, domain, tail, command_body)
            if response.status < 500:
                self.service.runtime.store.complete_web_command(
                    command_id, self._receipt_payload(response),
                    owner=owner, fencing_token=int(receipt["fencing_token"]), run_id=run_id,
                )
            return self._command_response(response, command_id)
        except ImmutableRecordError as exc:
            return self._error(409, str(exc))
        except StoreConflictError as exc:
            return self._error(409, str(exc))
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            return self._error(400, str(exc))

    @staticmethod
    def _receipt_payload(response: WebResponse) -> dict[str, Any]:
        encoded = base64.urlsafe_b64encode(response.body).decode("ascii")
        return {"status": response.status, "payload_encoding": "json-base64", "payload": encoded}

    @staticmethod
    def _replay_receipt(saved: Mapping[str, Any]) -> WebResponse | None:
        if saved.get("payload_encoding") != "json-base64":
            return None
        try:
            body = base64.urlsafe_b64decode(str(saved["payload"]).encode("ascii"))
            json.loads(body.decode("utf-8"))
        except (KeyError, ValueError, TypeError, UnicodeError, json.JSONDecodeError):
            return WebResponse.json(
                {"schema_version": WEB_SCHEMA_VERSION, "ok": False, "error": "command_receipt_corrupt"},
                status=409,
            )
        return WebResponse(int(saved.get("status", 200)), body)
    def _get(self, tail: list[str], query: Mapping[str, str]) -> WebResponse:
        if not tail:
            limit = self._int_query(query, "limit", 100, 1, 1000)
            views = self.service.list_runs(summary_only=True, limit=limit + 1, offset=self._int_query(query, "offset", 0, 0, 1000000),
                status=str(query.get("status") or ""), search=str(query.get("search") or ""))
            return self._ok({"runs": [self._run_projection(view) for view in views[:limit]], "truncated": len(views) > limit})
        run_id = tail[0]
        if len(tail) == 1:
            return self._ok({"run": self._run_projection(self.service.status(run_id))})
        resource = tail[1]
        if resource == "events":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            if query.get("channel") == "trajectory":
                after = self._int_query(query, "after_sequence", 0, 0, 2**63 - 1)
                items, cursor = self._trajectory_events(
                    run_id, after, self._int_query(query, "limit", DEFAULT_EVENT_LIMIT, 1, 1000)
                )
            else:
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
                    "next_sequence": cursor if query.get("channel") == "trajectory" else (items[-1]["sequence"] if items else None),
                }
            )
        if resource == "search-graph":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            records = self.service.exploration_records(run_id)
            visible_records = records[-MAX_SEARCH_RECORDS:]
            return self._ok({
                "run_id": run_id,
                "search_graph": search_graph_projection(self.service.exploration_state(run_id)),
                "records": [search_record_projection(item.to_dict()) for item in visible_records],
                "records_truncated": len(records) > len(visible_records),
            })
        if resource == "evidence-graph":
            if len(tail) == 3:
                return self._ok(self.service.evidence_lineage(run_id, tail[2]))
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok(self._evidence_graph(
                run_id,
                include_payload=query.get("include_payload") == "1",
                limit=self._int_query(query, "limit", 1000, 1, 10000),
                offset=self._int_query(query, "offset", 0, 0, 1_000_000),
            ))
        if resource == "asset-attack-graph":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            # Only explicitly versioned Core objects are materialized here;
            # ordinary EvidenceGraph payloads remain ordinary evidence.
            return self._ok(self.service.asset_attack_graph(
                run_id,
                limit=self._int_query(query, "limit", 1000, 1, 10000),
                offset=self._int_query(query, "offset", 0, 0, 1_000_000),
            ))
        if resource == "attack-paths":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            graph = self.service.asset_attack_graph(
                run_id,
                limit=self._int_query(query, "limit", 1000, 1, 10000),
                offset=self._int_query(query, "offset", 0, 0, 1_000_000),
            )
            return self._ok({
                "run_id": run_id,
                "materialized": bool(graph.get("materialized")),
                "attack_paths": list(graph.get("attack_paths", ())),
                "edges": list(graph.get("edges", ())),
                "source_evidence_ids": list(graph.get("source_evidence_ids", ())),
                "truncated": bool(graph.get("truncated")),
                "next_offset": graph.get("next_offset"),
            })
        if resource == "transparency":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            projection = self.service.inspect_session(
                run_id,
                event_limit=self._int_query(query, "limit", 1000, 1, 10000),
            )
            return self._ok({"run_id": run_id, **_jsonable(projection)})
        if resource == "tools":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            return self._ok(self.service.explain_tool_visibility(run_id, tool_name=str(query.get("tool") or "")))
        if resource == "transcript":
            if len(tail) != 2:
                return self._error(404, "resource_not_found")
            offset = self._int_query(query, "offset", 0, 0, 1_000_000)
            limit = self._int_query(query, "limit", 200, 1, 1000)
            latest = query.get("latest") == "1"
            messages = self.service.transcript(run_id, limit=limit + 1, offset=offset, latest=latest)
            return self._ok({"run_id": run_id, "messages": [_jsonable(item) for item in (messages[-limit:] if latest else messages[:limit])], "truncated": len(messages) > limit})
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
            from ..application.reporting import build_report, render_report_markdown
            report = build_report(self.service, run_id)
            if query.get("format") == "markdown":
                return WebResponse(200, render_report_markdown(report).encode("utf-8"),
                    "text/markdown; charset=utf-8", {"Content-Disposition": 'attachment; filename="trace-report.md"'})
            if query.get("format") == "html":
                from ..application.reporting import render_report_html
                return WebResponse(200, render_report_html(report).encode("utf-8"),
                    "text/html; charset=utf-8", {"Content-Disposition": 'attachment; filename="trace-report.html"'})
            return self._ok(report)
        return self._error(404, "resource_not_found")

    def _post(
        self,
        tail: list[str],
        body: Mapping[str, Any],
        headers: Mapping[str, str],
    ) -> WebResponse:
        command_id = str(headers.get("x-command-id") or body.get("command_id") or "").strip()
        command_body = {key: value for key, value in body.items() if key != "command_id"}
        run_id = tail[0] if tail else ""
        if not command_id:
            command_id = self._implicit_command_id(["runs", *tail], command_body, run_id=run_id)
        request_hash = contract_hash({"route": tail, "body": command_body})
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
                replay = self._replay_receipt(saved)
                if replay is not None:
                    return self._command_response(replay, command_id)
                if isinstance(saved.get("payload"), Mapping):
                    return self._command_response(WebResponse.json(saved["payload"], status=int(saved.get("status", 200))), command_id)
                return self._command_response(WebResponse.json(saved, status=200), command_id)
            if not receipt.get("claimed") or receipt["owner"] != claim_owner or receipt["status"] != "pending":
                return self._error(409, "command_in_progress")
            fencing_token = int(receipt["fencing_token"])
            if receipt.get("reclaimed") and self._command_replay_is_uncertain(tail):
                response = self._error(409, "command_result_uncertain")
                self.service.runtime.store.complete_web_command(
                    command_id,
                    self._receipt_payload(response),
                    owner=claim_owner,
                    fencing_token=fencing_token,
                    run_id=run_id,
                )
                return self._command_response(response, command_id)
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
                self._receipt_payload(response),
                owner=claim_owner,
                fencing_token=fencing_token,
                run_id=run_id or str(saved_run_id or ""),
            )
        return self._command_response(response, command_id)

    @staticmethod
    def _command_response(response: WebResponse, command_id: str) -> WebResponse:
        """Expose the effective id so legacy clients can retry durably."""

        if not command_id:
            return response
        headers = dict(response.headers)
        headers.setdefault("X-Command-ID", command_id)
        return WebResponse(response.status, response.body, response.content_type, headers)

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
        if command == "discussion":
            result = self.service.discuss(run_id, body.get("text"), message_id=command_id)
            return self._ok({"run": self._run_projection(self.service.status(run_id)), **_jsonable(result)})
        if command == "message":
            message = self.service.submit_message(run_id, body.get("text"), message_id=command_id)
            return self._ok({"run": self._run_projection(self.service.status(run_id)), "message": _jsonable(message)})
        if command == "run":
            budget_delta = self._command_budget_delta(body.get("budget_delta"), command_id)
            return self._run_ok(
                self.service.run(
                    run_id,
                    budget_delta,
                    max_actions=self._optional_int(body.get("max_actions")),
                    run_until_pause=bool(body.get("run_until_pause", True)),
                    max_cycles=int(body.get("max_cycles", 32)),
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
                    run_until_pause=bool(body.get("run_until_pause", True)),
                    max_cycles=int(body.get("max_cycles", 32)),
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
                "action_limit": resolved.action_limit,
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
            session = _jsonable(self.service.export_session(run_id))
            metadata = session.get("session", {})
            active = str(metadata.get("active_branch_id") or "")
            branches = metadata.get("branches", {})
            return self._ok({
                "run_id": run_id,
                "entry": _jsonable(entry),
                "branch_id": active,
                "active_branch_id": active,
                "branches": branches,
                "leaf_entry_id": branches.get(active),
            })
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
        return command in {"run", "resume", "observation", "fork", "message", "discussion"}

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

    def sse_events(self, run_id: str, *, after_sequence: int = 0, wait_seconds: float = 0.0,
                   channel: str = "") -> list[dict[str, Any]]:
        """Return event deltas for an SSE connection, with bounded polling."""

        wait = float(wait_seconds)
        if not math.isfinite(wait):
            raise ValueError("wait_seconds_must_be_finite")
        deadline = time.monotonic() + max(0.0, min(30.0, wait))
        while True:
            if channel == "trajectory":
                projected, cursor = self._trajectory_events(run_id, after_sequence, 1000)
                if projected or cursor > after_sequence or time.monotonic() >= deadline:
                    if cursor > after_sequence:
                        projected.append({"run_id": run_id, "sequence": cursor, "event_type": "trace-cursor"})
                    return projected
            else:
                events = self.service.events(run_id, after_sequence=after_sequence, limit=1000)
                if events or time.monotonic() >= deadline:
                    return [self._event_projection(item) for item in events]
            time.sleep(0.1)

    def _trajectory_events(self, run_id: str, after_sequence: int, limit: int) -> tuple[list[dict[str, Any]], int]:
        """Read durable events until a visible page is full or the log ends."""
        projected: list[dict[str, Any]] = []
        cursor = after_sequence
        while len(projected) < limit:
            events = self.service.events(run_id, after_sequence=cursor, limit=1000)
            for item in events:
                cursor = item.sequence
                step = project_event(item)
                if step is not None:
                    projected.append({"run_id": item.run_id, "sequence": item.sequence, "event_type": "trajectory",
                                      "trajectory": step, "created_at": item.created_at})
                    if len(projected) == limit:
                        return projected, cursor
            if len(events) < 1000:
                break
        return projected, cursor


def model_provider_from_environment(
    environ: Mapping[str, str] | None = None,
    *,
    model: str = "",
    base_url: str = "",
    api_key_env: str = "",
    timeout_seconds: float | None = None,
    max_context_tokens: int | None = None,
    **provider_options: Any,
) -> ModelPort | None:
    provider, _ = resolve_provider(None, (), {
        "model": model, "base_url": base_url, "api_key_env": api_key_env,
        "timeout_seconds": timeout_seconds, "max_context_tokens": max_context_tokens,
        **provider_options,
    }, environ=environ)
    return provider
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
    config_paths: list[str] | None = None,
    provider_options: Mapping[str, Any] | None = None,
) -> None:
    # Import lazily so ``python -m redteam_agent.adapters.web`` and the
    # installed ``trace-web`` entry point do not create a cycle:
    # web_server depends on WebApi while the server is only needed at startup.
    from .web_server import TraceHTTPServer

    tls_cert, tls_key = os.environ.get("TRACE_TLS_CERT", ""), os.environ.get("TRACE_TLS_KEY", "")
    tls_enabled = bool(tls_cert or tls_key)
    if bool(tls_cert) != bool(tls_key):
        raise ValueError("trace_tls_cert_and_key_required")
    tls_context = None
    if tls_enabled:
        tls_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls_context.load_cert_chain(tls_cert, tls_key)
    service = AgentService(root=root, model_port=model_port, model_name=model_name,
        model_streaming=model_port.capabilities().streaming if model_port is not None else False,
        config_paths=config_paths, provider_options={"model": model_name, "base_url": api_base_url,
        "api_key_env": api_key_env, "timeout_seconds": api_timeout_seconds,
        "max_context_tokens": model_context_tokens, **(provider_options or {})})
    try:
        is_loopback = ipaddress.ip_address(host).is_loopback
    except ValueError:
        is_loopback = host.casefold() == "localhost"
    if not is_loopback and not tls_enabled and os.environ.get("TRACE_ALLOW_INSECURE_HTTP") != "1":
        service.close()
        raise ValueError("non_loopback_requires_tls_or_explicit_insecure_http")
    api = WebApi(service)
    api.force_auth = not is_loopback
    api.tls_enabled = tls_enabled
    server = None
    try:
        server = TraceHTTPServer((host, int(port)), api)
        if tls_context is not None:
            server.socket = tls_context.wrap_socket(server.socket, server_side=True)
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        try:
            service.close()
        finally:
            if server is not None:
                server.server_close()

def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="trace-web")
    add_model_options(parser)
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or Path.home() / ".redteam-agent")
    parser.add_argument("--root", type=Path, default=Path(os.environ.get("TRACE_HOME") or agent_home / "operations"))
    parser.add_argument("--config", action="append", default=[])
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument("--model", default="")
    parser.add_argument("--api-base-url", default="")
    parser.add_argument("--api-key-env", default="")
    parser.add_argument("--api-timeout-seconds", type=float)
    parser.add_argument("--model-context-tokens", type=int)
    arguments = parser.parse_args(argv)
    # SIGTERM (Docker/systemd) follows the same cleanup path as Ctrl+C.
    previous_sigterm = signal.signal(signal.SIGTERM, signal.default_int_handler)
    try:
        serve(arguments.root.expanduser().resolve(), host=arguments.host, port=arguments.port, model_name=arguments.model, api_base_url=arguments.api_base_url, api_key_env=arguments.api_key_env, api_timeout_seconds=arguments.api_timeout_seconds, model_context_tokens=arguments.model_context_tokens, config_paths=arguments.config, provider_options=model_options(arguments))
    finally:
        signal.signal(signal.SIGTERM, previous_sigterm)
    return 0

__all__ = ["DEFAULT_EVENT_LIMIT", "MAX_JSON_RESPONSE_BYTES", "MAX_REQUEST_BYTES", "WEB_SCHEMA_VERSION", "TraceHTTPServer", "WebApi", "WebResponse", "main", "model_provider_from_environment", "serve"]
def __getattr__(name: str) -> Any:
    """Lazily expose the HTTP server while keeping the adapter importable."""
    if name == "TraceHTTPServer":
        from .web_server import TraceHTTPServer
        return TraceHTTPServer
    raise AttributeError(name)

if __name__ == "__main__":
    raise SystemExit(main())
