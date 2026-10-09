"""Exercise the public MCP lifecycle through a real stdio host, without a model."""
from __future__ import annotations

import json
import sys
import tempfile
from pathlib import Path


PUBLIC_TOOLS = {"redteam_run", "redteam_status", "redteam_evidence", "redteam_cancel", "redteam_events"}


def host_environment(root: Path) -> dict[str, str]:
    from redteam_agent.runtime.mcp_clients import _child_environment

    return _child_environment({"REDTEAM_AGENT_HOME": str(root / "isolated-home"),
        "REDTEAM_AGENT_CONFIG": str(root / "absent.toml"), "PYTHONUTF8": "1",
        "PYTHONDONTWRITEBYTECODE": "1"})


def exercise(command: str, *, root: Path, args=(), source: Path | None = None) -> dict:
    from redteam_agent.runtime.mcp_clients import StdioMcpClient

    root.mkdir(parents=True, exist_ok=True)
    environment = host_environment(root)
    if source is not None:
        environment["PYTHONPATH"] = str(source)
    client = StdioMcpClient("trace-host-smoke", command, (*args, "--root", str(root)),
                            environment, cwd=root)

    def call(name, arguments):
        result = client.call_tool(name, arguments, timeout=60)
        assert not result.get("isError"), result
        assert isinstance(result.get("structuredContent"), dict), result
        return result["structuredContent"]

    try:
        assert {item["name"] for item in client.list_tools()} == PUBLIC_TOOLS
        plan = call("redteam_run", {"session_id": "mcp-host-plan",
            "objective": "Give me a plan; do not make changes yet and no need to run tests",
            "targets": [str(root)], "max_actions": 32})
        run_id = plan["run_id"]
        status = call("redteam_status", {"run_id": run_id})
        assert status["status"] == "completed", status
        assert status["terminal"]["success"], status
        evidence = status["evidence"]
        assert evidence, status
        node = call("redteam_evidence", {"run_id": run_id, "evidence_id": evidence[0]["evidence_id"]})
        assert node, node
        events = call("redteam_events", {"run_id": run_id})
        assert events["events"], events
        resumed = call("redteam_run", {"run_id": run_id})
        assert resumed["status"] == "completed" and resumed["run_id"] == run_id, resumed
        assert len(call("redteam_events", {"run_id": run_id})["events"]) == len(events["events"])
        pending = call("redteam_run", {"session_id": "mcp-host-pending",
            "objective": "Inspect the target after a target is supplied", "auto_continue": False})
        cancelled = call("redteam_cancel", {"run_id": pending["run_id"], "reason": "host_smoke_complete"})
        assert cancelled["status"] == "cancelled", cancelled
        assert call("redteam_status", {"run_id": pending["run_id"]})["status"] == "cancelled"
        return {"mcp_tools": sorted(PUBLIC_TOOLS), "plan": "completed", "evidence": "read",
                "events": "read", "resume": "idempotent", "cancel": "persisted"}
    finally:
        client.close()


def main() -> None:
    source = Path(__file__).resolve().parents[1] / "src"
    sys.path.insert(0, str(source))
    with tempfile.TemporaryDirectory(prefix="trace-mcp-host-") as temporary:
        result = exercise(sys.executable, args=("-B", "-m", "redteam_agent.runtime.mcp_transport"),
                          root=Path(temporary), source=source)
    print(json.dumps(result))


if __name__ == "__main__":
    main()
