"""Offline regressions for on-demand preparation and live catalog readiness."""
from __future__ import annotations
import sys
import tempfile
import threading
import time
import stat
import zipfile
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
from model_recovery_smoke import scenario
from redteam_agent.core import ToolCall
from redteam_agent.runtime import tool_prepare as preparation
from redteam_agent.runtime.tool_setup import setup, run_probe, unpack


def main():
    checks = []
    with tempfile.TemporaryDirectory() as directory:
        archive, target = Path(directory) / "sdk.zip", Path(directory) / "sdk"
        target.mkdir()
        with zipfile.ZipFile(archive, "w") as package:
            package.writestr("docs/source.txt", "official SDK documentation")
            info = zipfile.ZipInfo("docs/link.txt")
            info.create_system = 3
            info.external_attr = (stat.S_IFLNK | 0o777) << 16
            package.writestr(info, "source.txt")
        unpack(archive, target, "zip", time.monotonic() + 5)
        assert (target / "docs/link.txt").read_text() == "official SDK documentation"
        with zipfile.ZipFile(archive, "w") as package:
            package.writestr(info, "../../outside.txt")
        try:
            unpack(archive, target, "zip", time.monotonic() + 5)
        except ValueError as exc:
            assert str(exc) == "archive_link_outside_directory"
        else:
            raise AssertionError("archive link escaped extraction directory")
    checks.append("official_sdk_zip_relative_links")
    with patch.object(preparation, "run_probe", side_effect=[(1, "missing"), (1, "installer failed")]), \
         patch.object(preparation, "resolve_executable", return_value=""):
        result = preparation.prepare(["capstone"], offline=True)
        assert not result["success"] and result["tools"][0]["error"] == "package_install_failed"
    checks.append("failed_installer_never_reports_success")
    with patch.object(preparation, "run_probe", side_effect=[(1, "missing"), (0, "installed"), (1, "broken import")]):
        result = preparation.prepare(["frida"], offline=True)
        assert not result["success"] and result["tools"][0]["error"] == "package_import_failed"
    checks.append("successful_installer_requires_import_validation")
    with patch.object(preparation, "run_probe", side_effect=[(1, "wrong SDK version"), (0, "installed"), (0, "")]) as probe:
        result = preparation.prepare(["huawei"], offline=True)
        assert result["success"] and result["tools"][0]["action"] == "installed"
        assert "version('huaweicloudsdkecs') == '3.1.217'" in probe.call_args_list[0].args[0][-1]
        assert "huaweicloudsdkecs==3.1.217" in probe.call_args_list[1].args[0]
        assert probe.call_args_list[0].args[0] == probe.call_args_list[2].args[0]
    checks.append("partial_or_old_sdk_install_is_repaired_and_revalidated")
    from redteam_agent.runtime.tool_doctor import doctor
    def inventory(name, root=None, **kwargs):
        return {"name": name, "installed": False, "path": "", "source": "missing", "checksum_status": "mismatch" if name == "gcp" else "not_installed"}
    def executable(*names, **kwargs):
        if names == ("gcloud",):
            assert kwargs.get("include_managed") is False, "doctor must not execute corrupt managed CLI"
        return ""
    with tempfile.TemporaryDirectory() as directory, \
         patch("redteam_agent.runtime.tool_doctor.managed_install", side_effect=inventory) as managed, \
         patch("redteam_agent.runtime.tool_doctor.resolve_executable", side_effect=executable), \
         patch("redteam_agent.runtime.tool_doctor.chromium_executable", return_value=""), \
         patch("redteam_agent.runtime.tool_doctor._package", side_effect=lambda name, version: {"name": name, "installed": False, "repair": "setup"}), \
         patch("redteam_agent.runtime.tool_doctor.run_probe", return_value=(1, "missing")):
        result = doctor(root=Path(directory), runtime_root=Path(directory), configs=[])
        gcp = next(item for item in result["tools"] if item["name"] == "gcp")
        assert not gcp["installed"] and gcp["checksum_status"] == "mismatch"
        for call in managed.call_args_list:
            if call.args[0] in {"gcp", "aliyun"}:
                assert call.kwargs["verify"] is True
    checks.append("doctor_rejects_corrupt_cloud_cli_before_execution")
    with tempfile.TemporaryDirectory() as directory, \
         patch("redteam_agent.runtime.tool_doctor.managed_install", side_effect=inventory), \
         patch("redteam_agent.runtime.tool_doctor.resolve_executable", return_value=""), \
         patch("redteam_agent.runtime.tool_doctor.chromium_executable", return_value=""), \
         patch("redteam_agent.runtime.tool_doctor._package", side_effect=lambda name, version: {"name": name, "installed": False, "repair": "setup"}), \
         patch("redteam_agent.runtime.tool_doctor.run_probe", side_effect=__import__("subprocess").TimeoutExpired("sdk-import", 5)):
        result = doctor(root=Path(directory), runtime_root=Path(directory), configs=[])
        assert all(not row["installed"] and row["runtime_validation"] == "failed" for row in result["tools"] if row["name"] in {"huawei", "volcengine", "baidu", "jdcloud"})
    checks.append("doctor_sdk_timeout_is_a_failed_row")
    with patch.object(preparation, "_package", return_value={"name": "playwright", "installed": True}), \
         patch.object(preparation, "setup", return_value={"tools": [{"name": "chromium", "installed": True}]}) as portable:
        assert preparation.prepare(["chromium"], offline=True)["success"]
        assert portable.call_args.kwargs["offline"] is True
    checks.append("chromium_prepares_playwright_then_existing_setup")
    with tempfile.TemporaryDirectory() as directory, \
         patch("redteam_agent.runtime.tool_setup.importlib.metadata.version", side_effect=__import__("importlib.metadata", fromlist=["PackageNotFoundError"]).PackageNotFoundError("playwright")):
        result = setup(["chromium"], root=Path(directory), offline=True)
        assert result["tools"][0]["error"] == "playwright_missing_run_trace_setup_playwright"
    checks.append("missing_playwright_has_actionable_diagnostic")
    with scenario([]) as (service, _, run_id):
        with patch.object(preparation, "module_available", return_value=False), patch.object(preparation, "chromium_executable", return_value=""):
            catalog = service.tools.catalog(run_id)
            names = {item.qualified_name for item in catalog.tools}
            assert "agent:prepare_tools" in names and "builtin:binary-analysis" in names
            assert "builtin:browser-create" not in names and "builtin:frida-processes" not in names
            assert "disassemble" not in next(item.capabilities for item in catalog.tools if item.qualified_name == "builtin:binary-analysis")
            assert any(item.reason == "dependency_unavailable" for item in catalog.visibility)
        with patch.object(preparation, "prepare", return_value={"success": True, "tools": []}), \
             patch.object(preparation, "module_available", return_value=True), patch.object(preparation, "chromium_executable", return_value="chromium"):
            call = ToolCall(call_id="prepare", run_id=run_id, tool_name="agent:prepare_tools", arguments={"tools": ["playwright", "chromium"]})
            result = service.tools.invoke(call)
            assert result.status == "success", result
            assert "builtin:browser-create" in result.output["available_tools"]
            assert service.tools.snapshot(run_id).revision == result.output["catalog_revision"]
        with patch.object(preparation, "prepare", return_value={"success": False, "tools": [{"error": "installer failed"}]}):
            result = service.tools.invoke(call)
            assert result.status == "failed" and result.error == "tool_preparation_failed"
    checks.append("agent_prepare_refreshes_catalog_and_propagates_failure")
    with scenario([]) as (service, _, run_id), tempfile.TemporaryDirectory() as directory:
        marker = Path(directory) / "started"
        def running_package(name, deadline, **kwargs):
            run_probe([sys.executable, "-c", "import pathlib,time; pathlib.Path(" + repr(str(marker)) + ").write_text('started'); time.sleep(30)"], timeout=30)
            raise AssertionError("cancelled process must not complete")
        results = []
        service.tools.catalog(run_id)
        call = ToolCall(call_id="cancel-prepare", run_id=run_id, tool_name="agent:prepare_tools", arguments={"tools": ["capstone", "frida"]})
        with patch.object(preparation, "_package", side_effect=running_package) as package:
            worker = threading.Thread(target=lambda: results.append(service.tools.invoke(call)))
            worker.start()
            deadline = time.monotonic() + 5
            while not marker.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            assert marker.exists(), "prepare child did not start"
            assert service.tools.cancel(call.call_id)
            worker.join(5)
            assert not worker.is_alive(), "prepare cancellation did not terminate child"
            assert results[0].status == "failed" and results[0].output["tools"][0]["error"] == "tool_preparation_cancelled"
            assert package.call_count == 1, "cancellation must stop remaining dependencies"
    checks.append("agent_cancel_terminates_real_prepare_child")
    with tempfile.TemporaryDirectory() as directory:
        active, overlap, lock = [], [], threading.Lock()
        def installing(name, deadline, **kwargs):
            with lock:
                if active:
                    overlap.append(name)
                active.append(name)
            time.sleep(0.05)
            with lock:
                active.remove(name)
            return {"name": name, "installed": True}
        with patch.object(preparation, "_package", side_effect=installing):
            results = []
            workers = [threading.Thread(target=lambda name=name: results.append(preparation.prepare([name], root=Path(directory) / name))) for name in ("capstone", "frida")]
            for worker in workers:
                worker.start()
            for worker in workers:
                worker.join(5)
            assert len(results) == 2 and all(result["success"] for result in results) and not overlap
    checks.append("python_install_serializes_across_tools_roots")
    assert not preparation.prepare(["not-a-supported-tool"], offline=True)["success"]
    checks.append("unsupported_dependency_is_not_installed")
    print({"status": "passed", "checks": checks})


if __name__ == "__main__":
    main()
