from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

from ..core import contract_hash
from ..runtime.model_common import utc_now
from ..runtime.security import secure_directory
from ..runtime.store_common import ImmutableRecordError, _dump, _load


SAFE_ENV_KEYS = frozenset(
    {
        "COMSPEC",
        "HOME",
        "LANG",
        "LC_ALL",
        "PATH",
        "PATHEXT",
        "SYSTEMDRIVE",
        "SYSTEMROOT",
        "TEMP",
        "TMP",
        "USERPROFILE",
        "WINDIR",
    }
)


@dataclass(frozen=True, slots=True)
class RunWorkspace:
    run_id: str
    workspace_key: str
    path: Path
    manifest_hash: str
    created_at: str


class WorkspaceManager:
    def __init__(self, root: Path, store: Any) -> None:
        self.root = root / "workspaces"
        self.store = store
        secure_directory(self.root)

    @staticmethod
    def _key(run_id: str) -> str:
        return hashlib.sha256(str(run_id).encode("utf-8")).hexdigest()

    def _path(self, workspace_key: str) -> Path:
        root = self.root.resolve()
        path = self.root / workspace_key[:2] / workspace_key[2:]
        if os.path.commonpath((str(root), str(path.parent.resolve()))) != str(root):
            raise ValueError("workspace_path_escape")
        return path

    def ensure(self, run_id: str) -> RunWorkspace:
        if self.store.load_operation(run_id) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        key = self._key(run_id)
        path = self._path(key)
        secure_directory(path)
        if path.is_symlink() or path.parent.is_symlink():
            raise ValueError("workspace_path_symlink")
        now = utc_now()
        manifest = {
            "schema_version": 1,
            "run_id": run_id,
            "workspace_key": key,
            "relative_path": f"{key[:2]}/{key[2:]}",
        }
        manifest_hash = contract_hash(manifest)
        with self.store.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM run_workspaces WHERE run_id=?", (run_id,)
            ).fetchone()
            if row is None:
                connection.execute(
                    "INSERT INTO run_workspaces(run_id, workspace_key, manifest_hash, manifest_json, "
                    "created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)",
                    (run_id, key, manifest_hash, _dump(manifest), now, now),
                )
                created_at = now
            else:
                stored = _load(row["manifest_json"], {})
                if (
                    str(row["workspace_key"]) != key
                    or str(row["manifest_hash"]) != manifest_hash
                    or stored != manifest
                ):
                    raise ImmutableRecordError(f"workspace_manifest_conflict:{run_id}")
                created_at = str(row["created_at"])
        return RunWorkspace(run_id, key, path, manifest_hash, created_at)

    def resolve(self, workspace: RunWorkspace, relative: str | Path = ".") -> Path:
        candidate = workspace.path / Path(relative)
        root = workspace.path.resolve()
        resolved = candidate.resolve(strict=False)
        if os.path.commonpath((str(root), str(resolved))) != str(root):
            raise ValueError("workspace_relative_path_escape")
        current = candidate
        while current != workspace.path.parent:
            if current.exists() and current.is_symlink():
                raise ValueError("workspace_relative_path_symlink")
            if current == workspace.path:
                break
            current = current.parent
        return candidate

    @staticmethod
    def environment(
        overlay: Mapping[str, Any] | None = None,
        *,
        credential_bindings: Mapping[str, Any] | None = None,
    ) -> dict[str, str]:
        environment = {key: value for key, value in os.environ.items() if key.upper() in SAFE_ENV_KEYS}
        for source in (overlay or {}, credential_bindings or {}):
            for key, value in source.items():
                name = str(key).strip()
                if not name or "=" in name or "\x00" in name:
                    raise ValueError("worker_environment_key_invalid")
                text = str(value)
                if "\x00" in text:
                    raise ValueError("worker_environment_value_invalid")
                environment[name] = text
        return environment


__all__ = ["RunWorkspace", "WorkspaceManager"]
