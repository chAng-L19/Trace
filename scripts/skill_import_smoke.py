"""Exercise external Skill import through the in-process Web API."""
from __future__ import annotations

import os
import sys
import tempfile
import threading
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch
from urllib.parse import quote

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi


def main() -> None:
    checks = 0
    with tempfile.TemporaryDirectory(prefix="trace-skill-import-") as directory:
        root = Path(directory)
        source_dir = root / "external-skill"
        source_dir.mkdir()
        content = "# External Skill\n\nUse the local fixture and report the result.\n"
        source = source_dir / "SKILL.md"
        source.write_text(content, encoding="utf-8")
        (root / "plain.md").write_text("# Plain Source\n\nImported over HTTP.\n", encoding="utf-8")
        empty_dir = root / "empty"
        empty_dir.mkdir()

        handler = partial(SimpleHTTPRequestHandler, directory=str(root))
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.dict(os.environ, {"TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "skill-pass"}, clear=False):
                with AgentService(root=root / "state", load_external_configuration=False) as service:
                    api = WebApi(service)
                    token = api.control.login("trace", "skill-pass")

                    def request(method: str, path: str, body=None, status: int = 200):
                        response = api.dispatch(method, path, body=body, headers={
                            "authorization": "Bearer " + token,
                            "x-command-id": f"skill-{os.urandom(6).hex()}",
                        })
                        assert response.status == status, (method, path, response.status, response.payload())
                        return response.payload()

                    item = request("POST", "/api/skills/import", {"source": str(source), "enabled": False}, 201)["skill"]
                    assert item["resource_id"] == "skill:external/external-skill"
                    assert item["enabled"] is False and item["byte_count"] == len(source.read_bytes())
                    assert "content" not in request("GET", "/api/skills")["skills"][0]
                    checks += 3

                    item = request("POST", "/api/skills/import", {"source": str(source_dir), "resource_id": "skill:external/explicit"}, 201)["skill"]
                    assert item["resource_id"] == "skill:external/explicit"
                    request("POST", "/api/skills/" + quote(item["resource_id"], safe=""), {"enabled": False})
                    assert request("GET", "/api/skills/" + quote(item["resource_id"], safe=""))["skill"]["enabled"] is False
                    request("DELETE", "/api/skills/" + quote(item["resource_id"], safe=""))
                    checks += 3

                    url = f"http://127.0.0.1:{server.server_port}/plain.md"
                    item = request("POST", "/api/skills/import", {"source": url}, 201)["skill"]
                    assert item["resource_id"] == "skill:external/plain"
                    checks += 1

                    item = request("POST", "/api/skills/import", {
                        "file_name": "uploaded.md", "content": "\ufeff# Uploaded\r\nBrowser file.\r\n",
                        "enabled": False}, 201)["skill"]
                    assert item["resource_id"] == "skill:external/uploaded" and not item["enabled"]
                    saved = request("GET", "/api/skills/" + quote(item["resource_id"], safe=""))["skill"]
                    assert saved["content"] == "# Uploaded\nBrowser file.\n"
                    checks += 2

                    for bad_source, expected in ((str(empty_dir), "skill_import_file_not_found"),
                                                 (str(root / "missing.md"), "skill_import_file_not_found")):
                        response = api.dispatch("POST", "/api/skills/import", body={"source": bad_source}, headers={
                            "authorization": "Bearer " + token,
                            "x-command-id": f"skill-{os.urandom(6).hex()}",
                        })
                        assert response.status == 400 and response.payload()["error"] == expected
                        checks += 1

                    oversized = root / "oversized.md"
                    oversized.write_text("x" * (64 * 1024 + 1), encoding="utf-8")
                    response = api.dispatch("POST", "/api/skills/import", body={"source": str(oversized)}, headers={
                        "authorization": "Bearer " + token,
                        "x-command-id": f"skill-{os.urandom(6).hex()}",
                    })
                    assert response.status == 400 and response.payload()["error"] == "skill_import_too_large"
                    checks += 1

                    restarted = AgentService(root=root / "state", load_external_configuration=False)
                    try:
                        assert any(item["resource_id"] == "skill:external/plain" for item in restarted.control.skills())
                        checks += 1
                    finally:
                        restarted.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)
    print(f"skill_import_smoke: PASS ({checks} checks; local file/directory/http/restart/errors)")


if __name__ == "__main__":
    main()
