from __future__ import annotations

from dataclasses import replace

from redteam_agent.runtime import EvidenceGate, EvidenceNode, EvidenceProvenance


def _node(
    evidence_id: str,
    *,
    run_id: str = "run-1",
    branch_id: str = "main",
    target: str = "fixture://target",
    parent_ids: tuple[str, ...] = (),
    clause_id: str = "clause-1",
    trusted: bool = True,
) -> EvidenceNode:
    payload = {
        "artifact_type": "surface_map",
        "target": target,
        "results": [evidence_id],
        "clause_support": {clause_id: {"source": evidence_id}},
    }
    return EvidenceNode(
        evidence_id=evidence_id,
        run_id=run_id,
        action_id="surface",
        artifact_type="surface_map",
        target=target,
        tool="fixture:tool" if trusted else "host:fixture",
        payload=payload,
        content_hash=EvidenceGate.content_hash(payload),
        parent_ids=parent_ids,
        verifier="surface_map",
        confidence=0.9,
        verified=trusted,
        trust="runtime_verified" if trusted else "host_asserted",
        provenance=EvidenceProvenance(
            run_id=run_id,
            branch_id=branch_id,
            plan_revision=1,
            action_id="surface",
            attempt_id=f"attempt-{evidence_id}",
            tool="fixture:tool" if trusted else "host:fixture",
            target=target,
            parent_ids=parent_ids,
        ),
    )


def test_hash_and_scope_are_deterministic() -> None:
    payload = {"b": 2, "a": ["x"]}
    assert EvidenceGate.content_hash(payload) == EvidenceGate.content_hash({"a": ["x"], "b": 2})
    node = _node("e-1")
    assert EvidenceGate.same_scope(node, run_id="run-1", branch_id="main", target=node.target)
    assert not EvidenceGate.same_scope(node, run_id="run-2", branch_id="main", target=node.target)
    assert not EvidenceGate.same_scope(node, run_id="run-1", branch_id="other", target=node.target)
    assert not EvidenceGate.same_scope(node, run_id="run-1", branch_id="main", target="fixture://other")


def test_parent_validation_rejects_missing_cross_scope_and_untrusted() -> None:
    parent = _node("parent")
    missing = EvidenceGate.validate_parent_ids(
        ("missing",),
        {},
        run_id="run-1",
        target=parent.target,
    )
    assert missing.reason.startswith("evidence_parent_missing")

    foreign = replace(parent, run_id="run-2", provenance=replace(parent.provenance, run_id="run-2"))
    cross_run = EvidenceGate.validate_parent_ids(
        (foreign.evidence_id,),
        {foreign.evidence_id: foreign},
        run_id="run-1",
        target=parent.target,
    )
    assert cross_run.reason == "evidence_parent_run_mismatch"

    assertion = _node("assertion", trusted=False)
    untrusted = EvidenceGate.validate_parent_ids(
        (assertion.evidence_id,),
        {assertion.evidence_id: assertion},
        run_id="run-1",
        target=assertion.target,
    )
    assert untrusted.reason == "evidence_parent_untrusted"


def test_lineage_and_clause_support_share_the_same_walk() -> None:
    parent = _node("parent")
    child = _node("child", parent_ids=(parent.evidence_id,))
    evidence = {parent.evidence_id: parent, child.evidence_id: child}
    assert EvidenceGate.lineage_error(tuple(evidence.values())) == ""
    assert EvidenceGate.clause_support_types(
        (child.evidence_id,),
        "clause-1",
        evidence,
        run_id="run-1",
        branch_id="main",
        target=child.target,
    ) == {"surface_map"}

    cycle = replace(parent, parent_ids=(child.evidence_id,), provenance=replace(parent.provenance, parent_ids=(child.evidence_id,)))
    assert EvidenceGate.lineage_error((cycle, child)) == "evidence_lineage_cycle"


def test_eligible_evidence_filters_hash_scope_and_trust() -> None:
    trusted = _node("trusted")
    assertion = _node("assertion", trusted=False)
    tampered = replace(trusted, payload={"tampered": True})
    eligible = EvidenceGate.eligible_evidence(
        (trusted, assertion, tampered),
        run_id="run-1",
        branch_id="main",
    )
    assert eligible == (trusted,)
    with_assertions = EvidenceGate.eligible_evidence(
        (trusted, assertion),
        run_id="run-1",
        branch_id="main",
        include_unverified=True,
    )
    assert with_assertions == (trusted, assertion)
