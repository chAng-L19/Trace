#!/usr/bin/env python3
"""Server-only default benchmark smoke; synthetic API/provider, not a platform run."""
import argparse
import hashlib
import http.server
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import uuid


def docker(*args, check=True):
    p = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=45)
    if check and p.returncode:
        raise RuntimeError(p.stderr[-2000:])
    return p


def main(image, report_path, source=None, binary=None):
    if sys.platform != "linux":
        raise RuntimeError("run this acceptance on the designated Linux server")
    ready, release = threading.Event(), threading.Event()
    calls, failures = [], []
    token = "synthetic-benchmark-token"
    challenge = "hosted_contract_fixture"
    # Exercise the delivered helper through the real Go agent loop/exec in both final-image
    # and readonly-overlay modes; inline HTTP calls would miss packaging gaps.
    code = (
        "import subprocess,sys; helper='/opt/trace/examples/tsecbench_api.py'; "
        "steps=[['list'],['start','--unique-code','hosted_contract_fixture'],"
        "['submit','--unique-code','hosted_contract_fixture','--flag','flag{fixture}'],"
        "['close','--unique-code','hosted_contract_fixture']]; "
        "[subprocess.run([sys.executable,helper,*step],check=True) for step in steps]"
    )
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def send_json(self, value):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(value).encode())

        def do_GET(self):
            if self.path in ("/health", "/health-followed"):
                self.server.health_calls += 1
                status = self.server.health_status if self.path == "/health" else 500
                self.send_response(status)
                if status == 302:
                    self.send_header("Location", "/health-followed")
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(self.server.health_body.encode())
                return
            self.api()

        def do_POST(self):
            if self.path == "/v1/chat/completions":
                return self.provider()
            self.api()

        def api(self):
            if self.headers.get("BENCHMARK_TOKEN") != token:
                failures.append("missing benchmark header")
                self.send_error(403)
                return
            calls.append([self.command, self.path])
            if self.command == "GET" and self.path == "/openapi/v1/challenges":
                self.send_json([{"unique_code": challenge, "flag_count": 1, "total_score": 1,
                                 "correct_flag_count": 0, "is_completed": False,
                                 "container_status": "stopped", "container_addr": []}])
            elif self.path == "/openapi/v1/challenges/start?unique_code=" + challenge:
                self.send_json({"unique_code": challenge, "container_addr": ["127.0.0.1:1"]})
            elif self.path == "/openapi/v1/challenges/submit":
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if body != {"unique_code": challenge, "flag": "flag{fixture}"}:
                    failures.append("incorrect submit contract")
                self.send_json({"correct": True, "awarded": 1, "cumulative_score": 1,
                                "correct_flag_count": 1, "total_flag_count": 1, "matched_flag_index": 0})
            elif self.path == "/openapi/v1/challenges/close?unique_code=" + challenge:
                self.send_json({"unique_code": challenge, "closed": True})
            else:
                failures.append("unexpected API path")
                self.send_error(404)

        def provider(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if self.headers.get("Authorization") != "Bearer synthetic-provider-key":
                failures.append("missing provider header")
            self.server.requests += 1
            if self.server.requests > 1:
                replies = [m for m in body.get("messages", []) if m.get("role") == "tool"]
                if not replies or 'cumulative_score' not in str(replies[-1].get("content")):
                    failures.append("real tool result not returned to model")
                ready.set()
                release.wait(45)
                return
            names = [t["function"]["name"] for t in body.get("tools", [])]
            if "exec" not in names:
                failures.append("exec absent from real tool schemas")
                self.send_error(500)
                return
            args = {"command": "python3 -c " + __import__("shlex").quote(code)}
            call = {"index": 0, "id": "hosted-fixture-call", "type": "function",
                    "function": {"name": "exec", "arguments": json.dumps(args)}}
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            frames = [{"choices": [{"index": 0, "delta": {"role": "assistant",
                       "tool_calls": [call]}, "finish_reason": None}]},
                      {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]}]
            for frame in frames:
                self.wfile.write(("data: " + json.dumps(frame) + chr(10) * 2).encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.requests = 0
    server.health_calls = 0
    threading.Thread(target=server.serve_forever, daemon=True).start()
    name = "trace-hosted-smoke-" + uuid.uuid4().hex[:12]
    report = {"status": "running", "image": image, "container": name,
              "scope": "Real default ENTRYPOINT/CMD, agent-loop/tool/persistence; synthetic provider/API and VPN response. Not official platform/VPN/gateway acceptance."}
    try:
        with tempfile.TemporaryDirectory(prefix="trace-hosted-smoke-") as tmp:
            root = Path(tmp)
            root.chmod(0o777)
            (root / "bin").mkdir()
            (root / "state").mkdir(mode=0o777)
            (root / "state").chmod(0o777)
            curl = root / "bin/curl"
            curl.write_text(r"""#!/bin/bash
set -euo pipefail
printf 'call\n' >> /fixture/health-calls
[[ $# == 6 && "$1" == get && "$2" == http://10.0.100.58 && "$3" == --body && "$4" == --timeout && "$5" == 10 && "$6" == --no-follow ]] || exit 91
case "${TRACE_FIXTURE_HEALTH:-ok}" in
  ok) printf '%s\n' '{"status":"ok"}' ;;
  unhealthy) printf '%s\n' '{"status":"failed"}' ;;
  invalid-json) printf '%s\n' 'invalid' ;;
  empty-body) ;;
  http-error) printf '%s\n' '{"status":"ok"}'; exit 1 ;;
  transport-error) exit 1 ;;
  *) exit 92 ;;
esac
""")
            curl.chmod(0o755)
            config = json.loads(docker("image", "inspect", image).stdout)[0]["Config"]
            assert config["Cmd"] == ["benchmark"] and config["User"] == "trace"
            assert config["Entrypoint"] == ["/usr/bin/tini", "-g", "--", "/opt/trace/docker/entrypoint.sh"]
            assert config["StopSignal"] == "SIGINT"
            image_env = dict(value.split("=", 1) for value in config["Env"])
            if not source:
                assert image_env.get("TRACE_EXEC_RESOURCE_MODE") == "process-group"
                assert not image_env.get("TRACE_EXEC_CGROUP_ROOT")
            path = next(e[5:] for e in config["Env"] if e.startswith("PATH="))
            base = "http://127.0.0.1:" + str(server.server_port)
            # Exercise the shipped CLI, bypassing the private-URL fixture below.
            # A 503 with an ok body must fail; a 302 must not be followed.
            health_command = (
                'set -o pipefail; status=$(/usr/local/bin/curl get "$1" --body --timeout 10 --no-follow | '
                'python3 -c \'import json,sys; print(json.load(sys.stdin).get("status", ""))\') || exit 1; '
                '[[ "$status" == ok ]]'
            )
            for status, body, expected_exit in (
                (200, '{"status":"ok"}', 0),
                (503, '{"status":"ok"}', 1),
                (200, '{"status":"failed"}', 1),
                (200, 'invalid', 1),
                (302, '{"status":"ok"}', 0),
            ):
                server.health_status, server.health_body = status, body
                count = server.health_calls
                probe = docker("run", "--rm", "--network", "host", "--entrypoint", "/bin/bash",
                               image, "-c", health_command, "health-check", base + "/health", check=False)
                assert probe.returncode == expected_exit, (status, body, probe.stdout, probe.stderr)
                assert server.health_calls == count + 1
                assert not calls and not server.requests
            report["frozen_cli_health_contract"] = True
            options = ["--network", "host", "--volume", str(root) + ":/fixture",
                       "--volume", str(root / "state") + ":/state",
                       "--env", "PATH=/fixture/bin:" + path,
                       "--env", "BENCHMARK_BASE_URL=" + base,
                       "--env", "BENCHMARK_TOKEN=" + token,
                       "--env", "TRACE_PROVIDER_URL=" + base + "/v1",
                       "--env", "TRACE_PROVIDER_KEY=synthetic-provider-key",
                       "--env", "TRACE_MODEL=hosted-fixture",
                       "--env", "TRACE_LOCAL_TOOLS=[]", "--env", "TRACE_MCP_SERVERS=[]",
                       "--env", "TRACE_MAX_SECONDS=90"]
            overlay = []
            if source:
                source_path = Path(source).resolve()
                for relative in (
                    "prompts/agent.md", "prompts/coordinator.md", "prompts/worker.md", "prompts/recon.md",
                    "examples/tsecbench-task.md", "examples/tsecbench_api.py", "docker/entrypoint.sh",
                ):
                    path = source_path / relative
                    assert path.is_file()
                    overlay += ["--volume", str(path) + ":/opt/trace/" + relative + ":ro"]
                assert binary and Path(binary).is_file()
                overlay += ["--volume", str(Path(binary).resolve()) + ":/usr/local/bin/trace:ro"]
                # Only overlays need to supply the new image metadata. Final
                # image acceptance must use the shipped default unchanged.
                overlay += ["--env", "TRACE_EXEC_RESOURCE_MODE=process-group"]
                options += overlay
                report["source_overlay"] = str(source_path)
                report["binary_sha256"] = hashlib.sha256(Path(binary).read_bytes()).hexdigest()
                report["scope"] += " Current binary/prompt/helper readonly overlays, not a rebuilt delivery image."
            missing = docker("run", "--rm", "--network", "none", *overlay, image, check=False)
            assert missing.returncode and "BENCHMARK_BASE_URL" in missing.stderr
            for mode, expected_error in (("typo", "unknown TRACE_EXEC_RESOURCE_MODE"),
                                         ("cgroup", "TRACE_EXEC_CGROUP_ROOT must name")):
                rejected = docker("run", "--rm", *options, "--env", "TRACE_EXEC_RESOURCE_MODE=" + mode,
                                  "--env", "TRACE_EXEC_CGROUP_ROOT=", image, check=False)
                assert rejected.returncode == 2 and expected_error in rejected.stderr
                assert not (root / "state/service/hosted-run.json").exists()
                assert not calls and not server.requests
            health_log = root / "health-calls"
            assert not health_log.exists()
            for count, health in enumerate(("unhealthy", "invalid-json", "empty-body", "http-error", "transport-error"), 1):
                unhealthy = docker("run", "--rm", *options, "--env", "TRACE_FIXTURE_HEALTH=" + health,
                                   image, check=False)
                assert unhealthy.returncode == 1 and "VPN health check failed" in unhealthy.stderr
                assert len(health_log.read_text().splitlines()) == count
                assert not (root / "state/service/hosted-run.json").exists()
                assert not calls and not server.requests
            report.update(missing_environment_fails_before_run=True,
                          failed_healthcheck_fails_before_run=True,
                          strict_health_argv=True, single_health_request_per_attempt=True,
                          invalid_mode_fails_before_run=True,
                          explicit_cgroup_requires_delegation=True)
            docker("run", "--detach", "--name", name, *options, image)
            if not ready.wait(40):
                raise TimeoutError("default benchmark did not complete synthetic API/tool chain")
            expected = [["GET", "/openapi/v1/challenges"],
                        ["POST", "/openapi/v1/challenges/start?unique_code=" + challenge],
                        ["POST", "/openapi/v1/challenges/submit"],
                        ["POST", "/openapi/v1/challenges/close?unique_code=" + challenge]]
            assert calls == expected and not failures, (calls, failures)
            assert len(health_log.read_text().splitlines()) == 6
            sentinel = json.loads((root / "state/service/hosted-run.json").read_text())
            saved = json.loads((root / "state" / (sentinel["id"] + ".json")).read_text())
            actions = saved.get("actions", [])
            assert actions and any("cumulative_score" in json.dumps(a) for a in actions)
            executed = [action for action in actions if action.get("call_id") == "hosted-fixture-call"]
            assert len(executed) == 1, actions
            action = executed[0]
            assert action["resource_root"] == "process-group://trace-exec/v1"
            assert action["resource_root_identity"] and action["resource_owner"]
            result = action["result"]
            assert result["worker_status"] == "completed" and result["exit_code"] == 0, result
            assert result["worker_resource_owner"] == action["resource_owner"]
            assert not result.get("worker_cleanup_error") and not result.get("outcome_uncertain"), result
            container = json.loads(docker("inspect", name).stdout)[0]
            assert not container["HostConfig"]["Privileged"]
            assert not container["HostConfig"].get("CapAdd")
            assert not any(mount["Destination"].startswith("/sys/fs/cgroup") for mount in container["Mounts"])
            report.update(api_calls=calls, model_requests=server.requests, run_id=sentinel["id"],
                          action_count=len(actions), default_benchmark_autostart=True,
                          packaged_helper="/opt/trace/examples/tsecbench_api.py",
                          actual_entrypoint=config["Entrypoint"], actual_cmd=config["Cmd"],
                          resource_mode="process-group", resource_root=action["resource_root"],
                          resource_root_identity=action["resource_root_identity"],
                          ordinary_container_without_cgroup_delegation=True,
                          persisted_result_sha256=hashlib.sha256(json.dumps(actions).encode()).hexdigest())
            docker("kill", "--signal", "SIGINT", name)
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                state = json.loads(docker("inspect", name).stdout)[0]["State"]
                if not state["Running"]:
                    break
                time.sleep(0.2)
            assert not state["Running"] and not state["OOMKilled"]
            assert state["ExitCode"] != 137
            report.update(status="pass", sigint_exit_code=state["ExitCode"])
    except BaseException as exc:
        report.update(status="failed", error=str(exc), api_calls=calls, failures=failures)
        raise
    finally:
        logs = docker("logs", name, check=False)
        report["logs"] = (logs.stdout + logs.stderr)[-5000:]
        docker("rm", "--force", name, check=False)
        report["fixture_container_removed"] = docker("inspect", name, check=False).returncode != 0
        if not report["fixture_container_removed"]:
            report.update(status="failed", error="fixture container cleanup failed")
        release.set()
        server.shutdown()
        server.server_close()
        Path(report_path).write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(json.dumps({k: v for k, v in report.items() if k != "logs"}))
        assert report["fixture_container_removed"]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--report", required=True)
    parser.add_argument("--source")
    parser.add_argument("--binary")
    args = parser.parse_args()
    main(args.image, args.report, args.source, args.binary)
