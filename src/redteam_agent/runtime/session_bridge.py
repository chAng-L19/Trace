from __future__ import annotations

import json
import hashlib
import os
import re
import time
from contextlib import contextmanager
from pathlib import Path
from typing import TYPE_CHECKING, Any, Iterator, Mapping
from uuid import uuid4

from .durable_store import StateVersionConflict
from .security import secure_directory, secure_file

if TYPE_CHECKING:
    from .durable_store import DurableStore


def _safe_session_key(session_id: str) -> str:
    raw = session_id.strip()
    if not raw:
        raise ValueError("session_id is required for persistent red-team state")
    readable = re.sub(r"[^a-zA-Z0-9_.-]+", "_", raw).strip("._-") or "session"
    digest = hashlib.sha256(raw.encode("utf-8")).hexdigest()[:20]
    return f"{readable[:96]}.{digest}"


def _legacy_session_key(session_id: str) -> str:
    raw = session_id.strip()
    return re.sub(r"[^a-zA-Z0-9_.-]+", "_", raw)[:120]


def _state_path(session_id: str) -> Path:
    configured = os.environ.get("CODEX_HOME", "").strip()
    if not configured:
        raise RuntimeError("codex_session_bridge_disabled")
    codex_home = Path(configured).expanduser().resolve(strict=False)
    return codex_home / "redteam-mode" / "state" / "sessions" / f"{_safe_session_key(session_id)}.json"


def _legacy_state_path(session_id: str) -> Path:
    configured = os.environ.get("CODEX_HOME", "").strip()
    if not configured:
        raise RuntimeError("codex_session_bridge_disabled")
    codex_home = Path(configured).expanduser().resolve(strict=False)
    return codex_home / "redteam-mode" / "state" / "sessions" / f"{_legacy_session_key(session_id)}.json"


def _lock_owner_alive(lock_path: Path) -> bool | None:
    try:
        pid = int(lock_path.read_text(encoding="ascii").strip())
    except (OSError, ValueError):
        return None
    if pid <= 0:
        return None
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except (PermissionError, OSError):
        return True
    return True


def _stale_lock_reclaimable(lock_path: Path) -> bool:
    try:
        age = time.time() - lock_path.stat().st_mtime
    except FileNotFoundError:
        return False
    if age <= 30.0:
        return False
    owner_alive = _lock_owner_alive(lock_path)
    return owner_alive is False or (owner_alive is None and age > 300.0)


