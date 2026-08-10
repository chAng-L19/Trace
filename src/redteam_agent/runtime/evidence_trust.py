from __future__ import annotations

from typing import Any, Sequence


HOST_ASSERTED = "host_asserted"
RUNTIME_VERIFIED = "runtime_verified"
TOOL_VERIFIED = "tool_verified"

VERIFIED_TRUST_LEVELS = frozenset(
    {RUNTIME_VERIFIED, TOOL_VERIFIED}
)


def direct_trust(source: str) -> str:
    return RUNTIME_VERIFIED if source == "registered-adapter" else TOOL_VERIFIED


def is_trusted_evidence(node: Any) -> bool:
    tool = str(getattr(node, "tool", ""))
    return bool(
        getattr(node, "verified", False)
        and str(getattr(node, "trust", "")) in VERIFIED_TRUST_LEVELS
        and not tool.startswith("host:")
    )


def is_host_assertion(node: Any) -> bool:
    return bool(
        not getattr(node, "verified", True)
        and str(getattr(node, "trust", "")) == HOST_ASSERTED
        and str(getattr(node, "artifact_type", "")) == "host_observation"
        and str(getattr(node, "tool", "")).startswith("host:")
    )


def valid_evidence_trust(node: Any) -> bool:
    return is_trusted_evidence(node) or is_host_assertion(node)


def host_assertion_metadata(nodes: Sequence[Any]) -> list[dict[str, Any]]:
    """Expose assertion identity without leaking self-reported payload text."""

    result: list[dict[str, Any]] = []
    for node in nodes:
        if not is_host_assertion(node):
            continue
        provenance = getattr(node, "provenance", None)
        result.append(
            {
                "evidence_id": str(getattr(node, "evidence_id", "")),
                "run_id": str(getattr(node, "run_id", "")),
                "branch_id": str(getattr(provenance, "branch_id", "")),
                "plan_revision": int(getattr(provenance, "plan_revision", 0) or 0),
                "action_id": str(getattr(node, "action_id", "")),
                "target": str(getattr(node, "target", "")),
                "content_hash": str(getattr(node, "content_hash", "")),
                "trust": HOST_ASSERTED,
                "verified": False,
            }
        )
    return result


__all__ = [
    "HOST_ASSERTED",
    "RUNTIME_VERIFIED",
    "TOOL_VERIFIED",
    "VERIFIED_TRUST_LEVELS",
    "direct_trust",
    "host_assertion_metadata",
    "is_host_assertion",
    "is_trusted_evidence",
    "valid_evidence_trust",
]
