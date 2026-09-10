from __future__ import annotations

import fnmatch
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence


RESOURCE_SCHEMA_VERSION = 1
DEFAULT_RESOURCE_BYTES = 64 * 1024
DEFAULT_RESOURCE_TOKENS = 4096
MAX_RESOURCE_FILES = 256


@dataclass(frozen=True, slots=True)
class ResourceDescriptor:
    resource_id: str
    kind: str
    source: str
    priority: int
    content_hash: str
    byte_count: int
    token_cost: int
    content: str

    def to_dict(self, *, include_content: bool = False) -> dict[str, Any]:
        payload = {
            "schema_version": RESOURCE_SCHEMA_VERSION,
            "resource_id": self.resource_id,
            "kind": self.kind,
            "source": self.source,
            "priority": self.priority,
            "content_hash": self.content_hash,
            "byte_count": self.byte_count,
            "token_cost": self.token_cost,
        }
        if include_content:
            payload["content"] = self.content
        return payload


@dataclass(frozen=True, slots=True)
class ResourceIssue:
    source: str
    error: str

    def to_dict(self) -> dict[str, str]:
        return {"source": self.source, "error": self.error}


@dataclass(frozen=True, slots=True)
class ResourceIndex:
    roots: tuple[str, ...]
    resources: tuple[ResourceDescriptor, ...]
    issues: tuple[ResourceIssue, ...]
    index_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": RESOURCE_SCHEMA_VERSION,
            "roots": list(self.roots),
            "resources": [item.to_dict() for item in self.resources],
            "issues": [item.to_dict() for item in self.issues],
            "index_hash": self.index_hash,
        }


@dataclass(frozen=True, slots=True)
class ResourceSelection:
    index_hash: str
    selected: tuple[ResourceDescriptor, ...]
    omitted: tuple[str, ...]
    disabled: tuple[str, ...]
    unmatched: tuple[str, ...]
    token_cost: int
    selection_hash: str

    @property
    def resource_ids(self) -> tuple[str, ...]:
        return tuple(item.resource_id for item in self.selected)

    def prompt_projection(self) -> dict[str, Any]:
        return {
            "selection_hash": self.selection_hash,
            "resources": [item.to_dict(include_content=True) for item in self.selected],
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": RESOURCE_SCHEMA_VERSION,
            "index_hash": self.index_hash,
            "selection_hash": self.selection_hash,
            "resource_ids": list(self.resource_ids),
            "omitted": list(self.omitted),
            "disabled": list(self.disabled),
            "unmatched": list(self.unmatched),
            "token_cost": self.token_cost,
        }


def resource_context_projection(
    selection: ResourceSelection,
    fixed_projection: list[dict[str, Any]],
    *,
    stable_prefix: bool,
) -> list[dict[str, Any]]:
    if not selection.selected:
        return fixed_projection
    resource_context = {"resource_context": selection.prompt_projection()}
    if stable_prefix:
        return [*fixed_projection, {"role": "system", "content": resource_context}]
    fixed_projection[0]["content"]["resource_context"] = selection.prompt_projection()
    return fixed_projection


def resource_context_metadata(selection: ResourceSelection) -> dict[str, Any]:
    return {
        "resource_index_hash": selection.index_hash,
        "resource_selection_hash": selection.selection_hash,
        "resource_ids": selection.resource_ids,
        "resource_tokens": selection.token_cost,
    }


