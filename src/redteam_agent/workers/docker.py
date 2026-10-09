from __future__ import annotations

import subprocess
import hashlib
import os
import math
from pathlib import Path

from ..core import WorkerResult, WorkerTask
from ..runtime.artifact_store import ArtifactStore
from ..runtime.worker_store import WorkerStore
from .local import LocalWorker
from .workspace import RunWorkspace, WorkspaceManager


class DockerWorkerAdapter(LocalWorker):
    """Run task argv in payload.image, sharing the run workspace at /workspace."""

    kind = "docker"

    def __init__(self, *, records: WorkerStore,
                 workspaces: WorkspaceManager | None = None,
                 artifacts: ArtifactStore | None = None) -> None:
        super().__init__(
            records=records,
            workspaces=workspaces or WorkspaceManager(records.store.root, records.store),
            artifacts=artifacts or ArtifactStore(records.store.root / "artifact-store", records.store),
            owner="docker-worker",
        )
        self._containers: dict[str, str] = {}
        self._daemon_environments: dict[str, dict[str, str]] = {}

    def capabilities(self) -> tuple[str, ...]:
        return ("docker.command", "docker.process")

    def _host_environment(self) -> dict[str, str]:
        environment = self.workspaces.environment()
        environment.update({key: value for key, value in os.environ.items()
                            if key.startswith("DOCKER_") or key in {"SSH_AUTH_SOCK", "XDG_RUNTIME_DIR"}})
        return environment

    def _environment(self, task: WorkerTask, environment: dict[str, str]) -> dict[str, str]:
        # Task env belongs inside the container, never to the host Docker CLI.
        host = self._host_environment()
        with self._lock:
            self._daemon_environments[self._container_name(task.task_id)] = host
        return host

    def _command(self, task: WorkerTask, workspace: RunWorkspace, cwd: Path) -> list[str]:
        image = str(task.payload.get("image") or "").strip()
        if not image:
            raise ValueError("docker_worker_image_required")
        uid, gid = (os.getuid(), os.getgid()) if os.name != "nt" else (10001, 10001)
        if uid == 0:
            raise ValueError("docker_worker_requires_non_root_host_for_workspace_ownership")
        limits = {}
        for field, setting, default in (("cpus", "CPUS", 1), ("memory_mb", "MEMORY_MB", 512),
                                        ("pids_limit", "PIDS", 128)):
            maximum = float(os.environ.get(f"TRACE_DOCKER_MAX_{setting}", default))
            value = task.payload.get(field, maximum)
            if isinstance(value, bool) or not isinstance(value, (int, float, str)):
                raise ValueError(f"docker_worker_resource_limit_invalid:{field}")
            try:
                requested = float(value)
            except (ValueError, OverflowError):
                raise ValueError(f"docker_worker_resource_limit_invalid:{field}") from None
            if (not math.isfinite(maximum) or maximum <= 0 or not math.isfinite(requested)
                    or requested <= 0 or requested > maximum):
                raise ValueError(f"docker_worker_resource_limit_exceeded:{field}")
            if field != "cpus" and (not maximum.is_integer() or not requested.is_integer()):
                raise ValueError(f"docker_worker_resource_limit_requires_integer:{field}")
            limits[field] = requested if field == "cpus" else int(requested)
        network = str(task.payload.get("network", "none"))
        if network not in {"none", "bridge"}:
            raise ValueError("docker_worker_network_requires_none_or_bridge")
        name = self._container_name(task.task_id)
        with self._lock:
            self._containers[task.task_id] = name
        argv = ["docker", "run", "--rm", "--name", name,
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
                "--user", f"{uid}:{gid}", "--cpus", str(limits["cpus"]),
                "--memory", f"{limits['memory_mb']}m", "--memory-swap", f"{limits['memory_mb']}m",
                "--pids-limit", str(limits["pids_limit"]), "--network", network,
                "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777",
                "--volume", f"{workspace.path.resolve()}:/workspace",
                "--workdir", "/workspace/" + cwd.relative_to(workspace.path).as_posix()]
        for key, value in (task.payload.get("env") or {}).items():
            argv.extend(("--env", f"{key}={value}"))
        argv.extend(("--entrypoint", task.payload["argv"][0], "--", image, *task.payload["argv"][1:]))
        return argv

    def _execute(self, task: WorkerTask) -> WorkerResult:
        result = None
        try:
            result = super()._execute(task)
            return result
        finally:
            with self._lock:
                name = self._containers.pop(task.task_id, None)
                environment = self._daemon_environments.pop(self._container_name(task.task_id), None)
            if name is None and result is not None and result.status == "unknown":
                name = self._container_name(task.task_id)
            if name is not None:
                self._remove_container(name, environment if environment is not None else self._host_environment())

    def _container_name(self, task_id: str) -> str:
        identity = f"{self.workspaces.root.resolve()}\0{task_id}"
        return "trace-worker-" + hashlib.sha256(identity.encode()).hexdigest()[:32]

    @staticmethod
    def _remove_container(name: str, environment: dict[str, str]) -> None:
        try:
            subprocess.run(("docker", "rm", "--force", name), check=False,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, env=environment)
        except (OSError, subprocess.TimeoutExpired):
            pass

    def _terminate_tree(self, process: subprocess.Popen[bytes]) -> None:
        # Killing the attached CLI alone leaves the container running on the daemon.
        argv = process.args
        name = argv[argv.index("--name") + 1]
        with self._lock:
            environment = self._daemon_environments.get(name)
        super()._terminate_tree(process)
        self._remove_container(name, environment if environment is not None else self._host_environment())

    def cancel(self, task_id: str) -> bool:
        record = self.records.get(task_id)
        if record is None or record.worker_kind != self.kind:
            return False
        return super().cancel(task_id)
