from __future__ import annotations

"""Small durable control-plane services used by the native Trace web UI."""

import hashlib
import hmac
import json
import os
import platform
import secrets
import sqlite3
import base64
import threading
import time
import tomllib
import urllib.parse
import urllib.request
from dataclasses import asdict
from pathlib import Path
from typing import Any, Mapping

from ..application.resources import ResourceResolver, builtin_resource_root
from ..runtime.skill_config import configured_resource_index
from ..runtime.mcp_config import PUBLIC_MCP_PRESETS, parse_mcp_server_specs
from ..providers.openai_protocol import token_limit


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _toml_string(value: Any) -> str:
    return json.dumps(str(value or ""), ensure_ascii=False)


def _toml_array(values: Any) -> str:
    return "[" + ", ".join(_toml_string(item) for item in (values or ())) + "]"


def _local_provider(base_url: str) -> bool:
    import ipaddress
    from urllib.parse import urlsplit
    hostname = urlsplit(base_url).hostname or ""
    try:
        return ipaddress.ip_address(hostname).is_loopback
    except ValueError:
        return hostname.casefold() == "localhost"


def _import_skill_source(source: str) -> tuple[str, str]:
    """Read one external Markdown skill without adding a package dependency."""
    source = str(source or "").strip()
    if not source:
        raise ValueError("skill_import_source_required")
    if source.casefold().startswith(("http://", "https://")):
        request = urllib.request.Request(source, headers={"User-Agent": "Trace-Skill-Importer/1"})
        with urllib.request.urlopen(request, timeout=20) as response:
            raw = response.read(64 * 1024 + 1)
        url_name = Path(urllib.parse.unquote(urllib.parse.urlsplit(source).path)).name
        name = Path(url_name).stem if url_name else "external"
    else:
        try:
            path = Path(source).expanduser().resolve(strict=True)
        except FileNotFoundError:
            raise ValueError("skill_import_file_not_found") from None
        if path.is_dir():
            candidates = [path / "SKILL.md", path / "skill.md"]
            path = next((candidate for candidate in candidates if candidate.is_file()), None)
            if path is None:
                raise ValueError("skill_import_file_not_found")
        try:
            raw = path.read_bytes()
        except OSError as exc:
            raise ValueError(f"skill_import_read_failed:{exc.strerror or type(exc).__name__}") from None
        name = path.parent.name if path.name.casefold() == "skill.md" else path.stem
    if len(raw) > 64 * 1024:
        raise ValueError("skill_import_too_large")
    try:
        content = raw.decode("utf-8-sig").replace("\r\n", "\n").replace("\r", "\n")
    except UnicodeDecodeError:
        raise ValueError("skill_import_encoding_invalid") from None
    name = name.strip() or "external"
    return name, content


