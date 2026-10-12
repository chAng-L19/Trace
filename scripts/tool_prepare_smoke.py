"""Verify local diagnostics cannot invoke package managers or download tools."""
from __future__ import annotations

import json
from pathlib import Path
import sys
import tempfile
import subprocess
import os
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from model_recovery_smoke import scenario
from redteam_agent.core import ToolCall
from redteam_agent.runtime import tool_prepare as preparation
from redteam_agent.runtime.tool_doctor import doctor
from redteam_agent.cli import main as cli


def main():
    checks = []
    with tempfile.TemporaryDirectory() as directory, \
         patch("subprocess.Popen", side_effect=AssertionError("unexpected child process")), \
         patch("urllib.request.urlopen", side_effect=AssertionError("unexpected network")), \
         patch("redteam_agent.runtime.tool_setup.setup", side_effect=AssertionError("unexpected installer")), \
         patch.object(preparation, "module_available", return_value=False), \
         patch.object(preparation, "chromium_executable", return_value=""), \
         patch("redteam_agent.runtime.tool_doctor.module_available", return_value=False), \
         patch("redteam_agent.runtime.tool_doctor.chromium_executable", return_value=""), \
         patch("redteam_agent.runtime.tool_doctor.resolve_executable", return_value=""):
        root = Path(directory)
        assert preparation.prepare(["page-fetch", "binary-disassemble", "process-inventory"], root=root)["success"]
        missing = preparation.prepare(["playwright", "chromium", "browser-automation"], root=root)
        assert not missing["success"] and not missing["downloaded"] and not missing["installed"]
        assert not preparation.prepare(["unknown"], root=root)["success"]
        assert preparation.adapter_dependencies("browser-screenshot") == ("playwright", "chromium")
        report = doctor(root=root, runtime_root=root, configs=[])
        assert report["ready"] and not report["downloads_allowed"]
        assert next(row for row in report["capabilities"] if row["name"] == "browser_automation")["status"] == "optional_unavailable"
        checks.append("diagnostics_do_not_execute_installers_or_network")
        assert cli(["setup", "page-fetch", "--json", "--tools-dir", str(root)]) == 0
        assert cli(["setup", "browser-automation", "--json", "--tools-dir", str(root)]) == 1
        checks.append("cli_reports_actual_capability_readiness")

    # GitHub Windows runners can expose a legacy code page on stdout.
    with tempfile.TemporaryDirectory() as directory:
        env = {**os.environ, "PYTHONIOENCODING": "cp1252"}
        completed = subprocess.run(
            [sys.executable, "-B", "-m", "redteam_agent", "setup", "page-fetch", "--json",
             "--tools-dir", directory], env=env, capture_output=True, text=True,
            encoding="cp1252", check=False,
        )
        assert completed.returncode == 0, completed.stderr
        assert json.loads(completed.stdout)["success"] is True

    with scenario([]) as (service, _, run_id):
        catalog = service.tools.catalog(run_id)
        tool = next(item for item in catalog.tools if item.qualified_name == "agent:prepare_tools")
        assert not tool.side_effecting and "never installs" in tool.description
        result = service.tools.invoke(ToolCall(call_id="inspect", run_id=run_id,
            tool_name="agent:prepare_tools", arguments={"tools": ["page-fetch"]}))
        assert result.status == "success" and result.output["downloaded"] is False
        assert result.output["catalog_revision"] == service.tools.snapshot(run_id).revision
        checks.append("agent_inspection_has_no_install_side_effect")
    print(json.dumps({"ok": True, "checks": checks}))


if __name__ == "__main__":
    main()
