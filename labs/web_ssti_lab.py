"""Local deterministic template-injection fixture for Trace tool tests."""
from __future__ import annotations

import argparse
import html
import re
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit


class SstiHandler(BaseHTTPRequestHandler):
    server_version = "TraceSstiLab/1.0"

    def log_message(self, *_args):
        pass

    def do_GET(self):
        query = parse_qs(urlsplit(self.path).query)
        name = query.get("name", ["visitor"])[0]

        # Deliberately tiny fixture behavior: only arithmetic expressions are
        # evaluated, keeping the lab deterministic and side-effect free.
        def evaluate(match):
            expression = match.group(1).strip()
            if not re.fullmatch(r"[0-9 ]+[+*\-/][0-9 ]+", expression):
                return "[template-error]"
            try:
                left, operator, right = re.fullmatch(r"([0-9 ]+)([+*\-/])([0-9 ]+)", expression).groups()
                values = int(left), int(right)
                operations = {"+": lambda a, b: a + b, "-": lambda a, b: a - b,
                              "*": lambda a, b: a * b, "/": lambda a, b: a // b}
                return str(operations[operator](*values))
            except (ArithmeticError, ValueError):
                return "[template-error]"

        rendered = re.sub(r"\{\{(.*?)\}\}", evaluate, name)
        body = f"<html><body><h1>Hello, {rendered}</h1></body></html>".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8782)
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), SstiHandler)
    print(f"http://127.0.0.1:{server.server_port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
