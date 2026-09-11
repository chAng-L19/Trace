from __future__ import annotations

"""Read-only, versioned projections for operators and deterministic evaluation.

The runtime remains the authority for state, evidence and terminal decisions.  This
module only joins already persisted records and never promotes observations or
changes operation state.
"""

from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from ..core import contract_hash
from ..runtime.security import redact_sensitive


SCHEMA_VERSION = 1


def _record_dict(value: Any) -> dict[str, Any]:
    if hasattr(value, "to_dict"):
        return dict(value.to_dict())
    if isinstance(value, Mapping):
        return dict(value)
    return {"value": str(value)}


def _int(value: Any, default: int = 0) -> int:
    try:
        return max(0, int(value))
    except (TypeError, ValueError, OverflowError):
        return default


def _usage_totals(records: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    totals = {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
    missing = 0
    for record in records:
        for key in totals:
            value = record.get(key)
            if value is not None:
                totals[key] += _int(value)
        if bool(record.get("usage_missing")):
            missing += 1
    return {**totals, "requests": len(records), "missing_requests": missing}


@dataclass(frozen=True, slots=True)
class TransparencyProjector:
    service: Any

    def inspect(self, run_id: str, *, event_limit: int = 1000) -> dict[str, Any]:
        return self._build(run_id, event_limit=event_limit, include_raw=False)

    def export(self, run_id: str, *, event_limit: int = 10000) -> dict[str, Any]:
        return self._build(run_id, event_limit=event_limit, include_raw=True)

    def _build(self, run_id: str, *, event_limit: int, include_raw: bool) -> dict[str, Any]:
        view = self.service.status(run_id)
        store = self.service.runtime.store
        events = self.service.events(run_id, limit=max(1, min(10000, int(event_limit))))
        requests = tuple(store.model_requests(run_id))
        responses = {item.request_id: item for item in store.model_responses(run_id)}
        observations = tuple(store.model_observations(run_id))
        usage = tuple(store.model_budget_usage(run_id))
        snapshots = tuple(store.context_snapshots(run_id))
        summaries = tuple(store.context_summaries(run_id))
        artifacts = tuple(self.service.artifacts(run_id))
        evidence = tuple(self.service.runtime.evidence_graph.list(run_id, include_unverified=True))
        catalog = self.service.tool_catalog(run_id)

        actions = []
        for request in requests:
            response = responses.get(request.request_id)
            request_item = {
                "request_id": request.request_id,
                "prompt_hash": request.prompt_hash,
                "provider": request.provider,
                "model": request.model,
                "capabilities_hash": contract_hash(request.capabilities),
                "created_at": request.created_at,
                "response": {
                    "status": response.status,
                    "response_hash": response.response_hash,
                    "claimed_response_hash": response.claimed_response_hash,
                    "usage": dict(response.usage),
                    "created_at": response.created_at,
                } if response is not None else None,
            }
            if include_raw:
                request_item["request"] = redact_sensitive(dict(request.request))
                if response is not None:
                    request_item["response"]["payload"] = redact_sensitive(dict(response.response))
            actions.append(request_item)

        context_usage = []
        for snapshot in snapshots:
            projection = snapshot.context.get("token_projection", {})
            context_usage.append(
                {
                    "snapshot_id": snapshot.snapshot_id,
                    "context_hash": snapshot.context_hash,
                    "source_hash": snapshot.source_hash,
                    "protected_hash": snapshot.protected_hash,
                    "created_at": snapshot.created_at,
                    "token_projection": dict(projection) if isinstance(projection, Mapping) else {},
                }
            )
        compactions = [
            {
                "summary_id": item.summary_id,
                "source_message_ids": list(item.source_message_ids),
                "source_hash": item.source_hash,
                "summary_hash": item.summary_hash,
                "source_count": len(item.source_message_ids),
                "created_at": item.created_at,
            }
            for item in summaries
        ]
        lineage = {
            node.evidence_id: self.service.runtime.evidence_graph.lineage(
                run_id, node.evidence_id, direction="both", include_payload=False
            )
            for node in evidence
        }
        event_items = [
            {
                "sequence": item.sequence,
                "event_type": item.event_type,
                "created_at": item.created_at,
                "payload": dict(item.payload),
            }
            for item in events
        ]
        if not include_raw:
            event_items = [
                {
                    **item,
                    "payload_hash": contract_hash(item["payload"]),
                    "payload": {"keys": sorted(item["payload"])},
                }
                for item in event_items
            ]
        else:
            event_items = [
                {**item, "payload": redact_sensitive(item["payload"])}
                for item in event_items
            ]
        report: dict[str, Any] = {
            "schema_version": SCHEMA_VERSION,
            "run": view.to_dict(),
            "session": self.service.export_session(run_id),
            "events": {
                "count": len(event_items),
                "first_sequence": event_items[0]["sequence"] if event_items else None,
                "last_sequence": event_items[-1]["sequence"] if event_items else None,
                "items": event_items,
            },
            "model": {
                "actions": actions,
                "observations": [
                    {
                        "observation_id": item.observation_id,
                        "request_id": item.request_id,
                        "action_id": item.action_id,
                        "call_id": item.call_id,
                        "tool_name": item.tool_name,
                        "status": item.status,
                        "input_hash": item.input_hash,
                        "output_hash": item.output_hash,
                        "created_at": item.created_at,
                    }
                    for item in observations
                ],
                "usage": list(usage),
                "totals": _usage_totals(usage),
            },
            "tools": {
                "revision": catalog.revision,
                "expanded": catalog.expanded,
                "estimated_prompt_bytes": catalog.estimated_prompt_bytes,
                "selected": [str(item.qualified_name) for item in catalog.tools],
                "visibility": [item.to_dict() for item in catalog.visibility],
            },
            "context": {
                "snapshots": context_usage,
                "compaction_boundaries": compactions,
                "latest": context_usage[-1] if context_usage else None,
            },
            "artifacts": [self.service.runtime.artifacts.project(item) for item in artifacts],
            "evidence": {
                "nodes": [
                    item.to_dict() if include_raw else {
                        "evidence_id": item.evidence_id,
                        "run_id": item.run_id,
                        "action_id": item.action_id,
                        "artifact_type": item.artifact_type,
                        "target": item.target,
                        "tool": item.tool,
                        "content_hash": item.content_hash,
                        "parent_ids": list(item.parent_ids),
                        "verified": item.verified,
                        "trust": item.trust,
                    }
                    for item in evidence
                ],
                "lineage": lineage,
            },
            "terminal": view.terminal.to_dict(),
        }
        report["report_hash"] = contract_hash(report)
        return report


__all__ = ["SCHEMA_VERSION", "TransparencyProjector"]
