from __future__ import annotations

import json
import sys
from dataclasses import replace
from io import BytesIO, StringIO
from pathlib import Path
from typing import Any

import pytest


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.durable_store import DurableStore
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.mcp_server import (
    MAX_OBSERVATION_BYTES,
    MAX_TOOL_ARGUMENT_BYTES,
    PUBLIC_TOOL_NAMES,
    RuntimeMcpServer,
    _iter_request_lines,
    _runtime_settings,
    _serve_stdio,
)
from redteam_agent.runtime.models import OperationState, WorkflowSpec
from redteam_agent.runtime.operation_runtime import OperationRuntime


class _Result:
    def __init__(self, payload: dict[str, Any]) -> None:
        self.payload = payload

    def summary(self) -> dict[str, Any]:
        return dict(self.payload)


class _CompatibilityRuntime:
    """Old facade shape used to prove MCP-side receipt compatibility."""

    def __init__(self, root: Path) -> None:
        self.store = DurableStore(root)
        self.submissions: list[dict[str, Any]] = []
        self.budget_deltas: list[dict[str, Any]] = []

    def status(self, run_id: str) -> _Result:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        action_id = state.current_action_id
        return _Result(
            {
                "run_id": run_id,
                "status": state.status,
                "current_action": action_id,
                "next_action": action_id,
                "next_action_spec": (
                    {
                        "action_id": action_id,
                        "name": "Host-only fixture",
                        "required_capabilities": ["fixture.host"],
                        "expected_artifact": "surface_map",
                        "verifier": "surface_map",
                        "target": state.goal.targets[0],
                        "evidence_refs": [],
                        "output_contract": {"required": ["results"]},
                        "feedback_gate": {"predicate": "artifact_verified"},
                        "exit_condition": "verified_artifact:surface_map",
                    }
                    if action_id
                    else None
                ),
                "missing_capabilities": ["fixture.host"] if action_id else [],
                "evidence": [],
                "terminal": {
                    "terminal": state.status == "completed",
                    "success": state.status == "completed",
                    "reason": "complete" if state.status == "completed" else "pending",
                    "satisfied": [],
                    "missing": [] if state.status == "completed" else ["surface_map"],
                },
            }
        )

    def resume(self, run_id: str, *, max_actions: int | None = None) -> _Result:
        return self.status(run_id)

    def apply_budget_delta(
        self,
        run_id: str,
        *,
        actions: int = 0,
        tokens: int = 0,
        time_seconds: float = 0.0,
        deadline: str = "",
    ) -> _Result:
        self.budget_deltas.append(
            {
                "run_id": run_id,
                "actions": actions,
                "tokens": tokens,
                "time_seconds": time_seconds,
                "deadline": deadline,
            }
        )
        return self.status(run_id)

    def submit_observation(
        self,
        *,
        run_id: str,
        action_id: str,
        output: Any,
        tool: str,
        continue_run: bool,
        max_actions: int,
    ) -> _Result:
        state = self.store.load_operation(run_id)
        assert state is not None
        assert state.current_action_id == action_id
        self.submissions.append({"run_id": run_id, "action_id": action_id, "output": output, "tool": tool})
        state.action_status[action_id] = "completed"
        state.current_action_id = ""
        state.status = "completed"
        self.store.save_operation(state)
        return self.status(run_id)


