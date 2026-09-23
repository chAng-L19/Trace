"""Dependency-free CLI/state regression check; all generated files live in TEMP."""
from __future__ import annotations

import json
import os
import sqlite3
import stat
import subprocess
import sys
import tempfile
import zipfile
from contextlib import closing
from pathlib import Path
from unittest.mock import patch


def main() -> None:
    sys.dont_write_bytecode = True
    source = Path(__file__).resolve().parents[1] / "src"
    sys.path.insert(0, str(source))
    from redteam_agent import state_backup

    env = {key: value for key, value in os.environ.items() if not key.startswith("TRACE_")}
    env.update(PYTHONPATH=str(source), PYTHONDONTWRITEBYTECODE="1", PYTHONUTF8="1")
    commands = []
    with tempfile.TemporaryDirectory(prefix="trace-cli-state-smoke-") as temporary:
        base = Path(temporary)
        root, restored = base / "state", base / "restored"
        env["TRACE_HOME"] = str(root)
        env["REDTEAM_AGENT_HOME"] = str(base / "agent-home")
        env["REDTEAM_AGENT_CONFIG"] = str(base / "missing-config.toml")
        env["TRACE_TOOLS_HOME"] = str(base / "tools")

        def cli(*arguments, success=True, stdin=None):
            result = subprocess.run([sys.executable, "-B", "-m", "redteam_agent", *map(str, arguments)],
                input=stdin, capture_output=True, text=True, env=env, cwd=base, timeout=90)
            assert (result.returncode in (0, 1) if success is None else (result.returncode == 0) == success), (
                arguments[:2], result.returncode, result.stderr, result.stdout[:500])
            assert "SENTINEL_CREDENTIAL_DO_NOT_PRINT" not in result.stdout + result.stderr
            commands.append({"command": " ".join(map(str, arguments[:2])), "exit": result.returncode})
            return result.stdout

        objective = "Give me a plan; do not make changes yet and no need to run tests"
        started = json.loads(cli("start", "--objective", objective, "--target", base, "--max-actions", 16))
        run_id = started["runs"][0]["run"]["run_id"]
        archive = base / "state.zip"
        cli("state", "backup", archive, success=False)
        assert not archive.exists()
        completed = json.loads(cli("run", run_id, "--root", root))
        assert completed["terminal"]["success"] and completed["run"]["status"] == "completed"
        status = json.loads(cli("status", run_id))
        assert status["run"]["run_id"] == run_id
        evidence = json.loads(cli("evidence", run_id))["evidence"]
        assert evidence and all("payload" not in item for item in evidence)
        assert json.loads(cli("evidence", run_id, evidence[0]["evidence_id"], "--include-payload"))
        events = [json.loads(line) for line in cli("events", run_id, "--jsonl").splitlines()]
        assert events and all(event["run_id"] == run_id for event in events)
        assert json.loads(cli("resume", run_id))["run"]["status"] == "completed"

        env["TRACE_TEST_KEY"] = "SENTINEL_CREDENTIAL_DO_NOT_PRINT"
        pending = json.loads(cli("start", objective, "--targets", base, "--model", "fixture",
            "--api-base-url", "http://127.0.0.1:9/v1", "--api-key-env", "TRACE_TEST_KEY"))
        cancelled = json.loads(cli("cancel", pending["runs"][0]["run"]["run_id"], "--reason", "smoke_stop"))
        assert cancelled["run"]["status"] == "cancelled"
        cli("status", "missing", success=False)
        cli("start", "--api-key", env["TRACE_TEST_KEY"], success=False)
        cli("run", run_id, "--max-actions", "SENTINEL_CREDENTIAL_DO_NOT_PRINT", success=False)

        request = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}) + "\n"
        for separator in ([], ["--"]):
            messages = [json.loads(line) for line in cli("mcp", *separator, "--root", root, stdin=request).splitlines()]
            assert messages[0]["id"] == 1 and "result" in messages[0]

        workspace = root / "workspaces" / "snapshot-fixture"
        workspace.mkdir()
        (workspace / "state.txt").write_text("workspace content", encoding="utf-8")
        cas_payload = b"content-addressed smoke artifact"
        import hashlib
        cas_digest = hashlib.sha256(cas_payload).hexdigest()
        cas_path = root / "artifact-store" / "cas" / "sha256" / cas_digest[:2] / cas_digest[2:]
        cas_path.parent.mkdir(parents=True)
        cas_path.write_bytes(cas_payload)
        (root / "empty-directory").mkdir()
        (root / "tools").mkdir()
        (root / "tools" / "active.json").write_text('{"fixture":"1"}', encoding="utf-8")
        original_key = (root / "trace-secrets.key").read_bytes()
        (root / "managed-mcp.toml").write_text("# SENTINEL_CREDENTIAL_DO_NOT_PRINT\n", encoding="utf-8")
        with closing(sqlite3.connect(root / "runtime.sqlite3")) as connection:
            connection.execute("PRAGMA journal_mode=WAL")
            connection.execute("PRAGMA wal_autocheckpoint=0")
            connection.execute("CREATE TABLE backup_fixture (value TEXT)")
            connection.execute("INSERT INTO backup_fixture VALUES ('committed WAL page')")
            connection.commit()
            assert (root / "runtime.sqlite3-wal").stat().st_size > 0
            (root / "service.pid").write_text("fixture")
            cli("state", "backup", archive, success=False)
            (root / "service.pid").unlink()
            cli("state", "backup", archive)
        assert json.loads(cli("state", "verify", archive))["ok"]
        cli("state", "backup", archive, success=False)
        with zipfile.ZipFile(archive) as bundle:
            assert bundle.read("state/trace-secrets.key") == original_key
            assert b"SENTINEL_CREDENTIAL_DO_NOT_PRINT" in bundle.read("state/managed-mcp.toml")
            manifest = json.loads(bundle.read("manifest.json"))
            assert f"artifact-store/cas/sha256/{cas_digest[:2]}/{cas_digest[2:]}" in manifest["files"]
            assert {"runtime.sqlite3", "trace-secrets.key", "managed-mcp.toml"} <= manifest["files"].keys()

        def corrupted(name, transform):
            output = base / name
            with zipfile.ZipFile(archive) as source_zip, zipfile.ZipFile(output, "w") as target_zip:
                for member in source_zip.infolist():
                    filename, value = transform(member.filename, source_zip.read(member))
                    target_zip.writestr(filename, value)
            return output

        tampered = corrupted("tampered.zip", lambda name, value:
            (name, b"X" * len(value) if name == "state/workspaces/snapshot-fixture/state.txt" else value))
        cli("state", "restore", tampered, "--root", restored, success=False)
        assert not restored.exists()
        traversal = corrupted("traversal.zip", lambda name, value:
            ("../escaped" if name == "state/workspaces/snapshot-fixture/state.txt" else name, value))
        cli("state", "verify", traversal, success=False)
        assert not (base / "escaped").exists()

        def newer(name, value):
            if name == "manifest.json":
                manifest = json.loads(value)
                manifest["schema_version"] = 999999
                value = json.dumps(manifest).encode()
            return name, value

        cli("state", "restore", corrupted("newer.zip", newer), "--root", restored, success=False)
        assert not restored.exists()

        duplicate = base / "duplicate.zip"
        duplicate.write_bytes(archive.read_bytes())
        import warnings
        with warnings.catch_warnings(), zipfile.ZipFile(duplicate, "a") as bundle:
            warnings.simplefilter("ignore", UserWarning)
            bundle.writestr("manifest.json", "{}")
        cli("state", "verify", duplicate, success=False)
        duplicate_key = corrupted("duplicate-key.zip", lambda name, value:
            (name, value.replace(b'"version": 1', b'"version": 1,"version": 1', 1)
             if name == "manifest.json" else value))
        cli("state", "verify", duplicate_key, success=False)
        for invalid in ("../outside", "C:/outside", "dir\\outside", "CON.txt", "NUL .txt", "dir/ads:secret", "bad?name"):
            try:
                state_backup._path(invalid)
            except state_backup.StateBackupError:
                pass
            else:
                raise AssertionError("accepted_invalid_path")
        special = base / "symlink.zip"
        with zipfile.ZipFile(archive) as source_zip, zipfile.ZipFile(special, "w") as target_zip:
            for member in source_zip.infolist():
                value = source_zip.read(member)
                if member.filename == "state/workspaces/snapshot-fixture/state.txt":
                    member.external_attr = (stat.S_IFLNK | 0o777) << 16
                target_zip.writestr(member, value)
        cli("state", "verify", special, success=False)
        bomb = base / "bomb.zip"
        with zipfile.ZipFile(bomb, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
            bundle.writestr("manifest.json", "{}")
            bundle.writestr("state/bomb", b"0" * (2 * 1024**2))
        assert json.loads(cli("state", "verify", bomb, success=False))["error"] == "archive_size_limit_exceeded"
        cli("state", "restore", archive, "--root", root, success=False)
        assert (root / "trace-secrets.key").read_bytes() == original_key
        cli("state", "restore", archive, "--root", restored)
        assert (restored / "empty-directory").is_dir()
        assert (restored / "trace-secrets.key").read_bytes() == original_key
        assert (restored / "tools/active.json").read_text() == '{"fixture":"1"}'
        assert (restored / f"artifact-store/cas/sha256/{cas_digest[:2]}/{cas_digest[2:]}").read_bytes() == cas_payload
        assert (restored / "workspaces/snapshot-fixture/state.txt").read_text() == "workspace content"
        assert "SENTINEL_CREDENTIAL_DO_NOT_PRINT" in (restored / "managed-mcp.toml").read_text()
        with closing(sqlite3.connect(restored / "runtime.sqlite3")) as connection:
            assert connection.execute("SELECT value FROM backup_fixture").fetchone()[0] == "committed WAL page"
        assert json.loads(cli("status", run_id, "--root", restored))["terminal"]["success"]
        assert json.loads(cli("self-test", "--root", restored))["terminal"]["success"]
        assert isinstance(json.loads(cli("setup", "rizin", "--offline", "--json", success=None))["success"], bool)
        assert isinstance(json.loads(cli("doctor", "--root", root, "--json", success=None))["ready"], bool)

        # Simulate an external workspace writer after inventory, before archiving.
        original_scan = state_backup._scan
        def changing_scan(directory):
            result = original_scan(directory)
            (directory / "workspaces/snapshot-fixture/state.txt").write_text("changed")
            return result
        with patch.object(state_backup, "_scan", changing_scan):
            try:
                state_backup.backup(root, base / "changed.zip")
            except state_backup.StateBackupError as exc:
                assert exc.code == "state_changed_during_backup"
            else:
                raise AssertionError("external_writer_not_detected")
        assert not (base / "changed.zip").exists()
        assert not list(base.glob(".trace-*-*"))
    print(json.dumps({"ok": True, "commands": commands, "checks": [
        "lifecycle", "json_jsonl", "env_provider", "secret_errors", "mcp_forwarding", "active_rejection",
        "wal_snapshot", "full_tree_restore", "tamper_rejection", "path_traversal_rejection",
        "schema_rejection", "target_preservation", "startup_self_test", "external_writer_rejection"]}, sort_keys=True))


if __name__ == "__main__":
    main()
