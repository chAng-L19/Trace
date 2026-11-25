#!/usr/bin/env python3
"""Server-only Docker acceptance, stdlib only; never builds or changes an image.
python3 docker/p2_runtime_test.py --image trace:p2-test --report /tmp/p2-docker.json
Uses the image's real ENTRYPOINT tini -> entrypoint.sh -> trace run, the Go agent loop,
a loopback scripted provider, and the existing TRACE_MCP_SERVERS stdio contract.
"""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import uuid


def emit_marker(name, value):
    path = Path(os.environ["P2_FIXTURE_DIR"]) / name
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value), encoding="utf-8")
    temporary.replace(path)


def fixture_children():
    label = os.environ["P2_LABEL"]
    emit_marker(label + "-active.json", {"pid": os.getpid(), "ppid": os.getppid()})
    child = subprocess.Popen([sys.executable, __file__, "--fixture", "child"],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL)
    middle = os.fork()
    if middle == 0:
        orphan = os.fork()
        if orphan:
            os._exit(0)
        for fd in (0, 1, 2):
            try:
                os.close(fd)
            except OSError:
                pass
        emit_marker(label + "-orphan.json", {"pid": os.getpid()})
        time.sleep(0.5)
        os._exit(0)
    os.waitpid(middle, 0)
    return child


def fixture(mode):
    if mode == "child":
        emit_marker(os.environ["P2_LABEL"] + "-child.json", {"pid": os.getpid()})
        while True:
            time.sleep(1)
    if mode == "tool":
        fixture_children()
        while True:
            time.sleep(1)
    if mode == "mcp":
        emit_marker("mcp-start.json", {"pid": os.getpid()})
        for line in sys.stdin:
            rpc = json.loads(line)
            if "id" not in rpc:
                continue
            method = rpc["method"]
            if method == "initialize":
                result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                          "serverInfo": {"name": "p2-fixture", "version": "1"}}
            elif method == "tools/list":
                result = {"tools": [{"name": "hold", "description": "P2 local process fixture",
                                    "inputSchema": {"type": "object", "properties": {}}}]}
            elif method == "tools/call":
                fixture_children()
                while True:
                    time.sleep(1)
            elif method == "ping":
                result = {}
            else:
                raise RuntimeError("unexpected MCP method " + method)
            print(json.dumps({"jsonrpc": "2.0", "id": rpc["id"], "result": result}), flush=True)
    if mode == "inspect":
        root = Path(os.environ["P2_FIXTURE_DIR"])
        records = []
        for directory in Path("/proc").iterdir():
            if not directory.name.isdigit():
                continue
            try:
                stat = (directory / "stat").read_text()
                fields = stat[stat.rfind(")") + 2:].split()
                records.append({"pid": int(directory.name), "state": fields[0], "ppid": int(fields[1]),
                                "cmd": (directory / "cmdline").read_bytes().replace(b"\0", b" ").decode(errors="replace")})
            except (FileNotFoundError, PermissionError):
                pass
        print(json.dumps({"processes": records, "markers": {p.name: json.loads(p.read_text()) for p in root.glob("*.json")}}))


