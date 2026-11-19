"""Trace JSONL Lightpanda adapter: one owned local CDP server per action.

Uses the official lightpanda serve/CDP contract through installed Playwright.
No upstream agent loop, browser downloads or automatic action replay.
"""
import contextlib
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import traceback


def interrupted(signum, frame):
    raise InterruptedError("browser action cancelled")


def main(protocol_output):
    request = json.loads(sys.stdin.readline())
    if not isinstance(request, dict) or not isinstance(request.get("code"), str):
        raise ValueError("code must be a Python string")
    args = request.get("args", {})
    if not isinstance(args, dict):
        raise ValueError("args must be an object")
    if not os.environ.get("TRACE_BROWSER_RUN_ID"):
        raise ValueError("Trace Run identity is required")
    import psutil
    from playwright.sync_api import sync_playwright
    root = psutil.Process()
    private_dir = server = driver = browser = context = None
    response = {"ok": False, "backend": "lightpanda", "cleanup": "unknown"}
    try:
        private_dir = Path(tempfile.mkdtemp(prefix="trace-lightpanda-"))
        os.environ.update(TMPDIR=str(private_dir), TMP=str(private_dir), TEMP=str(private_dir),
                          XDG_CACHE_HOME=str(private_dir / "cache"),
                          XDG_CONFIG_HOME=str(private_dir / "config"),
                          LIGHTPANDA_DISABLE_TELEMETRY="true")
        tempfile.tempdir = str(private_dir)
        executable = os.environ.get("LIGHTPANDA_BINARY_PATH") or shutil.which("lightpanda")
        if not executable or not Path(executable).is_file():
            raise FileNotFoundError("preinstall Lightpanda or set LIGHTPANDA_BINARY_PATH")
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        # One startup attempt. Poll readiness only; never retry user code.
        server = subprocess.Popen([executable, "serve", "--host", "127.0.0.1", "--port", str(port)],
                                  stdin=subprocess.DEVNULL, stdout=sys.stderr, stderr=sys.stderr)
        deadline = time.monotonic() + 10
        while True:
            if server.poll() is not None:
                raise RuntimeError("Lightpanda exited before CDP was ready: " + str(server.returncode))
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    listeners = psutil.Process(server.pid).net_connections(kind="tcp")
                    if not any(c.status == psutil.CONN_LISTEN and c.laddr.ip == "127.0.0.1" and c.laddr.port == port for c in listeners):
                        raise RuntimeError("CDP listener does not belong to this Lightpanda process")
                    break
            except InterruptedError:
                raise
            except OSError:
                if time.monotonic() >= deadline:
                    raise TimeoutError("Lightpanda CDP startup timed out")
                time.sleep(0.05)
        with contextlib.redirect_stdout(sys.stderr):
            driver = sync_playwright().start()
            browser = driver.chromium.connect_over_cdp("ws://127.0.0.1:" + str(port), timeout=10000)
            # CDP uses a new connection after readiness. Verify its socket
            # peers before creating any context: a listening-port observation
            # alone cannot identify that later WebSocket session.
            server_connections = [] if server.poll() is not None else psutil.Process(server.pid).net_connections(kind="tcp")
            server_peers = {(c.laddr, c.raddr) for c in server_connections
                            if c.status == psutil.CONN_ESTABLISHED and c.laddr.ip == "127.0.0.1" and c.laddr.port == port}
            client_peers = {(c.raddr, c.laddr) for child in root.children(recursive=True) if child.pid != server.pid
                            for c in child.net_connections(kind="tcp")
                            if c.status == psutil.CONN_ESTABLISHED and c.raddr.ip == "127.0.0.1" and c.raddr.port == port}
            if len(client_peers) != 1 or len(server_peers.intersection(client_peers)) != 1:
                raise RuntimeError("CDP session does not belong to this Lightpanda process")
            context = browser.new_context()
            context.set_default_timeout(10000)
            context.set_default_navigation_timeout(30000)
            page = context.new_page()
            namespace = {"page": page, "context": context, "browser": browser, "args": args, "result": None}
            exec(compile(request["code"], "<lightpanda-action>", "exec"), namespace)
        response.update(ok=True, result=namespace["result"])
        json.dumps(response, ensure_ascii=False, allow_nan=False)
    except BaseException as error:
        traceback.print_exc(file=sys.stderr)
        response = {"ok": False, "backend": "lightpanda", "error": type(error).__name__ + ": " + str(error), "cleanup": "unknown"}
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_errors = []
        owned = []
        try:
            owned = root.children(recursive=True)
        except Exception as error:
            cleanup_errors.append(str(error))
        # This call exclusively owns the server and driver. Terminating those
        # processes also closes their private context; no remote CDP close RPC
        # can block cleanup when the driver is unresponsive.
        try:
            owned.extend(p for p in root.children(recursive=True) if p not in owned)
            for child in owned:
                try:
                    child.terminate()
                except psutil.NoSuchProcess:
                    pass
            _, alive = psutil.wait_procs(owned, timeout=2)
            for child in alive:
                try:
                    child.kill()
                except psutil.NoSuchProcess:
                    pass
            _, alive = psutil.wait_procs(alive, timeout=2)
            if any(p.is_running() and p.status() != psutil.STATUS_ZOMBIE for p in alive):
                cleanup_errors.append("owned browser/driver processes still running")
            if server is not None:
                server.wait(timeout=2)
        except BaseException as error:
            cleanup_errors.append(str(error))
        if private_dir is not None:
            try:
                shutil.rmtree(private_dir)
            except OSError as error:
                cleanup_errors.append(str(error))
        response["cleanup"] = "failed" if cleanup_errors else "completed"
        if cleanup_errors:
            response.update(ok=False, cleanup_errors=cleanup_errors)
    print(json.dumps(response, ensure_ascii=False, allow_nan=False), file=protocol_output)
    return 0 if response["ok"] else 1


if __name__ == "__main__":
    # Reserve the protocol pipe and route fd 1 diagnostics to stderr as well as
    # Python print(): native libraries and subprocesses inherit fd 1.
    with os.fdopen(os.dup(sys.stdout.fileno()), "w", encoding="utf-8") as protocol_output:
        os.dup2(sys.stderr.fileno(), sys.stdout.fileno())
        sys.stdout = sys.stderr
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGINT, interrupted)
        try:
            sys.exit(main(protocol_output))
        except Exception as error:
            print(json.dumps({"ok": False, "backend": "lightpanda", "error": type(error).__name__ + ": " + str(error), "cleanup": "unknown"}), file=protocol_output)
            sys.exit(1)
