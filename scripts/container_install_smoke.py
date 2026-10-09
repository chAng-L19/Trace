"""Install an offline fixture wheel in the writable target; import it without restart."""
from __future__ import annotations

import importlib
import os
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

from redteam_agent.runtime.managed_tools import resolve_executable
from redteam_agent.runtime.tool_prepare import _pip_command


def main():
    target = Path(os.environ["PIP_TARGET"]).resolve()
    assert str(target) in sys.path, "target_not_loaded_at_interpreter_start"
    with tempfile.TemporaryDirectory(prefix="trace-install-smoke-") as temporary:
        wheel = Path(temporary) / "trace_install_fixture-1.0-py3-none-any.whl"
        metadata = "trace_install_fixture-1.0.dist-info/"
        with zipfile.ZipFile(wheel, "w") as archive:
            archive.writestr("trace_install_fixture.py", "VALUE = 'installed-live'\ndef main(): print(VALUE)\n")
            archive.writestr(metadata + "METADATA", "Metadata-Version: 2.1\nName: trace-install-fixture\nVersion: 1.0\n")
            archive.writestr(metadata + "WHEEL", "Wheel-Version: 1.0\nGenerator: trace-smoke\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
            archive.writestr(metadata + "entry_points.txt", "[console_scripts]\ntrace-install-fixture = trace_install_fixture:main\n")
            archive.writestr(metadata + "RECORD", "")
        subprocess.run(_pip_command(str(wheel), offline=True, proxy=None), check=True)
    importlib.invalidate_caches()
    module = importlib.import_module("trace_install_fixture")
    assert module.VALUE == "installed-live"
    assert Path(module.__file__).resolve().parent == target
    launcher = resolve_executable("trace-install-fixture")
    assert launcher, "new_cli_not_discoverable"
    child_environment = dict(os.environ)
    child_environment.pop("PYTHONPATH", None)
    assert subprocess.check_output([sys.executable, "-c", "import trace_install_fixture; print(trace_install_fixture.VALUE)"],
                                   text=True, env=child_environment).strip() == module.VALUE
    assert subprocess.check_output([launcher], text=True, env=child_environment).strip() == module.VALUE
    print({"installed": True, "imported_without_restart": True, "launcher": launcher})


if __name__ == "__main__":
    main()
