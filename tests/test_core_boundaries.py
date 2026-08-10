from __future__ import annotations

import ast
import sys
from pathlib import Path

from redteam_agent.core.ports import EventPort, ModelPort, StorePort, ToolPort, WorkerPort


REPO_ROOT = Path(__file__).resolve().parents[1]
CORE_ROOT = REPO_ROOT / "src" / "redteam_agent" / "core"


def test_core_imports_only_standard_library_or_core_modules() -> None:
    violations: list[str] = []
    for path in CORE_ROOT.rglob("*.py"):
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.ImportFrom):
                if node.level > 0:
                    continue
                module = str(node.module or "")
            elif isinstance(node, ast.Import):
                module = str(node.names[0].name if node.names else "")
            else:
                continue
            root = module.partition(".")[0]
            if root not in sys.stdlib_module_names and module != "redteam_agent.core":
                violations.append(f"{path.relative_to(CORE_ROOT)}:{node.lineno}:{module}")
    assert violations == []


def test_core_has_no_runtime_adapter_or_transport_imports() -> None:
    forbidden = {"sqlite3", "redteam_agent.runtime", "redteam_agent.adapters", "httpx", "mcp", "codex"}
    imported: set[str] = set()
    for path in CORE_ROOT.rglob("*.py"):
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.ImportFrom) and node.level == 0:
                imported.add(str(node.module or ""))
            elif isinstance(node, ast.Import):
                imported.update(alias.name for alias in node.names)
    assert not {name for name in imported if any(name == item or name.startswith(f"{item}.") for item in forbidden)}


def test_port_protocols_expose_the_frozen_phase1_methods() -> None:
    expected = {
        ModelPort: {"capabilities", "complete", "stream", "cancel"},
        ToolPort: {"discover", "invoke", "reconcile", "cancel"},
        WorkerPort: {"capabilities", "execute", "reconcile", "cancel"},
        StorePort: {"load_run", "commit_run"},
        EventPort: {"append", "read"},
    }
    for protocol, methods in expected.items():
        assert methods.issubset(vars(protocol))
