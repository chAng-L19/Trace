"""Trace's browser MCP adapter over BrowserSessions; not the Microsoft MCP server."""
from __future__ import annotations

import argparse
import json
import signal
import sys
import tempfile
from contextlib import ExitStack
from pathlib import Path
from typing import Any, Mapping
from uuid import uuid4

from .browser_sessions import BrowserSessions
from .mcp_transport import _serve_stdio
from .security import safe_error_text
from .tool_broker import ToolBroker


def _tool(name, description, properties=None, required=(), *, read_only=False):
    return {"name": name, "description": description,
            "inputSchema": {"type": "object", "properties": properties or {}, "required": list(required)},
            "annotations": {"readOnlyHint": read_only}}


_STRING = {"type": "string"}
_SELECTOR = {"type": "string", "description": "Playwright selector, e.g. #login or role=button[name=Submit]."}
TOOLS = (
    _tool("browser_navigate", "Navigate the current tab to a URL.", {"url": _STRING}, ("url",)),
    _tool("browser_navigate_back", "Go back in the current tab."),
    _tool("browser_navigate_forward", "Go forward in the current tab."),
    _tool("browser_snapshot", "Read page text and accessibility snapshot.", read_only=True),
    _tool("browser_find", "Read elements matching a Playwright selector.", {"selector": _SELECTOR}, ("selector",), read_only=True),
    _tool("browser_click", "Click an element.", {"selector": _SELECTOR}, ("selector",)),
    _tool("browser_type", "Fill or type into an element; optionally submit with Enter.",
          {"selector": _SELECTOR, "text": _STRING, "slowly": {"type": "boolean"}, "submit": {"type": "boolean"}}, ("selector", "text")),
    _tool("browser_fill_form", "Fill text, checkbox and select fields using selectors.", {"fields": {
        "type": "array", "items": {"type": "object", "properties": {"selector": _SELECTOR,
        "type": {"type": "string", "enum": ["text", "checkbox", "select"]}, "value": {}},
        "required": ["selector", "value"]}}}, ("fields",)),
    _tool("browser_select_option", "Select values from a dropdown.",
          {"selector": _SELECTOR, "values": {"type": "array", "items": _STRING}}, ("selector", "values")),
    _tool("browser_press_key", "Press a key or chord in the current tab.", {"key": _STRING}, ("key",)),
    _tool("browser_wait_for", "Wait for text to appear/disappear or for a duration in seconds.",
          {"text": _STRING, "textGone": _STRING, "time": {"type": "number"}}),
    _tool("browser_tabs", "List, create, select or close tabs by zero-based index.",
          {"action": {"type": "string", "enum": ["list", "new", "select", "close"]}, "index": {"type": "integer"}}, ("action",)),
    _tool("browser_network_requests", "Read captured request URLs, methods, HTTP statuses and failures.", read_only=True),
    _tool("browser_console_messages", "Read captured console messages and page errors.", read_only=True),
    _tool("browser_take_screenshot", "Save a screenshot in this run's output directory.",
          {"output_path": _STRING, "full_page": {"type": "boolean"}}, read_only=True),
    _tool("browser_evaluate", "Evaluate a JavaScript expression or function in the page (may change state).",
          {"expression": _STRING, "arg": {}}, ("expression",)),
    _tool("browser_close", "Close this MCP process's browser session; later calls start a fresh session."),
)
_TOOLS_BY_NAME = {tool["name"]: tool for tool in TOOLS}


class McpBrowserSessions(BrowserSessions):
    """Extend page operations on the existing browser worker and cleanup lifecycle."""

    async def _create(self, key, arguments, marker):
        session = await super()._create(key, arguments, marker)
        session.update(network=[], console=[], requests={})

        def request_started(request):
            item = {"url": request.url, "method": request.method, "resource_type": request.resource_type}
            session["network"].append(item)
            session["requests"][request] = item

        def response_received(response):
            item = session["requests"].get(response.request)
            if item is not None:
                item["status"] = response.status

        def request_finished(request):
            session["requests"].pop(request, None)

        def request_failed(request):
            item = session["requests"].pop(request, None)
            if item is not None:
                item["error"] = request.failure

        context = session["context"]
        context.on("request", request_started)
        context.on("response", response_received)
        context.on("requestfinished", request_finished)
        context.on("requestfailed", request_failed)
        context.on("console", lambda message: session["console"].append(
            {"type": message.type, "text": message.text, "location": message.location}))
        context.on("weberror", lambda error: session["console"].append(
            {"type": "error", "text": str(error.error)}))
        return session

    async def _operate(self, session, operation, arguments, url, seconds, *, capture=None):
        page, timeout = session["page"], int(seconds * 1000)
        extra = {}
        if operation in {"back", "forward"}:
            response = await (page.go_back if operation == "back" else page.go_forward)(
                wait_until="domcontentloaded", timeout=timeout)
            extra["status_code"] = response.status if response else 0
        elif operation == "snapshot":
            extra["snapshot"] = await page.locator("body").aria_snapshot(timeout=timeout)
        elif operation == "find":
            extra["elements"] = await page.locator(arguments["selector"]).evaluate_all(
                "els => els.map(e => ({tag: e.tagName, id: e.id, role: e.getAttribute('role'), "
                "text: e.innerText || e.textContent, value: e.value, name: e.getAttribute('name')}))")
        elif operation == "type":
            locator = page.locator(arguments["selector"]).first
            if arguments.get("slowly"):
                await locator.press_sequentially(arguments["text"], timeout=timeout)
            else:
                await locator.fill(arguments["text"], timeout=timeout)
            if arguments.get("submit"):
                await locator.press("Enter", timeout=timeout)
        elif operation == "fill_form":
            for field in arguments["fields"]:
                locator = page.locator(field["selector"]).first
                if field.get("type") == "checkbox":
                    await locator.set_checked(field["value"], timeout=timeout)
                elif field.get("type") == "select":
                    await locator.select_option(field["value"], timeout=timeout)
                else:
                    await locator.fill(str(field["value"]), timeout=timeout)
        elif operation == "select_option":
            extra["values"] = await page.locator(arguments["selector"]).first.select_option(arguments["values"], timeout=timeout)
        elif operation == "press_key":
            await page.keyboard.press(arguments["key"])
        elif operation == "wait_for":
            if "time" in arguments:
                await page.wait_for_timeout(arguments["time"] * 1000)
            for key, state in (("text", "visible"), ("textGone", "hidden")):
                if key in arguments:
                    await page.get_by_text(arguments[key], exact=True).first.wait_for(state=state, timeout=timeout)
        elif operation == "tabs":
            context, action = session["context"], arguments["action"]
            pages = context.pages
            if action == "new":
                session["page"] = await context.new_page()
            elif action in {"select", "close"}:
                index = arguments.get("index", pages.index(page))
                if index < 0 or index >= len(pages):
                    raise ValueError("browser_tab_not_found")
                if action == "select":
                    session["page"] = pages[index]
                else:
                    await pages[index].close()
                    if page.is_closed():
                        session["page"] = context.pages[-1] if context.pages else await context.new_page()
            if session["page"] is not page:
                session["page_id"] = uuid4().hex
            extra["tabs"] = [{"index": index, "url": tab.url, "title": await tab.title(),
                              "active": tab is session["page"]} for index, tab in enumerate(context.pages)]
        elif operation == "network_requests":
            extra["requests"] = list(session["network"])
        elif operation == "console_messages":
            extra["messages"] = list(session["console"])
        # Only navigation consumes a URL; read-only tools never navigate implicitly.
        result = await super()._operate(session, operation, arguments, url if operation == "navigate" else "", seconds, capture=capture)
        return {**result, **extra}


