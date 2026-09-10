from __future__ import annotations

import json
import sys
from pathlib import Path

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import BoundedOutput
from redteam_agent.core import ToolResult, WorkerTask


def _run(service: AgentService, session_id: str) -> str:
    return service.start(
        StartRequest(session_id=session_id, objective="Validate bounded output persistence")
    ).single.run.run_id


def test_bounded_output_preserves_raw_bytes_across_utf8_chunk_boundaries() -> None:
    raw = ("头部-" + "安全研究\n" * 40 + "尾部-" + "\udcff").encode(
        "utf-8", errors="replace"
    )
    output = BoundedOutput(max_bytes=64, max_lines=6)
    for index in range(0, len(raw), 2):
        output.append(raw[index : index + 2])
    preview = output.preview()
    output.close()

    assert output.path.read_bytes() == raw
    assert preview["byte_count"] == len(raw)
    assert preview["content_hash"]
    assert preview["truncated"] is True
    assert set(preview["truncation_reason"]) == {"byte_limit", "line_limit"}
    assert "\ufffd" not in preview["head"]
    assert "\ufffd" not in preview["tail"]
    output.discard()


def test_bounded_output_handles_binary_and_single_long_lines() -> None:
    output = BoundedOutput(max_bytes=32, max_lines=2)
    raw = b"\x00\xff" * 128
    output.append(raw)
    preview = output.preview()

    assert preview["byte_count"] == len(raw)
    assert preview["line_count"] == 1
    assert preview["truncated"] is True
    assert preview["truncation_reason"] == ["byte_limit"]
    output.discard()


def test_tool_result_raw_cas_is_complete_while_transcript_is_bounded(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "bounded-tool-result")
    result = ToolResult(
        call_id="bounded-call",
        status="success",
        tool_name="fixture:large",
        output={"body": "x" * 300_000, "items": list(range(500))},
    )

    artifact_ids = service.conversation.record_tool_results("bounded-request", run_id, (result,))
    artifact = service.runtime.artifacts.get_ref(artifact_ids["bounded-call"], run_id=run_id)
    assert artifact is not None
    assert json.loads(service.read_artifact(run_id, artifact.artifact_id)) == result.to_dict()
    message = service.transcript(run_id)[-1]
    assert len(json.dumps(message.content, ensure_ascii=False).encode("utf-8")) < 32_000
    assert message.content["raw"]["artifact"]["artifact_ref"] == artifact.artifact_id


def test_local_worker_preview_is_bounded_but_artifact_is_complete(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "bounded-local-worker")
    task = WorkerTask(
        task_id="bounded-local-task",
        run_id=run_id,
        capability="local.command",
        payload={
            "argv": [sys.executable, "-c", "print('line\\n' * 20000, end='')"],
        },
        idempotency_key="bounded-local-idempotency",
        metadata={"worker_kind": "local"},
    )

    result = service.execute_worker(task)
    assert result.status == "completed"
    stdout = service.runtime.artifacts.get_ref(result.artifact_refs[0], run_id=run_id)
    assert stdout is not None
    raw = service.read_artifact(run_id, stdout.artifact_id)
    assert len(raw) >= 100_000
    assert stdout.preview["truncated"] is True
    assert stdout.preview["line_count"] > 10_000
