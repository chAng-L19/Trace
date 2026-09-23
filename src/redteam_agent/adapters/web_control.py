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


def _local_provider(base_url: str) -> bool:
    import ipaddress
    from urllib.parse import urlsplit
    hostname = urlsplit(base_url).hostname or ""
    try:
        return ipaddress.ip_address(hostname).is_loopback
    except ValueError:
        return hostname.casefold() == "localhost"


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
        self._migrate_mcp_secret_rows()
        self._load_secret_values()

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
        with self._lock:
            with self.store.connection() as connection:
                rows = connection.execute("SELECT secret_name,ciphertext FROM trace_secrets").fetchall()
                servers = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers").fetchall()
            values: dict[str, str] = {}
            for row in rows:
                try:
                    values[str(row["secret_name"])] = _open_secret(self._secret_key, str(row["ciphertext"]))
                except (ValueError, UnicodeError):
                    continue
            self._mcp_secret_values = values
            self._mcp_secret_envs = {
                (str(row["server_id"]), field, key): name
                for row in servers
                for (field, key), name in self._mcp_secret_references(json.loads(row["spec_json"])).items()
            }

    def _persist_secret_values(self, connection: sqlite3.Connection, values: Mapping[str, str]) -> None:
        """Write ciphertexts in the caller's configuration transaction."""
        for name, value in values.items():
            connection.execute(
                "INSERT INTO trace_secrets(secret_name,ciphertext,updated_at) VALUES(?,?,?) ON CONFLICT(secret_name) DO UPDATE SET ciphertext=excluded.ciphertext,updated_at=excluded.updated_at",
                (name, _seal_secret(self._secret_key, value), self._now()),
            )

    @staticmethod
    def _mcp_secret_references(spec: Mapping[str, Any]) -> dict[tuple[str, str], str]:
        return {
            ("env" if field == "env" else "header", str(key)): str(value)[2:-1]
            for field in ("env", "headers")
            for key, value in dict(spec.get(field) or {}).items()
            if str(value).startswith("${TRACE_MCP_SECRET_") and str(value).endswith("}")
        }

    def _delete_unreferenced_secrets(self, connection: sqlite3.Connection, names: set[str]) -> None:
        if not names:
            return
        referenced = {
            name
            for row in connection.execute("SELECT spec_json FROM trace_mcp_servers")
            for name in self._mcp_secret_references(json.loads(row["spec_json"])).values()
        }
        connection.executemany("DELETE FROM trace_secrets WHERE secret_name=?", ((name,) for name in names - referenced))

    def _migrate_mcp_secret_rows(self) -> None:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers").fetchall()
        updates: list[tuple[str, str, dict[str, str]]] = []
        for row in rows:
            server_id = str(row["server_id"])
            spec = json.loads(str(row["spec_json"]))
            values: dict[str, str] = {}
            changed = False
            for field in ("env", "headers"):
                source = dict(spec.get(field) or {})
                for key, value in source.items():
                    value_text = str(value)
                    if value_text.startswith("${") and value_text.endswith("}"):
                        continue
                    kind = "env" if field == "env" else "header"
                    env_name = "TRACE_MCP_SECRET_" + hashlib.sha256(f"{server_id}:{kind}:{key}".encode()).hexdigest()[:24].upper()
                    source[str(key)] = "${" + env_name + "}"
                    values[env_name] = value_text
                    changed = True
                spec[field] = source
            if changed:
                updates.append((server_id, _json(spec), values))
        if not updates:
            return
        now = self._now()
        with self.store.transaction(immediate=True) as connection:
            for server_id, spec_json, values in updates:
                self._persist_secret_values(connection, values)
                connection.execute(
                    "UPDATE trace_mcp_servers SET spec_json=?,updated_at=? WHERE server_id=?",
                    (spec_json, now, server_id),
                )
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
            "configured": True,
            "ready": bool(row["enabled"]) and (bool(secret) or _local_provider(str(row["base_url"]))),
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

    def provider_secret(self, provider_id: str, *, include_environment: bool = True) -> str:
        item = self.provider(provider_id)
        environment = os.environ.get(item["api_key_env"], "") if include_environment and item["api_key_env"] else ""
        with self._lock:
            return environment or self._secrets.get(provider_id, "")

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
        clear_secret = bool(payload.get("clear_api_key")) or bool(existing_item and (
            existing_item["base_url"] != base_url
            or existing_item["model"] != model
            or existing_item["api_key_env"] != api_key_env
        ) and not secret)
        now = self._now()
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
        with self._lock:
            if secret:
                self._secrets[provider_id] = secret
            elif clear_secret:
                self._secrets.pop(provider_id, None)
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

    def mcp_secret_bindings(self) -> dict[str, dict[str, str]]:
        with self._lock:
            with self.store.connection() as connection:
                rows = connection.execute("SELECT server_id,spec_json FROM trace_mcp_servers").fetchall()
            bindings: dict[str, dict[str, str]] = {}
            for row in rows:
                names = self._mcp_secret_references(json.loads(row["spec_json"])).values()
                scoped = {name: self._mcp_secret_values[name] for name in names if name in self._mcp_secret_values}
                if scoped:
                    bindings[str(row["server_id"])] = scoped
            return bindings

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
        if (transport == "stdio" and not payload.get("command")) or (transport == "http" and not payload.get("url")):
            raise ValueError("mcp_endpoint_required")
        with self._lock:
            with self.store.transaction(immediate=True) as connection:
                row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=?", (server_id,)).fetchone()
                existing_spec = json.loads(row["spec_json"]) if row is not None else {}
                old_names = set(self._mcp_secret_references(existing_spec).values())
                spec = {
                    "transport": transport,
                    "preset": str(payload.get("preset") or ""),
                    "scope": scope,
                    "command": str(payload.get("command") or ""),
                    "args": [str(x) for x in payload.get("args", [])] if isinstance(payload.get("args", []), list) else [],
                    "cwd": str(payload.get("cwd") or ""),
                    "url": str(payload.get("url") or ""),
                    "enabled": payload.get("enabled") is not False,
                }
                new_values: dict[str, str] = {}
                # Omitted secret fields preserve references; an explicit empty
                # mapping clears them in the same transaction as the settings.
                for field in ("env", "headers"):
                    source = dict((payload[field] if field in payload else existing_spec.get(field)) or {})
                    target: dict[str, str] = {}
                    kind = "env" if field == "env" else "header"
                    for key, value in source.items():
                        key_text, value_text = str(key), str(value)
                        if value_text.startswith("${") and value_text.endswith("}"):
                            target[key_text] = value_text
                            continue
                        name = "TRACE_MCP_SECRET_" + hashlib.sha256(f"{server_id}:{kind}:{key_text}".encode()).hexdigest()[:24].upper()
                        target[key_text] = "${" + name + "}"
                        new_values[name] = value_text
                    spec[field] = target
                self._persist_secret_values(connection, new_values)
                for name in self._mcp_secret_references(spec).values():
                    secret = connection.execute("SELECT ciphertext FROM trace_secrets WHERE secret_name=?", (name,)).fetchone()
                    if secret is None:
                        raise ValueError("mcp_secret_unavailable")
                    _open_secret(self._secret_key, secret["ciphertext"])
                connection.execute(
                    "INSERT INTO trace_mcp_servers(server_id,spec_json,enabled,updated_at) VALUES(?,?,?,?) ON CONFLICT(server_id) DO UPDATE SET spec_json=excluded.spec_json,enabled=excluded.enabled,updated_at=excluded.updated_at",
                    (server_id, _json(spec), int(spec["enabled"]), self._now()),
                )
                self._delete_unreferenced_secrets(connection, old_names)
            # Cache publication follows successful COMMIT; every failure leaves
            # the old bindings intact, including failures while pruning secrets.
            self._load_secret_values()
            return next(item for item in self.mcp_servers() if item["server_id"] == server_id)

    def delete_mcp(self, server_id: str) -> None:
        with self._lock:
            with self.store.transaction(immediate=True) as connection:
                row = connection.execute("SELECT spec_json FROM trace_mcp_servers WHERE server_id=?", (server_id,)).fetchone()
                if row is None:
                    raise KeyError(f"mcp_server_not_found:{server_id}")
                names = set(self._mcp_secret_references(json.loads(row["spec_json"])).values())
                connection.execute("DELETE FROM trace_mcp_servers WHERE server_id=?", (server_id,))
                self._delete_unreferenced_secrets(connection, names)
            self._load_secret_values()

    def write_mcp_config(self) -> Path:
        with self.store.connection() as connection:
            rows = connection.execute("SELECT server_id,spec_json,enabled FROM trace_mcp_servers ORDER BY server_id").fetchall()
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
