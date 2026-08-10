from __future__ import annotations

import json
from pathlib import Path

import pytest

from redteam_agent.core import ContractError, ContractVersionError, canonical_json, contract_hash
from scripts.phase1_snapshot import SNAPSHOT_FILE, generate_document, sample_contracts


REPO_ROOT = Path(__file__).resolve().parents[1]
SNAPSHOT = REPO_ROOT / "tests" / "fixtures" / "phase1" / SNAPSHOT_FILE


def test_all_core_contracts_have_stable_round_trips() -> None:
    for contract in sample_contracts():
        payload = contract.to_dict()
        assert payload["schema_version"] == 1
        assert payload["kind"] == contract.KIND
        assert type(contract).from_dict(payload) == contract
        assert type(contract).from_dict(payload).to_dict() == payload


def test_all_core_contracts_upgrade_unversioned_payloads() -> None:
    for contract in sample_contracts():
        current = contract.to_dict()
        legacy = {key: value for key, value in current.items() if key not in {"schema_version", "kind"}}
        assert type(contract).from_dict(legacy).to_dict() == current


def test_all_core_contracts_reject_future_versions() -> None:
    for contract in sample_contracts():
        future = {**contract.to_dict(), "schema_version": 2}
        with pytest.raises(ContractVersionError, match="schema_version_unsupported"):
            type(contract).from_dict(future)


def test_all_core_contracts_reject_cross_type_payloads() -> None:
    for contract in sample_contracts():
        wrong = {**contract.to_dict(), "kind": "different_contract"}
        with pytest.raises(ContractError, match="contract_kind_mismatch"):
            type(contract).from_dict(wrong)


def test_core_contract_snapshot_matches_authoritative_fixture() -> None:
    assert generate_document() == json.loads(SNAPSHOT.read_text(encoding="utf-8"))


def test_canonical_hash_is_mapping_order_independent() -> None:
    left = {"b": [2, 3], "a": {"y": 2, "x": 1}}
    right = {"a": {"x": 1, "y": 2}, "b": [2, 3]}
    assert canonical_json(left) == canonical_json(right)
    assert contract_hash(left) == contract_hash(right)
