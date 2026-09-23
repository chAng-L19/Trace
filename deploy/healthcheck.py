"""Check a real Trace HTTP response without provider calls or credentials."""
from __future__ import annotations

import json
import os
import sys
from urllib.request import ProxyHandler, build_opener


def main() -> int:
    url = os.environ.get("TRACE_HEALTH_URL") or (
        "http://127.0.0.1:" + os.environ.get("TRACE_WEB_PORT", "8765") + "/api/auth/status"
    )
    try:
        with build_opener(ProxyHandler({})).open(url, timeout=3) as response:
            payload = json.loads(response.read(8192))
            valid = (
                response.status == 200
                and response.headers.get("X-Trace-Schema-Version") == "1"
                and payload.get("ok") is True
                and isinstance(payload.get("required"), bool)
                and isinstance(payload.get("authenticated"), bool)
            )
        return 0 if valid else 1
    except Exception:
        # Do not print URLs, headers, environment values or error bodies.
        return 1


if __name__ == "__main__":
    sys.exit(main())
