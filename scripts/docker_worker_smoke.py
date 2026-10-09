"""Docker worker lifecycle check; default offline CLI double, --live for a daemon."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import threading
import time
from contextlib import contextmanager, nullcontext
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from model_recovery_smoke import scenario
from redteam_agent.core import WorkerTask


@contextmanager
def fake_cli(calls, environments=None):
    """Exercise real process/output/store lifecycle without a Docker installation."""
    popen, run = subprocess.Popen, subprocess.run

    def launch(argv, **options):
        if argv[0] != "docker":
            return popen(argv, **options)
        calls.append(list(argv))
        if environments is not None:
            environments.append(("run", dict(options["env"])))
        assert list(argv[1:3]) == ["run", "--rm"]
        assert "--read-only" in argv
        for flag, value in (("--cap-drop", "ALL"), ("--security-opt", "no-new-privileges:true"),
                            ("--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777")):
            assert argv[argv.index(flag) + 1] == value
        assert int(argv[argv.index("--user") + 1].split(":")[0]) != 0
        root = argv[argv.index("--volume") + 1].removesuffix(":/workspace")
        relative = argv[argv.index("--workdir") + 1].removeprefix("/workspace/")
        assert Path(options["cwd"]).resolve() == (Path(root) / relative).resolve()
        environment = dict(options["env"])
        for index, value in enumerate(argv):
            if value == "--env":
                key, text = argv[index + 1].split("=", 1)
                environment[key] = text
        entry = argv.index("--entrypoint")
        assert argv[entry + 2] == "--", "image must not become Docker options"
        command = [sys.executable, *argv[entry + 4:]]
        process = popen(command, **{**options, "env": environment})
        process.args = argv
        return process

    def invoke(argv, **options):
        if argv[0] == "docker":
            calls.append(list(argv))
            if environments is not None:
                environments.append(("rm", dict(options["env"])))
            assert list(argv[1:3]) == ["rm", "--force"]
            return subprocess.CompletedProcess(argv, 0)
        return run(argv, **options)

    with patch("subprocess.Popen", launch), patch("subprocess.run", invoke):
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--image", default="python:3.13-slim")
    args = parser.parse_args()
    calls, checks, environments = [], [], []
    containers_before = set()
    if args.live:
        subprocess.run(["docker", "image", "inspect", args.image], check=True,
                       stdout=subprocess.DEVNULL)
        containers_before = set(subprocess.check_output(
            ["docker", "ps", "-aq", "--filter", "name=trace-worker-"], text=True).split())
    with fake_cli(calls, environments) if not args.live else nullcontext():
        with scenario([]) as (service, _, run_id):
            workspace = service.workspaces.ensure(run_id)

            def task(name, code, timeout=20, **payload):
                return WorkerTask(name, run_id, "docker.command",
                                  {"image": args.image, "argv": ["python", "-c", code], **payload},
                                  name, timeout_seconds=timeout)

            success = task("success", "import os,pathlib; print(os.environ['TRACE_FIXTURE']); "
                           "pathlib.Path('proof.txt').write_text('container-evidence')", cwd="nested",
                           env={"TRACE_FIXTURE": "docker-env-ok"})
            result = service.execute_worker(success)
            assert result.status == "completed", result
            assert result.metadata["worker_kind"] == "docker"
            assert (workspace.path / "nested/proof.txt").read_text() == "container-evidence"
            assert service.read_artifact(run_id, result.artifact_refs[0]).strip() == b"docker-env-ok"
            before = len(calls)
            assert service.execute_worker(success) == result
            assert len(calls) == before
            assert service.workers.reconcile(success.idempotency_key) == result
            checks.extend(["workspace_env_and_evidence", "idempotent_replay_and_reconcile"])
            if not args.live:
                option_image = service.execute_worker(task("image-option", "print('bounded')", image="--privileged"))
                assert option_image.status == "completed", option_image
                command = next(call for call in calls if "--privileged" in call)
                assert command[command.index("--privileged") - 1] == "--"
                checks.append("image_cannot_inject_docker_options")

            invalid = service.execute_worker(task("missing-image", "print('never')", image=""))
            assert invalid.status == "failed" and "docker_worker_image_required" in invalid.error
            checks.append("missing_image_is_recorded_failure")

            for name, payload in (("unlimited-pids", {"pids_limit": -1}),
                                  ("unlimited-memory", {"memory_mb": 0}),
                                  ("unlimited-cpu", {"cpus": 0}),
                                  ("excess-cpu", {"cpus": 1e9}),
                                  ("excess-memory", {"memory_mb": 1e9}),
                                  ("excess-pids", {"pids_limit": 1e9}),
                                  ("fractional-memory", {"memory_mb": 1.5}),
                                  ("huge-memory", {"memory_mb": 10**400}),
                                  ("null-cpu", {"cpus": None}),
                                  ("boolean-pids", {"pids_limit": True}),
                                  ("host-network", {"network": "host"})):
                rejected = service.execute_worker(task(name, "print('never')", **payload))
                assert rejected.status == "failed" and "docker_worker_" in rejected.error, rejected
            checks.append("isolation_parameters_reject_disabled_limits_and_host_network")

            policy = """import os,pathlib,json
