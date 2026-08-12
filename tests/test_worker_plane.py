from __future__ import annotations

import os
import sys
import threading
import time
from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import WorkerTask
from redteam_agent.runtime.store_common import ImmutableRecordError


def _service_and_run(tmp_path: Path, name: str = "worker") -> tuple[AgentService, str]:
    service = AgentService(root=tmp_path / "runtime")
    run_id = service.start(
        StartRequest(session_id=name, objective="Execute an isolated worker and preserve artifacts")
    ).single.run.run_id
    return service, run_id


def _task(run_id: str, task_id: str, script: str, **kwargs) -> WorkerTask:
    return WorkerTask(
        task_id=task_id,
        run_id=run_id,
        capability="local.command",
        payload={"argv": [sys.executable, "-c", script], **kwargs.pop("payload", {})},
        idempotency_key=kwargs.pop("idempotency_key", task_id),
        timeout_seconds=kwargs.pop("timeout_seconds", None),
        metadata={"worker_kind": "local"},
        **kwargs,
    )


def test_local_worker_persists_bounded_projection_and_full_artifacts(tmp_path: Path) -> None:
    service, run_id = _service_and_run(tmp_path)
    task = _task(run_id, "large-output", "import sys; sys.stdout.write('x' * 2000000)")

    before = service.runtime.store.path.stat().st_size
    result = service.execute_worker(task)
    after = service.runtime.store.path.stat().st_size

    assert result.status == "completed"
    assert len(str(result.output)) < 80_000
    assert after - before < 500_000
    stdout = service.read_artifact(run_id, result.artifact_refs[0])
    assert len(stdout) == 2_000_000
    assert stdout.startswith(b"x" * 100)
    assert result.output["stdout"]["preview"]["truncated"] is True


def test_local_worker_restart_reconciles_without_duplicate_execution(tmp_path: Path) -> None:
    service, run_id = _service_and_run(tmp_path)
    task = _task(run_id, "restart", "from pathlib import Path; p=Path('count'); p.write_text((p.read_text() if p.exists() else '')+'x')")

    first = service.execute_worker(task)
    restarted = AgentService(root=service.runtime.root)
    second = restarted.execute_worker(task)
    workspace = restarted.workspaces.ensure(run_id)

    assert first == second
    assert (workspace.path / "count").read_text(encoding="utf-8") == "x"
    assert len(restarted.worker_results(run_id)) == 1


def test_worker_idempotency_key_rejects_changed_payload(tmp_path: Path) -> None:
    service, run_id = _service_and_run(tmp_path)
    service.execute_worker(_task(run_id, "same-task", "print('first')", idempotency_key="same"))

    with pytest.raises(ImmutableRecordError, match="worker_idempotency_conflict"):
        service.execute_worker(
            _task(run_id, "same-task", "print('changed')", idempotency_key="same")
        )


def test_worker_timeout_terminates_process_and_records_terminal_result(tmp_path: Path) -> None:
    service, run_id = _service_and_run(tmp_path)
    task = _task(run_id, "timeout", "import time; print('started', flush=True); time.sleep(30)", timeout_seconds=0.2)

    started = time.monotonic()
    result = service.execute_worker(task)

    assert result.status == "timed_out"
    assert time.monotonic() - started < 15
    assert service.worker_status(run_id, task.task_id).status == "timed_out"
    assert b"started" in service.read_artifact(run_id, result.artifact_refs[0])


def test_worker_cancel_propagates_to_active_process(tmp_path: Path) -> None:
    service, run_id = _service_and_run(tmp_path)
    task = _task(run_id, "cancel", "import time; time.sleep(30)", timeout_seconds=60)
    result_holder = []
    thread = threading.Thread(target=lambda: result_holder.append(service.execute_worker(task)))
    thread.start()
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            record = service.worker_status(run_id, task.task_id)
        except KeyError:
            record = None
        if record is not None and record.status == "running":
            break
        time.sleep(0.02)
    assert service.cancel_worker(run_id, task.task_id) is True
    thread.join(timeout=15)

    assert thread.is_alive() is False
    assert result_holder
    assert result_holder[0].status == "cancelled"


def test_workspace_environment_isolated_between_runs(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("UNRELATED_PARENT_SECRET", "must-not-leak")
    service, first_run = _service_and_run(tmp_path, "env-first")
    second_run = service.start(
        StartRequest(session_id="env-second", objective="Execute an isolated worker")
    ).single.run.run_id
    script = "import os; print(os.getenv('BOUND_VALUE','')); print(os.getenv('UNRELATED_PARENT_SECRET','absent'))"

    first = service.execute_worker(
        _task(first_run, "env-first-task", script, payload={"env": {"BOUND_VALUE": "one"}})
    )
    second = service.execute_worker(_task(second_run, "env-second-task", script))

    assert b"one" in service.read_artifact(first_run, first.artifact_refs[0])
    assert b"must-not-leak" not in service.read_artifact(first_run, first.artifact_refs[0])
    assert b"one" not in service.read_artifact(second_run, second.artifact_refs[0])
    assert service.workspaces.ensure(first_run).path != service.workspaces.ensure(second_run).path


def test_workspace_rejects_relative_escape_and_cross_run_required_artifact(tmp_path: Path) -> None:
    service, first_run = _service_and_run(tmp_path, "scope-first")
    second_run = service.start(
        StartRequest(session_id="scope-second", objective="Execute an isolated worker")
    ).single.run.run_id
    artifact = service.runtime.artifacts.put_bytes(b"first", run_id=first_run)

    escaped = service.execute_worker(
        _task(first_run, "escape", "print('x')", payload={"cwd": "../../outside"})
    )
    assert escaped.status == "failed"
    assert "workspace_relative_path_escape" in escaped.error
    with pytest.raises(ValueError, match="worker_required_artifact_missing"):
        service.execute_worker(
            _task(
                second_run,
                "cross-run",
                "print('x')",
                required_artifacts=(artifact.artifact_id,),
            )
        )
