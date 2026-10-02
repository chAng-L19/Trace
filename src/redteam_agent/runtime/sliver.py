"""Bounded Sliver RPC integration for inventory and operator actions.

Sliver's operator config uses mTLS plus a Bearer token.  This adapter reads
those values from named environment variables so they never appear in tool
arguments or durable runtime state.  Actions are explicit, bounded, and use
an executable plus argument vector rather than a shell command string.
"""
from __future__ import annotations

import json
import math
import os
import re
from functools import lru_cache
from typing import Any, Mapping


MAX_GRPC_RESPONSE_BYTES = 4 * 1024 * 1024
MAX_OUTPUT_BYTES = 1 * 1024 * 1024
MAX_RESULTS = 200
DEFAULT_TIMEOUT_SECONDS = 15.0
_ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,127}$")
_HOST_NAME = re.compile(r"^[^\s\x00-\x1f\x7f/]{1,253}$")
_INVENTORY_METHODS = {
    "health": ("/rpcpb.SliverRPC/GetVersion", "Version"),
    "sessions": ("/rpcpb.SliverRPC/GetSessions", "Sessions"),
    "beacons": ("/rpcpb.SliverRPC/GetBeacons", "Beacons"),
    # Sliver exposes listeners as active jobs; there is no GetListeners RPC.
    "listeners": ("/rpcpb.SliverRPC/GetJobs", "Jobs"),
}
_ACTION_NAMES = frozenset({"execute"})
_LISTENER_PROTOCOLS = frozenset({"dns", "http", "https", "mtls", "multiplayer", "tcp", "wg"})


def _schema(required: tuple[str, ...], properties: Mapping[str, Any]) -> Mapping[str, Any]:
    return {
        "type": "object",
        "required": list(required),
        "properties": dict(properties),
        "additionalProperties": False,
        "x-agent-callable": True,
    }


SLIVER_SCHEMA = _schema(
    ("host", "port", "token_env", "ca_certificate_env", "certificate_env", "private_key_env"),
    {
        "host": {"type": "string", "minLength": 1, "maxLength": 253},
        "port": {"type": "integer", "minimum": 1, "maximum": 65535},
        "token_env": {"type": "string", "minLength": 1, "maxLength": 128},
        "ca_certificate_env": {"type": "string", "minLength": 1, "maxLength": 128},
        "certificate_env": {"type": "string", "minLength": 1, "maxLength": 128},
        "private_key_env": {"type": "string", "minLength": 1, "maxLength": 128},
        "server_name": {"type": "string", "maxLength": 253},
        "operation": {"type": "string", "enum": sorted((*_INVENTORY_METHODS, *_ACTION_NAMES))},
        "session_id": {"type": "string", "minLength": 1, "maxLength": 256},
        "path": {"type": "string", "minLength": 1, "maxLength": 4096},
        "args": {"type": "array", "maxItems": 64, "items": {"type": "string", "maxLength": 4096}},
        "output": {"type": "boolean", "default": True},
        "timeout": {"type": "number", "minimum": 0.1, "maximum": 60.0},
        "max_results": {"type": "integer", "minimum": 1, "maximum": MAX_RESULTS},
    },
)


def _required_text(arguments: Mapping[str, Any], name: str) -> str:
    value = arguments.get(name)
    if not isinstance(value, str):
        raise ValueError(f"{name}_invalid")
    value = value.strip()
    if not value:
        raise ValueError(f"{name}_required")
    return value


def _env_secret(arguments: Mapping[str, Any], field: str) -> str:
    name = _required_text(arguments, field)
    if not _ENV_NAME.fullmatch(name):
        raise ValueError(f"{field}_invalid")
    value = os.environ.get(name, "")
    if not value:
        raise ValueError(f"{field}_missing")
    if len(value.encode("utf-8", errors="replace")) > MAX_OUTPUT_BYTES:
        raise ValueError(f"{field}_too_large")
    return value


