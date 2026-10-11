"""Local authorization fixture for Trace tool integration tests."""
from __future__ import annotations

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class AuthzHandler(BaseHTTPRequestHandler):
    server_version = "TraceAuthzLab/1.0"

    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path == "/":
            self._send(200, {"service": "authz-lab", "routes": ["/api/resource/1", "/api/resource/2"]})
            return
        prefix = "/api/resource/"
        if not self.path.startswith(prefix):
            self._send(404, {"error": "not_found"})
            return
        resource_id = self.path[len(prefix):].split("?", 1)[0]
        owner = self.headers.get("X-User-ID", "")
        if resource_id not in {"1", "2"}:
            self._send(404, {"error": "resource_not_found"})
            return
        allowed = owner == resource_id or (resource_id == "2" and owner == "1")
        self._send(200 if allowed else 403, {
            "resource_id": resource_id,
            "owner": resource_id,
            "request_user": owner or None,
            "allowed": allowed,
            "content": f"fixture-resource-{resource_id}" if allowed else None,
        })

    def _send(self, status: int, payload: dict):
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8781)
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), AuthzHandler)
    print(f"http://127.0.0.1:{server.server_port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
