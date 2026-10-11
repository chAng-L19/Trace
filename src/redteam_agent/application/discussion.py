from __future__ import annotations

"""Terminal follow-ups: conversation/model records only, no runtime execution."""

import copy
import json
import threading
from contextlib import contextmanager
from dataclasses import replace
from typing import Any

from ..core import ModelRequest, ModelResponse, contract_hash
from ..runtime.model_common import utc_now
from ..runtime.security import redact_sensitive, safe_error_text
from ..runtime.session_journal import ModelRequestRecord, ModelResponseRecord, SessionJournal
from .context import ConversationLedger
from .model_loop import AgentLoop, ModelInterruptedError
from .asset_graph import _eligible_evidence, _project_typed_graph


@contextmanager
def _branch_records(store, run_id, branch_id=None):
    """Bind existing stores to one transaction, pinning writes to the origin branch.

    A branch override is transaction-local and restored before commit; concurrent
    checkout is never undone. The shared store object is not patched.
    """
    with store.transaction(immediate=True) as connection:
        row = connection.execute("SELECT active_branch_id FROM session_journal_state WHERE run_id=?", (run_id,)).fetchone()
        active = str(row[0]) if row else "main"
        selected = branch_id or active
        if selected != active:
            connection.execute("UPDATE session_journal_state SET active_branch_id=? WHERE run_id=?", (selected, run_id))

        @contextmanager
        def borrowed(**kwargs):
            yield connection

        bound = copy.copy(store)
        bound.connection = bound.transaction = borrowed
        ledger = ConversationLedger(bound, journal=SessionJournal(bound))
        yield bound, ledger, selected
        if selected != active:
            connection.execute("UPDATE session_journal_state SET active_branch_id=? WHERE run_id=?", (active, run_id))


def _result_context(service, run_id):
    """Use the report's evidence gate, projecting conclusions and references only."""
    view = service.status(run_id)
    graph = _project_typed_graph(run_id, _eligible_evidence(service, run_id))
    terminal = view.terminal.to_dict()
    return {
        "run_id": run_id, "status": view.run.status,
        "goal": {"objective": view.goal.objective, "targets": list(view.goal.targets)},
        "terminal": {key: terminal[key] for key in
                     ("terminal", "success", "reason", "satisfied", "missing", "evidence_refs")},
        "verified_findings": [
            {key: finding[key] for key in
             ("finding_id", "title", "severity", "status", "target", "description",
              "reproduction_evidence_ids", "impact_evidence_ids", "negative_control_evidence_ids", "cleanup_evidence_ids")}
            for finding in graph["findings"]
        ],
        "interpretation": (
            "Findings passed the report evidence gate; descriptions and severity remain declared. "
            "No verified findings does not establish that the target is safe. "
            "This is a read-only summary of the finished run, not a new execution."),
    }


def _history(service, ledger, run_id):
    # Request projections already contain earlier messages. Feeding them back
    # creates recursive history growth; only canonical records enter this prompt.
    messages = [{"role": "system", "content": (
        "Discuss the finished run using the conversation below. Answer the user's follow-up. "
        "Do not execute actions, call tools, or claim to have rerun the task.") },
        {"role": "system", "content": json.dumps(
            {"terminal_result": _result_context(service, run_id)}, ensure_ascii=False)}]
    for item in ledger.messages(run_id):
        if item.source_type == "model_request_projection":
            continue
        content = item.content
        if isinstance(content, dict):
            content = content.get("text") or json.dumps(content, ensure_ascii=False)
        elif not isinstance(content, str):
            content = json.dumps(content, ensure_ascii=False)
        # Historical tool results are context, not pending protocol tool calls.
        role = item.role if item.role in {"user", "assistant"} else "user"
        messages.append({"role": role, "content": content})
    return tuple(messages)


def _stored_response(store, run_id, request_id):
    record = next((item for item in store.model_responses(run_id) if item.request_id == request_id), None)
    if record is None:
        return None
    return replace(ModelResponse.from_dict(record.response), status=record.status, response_hash=record.response_hash)


def _save_response(store, ledger, loop, request, response):
    response = ModelResponse.from_dict(loop.service.runtime.project_model_credentials(request.run_id, response.to_dict()))
    claimed = response.response_hash
    response = replace(response, response_hash=contract_hash(loop._response_projection(response)))
    store.save_model_response(ModelResponseRecord(
        request_id=request.request_id, run_id=request.run_id, status=response.status,
        provider=response.provider or type(loop.model).__name__, model=response.model or request.model,
        response_hash=response.response_hash, claimed_response_hash=claimed,
        usage=response.usage, response=response.to_dict(), created_at=utc_now(),
    ))
    ledger.record_model_response(request.run_id, response)
    return response


