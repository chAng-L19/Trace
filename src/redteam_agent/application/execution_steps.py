"""Run-bound projections of existing execution records; no second execution log."""
from __future__ import annotations

import json
from dataclasses import asdict
from typing import Any

from ..core import contract_hash
from ..runtime.security import redact_sensitive


def event_pages(store, run_id: str):
    cursor = 0
    while page := store.events(run_id, after_event_id=cursor, limit=1000):
        yield from page
        cursor = page[-1]["event_id"]


def page(items: list, *, offset: int = 0, limit: int = 20) -> dict:
    if isinstance(offset, bool) or isinstance(limit, bool) or offset < 0 or not 1 <= limit <= 100:
        raise ValueError("execution_page_invalid")
    selected = items[offset:offset + limit]
    return {"items": selected, "total": len(items), "offset": offset,
            "next_offset": offset + len(selected) if offset + len(selected) < len(items) else None}


class ExecutionSteps:
    def __init__(self, service) -> None:
        self.service = service
        self.store = service.runtime.store

    def _records(self, run_id: str):
        if self.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        # ponytail: existing run loaders scan O(run records); add indexed paging when measured.
        for record in self.store.model_observations(run_id):
            yield "model", record.observation_id, record.to_dict()
        for record in self.store.task_attempts(run_id):
            yield "attempt", record.attempt_id, record.to_dict()
        for record in self.service.worker_records.records(run_id):
            yield "worker", record.task.task_id, asdict(record)

    @staticmethod
    def _summary(kind: str, identity: str, raw: dict) -> dict:
        task = raw.get("task", {})
        return {
            "step_id": f"{kind}:{identity}", "kind": kind, "status": raw["status"],
            "tool_name": raw.get("tool_name") or raw.get("tool") or task.get("capability", ""),
            "task_id": task.get("task_id", ""), "worker_kind": raw.get("worker_kind", ""),
            "attempt_id": raw.get("attempt_id", ""), "action_id": raw.get("action_id", ""),
            "request_id": raw.get("request_id", ""), "call_id": raw.get("call_id", ""),
            "created_at": raw.get("created_at") or raw.get("started_at", ""),
            "source_hash": contract_hash(raw), "authority": "execution_record",
        }

    def search(self, run_id: str, *, query: str = "", kind: str = "", status: str = "",
               task_id: str = "", offset: int = 0, limit: int = 20) -> dict:
        items = []
        responses = ({item.request_id: item for item in self.store.model_responses(run_id)}
                     if query and kind in {"", "model"} else {})
        for source_kind, identity, raw in self._records(run_id):
            summary = self._summary(source_kind, identity, raw)
            if kind and kind != source_kind or status and status != summary["status"]:
                continue
            if task_id and task_id != summary["task_id"]:
                continue
            if query:
                searchable = raw
                if source_kind == "model":
                    searchable = {**raw, "tool_call": self._model_call(raw, responses.get(raw["request_id"]))}
                if query.casefold() not in json.dumps(redact_sensitive(searchable), ensure_ascii=False).casefold():
                    continue
            items.append(summary)
        items.sort(key=lambda item: (item["created_at"], item["step_id"]))
        return page(items, offset=offset, limit=limit)

    @staticmethod
    def _model_call(raw: dict, response) -> dict | None:
        calls = response.response.get("tool_calls", ()) if response else ()
        return next((dict(call) for index, call in enumerate(calls)
                     if str(call.get("call_id") or call.get("id") or f"call-{index}").strip() == raw["call_id"]), None)

    def read(self, run_id: str, step_id: str, *, offset: int = 0, limit: int = 20) -> dict:
        for kind, identity, raw in self._records(run_id):
            if step_id == f"{kind}:{identity}":
                break
        else:
            raise KeyError(f"execution_step_not_found:{step_id}")
        summary = self._summary(kind, identity, raw)
        tool_call = None
        result = raw.get("result")
        if kind == "model":
            response = next((item for item in self.store.model_responses(run_id)
                             if item.request_id == raw["request_id"]), None)
            tool_call = self._model_call(raw, response)
            result = raw["observation"]
        elif kind == "worker":
            # The durable WorkerTask is the original invocation, including failures/cancellation.
            tool_call = raw["task"]
        refs = set()

        def references(value: Any):
            if isinstance(value, dict):
                for key, child in value.items():
                    if key in {"artifact_ref", "artifact_id", "complete_result_artifact"} and isinstance(child, str) and child:
                        refs.add(child)
                    elif key in {"artifact_refs", "complete_output_artifacts", "required_artifacts"} and isinstance(child, (list, tuple)):
                        refs.update(item for item in child if isinstance(item, str))
                    else:
                        references(child)
            elif isinstance(value, (list, tuple)):
                for child in value:
                    references(child)

        references(raw)
        artifacts, unavailable = [], []
        for ref in self.service.artifacts(run_id):
            metadata = ref.metadata
            linked = ((kind == "worker" and metadata.get("task_id") == identity)
                      or (kind == "attempt" and metadata.get("attempt_id") == identity)
                      or (kind == "model" and metadata.get("request_id") == raw["request_id"]
                          and metadata.get("call_id") == raw["call_id"]))
            if linked:
                refs.add(ref.artifact_id)
        for artifact_id in sorted(refs):
            try:
                ref = self.service.artifact(run_id, artifact_id)
                # Metadata only here; read_artifact verifies CAS bytes on expansion.
                artifacts.append({"artifact_ref": ref.artifact_id, "content_hash": ref.content_hash,
                                  "byte_count": ref.byte_count, "artifact_type": ref.artifact_type})
            except KeyError:
                unavailable.append(artifact_id)
        events = [item for item in event_pages(self.store, run_id)
                  if (item["payload"].get("task_id") == identity if kind == "worker"
                      else item["payload"].get("attempt_id") == identity if kind == "attempt"
                      else item["payload"].get("request_id") == raw["request_id"]
                      and item["payload"].get("call_id") == raw["call_id"])]
        table = {"model": "model_observations", "attempt": "task_attempts", "worker": "worker_tasks"}[kind]
        event_ids = {str(item["event_id"]) for item in events}
        journal_refs = [entry.to_dict() for entry in self.service.journal.entries(run_id)
                        if (entry.raw_table == table and entry.raw_id == identity)
                        or (entry.raw_table == "operation_events" and entry.raw_id in event_ids)]
        return redact_sensitive({**summary, "tool_call": tool_call, "record": raw, "result": result,
                                 "artifact_refs": artifacts, "unavailable_artifact_refs": unavailable,
                                 "events": page(events, offset=offset, limit=limit), "journal_refs": journal_refs,
                                 "input_availability": "recorded" if tool_call else "hash_only"})
