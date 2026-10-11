"""Build a host-matched, offline Trace release; also verify its installed payload."""
from __future__ import annotations

import argparse
import importlib
import importlib.metadata
import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import venv
import zipfile
from pathlib import Path
from unittest.mock import patch


PROJECT = Path(__file__).resolve().parents[1]
CLOUD_PACKAGES = {
    "aws": "awscli", "azure": "azure-cli", "tencent": "tccli",
    "huawei": "huaweicloudsdkecs", "volcengine": "volcengine-python-sdk",
    "baidu": "bce-python-sdk", "jdcloud": "jdcloud-sdk",
}
CLOUDS = (*CLOUD_PACKAGES, "gcp", "aliyun")
IMPORTS = {
    "aws": "awscli.clidriver", "azure": "azure.cli.core", "tencent": "tccli",
    "huawei": "huaweicloudsdkecs.v2", "volcengine": "volcenginesdkecs",
    "baidu": "baidubce.services.bcc.bcc_client",
    "jdcloud": "jdcloud_sdk.services.vm.client.VmClient",
}
CLI_COMMANDS = {"aws": "aws", "azure": "az", "tencent": "tccli"}
SDK_PROVIDERS = {"huawei", "volcengine", "baidu", "jdcloud"}


def target() -> dict:
    from sysconfig import get_platform

    if sys.platform != "linux" or platform.machine().lower() not in {"x86_64", "amd64"}:
        raise RuntimeError("Build on Linux x86_64 (the portable manifest's Linux target)")
    if not (3, 11) <= sys.version_info[:2] <= (3, 13):
        raise RuntimeError("Python 3.11-3.13 required")
    return {"platform": get_platform(), "python": list(sys.version_info[:2]),
            "implementation": sys.implementation.name, "libc": list(platform.libc_ver())}


def selection(profile: str, cloud: str | None, manifest: dict) -> dict:
    cloud = cloud if cloud is not None else ("all" if profile == "all" else "none")
    clouds = list(CLOUDS) if cloud == "all" else [] if cloud == "none" else list(dict.fromkeys(cloud.split(",")))
    if unknown := set(clouds) - set(CLOUDS):
        raise ValueError(f"Unknown cloud providers: {sorted(unknown)}")
    packages = manifest["python_packages"] if profile == "all" else (
        {"playwright": manifest["python_packages"]["playwright"]} if profile == "browser" else {})
    portable = (["chromium", "rizin"] if profile == "all" else ["chromium"] if profile == "browser" else [])
    portable += [name for name in ("gcp", "aliyun") if name in clouds]
    return {"profile": profile, "clouds": clouds,
            "full_catalog": profile == "all" and set(clouds) == set(CLOUDS),
            "python_packages": packages, "portable": portable,
            "requirements": [*(f"{name}=={version}" for name, version in packages.items()),
                             *(CLOUD_PACKAGES[name] for name in clouds if name in CLOUD_PACKAGES)],
            "sliver": profile == "all"}


def run(command: list[str], *, cwd: Path | None = None) -> None:
    # Build environments must not inherit a user's live pip target or source tree.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith("PIP_") and key not in {"PYTHONPATH", "PYTHONHOME"}}
    env.update(PIP_CONFIG_FILE=os.devnull, PIP_DISABLE_PIP_VERSION_CHECK="1",
               PYTHONDONTWRITEBYTECODE="1", PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD="1")
    subprocess.run(command, cwd=cwd, env=env, check=True)


def write_json(path: Path, value: dict) -> None:
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def verify(root: Path, *, probes: bool = True) -> None:
    from redteam_agent.runtime import managed_tools, tool_setup

    info = json.loads((root / "bundle.json").read_text(encoding="utf-8"))
    current = target()
    for key in ("platform", "python", "implementation"):
        if current[key] != info["target"][key]:
            raise RuntimeError(f"bundle_target_mismatch:{key}")
    # Import the actual engines, not diagnostic fallbacks that report partial success.
    for name, version in info["python_packages"].items():
        if importlib.metadata.version(name) != version:
            raise RuntimeError(f"package_version_mismatch:{name}")
        importlib.import_module(name)
        if name == "playwright":
            importlib.import_module("playwright.sync_api")
            importlib.import_module("playwright.async_api")
    for name in info["clouds"]:
        if name in IMPORTS:
            importlib.import_module(IMPORTS[name])
        if name in CLI_COMMANDS and not shutil.which(CLI_COMMANDS[name], path=str(Path(sys.executable).parent)):
            raise RuntimeError(f"cloud_cli_missing:{name}")
    if info["sliver"]:
        importlib.import_module("grpc")
        importlib.import_module("google.protobuf.descriptor_pb2")
    for name in info["portable"]:
        item = managed_tools.managed_install(name, root / "tools", verify=True)
        if not item["installed"] or item["source"] != "managed":
            raise RuntimeError(f"portable_missing_or_invalid:{name}:{item['checksum_status']}")
        if probes:
            tool_setup.validate(name, Path(item["path"]), item["version"], time.monotonic() + 60)
    # The existing wire smoke patches all four SDK transports, including baidubce.
    if SDK_PROVIDERS <= set(info["clouds"]):
        run([sys.executable, str(root / "cloud_sdk_smoke.py")], cwd=root)