class BrowserMcpServer:
    def __init__(self, *, output_dir: Path, executable_path: str = ""):
        self.sessions = McpBrowserSessions()
        self.output_dir = output_dir
        self.executable_path = executable_path
        self.started = False

    @staticmethod
    def _error(request_id, code, message):
        return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": safe_error_text(message)}}

    def handle(self, payload: Mapping[str, Any]):
        method, request_id = payload.get("method"), payload.get("id")
        if not method:
            return self._error(request_id, -32600, "invalid_request")
        if request_id is None:
            return None
        if method == "initialize":
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {"listChanged": False}},
                      "serverInfo": {"name": "trace-browser-mcp", "version": "1"},
                      "instructions": "Trace browser tools using Python Playwright. Use selectors, not Microsoft Playwright MCP refs."}
        elif method == "ping":
            result = {}
        elif method == "tools/list":
            result = {"tools": TOOLS}
        elif method == "tools/call":
            params = payload.get("params") or {}
            if not isinstance(params, Mapping):
                return self._error(request_id, -32602, "invalid_params")
            name, arguments = str(params.get("name") or ""), params.get("arguments", {})
            definition = _TOOLS_BY_NAME.get(name)
            if definition is None:
                return self._error(request_id, -32602, f"tool_not_found:{name}")
            error = ToolBroker._schema_error(definition["inputSchema"], arguments)
            if error:
                return self._error(request_id, -32602, error)
            try:
                output = self.call(name, arguments)
                result = {"content": [{"type": "text", "text": json.dumps(output, ensure_ascii=False)}],
                          "structuredContent": output, "isError": False}
            except Exception as exc:
                result = {"content": [{"type": "text", "text": safe_error_text(exc)}], "isError": True}
        else:
            return self._error(request_id, -32601, f"method_not_found:{method}")
        return {"jsonrpc": "2.0", "id": request_id, "result": result}

    def call(self, name, arguments):
        if name == "browser_close":
            reports = self.sessions.close_run("mcp")
            self.started = False
            return {"closed": True, "cleanup": list(reports)}
        if not self.started:
            self.sessions.call("create", {"executable_path": self.executable_path},
                               run_id="mcp", workspace=self.output_dir)
            self.started = True
        operation = {"browser_navigate_back": "back", "browser_navigate_forward": "forward",
                     "browser_take_screenshot": "screenshot"}.get(name, name.removeprefix("browser_"))
        return self.sessions.call(operation, arguments, run_id="mcp", workspace=self.output_dir)

    def close(self):
        self.sessions.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description="Trace built-in browser MCP (Python Playwright)")
    parser.add_argument("--workspace", default="", help="Run workspace; omitted uses a temporary session directory")
    parser.add_argument("--output-dir", type=Path, help="Explicit screenshot output directory")
    parser.add_argument("--executable-path", default="", help="Existing Chrome/Chromium executable")
    args = parser.parse_args(argv)
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
    with ExitStack() as stack:
        output = args.output_dir or (Path(args.workspace) / "playwright-output" if args.workspace else
                                    Path(stack.enter_context(tempfile.TemporaryDirectory(prefix="trace-browser-mcp-"))))
        server = BrowserMcpServer(output_dir=output.resolve(), executable_path=args.executable_path)
        previous = signal.signal(signal.SIGTERM, signal.default_int_handler)
        try:
            _serve_stdio(server, sys.stdin, sys.stdout)
        except KeyboardInterrupt:
            pass
        finally:
            signal.signal(signal.SIGTERM, previous)
            server.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
