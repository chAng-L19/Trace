"""Deterministic complexity and deletion-candidate audit for Lean L0."""

from __future__ import annotations

import ast
import json
import re
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
PACKAGE_ROOT = ROOT / "src" / "redteam_agent"
TEST_ROOT = ROOT / "tests"
OUTPUT_JSON = ROOT / "docs" / "acceptance" / "lean-l0-audit.json"

CANDIDATES = (
    "OperationRuntimeAdapter",
    "RuntimeStoreAdapter",
    "RuntimeEventAdapter",
    "RuntimeToolAdapter",
    "AdaptivePlanner",
    "Scheduler",
    "WorkflowRegistry",
    "TacticalLoopMixin",
)


def _relative(path: Path) -> str:
    return path.relative_to(ROOT).as_posix()


def _python_files(root: Path) -> tuple[Path, ...]:
    return tuple(
        sorted(
            path
            for path in root.rglob("*.py")
            if "__pycache__" not in path.parts
        )
    )


def _module_name(path: Path) -> str:
    relative = path.relative_to(PACKAGE_ROOT).with_suffix("")
    parts = relative.parts
    if parts[-1] == "__init__":
        parts = parts[:-1]
    return ".".join(("redteam_agent", *parts))


def _local_imports(path: Path) -> tuple[str, ...]:
    try:
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    except (OSError, SyntaxError):
        return ()
    imports: set[str] = set()
    current = _module_name(path).split(".")
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for item in node.names:
                if item.name.startswith("redteam_agent"):
                    imports.add(item.name)
        elif isinstance(node, ast.ImportFrom) and node.level:
            base = current[:-node.level]
            if node.module:
                base.extend(node.module.split("."))
            imports.add(".".join(base))
    return tuple(sorted(imports))


def _references(name: str) -> dict[str, int]:
    pattern = re.compile(rf"\b{re.escape(name)}\b")
    counts: dict[str, int] = {}
    for root in (PACKAGE_ROOT, TEST_ROOT):
        for path in _python_files(root):
            try:
                count = len(pattern.findall(path.read_text(encoding="utf-8")))
            except OSError:
                count = 0
            if count:
                counts[_relative(path)] = count
    return dict(sorted(counts.items()))


def build_report() -> dict[str, Any]:
    files = _python_files(PACKAGE_ROOT)
    rows = [
        {
            "path": _relative(path),
            "lines": len(path.read_text(encoding="utf-8").splitlines()),
            "imports": list(_local_imports(path)),
        }
        for path in files
    ]
    rows.sort(key=lambda item: (-item["lines"], item["path"]))
    imports = {
        row["path"]: row["imports"]
        for row in sorted(rows, key=lambda item: item["path"])
        if row["imports"]
    }
    total_lines = sum(int(row["lines"]) for row in rows)
    return {
        "schema_version": 1,
        "scope": {
            "production_root": _relative(PACKAGE_ROOT),
            "test_root": _relative(TEST_ROOT),
        },
        "metrics": {
            "production_files": len(rows),
            "production_lines": total_lines,
            "max_file_lines": rows[0]["lines"] if rows else 0,
            "max_file": rows[0]["path"] if rows else "",
            "files_over_800_lines": [row for row in rows if row["lines"] > 800],
            "top_files": rows[:15],
        },
        "local_import_edges": imports,
        "deletion_candidates": {
            name: {"references": _references(name)} for name in CANDIDATES
        },
    }


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main() -> int:
    report = build_report()
    OUTPUT_JSON.parent.mkdir(parents=True, exist_ok=True)
    OUTPUT_JSON.write_text(canonical_json(report), encoding="utf-8")
    print(canonical_json(report), end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
