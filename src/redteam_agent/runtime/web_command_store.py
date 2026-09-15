from __future__ import annotations

import sqlite3
import time
from typing import Any, Mapping

from .model_common import utc_now
from .store_common import ImmutableRecordError, StoreConflictError, _dump, _load


class WebCommandStoreMixin:
    """Durable single-flight receipts for protocol command adapters."""

    @staticmethod
    def _web_command_row(row: sqlite3.Row) -> dict[str, Any]:
        response = _load(row["response_json"], {})
        return {
            "command_id": str(row["command_id"]),
            "request_hash": str(row["request_hash"]),
            "run_id": str(row["run_id"] or ""),
            "owner": str(row["owner"]),
            "status": str(row["status"]),
            "response": dict(response) if isinstance(response, Mapping) else {},
            "lease_expires_at": float(row["lease_expires_at"] or 0),
            "fencing_token": max(1, int(row["fencing_token"] or 1)),
            "created_at": str(row["created_at"]),
            "updated_at": str(row["updated_at"]),
            "reclaimed": False,
        }

    def claim_web_command(
        self,
        command_id: str,
        request_hash: str,
        *,
        owner: str,
        run_id: str = "",
        ttl_seconds: float = 30.0,
    ) -> dict[str, Any]:
        """Claim a command or return its completed/single-flight receipt."""

        command = str(command_id or "").strip()
        digest = str(request_hash or "").strip()
        claimant = str(owner or "").strip()
        if not command or not digest or not claimant:
            raise ValueError("web_command_identity_required")
        now = time.time()
        expires = now + max(1.0, float(ttl_seconds))
        timestamp = utc_now()
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM web_command_receipts WHERE command_id=?",
                (command,),
            ).fetchone()
            if row is None:
                connection.execute(
                    "INSERT INTO web_command_receipts "
                    "(command_id, request_hash, run_id, owner, status, response_json, "
                    "lease_expires_at, fencing_token, created_at, updated_at) "
                    "VALUES(?, ?, ?, ?, 'pending', '{}', ?, 1, ?, ?)",
                    (command, digest, str(run_id or ""), claimant, expires, timestamp, timestamp),
                )
                claimed = True
            else:
                existing = self._web_command_row(row)
                if existing["request_hash"] != digest:
                    raise ImmutableRecordError(f"web_command_hash_conflict:{command}")
                if existing["status"] == "completed":
                    existing["claimed"] = False
                    return existing
                if float(existing["lease_expires_at"]) > now:
                    existing["claimed"] = False
                    return existing
                claimed = True
                connection.execute(
                    "UPDATE web_command_receipts SET owner=?, "
                    "run_id=CASE WHEN ?='' THEN run_id ELSE ? END, lease_expires_at=?, "
                    "fencing_token=?, updated_at=? WHERE command_id=?",
                    (
                        claimant,
                        str(run_id or ""),
                        str(run_id or ""),
                        expires,
                        int(existing["fencing_token"]) + 1,
                        timestamp,
                        command,
                    ),
                )
            saved = connection.execute(
                "SELECT * FROM web_command_receipts WHERE command_id=?",
                (command,),
            ).fetchone()
            if saved is None:
                raise RuntimeError(f"web_command_receipt_missing:{command}")
            result = self._web_command_row(saved)
            result["claimed"] = claimed
            result["reclaimed"] = bool(claimed and result["fencing_token"] > 1)
            return result

    def complete_web_command(
        self,
        command_id: str,
        response: Mapping[str, Any],
        *,
        owner: str,
        fencing_token: int,
        run_id: str = "",
    ) -> dict[str, Any]:
        """Complete a receipt only while its fencing token is authoritative."""

        command = str(command_id or "").strip()
        claimant = str(owner or "").strip()
        if not command or not claimant:
            raise ValueError("web_command_identity_required")
        expected_fencing_token = max(1, int(fencing_token))
        timestamp = utc_now()
        serialized = _dump(dict(response))
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM web_command_receipts WHERE command_id=?",
                (command,),
            ).fetchone()
            if row is None:
                raise KeyError(f"web_command_not_claimed:{command}")
            existing = self._web_command_row(row)
            if existing["status"] == "completed":
                return existing
            if (
                existing["owner"] != claimant
                or int(existing["fencing_token"]) != expected_fencing_token
            ):
                raise StoreConflictError(f"web_command_owner_conflict:{command}")
            cursor = connection.execute(
                "UPDATE web_command_receipts SET status='completed', response_json=?, "
                "run_id=CASE WHEN ?='' THEN run_id ELSE ? END, lease_expires_at=0, updated_at=? "
                "WHERE command_id=? AND owner=? AND fencing_token=? AND status='pending'",
                (
                    serialized,
                    str(run_id or ""),
                    str(run_id or ""),
                    timestamp,
                    command,
                    claimant,
                    expected_fencing_token,
                ),
            )
            if cursor.rowcount != 1:
                raise StoreConflictError(f"web_command_fence_lost:{command}")
            saved = connection.execute(
                "SELECT * FROM web_command_receipts WHERE command_id=?",
                (command,),
            ).fetchone()
            if saved is None:
                raise RuntimeError(f"web_command_receipt_missing:{command}")
            return self._web_command_row(saved)


__all__ = ["WebCommandStoreMixin"]
