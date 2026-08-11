from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from ..core import ToolResult, contract_hash
from ..core.contracts import json_value
from ..runtime.conversation_records import (
    ContextSnapshotRecord,
    ContextSummaryRecord,
    ConversationMessageRecord,
)
from ..runtime.model_common import utc_now
from .contracts import AgentRunView, StartRequest


@dataclass(frozen=True, slots=True)
class ContextSelection:
    run_id: str
    messages: tuple[Mapping[str, Any], ...]
    protected_context: Mapping[str, Any]
    source_message_ids: tuple[str, ...]
    summary_ids: tuple[str, ...]
    source_hash: str
    protected_hash: str
    context_hash: str


class ConversationLedger:
    def __init__(self, store: Any) -> None:
        self.store = store

    def append(
        self,
        *,
        run_id: str,
        role: str,
        content: Any,
        protected: bool,
        source_type: str,
        source_id: str,
        metadata: Mapping[str, Any] | None = None,
    ) -> ConversationMessageRecord:
        normalized = json_value(content, field="conversation.content")
        message_id = "message-" + contract_hash(
            {"run_id": run_id, "source_type": source_type, "source_id": source_id}
        )[:32]
        return self.store.append_conversation_message(
            ConversationMessageRecord(
                message_id=message_id,
                run_id=run_id,
                sequence=0,
                role=role,
                content=normalized,
                content_hash=contract_hash(normalized),
                protected=protected,
                source_type=source_type,
                source_id=source_id,
                created_at=utc_now(),
                metadata=dict(metadata or {}),
            )
        )

    def record_start(self, view: AgentRunView, request: StartRequest) -> None:
        self.append(
            run_id=view.run.run_id,
            role="user",
            content={
                "objective": request.objective,
                "targets": list(view.goal.targets),
                "constraints": dict(request.constraints),
                "success_predicates": [dict(item) for item in request.success_predicates],
            },
            protected=True,
            source_type="original_goal",
            source_id=view.goal.goal_id,
        )

    def record_model_request(self, request: Any) -> None:
        for index, message in enumerate(request.messages):
            self.append(
                run_id=request.run_id,
                role=str(message.get("role") or "user"),
                content=message.get("content"),
                protected=False,
                source_type="model_request_projection",
                source_id=f"{request.request_id}:{index}",
                metadata={"request_id": request.request_id, "ordinal": index},
            )

    def record_model_response(self, run_id: str, response: Any) -> None:
        self.append(
            run_id=run_id,
            role="assistant",
            content={
                "text": response.text,
                "structured_output": dict(response.structured_output),
                "tool_calls": [dict(item) for item in response.tool_calls],
                "status": response.status,
                "finish_reason": response.finish_reason,
            },
            protected=False,
            source_type="model_response",
            source_id=response.request_id,
            metadata={"provider": response.provider, "model": response.model},
        )

    def record_tool_results(self, request_id: str, run_id: str, results: Sequence[ToolResult]) -> None:
        for result in results:
            self.append(
                run_id=run_id,
                role="tool",
                content=result.to_dict(),
                protected=False,
                source_type="tool_result",
                source_id=f"{request_id}:{result.call_id}",
                metadata={"request_id": request_id, "call_id": result.call_id},
            )

    def messages(self, run_id: str) -> tuple[ConversationMessageRecord, ...]:
        return self.store.conversation_messages(run_id)


class TraceableCompactor:
    def __init__(self, store: Any) -> None:
        self.store = store

    def compact(
        self,
        run_id: str,
        message_ids: Sequence[str] = (),
    ) -> ContextSummaryRecord | None:
        all_messages = self.store.conversation_messages(run_id)
        by_id = {item.message_id: item for item in all_messages}
        if message_ids:
            selected = tuple(by_id[item] for item in message_ids)
        else:
            selected = tuple(
                item
                for item in all_messages
                if not item.protected and item.source_type != "model_request_projection"
            )
        if not selected:
            return None
        if any(item.protected for item in selected):
            raise ValueError("protected_context_cannot_be_compacted")
        source_hash = self.store.context_source_hash(selected)
        summary_id = "summary-" + source_hash[:32]
        for existing in self.store.context_summaries(run_id):
            if existing.summary_id == summary_id:
                return existing
        entries = []
        for item in selected:
            rendered = json.dumps(item.content, ensure_ascii=False, sort_keys=True, default=str)
            entries.append(
                {
                    "message_id": item.message_id,
                    "role": item.role,
                    "content_hash": item.content_hash,
                    "preview": rendered[:256],
                }
            )
        summary = {
            "kind": "traceable_context_summary",
            "source_count": len(selected),
            "entries": entries,
        }
        record = ContextSummaryRecord(
            summary_id=summary_id,
            run_id=run_id,
            source_message_ids=tuple(item.message_id for item in selected),
            source_hash=source_hash,
            summary=summary,
            summary_hash=contract_hash(summary),
            created_at=utc_now(),
        )
        self.store.save_context_summary(record)
        return record