def _waiting_operation(
    runtime: _CompatibilityRuntime,
    *,
    session_id: str = "mcp-session",
    batch_session_id: str = "",
    batch_index: int = 0,
    batch_size: int = 0,
) -> OperationState:
    goal = GoalCompiler().compile("Analyze https://target.invalid")
    if batch_session_id:
        goal = replace(
            goal,
            starting_context={
                **dict(goal.starting_context),
                "batch_session_id": batch_session_id,
                "parent_session_id": "batch-parent",
                "batch_index": batch_index,
                "batch_size": batch_size,
            },
        )
        session_id = f"{batch_session_id}:{batch_index}"
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {
                "id": "mcp-host-fixture",
                "version": 1,
                "name": "MCP host fixture",
                "required_artifacts": ["surface_map"],
            },
            "actions": [
                {
                    "id": "host-action",
                    "name": "Host action",
                    "capabilities": ["fixture.host"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
        }
    )
    state = OperationState.create(session_id=session_id, goal=goal, workflow=workflow)
    action_id = next(iter(state.action_status))
    state.status = "waiting_host"
    state.current_action_id = action_id
    runtime.store.create_operation(state, event={"source": "mcp-handoff-test"})
    return state


def test_public_mcp_surface_is_exactly_the_unified_five_tools() -> None:
    assert PUBLIC_TOOL_NAMES == (
        "redteam_run",
        "redteam_status",
        "redteam_evidence",
        "redteam_cancel",
        "redteam_events",
    )


def test_redteam_run_consumes_bound_handoff_once_without_user_relay(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    state = _waiting_operation(runtime)
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]

    status_result = server._call_tool("redteam_status", {"run_id": state.run_id})
    summary = status_result["structuredContent"]
    handoff = summary["next_action_spec"]["handoff"]
    assert handoff["run_id"] == state.run_id
    assert handoff["branch_id"] == state.branch_id
    assert handoff["plan_revision"] == state.plan_revision
    assert handoff["action_id"] == state.current_action_id
    repeated = server._call_tool("redteam_status", {"run_id": state.run_id})["structuredContent"][
        "next_action_spec"
    ]["handoff"]
    assert repeated == handoff

    with runtime.store.connection() as connection:
        row = connection.execute(
            "SELECT token_hash, status FROM host_handoffs WHERE handoff_id=?",
            (handoff["handoff_id"],),
        ).fetchone()
    assert row is not None and row["status"] == "pending"
    assert handoff["handoff_token"] not in row["token_hash"]

    forged = {
        **handoff,
        "handoff_token": "forged",
        "output": {"results": ["fixture"]},
    }
    with pytest.raises(ValueError, match="handoff_receipt_rejected"):
        server._call_tool("redteam_run", {"run_id": state.run_id, "observation": forged})
    assert runtime.store.get_handoff(handoff["handoff_id"]).status == "pending"

    observation = {**handoff, "output": {"results": ["fixture"]}}
    run_result = server._call_tool(
        "redteam_run",
        {"run_id": state.run_id, "observation": observation},
    )
    assert run_result["structuredContent"]["status"] == "completed"
    assert runtime.submissions == [
        {
            "run_id": state.run_id,
            "action_id": handoff["action_id"],
            "output": {"results": ["fixture"]},
            "tool": "host-agent",
        }
    ]
    assert runtime.store.get_handoff(handoff["handoff_id"]).status == "consumed"
    with pytest.raises(ValueError, match="handoff_receipt_not_pending"):
        server._call_tool(
            "redteam_run",
            {"run_id": state.run_id, "observation": observation},
        )


def test_handoff_rejects_cross_run_and_cross_revision_receipts(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    first = _waiting_operation(runtime, session_id="first")
    second = _waiting_operation(runtime, session_id="second")
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]
    handoff = server._call_tool("redteam_status", {"run_id": first.run_id})["structuredContent"][
        "next_action_spec"
    ]["handoff"]
    observation = {**handoff, "output": {"results": ["fixture"]}}

    with pytest.raises(ValueError, match="handoff_receipt_identity_mismatch"):
        server._call_tool("redteam_run", {"run_id": second.run_id, "observation": observation})

    current = runtime.store.load_operation(first.run_id)
    assert current is not None
    current.plan_revision += 1
    runtime.store.save_operation(current)
    with pytest.raises(ValueError, match="handoff_receipt_rejected"):
        server._call_tool("redteam_run", {"run_id": first.run_id, "observation": observation})
    assert runtime.submissions == []


def test_tools_call_schema_requires_complete_handoff_receipt(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    state = _waiting_operation(runtime)
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]
    response = server.handle(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {
                "name": "redteam_run",
                "arguments": {
                    "run_id": state.run_id,
                    "observation": {"output": {"results": ["fixture"]}},
                },
            },
        }
    )
    assert response is not None
    assert response["error"]["code"] == -32602
    assert "handoff_id" in response["error"]["message"]


