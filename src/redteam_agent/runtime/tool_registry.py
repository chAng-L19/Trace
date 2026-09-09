from __future__ import annotations

import fnmatch
import hashlib
import json
from dataclasses import dataclass
from typing import Any, Mapping, Sequence

from ..core import ToolCall, ToolPort, ToolResult
from .store_common import _load


@dataclass(frozen=True, slots=True)
class ToolVisibility:
    qualified_name: str
    visible: bool
    reason: str
    expanded: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "tool": self.qualified_name,
            "visible": self.visible,
            "reason": self.reason,
            "expanded": self.expanded,
        }


@dataclass(frozen=True, slots=True)
class ToolCatalog:
    revision: str
    tools: tuple[Any, ...]
    visibility: tuple[ToolVisibility, ...]
    expanded: bool
    estimated_prompt_bytes: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "revision": self.revision,
            "tools": [self._tool_dict(item) for item in self.tools],
            "visibility": [item.to_dict() for item in self.visibility],
            "expanded": self.expanded,
            "estimated_prompt_bytes": self.estimated_prompt_bytes,
        }

    @staticmethod
    def _tool_dict(tool: Any) -> dict[str, Any]:
        if hasattr(tool, "to_dict"):
            return dict(tool.to_dict())
        return {
            "qualified_name": str(getattr(tool, "qualified_name", "")),
            "name": str(getattr(tool, "name", "")),
            "server": str(getattr(tool, "server", "")),
            "description": str(getattr(tool, "description", "")),
            "input_schema": dict(getattr(tool, "input_schema", {}) or {}),
            "capabilities": list(getattr(tool, "capabilities", ()) or ()),
            "version": str(getattr(tool, "version", "unknown")),
            "schema_hash": str(getattr(tool, "schema_hash", "")),
            "side_effecting": bool(getattr(tool, "side_effecting", False)),
            "supports_reconcile": bool(getattr(tool, "supports_reconcile", False)),
            "metadata": dict(getattr(tool, "metadata", {}) or {}),
        }


class ToolVisibilityPolicy:
    def __init__(self, *, max_default_per_server: int = 6) -> None:
        self.max_default_per_server = max(1, int(max_default_per_server))

    @staticmethod
    def _matches_capability(tool: Any, capabilities: Sequence[str]) -> bool:
        offered = {str(item).casefold().replace("-", "_") for item in getattr(tool, "capabilities", ())}
        required = {str(item).casefold().replace("-", "_") for item in capabilities}
        return bool(offered & required)

    def select(
        self,
        tools: Sequence[Any],
        *,
        capabilities: Sequence[str] = (),
        expanded: bool = False,
    ) -> tuple[tuple[Any, ...], tuple[ToolVisibility, ...]]:
        if expanded:
            return tuple(tools), tuple(
                ToolVisibility(str(item.qualified_name), True, "explicit_expand", True)
                for item in tools
            )
        by_server: dict[str, int] = {}
        selected: list[Any] = []
        visibility: list[ToolVisibility] = []
        for tool in tools:
            qualified = str(getattr(tool, "qualified_name", ""))
            server = str(getattr(tool, "server", ""))
            source = str(getattr(tool, "source", "") or getattr(tool, "metadata", {}).get("source", ""))
            side_effecting = bool(getattr(tool, "side_effecting", False))
            matches = self._matches_capability(tool, capabilities)
            count = by_server.get(server, 0)
            if matches:
                reason = "capability_match"
            elif source in {"registered-adapter", "builtin"}:
                reason = "builtin_default"
            elif not side_effecting and count < self.max_default_per_server:
                reason = "read_only_default"
            else:
                visibility.append(ToolVisibility(qualified, False, "deferred_until_expand"))
                continue
            selected.append(tool)
            visibility.append(ToolVisibility(qualified, True, reason))
            by_server[server] = count + 1
        return tuple(selected), tuple(visibility)

    @staticmethod
    def profile_capabilities(profile: str) -> tuple[str, ...]:
        profiles = {
            "web": ("browser_automation", "page_fetch", "http_fingerprint"),
            "web-api": ("browser_automation", "page_fetch", "http_fingerprint"),
            "binary": ("binary_reverse", "decompile", "disassemble", "graph_analysis"),
            "reverse": ("binary_reverse", "decompile", "disassemble", "graph_analysis"),
            "cloud": ("cloud_inventory", "policy_simulation", "identity_validation"),
            "source": ("code_analysis", "source_inventory", "data_flow"),
            "code": ("code_analysis", "source_inventory", "data_flow"),
        }
        return profiles.get(profile.casefold().strip(), ())


