from __future__ import annotations

import argparse
import json
import tempfile
import os
from pathlib import Path

def _self_test(root: Path | None = None) -> int:
    if root is None:
        with tempfile.TemporaryDirectory(prefix="redteam-agent-self-test-") as directory:
            return _self_test(Path(directory))
    from .application.agent_service import AgentService

    service = AgentService(root=root)
    started = service.start(
        {
            "session_id": "self-test",
            "objective": f"Give me a plan for {root}; do not make changes yet and no need to run tests",
            "targets": (str(root),),
            "max_actions": 16,
        }
    ).single
    service.run(started.run.run_id, max_actions=16)
    payload = service.summary(started.run.run_id)
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


def _session_command(arguments: argparse.Namespace) -> int:
    from .application.agent_service import AgentService

    service = AgentService(root=Path(arguments.root).expanduser().resolve())
    try:
        run_id = str(arguments.run_id)
        if arguments.session_command == "inspect":
            payload = service.inspect_session(run_id, event_limit=arguments.limit)
        elif arguments.session_command == "export":
            payload = service.export_transparency(run_id, event_limit=arguments.limit)
        elif arguments.session_command == "events":
            events = service.events(run_id, after_sequence=arguments.after, limit=arguments.limit)
            payload = {
                "run_id": run_id,
                "events": [
                    {
                        "sequence": item.sequence,
                        "event_type": item.event_type,
                        "payload": dict(item.payload),
                        "created_at": item.created_at,
                    }
                    for item in events
                ],
                "next_sequence": events[-1].sequence if events else None,
            }
        elif arguments.session_command == "tools":
            payload = service.explain_tool_visibility(run_id, tool_name=arguments.tool_name)
        elif arguments.session_command == "context":
            payload = service.context_usage(run_id)
        elif arguments.session_command == "evidence":
            payload = service.evidence_lineage(
                run_id,
                arguments.evidence_id,
                direction=arguments.direction,
                include_payload=arguments.include_payload,
            )
        else:
            return 2
        if arguments.session_command == "events" and arguments.jsonl:
            for event in payload["events"]:
                print(json.dumps(event, ensure_ascii=False, sort_keys=True, default=str))
        else:
            print(json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str))
        return 0
    finally:
        service.close()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="redteam-agent")
    subcommands = parser.add_subparsers(dest="command", required=True)

    self_test = subcommands.add_parser("self-test", help="Run an isolated runtime self-test")
    self_test.add_argument("--root", type=Path, default=None)

    mcp = subcommands.add_parser("mcp", help="Start the MCP stdio transport")
    mcp.add_argument("arguments", nargs=argparse.REMAINDER)

    doctor = subcommands.add_parser("mcp-doctor", help="Discover MCP servers and print bounded status")
    doctor.add_argument("--config", action="append", default=[])

    session = subcommands.add_parser("session", help="Inspect or export one durable operation")
    session_commands = session.add_subparsers(dest="session_command", required=True)
    for name, help_text in (
        ("inspect", "Print the bounded transparency projection"),
        ("export", "Print the complete transparency export"),
    ):
        command = session_commands.add_parser(name, help=help_text)
        command.add_argument("run_id")
        command.add_argument("--root", type=Path, required=True)
        command.add_argument("--limit", type=int, default=1000 if name == "inspect" else 10000)
    events = session_commands.add_parser("events", help="Read the durable event stream")
    events.add_argument("run_id")
    events.add_argument("--root", type=Path, required=True)
    events.add_argument("--after", type=int, default=0)
    events.add_argument("--limit", type=int, default=200)
    events.add_argument("--jsonl", action="store_true")
    tools = session_commands.add_parser("tools", help="Explain tool visibility")
    tools.add_argument("run_id")
    tools.add_argument("--root", type=Path, required=True)
    tools.add_argument("--tool", dest="tool_name", default="")
    context = session_commands.add_parser("context", help="Show context and compaction usage")
    context.add_argument("run_id")
    context.add_argument("--root", type=Path, required=True)
    evidence = session_commands.add_parser("evidence", help="Query evidence lineage")
    evidence.add_argument("run_id")
    evidence.add_argument("evidence_id")
    evidence.add_argument("--root", type=Path, required=True)
    evidence.add_argument("--direction", choices=("ancestors", "descendants", "both"), default="both")
    evidence.add_argument("--include-payload", action="store_true")

    arguments = parser.parse_args(argv)
    if arguments.command == "self-test":
        return _self_test(arguments.root)
    if arguments.command == "mcp":
        from .runtime.mcp_transport import main as mcp_main

        return mcp_main(arguments.arguments)
    if arguments.command == "mcp-doctor":
        return _mcp_doctor(arguments.config)
    if arguments.command == "session":
        return _session_command(arguments)
    return 2