def test_redteam_run_applies_incremental_budget_before_resume(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    state = _waiting_operation(runtime)
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]
    result = server._call_tool(
        "redteam_run",
        {
            "run_id": state.run_id,
            "budget_delta": {
                "actions": 4,
                "tokens": 2048,
                "time_seconds": 60.0,
                "deadline": "2099-01-01T00:00:00Z",
            },
            "auto_continue": False,
        },
    )
    assert result["structuredContent"]["status"] == "waiting_host"
    assert runtime.budget_deltas == [
        {
            "run_id": state.run_id,
            "actions": 4,
            "tokens": 2048,
            "time_seconds": 60.0,
            "deadline": "2099-01-01T00:00:00Z",
        }
    ]
    with pytest.raises(ValueError, match="use_budget_delta_actions_when_resuming"):
        server._call_tool(
            "redteam_run",
            {"run_id": state.run_id, "max_total_actions": 512},
        )


def test_mcp_restart_rotates_lost_token_and_supersedes_old_placeholder(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    root = tmp_path / "operations"
    first_runtime = _CompatibilityRuntime(root)
    state = _waiting_operation(first_runtime)
    first_server = RuntimeMcpServer(first_runtime)  # type: ignore[arg-type]
    old_handoff = first_server._call_tool(
        "redteam_status",
        {"run_id": state.run_id},
    )["structuredContent"]["next_action_spec"]["handoff"]

    restarted_runtime = _CompatibilityRuntime(root)
    restarted_server = RuntimeMcpServer(restarted_runtime)  # type: ignore[arg-type]
    new_handoff = restarted_server._call_tool(
        "redteam_status",
        {"run_id": state.run_id},
    )["structuredContent"]["next_action_spec"]["handoff"]

    assert new_handoff["handoff_id"] != old_handoff["handoff_id"]
    assert new_handoff["attempt_id"] != old_handoff["attempt_id"]
    assert restarted_runtime.store.get_handoff(old_handoff["handoff_id"]).status == "superseded"
    attempts = {
        item.attempt_id: item
        for item in restarted_runtime.store.task_attempts(state.run_id)
    }
    assert attempts[old_handoff["attempt_id"]].status == "superseded"
    assert attempts[new_handoff["attempt_id"]].status == "waiting_host"
    with pytest.raises(ValueError, match="handoff_receipt_not_pending"):
        restarted_server._call_tool(
            "redteam_run",
            {
                "run_id": state.run_id,
                "observation": {**old_handoff, "output": {"results": ["stale"]}},
            },
        )
    result = restarted_server._call_tool(
        "redteam_run",
        {
            "run_id": state.run_id,
            "observation": {**new_handoff, "output": {"results": ["fresh"]}},
        },
    )
    assert result["structuredContent"]["status"] == "completed"


def test_runtime_repeated_resume_keeps_live_receipt_stable(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    goal = GoalCompiler().compile("Analyze https://target.invalid")
    workflow = WorkflowSpec.from_dict(
        {
            "workflow": {"id": "runtime-host-fixture", "version": 1, "name": "Runtime host fixture"},
            "actions": [
                {
                    "id": "host-action",
                    "name": "Host action",
                    "capabilities": ["fixture.host"],
                    "expected_artifact": "surface_map",
                    "verifier": "surface_map",
                }
            ],
        }
    )
    state = OperationState.create(session_id="runtime-handoff", goal=goal, workflow=workflow)
    runtime.store.create_operation(state, event={"source": "runtime-handoff-test"})

    first = runtime.resume(state.run_id, max_actions=1).summary()["next_action_spec"]["handoff"]
    second = runtime.resume(state.run_id, max_actions=1).summary()["next_action_spec"]["handoff"]

    assert second == first
    assert runtime.store.get_handoff(first["handoff_id"]).status == "pending"
    assert len(runtime.store.task_attempts(state.run_id, action_id="host-action")) == 1


def test_batch_status_is_non_advancing_and_run_accepts_per_run_receipts(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    batch_id = "batch-fixture"
    first = _waiting_operation(
        runtime,
        batch_session_id=batch_id,
        batch_index=1,
        batch_size=2,
    )
    second = _waiting_operation(
        runtime,
        batch_session_id=batch_id,
        batch_index=2,
        batch_size=2,
    )
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]
    status = server._call_tool("redteam_status", {"batch_session_id": batch_id})[
        "structuredContent"
    ]
    assert status["status"] == "waiting_host"
    assert [item["run_id"] for item in status["operations"]] == [first.run_id, second.run_id]
    assert runtime.submissions == []
    observations = []
    for operation in status["operations"]:
        observations.append(
            {
                "run_id": operation["run_id"],
                **operation["next_action_spec"]["handoff"],
                "output": {"results": [operation["run_id"]]},
            }
        )
    completed = server._call_tool(
        "redteam_run",
        {"batch_session_id": batch_id, "observations": observations},
    )["structuredContent"]
    assert completed["status"] == "completed"
    assert completed["terminal"]["success"] is True
    assert {item["run_id"] for item in runtime.submissions} == {first.run_id, second.run_id}


def test_transport_reader_discards_oversized_line_and_recovers_next_request() -> None:
    records = list(
        _iter_request_lines(
            BytesIO(b"x" * 65 + b"\n{}\n"),
            max_bytes=64,
        )
    )
    assert records == [(None, True), ("{}\n", False)]


def test_stdio_returns_request_too_large_then_processes_next_line() -> None:
    class _Server:
        @staticmethod
        def _error(request_id: Any, code: int, message: str) -> dict[str, Any]:
            return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}}

        @staticmethod
        def handle(payload: dict[str, Any]) -> dict[str, Any]:
            return {"jsonrpc": "2.0", "id": payload.get("id"), "result": {"ok": True}}

    output = StringIO()
    _serve_stdio(
        _Server(),  # type: ignore[arg-type]
        BytesIO(b"x" * 65 + b"\n{\"id\":2}\n"),
        output,
        max_request_bytes=64,
    )
    responses = [json.loads(line) for line in output.getvalue().splitlines()]
    assert responses[0]["error"] == {
        "code": -32600,
        "message": "invalid_request:request_too_large",
    }
    assert responses[1] == {"jsonrpc": "2.0", "id": 2, "result": {"ok": True}}


