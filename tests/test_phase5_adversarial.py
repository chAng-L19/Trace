from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import ModelIntegrityError, ModelLoopError
from redteam_agent.core import ModelResponse, ToolCall, ToolDefinition, ToolResult, WorkerTask
from redteam_agent.providers import FakeModelProvider, ScriptedStream
from redteam_agent.runtime.artifact_store import ArtifactIntegrityError
from redteam_agent.runtime.store_common import ImmutableRecordError


def _run(service: AgentService, name: str) -> str:
    return service.start(
        StartRequest(session_id=name, objective="Adversarially validate Phase 5 runtime boundaries")
    ).single.run.run_id


def _local_task(run_id: str, task_id: str, script: str) -> WorkerTask:
    return WorkerTask(
        task_id=task_id,
        run_id=run_id,
        capability="local.command",
        payload={"argv": [sys.executable, "-c", script]},
        idempotency_key=f"idempotency:{task_id}",
        metadata={"worker_kind": "local"},
    )


@pytest.mark.parametrize(
    ("column", "value", "error"),
    [
        ("byte_count", 1, "artifact_reference_integrity"),
        ("content_hash", "other-valid-blob", "artifact_reference_integrity"),
        ("artifact_type", "forged", "artifact_reference_integrity"),
        ("metadata_json", '{"forged":true}', "artifact_reference_integrity"),
    ],
)
def test_artifact_sqlite_column_tampering_is_detected(
    tmp_path: Path, column: str, value, error: str
) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, f"artifact-column-{column}")
    ref = service.runtime.artifacts.put_bytes(b"authoritative", run_id=run_id)
    if value == "other-valid-blob":
        value = service.runtime.artifacts.put_bytes(b"other", run_id=run_id).content_hash
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            f"UPDATE artifact_refs SET {column}=? WHERE artifact_id=?", (value, ref.artifact_id)
        )

    with pytest.raises(ArtifactIntegrityError, match=error):
        service.read_artifact(run_id, ref.artifact_id)


def test_artifact_blob_lineage_and_fts_tampering_are_detected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "artifact-derived-integrity")
    parent = service.runtime.artifacts.put_bytes(b"parent", run_id=run_id, metadata={"route": "safe"})
    child = service.runtime.artifacts.put_bytes(
        b"child", run_id=run_id, parents=(parent.artifact_id,), metadata={"route": "safe"}
    )

    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute("DELETE FROM artifact_links WHERE artifact_id=?", (child.artifact_id,))
    with pytest.raises(ArtifactIntegrityError, match="artifact_lineage_integrity"):
        service.read_artifact(run_id, child.artifact_id)

    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE artifact_fts SET metadata=? WHERE artifact_id=?",
            ('{"route":"evilneedle"}', parent.artifact_id),
        )
    with pytest.raises(ArtifactIntegrityError, match="artifact_fts_integrity"):
        service.search_artifacts(run_id, "evilneedle")


def test_worker_record_tampering_is_detected_before_replay(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "worker-record-tamper")
    task = _local_task(run_id, "record-task", "print('ok')")
    service.execute_worker(task)
    with service.runtime.store.transaction(immediate=True) as connection:
        connection.execute(
            "UPDATE worker_tasks SET input_hash=? WHERE task_id=?", ("0" * 64, task.task_id)
        )

    with pytest.raises(ImmutableRecordError, match="worker_record_integrity"):
        service.execute_worker(task)


