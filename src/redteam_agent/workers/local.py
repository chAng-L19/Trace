from __future__ import annotations

import os
import hashlib
import signal
import subprocess
import threading
import time
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from ..core import WorkerResult, WorkerTask
from ..runtime.artifact_store import ArtifactStore
from ..runtime.worker_store import WORKER_TERMINAL_STATUSES, WorkerStore
from ..runtime.security import safe_error_text
from .workspace import WorkspaceManager


PREVIEW_EDGE_BYTES = 16 * 1024


def _bounded_file_preview(path: Path, *, edge_bytes: int = PREVIEW_EDGE_BYTES) -> dict[str, Any]:
    byte_count = path.stat().st_size
    line_count = 0
    last_byte = b""
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            line_count += chunk.count(b"\n")
            last_byte = chunk[-1:]
    with path.open("rb") as stream:
        head = stream.read(edge_bytes)
        tail = b""
        if byte_count > edge_bytes:
            stream.seek(max(0, byte_count - edge_bytes))
            tail = stream.read(edge_bytes)
    return {
        "head": head.decode("utf-8", errors="replace"),
        "tail": tail.decode("utf-8", errors="replace"),
        "byte_count": byte_count,
        "line_count": line_count + (1 if byte_count and last_byte != b"\n" else 0),
        "truncated": byte_count > edge_bytes * 2,
    }


