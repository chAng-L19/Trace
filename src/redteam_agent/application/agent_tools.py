from __future__ import annotations

import base64
import json
import threading
from dataclasses import asdict
from typing import Any, TYPE_CHECKING

from ..core import ToolCall, ToolDefinition, ToolPort, ToolResult, WorkerTask, contract_hash
from ..runtime.security import safe_error_text
from ..runtime.tool_broker import ToolBroker
from .bounded_output import BoundedOutput

if TYPE_CHECKING:
    from .agent_service import AgentService


MAX_ARGUMENT_BYTES = 64 * 1024
MAX_OUTPUT_BYTES = 16 * 1024
MAX_READ_BYTES = 8 * 1024
_TEXT = {"type": "string", "minLength": 1, "maxLength": 1024}
_PAGE = {
    "offset": {"type": "integer", "minimum": 0},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100},
}


def _definition(name: str, description: str, properties: dict, required=(), *, write=False):
    return ToolDefinition(
        qualified_name=f"agent:{name}", name=name, server="agent",
        description=description + " Bound to the current run; run_id is not accepted.",
        input_schema={"type": "object", "properties": properties,
                      "required": list(required), "additionalProperties": False},
        capabilities=("worker_dispatch" if write else "agent_context",),
        version="1", side_effecting=write, supports_reconcile=True,
        metadata={"source": "application" if write else "builtin"},
    )


_TOOLS = (
    _definition("export_session", "Read session branches and a page of journal tree nodes.", _PAGE),
    _definition("transcript", "Read a page of the active branch's conversation messages.", _PAGE),
    _definition("artifacts", "List a page of artifact metadata.", _PAGE),
    _definition("artifact", "Read metadata for one artifact.", {"artifact_id": _TEXT}, ("artifact_id",)),
    _definition("read_artifact", "Read verified artifact bytes in bounded chunks; follow next_offset.", {
        "artifact_id": _TEXT, "offset": _PAGE["offset"],
        "limit": {"type": "integer", "minimum": 1, "maximum": MAX_READ_BYTES},
        "encoding": {"type": "string", "enum": ["utf-8", "base64"]},
    }, ("artifact_id",)),
    _definition("search_artifacts", "Search artifact metadata and indexed previews.", {
        "query": _TEXT, "limit": _PAGE["limit"],
    }, ("query",)),
    _definition("worker_status", "Read one worker's durable status and result.", {"task_id": _TEXT}, ("task_id",)),
    _definition("worker_results", "List a page of durable worker statuses and results.", _PAGE),
    _definition("submit_worker", "Execute a worker with a stable task_id; replaying identical input runs it once. Returns the durable task_id for status/cancel.", {
        "task_id": _TEXT, "capability": _TEXT, "payload": {"type": "object"},
        "worker_kind": {"type": "string", "enum": ["local", "mcp", "codex_handoff", "docker"]},
        "timeout_seconds": {"type": "number", "minimum": 0.001, "maximum": 300},
        "required_artifacts": {"type": "array", "items": _TEXT, "maxItems": 100},
    }, ("task_id", "capability", "payload"), write=True),
    _definition("cancel_worker", "Request cancellation of a worker; repeated requests are reconciled from durable state.", {"task_id": _TEXT}, ("task_id",), write=True),
)
_BY_NAME = {tool.qualified_name: tool for tool in _TOOLS}