assert os.getuid() != 0
status = pathlib.Path('/proc/self/status').read_text().splitlines()
assert next(line for line in status if line.startswith('CapEff:')).split()[1] == '0000000000000000'
assert next(line for line in status if line.startswith('NoNewPrivs:')).split()[1] == '1'
try:
    pathlib.Path('/etc/trace-worker-write').write_text('must-fail')
except OSError:
    pass
else:
    raise AssertionError('rootfs writable')
pathlib.Path('/tmp/worker-write').write_text('tmpfs')
pathlib.Path('isolated-proof').write_text('workspace')
assert set(os.listdir('/sys/class/net')) == {'lo'}
print('isolated')
"""
            if args.live:
                isolated = service.execute_worker(task("isolation", policy))
                assert isolated.status == "completed", isolated
                checks.append("live_nonroot_capabilities_readonly_tmpfs_network")
            else:
                before = len(calls)
                configured = service.execute_worker(task("limits", "print('limits')", cpus=.5,
                                                        memory_mb=256, pids_limit=32, network="bridge"))
                assert configured.status == "completed", configured
                command = next(call for call in calls[before:] if call[1] == "run")
                for flag, value in (("--cpus", "0.5"), ("--memory", "256m"), ("--memory-swap", "256m"),
                                    ("--pids-limit", "32"), ("--network", "bridge")):
                    assert command[command.index(flag) + 1] == value
                checks.append("explicit_bounded_resource_and_network_policy")
                with patch.dict(os.environ, {"TRACE_DOCKER_MAX_CPUS": "2", "TRACE_DOCKER_MAX_MEMORY_MB": "768",
                                             "TRACE_DOCKER_MAX_PIDS": "256"}):
                    before = len(calls)
                    configured = service.execute_worker(task("host-limits", "print('host-defaults')"))
                    assert configured.status == "completed", configured
                    command = next(call for call in calls[before:] if call[1] == "run")
                    for flag, value in (("--cpus", "2.0"), ("--memory", "768m"), ("--pids-limit", "256")):
                        assert command[command.index(flag) + 1] == value
                    over = service.execute_worker(task("over-host", "print('never')", cpus=2.1))
                    assert over.status == "failed" and "resource_limit_exceeded" in over.error
                with patch.dict(os.environ, {"TRACE_DOCKER_MAX_CPUS": "nan"}):
                    invalid_host = service.execute_worker(task("invalid-host-limit", "print('never')"))
                    assert invalid_host.status == "failed" and "resource_limit_exceeded" in invalid_host.error
                checks.append("deployment_caps_bound_task_limits_and_reject_nonfinite")

            failed = service.execute_worker(task("exit", "import sys; print('failure-evidence'); "
                                                "print('stderr-proof',file=sys.stderr); sys.exit(7)"))
            assert failed.status == "failed" and failed.output["return_code"] == 7, failed
            assert b"stderr-proof" in service.read_artifact(run_id, failed.artifact_refs[1])
            checks.append("nonzero_exit_preserves_evidence")

            timed = service.execute_worker(task("timeout", "import time; print('before-timeout',flush=True); "
                                               "time.sleep(60)", timeout=2))
            assert timed.status == "timed_out", timed
            assert b"before-timeout" in service.read_artifact(run_id, timed.artifact_refs[0])
            checks.append("timeout_preserves_evidence")

            for name in ("cancel", "restart"):
                outputs, errors = [], []
                sleeper = task(name, "import pathlib,time; print('before-cancel',flush=True); "
                               f"pathlib.Path('{name}.ready').write_text('ready'); time.sleep(60)")

                def execute():
                    try:
                        outputs.append(service.execute_worker(sleeper))
                    except BaseException as exc:
                        errors.append(exc)

                thread = threading.Thread(target=execute)
                thread.start()
                deadline = time.monotonic() + 15
                while not (workspace.path / f"{name}.ready").exists() and thread.is_alive():
                    assert time.monotonic() < deadline, "container_start_deadline"
                    time.sleep(.05)
                assert not errors, errors
                if name == "cancel":
                    assert service.workers.cancel(name, run_id)
                else:
                    assert service.workers.restart("docker")
                thread.join(15)
                assert not thread.is_alive() and not errors, errors
                assert outputs and outputs[0].status == "cancelled", outputs
                assert b"before-cancel" in service.read_artifact(run_id, outputs[0].artifact_refs[0])
            checks.extend(["cancel_stops_container", "restart_closes_active_worker"])

            restarted = service.execute_worker(task("restarted", "print('restarted')"))
            assert restarted.status == "completed", restarted
            checks.append("lazy_worker_restart")

            interrupted = task("interrupted", "print('must-not-replay')")
            service.worker_records.prepare(interrupted, worker_kind="docker", owner="lost-process")
            service.worker_records.transition("interrupted", expected_statuses=("prepared",),
                                              status="running", owner="lost-process")
            before = len([call for call in calls if call[1] == "run"])
            unknown = service.execute_worker(interrupted)
            assert unknown.status == "unknown", unknown
            assert len([call for call in calls if call[1] == "run"]) == before
            assert service.workers.reconcile("interrupted") is None
            checks.append("interrupted_execution_requires_reconcile")

            if not args.live:
                from redteam_agent.workers.docker import DockerWorkerAdapter
                finish = DockerWorkerAdapter._finish
                def mutate_after_run(worker, *values):
                    os.environ["DOCKER_HOST"] = "tcp://changed-after-run:2376"
                    return finish(worker, *values)
                for index in range(2):
                    host = {"DOCKER_HOST": f"tcp://fixture-{index}:2376", "DOCKER_CONTEXT": f"context-{index}",
                            "DOCKER_CONFIG": f"config-{index}", "DOCKER_TLS_VERIFY": "1", "DOCKER_CERT_PATH": "tls"}
                    before = len(environments)
                    with patch.dict(os.environ, host), patch.object(DockerWorkerAdapter, "_finish", mutate_after_run):
                        configured = service.execute_worker(task(f"daemon-{index}",
                            "import os; assert os.environ['DOCKER_HOST']=='container-only'; print('bound')",
                            env={"DOCKER_HOST": "container-only"}))
                    assert configured.status == "completed", configured
                    captured = environments[before:]
                    assert [item[0] for item in captured] == ["run", "rm"], captured
                    assert captured[0][1] == captured[1][1]
                    assert all(captured[0][1].get(key) == value for key, value in host.items())
                checks.append("daemon_environment_snapshot_separate_from_container_env")
                with patch("subprocess.Popen", side_effect=FileNotFoundError("docker missing")):
                    missing = service.execute_worker(task("missing-cli", "print('never')"))
                assert missing.status == "failed" and "FileNotFoundError" in missing.error
                started = {call[call.index("--name") + 1] for call in calls if call[1] == "run"}
                removed = {call[-1] for call in calls if call[1] == "rm"}
                assert started <= removed
                checks.extend(["missing_cli_real_failure", "container_cleanup"])
    if args.live:
        containers_after = set(subprocess.check_output(
            ["docker", "ps", "-aq", "--filter", "name=trace-worker-"], text=True).split())
        assert not containers_after - containers_before, "worker_container_leaked"
        checks.append("daemon_confirms_no_container_leaks")
    print(json.dumps({"mode": "live" if args.live else "offline", "checks": checks}, indent=2))


if __name__ == "__main__":
    main()
