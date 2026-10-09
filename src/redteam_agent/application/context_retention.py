from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from typing import Any

from ..core import contract_hash
from .tool_projection import ToolObservationProjector


MAX_RETAINED_HYPOTHESES = 8
MAX_RETAINED_REFERENCES = 32
MAX_RETAINED_MESSAGES = 256
MAX_RETAINED_HYPOTHESIS_BYTES = 1024


def retained_tactical_state(messages: Sequence[Any]) -> dict[str, Any]:
    """Bound historical hints; original goal and active focus live outside this projection."""
    hypotheses, evidence, artifacts = [], [], []
    seen: set[str] = set()
    truncated = False

    def reference(items: list[str], value: Any) -> None:
        nonlocal truncated
        if not isinstance(value, str) or value in items:
            return
        if len(items) >= MAX_RETAINED_REFERENCES or len(value) > 256:
            truncated = True
            return
        items.append(value)

    def hypothesis(value: Any) -> None:
        nonlocal truncated
        if not isinstance(value, Mapping):
            return
        digest = contract_hash(value)
        if digest in seen:
            return
        if len(hypotheses) >= MAX_RETAINED_HYPOTHESES:
            truncated = True
            return
        seen.add(digest)
        projected = ToolObservationProjector._bounded(value)
        raw = json.dumps(projected, ensure_ascii=False, sort_keys=True).encode("utf-8")
        if len(raw) > MAX_RETAINED_HYPOTHESIS_BYTES:
            projected = {"source_hash": digest,
                         "preview": raw[:MAX_RETAINED_HYPOTHESIS_BYTES].decode("utf-8", errors="replace"),
                         "truncated": True}
            truncated = True
        hypotheses.append(projected)

    def visit(value: Any, key: str = "", depth: int = 0) -> None:
        nonlocal truncated
        if depth > 8:
            truncated = True
            return
        if isinstance(value, Mapping):
            if key == "hypotheses":
                for child in value.values():
                    hypothesis(child)
            for child_key, child in value.items():
                if child_key in {"evidence_ref", "evidence_id"}:
                    reference(evidence, child)
                elif child_key == "artifact_ref":
                    reference(artifacts, child)
                visit(child, str(child_key), depth + 1)
        elif isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
            for child in value:
                if key == "hypotheses":
                    hypothesis(child)
                elif key == "evidence_refs":
                    reference(evidence, child)
                elif key == "artifact_refs":
                    reference(artifacts, child)
                visit(child, "" if key == "hypotheses" else key, depth + 1)

    scanned = 0
    for message in reversed(messages):
        if message.source_type == "model_request_projection":
            continue
        if scanned == MAX_RETAINED_MESSAGES:
            truncated = True
            break
        scanned += 1
        visit(message.content)
    return {"unverified_hypotheses": hypotheses, "referenced_evidence": evidence,
            "referenced_artifacts": artifacts,
            "historical_projection": {"recent_messages_scanned": scanned, "truncated": truncated,
                                      "source": "durable_conversation",
                                      "omitted_detail": "search_execution_steps_or_artifacts"}}