class AgentToolAdapter(ToolPort):
    """Compose application capabilities with the existing tool port after service setup."""

    def __init__(self, service: AgentService, delegate: ToolPort) -> None:
        self.service = service
        self.delegate = delegate
        self._active: dict[str, list[tuple[str, str]]] = {}
        self._lock = threading.Lock()

    def discover(self) -> tuple[ToolDefinition, ...]:
        return (*_TOOLS, *(item for item in self.delegate.discover() if item.qualified_name not in _BY_NAME))

    def capabilities(self) -> tuple[str, ...]:
        return tuple(dict.fromkeys(cap for tool in self.discover() for cap in tool.capabilities))

    def invoke(self, call: ToolCall) -> ToolResult:
        if call.tool_name not in _BY_NAME:
            return self.delegate.invoke(call)
        return self._invoke(call, reconcile=False)

    def reconcile(self, call: ToolCall) -> ToolResult | None:
        if call.tool_name not in _BY_NAME:
            return self.delegate.reconcile(call)
        return self._invoke(call, reconcile=True)

    def _invoke(self, call: ToolCall, *, reconcile: bool) -> ToolResult | None:
        try:
            if self.service.runtime.store.load_operation(call.run_id) is None:
                raise KeyError(f"operation_not_found:{call.run_id}")
            arguments = dict(call.arguments)
            if "run_id" in arguments:
                raise ValueError("agent_tool_run_override_forbidden")
            if len(json.dumps(arguments, ensure_ascii=False).encode("utf-8")) > MAX_ARGUMENT_BYTES:
                raise ValueError("agent_tool_arguments_too_large")
            error = ToolBroker._schema_error(_BY_NAME[call.tool_name].input_schema, arguments)
            if error:
                raise ValueError(error)
            if len(arguments.get("required_artifacts", ())) > 100:
                raise ValueError("worker_required_artifacts_too_many")
            name = _BY_NAME[call.tool_name].name
            if name == "submit_worker":
                task = self._worker_task(call, arguments)
                cancellation_id = str(call.metadata.get("cancellation_id") or call.call_id)
                if reconcile:
                    record = self.service.worker_records.get_for_run(task.task_id, call.run_id)
                    if record is None:
                        return None
                    if record.input_hash != self.service.worker_records.input_hash(task):
                        raise ValueError("worker_idempotency_conflict")
                with self._lock:
                    self._active.setdefault(cancellation_id, []).append((call.run_id, task.task_id))
                try:
                    output = self.service.execute_worker(task).to_dict()
                finally:
                    with self._lock:
                        active = self._active[cancellation_id]
                        active.remove((call.run_id, task.task_id))
                        if not active:
                            self._active.pop(cancellation_id)
            else:
                output = self._read_or_cancel(call.run_id, name, arguments)
            return ToolResult(call_id=call.call_id, tool_name=call.tool_name, status="success",
                              output=self._bounded(call, output))
        except (KeyError, ValueError, RuntimeError, OSError, TypeError) as exc:
            return ToolResult(call_id=call.call_id, tool_name=call.tool_name, status="failed",
                              error=safe_error_text(exc)[:1024])

    @staticmethod
    def _page(items, arguments: dict) -> dict:
        offset, limit = arguments.get("offset", 0), arguments.get("limit", 20)
        selected = items[offset:offset + limit]
        return {"items": selected, "total": len(items), "offset": offset,
                "next_offset": offset + len(selected) if offset + len(selected) < len(items) else None}

    def _read_or_cancel(self, run_id: str, name: str, arguments: dict) -> Any:
        if name == "export_session":
            exported = self.service.export_session(run_id)
            return {"session": exported["session"],
                    **self._page(list(exported["tree"]["nodes"].values()), arguments)}
        if name == "transcript":
            return self._page([item.to_dict() for item in self.service.transcript(run_id)], arguments)
        if name == "artifacts":
            return self._page([item.to_dict() for item in self.service.artifacts(run_id)], arguments)
        if name == "artifact":
            return self.service.artifact(run_id, arguments["artifact_id"]).to_dict()
        if name == "read_artifact":
            ref = self.service.artifact(run_id, arguments["artifact_id"])
            offset, limit = arguments.get("offset", 0), arguments.get("limit", 4096)
            raw = self.service.read_artifact(run_id, ref.artifact_id, offset=offset, limit=limit)
            encoding = arguments.get("encoding", "utf-8")
            return {"artifact_ref": ref.artifact_id, "content_hash": ref.content_hash,
                    "byte_count": ref.byte_count, "offset": offset, "returned_bytes": len(raw),
                    "next_offset": offset + len(raw) if offset + len(raw) < ref.byte_count else None,
                    "encoding": encoding,
                    "content": base64.b64encode(raw).decode("ascii") if encoding == "base64" else raw.decode("utf-8", errors="replace")}
        if name == "search_artifacts":
            return {"items": [item.to_dict() for item in self.service.search_artifacts(run_id, **arguments)]}
        if name == "worker_status":
            return asdict(self.service.worker_status(run_id, arguments["task_id"]))
        if name == "worker_results":
            return self._page([asdict(item) for item in self.service.worker_results(run_id)], arguments)
        if name == "cancel_worker":
            task_id = arguments["task_id"]
            record = self.service.worker_status(run_id, task_id)
            accepted = (record.status == "cancelled" or self.service.worker_records.cancel_requested(task_id)
                        or self.service.cancel_worker(run_id, task_id))
            return {"task_id": task_id, "cancel_requested": accepted,
                    "status": self.service.worker_status(run_id, task_id).status}
        raise KeyError(f"agent_tool_not_found:{name}")

    @staticmethod
    def _worker_task(call: ToolCall, arguments: dict) -> WorkerTask:
        identity = contract_hash({"run_id": call.run_id, "task_id": arguments["task_id"]})
        return WorkerTask.from_dict({
            "task_id": "agent-worker-" + identity[:32], "run_id": call.run_id,
            "capability": arguments["capability"], "payload": arguments["payload"],
            "idempotency_key": identity, "timeout_seconds": arguments.get("timeout_seconds", 60),
            "required_artifacts": arguments.get("required_artifacts", []),
            "metadata": {"worker_kind": arguments.get("worker_kind") or arguments["capability"].partition(".")[0]},
        })

    def _bounded(self, call: ToolCall, output: Any) -> Any:
        # Leave space for JSON escaping and the artifact envelope in the returned preview.
        bounded = BoundedOutput.capture_json(output, max_bytes=MAX_OUTPUT_BYTES // 8)
        try:
            preview = bounded.preview()
            if bounded.byte_count <= MAX_OUTPUT_BYTES and preview["line_count"] <= 200:
                return output
            bounded.close()
            ref = self.service.runtime.artifacts.put_file(
                bounded.path, run_id=call.run_id, artifact_type="agent_tool_output",
                media_type="application/json", metadata={"tool_name": call.tool_name},
            )
            identity = {key: output[key] for key in ("task_id", "status", "offset", "next_offset", "total")
                        if isinstance(output, dict) and key in output}
            return {**identity, "artifact_ref": ref.artifact_id, "content_hash": ref.content_hash,
                    "byte_count": ref.byte_count, "preview": preview}
        finally:
            bounded.discard()

    def cancel(self, call_id: str) -> bool:
        with self._lock:
            active = tuple(set(self._active.get(call_id, ())))
        if len(active) > 1:
            return False
        if active:
            try:
                return self.service.cancel_worker(*active[0])
            except KeyError:
                return False
        return self.delegate.cancel(call_id)

    def refresh(self, *, force: bool = False) -> None:
        refresher = getattr(self.delegate, "refresh", None)
        if callable(refresher):
            refresher(force=force)

    def restart(self, server: str, *, run_id: str = "") -> bool:
        restarter = getattr(self.delegate, "restart", None)
        return bool(restarter(server, run_id=run_id)) if callable(restarter) else False

    def close(self) -> None:
        closer = getattr(self.delegate, "close", None)
        if callable(closer):
            closer()
