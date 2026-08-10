from __future__ import annotations

import argparse
import json
import tempfile
from pathlib import Path

from .runtime.operation_runtime import OperationRuntime


def _self_test(root: Path | None = None) -> int:
    if root is None:
        with tempfile.TemporaryDirectory(prefix="redteam-agent-self-test-") as directory:
            return _self_test(Path(directory))
    runtime = OperationRuntime(root=root)
    state = runtime.start(
        session_id="self-test",
        objective=f"先给我针对 {root} 的方案，暂不修改文件，不用执行测试",
        targets=(str(root),),
        max_actions=16,
    )
    result = runtime.resume(state.run_id, max_actions=16)
    payload = result.summary()
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
    terminal = payload.get("terminal") if isinstance(payload.get("terminal"), dict) else {}
    return 0 if terminal.get("terminal") and terminal.get("success") else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="redteam-agent")
    subcommands = parser.add_subparsers(dest="command", required=True)

    self_test = subcommands.add_parser("self-test", help="Run an isolated runtime self-test")
    self_test.add_argument("--root", type=Path, default=None)

    mcp = subcommands.add_parser("mcp", help="Start the MCP stdio transport")
    mcp.add_argument("arguments", nargs=argparse.REMAINDER)

    arguments = parser.parse_args(argv)
    if arguments.command == "self-test":
        return _self_test(arguments.root)
    if arguments.command == "mcp":
        from .runtime.mcp_transport import main as mcp_main

        return mcp_main(arguments.arguments)
    return 2