def _connection_config(arguments: Mapping[str, Any]) -> dict[str, Any]:
    if set(arguments) - SLIVER_SCHEMA["properties"].keys():
        raise ValueError("arguments_invalid")
    host = _required_text(arguments, "host")
    if not _HOST_NAME.fullmatch(host):
        raise ValueError("host_invalid")
    try:
        port = int(arguments["port"])
    except (KeyError, TypeError, ValueError, OverflowError):
        raise ValueError("port_invalid") from None
    if type(arguments.get("port")) is not int or not 1 <= port <= 65535:
        raise ValueError("port_invalid")
    operation = str(arguments.get("operation") or "health").strip().casefold()
    if operation not in {*_INVENTORY_METHODS, *_ACTION_NAMES}:
        raise ValueError("operation_invalid")
    try:
        timeout = float(arguments.get("timeout", DEFAULT_TIMEOUT_SECONDS))
    except (TypeError, ValueError, OverflowError):
        raise ValueError("timeout_invalid") from None
    if isinstance(arguments.get("timeout"), bool) or not math.isfinite(timeout) or not 0.1 <= timeout <= 60.0:
        raise ValueError("timeout_invalid")
    try:
        max_results = int(arguments.get("max_results", MAX_RESULTS))
    except (TypeError, ValueError, OverflowError):
        raise ValueError("max_results_invalid") from None
    if type(arguments.get("max_results", MAX_RESULTS)) is not int or not 1 <= max_results <= MAX_RESULTS:
        raise ValueError("max_results_invalid")
    server_name = str(arguments.get("server_name") or host).strip()
    if not _HOST_NAME.fullmatch(server_name):
        raise ValueError("server_name_invalid")
    session_id = str(arguments.get("session_id") or "").strip()
    path = str(arguments.get("path") or "").strip()
    raw_args = arguments.get("args", ())
    if not isinstance(raw_args, (list, tuple)) or len(raw_args) > 64:
        raise ValueError("args_invalid")
    args = []
    for value in raw_args:
        if not isinstance(value, str) or len(value) > 4096 or "\x00" in value:
            raise ValueError("args_invalid")
        args.append(value)
    output = arguments.get("output", True)
    if not isinstance(output, bool):
        raise ValueError("output_invalid")
    if operation == "execute":
        if not isinstance(arguments.get("session_id"), str) or not isinstance(arguments.get("path"), str):
            raise ValueError("action_arguments_invalid")
        if not session_id:
            raise ValueError("session_id_required")
        if len(session_id) > 256 or "\x00" in session_id:
            raise ValueError("session_id_invalid")
        if not path:
            raise ValueError("path_required")
        if len(path) > 4096 or "\x00" in path:
            raise ValueError("path_invalid")
    token = _env_secret(arguments, "token_env")
    if not token.isascii() or any(ord(character) < 33 or ord(character) > 126 for character in token):
        raise ValueError("token_env_invalid")
    return {
        "host": host,
        "port": port,
        "operation": operation,
        "timeout": timeout,
        "max_results": max_results,
        "server_name": server_name,
        "session_id": session_id,
        "path": path,
        "args": tuple(args),
        "output": output,
        "token": token,
        "ca_certificate": _env_secret(arguments, "ca_certificate_env").encode("utf-8"),
        "certificate": _env_secret(arguments, "certificate_env").encode("utf-8"),
        "private_key": _env_secret(arguments, "private_key_env").encode("utf-8"),
    }


def _field(message: Any, name: str, number: int, field_type: int, *, label: int = 1, type_name: str = "") -> None:
    item = message.field.add(name=name, number=number, label=label, type=field_type)
    if type_name:
        item.type_name = type_name