def discuss(service: Any, run_id: str, text: str, *, message_id: str) -> dict[str, Any]:
    """Return {message: user_record.to_dict(), response: model_response.to_dict()}.

    message_id is required and scoped to run + current branch. Replays return the
    saved outcome (including failures); changed text conflicts. An orphaned saved
    request is recorded as interrupted/unknown, never automatically sent again.
    """
    if not isinstance(text, str) or not text.strip():
        raise ValueError("message_text_required")
    if not isinstance(message_id, str) or not message_id.strip():
        raise ValueError("message_id_required")
    with service._model_condition:
        service._ensure_open()
        if not hasattr(service, "_discussion_locks"):
            service._discussion_locks = {}
            service._discussion_runs = {}
        lock = service._discussion_locks.setdefault(run_id, threading.RLock())
    with lock:
        with service._model_condition:
            service._ensure_open()
            state = service.runtime.store.load_operation(run_id)
            if state is None:
                raise KeyError(f"operation_not_found:{run_id}")
            if state.status not in {"completed", "failed", "cancelled"}:
                raise ValueError(f"discussion_requires_terminal:{state.status}")
            configured = service.agent_loop
            # Replays are allowed even if the provider has since been removed.
            loop = AgentLoop(service=service, model=configured.model if configured else None,
                             model_name=configured.model_name if configured else "", tools=None)
            service._active_model_loops.setdefault(run_id, {})[loop] = 1
            service._discussion_runs[run_id] = 1
        try:
            return _discuss_registered(service, loop, run_id, text, message_id)
        finally:
            with service._model_condition:
                del service._active_model_loops[run_id][loop]
                if not service._active_model_loops[run_id]:
                    del service._active_model_loops[run_id]
                del service._discussion_runs[run_id]
                service._model_condition.notify_all()


def _discuss_registered(service, loop, run_id, text, message_id):
    store = service.runtime.store
    with _branch_records(store, run_id) as (bound, ledger, branch):
        identity = contract_hash({"run_id": run_id, "branch_id": branch, "message_id": message_id})
        request_id = "discussion-" + identity[:32]
        content = service.runtime.project_model_credentials(run_id, {"text": text})
        previous = next((item for item in bound.model_requests(run_id) if item.request_id == request_id), None)
        if previous is None and loop.model is None:
            raise ValueError("model_provider_required")
        message = ledger.append(run_id=run_id, role="user", content=content, protected=False,
            source_type="discussion_message", source_id=identity, metadata={"branch_id": branch, "message_id": message_id})
        response = _stored_response(bound, run_id, request_id)
        if response is not None:
            ledger.record_model_response(run_id, response)
            return {"message": message.to_dict(), "response": response.to_dict()}
        if previous is not None:
            request = ModelRequest.from_dict(previous.request)
            response = _save_response(bound, ledger, loop, request, ModelResponse(
                request_id=request_id, status="interrupted", model=request.model,
                error="discussion_outcome_unknown"))
            return {"message": message.to_dict(), "response": response.to_dict()}
        capabilities = loop.model.capabilities()
        request = ModelRequest(request_id=request_id, run_id=run_id, messages=_history(service, ledger, run_id),
            model=loop.model_name or str(capabilities.metadata.get("model") or ""),
            metadata={"discussion": True, "branch_id": branch, "message_id": message_id})
        bound.save_model_request(ModelRequestRecord(request_id=request_id, run_id=run_id,
            prompt_hash=contract_hash(redact_sensitive(loop._prompt_projection(request))),
            provider=str(capabilities.metadata.get("provider") or type(loop.model).__name__),
            model=request.model, capabilities=capabilities.to_dict(), request=request.to_dict(), created_at=utc_now()))
    try:
        with loop._lock:
            loop._active_requests.setdefault(run_id, set()).add(request_id)
        if service.closing or loop._is_interrupted(run_id):
            raise ModelInterruptedError("discussion_interrupted")
        response = loop.model.complete(request)
        if service.closing or loop._is_interrupted(run_id):
            raise ModelInterruptedError("discussion_interrupted")
        if response.request_id != request_id:
            raise ValueError("model_response_request_mismatch")
        response = replace(response, usage=loop._normalize_usage(response.usage))
        if response.response_hash and response.response_hash != contract_hash(loop._response_projection(response)):
            raise ValueError("model_response_hash_mismatch")
        if response.tool_calls:
            response = replace(response, status="failed", error="discussion_tools_not_allowed", response_hash="")
    except Exception as exc:
        response = ModelResponse(request_id=request_id,
            status="interrupted" if service.closing or loop._is_interrupted(run_id) else "failed",
            model=request.model, error=safe_error_text(service.runtime._credential_vault.project(str(exc))))
    finally:
        with loop._lock:
            loop._active_requests.pop(run_id, None)
    with _branch_records(store, run_id, branch) as (bound, ledger, _):
        # A second service may have claimed this same durable command meanwhile.
        saved = _stored_response(bound, run_id, request_id)
        response = saved or _save_response(bound, ledger, loop, request, response)
    return {"message": message.to_dict(), "response": response.to_dict()}