def test_crashed_running_local_worker_becomes_unknown_without_reexecution(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    first = AgentService(root=root)
    run_id = _run(first, "worker-crash")
    task = _local_task(
        run_id,
        "crashed-task",
        "from pathlib import Path; Path('side-effect').write_text('executed')",
    )
    first.worker_records.prepare(task, worker_kind="local", owner="dead-worker")
    first.worker_records.transition(
        task.task_id, expected_statuses=("prepared",), status="running", owner="dead-worker"
    )

    restarted = AgentService(root=root)
    result = restarted.execute_worker(task)

    assert result.status == "unknown"
    assert result.retryable is True
    assert not (restarted.workspaces.ensure(run_id).path / "side-effect").exists()
    assert restarted.worker_status(run_id, task.task_id).status == "unknown"


def test_local_worker_artifact_failure_is_durable_and_secret_safe(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "worker-artifact-failure")
    task = _local_task(run_id, "artifact-failure", "print('done')")

    def fail(*_args, **_kwargs):
        raise OSError("api_key=super-secret-value")

    monkeypatch.setattr(service.runtime.artifacts, "put_file", fail)
    result = service.execute_worker(task)

    assert result.status == "failed"
    assert "super-secret-value" not in result.error
    assert service.worker_status(run_id, task.task_id).status == "failed"


def test_malicious_task_id_cannot_control_workspace_paths(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "malicious-task-id")
    outside = tmp_path / "outside.stdout"
    task = _local_task(run_id, "../../outside.stdout", "print('contained')")

    result = service.execute_worker(task)

    assert result.status == "completed"
    assert not outside.exists()
    assert b"contained" in service.read_artifact(run_id, result.artifact_refs[0])


def test_worker_status_and_cancel_reject_cross_run_task_access(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    first_run = _run(service, "worker-scope-a")
    second_run = _run(service, "worker-scope-b")
    task = _local_task(first_run, "scope-task", "print('scope')")
    service.execute_worker(task)

    with pytest.raises(KeyError, match="worker_task_not_found"):
        service.worker_status(second_run, task.task_id)
    with pytest.raises(KeyError, match="worker_task_not_found"):
        service.cancel_worker(second_run, task.task_id)


class CancelTrackingTools:
    def __init__(self) -> None:
        self.cancelled: list[str] = []

    def discover(self):
        return (
            ToolDefinition(
                qualified_name="fixture:cancel",
                name="cancel",
                server="fixture",
                input_schema={"type": "object"},
            ),
        )

    def invoke(self, call: ToolCall):
        raise AssertionError("invoke_not_expected")

    def reconcile(self, call: ToolCall):
        return None

    def cancel(self, call_id: str):
        self.cancelled.append(call_id)
        return True


def test_worker_cancel_routes_only_to_the_owning_adapter(tmp_path: Path) -> None:
    tools = CancelTrackingTools()
    service = AgentService(root=tmp_path / "runtime", tool_port=tools)
    run_id = _run(service, "worker-cancel-routing")
    task = WorkerTask(
        task_id="mcp-running-task",
        run_id=run_id,
        capability="mcp.cancel",
        payload={"tool_name": "fixture:cancel", "arguments": {}},
        idempotency_key="mcp-cancel-routing",
        metadata={"worker_kind": "mcp"},
    )
    service.worker_records.prepare(task, worker_kind="mcp", owner="fixture")
    service.worker_records.transition(
        task.task_id, expected_statuses=("prepared",), status="running", owner="fixture"
    )

    assert service.cancel_worker(run_id, task.task_id) is True
    assert tools.cancelled == [task.task_id]


def test_worker_required_and_replayed_artifacts_require_valid_cas_bytes(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "worker-artifact-verification")
    required = service.runtime.artifacts.put_bytes(b"required", run_id=run_id)
    base_task = _local_task(run_id, "required-artifact-task", "print('must-not-run')")
    required_task = WorkerTask(
        task_id=base_task.task_id,
        run_id=base_task.run_id,
        capability=base_task.capability,
        payload=base_task.payload,
        idempotency_key=base_task.idempotency_key,
        timeout_seconds=base_task.timeout_seconds,
        required_artifacts=(required.artifact_id,),
        metadata=base_task.metadata,
    )
    service.runtime.artifacts._path(required.content_hash).write_bytes(b"tampered")

    with pytest.raises(ArtifactIntegrityError, match="artifact_integrity_mismatch"):
        service.execute_worker(required_task)
    with pytest.raises(KeyError, match="worker_task_not_found"):
        service.worker_status(run_id, required_task.task_id)

    replay_task = _local_task(run_id, "replayed-artifact-task", "print('once')")
    completed = service.execute_worker(replay_task)
    replay_ref = service.runtime.artifacts.get_ref(completed.artifact_refs[0], run_id=run_id)
    assert replay_ref is not None
    service.runtime.artifacts._path(replay_ref.content_hash).write_bytes(b"tampered")

    with pytest.raises(ArtifactIntegrityError, match="artifact_integrity_mismatch"):
        service.execute_worker(replay_task)


def test_explicit_message_limit_never_splits_tool_turn(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "explicit-message-limit")
    request_id = "atomic-request"
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content={"tool_calls": [{"id": "atomic-call"}]},
        protected=False,
        source_type="model_response",
        source_id=request_id,
        metadata={"request_id": request_id},
    )
    service.conversation.append(
        run_id=run_id,
        role="tool",
        content={"call_id": "atomic-call", "output": "result"},
        protected=False,
        source_type="tool_result",
        source_id=f"{request_id}:atomic-call",
        metadata={"request_id": request_id, "call_id": "atomic-call"},
    )

    selection = service.select_context(run_id, max_messages=1)
    selected = set(selection.source_message_ids)
    atomic_ids = {
        item.message_id
        for item in service.transcript(run_id)
        if item.metadata.get("request_id") == request_id
    }

    assert atomic_ids <= selected


def test_old_unverified_hypothesis_survives_extreme_context_pressure(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "hypothesis-retention")
    hypothesis = {
        "id": "hypothesis-critical",
        "statement": "UNIQUE_CRITICAL_HYPOTHESIS must be validated",
        "status": "unvalidated",
        "evidence_refs": ["evidence-pending"],
    }
    service.conversation.append(
        run_id=run_id,
        role="assistant",
        content={"hypotheses": [hypothesis], "padding": "x" * 200_000},
        protected=False,
        source_type="model_response",
        source_id="old-hypothesis",
        metadata={"request_id": "old-hypothesis"},
    )
    for index in range(12):
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content={"noise": str(index) * 10_000},
            protected=False,
            source_type="fixture-noise",
            source_id=str(index),
        )

    selection = service.context_selector.prepare_model_context(
        service.status(run_id), max_context_tokens=1024
    )
    retained = selection.protected_context["active_plan"]["retained_tactical_state"]

    assert hypothesis in retained["unverified_hypotheses"]
    assert "evidence-pending" in retained["referenced_evidence"]
    assert selection.context_overflow_tokens > 0


