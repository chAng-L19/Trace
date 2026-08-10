from __future__ import annotations

import hashlib
import json
from collections import deque
from typing import Any, Mapping, Sequence

from .models import FactRecord


class FactValidationError(ValueError):
    pass


def _identity(
    *,
    run_id: str,
    branch_id: str,
    key: str,
    version: int,
    value: Any,
    source_evidence_ids: Sequence[str],
    parent_fact_ids: Sequence[str],
    valid: bool,
) -> str:
    payload = json.dumps(
        {
            "run_id": run_id,
            "branch_id": branch_id,
            "key": key,
            "version": version,
            "value": value,
            "source_evidence_ids": list(source_evidence_ids),
            "parent_fact_ids": list(parent_fact_ids),
            "valid": valid,
        },
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        default=str,
    )
    return f"fact-{hashlib.sha256(payload.encode('utf-8')).hexdigest()[:32]}"


class FactLedger:
    @staticmethod
    def latest(records: Sequence[FactRecord], *, run_id: str, branch_id: str, key: str) -> FactRecord | None:
        matches = [
            item
            for item in records
            if item.run_id == run_id and item.branch_id == branch_id and item.key == key
        ]
        return max(matches, key=lambda item: (item.version, item.created_at, item.fact_id), default=None)

    @classmethod
    def create(
        cls,
        records: Sequence[FactRecord],
        *,
        run_id: str,
        branch_id: str,
        plan_revision: int,
        key: str,
        value: Any,
        source_evidence_ids: Sequence[str] = (),
        parent_fact_ids: Sequence[str] = (),
    ) -> FactRecord:
        if not run_id or not branch_id or not key:
            raise FactValidationError("fact_identity_required")
        by_id = {item.fact_id: item for item in records}
        missing = set(parent_fact_ids) - set(by_id)
        if missing:
            raise FactValidationError(f"fact_parent_missing:{','.join(sorted(missing))}")
        if any(
            (by_id[parent_id].run_id, by_id[parent_id].branch_id) != (run_id, branch_id)
            for parent_id in parent_fact_ids
        ):
            raise FactValidationError("fact_parent_scope_mismatch")
        if any(not by_id[parent_id].valid for parent_id in parent_fact_ids):
            raise FactValidationError("fact_parent_invalid")
        previous = cls.latest(records, run_id=run_id, branch_id=branch_id, key=key)
        version = previous.version + 1 if previous else 1
        sources = tuple(dict.fromkeys(str(item) for item in source_evidence_ids if str(item)))
        parents = tuple(dict.fromkeys(str(item) for item in parent_fact_ids if str(item)))
        return FactRecord(
            fact_id=_identity(
                run_id=run_id,
                branch_id=branch_id,
                key=key,
                version=version,
                value=value,
                source_evidence_ids=sources,
                parent_fact_ids=parents,
                valid=True,
            ),
            run_id=run_id,
            branch_id=branch_id,
            plan_revision=max(1, int(plan_revision)),
            key=key,
            value=value,
            version=version,
            source_evidence_ids=sources,
            parent_fact_ids=parents,
        )

    @classmethod
    def invalidate(
        cls,
        records: Sequence[FactRecord],
        *,
        fact_id: str,
        invalidated_by: Sequence[str],
        reason: str,
        plan_revision: int,
        propagate: bool = True,
    ) -> tuple[FactRecord, ...]:
        by_id = {item.fact_id: item for item in records}
        root = by_id.get(fact_id)
        if root is None:
            raise FactValidationError(f"fact_not_found:{fact_id}")
        affected_ids = {fact_id}
        if propagate:
            children: dict[str, set[str]] = {}
            for record in records:
                for parent_id in record.parent_fact_ids:
                    children.setdefault(parent_id, set()).add(record.fact_id)
            queue = deque([fact_id])
            while queue:
                parent_id = queue.popleft()
                for child_id in children.get(parent_id, ()):
                    if child_id not in affected_ids:
                        affected_ids.add(child_id)
                        queue.append(child_id)

        latest_by_key: dict[tuple[str, str, str], FactRecord] = {}
        for record in records:
            identity = (record.run_id, record.branch_id, record.key)
            current = latest_by_key.get(identity)
            if current is None or record.version > current.version:
                latest_by_key[identity] = record

        invalidators = tuple(dict.fromkeys(str(item) for item in invalidated_by if str(item)))
        tombstones: list[FactRecord] = []
        affected_keys = {
            (by_id[item].run_id, by_id[item].branch_id, by_id[item].key)
            for item in affected_ids
            if item in by_id
        }
        for identity in sorted(affected_keys):
            current = latest_by_key[identity]
            if not current.valid:
                continue
            version = current.version + 1
            tombstones.append(
                FactRecord(
                    fact_id=_identity(
                        run_id=current.run_id,
                        branch_id=current.branch_id,
                        key=current.key,
                        version=version,
                        value=current.value,
                        source_evidence_ids=current.source_evidence_ids,
                        parent_fact_ids=current.parent_fact_ids,
                        valid=False,
                    ),
                    run_id=current.run_id,
                    branch_id=current.branch_id,
                    plan_revision=max(1, int(plan_revision)),
                    key=current.key,
                    value=current.value,
                    version=version,
                    source_evidence_ids=current.source_evidence_ids,
                    parent_fact_ids=current.parent_fact_ids,
                    valid=False,
                    invalidated_by=invalidators,
                    invalidation_reason=reason or "invalidated",
                )
            )
        return tuple(tombstones)

    @classmethod
    def effective(
        cls,
        records: Sequence[FactRecord],
        *,
        run_id: str,
        branch_id: str,
    ) -> dict[str, FactRecord]:
        latest: dict[str, FactRecord] = {}
        for record in records:
            if record.run_id != run_id or record.branch_id != branch_id:
                continue
            current = latest.get(record.key)
            if current is None or record.version > current.version:
                latest[record.key] = record
        return {key: value for key, value in latest.items() if value.valid}

    @classmethod
    def versions(
        cls,
        records: Sequence[FactRecord],
        *,
        run_id: str,
        branch_id: str,
    ) -> dict[str, int]:
        return {
            key: record.version
            for key, record in cls.effective(records, run_id=run_id, branch_id=branch_id).items()
        }

    @staticmethod
    def validate_overlay(
        records: Sequence[FactRecord],
        overlay: Mapping[str, int],
        *,
        run_id: str = "",
        branch_id: str = "",
    ) -> None:
        scoped = tuple(
            item
            for item in records
            if (not run_id or item.run_id == run_id) and (not branch_id or item.branch_id == branch_id)
        )
        indexed = {(item.key, item.version): item for item in scoped}
        latest: dict[str, FactRecord] = {}
        for item in scoped:
            current = latest.get(item.key)
            if current is None or item.version > current.version:
                latest[item.key] = item
        for key, version in overlay.items():
            record = indexed.get((str(key), int(version)))
            if record is None:
                raise FactValidationError(f"fact_overlay_missing:{key}@{version}")
            current = latest.get(str(key))
            if not record.valid or current is None or not current.valid:
                raise FactValidationError(f"fact_overlay_invalid:{key}@{version}")
            if current.version != int(version):
                raise FactValidationError(f"fact_overlay_stale:{key}@{version}:{current.version}")


__all__ = ["FactLedger", "FactValidationError"]