@lru_cache(maxsize=1)
def _proto_types() -> dict[str, type[Any]]:
    try:
        from google.protobuf import descriptor_pb2, descriptor_pool, message_factory
    except ImportError as exc:
        raise RuntimeError("sliver_python_dependencies_missing:grpcio,protobuf") from exc

    file_proto = descriptor_pb2.FileDescriptorProto(
        name="trace_sliver.proto",
        package="clientpb",
        syntax="proto3",
    )
    empty = file_proto.message_type.add(name="Empty")
    version = file_proto.message_type.add(name="Version")
    for name, number, field_type in (
        ("Major", 1, 5), ("Minor", 2, 5), ("Patch", 3, 5),
        ("Commit", 4, 9), ("Dirty", 5, 8), ("CompiledAt", 6, 3),
        ("OS", 7, 9), ("Arch", 8, 9),
    ):
        _field(version, name, number, field_type)

    session = file_proto.message_type.add(name="Session")
    session_fields = (
        ("ID", 1, 9), ("Name", 2, 9), ("Hostname", 3, 9), ("OS", 8, 9),
        ("Arch", 9, 9), ("Transport", 10, 9), ("RemoteAddress", 11, 9),
        ("PID", 12, 5), ("LastCheckin", 14, 3), ("ActiveC2", 15, 9),
        ("Version", 16, 9), ("Evasion", 17, 8), ("IsDead", 18, 8),
        ("Burned", 22, 8),
    )
    for name, number, field_type in session_fields:
        _field(session, name, number, field_type)
    sessions = file_proto.message_type.add(name="Sessions")
    _field(sessions, "Sessions", 1, 11, label=3, type_name=".clientpb.Session")

    beacon = file_proto.message_type.add(name="Beacon")
    for name, number, field_type in (
        ("ID", 1, 9), ("Name", 2, 9), ("Hostname", 3, 9), ("OS", 8, 9),
        ("Arch", 9, 9), ("Transport", 10, 9), ("RemoteAddress", 11, 9),
        ("PID", 12, 5), ("LastCheckin", 14, 3), ("ActiveC2", 15, 9),
        ("Version", 16, 9), ("Evasion", 17, 8), ("IsDead", 18, 8),
        ("Interval", 22, 3), ("Jitter", 23, 3), ("Burned", 24, 8),
        ("NextCheckin", 25, 3), ("TasksCount", 26, 3), ("TasksCountCompleted", 27, 3),
    ):
        _field(beacon, name, number, field_type)
    beacons = file_proto.message_type.add(name="Beacons")
    _field(beacons, "Beacons", 2, 11, label=3, type_name=".clientpb.Beacon")

    job = file_proto.message_type.add(name="Job")
    for name, number, field_type in (
        ("ID", 1, 13), ("Name", 2, 9), ("Description", 3, 9),
        ("Protocol", 4, 9), ("Port", 5, 13), ("Domains", 6, 9), ("ProfileName", 7, 9),
    ):
        _field(job, name, number, field_type, label=3 if name == "Domains" else 1)
    jobs = file_proto.message_type.add(name="Jobs")
    _field(jobs, "Active", 1, 11, label=3, type_name=".clientpb.Job")

    request = file_proto.message_type.add(name="Request")
    _field(request, "Async", 1, 8)
    _field(request, "Timeout", 2, 3)
    _field(request, "BeaconID", 8, 9)
    _field(request, "SessionID", 9, 9)
    execute = file_proto.message_type.add(name="ExecuteReq")
    _field(execute, "Path", 1, 9)
    _field(execute, "Args", 2, 9, label=3)
    _field(execute, "Output", 3, 8)
    _field(execute, "Stdout", 4, 9)
    _field(execute, "Stderr", 5, 9)
    _field(execute, "EnvInheritance", 6, 8)
    _field(execute, "Background", 8, 8)
    _field(execute, "Request", 9, 11, type_name=".clientpb.Request")
    rpc_response = file_proto.message_type.add(name="Response")
    _field(rpc_response, "Err", 1, 9)
    _field(rpc_response, "Async", 2, 8)
    _field(rpc_response, "BeaconID", 8, 9)
    _field(rpc_response, "TaskID", 9, 9)
    execute_resp = file_proto.message_type.add(name="ExecuteResp")
    _field(execute_resp, "Status", 1, 13)
    _field(execute_resp, "Stdout", 2, 12)
    _field(execute_resp, "Stderr", 3, 12)
    _field(execute_resp, "PID", 4, 13)
    _field(execute_resp, "Response", 9, 11, type_name=".clientpb.Response")

    descriptor = descriptor_pool.DescriptorPool().Add(file_proto)
    get_class = getattr(message_factory, "GetMessageClass", None)
    if get_class is None:
        factory = message_factory.MessageFactory(descriptor.pool)
        get_class = factory.GetPrototype
    return {
        name: get_class(descriptor.message_types_by_name[name])
        for name in ("Empty", "Version", "Sessions", "Beacons", "Jobs", "Request", "Response", "ExecuteReq", "ExecuteResp")
    }


