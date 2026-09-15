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
from pathlib import Path
from typing import Any, Mapping

from ..application.resources import ResourceResolver


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _toml_string(value: Any) -> str:
    return json.dumps(str(value or ""), ensure_ascii=False)


def _toml_array(values: Any) -> str:
    return "[" + ", ".join(_toml_string(item) for item in (values or ())) + "]"


def _secret_stream(key: bytes, nonce: bytes, length: int) -> bytes:
    chunks = []
    for counter in range((length + 31) // 32):
        chunks.append(hashlib.sha256(key + nonce + counter.to_bytes(4, "big")).digest())
    return b"".join(chunks)[:length]


def _seal_secret(key: bytes, value: str) -> str:
    nonce = secrets.token_bytes(16)
    plain = value.encode("utf-8")
    cipher = bytes(left ^ right for left, right in zip(plain, _secret_stream(key, nonce, len(plain))))
    tag = hmac.new(key, nonce + cipher, hashlib.sha256).digest()
    return base64.urlsafe_b64encode(nonce + tag + cipher).decode("ascii")


def _open_secret(key: bytes, value: str) -> str:
    raw = base64.urlsafe_b64decode(value.encode("ascii"))
    if len(raw) < 48:
        raise ValueError("secret_ciphertext_invalid")
    nonce, tag, cipher = raw[:16], raw[16:48], raw[48:]
    if not hmac.compare_digest(tag, hmac.new(key, nonce + cipher, hashlib.sha256).digest()):
        raise ValueError("secret_ciphertext_invalid")
    plain = bytes(left ^ right for left, right in zip(cipher, _secret_stream(key, nonce, len(cipher))))
    return plain.decode("utf-8")


class ControlPlane:
    """Durable, redacted settings with process-local secret bindings."""

    def __init__(self, store: Any, root: Path) -> None:
        self.store = store
        self.root = root
        self._lock = threading.RLock()
        self._secrets: dict[str, str] = {}
        self._mcp_secret_values: dict[str, str] = {}
        self._mcp_secret_envs: dict[tuple[str, str, str], str] = {}
        self._sessions: dict[str, tuple[str, float]] = {}
        self._failed_logins: dict[str, list[float]] = {}
        self._managed_mcp = root / "managed-mcp.toml"
        self._secret_key = self._load_secret_key()
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_providers (
                    provider_id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT NOT NULL,
                    model TEXT NOT NULL, api_key_env TEXT NOT NULL DEFAULT '',
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
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_mcp_servers (
                    server_id TEXT PRIMARY KEY, spec_json TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL
                )"""
            )
            connection.execute(
                """CREATE TABLE IF NOT EXISTS trace_secrets (
                    secret_name TEXT PRIMARY KEY, ciphertext TEXT NOT NULL, updated_at TEXT NOT NULL
                )"""
            )
        self._load_secret_values()
        self._migrate_mcp_secret_rows()

    @staticmethod
    def _now() -> str:
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    def _load_secret_key(self) -> bytes:
        self.root.mkdir(parents=True, exist_ok=True)
        path = self.root / "trace-secrets.key"
        try:
            key = path.read_bytes()
            if len(key) == 32:
                return key
        except OSError:
            pass
        key = secrets.token_bytes(32)
        try:
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_BINARY", 0), 0o600)
            try:
                os.write(fd, key)
            finally:
                os.close(fd)
        except FileExistsError:
            key = path.read_bytes()
        if len(key) != 32:
            raise ValueError("trace_secret_key_invalid")
        os.chmod(path, 0o600)
        return key

    def _load_secret_values(self) -> None:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT secret_name,ciphertext FROM trace_secrets").fetchall()
        with self._lock:
            for row in rows:
                try:
                    self._mcp_secret_values[str(row["secret_name"])] = _open_secret(self._secret_key, str(row["ciphertext"]))
                except (ValueError, UnicodeError):
                    continue

    def _persist_secret_values(self, values: Mapping[str, str]) -> None:
        if not values:
            return
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            for name, value in values.items():
                connection.execute(
                    "INSERT INTO trace_secrets(secret_name,ciphertext,updated_at) VALUES(?,?,?) ON CONFLICT(secret_name) DO UPDATE SET ciphertext=excluded.ciphertext,updated_at=excluded.updated_at",
                    (name, _seal_secret(self._secret_key, value), now),
                )

    def _migrate_mcp_secret_rows(self) -> None:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers").fetchall()
        updates: list[tuple[str, str, dict[tuple[str, str, str], str], dict[str, str]]] = []
        for row in rows:
            server_id = str(row["server_id"])
            spec = json.loads(str(row["spec_json"]))
            bindings: dict[tuple[str, str, str], str] = {}
            values: dict[str, str] = {}
            changed = False
            for field in ("env", "headers"):
                source = dict(spec.get(field) or {})
                for key, value in source.items():
                    value_text = str(value)
                    if value_text.startswith("${") and value_text.endswith("}"):
                        continue
                    env_name = "TRACE_MCP_SECRET_" + hashlib.sha256(f"{server_id}:{field[:-1]}:{key}".encode()).hexdigest()[:24].upper()
                    source[str(key)] = "${" + env_name + "}"
                    bindings[(server_id, field[:-1], str(key))] = env_name
                    values[env_name] = value_text
                    changed = True
                spec[field] = source
            if changed:
                updates.append((server_id, _json(spec), bindings, values))
        if not updates:
            return
        now = self._now()
        migrated_values: dict[str, str] = {}
        with self.store.transaction(immediate=True) as connection:
            for server_id, spec_json, _bindings, _values in updates:
                connection.execute(
                    "UPDATE trace_mcp_servers SET spec_json=?,updated_at=? WHERE server_id=?",
                    (spec_json, now, server_id),
                )
        with self._lock:
            for _server_id, _spec_json, bindings, values in updates:
                self._mcp_secret_envs.update(bindings)
                self._mcp_secret_values.update(values)
                migrated_values.update(values)
        self._persist_secret_values(migrated_values)
        try:
            with self.store.connection() as connection:
                connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        except sqlite3.DatabaseError:
            pass

    @staticmethod
    def _provider_payload(row: Mapping[str, Any], secret: bool = False) -> dict[str, Any]:
        return {
            "provider_id": str(row["provider_id"]),
            "name": str(row["name"]),
            "base_url": str(row["base_url"]),
            "model": str(row["model"]),
            "api_key_env": str(row["api_key_env"]),
            "api_key_set": bool(secret),
            "timeout_seconds": float(row["timeout_seconds"]),
            "max_context_tokens": int(row["max_context_tokens"]),
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
                    bool(self._secrets.get(str(row["provider_id"])))
                    or bool(str(row["api_key_env"]) and os.environ.get(str(row["api_key_env"]))),
                )
                for row in rows
            ]

    def provider(self, provider_id: str) -> dict[str, Any]:
        for item in self.providers():
            if item["provider_id"] == provider_id:
                return item
        raise KeyError(f"provider_not_found:{provider_id}")

    def provider_secret(self, provider_id: str) -> str:
        with self._lock:
            secret = self._secrets.get(provider_id, "")
        if secret:
            return secret
        item = self.provider(provider_id)
        return os.environ.get(item["api_key_env"], "") if item["api_key_env"] else ""

    def save_provider(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        provider_id = str(payload.get("provider_id") or payload.get("id") or secrets.token_hex(8)).strip()
        name = str(payload.get("name") or provider_id).strip()
        base_url = str(payload.get("base_url") or "https://api.openai.com/v1").strip().rstrip("/")
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
        except (TypeError, ValueError, OverflowError):
            raise ValueError("provider_limits_invalid") from None
        if not 0 < timeout <= 3600 or not 1 <= context <= 10_000_000:
            raise ValueError("provider_limits_invalid")
        from ..providers import OpenAICompatibleProvider
        try:
            OpenAICompatibleProvider(base_url, model, "", timeout_seconds=timeout, max_context_tokens=context)
        except (TypeError, ValueError) as exc:
            raise ValueError(str(exc)) from None
        secret = str(payload.get("api_key") or "")
        existing_item = next((item for item in self.providers() if item["provider_id"] == provider_id), None)
        if existing_item and (
            existing_item["base_url"] != base_url
            or existing_item["model"] != model
            or existing_item["api_key_env"] != api_key_env
        ) and not secret:
            with self._lock:
                self._secrets.pop(provider_id, None)
        now = self._now()
        with self._lock:
            if secret:
                self._secrets[provider_id] = secret
            elif payload.get("clear_api_key"):
                self._secrets.pop(provider_id, None)
        with self.store.transaction(immediate=True) as connection:
            existing = connection.execute("SELECT created_at FROM trace_providers WHERE provider_id=?", (provider_id,)).fetchone()
            connection.execute(
                """INSERT INTO trace_providers(provider_id,name,base_url,model,api_key_env,timeout_seconds,max_context_tokens,enabled,active,created_at,updated_at)
                   VALUES(?,?,?,?,?,?,?,?,?,?,?)
                   ON CONFLICT(provider_id) DO UPDATE SET name=excluded.name,base_url=excluded.base_url,model=excluded.model,
                   api_key_env=excluded.api_key_env,timeout_seconds=excluded.timeout_seconds,max_context_tokens=excluded.max_context_tokens,
                   enabled=excluded.enabled,updated_at=excluded.updated_at""",
                (provider_id, name, base_url, model, api_key_env, timeout, context, int(payload.get("enabled", True) is not False), 0, str(existing["created_at"]) if existing else now, now),
            )
        return self.provider(provider_id)

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
        index = ResourceResolver().index((self.root,))
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

    def set_skill(self, skill_id: str, *, enabled: bool, config: Mapping[str, Any] | None = None) -> dict[str, Any]:
        if not skill_id or len(skill_id) > 300:
            raise ValueError("skill_id_invalid")
        if not any(item["resource_id"] == skill_id for item in self.skills()):
            raise KeyError(f"skill_not_found:{skill_id}")
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO trace_skills(skill_id,enabled,config_json,updated_at) VALUES(?,?,?,?) ON CONFLICT(skill_id) DO UPDATE SET enabled=excluded.enabled,config_json=excluded.config_json,updated_at=excluded.updated_at",
                (skill_id, int(enabled), _json(dict(config or {})), now),
            )
        return next(item for item in self.skills() if item["resource_id"] == skill_id)

    def mcp_servers(self) -> list[dict[str, Any]]:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT * FROM trace_mcp_servers ORDER BY server_id").fetchall()
        statuses = getattr(self.store, "_trace_mcp_statuses", {})
        result = []
        for row in rows:
            spec = json.loads(str(row["spec_json"]))
            spec.pop("env", None)
            spec.pop("headers", None)
            result.append({"server_id": row["server_id"], **spec, "enabled": bool(row["enabled"]), "status": statuses.get(str(row["server_id"]), {"status": "configured"})})
        return result

    def _clear_mcp_bindings_locked(self, server_id: str) -> None:
        for key, env_name in tuple(self._mcp_secret_envs.items()):
            if key[0] == server_id:
                self._mcp_secret_values.pop(env_name, None)
                self._mcp_secret_envs.pop(key, None)

    def mcp_secret_bindings(self) -> dict[str, dict[str, str]]:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers").fetchall()
        with self._lock:
            values = dict(self._mcp_secret_values)
            bindings: dict[str, dict[str, str]] = {}
            for row in rows:
                server_id = str(row["server_id"])
                spec = json.loads(str(row["spec_json"]))
                names = {
                    str(value)[2:-1]
                    for field in ("env", "headers")
                    for value in dict(spec.get(field) or {}).values()
                    if str(value).startswith("${") and str(value).endswith("}")
                }
                scoped = {name: values[name] for name in names if name in values}
                if scoped:
                    bindings[server_id] = scoped
            return bindings

    def _stored_mcp_secret_names(self, server_id: str) -> set[str]:
        with self.store.connection() as connection:
            row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=?", (server_id,)).fetchone()
        if row is None:
            return set()
        spec = json.loads(str(row["spec_json"]))
        return {
            str(value)[2:-1]
            for field in ("env", "headers")
            for value in dict(spec.get(field) or {}).values()
            if str(value).startswith("${TRACE_MCP_SECRET_") and str(value).endswith("}")
        }

    def save_mcp(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        server_id = str(payload.get("server_id") or payload.get("name") or "").strip()
        if not server_id or len(server_id) > 100 or any(ch not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for ch in server_id):
            raise ValueError("mcp_server_id_invalid")
        transport = str(payload.get("transport") or ("stdio" if payload.get("command") else "http")).casefold()
        if transport not in {"stdio", "http"}:
            raise ValueError("mcp_transport_invalid")
        scope = str(payload.get("scope") or "shared").casefold()
        if scope not in {"shared", "run"}:
            raise ValueError("mcp_scope_invalid")
        raw_env = dict(payload.get("env") or {})
        raw_headers = dict(payload.get("headers") or {})
        env: dict[str, str] = {}
        headers: dict[str, str] = {}
        if (transport == "stdio" and not payload.get("command")) or (transport == "http" and not payload.get("url")):
            raise ValueError("mcp_endpoint_required")
        new_values: dict[str, str] = {}
        new_env_names: dict[tuple[str, str, str], str] = {}
        for field, source, target in (("env", raw_env, env), ("header", raw_headers, headers)):
            for key, value in source.items():
                key_text, value_text = str(key), str(value)
                if value_text.startswith("${") and value_text.endswith("}"):
                    target[key_text] = value_text
                    continue
                env_name = "TRACE_MCP_SECRET_" + hashlib.sha256(f"{server_id}:{field}:{key_text}".encode()).hexdigest()[:24].upper()
                target[key_text] = "${" + env_name + "}"
                new_values[env_name] = value_text
                new_env_names[(server_id, field, key_text)] = env_name
        spec = {
            "transport": transport,
            "preset": str(payload.get("preset") or ""),
            "scope": scope,
            "command": str(payload.get("command") or ""),
            "args": [str(x) for x in payload.get("args", [])] if isinstance(payload.get("args", []), list) else [],
            "env": env,
            "cwd": str(payload.get("cwd") or ""),
            "url": str(payload.get("url") or ""),
            "headers": headers,
            "enabled": payload.get("enabled") is not False,
        }
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO trace_mcp_servers(server_id,spec_json,enabled,updated_at) VALUES(?,?,?,?) ON CONFLICT(server_id) DO UPDATE SET spec_json=excluded.spec_json,enabled=excluded.enabled,updated_at=excluded.updated_at",
                (server_id, _json(spec), int(spec["enabled"]), now),
            )
        old_names = self._stored_mcp_secret_names(server_id)
        with self._lock:
            self._clear_mcp_bindings_locked(server_id)
            self._mcp_secret_values.update(new_values)
            self._mcp_secret_envs.update(new_env_names)
        self._persist_secret_values(new_values)
        removed_names = old_names - set(new_values)
        if removed_names:
            with self.store.transaction(immediate=True) as connection:
                connection.executemany("DELETE FROM trace_secrets WHERE secret_name=?", ((name,) for name in removed_names))
        return next(item for item in self.mcp_servers() if item["server_id"] == server_id)

    def delete_mcp(self, server_id: str) -> None:
        with self.store.transaction(immediate=True) as connection:
            cursor = connection.execute("DELETE FROM trace_mcp_servers WHERE server_id=?", (server_id,))
        if cursor.rowcount != 1:
            raise KeyError(f"mcp_server_not_found:{server_id}")
        names = self._stored_mcp_secret_names(server_id)
        with self._lock:
            self._clear_mcp_bindings_locked(server_id)
        if names:
            with self.store.transaction(immediate=True) as connection:
                connection.executemany("DELETE FROM trace_secrets WHERE secret_name=?", ((name,) for name in names))

    def write_mcp_config(self) -> Path:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers ORDER BY server_id").fetchall()
        lines = []
        for row in rows:
            spec = json.loads(str(row["spec_json"]))
            lines.append(f"[mcp_servers.{row['server_id']}]")
            for key in ("transport", "preset", "scope", "command", "cwd", "url"):
                if spec.get(key):
                    lines.append(f"{key} = {_toml_string(spec[key])}")
            for key in ("args",):
                if spec.get(key):
                    lines.append(f"{key} = {_toml_array(spec[key])}")
            for key in ("env", "headers"):
                values = {str(name): str(value) for name, value in dict(spec.get(key, {})).items() if not str(value).startswith("secret://")}
                if values:
                    lines.append(f"{key} = " + "{" + ", ".join(f"{_toml_string(name)} = {_toml_string(value)}" for name, value in values.items()) + "}")
            lines.append(f"enabled = {str(bool(spec.get('enabled', True))).lower()}")
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
        configured = bool(os.environ.get("TRACE_ADMIN_PASSWORD") or os.environ.get("TRACE_ADMIN_TOKEN"))
        return os.environ.get("TRACE_AUTH_REQUIRED", "1" if configured else "0").casefold() not in {"0", "false", "no"}

    def login(self, password: str, *, client_key: str = "direct") -> str:
        expected = os.environ.get("TRACE_ADMIN_PASSWORD", "")
        token = os.environ.get("TRACE_ADMIN_TOKEN", "")
        if not expected and not token:
            return ""
        now = time.time()
        bucket = str(client_key or "direct")[:128]
        with self._lock:
            failures = [stamp for stamp in self._failed_logins.get(bucket, []) if stamp > now - 60]
            self._failed_logins[bucket] = failures
            if len(failures) >= 5:
                raise ValueError("login_rate_limited")
        if not (hmac.compare_digest(password, expected) or hmac.compare_digest(password, token)):
            with self._lock:
                self._failed_logins.setdefault(bucket, []).append(now)
            raise ValueError("invalid_credentials")
        session = secrets.token_urlsafe(32)
        with self._lock:
            self._sessions[hashlib.sha256(session.encode()).hexdigest()] = (session, time.time() + 43200)
        return session

    def authenticated(self, headers: Mapping[str, str], *, force: bool = False) -> bool:
        if not (self.auth_required or force):
            return True
        raw = str(headers.get("authorization") or "")
        if raw.casefold().startswith("bearer "):
            token = raw[7:].strip()
        else:
            cookie = str(headers.get("cookie") or "")
            token = next((part.split("=", 1)[1] for part in cookie.split(";") if part.strip().startswith("trace_session=")), "")
        if not token:
            return False
        digest = hashlib.sha256(token.encode()).hexdigest()
        with self._lock:
            record = self._sessions.get(digest)
            if record is None or record[1] <= time.time():
                self._sessions.pop(digest, None)
                return False
        return True

    def logout(self, headers: Mapping[str, str]) -> None:
        cookie = str(headers.get("cookie") or "")
        token = next((part.split("=", 1)[1] for part in cookie.split(";") if part.strip().startswith("trace_session=")), "")
        if not token:
            auth = str(headers.get("authorization") or "")
            token = auth[7:].strip() if auth.casefold().startswith("bearer ") else ""
        if token:
            with self._lock:
                self._sessions.pop(hashlib.sha256(token.encode()).hexdigest(), None)


__all__ = ["ControlPlane"]
