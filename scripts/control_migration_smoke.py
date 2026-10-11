"""Legacy credential migrations preserve data on failure and resolve MCP env references."""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web_control import ControlPlane
from redteam_agent.runtime.mcp_config import parse_mcp_server_specs


def legacy_ciphertext(key, value):
    nonce = b"n" * 16
    plain = value.encode("utf-8")
    stream = b"".join(hashlib.sha256(key + nonce + n.to_bytes(4, "big")).digest()
                      for n in range((len(plain) + 31) // 32))
    cipher = bytes(a ^ b for a, b in zip(plain, stream))
    return base64.urlsafe_b64encode(nonce + hmac.new(key, nonce + cipher, hashlib.sha256).digest()
                                   + cipher).decode("ascii")


def main():
    for failure in ("unreadable-key", "invalid-key", "invalid-ciphertext", "missing-reference", "none"):
        with tempfile.TemporaryDirectory(prefix="trace-control-migration-") as temporary:
            root = Path(temporary)
            with AgentService(root=root, load_external_configuration=False) as service:
                control = service.control
                control.save_provider({"provider_id": "legacy", "base_url": "https://fixture.invalid/v1",
                                       "model": "fixture", "api_key": ""})
                control.save_mcp({"server_id": "legacy", "transport": "http", "enabled": False,
                                  "url": "https://fixture.invalid/mcp",
                                  "headers": {"Authorization": "${TRACE_MCP_SECRET_FIXTURE}"}})
                key = b"k" * 32
                key_path = root / "trace-secrets.key"
                key_path.write_bytes(b"bad-key" if failure == "invalid-key" else key)
                rows = [("provider:legacy", legacy_ciphertext(key, "provider-fixture")),
                        ("TRACE_MCP_SECRET_FIXTURE", legacy_ciphertext(key, "mcp-fixture"))]
                if failure == "invalid-ciphertext":
                    rows[-1] = (rows[-1][0], base64.urlsafe_b64encode(b"bad" * 20).decode("ascii"))
                if failure == "missing-reference":
                    rows.pop()
                with control.store.transaction(immediate=True) as connection:
                    connection.execute("CREATE TABLE trace_secrets (secret_name TEXT PRIMARY KEY, ciphertext TEXT)")
                    connection.executemany("INSERT INTO trace_secrets VALUES (?,?)", rows)
                with control.store.connection() as connection:
                    before = tuple(connection.iterdump())
                original_read = Path.read_bytes

                def read(path):
                    if path == key_path and failure == "unreadable-key":
                        raise OSError("fixture read failure")
                    return original_read(path)

                try:
                    with patch.object(Path, "read_bytes", read):
                        migrated = ControlPlane(control.store, root)
                except (OSError, ValueError, KeyError):
                    assert failure != "none"
                    with control.store.connection() as connection:
                        assert tuple(connection.iterdump()) == before
                    assert key_path.exists()
                else:
                    assert failure == "none"
                    assert migrated.provider_secret("legacy", include_environment=False) == "provider-fixture"
                    with control.store.connection() as connection:
                        assert not connection.execute("SELECT 1 FROM sqlite_master WHERE name='trace_secrets'").fetchone()
                        spec = json.loads(connection.execute("SELECT spec_json FROM trace_mcp_servers").fetchone()[0])
                    assert spec["headers"]["Authorization"] == "mcp-fixture"
                    assert not key_path.exists()
                    ControlPlane(control.store, root)
    with tempfile.TemporaryDirectory(prefix="trace-mcp-env-") as temporary:
        with AgentService(root=Path(temporary), load_external_configuration=False) as service:
            service.control.save_mcp({"server_id": "refs", "transport": "stdio", "enabled": False,
                "command": sys.executable, "env": {"API_TOKEN": "${API_TOKEN}", "LITERAL": "plain-fixture"},
                "headers": {"Authorization": "Bearer ${API_TOKEN}"}})
            with service.control.store.connection() as connection:
                spec_json = json.loads(connection.execute("SELECT spec_json FROM trace_mcp_servers").fetchone()[0])
            spec, = parse_mcp_server_specs(Path(temporary) / "managed.toml", {"mcp_servers": {"refs": spec_json}})
            service.runtime.broker.set_secret_bindings(service.control.mcp_secret_bindings())
            with patch.dict(os.environ, {"API_TOKEN": "environment-fixture"}), \
                    patch("redteam_agent.runtime.mcp_broker.StdioMcpClient") as client:
                service.runtime.broker._create_client(spec)
                assert client.call_args.args[3] == {"API_TOKEN": "environment-fixture", "LITERAL": "plain-fixture"}
                assert spec.render().headers["Authorization"] == "Bearer environment-fixture"
    print("control migration smoke: failures preserve all data, successful plaintext migration, MCP env references passed")


if __name__ == "__main__":
    main()
