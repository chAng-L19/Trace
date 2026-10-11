from __future__ import annotations

from collections.abc import Mapping
from importlib.resources import files
from typing import Any


TACTICAL_SYSTEM = files("redteam_agent").joinpath("prompts/agent.md").read_text(encoding="utf-8")


def current_task(service: Any, view: Any, state: Any) -> dict[str, Any]:
    """Rebuild the branch's task anchor from original records, never summaries."""
    requests = {item.request_id: item for item in service.journal.model_requests(view.run.run_id)}
    focus: dict[str, Any] = {"authority": "model_declared_not_evidence", "status": "unset"}
    for response in reversed(service.journal.model_responses(view.run.run_id)):
        request = requests.get(response.request_id)
        if response.status not in {"completed", "success"} or request is None:
            continue
        metadata = request.request.get("metadata", {})
        if metadata.get("branch_id") != state.branch_id:
            continue
        update = response.response.get("structured_output", {}).get("tactical_update")
        if not isinstance(update, Mapping) or "active_hypothesis_id" not in update:
            continue
        identity = update["active_hypothesis_id"]
        focus["source_request_id"] = response.request_id
        if not isinstance(identity, str):
            focus["status"] = "invalid_declaration"
            break
        identity = identity.strip()
        if not identity:
            focus["status"] = "cleared"
            break
        record = next((item for item in service.exploration.current(view.run.run_id)
                       if item.hypothesis_id == identity), None)
        if record is None:
            focus["status"] = "unresolved_reference"
            break
        focus.update({"hypothesis_id": identity, "record_id": record.record_id,
                      "status": record.status, "statement": record.statement,
                      "target": record.target, "intent_id": record.metadata.get("intent_id", "")})
        break
    return {"run_id": view.run.run_id, "goal_id": view.goal.goal_id,
            "runtime_branch_id": state.branch_id,
            "session_branch_id": service.journal.active_branch_id(view.run.run_id),
            "action_id": view.next_action, "plan_revision": state.plan_revision,
            "focus": focus}