def _rpc_error(exc: BaseException) -> str:
    try:
        code_name = str(exc.code().name).casefold()
    except (AttributeError, TypeError):
        code_name = "unknown"
    if code_name in {"unauthenticated", "permission_denied"}:
        return "sliver_authentication_failed"
    if code_name in {"deadline_exceeded", "unavailable", "resource_exhausted"}:
        return f"sliver_rpc_{code_name}"
    return "sliver_rpc_failed"


def _call_rpc(config: Mapping[str, Any], method: str, request_name: str, response_name: str) -> Any:
    try:
        import grpc
    except ImportError as exc:
        raise RuntimeError("sliver_python_dependencies_missing:grpcio,protobuf") from exc
    types = _proto_types()
    try:
        credentials = grpc.ssl_channel_credentials(
            root_certificates=config["ca_certificate"],
            private_key=config["private_key"],
            certificate_chain=config["certificate"],
        )
        target = config["host"]
        if ":" in target and not target.startswith("["):
            target = f"[{target}]"
        channel = grpc.secure_channel(
            f"{target}:{config['port']}",
            credentials,
            options=(
                ("grpc.ssl_target_name_override", config["server_name"]),
                ("grpc.max_receive_message_length", MAX_GRPC_RESPONSE_BYTES),
                ("grpc.max_send_message_length", 64 * 1024),
            ),
        )
        try:
            grpc.channel_ready_future(channel).result(timeout=config["timeout"])
            request = types[request_name]()
            if request_name == "ExecuteReq":
                request.Path = config["path"]
                request.Args.extend(config["args"])
                request.Output = config["output"]
                request.Request.SessionID = config["session_id"]
            call = channel.unary_unary(
                method,
                request_serializer=lambda message: message.SerializeToString(),
                response_deserializer=types[response_name].FromString,
            )
            return call(
                request,
                timeout=config["timeout"],
                metadata=(("authorization", f"Bearer {config['token']}"),),
            )
        finally:
            channel.close()
    except grpc.FutureTimeoutError:
        raise RuntimeError("sliver_rpc_deadline_exceeded") from None
    except grpc.RpcError as exc:
        raise RuntimeError(_rpc_error(exc)) from None
    except (ValueError, TypeError, OSError):
        raise RuntimeError("sliver_tls_configuration_invalid") from None


def _bounded_items(items: list[Mapping[str, Any]], max_results: int) -> tuple[list[Mapping[str, Any]], bool]:
    return items[:max_results], len(items) > max_results


def _version_payload(response: Any) -> Mapping[str, Any]:
    return {
        "major": int(response.Major), "minor": int(response.Minor), "patch": int(response.Patch),
        "commit": str(response.Commit), "dirty": bool(response.Dirty), "compiled_at": int(response.CompiledAt),
        "os": str(response.OS), "arch": str(response.Arch),
    }


def _sessions_payload(response: Any, max_results: int) -> Mapping[str, Any]:
    items = [
        {
            "id": str(item.ID), "name": str(item.Name), "hostname": str(item.Hostname),
            "os": str(item.OS), "arch": str(item.Arch), "transport": str(item.Transport),
            "remote_address": str(item.RemoteAddress), "pid": int(item.PID),
            "last_checkin": int(item.LastCheckin), "active_c2": str(item.ActiveC2),
            "version": str(item.Version), "evasion": bool(item.Evasion),
            "is_dead": bool(item.IsDead), "burned": bool(item.Burned),
        }
        for item in response.Sessions
    ]
    selected, truncated = _bounded_items(items, max_results)
    return {"items": selected, "count": len(selected), "truncated": truncated}


