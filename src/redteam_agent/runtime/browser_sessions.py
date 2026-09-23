from __future__ import annotations

import asyncio
import tempfile
import threading
from concurrent.futures import CancelledError, TimeoutError as FutureTimeout
from pathlib import Path
from typing import Any, Mapping
from uuid import uuid4

from .managed_tools import chromium_executable


def _chrome_path() -> str:
    return chromium_executable()


class BrowserAdapter:
    def __init__(self, operation: str) -> None:
        self.operation = operation

    def __call__(self, arguments: Mapping[str, Any]) -> Mapping[str, Any]:
        sessions = BrowserSessions()
        try:
            return sessions.call(self.operation, arguments)
        finally:
            sessions.close()


class BrowserSessions:
    """One broker-owned Playwright worker; browser state never crosses runs."""

    def __init__(self) -> None:
        self._guard = threading.Lock()
        self._thread: threading.Thread | None = None
        self._ready = threading.Event()
        self._shutdown_done = threading.Event()
        self._closed = False
        self._loop: asyncio.AbstractEventLoop | None = None
        self._driver: Any = None
        self._sessions: dict[str, dict[str, Any]] = {}
        self._starting: set[str] = set()
        self._closed_runs: set[str] = set()
        self._closing_runs: dict[str, asyncio.Future[Any]] = {}
        self._disposing: dict[str, asyncio.Task[Mapping[str, Any]]] = {}
        self._cleanup_reports: dict[str, tuple[Mapping[str, Any], ...]] = {}
        self._tasks: dict[asyncio.Task[Any], str] = {}
        # ponytail: serialize browser operations; use per-run locks if throughput matters.
        self._operations = asyncio.Lock()

    def _worker(self) -> None:
        self._loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self._loop)
        self._ready.set()
        try:
            self._loop.run_forever()
        finally:
            self._loop.close()

    def _submit(self, coroutine: Any) -> Any:
        with self._guard:
            if self._closed:
                coroutine.close()
                raise RuntimeError("browser_sessions_closed")
            if self._thread is None:
                self._thread = threading.Thread(target=self._worker, name="playwright-sessions", daemon=True)
                self._thread.start()
            self._ready.wait()
            return asyncio.run_coroutine_threadsafe(coroutine, self._loop)

    def call(
        self, operation: str, arguments: Mapping[str, Any], *, run_id: str = "",
        workspace: Path | None = None, timeout: float = 60.0,
    ) -> Mapping[str, Any]:
        if not run_id and (arguments.get("session_id") or arguments.get("page_id")):
            raise ValueError("browser_run_id_required")
        key = run_id or f"ephemeral-{uuid4().hex}"
        future = self._submit(self._call(key, operation, dict(arguments), workspace, ephemeral=not run_id))
        try:
            return future.result(timeout=max(0.001, timeout))
        except FutureTimeout:
            try:
                self._submit(self._close_run(key, lost=True))
            except RuntimeError:
                if not self._closed:
                    raise
            raise TimeoutError("browser_timeout:outcome_unknown:session_lost") from None
        except CancelledError:
            raise ConnectionError("browser_call_cancelled:outcome_unknown:session_lost") from None

    async def _call(
        self, key: str, operation: str, arguments: Mapping[str, Any],
        workspace: Path | None, *, ephemeral: bool,
    ) -> Mapping[str, Any]:
        task = asyncio.current_task()
        self._tasks[task] = key
        try:
            async with self._operations:
                if key in self._closing_runs:
                    raise RuntimeError("browser_session_closing")
                if key in self._closed_runs and operation != "create":
                    raise RuntimeError("session_lost:browser-create_required")
                url = str(arguments.get("url") or "").strip()
                if operation == "navigate" and not url:
                    raise ValueError("url_required")
                if operation in {"click", "fill"} and not str(arguments.get("selector") or "").strip():
                    raise ValueError("selector_required")
                if operation == "evaluate" and not str(arguments.get("expression") or "").strip():
                    raise ValueError("expression_required")
                session = self._sessions.get(key)
                marker = workspace / ".playwright-session" if workspace else None
                for field in ("session_id", "page_id"):
                    if arguments.get(field) and (session is None or arguments[field] != session[field]):
                        raise RuntimeError("session_lost:browser-create_required")
                if operation == "create":
                    if session:
                        report = await self._dispose(key)
                        if report["errors"]:
                            raise RuntimeError("browser_cleanup_failed")
                    self._closed_runs.discard(key)
                    session = None
                elif session is None and (
                    arguments.get("session_id") or arguments.get("page_id") or (marker and marker.exists())
                ):
                    raise RuntimeError("session_lost:browser-create_required")
                if session is None:
                    if not url and operation != "create":
                        raise ValueError("url_required")
                    self._starting.add(key)
                    try:
                        session = await self._create(key, arguments, marker)
                    finally:
                        self._starting.discard(key)
                if key in self._closing_runs:
                    raise ConnectionError("browser_call_cancelled:outcome_unknown:session_lost")
                if not self._alive(session):
                    await self._dispose(key, lost=True)
                    raise RuntimeError("session_lost:browser-create_required")
                try:
                    seconds = max(1.0, min(300.0, float(arguments.get("timeout", 30.0))))
                except (TypeError, ValueError, OverflowError):
                    seconds = 30.0
                try:
                    result = await asyncio.wait_for(self._operate(session, operation, arguments, url, seconds), seconds)
                except asyncio.TimeoutError:
                    await self._dispose(key, lost=True)
                    raise TimeoutError("browser_timeout:outcome_unknown:session_lost") from None
                except Exception as exc:
                    if not self._alive(session):
                        await self._dispose(key, lost=True)
                        raise ConnectionError("session_lost:outcome_unknown:browser-create_required") from exc
                    raise
                if not self._alive(session):
                    await self._dispose(key, lost=True)
                    raise ConnectionError("session_lost:outcome_unknown:browser-create_required")
                return {**result, "session_id": session["session_id"], "page_id": session["page_id"]}
        finally:
            self._tasks.pop(task, None)
            if ephemeral:
                await self._dispose(key)

    @staticmethod
    def _alive(session: Mapping[str, Any]) -> bool:
        return bool(session["browser"].is_connected() and session.get("page") and not session["page"].is_closed())

    async def _create(self, key: str, arguments: Mapping[str, Any], marker: Path | None) -> dict[str, Any]:
        if self._driver is not None and not self._closing_runs and not any(item["browser"].is_connected() for item in self._sessions.values()):
            try:
                await self._driver.stop()
            except Exception:
                pass
            self._driver = None
        if self._driver is None:
            try:
                from playwright.async_api import async_playwright
            except ImportError as exc:
                raise RuntimeError("playwright_python_package_missing") from exc
            self._driver = await async_playwright().start()
        options: dict[str, Any] = {"headless": bool(arguments.get("headless", True))}
        executable = str(arguments.get("executable_path") or _chrome_path()).strip()
        if executable:
            options["executable_path"] = executable
        fallback = Path(tempfile.gettempdir()) / "trace-browser" / uuid4().hex
        workspace = (marker.parent if marker else fallback).resolve()
        workspace.mkdir(parents=True, exist_ok=True)
        session = {"session_id": uuid4().hex, "page_id": uuid4().hex, "marker": marker,
                   "workspace": workspace,
                   "browser": await self._driver.chromium.launch(**options), "context": None, "page": None}
        self._sessions[key] = session
        try:
            session["context"] = await session["browser"].new_context(
                ignore_https_errors=bool(arguments.get("ignore_https_errors", False)))
            session["page"] = await session["context"].new_page()
            if marker:
                marker.write_text(session["session_id"], encoding="ascii")
            return session
        except BaseException:
            await self._dispose(key, lost=True)
            raise

    async def _operate(
        self, session: Mapping[str, Any], operation: str, arguments: Mapping[str, Any], url: str, seconds: float,
    ) -> Mapping[str, Any]:
        page = session["page"]
        timeout = int(seconds * 1000)
        response = None
        if url and (operation in {"create", "navigate"} or page.url != url):
            response = await page.goto(url, wait_until=str(arguments.get("wait_until") or "domcontentloaded"), timeout=timeout)
        extra: dict[str, Any] = {}
        if operation in {"click", "fill"}:
            locator = page.locator(str(arguments["selector"])).first
            if operation == "click":
                await locator.click(timeout=timeout)
            else:
                await locator.fill(str(arguments.get("value") or ""), timeout=timeout)
        elif operation == "evaluate":
            extra["value"] = await page.evaluate(str(arguments["expression"]), arguments.get("arg"))
        elif operation == "screenshot":
            root = session["workspace"]
            requested = Path(str(arguments.get("output_path") or "browser-screenshot.png")).expanduser()
            output = (requested if requested.is_absolute() else root / requested).resolve()
            try:
                output.relative_to(root)
            except ValueError as exc:
                raise ValueError("browser_output_path_outside_workspace") from exc
            output.parent.mkdir(parents=True, exist_ok=True)
            await page.screenshot(path=str(output), full_page=bool(arguments.get("full_page", False)), timeout=timeout)
            extra["screenshot_path"] = str(output)
        title, text, links = "", "", []
        try:
            title = await page.title()
            text = await page.locator("body").inner_text(timeout=timeout)
            links = await page.locator("a").evaluate_all(
                "els => els.slice(0, 100).map(e => ({text: (e.innerText || '').trim(), href: e.href}))")
        except Exception:
            pass
        return {"operation": operation, "requested_url": url, "url": page.url, "title": title,
                "status_code": int(response.status) if response is not None else 0,
                "text": text[:256 * 1024], "text_truncated": len(text) > 256 * 1024, "links": links, **extra}

    async def _dispose(self, key: str, *, lost: bool = False) -> Mapping[str, Any]:
        if lost:
            self._closed_runs.add(key)
        task = self._disposing.get(key)
        if task is None:
            task = asyncio.create_task(self._dispose_session(key, lost=lost))
            self._disposing[key] = task
        try:
            return await asyncio.shield(task)
        finally:
            if task.done() and key not in self._closing_runs and self._disposing.get(key) is task:
                self._disposing.pop(key, None)

    async def _dispose_session(self, key: str, *, lost: bool = False) -> Mapping[str, Any]:
        session = self._sessions.pop(key, None)
        closed: list[str] = []
        errors: list[str] = []
        if lost:
            self._closed_runs.add(key)
        if session:
            for kind in ("page", "context", "browser"):
                resource = session.get(kind)
                if resource is not None:
                    try:
                        await resource.close()
                        closed.append(kind)
                    except Exception as exc:
                        errors.append(f"{kind}:{type(exc).__name__}")
            if session["marker"] and not lost and not errors:
                try:
                    if session["marker"].exists() and session["marker"].read_text(encoding="ascii") == session["session_id"]:
                        session["marker"].unlink()
                except OSError as exc:
                    errors.append(f"marker:{type(exc).__name__}")
        report = {"server": "builtin:playwright", "preset": "playwright", "run_id": key,
                  "resources_discovered": sum(session.get(kind) is not None for kind in ("page", "context", "browser")) if session else 0,
                  "resources_closed": closed, "errors": errors, "status": "failed" if errors else "closed"}
        if session and (lost or key in self._closing_runs):
            self._cleanup_reports[key] = (*self._cleanup_reports.get(key, ()), report)
        return report

    async def _close_run(self, key: str, *, lost: bool = False) -> tuple[Mapping[str, Any], ...]:
        pending = self._closing_runs.get(key)
        if pending is not None:
            return await asyncio.shield(pending)
        pending = asyncio.get_running_loop().create_future()
        self._closing_runs[key] = pending
        self._closed_runs.add(key)
        try:
            tasks = [task for task, run in self._tasks.items() if run == key]
            for task in tasks:
                # A launch must yield its browser handle before cancellation can close it.
                if key not in self._starting or key in self._sessions:
                    task.cancel()
            if tasks:
                await asyncio.gather(*tasks, return_exceptions=True)
            reports = (
                (await self._dispose(key, lost=lost),)
                if key in self._sessions or key in self._disposing
                else ()
            )
            pending.set_result(reports)
            return reports
        finally:
            if not pending.done():
                pending.cancel()
            disposing = self._disposing.get(key)
            if disposing is not None and disposing.done():
                self._disposing.pop(key, None)
            self._closing_runs.pop(key, None)

    def close_run(self, run_id: str) -> tuple[Mapping[str, Any], ...]:
        with self._guard:
            closed = self._closed
            started = self._thread is not None
        if closed:
            self._shutdown_done.wait(timeout=15.0)
            with self._guard:
                return self._cleanup_reports.pop(run_id, ())
        if not started:
            return ()
        try:
            future = self._submit(self._close_and_drain(run_id))
        except RuntimeError as exc:
            if str(exc) != "browser_sessions_closed":
                raise
            self._shutdown_done.wait(timeout=15.0)
            with self._guard:
                return self._cleanup_reports.pop(run_id, ())
        return future.result(timeout=15.0)

    async def _close_and_drain(self, run_id: str) -> tuple[Mapping[str, Any], ...]:
        await self._close_run(run_id)
        return self._cleanup_reports.pop(run_id, ())

    async def _shutdown(self) -> None:
        results = await asyncio.gather(*(
            self._close_run(key)
            for key in set(self._sessions) | set(self._tasks.values()) | set(self._disposing)
        ), return_exceptions=True)
        errors = [result for result in results if isinstance(result, BaseException)]
        for reports in results:
            if isinstance(reports, (list, tuple)):
                for report in reports:
                    if report.get("errors"):
                        errors.append(RuntimeError("browser_resource_cleanup_failed:" + ",".join(report["errors"])))
        if self._driver is not None:
            try:
                await self._driver.stop()
            except BaseException as exc:
                errors.append(exc)
            finally:
                self._driver = None
        self._sessions.clear()
        self._starting.clear()
        self._closing_runs.clear()
        self._disposing.clear()
        self._tasks.clear()
        if errors:
            raise BaseExceptionGroup("browser_cleanup_failed", errors)

    def close(self) -> None:
        with self._guard:
            if self._closed:
                shutdown_done = self._shutdown_done
                already_closed = True
            else:
                self._closed = True
                loop, thread = self._loop, self._thread
                shutdown_done = self._shutdown_done
                already_closed = False
        if already_closed:
            shutdown_done.wait(timeout=15.0)
            return
        try:
            if loop is not None:
                try:
                    asyncio.run_coroutine_threadsafe(self._shutdown(), loop).result(timeout=15.0)
                finally:
                    loop.call_soon_threadsafe(loop.stop)
                    thread.join(timeout=2.0)
        finally:
            shutdown_done.set()