class ToolRegistry(ToolPort):
    """Selected-tool projection over the existing ToolPort; execution stays delegated."""

    def __init__(
        self,
        delegate: ToolPort,
        *,
        store: Any | None = None,
        policy: ToolVisibilityPolicy | None = None,
    ) -> None:
        self.delegate = delegate
        self.store = store
        self.policy = policy or ToolVisibilityPolicy()
        self._last_revision: dict[str, str] = {}
        self._expanded: dict[str, tuple[str, ...]] = {}
        self._selected: dict[str, dict[str, Mapping[str, Any]]] = {}

    def capabilities(self) -> tuple[str, ...]:
        return tuple(getattr(self.delegate, "capabilities", lambda: ())())

    def discover(self) -> tuple[Any, ...]:
        return self.catalog().tools

    def discover_for(
        self,
        run_id: str,
        *,
        capabilities: Sequence[str] = (),
        profile: str = "",
    ) -> tuple[Any, ...]:
        return self.catalog(run_id, capabilities=capabilities, profile=profile).tools

    def catalog(
        self,
        run_id: str = "",
        *,
        capabilities: Sequence[str] = (),
        profile: str = "",
    ) -> ToolCatalog:
        tools = tuple(self.delegate.discover())
        persisted_patterns = self._expansion(run_id) if run_id else None
        if run_id and persisted_patterns is not None:
            self._expanded.setdefault(run_id, persisted_patterns)
        expanded = bool(run_id and (run_id in self._expanded or persisted_patterns is not None))
        effective_capabilities = tuple(
            dict.fromkeys((*capabilities, *self.policy.profile_capabilities(profile)))
        )
        selected, visibility = self.policy.select(
            tools,
            capabilities=effective_capabilities,
            expanded=expanded,
        )
        patterns = self._expanded.get(run_id, ()) if run_id else ()
        if expanded and patterns:
            matched = {
                item.qualified_name
                for item in selected
                if any(fnmatch.fnmatchcase(item.qualified_name, pattern) for pattern in patterns)
            }
            selected = tuple(
                item for item in selected
                if item.qualified_name in matched
            )
            visibility = tuple(
                item
                if item.qualified_name in matched
                else ToolVisibility(item.qualified_name, False, "selector_filtered", True)
                for item in visibility
            )
        revision = self._revision(tools, selected, visibility, expanded)
        estimated = len(json.dumps([ToolCatalog._tool_dict(item) for item in selected], ensure_ascii=False))
        selected_records = {
            item.qualified_name: self._tool_signature(item)
            for item in selected
        }
        if run_id:
            self._selected[run_id] = selected_records
        if run_id and self.store is not None and self._last_revision.get(run_id) != revision:
            self._last_revision[run_id] = revision
            self.store.append_event(
                run_id,
                "tool_catalog_selected",
                {
                    "revision": revision,
                    "expanded": expanded,
                    "required_capabilities": list(effective_capabilities),
                    "selected_tools": list(selected_records.values()),
                    "visibility": [item.to_dict() for item in visibility],
                    "estimated_prompt_bytes": estimated,
                },
            )
        return ToolCatalog(revision, selected, visibility, expanded, estimated)

    @staticmethod
    def prompt_definitions(catalog: ToolCatalog) -> tuple[dict[str, Any], ...]:
        definitions: list[dict[str, Any]] = []
        for item in catalog.tools:
            lines = str(getattr(item, "description", "") or "").splitlines()
            description = lines[0][:240] if lines else ""
            definitions.append(
                {
                    "type": "function",
                    "name": item.qualified_name,
                    "description": description,
                    "input_schema": dict(getattr(item, "input_schema", {}) or {}),
                }
            )
        return tuple(definitions)

    def expand(self, run_id: str, selectors: Sequence[str] = ()) -> ToolCatalog:
        if self.store is None:
            raise ValueError("tool_registry_store_required")
        values = (selectors,) if isinstance(selectors, str) else selectors
        patterns = tuple(str(item).strip() for item in values if str(item).strip())
        self._expanded[run_id] = patterns
        self.store.append_event(
            run_id,
            "tool_catalog_expanded",
            {"selectors": list(patterns), "mode": "all" if not patterns else "patterns"},
        )
        return self.catalog(run_id)

    def refresh(self, *, force: bool = False) -> ToolCatalog:
        refresher = getattr(self.delegate, "refresh", None)
        if callable(refresher):
            refresher(force=force)
        self._last_revision.clear()
        return self.catalog()

    def invoke(self, call: ToolCall) -> ToolResult:
        rejected = self._rejected_call(call)
        if rejected is not None:
            return rejected
        return self.delegate.invoke(call)

    def reconcile(self, call: ToolCall) -> ToolResult | None:
        rejected = self._rejected_call(call)
        if rejected is not None:
            return rejected
        return self.delegate.reconcile(call)

    def cancel(self, call_id: str) -> bool:
        return self.delegate.cancel(call_id)

    def _expansion(self, run_id: str) -> tuple[str, ...] | None:
        if self.store is None:
            return None
        expansion = None
        if hasattr(self.store, "connection"):
            with self.store.connection() as connection:
                rows = connection.execute(
                    "SELECT payload_json FROM operation_events "
                    "WHERE run_id=? AND event_type='tool_catalog_expanded' ORDER BY event_id",
                    (run_id,),
                ).fetchall()
            payloads = (_load(row["payload_json"], {}) for row in rows)
        else:
            payloads = (
                item["payload"]
                for item in self.store.events(run_id, limit=1000)
                if item["event_type"] == "tool_catalog_expanded"
            )
        for payload in payloads:
            if isinstance(payload, Mapping):
                raw = payload.get("selectors", [])
                expansion = tuple(str(value) for value in raw if str(value).strip())
        return expansion

    def _rejected_call(self, call: ToolCall) -> ToolResult | None:
        selected = self._selected.get(call.run_id) or self._persisted_selection(call.run_id)
        if selected is None:
            self.catalog(call.run_id)
            selected = self._selected.get(call.run_id, {})
        expected = selected.get(call.tool_name)
        if expected is None:
            return ToolResult(
                call_id=call.call_id,
                status="failed",
                tool_name=call.tool_name,
                error="tool_not_visible:expand_required",
            )
        current = next(
            (item for item in self.delegate.discover() if item.qualified_name == call.tool_name),
            None,
        )
        if current is None or self._tool_signature(current) != expected:
            return ToolResult(
                call_id=call.call_id,
                status="failed",
                tool_name=call.tool_name,
                error="tool_catalog_changed:refresh_required",
            )
        return None

    def _persisted_selection(self, run_id: str) -> dict[str, Mapping[str, Any]] | None:
        if self.store is None or not run_id:
            return None
        payload = None
        if hasattr(self.store, "connection"):
            with self.store.connection() as connection:
                row = connection.execute(
                    "SELECT payload_json FROM operation_events "
                    "WHERE run_id=? AND event_type='tool_catalog_selected' "
                    "ORDER BY event_id DESC LIMIT 1",
                    (run_id,),
                ).fetchone()
            payload = _load(row["payload_json"], {}) if row is not None else None
        else:
            events = tuple(
                item
                for item in self.store.events(run_id, limit=1000)
                if item["event_type"] == "tool_catalog_selected"
            )
            payload = events[-1]["payload"] if events else None
        if not isinstance(payload, Mapping):
            return None
        records = payload.get("selected_tools")
        if not isinstance(records, Sequence) or isinstance(records, (str, bytes)):
            return None
        selected = {
            str(item.get("name") or ""): dict(item)
            for item in records
            if isinstance(item, Mapping) and str(item.get("name") or "")
        }
        self._selected[run_id] = selected
        return selected

    @staticmethod
    def _tool_signature(tool: Any) -> dict[str, Any]:
        metadata = getattr(tool, "metadata", {}) or {}
        schema = dict(getattr(tool, "input_schema", {}) or {})
        schema_hash = str(getattr(tool, "schema_hash", "") or metadata.get("schema_hash", ""))
        if not schema_hash:
            schema_hash = hashlib.sha256(
                json.dumps(schema, sort_keys=True, separators=(",", ":")).encode()
            ).hexdigest()
        return {
            "name": str(getattr(tool, "qualified_name", "")),
            "schema_hash": schema_hash,
            "version": str(getattr(tool, "version", "unknown")),
            "source": str(getattr(tool, "source", "") or metadata.get("source", "")),
            "description_hash": hashlib.sha256(
                str(getattr(tool, "description", "")).encode()
            ).hexdigest(),
            "capabilities": list(getattr(tool, "capabilities", ()) or ()),
            "side_effecting": bool(getattr(tool, "side_effecting", False)),
            "supports_reconcile": bool(getattr(tool, "supports_reconcile", False)),
        }

    @staticmethod
    def _revision(
        all_tools: Sequence[Any],
        selected_tools: Sequence[Any],
        visibility: Sequence[ToolVisibility],
        expanded: bool,
    ) -> str:
        payload = {
            "expanded": expanded,
            "tools": [
                ToolRegistry._tool_signature(item)
                for item in all_tools
            ],
            "selected": [item.qualified_name for item in selected_tools],
            "visibility": [item.to_dict() for item in visibility],
        }
        return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


__all__ = ["ToolCatalog", "ToolRegistry", "ToolVisibility", "ToolVisibilityPolicy"]