class ResourceResolver:
    """Deterministic, read-only resource index and selection boundary."""

    _known_names = {
        "agents.md",
        "trace.md",
        "context.md",
        "mcp-instructions.md",
        "mcp_instructions.md",
    }
    _known_dirs = {
        ".trace",
        "skills",
        "capability-packs",
        "capability_packs",
        "mcp-instructions",
        "mcp_instructions",
    }

    def __init__(
        self,
        *,
        max_resource_bytes: int = DEFAULT_RESOURCE_BYTES,
        max_resource_tokens: int = DEFAULT_RESOURCE_TOKENS,
    ) -> None:
        self.max_resource_bytes = max(1024, int(max_resource_bytes))
        self.max_resource_tokens = max(1, int(max_resource_tokens))

    def index(self, roots: Iterable[Path | str]) -> ResourceIndex:
        resolved_root_paths = tuple(
            sorted({self._resolve_root(item) for item in roots}, key=str)
        )
        files: list[Path] = []
        issues: list[ResourceIssue] = []
        for root in resolved_root_paths:
            if not root.exists():
                issues.append(ResourceIssue(str(root), "resource_root_missing"))
                continue
            if root.is_symlink():
                issues.append(ResourceIssue(str(root), "resource_root_symlink"))
                continue
            if root.is_file():
                files.append(root)
                continue
            files.extend(self._discover(root, issues))
        descriptors: list[ResourceDescriptor] = []
        for path in sorted(set(files), key=lambda item: str(item).casefold()):
            descriptor = self._read(path, resolved_root_paths, issues)
            if descriptor is not None:
                descriptors.append(descriptor)
        descriptors.sort(key=lambda item: (-item.priority, item.source.casefold(), item.resource_id))
        index_payload = {
            "schema_version": RESOURCE_SCHEMA_VERSION,
            "roots": [str(item) for item in resolved_root_paths],
            "resources": [item.to_dict() for item in descriptors],
            "issues": [item.to_dict() for item in issues],
        }
        index_hash = hashlib.sha256(self._encode(index_payload)).hexdigest()
        return ResourceIndex(
            roots=tuple(str(item) for item in resolved_root_paths),
            resources=tuple(descriptors),
            issues=tuple(issues),
            index_hash=index_hash,
        )

    def select(
        self,
        index: ResourceIndex,
        *,
        requested: Sequence[str] = (),
        disabled: Sequence[str] = (),
        token_budget: int = DEFAULT_RESOURCE_TOKENS,
    ) -> ResourceSelection:
        patterns = tuple(str(item).strip() for item in requested if str(item).strip())
        disabled_patterns = tuple(str(item).strip() for item in disabled if str(item).strip())
        budget = max(0, min(self.max_resource_tokens, int(token_budget)))
        selected: list[ResourceDescriptor] = []
        omitted: list[str] = []
        disabled_ids: list[str] = []
        unmatched = [pattern for pattern in patterns if not any(self._matches(item, pattern) for item in index.resources)]
        for item in index.resources:
            explicitly_requested = bool(patterns) and any(self._matches(item, pattern) for pattern in patterns)
            default_selected = not patterns and item.kind in {"agents", "project_context"}
            if not (explicitly_requested or default_selected):
                continue
            if any(self._matches(item, pattern) for pattern in disabled_patterns):
                disabled_ids.append(item.resource_id)
                continue
            if item.token_cost > budget - sum(entry.token_cost for entry in selected):
                omitted.append(item.resource_id)
                continue
            selected.append(item)
        selected_tuple = tuple(selected)
        projection = {
            "index_hash": index.index_hash,
            "resource_ids": [item.resource_id for item in selected_tuple],
            "content_hashes": [item.content_hash for item in selected_tuple],
            "omitted": omitted,
            "disabled": disabled_ids,
            "unmatched": unmatched,
            "token_cost": sum(item.token_cost for item in selected_tuple),
        }
        selection_hash = hashlib.sha256(self._encode(projection)).hexdigest()
        return ResourceSelection(
            index_hash=index.index_hash,
            selected=selected_tuple,
            omitted=tuple(omitted),
            disabled=tuple(disabled_ids),
            unmatched=tuple(unmatched),
            token_cost=int(projection["token_cost"]),
            selection_hash=selection_hash,
        )

    def _discover(self, root: Path, issues: list[ResourceIssue]) -> list[Path]:
        discovered: list[Path] = []
        for current, directories, names in os.walk(root, followlinks=False):
            current_path = Path(current)
            directories[:] = [item for item in directories if not (current_path / item).is_symlink()]
            depth = len(current_path.relative_to(root).parts)
            if depth > 3:
                directories[:] = []
                continue
            for name in sorted(names, key=str.casefold):
                path = current_path / name
                if path.is_symlink() or not path.is_file():
                    continue
                lower = name.casefold()
                relative_parts = {part.casefold() for part in path.relative_to(root).parts[:-1]}
                suffix = path.suffix.casefold()
                if lower in self._known_names or relative_parts & self._known_dirs:
                    if suffix in {".md", ".markdown", ".txt", ".toml", ".yaml", ".yml"}:
                        discovered.append(path)
        if len(discovered) > MAX_RESOURCE_FILES:
            issues.append(ResourceIssue(str(root), "resource_file_limit_exceeded"))
            return discovered[:MAX_RESOURCE_FILES]
        return discovered

    def _read(
        self,
        path: Path,
        roots: Sequence[Path | str],
        issues: list[ResourceIssue],
    ) -> ResourceDescriptor | None:
        try:
            raw = path.read_bytes()
        except OSError as exc:
            issues.append(ResourceIssue(str(path), f"resource_read_error:{type(exc).__name__}"))
            return None
        if len(raw) > self.max_resource_bytes:
            issues.append(ResourceIssue(str(path), "resource_byte_limit_exceeded"))
            return None
        try:
            content = raw.decode("utf-8-sig")
        except UnicodeDecodeError:
            issues.append(ResourceIssue(str(path), "resource_encoding_invalid"))
            return None
        source = str(path.resolve())
        kind = self._kind(path)
        relative = self._relative_source(path, roots)
        resource_id = f"{kind}:{relative}"
        token_cost = max(1, (len(raw) + 3) // 4) if raw else 0
        return ResourceDescriptor(
            resource_id=resource_id,
            kind=kind,
            source=source,
            priority=self._priority(kind),
            content_hash=hashlib.sha256(raw).hexdigest(),
            byte_count=len(raw),
            token_cost=token_cost,
            content=content,
        )

    @staticmethod
    def _resolve_root(root: Path | str) -> Path:
        # Keep the final path absolute without resolving a symlink. `index()`
        # must be able to reject a symlink root before traversing it.
        return Path(root).expanduser().absolute()

    @staticmethod
    def _encode(value: Mapping[str, Any]) -> bytes:
        return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")

    @staticmethod
    def _relative_source(path: Path, roots: Sequence[Path | str]) -> str:
        for root in roots:
            root_path = Path(root)
            try:
                return path.resolve().relative_to(root_path).as_posix() or path.name
            except ValueError:
                continue
        return path.name

    @classmethod
    def _kind(cls, path: Path) -> str:
        lower = path.name.casefold()
        parts = {part.casefold() for part in path.parts}
        if lower == "agents.md":
            return "agents"
        if "skill" in lower or "skills" in parts:
            return "skill"
        if "capability" in lower or "capability-packs" in parts or "capability_packs" in parts:
            return "capability_pack"
        if "mcp" in lower or "mcp-instructions" in parts or "mcp_instructions" in parts:
            return "mcp_instruction"
        return "project_context"

    @staticmethod
    def _priority(kind: str) -> int:
        return {
            "agents": 100,
            "project_context": 80,
            "capability_pack": 60,
            "mcp_instruction": 50,
            "skill": 40,
        }.get(kind, 10)

    @staticmethod
    def _matches(item: ResourceDescriptor, pattern: str) -> bool:
        candidates = (item.resource_id, item.source, Path(item.source).name, item.kind)
        return any(fnmatch.fnmatchcase(candidate, pattern) for candidate in candidates)


__all__ = [
    "DEFAULT_RESOURCE_BYTES",
    "DEFAULT_RESOURCE_TOKENS",
    "RESOURCE_SCHEMA_VERSION",
    "ResourceDescriptor",
    "ResourceIndex",
    "ResourceIssue",
    "ResourceResolver",
    "ResourceSelection",
    "resource_context_metadata",
    "resource_context_projection",
]
