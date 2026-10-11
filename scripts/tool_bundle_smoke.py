"""Offline bundle/receipt fixtures and real shell flow with mocked Python/CLI."""
from __future__ import annotations

import copy
import io
import json
import os
import shlex
import shutil
import subprocess
import sys
import sysconfig
import tarfile
import tempfile
import venv
import zipfile
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
import build_tool_bundle as bundle
from redteam_agent.runtime import managed_tools, tool_setup, tool_prepare, tool_doctor


def wheel(path: Path, name: str, version: str = "1.0", manifest: dict | None = None,
          requires: tuple[str, ...] = ()) -> Path:
    stem = name.replace("-", "_") + "-" + version
    destination = path / f"{stem}-py3-none-any.whl"
    with zipfile.ZipFile(destination, "w") as archive:
        archive.writestr(f"{stem}.dist-info/METADATA", f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n"
                         + "Provides-Extra: sliver\n" + "".join(f"Requires-Dist: {item}\n" for item in requires))
        archive.writestr(f"{stem}.dist-info/WHEEL", "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
        archive.writestr(f"{stem}.dist-info/RECORD", "")
        archive.writestr(name.replace("-", "_") + ".py", "VALUE = 'fixture'\n")
        if manifest:
            archive.writestr("redteam_agent/tool_manifest.json", json.dumps(manifest))
    return destination


def fixtures(root: Path) -> tuple[dict, dict]:
    manifest = copy.deepcopy(managed_tools.manifest())
    archives = {}
    for name, definition in manifest["tools"].items():
        asset = definition["platforms"]["linux-x86_64"]
        path = root / (name + "." + asset["format"])
        content = f"#!/bin/sh\necho {definition['version']}\n".encode()
        if asset["format"] == "zip":
            with zipfile.ZipFile(path, "w") as archive:
                item = zipfile.ZipInfo(asset["executable"])
                item.external_attr = 0o100755 << 16
                archive.writestr(item, content)
        else:
            with tarfile.open(path, "w:xz" if asset["format"] == "tar.xz" else "w:gz") as archive:
                item = tarfile.TarInfo(asset["executable"])
                item.size, item.mode = len(content), 0o755
                archive.addfile(item, io.BytesIO(content))
        asset.update(size=path.stat().st_size, sha256=managed_tools.sha256(path))
        asset.pop("publisher_md5", None)
        archives[asset["url"]] = path
    return manifest, archives


def expect_failure(function, message: str) -> None:
    try:
        function()
    except (RuntimeError, FileNotFoundError, FileExistsError, ValueError, subprocess.CalledProcessError):
        return
    raise AssertionError(message)


def build_smoke(root: Path) -> Path:
    manifest, archives = fixtures(root)
    source = wheel(root, "trace-agent", manifest=manifest)
    output = root / "release.tar.gz"
    host = {"platform": "linux-x86_64", "python": [3, 12], "implementation": "cpython", "libc": ["glibc", "2.36"]}
    playwright = root / "playwright"
    browsers = playwright / "driver/package/browsers.json"
    browsers.parent.mkdir(parents=True)
    definition = manifest["tools"]["chromium"]
    browsers.write_text(json.dumps({"browsers": [{"name": "chromium", "revision": definition["revision"],
                                                "browserVersion": definition["version"]}]}))
    commands, downloads = [], []
    original_import = bundle.importlib.import_module
    expected_modules = {*manifest["python_packages"], "playwright.sync_api", "playwright.async_api",
                        *bundle.IMPORTS.values(), "grpc", "google.protobuf.descriptor_pb2"}
    seen_imports = set()

    def imported(name, *args, **kwargs):
        if name in expected_modules:
            seen_imports.add(name)
            return object()
        return original_import(name, *args, **kwargs)

    def download(asset, *_args, **_kwargs):
        downloads.append(asset["url"])
        return archives[asset["url"]]

    def validate(name, executable, version, _deadline):
        assert executable.is_file(), name
        return {"status": "passed", "version": version, "probe": "fixture"}

    def runner(command, **_kwargs):
        commands.append(command)
        if command[1:4] == ["-m", "pip", "wheel"]:
            assert "https://pypi.org/simple" in command
            destination = Path(command[command.index("--wheel-dir") + 1])
            assert str(source) + "[sliver]" in command, "--wheel must bypass project build"
            requirements = command[command.index(str(source) + "[sliver]") + 1:]
            assert set(requirements) == set(bundle.selection("all", None, manifest)["requirements"])
            for requirement in requirements:
                name, _, version = requirement.partition("==")
                wheel(destination, name, version or "1.0")
            for dependency in ("pyee", "greenlet", "typing-extensions", "grpcio", "protobuf", "fixture-transitive"):
                wheel(destination, dependency)
        elif command[1:4] == ["-m", "pip", "install"]:
            assert {"--no-index", "--no-deps", "--find-links"} <= set(command)
            assert any("fixture_transitive" in arg for arg in command)
            assert all(Path(arg).is_file() for arg in command if arg.endswith(".whl"))
        elif command[1:4] == ["-m", "pip", "check"]:
            pass
        elif "--prepare" in command:
            bundle.prepare(Path(command[-2]), float(command[-1]))
        elif Path(command[1]).name == "cloud_sdk_smoke.py":
            assert Path(command[1]).is_file()
            assert "baidubce.http.bce_http_client" in Path(command[1]).read_text()
        else:
            raise AssertionError(f"Unexpected/unmocked command: {command}")

    with ExitStack() as stack:
        checkout = root / "checkout"
        for relative in ("deploy/install-tool-bundle.sh", "deploy/run-trace.sh", "deploy/TOOLS_BUNDLE.md",
                         "scripts/cloud_sdk_smoke.py"):
            destination = checkout / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(bundle.PROJECT / relative, destination)
        stack.enter_context(patch.object(bundle, "PROJECT", checkout))
        stack.enter_context(patch.object(bundle, "target", return_value=host))
        stack.enter_context(patch.object(bundle, "run", side_effect=runner))
        stack.enter_context(patch.object(bundle.venv.EnvBuilder, "create"))
        for module in (managed_tools, tool_setup):
            stack.enter_context(patch.object(module, "manifest", return_value=manifest))
            stack.enter_context(patch.object(module, "platform_key", return_value="linux-x86_64"))
        stack.enter_context(patch.object(tool_setup, "download", side_effect=download))
        stack.enter_context(patch.object(tool_setup, "validate", side_effect=validate))
        stack.enter_context(patch.object(tool_setup.importlib.metadata, "version", side_effect=manifest["python_packages"].__getitem__))
        stack.enter_context(patch.object(tool_setup.importlib.resources, "files", return_value=playwright))
        stack.enter_context(patch.object(bundle.importlib, "import_module", side_effect=imported))
        # Even a matching host engine must not replace the release payload.
        stack.enter_context(patch.object(managed_tools.shutil, "which", return_value="host-engine"))
        with patch.object(sys, "argv", ["build_tool_bundle.py", "--wheel", str(source), "--output", str(output)]):
            bundle.main()
        assert len(downloads) == 4 and seen_imports == expected_modules
        assert len(commands) == 5
        with tarfile.open(output) as archive:
            names = archive.getnames()
            assert not any(part in namestr.split("/") for namestr in names for part in
                           ("downloads", "staging", ".setup.lock", "state", "build-venv"))
            archive.extractall(root / "unpacked", filter="data")
        extracted = root / "unpacked/trace-tool-bundle"
        info = json.loads((extracted / "bundle.json").read_text())
        assert info["full_catalog"] and set(info["portable"]) == set(manifest["tools"])
        assert set(info["clouds"]) == set(bundle.CLOUDS)
        assert info["wheels"] == sorted(p.name for p in (extracted / "wheels").glob("*.whl"))
        assert any("fixture_transitive" in name for name in info["wheels"])
        assert all((extracted / name).is_file() for name in
                   ("install-tool-bundle.sh", "run-trace.sh", "bundle.py", "cloud_sdk_smoke.py", "TOOLS_BUNDLE.md"))
        # Execute the installer's actual preflight expression against the fixture.
        command = next(line for line in (extracted / "install-tool-bundle.sh").read_text().splitlines()
                       if line.startswith('"$python" -c '))
        code = compile(shlex.split(command)[2], "installer-preflight", "exec")
        with patch.object(sys, "argv", ["-c", str(extracted / "bundle.json")]), \
                patch.object(sys, "platform", "linux"), patch.object(sys, "version_info", (3, 12, 0)), \
                patch.object(sysconfig, "get_platform", return_value=host["platform"]):
            exec(code, {})
            missing_wheel = extracted / "wheels" / info["wheels"][-1]
            saved = missing_wheel.read_bytes()
            missing_wheel.unlink()
            try:
                exec(code, {})
            except AssertionError as error:
                assert "Missing or unexpected bundle wheels" in str(error)
            else:
                raise AssertionError("Installer accepted a missing wheel")
            missing_wheel.write_bytes(saved)
        bundle.verify(extracted)
        diagnostic_smoke(extracted, root)
        for name in info["portable"]:
            installed = managed_tools.managed_install(name, extracted / "tools", verify=True)
            receipt = json.loads((Path(installed["directory"]) / "receipt.json").read_text())
            assert installed["installed"] and not Path(receipt["executable"]).is_absolute()
            assert all(not Path(path).is_absolute() for path in receipt["files"])
        moved = root / "relocated"
        shutil.copytree(extracted, moved)
        bundle.verify(moved)
        # Receipts passed on the build host do not waive the target's library probe.
        with patch.object(tool_setup, "validate", side_effect=REAL_VALIDATE), \
                patch.object(tool_setup, "run_probe", return_value=(127, "error while loading shared libraries: libnss3.so")):
            try:
                bundle.verify(moved)
            except RuntimeError as error:
                assert str(error) == "chromium_system_libraries_missing"
            else:
                raise AssertionError("Target Chromium libraries were not checked")
        missing = managed_tools.managed_install("rizin", moved / "tools")
        Path(missing["path"]).unlink()
        expect_failure(lambda: bundle.verify(moved), "Missing portable accepted")
        with patch.object(bundle.importlib, "import_module", side_effect=RuntimeError("missing engine")):
            expect_failure(lambda: bundle.verify(extracted), "Missing Python engine accepted")
        with patch.object(bundle.shutil, "which", return_value=None):
            expect_failure(lambda: bundle.verify(extracted), "Missing cloud CLI accepted")
        with patch.object(tool_setup, "download", side_effect=FileNotFoundError("missing archive")):
            failed = root / "failed.tar.gz"
            expect_failure(lambda: bundle.build(failed, source, "all", None, 60), "Missing archive accepted")
            assert not failed.exists()
        with patch.object(bundle, "run", side_effect=subprocess.CalledProcessError(1, "pip wheel")):
            failed = root / "dependency-failed.tar.gz"
            expect_failure(lambda: bundle.build(failed, source, "all", None, 60), "Resolution failure accepted")
            assert not failed.exists()
        before = output.read_bytes()
        expect_failure(lambda: bundle.build(output, source, "all", None, 60), "Release overwritten")
        assert output.read_bytes() == before
    assert bundle.selection("core", None, manifest)["requirements"] == []
    assert bundle.selection("browser", None, manifest)["portable"] == ["chromium"]
    assert not bundle.selection("all", "none", manifest)["full_catalog"]
    assert bundle.selection("core", "aws,huawei,gcp", manifest)["portable"] == ["gcp"]
    expect_failure(lambda: bundle.selection("all", "unknown", manifest), "Unknown cloud accepted")
    return extracted


REAL_VALIDATE = tool_setup.validate


def diagnostic_smoke(extracted: Path, root: Path) -> None:
    tools = extracted / "tools"
    # No browser in PATH or cache: only the portable receipt is available.
    def chromium(root=None, *, include_managed=True):
        return managed_tools.managed_install("chromium", root)["path"] if include_managed else ""

    resolve = managed_tools.resolve_executable
    def executable(*names, **kwargs):
        return "fixture-cli" if names[0] in {"aws", "az", "tccli"} else resolve(*names, **kwargs)

    with patch.dict(os.environ, {"TRACE_TOOLS_HOME": str(root / "wrong-tools")}), \
            patch.object(managed_tools.shutil, "which", return_value=None), \
            patch.object(tool_prepare, "module_available", return_value=True), \
            patch.object(tool_doctor, "module_available", return_value=True), \
            patch.object(tool_prepare, "chromium_executable", side_effect=chromium), \
            patch.object(tool_doctor, "chromium_executable", side_effect=chromium), \
            patch.object(tool_prepare, "resolve_executable", side_effect=executable), \
            patch.object(tool_doctor, "run_probe", return_value=(0, "")), \
            patch("redteam_agent.application.bootstrap.configuration_projection", return_value={"status": "valid"}), \
            patch.object(tool_setup, "setup", side_effect=AssertionError("runtime installer invoked")):
        names = [*managed_tools.manifest()["tools"], "browser-automation"]
        report = tool_prepare.prepare(names, root=tools)
        assert report["success"] and not report["downloaded"]
        for item in report["tools"][:-1]:
            assert item["source"] == "managed" and Path(item["path"]).is_relative_to(tools)
        assert not tool_prepare.adapter_dependencies("browser-screenshot", root=tools)
        with patch.dict(os.environ, {"TRACE_TOOLS_HOME": str(tools)}):
            assert not tool_prepare.adapter_dependencies("browser-screenshot")
        report = tool_doctor.doctor(root=tools, runtime_root=root, probe_browser=True)
        rows = {item["name"]: item for item in report["tools"]}
        assert all(rows[name]["source"] == "managed" for name in managed_tools.manifest()["tools"])
        caps = {item["name"]: item for item in report["capabilities"]}
        assert caps["browser_automation"]["status"] == "ready"
        assert caps["cloud_inventory"]["status"] == "external_credentials_required"
        with patch.object(tool_prepare, "module_available", side_effect=lambda name: name != "baidubce"):
            report = tool_doctor.doctor(root=tools, runtime_root=root)
            assert next(item for item in report["capabilities"] if item["name"] == "cloud_inventory")["status"] == "partial"
        with patch.object(tool_doctor, "run_probe", return_value=(127, "libnss3.so missing")):
            report = tool_doctor.doctor(root=tools, runtime_root=root, probe_browser=True)
            assert next(item for item in report["capabilities"] if item["name"] == "browser_automation")["status"] == "failed"
            assert not report["ready"]
        with patch.object(tool_doctor, "module_available", return_value=False):
            assert not tool_doctor.doctor(root=tools, runtime_root=root, probe_browser=True)["ready"]


def offline_closure_smoke(root: Path) -> None:
    source, resolved = root / "fixture-wheels", root / "resolved-wheels"
    source.mkdir()
    project = wheel(source, "fixture-project", requires=("fixture-leaf==1.0", "fixture-grpc==1.0; extra == 'sliver'"))
    leaf = wheel(source, "fixture-leaf")
    wheel(source, "fixture-grpc")
    # Use real pip resolution, venv, installation and pip check; all inputs are local.
    bundle.run([sys.executable, "-m", "pip", "wheel", "--no-index", "--find-links", str(source),
                "--wheel-dir", str(resolved), str(project) + "[sliver]"])
    assert {path.name for path in resolved.glob("*.whl")} == {path.name for path in source.glob("*.whl")}
    environment = root / "closure-venv"
    venv.EnvBuilder(with_pip=True).create(environment)
    python = environment / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
    command = [str(python), "-m", "pip", "install", "--no-index", "--no-deps", "--find-links", str(resolved)]
    bundle.run([*command, *(str(p) for p in resolved.glob("*.whl") if p.name != leaf.name)])
    expect_failure(lambda: bundle.run([str(python), "-m", "pip", "check"]), "Missing transitive wheel accepted")
    bundle.run([*command, str(resolved / leaf.name)])
    bundle.run([str(python), "-m", "pip", "check"])
    bundle.run([str(python), "-c", "import fixture_project, fixture_leaf, fixture_grpc"])


MOCK_PYTHON = r'''#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$SMOKE_LOG"
case "$1" in
    -c) [ "${FAIL_PREFLIGHT:-0}" = 0 ]; exit ;;
    -m)
        if [ "$2" = venv ]; then
            mkdir -p "$3/bin"
            cp "$TRACE_PYTHON" "$3/bin/python"
            cp "$SMOKE_TRACE" "$3/bin/trace"
            chmod +x "$3/bin/python" "$3/bin/trace"
        elif [ "$2 $3" = 'pip install' ]; then
            case " $* " in *' --no-index '*) ;; *) exit 31 ;; esac
            case " $* " in *' --no-deps '*) ;; *) exit 32 ;; esac
            case " $* " in *' --find-links '*) ;; *) exit 33 ;; esac
            [ "$PIP_CONFIG_FILE" = /dev/null ] && [ "$PIP_NO_INDEX" = 1 ]
            [ "$CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK" = true ]
            [ "$CLOUDSDK_CORE_DISABLE_USAGE_REPORTING" = true ]
            [ -z "${PIP_TARGET:-}" ] && [ -z "${PYTHONPATH:-}" ]
            for item in "$@"; do case "$item" in *.whl) [ -f "$item" ] ;; esac; done
        elif [ "$2 $3" != 'pip check' ]; then exit 34; fi ;;
    -) cat >/dev/null ;;
    *) [ "$2" = --verify ] && [ "${FAIL_VERIFY:-0}" = 0 ] ;;
esac
'''
MOCK_TRACE = '''#!/bin/sh
set -eu
printf '%s\\n' "trace:$*" >> "$SMOKE_LOG"
if [ "${1:-}" != self-test ]; then
    printf '%s\\n' "bin=$TRACE_BIN" "tools=$TRACE_TOOLS_HOME" "state=$TRACE_HOME"
fi
'''


def shell_smoke(root: Path, extracted: Path) -> None:
    shell = shutil.which("sh")
    if os.name == "nt":
        candidate = Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git/bin/bash.exe"
        shell = str(candidate) if candidate.exists() else shell
    if not shell:
        raise RuntimeError("POSIX sh required for installer smoke")
    prefix, state = root / "prefix with spaces", root / "existing-state"
    prefix.mkdir()
    state.mkdir()
    for name in ("state.db", "master.key", "config.toml"):
        (state / name).write_bytes(b"existing-private-state\x00\xff")
    before = {p.name: p.read_bytes() for p in state.iterdir()}
    fake_python, fake_trace = root / "python-mock", root / "trace-mock"
    fake_python.write_text(MOCK_PYTHON, encoding="utf-8", newline="\n")
    fake_trace.write_text(MOCK_TRACE, encoding="utf-8", newline="\n")
    fake_python.chmod(0o700)
    fake_trace.chmod(0o700)

    def shell_path(path):
        return subprocess.check_output([shell, "-c", 'cygpath -u "$1"', "--", str(path)], text=True).strip() if os.name == "nt" else str(path)

    env = {**os.environ, "TRACE_INSTALL_PREFIX": shell_path(prefix), "TRACE_HOME": shell_path(state),
           "TRACE_PYTHON": shell_path(fake_python), "SMOKE_TRACE": shell_path(fake_trace),
           "SMOKE_LOG": shell_path(root / "commands.log"), "PIP_TARGET": shell_path(state),
           "PYTHONPATH": shell_path(state), "MSYS": "winsymlinks:sys"}
    script = shell_path(extracted / "install-tool-bundle.sh")
    subprocess.run([shell, "-n", script], check=True)
    installed = subprocess.run([shell, script], env=env, capture_output=True, text=True)
    assert installed.returncode == 0, installed.stdout + installed.stderr
    def readlink(path):
        result = subprocess.run([shell, "-c", 'readlink "$1" || { ls -la "$1"; exit 1; }', "--", shell_path(path)], env=env, capture_output=True)
        assert result.returncode == 0, result.stdout + result.stderr
        return result.stdout

    # Read through POSIX tools: Git sh supports emulated links without Windows privileges.
    current_before = readlink(prefix / "current")
    failed = subprocess.run([shell, script], env={**env, "FAIL_VERIFY": "1"}, capture_output=True)
    assert failed.returncode != 0
    assert readlink(prefix / "current") == current_before
    assert not (prefix / "previous").exists()
    assert {p.name: p.read_bytes() for p in state.iterdir()} == before
    releases = sorted((prefix / "releases").iterdir())
    assert len(releases) == 2
    log = (root / "commands.log").read_text()
    assert log.count("-m pip install --no-index --no-deps --find-links") == 2
    assert log.count("trace:self-test") == 1
    assert "apt" not in log and "download" not in log and "-m playwright" not in log
    assert all((p / "tools/active.json").exists() for p in releases)
    # Run the published launcher through both prefix/current symlinks.
    active = next(p for p in releases if (p / "trace-service").exists())
    result = subprocess.run([shell, shell_path(prefix / "trace-service"), "cli", "status"],
                            env={**env, "TRACE_BIN": "/wrong", "TRACE_TOOLS_HOME": "/wrong"},
                            text=True, capture_output=True, check=True)
    assert "bin=" + shell_path(active / "bin") in result.stdout
    assert "tools=" + shell_path(active / "tools") in result.stdout
    assert "state=" + shell_path(state) in result.stdout
    assert {p.name: p.read_bytes() for p in state.iterdir()} == before
    previous = readlink(prefix / "current")
    subprocess.run([shell, script], env=env, check=True, capture_output=True)
    assert readlink(prefix / "previous") == previous
    assert readlink(prefix / "current") != previous


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="trace-bundle-smoke-") as temporary:
        root = Path(temporary)
        extracted = build_smoke(root)
        shell_smoke(root, extracted)
        offline_closure_smoke(root)
    print(json.dumps({"ok": True, "checks": ["all_profile_and_transitive_wheels", "official_archive_setup_fixtures",
          "relative_receipts_relocate", "missing_dependencies_fail", "partial_profiles_not_full",
          "installer_offline_flags", "failed_install_preserves_current", "state_and_keys_preserved",
          "release_tool_paths", "managed_runtime_diagnostics", "chromium_target_libraries",
          "real_offline_pip_dependency_closure"], "real_downloads": False, "real_deployment": False}))


if __name__ == "__main__":
    main()