class ContextSelector:
    def __init__(
        self,
        service: Any,
        ledger: ConversationLedger,
        compactor: TraceableCompactor,
        *,
        default_max_messages: int = 32,
        compaction_threshold: int = 48,
    ) -> None:
        self.service = service
        self.ledger = ledger
        self.compactor = compactor
        self.default_max_messages = max(1, int(default_max_messages))
        self.compaction_threshold = max(2, int(compaction_threshold))

    def model_messages(self, view: AgentRunView) -> tuple[Mapping[str, Any], ...]:
        run_id = view.run.run_id
        self.ledger.append(
            run_id=run_id,
            role="system",
            content=(
                "You are the tactical planner. Runtime owns state, evidence promotion, "
                "budgets, cleanup, and terminal decisions. Use native tool calls for the "
                "current action; model text is never verified evidence."
            ),
            protected=True,
            source_type="system_base",
            source_id="model-loop-v1",
        )
        self.ledger.append(
            run_id=run_id,
            role="user",
            content={
                "objective": view.goal.objective,
                "targets": list(view.goal.targets),
                "run_id": run_id,
                "action_id": view.next_action,
                "missing_capabilities": list(view.missing_capabilities),
                "evidence_refs": [item.evidence_id for item in view.evidence],
            },
            protected=False,
            source_type="action_prompt",
            source_id=f"{view.next_action}:{view.run.state_version}",
        )
        return self.select(view).messages

    def select(
        self,
        view: AgentRunView,
        *,
        max_messages: int | None = None,
    ) -> ContextSelection:
        limit = self.default_max_messages if max_messages is None else max(0, int(max_messages))
        messages = self.ledger.messages(view.run.run_id)
        candidates = tuple(
            item for item in messages if item.source_type != "model_request_projection"
        )
        protected_messages = tuple(item for item in candidates if item.protected)
        unprotected = tuple(item for item in candidates if not item.protected)
        if len(unprotected) > self.compaction_threshold:
            compact_count = max(1, len(unprotected) - limit)
            self.compactor.compact(
                view.run.run_id,
                [item.message_id for item in unprotected[:compact_count]],
            )
        summaries = self.store_summaries(view.run.run_id)
        selected = unprotected[-limit:] if limit else ()
        protected_context = self._protected_context(view)
        protected_hash = contract_hash(protected_context)
        source_messages = (*protected_messages, *selected)
        source_projection: list[Mapping[str, Any]] = [
            {"message_id": item.message_id, "content_hash": item.content_hash}
            for item in source_messages
        ]
        source_projection.extend(
            {"summary_id": item.summary_id, "summary_hash": item.summary_hash}
            for item in summaries[-1:]
        )
        source_hash = contract_hash(source_projection)
        projected: list[Mapping[str, Any]] = [
            {"role": "system", "content": {"protected_context": protected_context}}
        ]
        projected.extend(
            {"role": item.role, "content": item.content}
            for item in protected_messages
        )
        if summaries:
            latest = summaries[-1]
            projected.append(
                {
                    "role": "system",
                    "content": {
                        "context_summary": dict(latest.summary),
                        "summary_id": latest.summary_id,
                        "source_hash": latest.source_hash,
                    },
                }
            )
        projected.extend({"role": item.role, "content": item.content} for item in selected)
        context = {
            "messages": projected,
            "source_message_ids": [item.message_id for item in source_messages],
            "summary_ids": [item.summary_id for item in summaries[-1:]],
            "source_hash": source_hash,
            "protected_hash": protected_hash,
        }
        context_hash = contract_hash(context)
        snapshot_id = "context-" + context_hash[:32]
        if not any(
            item.snapshot_id == snapshot_id
            for item in self.service.runtime.store.context_snapshots(view.run.run_id)
        ):
            self.service.runtime.store.save_context_snapshot(
                ContextSnapshotRecord(
                    snapshot_id=snapshot_id,
                    run_id=view.run.run_id,
                    source_hash=source_hash,
                    protected_hash=protected_hash,
                    context_hash=context_hash,
                    context=context,
                    created_at=utc_now(),
                )
            )
        return ContextSelection(
            run_id=view.run.run_id,
            messages=tuple(projected),
            protected_context=protected_context,
            source_message_ids=tuple(item.message_id for item in source_messages),
            summary_ids=tuple(item.summary_id for item in summaries[-1:]),
            source_hash=source_hash,
            protected_hash=protected_hash,
            context_hash=context_hash,
        )

    def store_summaries(self, run_id: str) -> tuple[ContextSummaryRecord, ...]:
        return self.service.runtime.store.context_summaries(run_id)

    def _protected_context(self, view: AgentRunView) -> dict[str, Any]:
        state = self.service.runtime.store.load_operation(view.run.run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{view.run.run_id}")
        verified_refs = [
            item.evidence_id
            for item in view.evidence
            if item.verified and item.trust in {"runtime_verified", "derived_verified"}
        ]
        return {
            "original_goal": {
                "goal_id": view.goal.goal_id,
                "objective": view.goal.objective,
                "targets": list(view.goal.targets),
                "criteria": [item.to_dict() for item in view.goal.criteria],
                "constraints": dict(view.goal.constraints),
                "success_predicates": [dict(item) for item in view.goal.success_predicates],
            },
            "unsatisfied_clauses": list(view.terminal.missing),
            "active_plan": {
                "plan_id": state.plan_id,
                "revision": state.plan_revision,
                "branch_id": state.branch_id,
                "current_action_id": state.current_action_id,
                "snapshot": dict(state.plan_snapshot),
            },
            "critical_evidence_refs": verified_refs,
            "irreversible_state": {
                "completed_actions": sorted(
                    key for key, value in state.action_status.items() if value in {"completed", "skipped"}
                ),
                "successful_tools": {
                    key: list(value)
                    for key, value in state.action_tools_succeeded.items()
                    if value
                },
                "cleanup_status": state.cleanup_status,
                "cancel_reason": state.cancel_reason,
                "terminal_reason": state.terminal_reason,
            },
        }