def test_stream_flood_is_content_addressed_and_sqlite_events_are_bounded(tmp_path: Path) -> None:
    chunks = tuple(
        {"sequence": index, "event_type": "text_delta", "payload": {"delta": "多" * 10_000}}
        for index in range(100)
    )
    provider = FakeModelProvider(
        streams=[
            ScriptedStream(
                events=(*chunks, {"sequence": 100, "event_type": "completed", "payload": {}})
            )
        ]
    )
    service = AgentService(
        root=tmp_path / "runtime",
        model_port=provider,
        model_streaming=True,
        model_max_turns=1,
    )
    run_id = _run(service, "stream-flood")
    before = service.runtime.store.path.stat().st_size
    service.run(run_id)
    after = service.runtime.store.path.stat().st_size
    response = service.runtime.store.model_responses(run_id)[0].response
    artifact_id = response["metadata"]["complete_text_artifact"]

    assert len(service.read_artifact(run_id, artifact_id)) == 3_000_000
    assert after - before < 1_000_000
    events = service.runtime.store.model_stream_events(
        service.runtime.store.model_requests(run_id)[0].request_id
    )
    assert all(len(json.dumps(item.payload, ensure_ascii=False)) < 3000 for item in events)


def test_interrupted_stream_flood_is_diagnostic_only_and_content_addressed(
    tmp_path: Path,
) -> None:
    chunks = tuple(
        {"sequence": index, "event_type": "text_delta", "payload": {"delta": "x" * 100_000}}
        for index in range(12)
    )
    provider = FakeModelProvider(
        streams=[ScriptedStream(events=chunks, error=ConnectionError("api_key=stream-secret"))]
    )
    service = AgentService(
        root=tmp_path / "runtime",
        model_port=provider,
        model_streaming=True,
        model_max_retries=0,
    )
    run_id = _run(service, "interrupted-stream-flood")

    with pytest.raises(ModelLoopError, match="retries_exhausted"):
        service.run(run_id)

    response = service.runtime.store.model_responses(run_id)[0]
    diagnostic = service.runtime.store.diagnostic_artifacts(run_id)[0]
    projection = diagnostic.payload["partial_artifact"]

    assert response.status == "failed"
    assert "stream-secret" not in response.response["error"]
    assert diagnostic.payload["promoted"] is False
    assert diagnostic.payload["partial_text"] == ""
    assert len(service.read_artifact(run_id, projection["artifact_ref"])) == 1_200_000


class LargeTargetTool:
    def __init__(self, target: str) -> None:
        self.target = target
        self.calls = 0

    def discover(self):
        return (
            ToolDefinition(
                qualified_name="fixture:large-target",
                name="large-target",
                server="fixture",
                input_schema={"type": "object"},
            ),
        )

    def invoke(self, call: ToolCall):
        self.calls += 1
        return ToolResult(
            call_id=call.call_id,
            status="success",
            tool_name=call.tool_name,
            output={"target": self.target, "padding": "z" * 200_000},
        )

    def reconcile(self, call: ToolCall):
        return None

    def cancel(self, call_id: str):
        return True


def test_tampered_cas_model_observation_blocks_recovery(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("target", encoding="utf-8")
    root = tmp_path / "runtime"
    provider = FakeModelProvider(
        [
            ModelResponse(
                request_id="placeholder",
                status="completed",
                tool_calls=(
                    {"id": "large-call", "name": "fixture:large-target", "arguments": {}},
                ),
                usage={"input_tokens": 1, "output_tokens": 1},
            )
        ]
    )
    tools = LargeTargetTool(str(target))
    first = AgentService(root=root, model_port=provider, tool_port=tools)
    run_id = _run(first, "observation-cas-tamper")
    assert first.model_loop is not None
    first.model_loop._target_from_results = lambda _items: (_ for _ in ()).throw(  # type: ignore[method-assign]
        RuntimeError("crash-after-large-observation")
    )
    with pytest.raises(RuntimeError, match="crash-after-large-observation"):
        first.run(run_id)
    observation = first.runtime.store.model_observations(run_id)[0]
    artifact_id = observation.observation["tool_result_artifact"]["artifact_ref"]
    ref = first.runtime.artifacts.get_ref(artifact_id, run_id=run_id)
    assert ref is not None
    first.runtime.artifacts._path(ref.content_hash).write_bytes(b"tampered")

    restarted = AgentService(
        root=root,
        model_port=FakeModelProvider([]),
        tool_port=tools,
    )
    with pytest.raises(ModelIntegrityError, match="model_observation_artifact_invalid"):
        restarted.run(run_id)
    assert tools.calls == 1
