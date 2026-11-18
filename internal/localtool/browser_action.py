"""Trace JSONL adapter for CloakBrowser 0.6.4; one headed browser per call.

Upstream: CloakHQ/CloakBrowser b1a00eda61efe71da2a1a38831fa6fe861f5d91d
browser.py launch()/close(): official SDK owns its Playwright driver lifecycle.
"""
import contextlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import traceback


def interrupted(signum, frame):
    raise InterruptedError("browser action cancelled")


def set_document(page, html, *, wait_until="load", ready_selector=None, timeout=10000):
    """Explicit document replacement, not a patch for Page.set_content.

    The write executes once. Timeout/error propagates; document/script side
    effects may already exist and must be inspected, never automatically retried.
    """
    if not isinstance(html, str):
        raise ValueError("html must be a string")
    if wait_until not in ("load", "domcontentloaded"):
        raise ValueError("wait_until must be load or domcontentloaded")
    if ready_selector is not None and (not isinstance(ready_selector, str) or not ready_selector.strip()):
        raise ValueError("ready_selector must be a nonempty CSS selector")
    if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not 0 < timeout <= 30000:
        raise ValueError("timeout must be greater than 0 and at most 30000 ms")
    deadline = time.monotonic() + timeout / 1000
    original_url = page.url
    def remaining():
        value = (deadline - time.monotonic()) * 1000
        if value <= 0:
            raise TimeoutError("set_document timed out; document effects may already exist")
        return value
    # Never put the write in wait_for_function: context destruction can retry its
    # predicate. evaluate writes once; the outer JSONL worker bounds execution.
    page.evaluate(
        "html => { document.open(); document.write(html); document.close(); return true; }",
        html,
    )
    ready = page.wait_for_function(
        "([url, until, selector]) => { if(location.href !== url) throw new Error('set_document URL changed'); return (until === 'load' ? document.readyState === 'complete' : document.readyState !== 'loading') && (!selector || !!document.querySelector(selector)); }",
        arg=[original_url, wait_until, ready_selector], timeout=remaining(),
    )
    ready.dispose()
    if page.url != original_url:
        raise RuntimeError("set_document URL changed; document effects may already exist")
    return {"url": original_url, "wait_until": wait_until, "ready_selector": ready_selector}


def main():
    request = json.loads(sys.stdin.readline())
    if not isinstance(request, dict) or not isinstance(request.get("code"), str):
        raise ValueError("code must be a Python string")
    args = request.get("args", {})
    if not isinstance(args, dict):
        raise ValueError("args must be an object")
    if not os.environ.get("TRACE_BROWSER_RUN_ID"):
        raise ValueError("Trace Run identity is required")
    import psutil
    root = psutil.Process()
    private_dir = display = browser = context = None
    response = {"ok": False, "cleanup": "unknown"}
    try:
        # Keep SDK-created profiles/download artifacts inside this call's own
        # temporary directory, including when browser startup raises.
        private_dir = Path(tempfile.mkdtemp(prefix="trace-cloakbrowser-"))
        os.environ.update(TMPDIR=str(private_dir), TMP=str(private_dir), TEMP=str(private_dir))
        tempfile.tempdir = str(private_dir)
        if sys.platform.startswith("linux") and not os.environ.get("DISPLAY"):
            xvfb = shutil.which("xvfb-run")
            if not xvfb:
                raise FileNotFoundError("Headed CloakBrowser requires DISPLAY or xvfb-run with xauth")
            # The worker owner remains this Python process, not xvfb-run's
            # shell. Therefore SIGTERM interrupts the actual browser action.
            holder = "import json,os,sys; print(json.dumps({k:os.environ[k] for k in ('DISPLAY','XAUTHORITY') if k in os.environ}),flush=True); sys.stdin.read()"
            display = subprocess.Popen(
                [xvfb, "-a", "--server-args=-screen 0 1280x720x24 -nolisten tcp", sys.executable, "-c", holder],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=sys.stderr, text=True,
            )
            line = display.stdout.readline()
            if not line:
                raise RuntimeError("xvfb-run exited before its display was ready")
            os.environ.update(json.loads(line))
        with contextlib.redirect_stdout(sys.stderr):
            import cloakbrowser
            if cloakbrowser.__version__ != "0.6.4":
                raise RuntimeError("cloakbrowser==0.6.4 required")
            executable = os.environ.get("CLOAKBROWSER_BINARY_PATH") or cloakbrowser.binary_info()["binary_path"]
            if not Path(executable).is_file():
                raise FileNotFoundError("CloakBrowser Chromium missing; preinstall it or set CLOAKBROWSER_BINARY_PATH")
            # The SDK's explicit local-binary route never downloads or updates.
            # Do not resolve old TRACE_BROWSER_EXECUTABLE/host Chromium values.
            os.environ["CLOAKBROWSER_BINARY_PATH"] = executable
            # Never substitute host Chromium, DrissionPage, or a CLI on error.
            browser = cloakbrowser.launch(headless=False, handle_sigterm=False, handle_sigint=False)
            context = browser.new_context()
            context.set_default_timeout(10000)
            context.set_default_navigation_timeout(30000)
            page = context.new_page()
            namespace = {"page": page, "context": context, "browser": browser, "args": args, "result": None, "set_document": lambda html, **options: set_document(page, html, **options)}
            exec(compile(request["code"], "<cloakbrowser-action>", "exec"), namespace)
        response.update(ok=True, result=namespace["result"])
        # Fail serialization before printing, preserving one complete JSONL record.
        json.dumps(response, ensure_ascii=False, allow_nan=False)
    except BaseException as error:
        traceback.print_exc(file=sys.stderr)
        response = {"ok": False, "error": type(error).__name__ + ": " + str(error), "cleanup": "unknown"}
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_errors = []
        owned = []
        try:
            owned = root.children(recursive=True)
        except Exception as error:
            cleanup_errors.append(str(error))
        for resource in (context, browser):
            if resource is not None:
                try:
                    with contextlib.redirect_stdout(sys.stderr):
                        resource.close()
                except BaseException as error:
                    cleanup_errors.append(str(error))
        if display is not None:
            try:
                display.stdin.close()
                display.wait(timeout=2)
                display.stdout.close()
                if display.returncode:
                    cleanup_errors.append("xvfb-run exited with status " + str(display.returncode))
            except BaseException as error:
                cleanup_errors.append(str(error))
        try:
            # Includes the SDK driver, its Chromium process group, and the
            # display helper, never an operator's pre-existing browser.
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
                cleanup_errors.append("owned browser/display processes still running")
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
    print(json.dumps(response, ensure_ascii=False, allow_nan=False))
    return 0 if response["ok"] else 1


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        sys.exit(main())
    except Exception as error:
        print(json.dumps({"ok": False, "error": type(error).__name__ + ": " + str(error), "cleanup": "unknown"}))
        sys.exit(1)