def test_observation_and_tool_argument_size_limits_precede_runtime_side_effects(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CODEX_HOME", str(tmp_path / "codex-home"))
    runtime = _CompatibilityRuntime(tmp_path / "operations")
    state = _waiting_operation(runtime)
    server = RuntimeMcpServer(runtime)  # type: ignore[arg-type]
    handoff = server._call_tool("redteam_status", {"run_id": state.run_id})["structuredContent"][
        "next_action_spec"
    ]["handoff"]
    with pytest.raises(ValueError, match="observation_output_too_large"):
        server._call_tool(
            "redteam_run",
            {
                "run_id": state.run_id,
                "observation": {
                    **handoff,
                    "output": "x" * (MAX_OBSERVATION_BYTES + 1),
                },
            },
        )
    assert runtime.store.get_handoff(handoff["handoff_id"]).status == "pending"
    response = server.handle(
        {
            "jsonrpc": "2.0",
            "id": 9,
            "method": "tools/call",
            "params": {
                "name": "redteam_status",
                "arguments": {"run_id": "x" * (MAX_TOOL_ARGUMENT_BYTES + 1)},
            },
        }
    )
    assert response is not None
    assert response["error"]["code"] == -32602
    assert response["error"]["message"] == "tool_arguments_too_large"


def test_runtime_settings_default_or_clamp_malformed_values(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    config = tmp_path / "config.toml"
    config.write_text(
        """
[automation]
tool_priority = "not-an-array"
max_actions_per_cycle = "not-a-number"
action_timeout_seconds = nan
max_retries_per_action = true
max_domains = 999
max_hypothesis_branches = -5
handoff_ttl_seconds = 999999999
""".strip(),
        encoding="utf-8",
    )
    settings = _runtime_settings([config])
    assert settings["tool_priority"] == ()
    assert settings["max_actions_per_cycle"] == 64
    assert settings["action_timeout_seconds"] is None
    assert settings["max_retries_per_action"] == 2
    assert settings["max_domains"] == 7
    assert settings["max_hypothesis_branches"] == 1
    assert settings["handoff_ttl_seconds"] == 86_400.0
    warning = capsys.readouterr().err
    for key in (
        "tool_priority",
        "max_actions_per_cycle",
        "action_timeout_seconds",
        "max_retries_per_action",
        "max_domains",
        "max_hypothesis_branches",
        "handoff_ttl_seconds",
    ):
        assert f"automation.{key}" in warning
