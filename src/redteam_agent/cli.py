from __future__ import annotations

import argparse
import json
import tempfile
import os
import sys
from pathlib import Path
from uuid import uuid4


class _Parser(argparse.ArgumentParser):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, allow_abbrev=False, **kwargs)

    def error(self, message: str) -> None:
        # argparse normally echoes rejected values, including accidentally pasted keys.
        self.exit(2, '{"error":"invalid_arguments","ok":false,"schema_version":1}\n')


def _root(value: Path | str | None = None) -> Path:
    agent_home = Path(os.environ.get("REDTEAM_AGENT_HOME") or Path.home() / ".redteam-agent")
    return Path(value or os.environ.get("TRACE_HOME") or agent_home / "operations").expanduser().resolve()


def _print(payload: object) -> None:
    from .runtime.security import redact_sensitive

    print(json.dumps(redact_sensitive(payload), ensure_ascii=False, sort_keys=True, default=str))


def _provider_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--root", type=Path, help="State directory; defaults to TRACE_HOME or REDTEAM_AGENT_HOME/operations")
    parser.add_argument("--config", action="append", default=[])
    parser.add_argument("--model", default="")
    parser.add_argument("--api-base-url", default="")
    parser.add_argument("--api-key-env", default="", help="Environment variable name containing the provider key")
    parser.add_argument("--api-timeout-seconds", type=float)
    parser.add_argument("--model-context-tokens", type=int)


def _operation_command(arguments: argparse.Namespace) -> int:
    from .application.agent_service import AgentService

    options = {"model": getattr(arguments, "model", ""), "base_url": getattr(arguments, "api_base_url", ""),
               "api_key_env": getattr(arguments, "api_key_env", ""),
               "timeout_seconds": getattr(arguments, "api_timeout_seconds", None),
               "max_context_tokens": getattr(arguments, "model_context_tokens", None)}
    with AgentService(root=_root(arguments.root), config_paths=getattr(arguments, "config", []),
                      provider_options=options) as service:
        command = arguments.command
        if command == "start":
            objective = arguments.objective or arguments.objective_option
            if not objective or (arguments.objective and arguments.objective_option):
                raise ValueError("one_objective_required")
            request = {"session_id": arguments.session_id or f"cli-{uuid4().hex}",
                       "objective": objective, "targets": [*arguments.target, *arguments.targets],
                       "workflow_hint": arguments.workflow, "token_limit": arguments.token_limit,
                       "time_limit_seconds": arguments.time_limit_seconds, "deadline": arguments.deadline}
            if arguments.max_actions is not None:
                request["max_actions"] = arguments.max_actions
            payload = service.start(request).to_dict()
        elif command in {"run", "resume"}:
            if arguments.max_cycles < 1 or (arguments.max_actions is not None and arguments.max_actions < 1):
                raise ValueError("execution_limit_must_be_positive")
            method = service.resume if command == "resume" else service.run
            view = method(arguments.run_id, {"actions": arguments.add_actions, "tokens": arguments.add_tokens,
                "time_seconds": arguments.add_time_seconds, "deadline": arguments.deadline,
                "idempotency_key": arguments.idempotency_key,
                "acknowledge_missing_usage": arguments.acknowledge_missing_usage},
                max_actions=arguments.max_actions, run_until_pause=not arguments.single_cycle,
                max_cycles=arguments.max_cycles)
            _print(view.to_dict())
            return 1 if view.run.status == "failed" else 0
        elif command == "cancel":
            payload = service.cancel(arguments.run_id, reason=arguments.reason).to_dict()
        elif command == "status":
            payload = service.status(arguments.run_id).to_dict()
        elif command == "events":
            if arguments.after < 0 or not 1 <= arguments.limit <= 10000:
                raise ValueError("event_bounds_invalid")
            events = service.events(arguments.run_id, after_sequence=arguments.after, limit=arguments.limit)
            rows = [{"run_id": event.run_id, "sequence": event.sequence, "event_type": event.event_type,
                     "payload": dict(event.payload), "created_at": event.created_at} for event in events]
            if arguments.jsonl:
                for row in rows:
                    _print(row)
                return 0
            payload = {"run_id": arguments.run_id, "events": rows,
                       "next_sequence": events[-1].sequence if events else arguments.after}
        else:
            view = service.status(arguments.run_id)
            if arguments.evidence_id:
                payload = service.evidence_lineage(arguments.run_id, arguments.evidence_id,
                    direction=arguments.direction, include_payload=arguments.include_payload)
            else:
                rows = [item.to_dict() for item in view.evidence]
                if not arguments.include_payload:
                    for row in rows:
                        row.pop("payload", None)
                payload = {"run_id": arguments.run_id, "evidence": rows}
        _print(payload)
    return 0


def _state_command(arguments: argparse.Namespace) -> int:
    from .state_backup import backup, restore, verify

    if arguments.state_command == "backup":
        payload = backup(_root(arguments.root), arguments.archive)
    elif arguments.state_command == "restore":
        payload = restore(arguments.archive, _root(arguments.root))
    else:
        payload = verify(arguments.archive)
    _print(payload)
    return 0


