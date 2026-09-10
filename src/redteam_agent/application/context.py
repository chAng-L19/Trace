from __future__ import annotations

import json
import math
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
from .bounded_output import BoundedOutput
from .resources import resource_context_metadata, resource_context_projection
from .tool_projection import ToolObservationProjector
@dataclass(frozen=True, slots=True)
class ContextBudget:
    """The single context-window budget used by selection and compaction."""

    window_tokens: int = 0
    reserved_output_tokens: int = 0
    keep_recent_messages: int = 32
    fallback_bytes_per_token: int = 3
    max_compaction_retries: int = 1

    @classmethod
    def from_values(
        cls,
        *,
        window_tokens: int = 0,
        reserved_output_tokens: int | None = None,
        keep_recent_messages: int = 32,
        fallback_bytes_per_token: int = 3,
        max_compaction_retries: int = 1,
    ) -> "ContextBudget":
        window = max(0, int(window_tokens))
        reserve = (
            max(0, int(reserved_output_tokens))
            if reserved_output_tokens is not None
            else (max(1024, min(32768, window // 8)) if window else 0)
        )
        return cls(
            window_tokens=window,
            reserved_output_tokens=reserve,
            keep_recent_messages=max(0, int(keep_recent_messages)),
            fallback_bytes_per_token=max(1, int(fallback_bytes_per_token)),
            max_compaction_retries=max(0, int(max_compaction_retries)),
        )

    @property
    def available_tokens(self) -> int:
        return max(0, self.window_tokens - self.reserved_output_tokens)

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
    estimated_context_tokens: int = 0
    provider_context_tokens: int | None = None
    selected_tokens: int = 0
    reserved_output_tokens: int = 0
    projection_bytes: int = 0
    cache_read_tokens: int | None = None
    cache_write_tokens: int | None = None
    context_overflow_tokens: int = 0
    compaction_ids: tuple[str, ...] = ()
    overflow_retry: int = 0
    resource_index_hash: str = ""
    resource_selection_hash: str = ""
    resource_ids: tuple[str, ...] = ()
    resource_tokens: int = 0

class ConversationLedger:
    def __init__(
        self,
        store: Any,
        artifacts: Any | None = None,
        projector: ToolObservationProjector | None = None,
        journal: Any | None = None,
    ) -> None:
        self.store = store
        self.artifacts = artifacts
        self.projector = projector or ToolObservationProjector()
        self.journal = journal
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

    def record_tool_results(
        self,
        request_id: str,
        run_id: str,
        results: Sequence[ToolResult],
    ) -> Mapping[str, str]:
        artifact_ids: dict[str, str] = {}
        for result in results:
            content = result.to_dict()
            if self.artifacts is not None:
                preview = self._tool_result_preview(result)
                bounded = BoundedOutput.capture_json(content)
                try:
                    bounded.close()
                    artifact = self.artifacts.put_file(
                        bounded.path,
                        run_id=run_id,
                        artifact_type="model_tool_result",
                        media_type="application/json",
                        preview={**preview, **bounded.preview()},
                        metadata={
                            "request_id": request_id,
                            "call_id": result.call_id,
                            "tool_name": result.tool_name,
                            "output_hash": result.output_hash,
                        },
                    )
                finally:
                    bounded.discard()
                artifact_projection = self.artifacts.project(artifact)
                raw_reference = {
                    key: artifact_projection[key]
                    for key in (
                        "artifact_ref",
                        "content_hash",
                        "byte_count",
                        "media_type",
                        "artifact_type",
                    )
                    if key in artifact_projection
                }
                projection = self.projector.project(
                    result,
                    raw_artifact=raw_reference,
                )
                content = dict(projection.content)
                artifact_ids[result.call_id] = artifact.artifact_id
            self.append(
                run_id=run_id,
                role="tool",
                content=content,
                protected=False,
                source_type="tool_result",
                source_id=f"{request_id}:{result.call_id}",
                metadata={"request_id": request_id, "call_id": result.call_id},
            )
        return artifact_ids

    @staticmethod
    def _tool_result_preview(result: ToolResult) -> Mapping[str, Any]:
        bounded = BoundedOutput.capture_json(result.output)
        stats = bounded.preview()
        bounded.discard()
        return {
            "call_id": result.call_id,
            "tool_name": result.tool_name,
            "status": result.status,
            "output_bytes": stats["byte_count"],
            "head": stats["head"],
            "tail": stats["tail"],
            "truncated": stats["truncated"],
            "truncation_reason": stats["truncation_reason"],
        }

    def messages(self, run_id: str) -> tuple[ConversationMessageRecord, ...]:
        if self.journal is not None:
            return self.journal.conversation_messages(run_id)
        return self.store.conversation_messages(run_id)

class TraceableCompactor:
    def __init__(self, store: Any, *, journal: Any | None = None) -> None:
        self.store = store
        self.journal = journal

    def compact(
        self,
        run_id: str,
        message_ids: Sequence[str] = (),
    ) -> ContextSummaryRecord | None:
        all_messages = (
            self.journal.conversation_messages(run_id)
            if self.journal is not None
            else self.store.conversation_messages(run_id)
        )
        by_id = {item.message_id: item for item in all_messages}
        if message_ids:
            requested = {by_id[item].message_id for item in message_ids}
            selected = tuple(
                item
                for group in ContextSelector._atomic_groups(
                    tuple(
                        item
                        for item in all_messages
                        if item.source_type != "model_request_projection"
                    )
                )
                if any(item.message_id in requested for item in group)
                for item in group
            )
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
        active_summaries = (
            self.journal.context_summaries(run_id)
            if self.journal is not None
            else self.store.context_summaries(run_id)
        )
        for existing in active_summaries:
            if existing.source_hash == source_hash:
                return existing
        summary_identity = {
            "source_hash": source_hash,
            "parent_entry_id": self.journal.leaf_id(run_id) if self.journal is not None else None,
            "branch_id": self.journal.active_branch_id(run_id) if self.journal is not None else "",
        }
        summary_id = "summary-" + contract_hash(summary_identity)[:32]
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
        fallback_bytes_per_token: int = 3,
    ) -> None:
        self.service = service
        self.ledger = ledger
        self.compactor = compactor
        self.default_max_messages = max(1, int(default_max_messages))
        self.compaction_threshold = max(2, int(compaction_threshold))
        self.fallback_bytes_per_token = max(1, int(fallback_bytes_per_token))

    def model_messages(
        self,
        view: AgentRunView,
        *,
        max_context_tokens: int = 0,
    ) -> tuple[Mapping[str, Any], ...]:
        return self.prepare_model_context(
            view,
            max_context_tokens=max_context_tokens,
        ).messages

    def prepare_model_context(
        self,
        view: AgentRunView,
        *,
        max_context_tokens: int = 0,
        reserved_output_tokens: int | None = None,
        force_compaction: bool = False,
        overflow_retry: int = 0,
    ) -> ContextSelection:
        run_id = view.run.run_id
        self.ledger.append(
            run_id=run_id,
            role="system",
            content=(
                "You are the primary tactical agent. Runtime owns deterministic invariants, "
                "evidence promotion, budgets, cleanup, and terminal decisions. The current "
                "action is a quality gate, not a prescribed tactic. Generate and prioritize "
                "search nodes yourself, use native tool calls, preserve uncertainty, and "
                "reopen prior directions when new evidence or capability appears. Model text "
                "and exploration records are never verified evidence."
            ),
            protected=True,
            source_type="system_base",
            source_id="model-loop-v2",
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
        return self.select(
            view,
            max_context_tokens=max_context_tokens,
            reserved_output_tokens=reserved_output_tokens,
            stable_prefix=True,
            turn_boundary=True,
            force_compaction=force_compaction,
            overflow_retry=overflow_retry,
        )
    def select(
        self,
        view: AgentRunView,
        *,
        max_messages: int | None = None,
        max_context_tokens: int = 0,
        reserved_output_tokens: int | None = None,
        stable_prefix: bool = False,
        turn_boundary: bool = False,
        force_compaction: bool = False,
        overflow_retry: int = 0,
    ) -> ContextSelection:
        budget = ContextBudget.from_values(
            window_tokens=max_context_tokens,
            reserved_output_tokens=reserved_output_tokens,
            keep_recent_messages=(
                self.default_max_messages if max_messages is None else max(0, int(max_messages))
            ),
            fallback_bytes_per_token=self.fallback_bytes_per_token,
        )
        limit = budget.keep_recent_messages
        messages = self.ledger.messages(view.run.run_id)
        candidates = tuple(
            item for item in messages if item.source_type != "model_request_projection"
        )
        protected_messages = tuple(item for item in candidates if item.protected)
        unprotected = tuple(item for item in candidates if not item.protected)
        protected_context = self._protected_context(view)
        protected_hash = contract_hash(protected_context)
        usage = self._latest_usage(view.run.run_id)
        window = budget.window_tokens
        reserve = budget.reserved_output_tokens
        resource_budget = max(0, min(8192, window // 8 if window else 4096))
        resource_selection = self.service.resource_selection(
            view.run.run_id,
            token_budget=resource_budget,
        )
        system_invariant = (
            "You are the primary tactical agent. Runtime owns deterministic invariants, "
            "evidence promotion, budgets, cleanup, and terminal decisions. The current "
            "action is a quality gate, not a prescribed tactic. Generate and prioritize "
            "search nodes yourself, use native tool calls, preserve uncertainty, and reopen "
            "prior directions when new evidence or capability appears. Model text and "
            "exploration records are never verified evidence."
        )
        fixed_projection = (
            [
                {"role": "system", "content": {"system_invariant": system_invariant}},
                {"role": "system", "content": {"protected_context": protected_context}},
            ]
            if stable_prefix
            else [
                {
                    "role": "system",
                    "content": {
                        "system_invariant": system_invariant,
                        "protected_context": protected_context,
                    },
                }
            ]
        )
        fixed_projection = resource_context_projection(
            resource_selection,
            fixed_projection,
            stable_prefix=stable_prefix,
        )
        fixed_tokens = self._estimate_tokens(fixed_projection)
        groups = self._atomic_groups(unprotected)
        if force_compaction and groups:
            selected = tuple(groups[-1])
        elif max_messages is not None:
            if not limit:
                selected = ()
            else:
                chosen_groups: list[tuple[ConversationMessageRecord, ...]] = []
                count = 0
                for group in reversed(groups):
                    if chosen_groups and count + len(group) > limit:
                        break
                    chosen_groups.append(group)
                    count += len(group)
                selected = tuple(item for group in reversed(chosen_groups) for item in group)
        elif window:
            available = max(0, window - reserve - fixed_tokens)
            chosen: list[tuple[ConversationMessageRecord, ...]] = []
            consumed = 0
            for group in reversed(groups):
                group_tokens = self._estimate_tokens(
                    [{"role": item.role, "content": item.content} for item in group]
                )
                if chosen and consumed + group_tokens > available:
                    break
                chosen.append(group)
                consumed += group_tokens
            selected = tuple(item for group in reversed(chosen) for item in group)
        else:
            if not limit:
                selected = ()
            else:
                chosen_groups = []
                count = 0
                for group in reversed(groups):
                    if chosen_groups and count + len(group) > limit:
                        break
                    chosen_groups.append(group)
                    count += len(group)
                selected = tuple(item for group in reversed(chosen_groups) for item in group)
        selected_ids = {item.message_id for item in selected}
        excluded = tuple(item for item in unprotected if item.message_id not in selected_ids)
        provider_context_tokens = usage.get("input_tokens")
        summaries_before = self.store_summaries(view.run.run_id)
        summarized_ids = {
            message_id
            for summary in summaries_before
            for message_id in summary.source_message_ids
        }
        compaction_candidates = tuple(
            item for item in excluded if item.message_id not in summarized_ids
        )
        should_compact = turn_boundary and bool(compaction_candidates) and (
            force_compaction
            or (
            len(unprotected) > self.compaction_threshold
            or (window and (self._estimate_tokens([{"role": item.role, "content": item.content} for item in unprotected]) + fixed_tokens + reserve > window))
            or (window and isinstance(provider_context_tokens, int) and provider_context_tokens + reserve > window)
            )
        )
        compaction_ids: list[str] = []
        if should_compact:
            summary = self.compactor.compact(
                view.run.run_id,
                [item.message_id for item in compaction_candidates],
            )
            if summary is not None:
                compaction_ids.append(summary.summary_id)
                self.service.exploration.build_recon_digest(
                    view.run.run_id,
                    source_message_ids=tuple(item.message_id for item in compaction_candidates),
                )
        summaries = self.store_summaries(view.run.run_id)
        chosen_summaries: tuple[ContextSummaryRecord, ...] = ()
        source_messages = (*protected_messages, *selected)
        source_projection: list[Mapping[str, Any]] = [
            {"message_id": item.message_id, "content_hash": item.content_hash}
            for item in source_messages
        ]
        resource_metadata = resource_context_metadata(resource_selection)
        source_projection.append(resource_metadata)
        projected: list[Mapping[str, Any]] = list(fixed_projection)
        projected.extend(
            {"role": item.role, "content": item.content}
            for item in protected_messages
            if item.source_type not in {"system_base", "original_goal"}
        )
        if summaries:
            latest = summaries[-1]
            summary_message = {
                "role": "system",
                "content": {
                    "context_summary": dict(latest.summary),
                    "summary_id": latest.summary_id,
                    "source_hash": latest.source_hash,
                },
            }
            projected_with_summary = [*projected, summary_message]
            projected_with_summary.extend(
                {"role": item.role, "content": item.content} for item in selected
            )
            if not window or self._estimate_tokens(projected_with_summary) + reserve <= window:
                projected.append(summary_message)
                chosen_summaries = (latest,)
        projected.extend({"role": item.role, "content": item.content} for item in selected)
        source_projection.extend(
            {"summary_id": item.summary_id, "summary_hash": item.summary_hash}
            for item in chosen_summaries
        )
        for summary in chosen_summaries:
            if summary.summary_id not in compaction_ids:
                compaction_ids.append(summary.summary_id)
        if force_compaction and summaries:
            latest_summary_id = summaries[-1].summary_id
            if latest_summary_id not in compaction_ids:
                compaction_ids.append(latest_summary_id)
        source_hash = contract_hash(source_projection)
        projection_bytes = len(
            json.dumps(projected, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
        )
        estimated_tokens = self._estimate_tokens(projected)
        overflow_tokens = max(0, estimated_tokens + reserve - window) if window else 0
        selected_tokens = self._estimate_tokens(
            [{"role": item.role, "content": item.content} for item in selected]
        )
        context = {
            "messages": projected,
            "source_message_ids": [item.message_id for item in source_messages],
            "summary_ids": [item.summary_id for item in summaries[-1:]],
            "source_hash": source_hash,
            "protected_hash": protected_hash,
            "token_projection": {
                "estimated_context_tokens": estimated_tokens,
                "provider_context_tokens": provider_context_tokens,
                "selected_tokens": selected_tokens,
                "reserved_output_tokens": reserve,
                "projection_bytes": projection_bytes,
                "cache_read_tokens": usage.get("cache_read_tokens"),
                "cache_write_tokens": usage.get("cache_write_tokens"),
                "context_overflow_tokens": overflow_tokens,
                "compaction_ids": list(compaction_ids),
                "overflow_retry": max(0, int(overflow_retry)),
                **resource_metadata,
            },
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
            summary_ids=tuple(item.summary_id for item in chosen_summaries),
            source_hash=source_hash,
            protected_hash=protected_hash,
            context_hash=context_hash,
            estimated_context_tokens=estimated_tokens,
            provider_context_tokens=provider_context_tokens,
            selected_tokens=selected_tokens,
            reserved_output_tokens=reserve,
            projection_bytes=projection_bytes,
            cache_read_tokens=usage.get("cache_read_tokens"),
            cache_write_tokens=usage.get("cache_write_tokens"),
            context_overflow_tokens=overflow_tokens,
            compaction_ids=tuple(compaction_ids),
            overflow_retry=max(0, int(overflow_retry)),
            **resource_metadata,
        )

    def _estimate_tokens(self, value: Any) -> int:
        raw = json.dumps(value, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
        return max(1, math.ceil(len(raw) / self.fallback_bytes_per_token)) if raw else 0

    @staticmethod
    def _atomic_groups(
        messages: Sequence[ConversationMessageRecord],
    ) -> tuple[tuple[ConversationMessageRecord, ...], ...]:
        groups: dict[str, list[ConversationMessageRecord]] = {}
        order: list[str] = []
        for item in messages:
            request_id = str(item.metadata.get("request_id") or "")
            if not request_id and item.source_type == "model_response":
                request_id = item.source_id
            key = f"request:{request_id}" if request_id else f"message:{item.message_id}"
            if key not in groups:
                groups[key] = []
                order.append(key)
            groups[key].append(item)
        return tuple(tuple(groups[key]) for key in order)

    def _latest_usage(self, run_id: str) -> dict[str, int | None]:
        result: dict[str, int | None] = {
            "input_tokens": None,
            "cache_read_tokens": None,
            "cache_write_tokens": None,
        }
        responses = self.service.journal.model_responses(run_id)
        if not responses:
            return result
        usage = responses[-1].usage
        aliases = {
            "input_tokens": ("input_tokens", "prompt_tokens"),
            "cache_read_tokens": ("cache_read_tokens", "cached_input_tokens"),
            "cache_write_tokens": ("cache_write_tokens",),
        }
        for target, names in aliases.items():
            for name in names:
                value = usage.get(name)
                if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                    result[target] = value
                    break
        return result

    def store_summaries(self, run_id: str) -> tuple[ContextSummaryRecord, ...]:
        return self.service.journal.context_summaries(run_id)

    def _protected_context(self, view: AgentRunView) -> dict[str, Any]:
        state = self.service.runtime.store.load_operation(view.run.run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{view.run.run_id}")
        verified_refs = [
            item.evidence_id
            for item in view.evidence
            if item.verified and item.trust in {"runtime_verified", "derived_verified"}
        ]
        recon_digests = self.service.journal.recon_digests(view.run.run_id)
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
                "retained_tactical_state": self._retained_tactical_state(view.run.run_id),
                "tactical_ledger": self.service.exploration.projection(view.run.run_id),
                "latest_recon_digest": (
                    dict(recon_digests[-1].digest) if recon_digests else {}
                ),
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

    def _retained_tactical_state(self, run_id: str) -> dict[str, Any]:
        hypotheses: list[Mapping[str, Any]] = []
        evidence_refs: list[str] = []
        artifact_refs: list[str] = []
        seen_hypotheses: set[str] = set()

        def visit(value: Any, key: str = "") -> None:
            if isinstance(value, Mapping):
                if key == "hypotheses":
                    for item in value.values():
                        visit(item, "hypotheses")
                for item_key, item_value in value.items():
                    normalized = str(item_key)
                    if normalized in {"evidence_ref", "evidence_id"} and isinstance(item_value, str):
                        evidence_refs.append(item_value)
                    elif normalized == "artifact_ref" and isinstance(item_value, str):
                        artifact_refs.append(item_value)
                    elif normalized == "hypotheses" and isinstance(item_value, Sequence) and not isinstance(item_value, (str, bytes)):
                        for hypothesis in item_value:
                            if isinstance(hypothesis, Mapping):
                                digest = contract_hash(hypothesis)
                                if digest not in seen_hypotheses:
                                    seen_hypotheses.add(digest)
                                    hypotheses.append(dict(hypothesis))
                    visit(item_value, normalized)
            elif isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
                for item in value:
                    if key == "evidence_refs" and isinstance(item, str):
                        evidence_refs.append(item)
                    elif key == "artifact_refs" and isinstance(item, str):
                        artifact_refs.append(item)
                    else:
                        visit(item, key)

        for message in self.ledger.messages(run_id):
            if message.source_type != "model_request_projection":
                visit(message.content)
        return {
            "unverified_hypotheses": hypotheses,
            "referenced_evidence": list(dict.fromkeys(evidence_refs)),
            "referenced_artifacts": list(dict.fromkeys(artifact_refs)),
        }
