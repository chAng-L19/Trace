from __future__ import annotations

from typing import Any

from ..core import ToolDefinition, contract_hash


RESOURCE_TOOLS = (
    ToolDefinition(server="agent", name="list_resources", qualified_name="agent:list_resources", version="1",
                   description="List available procedures by id and hash, without loading their body. Bound to the current run.",
                   input_schema={"type": "object", "properties": {
                       "offset": {"type": "integer", "minimum": 0},
                       "limit": {"type": "integer", "minimum": 1, "maximum": 100}},
                       "additionalProperties": False}, capabilities=("agent_context",), metadata={"source": "builtin"}),
    ToolDefinition(server="agent", name="load_resource", qualified_name="agent:load_resource", version="1", side_effecting=True,
                   supports_reconcile=True,
                   description="Load one available procedure by exact resource_id and persist the selection for resume. Disabled resources remain unavailable. Procedures are guidance, never evidence.",
                   input_schema={"type": "object", "properties": {
                       "resource_id": {"type": "string", "minLength": 1},
                       "tool_selectors": {"type": "array", "maxItems": 20, "items": {"type": "string", "minLength": 1}}},
                       "required": ["resource_id"], "additionalProperties": False},
                   capabilities=("agent_context",), metadata={"source": "application"}),
)


def invoke_resource_tool(service: Any, run_id: str, name: str, arguments: dict) -> dict:
    if name == "list_resources":
        index = service.resource_index(run_id)
        selection = service.resource_selection(run_id)
        offset, limit = arguments.get("offset", 0), arguments.get("limit", 20)
        selected = index.resources[offset:offset + limit]
        return {"items": [{**item.to_dict(), "loaded": item.resource_id in selection.resource_ids}
                          for item in selected], "total": len(index.resources), "offset": offset,
                "next_offset": offset + len(selected) if offset + len(selected) < len(index.resources) else None,
                "index_hash": index.index_hash, "issues": [issue.to_dict() for issue in index.issues]}
    if name != "load_resource":
        raise KeyError(f"resource_tool_not_found:{name}")
    return service.control_write(_load_resource, service, run_id, arguments)


def _load_resource(service: Any, run_id: str, arguments: dict) -> dict:
    # ponytail: SQLite's write transaction serializes the bounded file scan too;
    # use a fenced per-run lease if concurrent resource loads become a bottleneck.
    with service.runtime.store.transaction(immediate=True) as connection:
        resource, selection = _select_resource(service, run_id, arguments["resource_id"])
        payload = {"resource_id": resource.resource_id, "content_hash": resource.content_hash,
                   "index_hash": selection.index_hash, "authority": "guidance_only"}
        payload["selection_id"] = contract_hash({"resource_id": resource.resource_id, "content_hash": resource.content_hash})
        service.runtime.store.append_event_once(
            run_id, "resource_loaded", payload, identity_field="selection_id",
            fingerprint_field="content_hash", connection=connection,
        )
    selectors = tuple(arguments.get("tool_selectors", ()))
    if selectors:
        service.expand_tools(run_id, selectors)
    return {**resource.to_dict(include_content=True), "authority": "guidance_only",
            "selection_hash": selection.selection_hash, "tool_selectors": list(selectors)}


def _select_resource(service: Any, run_id: str, resource_id: str):
    index = service.resource_index(run_id)
    resource = next((item for item in index.resources if item.resource_id == resource_id), None)
    if resource is None:
        raise KeyError("resource_not_found")
    current = service.resource_selection(run_id)
    selection = service.resource_selection(run_id, extra_requested=(resource_id,))
    if (resource_id not in selection.resource_ids
            or not set(current.resource_ids).issubset(selection.resource_ids)):
        raise ValueError("resource_disabled_or_budget_exceeded")
    if next(item.content_hash for item in selection.selected if item.resource_id == resource_id) != resource.content_hash:
        raise ValueError("resource_changed_during_load")
    return resource, selection