def _tools_command(arguments: argparse.Namespace) -> int:
    try:
        if arguments.command == "setup":
            from .runtime.tool_setup import setup

            payload = setup(arguments.tools or ["chromium", "rizin"], root=arguments.tools_dir,
                            offline=arguments.offline, proxy=arguments.proxy, timeout=arguments.timeout,
                            progress=lambda message: print(message, file=sys.stderr, flush=True))
            success = payload["success"]
        else:
            from .runtime.settings import _default_config_paths
            from .runtime.tool_doctor import doctor

            payload = doctor(root=arguments.tools_dir, configs=_default_config_paths(arguments.config),
                             probe_browser=arguments.probe_browser, runtime_root=arguments.root)
            success = payload["ready"]
    except KeyboardInterrupt:
        print("trace setup/doctor cancelled; previous active tools preserved", file=sys.stderr)
        return 130
    except (OSError, ValueError) as exc:
        # Paths/proxy values in exception messages may contain credentials.
        payload, success = {"success": False, "error": type(exc).__name__}, False
    if arguments.json:
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True))
    else:
        print(f"Tools: {payload.get('tools_root', '')}")
        for item in payload.get("tools", []):
            status = item.get("status", "installed" if item.get("installed") else "missing")
            print(f"{item['name']}: {status} ({item.get('source', 'missing')}) "
                  f"{item.get('version', '')} {item.get('path', '')}".rstrip())
            if item.get("error"):
                print(f"  {item['error']}")
            if not item.get("installed") and item.get("repair"):
                print(f"  repair: {item['repair']}")
        if payload.get("error"):
            print(payload["error"])
        if arguments.command == "doctor":
            print("Use --json for the capability matrix; --probe-browser validates an isolated browser launch.")
    return 0 if success else 1


def _self_test(root: Path | None = None) -> int:
    if root is None:
        with tempfile.TemporaryDirectory(prefix="redteam-agent-self-test-") as directory:
            return _self_test(Path(directory))
    from .application.agent_service import AgentService

    with AgentService(root=root, load_external_configuration=False) as service:
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


def _mcp_doctor(configs: list[str], root: Path | None = None) -> int:
    from .runtime.mcp_transport import _default_config_paths
    from .runtime.tool_broker import ToolBroker

    broker = ToolBroker()
    broker.bind_workspace_root(_root(root) / "workspaces")
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

    service = AgentService(root=_root(arguments.root))
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