class ControlPlane:
    """Durable, redacted control-plane settings."""

    def __init__(self, store: Any, root: Path) -> None:
        self.store = store
        self.root = root
        self._lock = threading.RLock()
        self._secrets: dict[str, str] = {}
        self._managed_mcp = root / "managed-mcp.toml"
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_providers (
                    provider_id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT NOT NULL,
                    model TEXT NOT NULL, api_key_env TEXT NOT NULL DEFAULT '', api_key TEXT NOT NULL DEFAULT '',
                    timeout_seconds REAL NOT NULL, max_context_tokens INTEGER NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1, active INTEGER NOT NULL DEFAULT 0,
                    created_at TEXT NOT NULL, updated_at TEXT NOT NULL
                )"""
            )
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_skills (
                    skill_id TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 1,
                    config_json TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL
                )"""
            )
            columns = {row["name"] for row in connection.execute("PRAGMA table_info(trace_providers)")}
            for name, declaration in {
                "provider": "TEXT NOT NULL DEFAULT 'openai-compatible'",
                "max_output_tokens": "INTEGER NOT NULL DEFAULT 0",
                "reasoning_effort": "TEXT NOT NULL DEFAULT ''",
                "thinking_type": "TEXT NOT NULL DEFAULT ''",
                "thinking_budget_tokens": "INTEGER NOT NULL DEFAULT 0",
                "api_key": "TEXT NOT NULL DEFAULT ''",
            }.items():
                if name not in columns:
                    connection.execute(f"ALTER TABLE trace_providers ADD COLUMN {name} {declaration}")
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_mcp_servers (
                    server_id TEXT PRIMARY KEY, spec_json TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL
                )"""
            )
            for table, additions in {
                "trace_skills": {"deleted": "INTEGER NOT NULL DEFAULT 0", "content_path": "TEXT NOT NULL DEFAULT ''"},
                "trace_mcp_servers": {"deleted": "INTEGER NOT NULL DEFAULT 0"},
            }.items():
                columns = {row["name"] for row in connection.execute(f"PRAGMA table_info({table})")}
                for name, declaration in additions.items():
                    if name not in columns:
                        connection.execute(f"ALTER TABLE {table} ADD COLUMN {name} {declaration}")
        from .web_accounts import Accounts
        self.accounts = Accounts(self.store)
        self._migrate_legacy_secret_storage()

    @staticmethod
    def _now() -> str:
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    def _migrate_legacy_secret_storage(self) -> None:
        """Upgrade the previous encrypted store, then keep credentials plaintext.

        This is intentionally one-way compatibility code. New installations do
        not create a key file or a secret table; Provider keys stay in
        ``trace_providers.api_key`` and MCP values stay in their JSON spec.
        """
        key_path = self.root / "trace-secrets.key"
        with self.store.transaction(immediate=True) as connection:
            has_table = connection.execute(
                "SELECT 1 FROM sqlite_master WHERE type='table' AND name='trace_secrets'"
            ).fetchone() is not None
            if not has_table:
                self._secrets = {
                    str(row["provider_id"]): str(row["api_key"] or "")
                    for row in connection.execute("SELECT provider_id,api_key FROM trace_providers")
                    if str(row["api_key"] or "")
                }
                if key_path.exists():
                    try:
                        key_path.unlink()
                    except OSError:
                        pass
                return
            secret_rows = {
                str(row["secret_name"]): str(row["ciphertext"])
                for row in connection.execute("SELECT secret_name,ciphertext FROM trace_secrets")
            }
            values: dict[str, str] = {}
            if secret_rows:
                key = key_path.read_bytes()
                if len(key) != 32:
                    raise ValueError("legacy_secret_key_invalid")
                for name, ciphertext in secret_rows.items():
                    raw = base64.urlsafe_b64decode(ciphertext.encode("ascii"))
                    nonce, tag, cipher = raw[:16], raw[16:48], raw[48:]
                    if not hmac.compare_digest(tag, hmac.new(key, nonce + cipher, hashlib.sha256).digest()):
                        raise ValueError("legacy_secret_ciphertext_invalid:" + name)
                    stream = b"".join(hashlib.sha256(key + nonce + n.to_bytes(4, "big")).digest() for n in range((len(cipher) + 31) // 32))[:len(cipher)]
                    values[name] = bytes(a ^ b for a, b in zip(cipher, stream)).decode("utf-8")
            for row in connection.execute("SELECT provider_id,api_key,api_key_env FROM trace_providers"):
                provider_id = str(row["provider_id"])
                value = str(row["api_key"] or "") or values.get("provider:" + provider_id, "")
                api_key_env = str(row["api_key_env"] or "")
                if not value and api_key_env.startswith("sk-"):
                    value, api_key_env = api_key_env, ""
                if value != str(row["api_key"] or "") or api_key_env != str(row["api_key_env"] or ""):
                    connection.execute("UPDATE trace_providers SET api_key=?,api_key_env=?,updated_at=? WHERE provider_id=?", (value, api_key_env, self._now(), provider_id))
            for row in connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers"):
                spec = json.loads(str(row["spec_json"]))
                changed = False
                for field in ("env", "headers"):
                    source = dict(spec.get(field) or {})
                    for name, value in list(source.items()):
                        text = str(value)
                        if text.startswith("${TRACE_MCP_SECRET_") and text.endswith("}"):
                            source[str(name)] = values[text[2:-1]]
                            changed = True
                    spec[field] = source
                if changed:
                    connection.execute("UPDATE trace_mcp_servers SET spec_json=?,updated_at=? WHERE server_id=?", (_json(spec), self._now(), str(row["server_id"])))
            connection.execute("DROP TABLE trace_secrets")
        try:
            key_path.unlink(missing_ok=True)
        except OSError:
            pass
        with self.store.connection() as connection:
            self._secrets = {
                str(row["provider_id"]): str(row["api_key"] or "")
                for row in connection.execute("SELECT provider_id,api_key FROM trace_providers")
                if str(row["api_key"] or "")
            }

    @staticmethod
    def _provider_payload(row: Mapping[str, Any], secret: bool = False) -> dict[str, Any]:
        return {
            "provider_id": str(row["provider_id"]),
            "name": str(row["name"]),
            "base_url": str(row["base_url"]),
            "model": str(row["model"]),
            "api_key_env": str(row["api_key_env"]),
            "api_key_set": bool(secret),
            "configured": True,
            "ready": bool(row["enabled"]) and (bool(secret) or _local_provider(str(row["base_url"]))),
            "timeout_seconds": float(row["timeout_seconds"]),
            "max_context_tokens": int(row["max_context_tokens"]),
            "provider": str(row["provider"]),
            "max_output_tokens": int(row["max_output_tokens"]),
            "reasoning_effort": str(row["reasoning_effort"]),
            "thinking_type": str(row["thinking_type"]),
            "thinking_budget_tokens": int(row["thinking_budget_tokens"]),
            "enabled": bool(row["enabled"]),
            "active": bool(row["active"]),
            "updated_at": str(row["updated_at"]),
        }

    def providers(self) -> list[dict[str, Any]]:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT * FROM trace_providers ORDER BY name, provider_id").fetchall()
        with self._lock:
            return [
                self._provider_payload(
                    row,
                    bool(str(row["api_key"] or ""))
                    or bool(str(row["api_key_env"]) and os.environ.get(str(row["api_key_env"]))),
                )
                for row in rows
            ]

    def provider(self, provider_id: str) -> dict[str, Any]:
        for item in self.providers():
            if item["provider_id"] == provider_id:
                return item
        raise KeyError(f"provider_not_found:{provider_id}")

    def provider_secret(self, provider_id: str, *, include_environment: bool = True) -> str:
        item = self.provider(provider_id)
        environment = os.environ.get(item["api_key_env"], "") if include_environment and item["api_key_env"] else ""
        with self.store.connection() as connection:
            row = connection.execute("SELECT api_key FROM trace_providers WHERE provider_id=?", (provider_id,)).fetchone()
        return environment or (str(row["api_key"] or "") if row else "")

    def provider_secrets(self) -> list[str]:
        with self.store.connection() as connection:
            return [str(row["api_key"] or "") for row in connection.execute("SELECT api_key FROM trace_providers")]

    def save_provider(self, payload: Mapping[str, Any], *, on_active: Any = None) -> dict[str, Any]:
        provider_id = str(payload.get("provider_id") or payload.get("id") or secrets.token_hex(8)).strip()
        name = str(payload.get("name") or provider_id).strip()
        kind = str(payload.get("provider") or "openai-compatible").strip().lower()
        base_url = str(payload.get("base_url") or ("https://api.anthropic.com/v1" if kind == "anthropic" else "https://api.openai.com/v1")).strip().rstrip("/")
        model = str(payload.get("model") or "").strip()
        api_key_env = str(payload.get("api_key_env") or "").strip()
        if not provider_id or len(provider_id) > 100 or not name or len(name) > 160:
            raise ValueError("provider_identity_invalid")
        if not base_url.startswith(("http://", "https://")):
            raise ValueError("provider_base_url_invalid")
        if not model or len(model) > 200:
            raise ValueError("provider_model_required")
        try:
            timeout = float(payload.get("timeout_seconds", 120))
            context = int(payload.get("max_context_tokens", 128000))
            maximum = token_limit(payload.get("max_output_tokens", 0))
            thinking_budget = token_limit(payload.get("thinking_budget_tokens", 0))
        except (TypeError, ValueError, OverflowError):
            raise ValueError("provider_limits_invalid") from None
        if not 0 < timeout <= 3600 or not 1 <= context <= 10_000_000:
            raise ValueError("provider_limits_invalid")
        from ..application.bootstrap import resolve_provider
        effort, thinking_type = str(payload.get("reasoning_effort") or ""), str(payload.get("thinking_type") or "")
        try:
            resolve_provider(None, (), {"provider": kind, "base_url": base_url, "model": model,
                "timeout_seconds": timeout, "max_context_tokens": context, "max_output_tokens": maximum,
                "reasoning_effort": effort, "thinking_type": thinking_type, "thinking_budget_tokens": thinking_budget}, environ={})
        except (TypeError, ValueError) as exc:
            raise ValueError(str(exc)) from None
        secret = str(payload.get("api_key") or "")
        existing_item = next((item for item in self.providers() if item["provider_id"] == provider_id), None)
        clear_secret = bool(payload.get("clear_api_key")) or bool(existing_item and (
            existing_item["base_url"] != base_url
            or existing_item["provider"] != kind
            or existing_item["api_key_env"] != api_key_env
        ) and not secret)
        candidate_secret = secret or ("" if clear_secret else (existing_item and self._secrets.get(provider_id, "") or ""))
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            existing = connection.execute("SELECT created_at FROM trace_providers WHERE provider_id=?", (provider_id,)).fetchone()
            connection.execute(
                """INSERT INTO trace_providers(provider_id,name,base_url,model,api_key_env,api_key,timeout_seconds,max_context_tokens,enabled,active,created_at,updated_at)
                   VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
                   ON CONFLICT(provider_id) DO UPDATE SET name=excluded.name,base_url=excluded.base_url,model=excluded.model,
                   api_key_env=excluded.api_key_env,api_key=excluded.api_key,timeout_seconds=excluded.timeout_seconds,max_context_tokens=excluded.max_context_tokens,
                   enabled=excluded.enabled,updated_at=excluded.updated_at""",
                (provider_id, name, base_url, model, api_key_env, candidate_secret, timeout, context, int(payload.get("enabled", True) is not False), 0, str(existing["created_at"]) if existing else now, now),
            )
            connection.execute(
                "UPDATE trace_providers SET provider=?,max_output_tokens=?,reasoning_effort=?,thinking_type=?,thinking_budget_tokens=? WHERE provider_id=?",
                (kind, maximum, effort, thinking_type, thinking_budget, provider_id),
            )
            row = connection.execute("SELECT * FROM trace_providers WHERE provider_id=?", (provider_id,)).fetchone()
            saved = self._provider_payload(row, bool(candidate_secret or (api_key_env and os.environ.get(api_key_env))))
            if saved["active"] and on_active is not None:
                on_active(saved, api_key=candidate_secret)
        with self._lock:
            if candidate_secret:
                self._secrets[provider_id] = candidate_secret
            else:
                self._secrets.pop(provider_id, None)
        return saved

    def test_provider(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        """Explicit, read-only model check; never called automatically on save."""
        from .provider_check import test_provider

        return test_provider(self, payload)

    def activate_provider(self, provider_id: str) -> dict[str, Any]:
        item = self.provider(provider_id)
        if not item["enabled"]:
            raise ValueError("provider_disabled")
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            connection.execute("UPDATE trace_providers SET active=0, updated_at=?", (now,))
            connection.execute("UPDATE trace_providers SET active=1, updated_at=? WHERE provider_id=?", (now, provider_id))
        return self.provider(provider_id)

    def delete_provider(self, provider_id: str) -> None:
        with self.store.transaction(immediate=True) as connection:
            cursor = connection.execute("DELETE FROM trace_providers WHERE provider_id=?", (provider_id,))
        if cursor.rowcount != 1:
            raise KeyError(f"provider_not_found:{provider_id}")
        with self._lock:
            self._secrets.pop(provider_id, None)

    def skills(self) -> list[dict[str, Any]]:
        index = configured_resource_index(self.store, ResourceResolver(), (self.root, builtin_resource_root()))
        with self.store.connection() as connection:
            rows = {str(row["skill_id"]): row for row in connection.execute("SELECT * FROM trace_skills")}
        result = []
        for item in index.resources:
            if item.kind != "skill":
                continue
            row = rows.get(item.resource_id)
            config = json.loads(str(row["config_json"])) if row else {}
            result.append({**item.to_dict(), "enabled": bool(row["enabled"]) if row else True, "config": config})
        return result

    def skill(self, skill_id: str) -> dict[str, Any]:
        for item in self.skills():
            if item["resource_id"] == skill_id:
                return {**item, "content": Path(item["source"]).read_text(encoding="utf-8-sig")}
        raise KeyError(f"skill_not_found:{skill_id}")

    def save_skill(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        skill_id = str(payload.get("resource_id") or payload.get("skill_id") or "").strip()
        if not skill_id or len(skill_id) > 300:
            raise ValueError("skill_id_invalid")
        content = str(payload["content"])
        digest = hashlib.sha256(content.encode("utf-8")).hexdigest()
        path = self.root / "skill-content" / f"{digest}.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            path.write_text(content, encoding="utf-8")
        indexed = ResourceResolver().index((path,))
        if not indexed.resources:
            raise ValueError(indexed.issues[0].error)
        with self.store.transaction(immediate=True) as connection:
            existing = connection.execute("SELECT enabled,config_json FROM trace_skills WHERE skill_id=? AND deleted=0", (skill_id,)).fetchone()
            enabled = payload.get("enabled", bool(existing["enabled"]) if existing else True)
            config = payload.get("config", json.loads(existing["config_json"]) if existing else {})
            connection.execute(
                "INSERT INTO trace_skills(skill_id,enabled,config_json,updated_at,deleted,content_path) VALUES(?,?,?,?,0,?) "
                "ON CONFLICT(skill_id) DO UPDATE SET enabled=excluded.enabled,config_json=excluded.config_json,"
                "updated_at=excluded.updated_at,deleted=0,content_path=excluded.content_path",
                (skill_id, int(enabled is not False), _json(dict(config or {})), self._now(), str(path)),
            )
        return self.skill(skill_id)

    def import_skill(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        if "content" in payload:
            content = str(payload["content"]).lstrip("\ufeff").replace("\r\n", "\n").replace("\r", "\n")
            name = Path(str(payload.get("file_name") or "external.md")).stem or "external"
            if len(content.encode("utf-8")) > 64 * 1024:
                raise ValueError("skill_import_too_large")
        else:
            name, content = _import_skill_source(str(payload.get("source") or ""))
        skill_id = str(payload.get("resource_id") or payload.get("skill_id") or f"skill:external/{name}").strip()
        config = payload.get("config") if isinstance(payload.get("config"), Mapping) else {}
        return self.save_skill({
            "resource_id": skill_id,
            "content": content,
            "enabled": payload.get("enabled", True),
            "config": config,
        })

    def delete_skill(self, skill_id: str) -> None:
        self.skill(skill_id)
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO trace_skills(skill_id,enabled,config_json,updated_at,deleted) VALUES(?,0,'{}',?,1) "
                "ON CONFLICT(skill_id) DO UPDATE SET enabled=0,deleted=1,updated_at=excluded.updated_at",
                (skill_id, self._now()),
            )

    def set_skill(self, skill_id: str, *, enabled: bool, config: Mapping[str, Any] | None = None) -> dict[str, Any]:
        if not skill_id or len(skill_id) > 300:
            raise ValueError("skill_id_invalid")
        existing = self.skill(skill_id)
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO trace_skills(skill_id,enabled,config_json,updated_at) VALUES(?,?,?,?) ON CONFLICT(skill_id) DO UPDATE SET enabled=excluded.enabled,config_json=excluded.config_json,updated_at=excluded.updated_at",
                (skill_id, int(enabled), _json(dict(existing["config"] if config is None else config)), now),
            )
        return next(item for item in self.skills() if item["resource_id"] == skill_id)

    def mcp_servers(self) -> list[dict[str, Any]]:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT * FROM trace_mcp_servers WHERE deleted=0 ORDER BY server_id").fetchall()
        statuses = getattr(self.store, "_trace_mcp_statuses", {})
        result = []
        for row in rows:
            spec = json.loads(str(row["spec_json"]))
            spec.pop("env", None)
            spec.pop("headers", None)
            result.append({"server_id": row["server_id"], **spec, "enabled": bool(row["enabled"]), "status": statuses.get(str(row["server_id"]), {"status": "configured"})})
        return result

    def deleted_mcp_ids(self) -> set[str]:
        with self.store.connection() as connection:
            return {str(row[0]) for row in connection.execute("SELECT server_id FROM trace_mcp_servers WHERE deleted=1")}

    def mcp_source_spec(self, server_id: str, paths: Any) -> dict[str, Any]:
        for path in paths:
            path = Path(path)
            if not path.is_file():
                continue
            config = tomllib.loads(path.read_text(encoding="utf-8-sig"))
            for spec in parse_mcp_server_specs(path, config):
                if spec.name == server_id:
                    payload = asdict(spec)
                    payload["args"] = list(spec.args)
                    payload["cwd"] = str(spec.cwd or "")
                    return payload
        if server_id in PUBLIC_MCP_PRESETS:
            return dict(PUBLIC_MCP_PRESETS[server_id])
        raise KeyError(f"mcp_server_not_found:{server_id}")

    def set_mcp(self, server_id: str, *, enabled: bool, paths: Any = ()) -> dict[str, Any]:
        with self.store.connection() as connection:
            row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=? AND deleted=0", (server_id,)).fetchone()
        if row is not None:
            spec = json.loads(row[0])
        else:
            spec = self.mcp_source_spec(server_id, paths)
        return self.save_mcp({**spec, "server_id": server_id, "enabled": enabled})

    def mcp_secret_bindings(self) -> dict[str, dict[str, str]]:
        # Plain values are already in the spec; references resolve from the environment.
        return {}

    def save_mcp(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        server_id = str(payload.get("server_id") or payload.get("name") or "").strip()
        if not server_id or len(server_id) > 100:
            raise ValueError("mcp_server_id_invalid")
        transport = str(payload.get("transport") or ("stdio" if payload.get("command") else "http")).casefold()
        if transport not in {"stdio", "http"}:
            raise ValueError("mcp_transport_invalid")
        scope = str(payload.get("scope") or "shared").casefold()
        if scope not in {"shared", "run"}:
            raise ValueError("mcp_scope_invalid")
        if (transport == "stdio" and not payload.get("command")) or (transport == "http" and not payload.get("url")):
            raise ValueError("mcp_endpoint_required")
        with self._lock:
            with self.store.transaction(immediate=True) as connection:
                row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=?", (server_id,)).fetchone()
                existing_spec = json.loads(row["spec_json"]) if row is not None else {}
                spec = {**existing_spec,
                    **{key: payload[key] for key in ("startup_timeout_seconds", "tool_timeout_seconds",
                        "include_tools", "exclude_tools", "read_only_tools", "write_tools", "read_only") if key in payload},
                    "transport": transport,
                    "preset": str(payload.get("preset") or ""),
                    "scope": scope,
                    "command": str(payload.get("command") or ""),
                    "args": [str(x) for x in payload.get("args", [])],
                    "cwd": str(payload.get("cwd") or ""),
                    "url": str(payload.get("url") or ""),
                    "enabled": payload.get("enabled") is not False,
                }
                # Omitted fields preserve existing values; an explicit empty
                # mapping clears them in the same transaction as the settings.
                for field in ("env", "headers"):
                    source = dict((payload[field] if field in payload else existing_spec.get(field)) or {})
                    spec[field] = {str(key): str(value) for key, value in source.items()}
                connection.execute(
                    "INSERT INTO trace_mcp_servers(server_id,spec_json,enabled,updated_at) VALUES(?,?,?,?) ON CONFLICT(server_id) DO UPDATE SET spec_json=excluded.spec_json,enabled=excluded.enabled,deleted=0,updated_at=excluded.updated_at",
                    (server_id, _json(spec), int(spec["enabled"]), self._now()),
                )
            return next(item for item in self.mcp_servers() if item["server_id"] == server_id)

    def delete_mcp(self, server_id: str, spec: Mapping[str, Any] | None = None) -> None:
        with self._lock:
            with self.store.transaction(immediate=True) as connection:
                row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=?", (server_id,)).fetchone()
                if row is None and spec is None:
                    raise KeyError(f"mcp_server_not_found:{server_id}")
                connection.execute(
                    "INSERT INTO trace_mcp_servers(server_id,spec_json,enabled,updated_at,deleted) VALUES(?,'{}',0,?,1) "
                    "ON CONFLICT(server_id) DO UPDATE SET spec_json='{}',enabled=0,deleted=1,updated_at=excluded.updated_at",
                    (server_id, self._now()),
                )

    def write_mcp_config(self) -> Path:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json,enabled FROM trace_mcp_servers ORDER BY server_id").fetchall()
        lines = []
        for row in rows:
            spec = json.loads(str(row["spec_json"]))
            lines.append(f"[mcp_servers.{_toml_string(row['server_id'])}]")
            for key in ("transport", "preset", "scope", "command", "cwd", "url"):
                if spec.get(key):
                    lines.append(f"{key} = {_toml_string(spec[key])}")
            for key in ("args", "include_tools", "exclude_tools", "read_only_tools", "write_tools"):
                if spec.get(key):
                    lines.append(f"{key} = {_toml_array(spec[key])}")
            for key in ("startup_timeout_seconds", "tool_timeout_seconds"):
                if key in spec:
                    lines.append(f"{key} = {float(spec[key])}")
            if "read_only" in spec:
                lines.append(f"read_only = {str(bool(spec['read_only'])).lower()}")
            for key in ("env", "headers"):
                values = {str(name): str(value) for name, value in dict(spec.get(key, {})).items() if not str(value).startswith("secret://")}
                if values:
                    lines.append(f"{key} = " + "{" + ", ".join(f"{_toml_string(name)} = {_toml_string(value)}" for name, value in values.items()) + "}")
            lines.append(f"enabled = {str(bool(row['enabled'])).lower()}")
            lines.append("")
        self._managed_mcp.parent.mkdir(parents=True, exist_ok=True)
        temporary = self._managed_mcp.with_suffix(".tmp")
        temporary.write_text("\n".join(lines), encoding="utf-8")
        os.replace(temporary, self._managed_mcp)
        return self._managed_mcp

    def system(self) -> dict[str, Any]:
        return {
            "platform": platform.system().lower(),
            "platform_release": platform.release(),
            "python": platform.python_version(),
            "root": str(self.root),
            "database": str(self.root / "runtime.sqlite3"),
            "managed_mcp_config": str(self._managed_mcp),
            "path_separator": os.pathsep,
            "shell": os.environ.get("SHELL") or os.environ.get("COMSPEC") or "",
        }

    @property
    def auth_required(self) -> bool:
        return True

    def login(self, username: str, password: str, *, client_key: str = "direct") -> str:
        return self.accounts.login(username, password, client_key)

    def authenticated(self, headers: Mapping[str, str], *, force: bool = False) -> bool:
        return self.accounts.user(headers) is not None

    def logout(self, headers: Mapping[str, str]) -> None:
        self.accounts.logout(headers)
