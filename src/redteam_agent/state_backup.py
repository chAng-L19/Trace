"""Stopped-state ZIP snapshots. No service construction, migrations or network access."""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import sqlite3
import stat
import tempfile
import time
import zipfile
from contextlib import closing
from datetime import datetime, timezone
from importlib.resources import files
from pathlib import Path, PurePosixPath

from .runtime.security import SCHEMA_VERSION

FORMAT_VERSION = 1
DATABASE = "runtime.sqlite3"
SIDECARS = {DATABASE + suffix for suffix in ("-wal", "-shm", "-journal")}
MAX_FILES = 100_000
MAX_FILE_BYTES = 2 * 1024**3
MAX_TOTAL_BYTES = 8 * 1024**3
MAX_MANIFEST_BYTES = 16 * 1024**2


class StateBackupError(ValueError):
    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


def _fail(code: str) -> None:
    raise StateBackupError(code)


def _path(value: str) -> str:
    # Validate Windows spellings on POSIX too; ZIP names always use '/'.
    if (not isinstance(value, str) or not value or any(ord(char) < 32 for char in value)
            or any(char in value for char in '\\:<>"|?*')):
        _fail("archive_path_invalid")
    parts = value.split("/")
    reserved = {"CON", "PRN", "AUX", "NUL", *(f"COM{i}" for i in "123456789¹²³"), *(f"LPT{i}" for i in "123456789¹²³")}
    if PurePosixPath(value).is_absolute() or any(
        part in {"", ".", ".."} or part[-1:] in {".", " "}
        or part.split(".", 1)[0].rstrip(" ").upper() in reserved for part in parts
    ):
        _fail("archive_path_invalid")
    return value