def docker(*args, check=True, timeout=30):
    result = subprocess.run(["docker", *args], text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError("docker " + " ".join(args[:3]) + ": " + result.stderr)
    return result


def wait_for(check, timeout=25):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(0.1)
    raise TimeoutError("fixture readiness/cleanup deadline exceeded")


def process_identity(pid):
    try:
        stat = Path("/proc") / str(pid) / "stat"
        text = stat.read_text()
        return text[text.rfind(")") + 2:].split()[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def run(image, report_path):
    if sys.platform != "linux":
        raise RuntimeError("run this acceptance on the designated Linux server")
    config = json.loads(docker("image", "inspect", image).stdout)[0]["Config"]
    expected = ["/usr/bin/tini", "-g", "--", "/opt/trace/docker/entrypoint.sh"]
    if config.get("Entrypoint") != expected or config.get("StopSignal") != "SIGINT":
        raise AssertionError("image does not use the requested real tini/SIGINT entrypoint")
    report = {"status": "running", "image": image, "entrypoint": expected, "cases": [],
              "exec_resource_mode": "process-group",
              "scope": "Real container lifecycle after one SIGINT; namespace teardown may also reap remaining processes. Independent Go graceful-cleanup tests cover cooperative ordering."}
    class Provider(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            case = self.server.fixture_case
            definitions = [row["function"]["name"] for row in body.get("tools", [])]
            if case == "exec":
                name, arguments = "exec", {"command": "P2_LABEL=exec P2_FIXTURE_DIR=/fixture python3 /fixture/p2_runtime_test.py --fixture tool"}
                expected_requests = 1
            else:
                hold_id = "mcp_" + hashlib.sha256(json.dumps(["p2", "hold"], separators=(",", ":")).encode()).hexdigest()[:56]
                stage = self.server.requests
                if stage in (1, 2):
                    call_id = "p2-flow-" + str(stage)
                    replies = [message for message in body.get("messages", [])
                               if message.get("role") == "tool" and message.get("tool_call_id") == call_id]
                    try:
                        result = json.loads(replies[-1]["content"])
                        if result.get("error"):
                            raise ValueError(result["error"])
                        payload = json.loads(result["stdout"])
                        if stage == 1:
                            matches = [tool["id"] for tool in payload["tools"]
                                       if tool.get("server") == "p2" and tool.get("remote_name") == "hold"]
                            if matches != [hold_id]:
                                raise ValueError("MCP discovery did not return the configured hold tool")
                            self.server.hold_id = matches[0]
                        elif payload.get("selected_ids") != [self.server.hold_id]:
                            raise ValueError("MCP selection was not persisted")
                    except (IndexError, KeyError, ValueError, TypeError) as error:
                        self.server.failures.append("tool discovery/selection failed: " + str(error))
                        self.send_error(500, self.server.failures[-1])
                        return
                name, arguments = (("tool_search", {"query": "p2 hold", "offset": 0, "limit": 10})
                                   if stage == 0 else
                                   ("tool_select", {"ids": [self.server.hold_id], "replace": True})
                                   if stage == 1 else (self.server.hold_id, {}))
                expected_requests = 3
            if name not in definitions:
                self.server.failures.append("requested tool absent from real model schemas: " + name)
                self.send_error(500)
                return
            self.server.requests += 1
            if self.server.requests > expected_requests:
                self.server.failures.append("unexpected model call after expected tool flow")
            call = {"id": "p2-flow-" + str(self.server.requests), "type": "function", "function": {"name": name, "arguments": json.dumps(arguments)}}
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            frames = [{"choices": [{"index": 0, "delta": {"role": "assistant", "tool_calls": [{"index": 0, **call}]}, "finish_reason": None}]},
                      {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]}]
            for frame in frames:
                self.wfile.write(("data: " + json.dumps(frame) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        for case in ("exec", "mcp"):
            server.fixture_case, server.requests, server.failures = case, 0, []
            name = "trace-p2-runtime-" + uuid.uuid4().hex[:12]
            with tempfile.TemporaryDirectory(prefix="trace-p2-docker-") as temporary:
                root = Path(temporary).resolve()
                root.chmod(0o777)
                (root / "state").mkdir(mode=0o777)
                (root / "state").chmod(0o777)
                (root / "p2_runtime_test.py").write_text(Path(__file__).read_text(), encoding="utf-8")
                mcp = [{"name": "p2", "command": "python3", "args": ["/fixture/p2_runtime_test.py", "--fixture", "mcp"],
                        "env": {"P2_FIXTURE_DIR": "/fixture", "P2_LABEL": "mcp"}}]
                options = ["--network", "host", "--volume", str(root) + ":/fixture", "--volume", str(root / "state") + ":/state",
                           "--env", "TRACE_PROVIDER_URL=http://127.0.0.1:" + str(server.server_port) + "/v1",
                           "--env", "TRACE_PROVIDER_KEY=local-fixture", "--env", "TRACE_MODEL=p2-fixture",
                           "--env", "TRACE_MCP_SERVERS=" + json.dumps(mcp), "--env", "TRACE_LOCAL_TOOLS=[]",
                           "--env", "TRACE_EXEC_RESOURCE_MODE=process-group", "--env", "TRACE_EXEC_CGROUP_ROOT=",
                           "--env", "TRACE_CONFIG_FILE=/state/nonexistent-config.json", "--env", "TRACE_MAX_SECONDS=60",
                           "--env", "TRACE_MAX_ROUNDS=0", "--env", "TRACE_MAX_ACTIONS=0", "--env", "P2_FIXTURE_DIR=/fixture"]
                created = docker("run", "--rm", *options, image, "create", "P2 process cleanup fixture")
                run_id = json.loads(created.stdout)["id"]
                item = {"case": case, "container": name, "run_id": run_id}
                try:
                    docker("run", "--detach", "--name", name, *options, image, "run", run_id)
                    wait_for(lambda: (root / (case + "-active.json")).exists() and (root / (case + "-child.json")).exists() and (root / (case + "-orphan.json")).exists())
                    orphan = json.loads((root / (case + "-orphan.json")).read_text())["pid"]
                    time.sleep(0.8)
                    def snapshot():
                        return json.loads(docker("exec", name, "python3", "/fixture/p2_runtime_test.py", "--fixture", "inspect").stdout)
                    before = wait_for(lambda: (lambda value: value if all(row["pid"] != orphan for row in value["processes"]) else None)(snapshot()))
                    processes = before["processes"]
                    if any("host.mjs" in row["cmd"] or "/runtime/pi/" in row["cmd"] or ("node" in row["cmd"] and "--input-type=module" in row["cmd"]) for row in processes):
                        raise AssertionError("legacy Node pi host remains in the Go-loop image")
                    if any(row["state"] == "Z" for row in processes):
                        raise AssertionError("zombie remains in live tini container")
                    if not any(row["pid"] == 1 and "tini" in row["cmd"] for row in processes):
                        raise AssertionError("tini is not PID1")
                    if not any("--fixture mcp" in row["cmd"] for row in processes):
                        raise AssertionError("real configured MCP process not alive")
                    host_rows = docker("top", name, "-eo", "pid,ppid,stat").stdout.splitlines()[1:]
                    identities = {int(row.split(None, 1)[0]): process_identity(int(row.split(None, 1)[0])) for row in host_rows}
                    start = time.monotonic()
                    docker("kill", "--signal", "SIGINT", name)
                    state = wait_for(lambda: (lambda value: value if not value["Running"] else None)(json.loads(docker("inspect", name).stdout)[0]["State"]), 20)
                    wait_for(lambda: all(process_identity(pid) != identity for pid, identity in identities.items() if identity is not None), 5)
                    if state.get("OOMKilled") or state.get("ExitCode") == 137:
                        raise AssertionError("container required an unrequested hard kill/OOM")
                    expected_requests = 1 if case == "exec" else 3
                    if server.failures or server.requests != expected_requests:
                        raise AssertionError(str(server.failures) + " provider requests=" + str(server.requests))
                    snapshot_file = root / "state" / (run_id + ".json")
                    saved = json.loads(snapshot_file.read_text())
                    actions = saved.get("actions", [])
                    if not actions or saved.get("result", {}).get("status") == "success":
                        raise AssertionError("cancellation lost action or claimed success")
                    if case == "exec":
                        assert len(actions) == 1, actions
                        action = actions[0]
                        assert action["resource_root"] == "process-group://trace-exec/v1"
                        assert action["resource_root_identity"] and action["resource_owner"]
                        result = action.get("result", {})
                        assert result.get("worker_status") == "cancelled", action
                        assert result.get("worker_resource_owner") == action["resource_owner"], action
                        assert not result.get("worker_cleanup_error"), action
                        item["compat_exec_identity_and_cancel_result_persisted"] = True
                    item.update(status="pass", double_fork_pid=orphan, orphan_reaped_while_container_live=True,
                                live_processes=[{key: row[key] for key in ("pid", "ppid", "state")} for row in processes],
                                host_process_identities=identities, all_original_host_pids_gone=True,
                                seconds_to_exit=time.monotonic() - start, container_exit_code=state["ExitCode"],
                                run_status=saved["run"]["status"], action_statuses=[a["status"] for a in actions],
                                provider_requests=server.requests)
                except BaseException as error:
                    item.update(status="failed", error=str(error))
                    raise
                finally:
                    logs = docker("logs", name, check=False)
                    item["logs"] = (logs.stdout + logs.stderr)[-12000:]
                    report["cases"].append(item)
                    docker("rm", "--force", name, check=False)
                    item["fixture_container_removed"] = docker("inspect", name, check=False).returncode != 0
                    if not item["fixture_container_removed"]:
                        item["status"] = "failed"
                        raise AssertionError("fixture container cleanup failed: " + name)
        report["status"] = "pass"
    except BaseException as error:
        report.update(status="failed", error=str(error))
        raise
    finally:
        server.shutdown()
        server.server_close()
        output = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
        if report_path:
            Path(report_path).resolve().write_text(output, encoding="utf-8")
        print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", choices=("tool", "mcp", "child", "inspect"))
    parser.add_argument("--image")
    parser.add_argument("--report")
    arguments = parser.parse_args()
    if arguments.fixture:
        fixture(arguments.fixture)
    elif not arguments.image:
        parser.error("--image is required; use an already built isolated P2 image")
    else:
        run(arguments.image, arguments.report)