def _main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if argv[:1] == ["mcp"]:
        from .runtime.mcp_transport import main as mcp_main

        forwarded = argv[1:]
        if forwarded[:1] == ["--"]:
            forwarded = forwarded[1:]
        check = _Parser(prog="trace mcp")
        _provider_arguments(check)
        check.parse_args(forwarded)
        return mcp_main(forwarded)
    parser = _Parser(prog="trace")
    subcommands = parser.add_subparsers(dest="command", required=True)

    self_test = subcommands.add_parser("self-test", help="Run an isolated runtime self-test")
    self_test.add_argument("--root", type=Path, default=None)

    mcp = subcommands.add_parser("mcp", help="Start the MCP stdio transport")
    mcp.add_argument("arguments", nargs=argparse.REMAINDER)

    mcp_doctor = subcommands.add_parser("mcp-doctor", help="Discover MCP servers and print bounded status")
    mcp_doctor.add_argument("--config", action="append", default=[])
    mcp_doctor.add_argument("--root", type=Path)

    for name in ("start", "run", "resume", "status", "cancel", "events", "evidence"):
        command = subcommands.add_parser(name, help=f"{name.title()} a durable operation (JSON output)")
        command.add_argument("--json", action="store_true", help="JSON is the default output")
        if name in {"start", "run", "resume"}:
            _provider_arguments(command)
            command.add_argument("--max-actions", type=int)
            command.add_argument("--deadline", default="")
        else:
            command.add_argument("--root", type=Path)
        if name == "start":
            command.add_argument("objective", nargs="?")
            command.add_argument("--objective", dest="objective_option")
            command.add_argument("--target", action="append", default=[])
            command.add_argument("--targets", nargs="+", default=[])
            command.add_argument("--session-id", default="")
            command.add_argument("--workflow", default="")
            command.add_argument("--token-limit", type=int)
            command.add_argument("--time-limit-seconds", type=float)
        else:
            command.add_argument("run_id")
        if name in {"run", "resume"}:
            command.add_argument("--single-cycle", action="store_true", help="Stop after one model cycle")
            command.add_argument("--max-cycles", type=int, default=32)
            command.add_argument("--add-actions", type=int, default=0)
            command.add_argument("--add-tokens", type=int, default=0)
            command.add_argument("--add-time-seconds", type=float, default=0)
            command.add_argument("--idempotency-key", default="")
            command.add_argument("--acknowledge-missing-usage", action="store_true")
        if name == "cancel":
            command.add_argument("--reason", default="user_requested")
        if name == "events":
            command.add_argument("--after", type=int, default=0)
            command.add_argument("--limit", type=int, default=200)
            command.add_argument("--jsonl", action="store_true")
        if name == "evidence":
            command.add_argument("evidence_id", nargs="?")
            command.add_argument("--direction", choices=("ancestors", "descendants", "both"), default="both")
            command.add_argument("--include-payload", action="store_true")

    state = subcommands.add_parser("state", help="Back up or restore stopped state; stop Web/MCP/workers first")
    state_commands = state.add_subparsers(dest="state_command", required=True)
    for name in ("backup", "restore", "verify"):
        command = state_commands.add_parser(name, help=f"{name.title()} a versioned, SHA-256 checked ZIP")
        command.add_argument("archive", type=Path)
        if name != "verify":
            command.add_argument("--root", type=Path)

    setup = subcommands.add_parser("setup", help="Explicitly install missing pinned portable tools in a user directory")
    setup.add_argument("tools", nargs="*", choices=("chromium", "rizin"), metavar="TOOL")
    setup.add_argument("--tools-dir", type=Path, help="Override TRACE_TOOLS_HOME for this command")
    setup.add_argument("--offline", action="store_true", help="Only use verified cached archives; never access the network")
    setup.add_argument("--proxy", help="HTTP(S) proxy; defaults to HTTPS_PROXY/HTTP_PROXY environment settings")
    setup.add_argument("--timeout", type=float, default=600, help="Total setup timeout in seconds (default: 600)")
    setup.add_argument("--json", action="store_true")

    doctor = subcommands.add_parser("doctor", help="Inspect local tools and capability readiness without downloads")
    doctor.add_argument("--tools-dir", type=Path)
    doctor.add_argument("--root", type=Path, help="Runtime state directory to inspect without opening a service")
    doctor.add_argument("--config", action="append", default=[])
    doctor.add_argument("--json", action="store_true")
    doctor.add_argument("--probe-browser", action="store_true", help="Launch an isolated headless browser and validate a local page")

    session = subcommands.add_parser("session", help="Inspect or export one durable operation")
    session_commands = session.add_subparsers(dest="session_command", required=True)
    for name, help_text in (
        ("inspect", "Print the bounded transparency projection"),
        ("export", "Print the complete transparency export"),
    ):
        command = session_commands.add_parser(name, help=help_text)
        command.add_argument("run_id")
        command.add_argument("--root", type=Path)
        command.add_argument("--limit", type=int, default=1000 if name == "inspect" else 10000)
    events = session_commands.add_parser("events", help="Read the durable event stream")
    events.add_argument("run_id")
    events.add_argument("--root", type=Path)
    events.add_argument("--after", type=int, default=0)
    events.add_argument("--limit", type=int, default=200)
    events.add_argument("--jsonl", action="store_true")
    tools = session_commands.add_parser("tools", help="Explain tool visibility")
    tools.add_argument("run_id")
    tools.add_argument("--root", type=Path)
    tools.add_argument("--tool", dest="tool_name", default="")
    context = session_commands.add_parser("context", help="Show context and compaction usage")
    context.add_argument("run_id")
    context.add_argument("--root", type=Path)
    evidence = session_commands.add_parser("evidence", help="Query evidence lineage")
    evidence.add_argument("run_id")
    evidence.add_argument("evidence_id")
    evidence.add_argument("--root", type=Path)
    evidence.add_argument("--direction", choices=("ancestors", "descendants", "both"), default="both")
    evidence.add_argument("--include-payload", action="store_true")

    arguments = parser.parse_args(argv)
    if arguments.command == "self-test":
        return _self_test(arguments.root)
    if arguments.command in {"setup", "doctor"}:
        return _tools_command(arguments)
    if arguments.command == "mcp":
        from .runtime.mcp_transport import main as mcp_main

        return mcp_main(arguments.arguments)
    if arguments.command == "mcp-doctor":
        return _mcp_doctor(arguments.config, arguments.root)
    if arguments.command == "session":
        return _session_command(arguments)
    if arguments.command == "state":
        return _state_command(arguments)
    if arguments.command in {"start", "run", "resume", "status", "cancel", "events", "evidence"}:
        return _operation_command(arguments)
    return 2


def main(argv: list[str] | None = None) -> int:
    try:
        return _main(argv)
    except KeyboardInterrupt:
        _print({"schema_version": 1, "ok": False, "error": "interrupted"})
        return 130
    except Exception as exc:
        from .state_backup import StateBackupError

        # Never print arbitrary parser/provider/OS exception text or a traceback.
        code = exc.code if isinstance(exc, StateBackupError) else type(exc).__name__
        _print({"schema_version": 1, "ok": False, "error": code})
        return 2 if isinstance(exc, (ValueError, TypeError, KeyError)) else 1
