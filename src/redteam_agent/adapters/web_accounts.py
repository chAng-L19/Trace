"""Persistent shared-workbench accounts; process sessions revalidate DB revisions."""
from __future__ import annotations
import hashlib
import hmac
import os
import secrets
import sqlite3
import threading
import time
from typing import Mapping


def profile(row):
    return {key: row[key] for key in ("user_id", "username", "display_name", "role")}


def password_hash(password, salt):
    return hashlib.pbkdf2_hmac("sha256", password.encode(), bytes.fromhex(salt), 600_000).hex()


def fields(payload):
    username = str(payload.get("username") or "").strip()
    display = str(payload.get("display_name") or username).strip()
    if not username or len(username) > 128 or len(display) > 256:
        raise ValueError("invalid_profile")
    return username, display


class Accounts:
    def __init__(self, store):
        self.store = store
        self.lock = threading.RLock()
        self.sessions = {}
        self.failures = {}
        with store.transaction(immediate=True) as db:
            db.execute("CREATE TABLE IF NOT EXISTS trace_users (user_id TEXT PRIMARY KEY, username TEXT NOT NULL, username_key TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL, role TEXT NOT NULL, salt TEXT NOT NULL, password_hash TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1)")
            if db.execute("SELECT 1 FROM trace_users LIMIT 1").fetchone() is None:
                self._insert(db, {"username": os.environ.get("TRACE_ADMIN_USERNAME") or "trace", "password": os.environ.get("TRACE_ADMIN_PASSWORD") or "admin@123", "role": "admin"})

    def _insert(self, db, payload):
        username, display = fields(payload)
        password = payload.get("password")
        if not isinstance(password, str) or not password or len(password) > 4096:
            raise ValueError("invalid_password")
        role = payload.get("role", "member")
        if role not in {"admin", "member"}:
            raise ValueError("invalid_role")
        identity, salt = secrets.token_hex(16), secrets.token_hex(16)
        try:
            db.execute("INSERT INTO trace_users VALUES (?, ?, ?, ?, ?, ?, ?, 1)", (identity, username, username.casefold(), display, role, salt, password_hash(password, salt)))
        except sqlite3.IntegrityError:
            raise ValueError("username_exists") from None
        return profile(db.execute("SELECT * FROM trace_users WHERE user_id=?", (identity,)).fetchone())

    @staticmethod
    def token(headers):
        raw = headers.get("authorization", "")
        if raw.lower().startswith("bearer "):
            return raw[7:].strip()
        return next((part.strip().split("=", 1)[1] for part in headers.get("cookie", "").split(";") if part.strip().startswith("trace_session=")), "")

    def user(self, headers):
        digest = hashlib.sha256(self.token(headers).encode()).hexdigest()
        with self.lock:
            session = self.sessions.get(digest)
            if not session:
                return None
            with self.store.transaction() as db:
                row = db.execute("SELECT * FROM trace_users WHERE user_id=?", (session[0],)).fetchone()
            if session[2] <= time.time() or row is None or row["revision"] != session[1]:
                self.sessions.pop(digest, None)
                return None
            return profile(row)

    def login(self, username, password, client_key="direct"):
        if not isinstance(username, str) or not isinstance(password, str) or len(password) > 4096:
            raise ValueError("invalid_credentials")
        with self.lock:
            now = time.time()
            failures = [stamp for stamp in self.failures.get(client_key, []) if stamp > now - 60]
            self.failures[client_key] = failures
            if len(failures) >= 5:
                raise ValueError("login_rate_limited")
            with self.store.transaction() as db:
                row = db.execute("SELECT * FROM trace_users WHERE username_key=?", (username.strip().casefold(),)).fetchone()
            candidate = password_hash(password, row["salt"] if row else "00" * 16)
            if row is None or not hmac.compare_digest(candidate, row["password_hash"]):
                failures.append(now)
                raise ValueError("invalid_credentials")
            token = secrets.token_urlsafe(32)
            self.sessions[hashlib.sha256(token.encode()).hexdigest()] = (row["user_id"], row["revision"], now + 43200)
            return token

    def logout(self, headers):
        with self.lock:
            self.sessions.pop(hashlib.sha256(self.token(headers).encode()).hexdigest(), None)

    def update(self, headers, payload):
        with self.lock:
            user = self.user(headers)
            if user is None:
                raise PermissionError("authentication_required")
            with self.store.transaction(immediate=True) as db:
                row = db.execute("SELECT * FROM trace_users WHERE user_id=?", (user["user_id"],)).fetchone()
                key = hashlib.sha256(self.token(headers).encode()).hexdigest()
                if row["revision"] != self.sessions[key][1]:
                    raise PermissionError("authentication_required")
                username, display = fields({**dict(row), **payload})
                salt, digest, revision = row["salt"], row["password_hash"], row["revision"]
                identity_changed = username != row["username"] or "password" in payload
                if identity_changed:
                    current = payload.get("current_password", "")
                    if not isinstance(current, str) or len(current) > 4096 or not hmac.compare_digest(password_hash(current, salt), digest):
                        raise ValueError("current_password_invalid")
                    revision += 1
                if "password" in payload:
                    password = payload["password"]
                    if not isinstance(password, str) or not password or len(password) > 4096:
                        raise ValueError("invalid_password")
                    salt = secrets.token_hex(16)
                    digest = password_hash(password, salt)
                try:
                    db.execute("UPDATE trace_users SET username=?, username_key=?, display_name=?, salt=?, password_hash=?, revision=? WHERE user_id=?", (username, username.casefold(), display, salt, digest, revision, user["user_id"]))
                except sqlite3.IntegrityError:
                    raise ValueError("username_exists") from None
                result = profile(db.execute("SELECT * FROM trace_users WHERE user_id=?", (user["user_id"],)).fetchone())
            key = hashlib.sha256(self.token(headers).encode()).hexdigest()
            old = self.sessions[key]
            self.sessions[key] = (old[0], revision, old[2])
            return result

    def users(self, headers, payload=None):
        with self.lock:
            token_hash = hashlib.sha256(self.token(headers).encode()).hexdigest()
            session = self.sessions.get(token_hash)
            with self.store.transaction(immediate=payload is not None) as db:
                row = db.execute("SELECT * FROM trace_users WHERE user_id=?", (session[0],)).fetchone() if session else None
                if row is None or session[2] <= time.time() or row["revision"] != session[1]:
                    raise PermissionError("authentication_required")
                if row["role"] != "admin":
                    raise PermissionError("admin_required")
                if payload is not None:
                    return self._insert(db, payload)
                return [profile(row) for row in db.execute("SELECT * FROM trace_users ORDER BY username_key")]