def prepare(root: Path, timeout: float) -> None:
    from redteam_agent.runtime import tool_setup

    info = json.loads((root / "bundle.json").read_text(encoding="utf-8"))
    # setup normally accepts a host installation. A release needs its own files.
    with patch.object(tool_setup, "resolve_executable", return_value=""), \
            patch.object(tool_setup, "chromium_executable", return_value=""):
        result = tool_setup.setup(info["portable"], root=root / "tools", timeout=timeout,
                                  progress=lambda message: print(message, flush=True))
    if not result["success"]:
        raise RuntimeError("portable_setup_failed:" + json.dumps(result["tools"]))
    verify(root, probes=False)  # setup has already launched each executable.


def build(output: Path, wheel: Path | None, profile: str, cloud: str | None, timeout: float) -> Path:
    host = target()
    output = output.resolve()
    if output.exists():
        raise FileExistsError(f"Output already exists: {output}")
    if not output.name.endswith(".tar.gz"):
        raise ValueError("Output must end in .tar.gz")
    output.parent.mkdir(parents=True, exist_ok=True)
    # Only this fresh staging tree is archived; no source/state/credentials/cache.
    with tempfile.TemporaryDirectory(prefix="trace-bundle-") as temporary:
        work = Path(temporary)
        root = work / "trace-tool-bundle"
        wheels = root / "wheels"
        wheels.mkdir(parents=True)
        if wheel is None:
            project_wheels = work / "project-wheel"
            run([sys.executable, "-m", "pip", "wheel", "--index-url", "https://pypi.org/simple",
                 "--no-deps", "--wheel-dir", str(project_wheels), str(PROJECT)], cwd=work)
            wheel, = project_wheels.glob("trace_agent-*.whl")
        wheel = wheel.resolve()
        with zipfile.ZipFile(wheel) as archive:
            manifest = json.loads(archive.read("redteam_agent/tool_manifest.json"))
        info = selection(profile, cloud, manifest)
        info.update(target=host, project_wheel=wheel.name)
        project_requirement = str(wheel) + ("[sliver]" if info["sliver"] else "")
        run([sys.executable, "-m", "pip", "wheel", "--index-url", "https://pypi.org/simple",
             "--wheel-dir", str(wheels), project_requirement, *info["requirements"]], cwd=work)
        # pip wheel resolves/builds the whole dependency closure, including sdist-only SDKs.
        shutil.copy2(wheel, wheels / wheel.name)
        info["wheels"] = sorted(path.name for path in wheels.glob("*.whl"))
        write_json(root / "bundle.json", info)
        shutil.copy2(__file__, root / "bundle.py")
        for source, destination in (("deploy/install-tool-bundle.sh", "install-tool-bundle.sh"),
                                    ("deploy/run-trace.sh", "run-trace.sh"),
                                    ("scripts/cloud_sdk_smoke.py", "cloud_sdk_smoke.py"),
                                    ("deploy/TOOLS_BUNDLE.md", "TOOLS_BUNDLE.md")):
            # Git checkouts on Windows may use CRLF; the release contains POSIX scripts.
            (root / destination).write_text((PROJECT / source).read_text(encoding="utf-8"),
                                           encoding="utf-8", newline="\n")
        environment = work / "build-venv"
        venv.EnvBuilder(with_pip=True).create(environment)
        python = environment / "bin/python"
        run([str(python), "-m", "pip", "install", "--no-index", "--no-deps",
             "--find-links", str(wheels), *(str(path) for path in sorted(wheels.glob("*.whl")))], cwd=work)
        run([str(python), "-m", "pip", "check"], cwd=work)
        run([str(python), str(root / "bundle.py"), "--prepare", str(root), str(timeout)], cwd=work)
        # Keep receipts/relative active pointers and payloads, not downloaded archives twice.
        for child in (root / "tools").iterdir():
            if child.name not in {"installed", "active.json"}:
                shutil.rmtree(child) if child.is_dir() else child.unlink()
        with tempfile.TemporaryDirectory(prefix=".trace-bundle-", dir=output.parent) as archive_work:
            staged = Path(archive_work) / output.name
            with tarfile.open(staged, "w:gz") as archive:
                archive.add(root, arcname=root.name)
            # Exclusive publication never silently overwrites a previous release.
            with staged.open("rb") as source, output.open("xb") as destination:
                shutil.copyfileobj(source, destination)
    print(json.dumps({"bundle": str(output), "profile": profile, "full_catalog": info["full_catalog"],
                      "portable": info["portable"], "clouds": info["clouds"]}))
    return output


def main() -> None:
    if len(sys.argv) > 1 and sys.argv[1] in {"--prepare", "--verify"}:
        root = Path(sys.argv[2]).resolve()
        if sys.argv[1] == "--prepare":
            prepare(root, float(sys.argv[3]))
        else:
            verify(root)
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="New .tar.gz release path")
    parser.add_argument("--wheel", type=Path, help="Reuse an existing project wheel")
    parser.add_argument("--profile", choices=("all", "core", "browser"), default="all")
    parser.add_argument("--cloud", help="all, none, or comma-separated providers (default: all for profile all)")
    parser.add_argument("--setup-timeout", type=float, default=3600, help="Build-time portable setup budget in seconds")
    args = parser.parse_args()
    build(args.output, args.wheel, args.profile, args.cloud, args.setup_timeout)


if __name__ == "__main__":
    main()