def _digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def _fsync_writable(path: Path) -> None:
    """Flush a file through a writable descriptor (Windows rejects read handles)."""

    flags = os.O_RDWR
    if hasattr(os, "O_BINARY"):
        flags |= os.O_BINARY
    descriptor = os.open(path, flags)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _json_object(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            _fail("archive_manifest_duplicate_key")
        result[key] = value
    return result


def _scan(root: Path) -> tuple[dict[str, dict], list[str]]:
    records, directories = {}, []
    for path in sorted(root.rglob("*")):
        relative = _path(path.relative_to(root).as_posix())
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400:
            _fail("state_links_not_supported")
        if stat.S_ISDIR(info.st_mode):
            directories.append(relative)
        elif stat.S_ISREG(info.st_mode):
            if relative == DATABASE or relative in SIDECARS:
                continue
            records[relative] = {"sha256": _digest(path), "size": info.st_size,
                                 "mode": stat.S_IMODE(info.st_mode), "mtime_ns": info.st_mtime_ns}
        else:
            _fail("state_special_file_not_supported")
    return records, directories


def _schema(connection: sqlite3.Connection) -> int:
    try:
        pragma = int(connection.execute("PRAGMA user_version").fetchone()[0])
        row = connection.execute("SELECT value FROM schema_metadata WHERE key='schema_version'").fetchone()
        metadata_version = int(row[0]) if row else 0
        quick_check = connection.execute("PRAGMA quick_check").fetchone()
    except (sqlite3.DatabaseError, TypeError, ValueError, IndexError) as exc:
        raise StateBackupError("state_schema_incompatible") from exc
    if metadata_version != pragma or not 1 <= pragma <= SCHEMA_VERSION:
        _fail("state_schema_incompatible")
    if not quick_check or quick_check[0] != "ok":
        _fail("state_database_corrupt")
    return pragma


def _stopped(root: Path, connection: sqlite3.Connection) -> None:
    if any(path.suffix in {".lock", ".pid"} or path.name == "LOCK" for path in root.iterdir()):
        _fail("state_process_lock_present_stop_services_first")
    tables = {row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    checks = {
        "operations": ("status IN ('running','cancelling')", ()),
        "action_leases": ("expires_at > ?", (time.time(),)),
        "worker_tasks": ("status IN ('prepared','running','waiting_worker')", ()),
        "web_command_receipts": ("status='running' OR lease_expires_at > ?", (time.time(),)),
    }
    for table, (where, parameters) in checks.items():
        if table in tables and connection.execute(f"SELECT 1 FROM {table} WHERE {where} LIMIT 1", parameters).fetchone():
            _fail("state_active_stop_services_first")


def _database_path(root: Path) -> Path:
    database = root / DATABASE
    if not root.is_dir() or not database.is_file():
        _fail("state_database_missing")
    if any(path.is_symlink() or getattr(path.lstat(), "st_file_attributes", 0) & 0x400
           for path in (root, database)):
        _fail("state_links_not_supported")
    return database


def backup(root: Path, archive: Path) -> dict:
    root, archive = root.expanduser().absolute(), archive.expanduser().absolute()
    database = _database_path(root)
    if archive.resolve().is_relative_to(root.resolve()):
        _fail("archive_must_be_outside_state")
    if archive.is_symlink() or archive.exists():
        _fail("archive_already_exists")
    archive.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".trace-backup-", dir=archive.parent) as directory:
        stage = Path(directory)
        snapshot = stage / DATABASE
        temporary = stage / "backup.zip"
        # A reserved SQLite writer lock freezes metadata while CAS/workspaces are copied.
        # SQLite's backup API includes committed WAL pages without copying stale sidecars.
        with closing(sqlite3.connect(database.as_uri() + "?mode=rw", uri=True, timeout=0)) as lock:
            try:
                lock.execute("BEGIN IMMEDIATE")
            except sqlite3.OperationalError as exc:
                raise StateBackupError("state_database_busy_stop_services_first") from exc
            try:
                version = _schema(lock)
                _stopped(root, lock)
                before, directories = _scan(root)
                if (len(before) + len(directories) + 1 > MAX_FILES
                        or any(item["size"] > MAX_FILE_BYTES for item in before.values())
                        or sum(item["size"] for item in before.values()) > MAX_TOTAL_BYTES):
                    _fail("archive_size_limit_exceeded")
                with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as source:
                    with closing(sqlite3.connect(snapshot)) as destination:
                        source.backup(destination)
                        destination.execute("PRAGMA journal_mode=DELETE")
            except sqlite3.DatabaseError as exc:
                raise StateBackupError("state_database_corrupt") from exc
            try:
                records = {name: {key: value for key, value in item.items() if key != "mtime_ns"}
                           for name, item in before.items()}
                records[DATABASE] = {"sha256": _digest(snapshot), "size": snapshot.stat().st_size, "mode": 0o600}
                total_size = sum(int(item["size"]) for item in records.values())
                if (len(records) + len(directories) + 1 > MAX_FILES
                        or any(item["size"] > MAX_FILE_BYTES for item in records.values())
                        or total_size > MAX_TOTAL_BYTES):
                    _fail("archive_size_limit_exceeded")
                manifest = {"format": "trace-state", "version": FORMAT_VERSION,
                    "schema_version": version, "created_at": datetime.now(timezone.utc).isoformat(),
                    "files": records, "directories": directories,
                    "tool_manifest": json.loads(files("redteam_agent").joinpath("tool_manifest.json").read_text())}
                manifest_bytes = json.dumps(manifest, sort_keys=True).encode("utf-8")
                if len(manifest_bytes) > MAX_MANIFEST_BYTES:
                    _fail("archive_manifest_invalid")
                try:
                    with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
                        for name in sorted(records):
                            bundle.write(snapshot if name == DATABASE else root / name, "state/" + name)
                        bundle.writestr("manifest.json", manifest_bytes)
                except OSError as exc:
                    raise StateBackupError("state_changed_during_backup") from exc
                if (before, directories) != _scan(root):
                    _fail("state_changed_during_backup")
                verify(temporary)
                os.chmod(temporary, 0o600)
                # Publish only after close, verification and a durable flush. A hard link
                # is atomic and refuses to replace an archive created by a racing caller.
                _fsync_writable(temporary)
                try:
                    os.link(temporary, archive)
                except FileExistsError:
                    _fail("archive_already_exists")
                except OSError as exc:
                    raise StateBackupError("archive_publish_failed") from exc
                try:
                    _fsync_writable(archive)
                except OSError as exc:
                    try:
                        archive.unlink(missing_ok=True)
                    except OSError:
                        pass
                    raise StateBackupError("archive_publish_failed") from exc
            finally:
                lock.rollback()
    return {"ok": True, "archive": str(archive), "schema_version": version, "files": len(records)}


def _extract_unchecked(archive: Path, destination: Path) -> dict:
    with zipfile.ZipFile(archive) as bundle:
        members = bundle.infolist()
        names = [item.filename for item in members]
        if (len(members) > MAX_FILES + 1 or any(item.file_size > MAX_FILE_BYTES for item in members)
                or sum(item.file_size for item in members) > MAX_TOTAL_BYTES
                or any(item.file_size > 1024**2 and item.file_size > max(1, item.compress_size) * 1000
                       for item in members)):
            _fail("archive_size_limit_exceeded")
        if len(names) != len(set(names)) or len(names) != len({name.casefold() for name in names}):
            _fail("archive_duplicate_path")
        if "manifest.json" not in names or bundle.getinfo("manifest.json").file_size > MAX_MANIFEST_BYTES:
            _fail("archive_manifest_invalid")
        manifest = json.loads(bundle.read("manifest.json"), object_pairs_hook=_json_object)
        if (not isinstance(manifest, dict) or manifest.get("format") != "trace-state"
                or type(manifest.get("version")) is not int or manifest["version"] != FORMAT_VERSION):
            _fail("archive_version_incompatible")
        if type(manifest.get("schema_version")) is not int or not 1 <= manifest["schema_version"] <= SCHEMA_VERSION:
            _fail("state_schema_incompatible")
        records, directories = manifest.get("files"), manifest.get("directories")
        if (not isinstance(records, dict) or not isinstance(directories, list)
                or DATABASE not in records or any(not isinstance(name, str) for name in directories)):
            _fail("archive_manifest_invalid")
        for name in [*records, *directories]:
            _path(name)
        if any(name in records for name in SIDECARS):
            _fail("archive_database_sidecar_invalid")
        paths = [*records, *directories]
        if len(paths) > MAX_FILES:
            _fail("archive_size_limit_exceeded")
        if len(paths) != len({name.casefold() for name in paths}):
            _fail("archive_duplicate_path")
        total_manifest_size = 0
        directory_names = set(directories)
        if any(str(parent) not in directory_names for name in paths
               for parent in PurePosixPath(name).parents if str(parent) != "."):
            _fail("archive_parent_directory_invalid")
        if set(names) != {"manifest.json", *("state/" + name for name in records)}:
            _fail("archive_unlisted_files")
        for item in members:
            mode = item.external_attr >> 16
            if (stat.S_IFMT(mode) not in {0, stat.S_IFREG} or item.external_attr & 0x10
                    or item.flag_bits & 1):
                _fail("archive_special_file_invalid")
        for name, record in records.items():
            if (not isinstance(record, dict) or not isinstance(record.get("sha256"), str)
                    or len(record["sha256"]) != 64 or any(c not in "0123456789abcdef" for c in record["sha256"])
                    or type(record.get("size")) is not int or not 0 <= record["size"] <= MAX_FILE_BYTES
                    or type(record.get("mode")) is not int or not 0 <= record["mode"] <= 0o7777):
                _fail("archive_manifest_invalid")
            total_manifest_size += int(record["size"])
            if bundle.getinfo("state/" + name).file_size != record["size"]:
                _fail("archive_size_mismatch")
        if total_manifest_size > MAX_TOTAL_BYTES:
            _fail("archive_size_limit_exceeded")
        for name in sorted(directories):
            (destination / name).mkdir(mode=0o700, parents=True, exist_ok=True)
        for name, record in records.items():
            target = destination / name
            item = bundle.getinfo("state/" + name)
            with bundle.open(item) as source, target.open("xb") as output:
                shutil.copyfileobj(source, output)
            if _digest(target) != record["sha256"]:
                _fail("archive_hash_mismatch")
            # Preserve executable tools/workspaces while keeping restored secrets private.
            os.chmod(target, 0o700 if int(record.get("mode", 0)) & 0o111 else 0o600)
        try:
            with closing(sqlite3.connect((destination / DATABASE).as_uri() + "?mode=ro&immutable=1", uri=True)) as connection:
                if _schema(connection) != manifest["schema_version"]:
                    _fail("state_schema_incompatible")
        except sqlite3.DatabaseError as exc:
            raise StateBackupError("archive_database_invalid") from exc
    return manifest


def _extract(archive: Path, destination: Path) -> dict:
    """Extract and validate an archive, normalizing malformed ZIP errors."""

    try:
        return _extract_unchecked(archive, destination)
    except StateBackupError:
        raise
    except (OSError, KeyError, TypeError, ValueError, UnicodeError, RuntimeError,
            NotImplementedError, zipfile.BadZipFile) as exc:
        raise StateBackupError("archive_invalid") from exc


def _archive_path(value: Path) -> Path:
    path = value.expanduser().absolute()
    if path.is_symlink():
        _fail("archive_path_invalid")
    return path


def verify(archive: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="trace-verify-") as directory:
        manifest = _extract(_archive_path(archive), Path(directory).resolve())
    return {"ok": True, "version": FORMAT_VERSION, "schema_version": manifest["schema_version"],
            "files": len(manifest["files"])}


def restore(archive: Path, root: Path) -> dict:
    root = root.expanduser().absolute()
    if root.exists() or root.is_symlink():
        # Require a new root, including on Windows where replacing directories differs.
        _fail("restore_target_exists_use_new_directory")
    root.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".trace-restore-", dir=root.parent) as directory:
        stage = Path(directory) / "state"
        stage.mkdir(mode=0o700)
        manifest = _extract(_archive_path(archive), stage.resolve())
        for path in stage.rglob("*"):
            if path.is_file():
                with path.open("r+b") as stream:
                    os.fsync(stream.fileno())
        if root.exists() or root.is_symlink():
            _fail("restore_target_exists_use_new_directory")
        os.rename(stage, root)
    return {"ok": True, "root": str(root), "schema_version": manifest["schema_version"],
            "files": len(manifest["files"])}