def _beacons_payload(response: Any, max_results: int) -> Mapping[str, Any]:
    items = [
        {
            "id": str(item.ID), "name": str(item.Name), "hostname": str(item.Hostname),
            "os": str(item.OS), "arch": str(item.Arch), "transport": str(item.Transport),
            "remote_address": str(item.RemoteAddress), "pid": int(item.PID),
            "last_checkin": int(item.LastCheckin), "next_checkin": int(item.NextCheckin),
            "interval": int(item.Interval), "jitter": int(item.Jitter),
            "active_c2": str(item.ActiveC2), "version": str(item.Version),
            "tasks_count": int(item.TasksCount), "tasks_count_completed": int(item.TasksCountCompleted),
            "is_dead": bool(item.IsDead), "burned": bool(item.Burned),
        }
        for item in response.Beacons
    ]
    selected, truncated = _bounded_items(items, max_results)
    return {"items": selected, "count": len(selected), "truncated": truncated}


def _listeners_payload(response: Any, max_results: int) -> Mapping[str, Any]:
    items = [
        {
            "id": int(item.ID), "name": str(item.Name), "description": str(item.Description),
            "protocol": str(item.Protocol), "port": int(item.Port),
            "domains": [str(domain) for domain in item.Domains], "profile_name": str(item.ProfileName),
        }
        for item in response.Active
        if str(item.Protocol).casefold() in _LISTENER_PROTOCOLS
    ]
    selected, truncated = _bounded_items(items, max_results)
    return {"items": selected, "count": len(selected), "truncated": truncated}


def _execute_payload(response: Any) -> Mapping[str, Any]:
    if response.Response.Err:
        raise RuntimeError("sliver_implant_error")
    if response.Response.Async:
        raise RuntimeError("sliver_async_result_unavailable")
    stdout = bytes(response.Stdout)
    stderr = bytes(response.Stderr)
    if len(stdout) > MAX_OUTPUT_BYTES or len(stderr) > MAX_OUTPUT_BYTES:
        raise RuntimeError("sliver_output_too_large")
    return {
        "status": int(response.Status),
        "pid": int(response.PID),
        "stdout": stdout.decode("utf-8", errors="replace"),
        "stderr": stderr.decode("utf-8", errors="replace"),
    }


def sliver(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    """Run one bounded, authenticated Sliver RPC call selected by operation."""
    config = _connection_config(arguments)
    operation = config["operation"]
    if operation == "execute":
        response = _call_rpc(config, "/rpcpb.SliverRPC/Execute", "ExecuteReq", "ExecuteResp")
        result: Mapping[str, Any] = {
            "resource": "execute",
            "session_id": config["session_id"],
            "path": config["path"],
            "args": list(config["args"]),
            **_execute_payload(response),
        }
    else:
        method, response_name = _INVENTORY_METHODS[operation]
        response = _call_rpc(config, method, "Empty", response_name)
        if operation == "health":
            result = {"resource": "health", "status": "ok", "version": _version_payload(response)}
        elif operation == "sessions":
            result = {"resource": operation, **_sessions_payload(response, config["max_results"])}
        elif operation == "beacons":
            result = {"resource": operation, **_beacons_payload(response, config["max_results"])}
        else:
            result = {"resource": operation, **_listeners_payload(response, config["max_results"])}
    secrets = [value.decode("utf-8") if isinstance(value, bytes) else value for value in (config["token"], config["ca_certificate"], config["certificate"], config["private_key"])]

    def redact(value: Any) -> Any:
        if isinstance(value, str):
            for secret in secrets:
                value = value.replace(secret, "[REDACTED]")
            return value
        if isinstance(value, dict):
            return {key: redact(item) for key, item in value.items()}
        if isinstance(value, list):
            return [redact(item) for item in value]
        return value

    result = redact(result)
    if len(json.dumps(result, ensure_ascii=False, separators=(",", ":")).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise RuntimeError("sliver_output_too_large")
    return result


def register_sliver_tools(broker: Any) -> None:
    broker.register_adapter(
        name="sliver-c2",
        capabilities=("c2_inventory", "c2_health", "session_inventory", "listener_inventory", "c2_action"),
        adapter=sliver,
        description="Authenticated Sliver RPC inventory plus explicit bounded session command execution.",
        priority=610,
        input_schema=SLIVER_SCHEMA,
        version="sliver-rpc-v2",
        side_effecting=True,
    )


__all__ = [
    "MAX_GRPC_RESPONSE_BYTES",
    "MAX_OUTPUT_BYTES",
    "MAX_RESULTS",
    "SLIVER_SCHEMA",
    "register_sliver_tools",
    "sliver",
]