class LocalWorker:
    kind = "local"

    def __init__(
        self,
        *,
        workspaces: WorkspaceManager,
        artifacts: ArtifactStore,
        records: WorkerStore,
        owner: str = "local-worker",
    ) -> None:
        self.workspaces = workspaces
        self.artifacts = artifacts
        self.records = records
        self.owner = owner
        self._active: dict[str, subprocess.Popen[bytes]] = {}
        self._cancel_requested: set[str] = set()
        self._cancel_events: dict[str, threading.Event] = {}
        self._lock = threading.RLock()

    def capabilities(self) -> tuple[str, ...]:
        return ("local.command", "local.process")

    def execute(self, task: WorkerTask) -> WorkerResult:
        prepared = self.records.prepare(task, worker_kind=self.kind, owner=self.owner)
        if prepared.result is not None and prepared.status in WORKER_TERMINAL_STATUSES:
            return prepared.result
        if prepared.status == "running":
            with self._lock:
                active = self._active.get(task.task_id)
            if active is not None and active.poll() is None:
                raise RuntimeError(f"worker_task_already_active:{task.task_id}:running")
            prepared = self.records.mark_interrupted_unknown(task.task_id, owner=self.owner)
            return prepared.result  # type: ignore[return-value]
        if prepared.status == "unknown":
            return prepared.result or WorkerResult(
                task_id=task.task_id,
                status="unknown",
                error="worker_interrupted_requires_reconcile",
                retryable=True,
            )
        if prepared.status != "prepared":
            raise RuntimeError(f"worker_task_already_active:{task.task_id}:{prepared.status}")
        self.records.transition(
            task.task_id,
            expected_statuses=(prepared.status,),
            status="running",
            owner=self.owner,
        )
        cancel_event = threading.Event()
        with self._lock:
            self._cancel_events[task.task_id] = cancel_event
        payload = dict(task.payload)
        argv = payload.get("argv")
        if (
            isinstance(argv, (str, bytes))
            or not isinstance(argv, Sequence)
            or not argv
            or any(not isinstance(item, str) or not item or "\x00" in item for item in argv)
        ):
            return self._finish(
                task,
                "failed",
                WorkerResult(task_id=task.task_id, status="failed", error="local_worker_argv_required"),
            )
        try:
            workspace = self.workspaces.ensure(task.run_id)
            cwd = self.workspaces.resolve(workspace, str(payload.get("cwd") or "."))
            cwd.mkdir(parents=True, exist_ok=True)
            if cwd.is_symlink():
                raise ValueError("local_worker_cwd_symlink")
            env_overlay = payload.get("env")
            if env_overlay is not None and not isinstance(env_overlay, Mapping):
                raise ValueError("local_worker_env_invalid")
            environment = self.workspaces.environment(env_overlay or {})
        except (KeyError, OSError, ValueError) as exc:
            return self._finish(
                task,
                "failed",
                WorkerResult(
                    task_id=task.task_id,
                    status="failed",
                    error=f"local_worker_setup_error:{exc}",
                ),
            )
        task_key = hashlib.sha256(task.task_id.encode("utf-8")).hexdigest()
        try:
            stdout_path = self.workspaces.resolve(workspace, f".worker/{task_key}.stdout")
            stderr_path = self.workspaces.resolve(workspace, f".worker/{task_key}.stderr")
            stdout_path.parent.mkdir(parents=True, exist_ok=True)
        except (OSError, ValueError) as exc:
            return self._finish(
                task,
                "failed",
                WorkerResult(task_id=task.task_id, status="failed", error=f"local_worker_path_error:{exc}"),
            )
        timeout = task.timeout_seconds
        process: subprocess.Popen[bytes] | None = None
        timed_out = False
        try:
            with stdout_path.open("wb") as stdout_stream, stderr_path.open("wb") as stderr_stream:
                options: dict[str, Any] = {
                    "cwd": cwd,
                    "env": environment,
                    "stdin": subprocess.DEVNULL,
                    "stdout": stdout_stream,
                    "stderr": stderr_stream,
                    "shell": False,
                }
                if os.name == "nt":
                    options["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
                else:
                    options["start_new_session"] = True
                process = subprocess.Popen(tuple(argv), **options)
                with self._lock:
                    self._active[task.task_id] = process
                    cancel_requested = task.task_id in self._cancel_requested
                if cancel_requested:
                    cancel_event.set()
                    self._terminate_tree(process)
                deadline = time.monotonic() + timeout if timeout is not None else None
                while True:
                    return_code = process.poll()
                    if return_code is not None:
                        break
                    if cancel_event.wait(0.05):
                        self._terminate_tree(process)
                        return_code = process.wait(timeout=5)
                        break
                    if deadline is not None and time.monotonic() >= deadline:
                        timed_out = True
                        self._terminate_tree(process)
                        try:
                            return_code = process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            return_code = -1
                        break
        except BaseException as exc:
            if process is not None and process.poll() is None:
                self._terminate_tree(process)
            result = WorkerResult(
                task_id=task.task_id,
                status="failed",
                error=f"local_worker_error:{type(exc).__name__}:{safe_error_text(exc)}",
                retryable=True,
            )
            return self._finish(task, "failed", result)
        finally:
            with self._lock:
                self._active.pop(task.task_id, None)
                cancelled = task.task_id in self._cancel_requested
                self._cancel_requested.discard(task.task_id)
                self._cancel_events.pop(task.task_id, None)

        try:
            stdout_ref = self.artifacts.put_file(
                stdout_path,
                run_id=task.run_id,
                artifact_type="worker_stdout",
                media_type="text/plain; charset=utf-8",
                preview=_bounded_file_preview(stdout_path),
                metadata={"task_id": task.task_id, "worker_kind": self.kind},
            )
            stderr_ref = self.artifacts.put_file(
                stderr_path,
                run_id=task.run_id,
                artifact_type="worker_stderr",
                media_type="text/plain; charset=utf-8",
                preview=_bounded_file_preview(stderr_path),
                metadata={"task_id": task.task_id, "worker_kind": self.kind},
            )
        except BaseException as exc:
            return self._finish(
                task,
                "failed",
                WorkerResult(
                    task_id=task.task_id,
                    status="failed",
                    error=f"local_worker_artifact_error:{type(exc).__name__}:{safe_error_text(exc)}",
                    retryable=True,
                ),
            )
        status = (
            "timed_out"
            if timed_out
            else ("cancelled" if cancelled else ("completed" if return_code == 0 else "failed"))
        )
        result = WorkerResult(
            task_id=task.task_id,
            status=status,
            output={
                "return_code": return_code,
                "stdout": self.artifacts.project(stdout_ref),
                "stderr": self.artifacts.project(stderr_ref),
            },
            artifact_refs=(stdout_ref.artifact_id, stderr_ref.artifact_id),
            error=(
                "worker_timeout"
                if timed_out
                else ("worker_cancelled" if cancelled else ("" if return_code == 0 else f"process_exit:{return_code}"))
            ),
            retryable=timed_out,
            metadata={"workspace_key": workspace.workspace_key, "worker_kind": self.kind},
        )
        return self._finish(task, status, result)

    def reconcile(self, idempotency_key: str) -> WorkerResult | None:
        matches = []
        with self.records.store.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM worker_tasks WHERE worker_kind=? AND idempotency_key=?",
                (self.kind, idempotency_key),
            ).fetchall()
        for row in rows:
            record = self.records._from_row(row)
            if record.result is not None and record.status in WORKER_TERMINAL_STATUSES:
                matches.append(record.result)
        return matches[0] if len(matches) == 1 else None

    def cancel(self, task_id: str) -> bool:
        with self._lock:
            process = self._active.get(task_id)
            event = self._cancel_events.get(task_id)
            if process is None:
                record = self.records.get(task_id)
                if record is None or record.status not in {"prepared", "running"}:
                    return False
                self._cancel_requested.add(task_id)
                if event is not None:
                    event.set()
                return True
            self._cancel_requested.add(task_id)
            if event is not None:
                event.set()
        if process.poll() is None:
            self._terminate_tree(process)
        return True

    def _finish(self, task: WorkerTask, status: str, result: WorkerResult) -> WorkerResult:
        self.records.transition(
            task.task_id,
            expected_statuses=("running",),
            status=status,
            result=result,
            owner=self.owner,
        )
        return result

    @staticmethod
    def _terminate_tree(process: subprocess.Popen[bytes]) -> None:
        if process.poll() is not None:
            return
        if os.name == "nt":
            try:
                subprocess.run(
                    ("taskkill", "/PID", str(process.pid), "/T", "/F"),
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                    timeout=2,
                )
            except (OSError, subprocess.TimeoutExpired):
                pass
            if process.poll() is None:
                process.kill()
        else:
            try:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=3)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass


__all__ = ["LocalWorker"]