@contextmanager
def _state_lock(path: Path, *, timeout_seconds: float = 10.0) -> Iterator[None]:
    secure_directory(path.parent)
    lock_path = path.with_name(f"{path.name}.lock")
    deadline = time.monotonic() + max(0.1, timeout_seconds)
    while True:
        try:
            descriptor = os.open(lock_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            if _stale_lock_reclaimable(lock_path):
                try:
                    lock_path.unlink()
                except FileNotFoundError:
                    pass
                continue
            if time.monotonic() >= deadline:
                raise TimeoutError(f"session_state_lock_timeout:{path.stem}")
            time.sleep(0.01)
            continue
        try:
            os.write(descriptor, f"{os.getpid()}\n".encode("ascii"))
        finally:
            os.close(descriptor)
        secure_file(lock_path)
        break
    try:
        yield
    finally:
        try:
            lock_path.unlink()
        except FileNotFoundError:
            pass


def _binding_pointer(
    session_id: str,
    summary: Mapping[str, Any],
    current: Mapping[str, Any] | None,
) -> dict[str, str]:
    batch_session_id = str(summary.get("batch_session_id") or "").strip()
    run_id = "" if batch_session_id else str(summary.get("run_id") or "").strip()
    old_run_id = str((current or {}).get("run_id") or "")
    old_batch_session_id = str((current or {}).get("batch_session_id") or "")
    next_run_id = run_id or old_run_id
    next_batch_session_id = batch_session_id or old_batch_session_id
    if batch_session_id:
        next_run_id = ""
    elif run_id:
        next_batch_session_id = ""
    return {
        "session_id": session_id,
        "run_id": next_run_id,
        "batch_session_id": next_batch_session_id,
    }


def _sync_runtime_binding(
    store: "DurableStore",
    session_id: str,
    summary: Mapping[str, Any],
    *,
    max_retries: int = 8,
) -> Mapping[str, Any]:
    for _ in range(max(1, max_retries)):
        current = store.session_binding(session_id)
        desired = _binding_pointer(session_id, summary, current)
        if current is not None and all(str(current.get(key) or "") == value for key, value in desired.items()):
            return current
        expected_version = int(current.get("_version") or 0) if current is not None else 0
        try:
            version = store.save_session_binding(
                session_id,
                desired,
                expected_version=expected_version,
            )
        except StateVersionConflict:
            continue
        return {**desired, "_version": version}
    raise StateVersionConflict(f"session_binding_retry_exhausted:{session_id}")


def _sync_session_summary(
    session_id: str,
    summary: Mapping[str, Any],
    *,
    binding: Mapping[str, Any] | None = None,
) -> None:
    if not session_id.strip():
        return
    path = _state_path(session_id)

    with _state_lock(path):
        source = path
        if not source.is_file():
            legacy = _legacy_state_path(session_id)
            source = legacy if legacy.is_file() else path
        try:
            current = json.loads(source.read_text(encoding="utf-8")) if source.is_file() else {}
        except (OSError, json.JSONDecodeError):
            current = {}
        if not isinstance(current, dict):
            current = {}
        if source != path and str(current.get("session_id") or "").strip() != session_id.strip():
            current = {}
        mode = str(current.get("mode") or "normal")
        opsec_level = str(current.get("opsec_level") or "balanced")
        lightweight = {
            "mode": mode if mode in {"normal", "redteam-light", "redteam-full"} else "normal",
            "opsec_level": opsec_level if opsec_level in {"strict", "balanced"} else "balanced",
            "session_id": session_id,
            "active_model": str(current.get("active_model") or ""),
            "active_prompt_profile": str(current.get("active_prompt_profile") or ""),
            "history_seed_version": str(current.get("history_seed_version") or ""),
            "history_seed_status": str(current.get("history_seed_status") or ""),
        }
        old_run_id = str(current.get("current_run_id") or current.get("operation_run_id") or "")
        old_batch_session_id = str(current.get("current_batch_session_id") or current.get("batch_session_id") or "")
        if binding is None:
            pointer = _binding_pointer(
                session_id,
                summary,
                {"run_id": old_run_id, "batch_session_id": old_batch_session_id},
            )
            next_run_id = pointer["run_id"]
            next_batch_session_id = pointer["batch_session_id"]
        else:
            next_run_id = str(binding.get("run_id") or "")
            next_batch_session_id = str(binding.get("batch_session_id") or "")
        changed = (next_run_id, next_batch_session_id) != (old_run_id, old_batch_session_id)
        try:
            revision = max(0, int(current.get("binding_revision") or 0))
        except (TypeError, ValueError, OverflowError):
            revision = 0
        lightweight["current_run_id"] = next_run_id
        lightweight["current_batch_session_id"] = next_batch_session_id
        if binding is not None:
            try:
                lightweight["binding_revision"] = max(0, int(binding.get("_version") or revision))
            except (TypeError, ValueError, OverflowError):
                lightweight["binding_revision"] = revision + (1 if changed else 0)
        else:
            lightweight["binding_revision"] = revision + (1 if changed else 0)
        temporary = path.with_name(f"{path.name}.{uuid4().hex}.tmp")
        temporary.write_text(json.dumps(lightweight, ensure_ascii=False, indent=2), encoding="utf-8")
        secure_file(temporary)
        temporary.replace(path)
        secure_file(path)


def sync_session_summary(
    session_id: str,
    summary: Mapping[str, Any],
    *,
    store: "DurableStore | None" = None,
) -> bool:
    if not session_id.strip():
        return False
    if not os.environ.get("CODEX_HOME", "").strip():
        return False
    try:
        binding = _sync_runtime_binding(store, session_id, summary) if store is not None else None
        _sync_session_summary(session_id, summary, binding=binding)
    except (OSError, StateVersionConflict):
        return False
    return True


__all__ = ["sync_session_summary"]
