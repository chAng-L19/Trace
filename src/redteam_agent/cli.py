from __future__ import annotations

import argparse
import json
import tempfile
import os
from pathlib import Path

from .runtime.operation_runtime import OperationRuntime


def _self_test(root: Path | None = None) -> int:
    if root is None:
        with tempfile.TemporaryDirectory(prefix="redteam-agent-self-test-") as directory:
            return _self_test(Path(directory))
    runtime = OperationRuntime(root=root)
    state = runtime.start(
        session_id="self-test",
        objective=f"Give me a plan for {root}; do not make changes yet and no need to run tests",
        targets=(str(root),),
        max_actions=16,
    )
    result = runtime.resume(state.run_id, max_actions=16)
    payload = result.summary()
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
    terminal = payload.get("terminal") if isinstance(payload.get("terminal"), dict) else {}
    return 0 if terminal.get("terminal") and terminal.get("success") else 1


def _mcp_doctor(configs: list[str]) -> int:
    from .runtime.mcp_transport import _default_config_paths
    from .runtime.tool_broker import ToolBroker

    broker = ToolBroker()
    root = Path(os.environ.get("REDTEAM_AGENT_HOME") or (Path.home() / ".redteam-agent"))
    broker.bind_workspace_root(root.expanduser().resolve(strict=False) / "operations" / "workspaces")
    try:
        descriptors = broker.discover_from_configs(_default_config_paths(configs))
        payload = {
            "servers": broker.server_statuses(),
            "discovery_errors": list(broker.discovery_errors),
            "tools": [
                {
                    "name": item.qualified_name,
                    "capabilities": list(item.capabilities),
                    "side_effecting": item.side_effecting,
                    "source": item.source,
                    "schema_hash": item.schema_hash,
                    "preset": str(item.metadata.get("mcp_preset") or ""),
                    "scope": str(item.metadata.get("mcp_scope") or ""),
                }
                for item in descriptors
            ],
        }
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
        return 1 if broker.discovery_errors else 0
    finally:
        broker.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="redteam-agent")
    subcommands = parser.add_subparsers(dest="command", required=True)

    self_test = subcommands.add_parser("self-test", help="Run an isolated runtime self-test")
    self_test.add_argument("--root", type=Path, default=None)

    mcp = subcommands.add_parser("mcp", help="Start the MCP stdio transport")
    mcp.add_argument("arguments", nargs=argparse.REMAINDER)

    doctor = subcommands.add_parser("mcp-doctor", help="Discover MCP servers and print bounded status")
    doctor.add_argument("--config", action="append", default=[])

    arguments = parser.parse_args(argv)
    if arguments.command == "self-test":
        return _self_test(arguments.root)
    if arguments.command == "mcp":
        from .runtime.mcp_transport import main as mcp_main

        return mcp_main(arguments.arguments)
    if arguments.command == "mcp-doctor":
        return _mcp_doctor(arguments.config)
    return 2
