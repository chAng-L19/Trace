"""Exercise a deployed desktop UI with real APIs, using only disposable records."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import time
from uuid import uuid4

from playwright.sync_api import sync_playwright, expect
from redteam_agent.runtime.managed_tools import chromium_executable


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    marker = "live-ui-" + uuid4().hex[:12]
    skill_ids = ["skill:external/" + marker, "skill:external/" + marker + "-import"]
    run_id = ""
    errors: list[str] = []
    checks: list[str] = []
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(executable_path=chromium_executable(), headless=True,
                                             args=["--no-sandbox", "--disable-dev-shm-usage"])
        context = browser.new_context(viewport={"width": 1440, "height": 900})
        page = context.new_page()
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.on("dialog", lambda dialog: dialog.accept())

        def request(method: str, path: str, body=None):
            response = context.request.fetch(args.url + path, method=method, data=body,
                headers={"X-Command-ID": uuid4().hex})
            assert response.ok, (path, response.status, response.text())
            return response.json()

        def tab(name: str):
            page.locator(f'[data-control-tab="{name}"]').click()
            expect(page.locator(f'[data-control-panel="{name}"]')).to_be_visible()

        def skill_row(identifier: str):
            return page.locator("#skill-list .setting-row").filter(has_text=identifier)

        try:
            page.goto(args.url, wait_until="domcontentloaded")
            page.locator("#login-username").fill(os.environ.get("TRACE_ADMIN_USERNAME", "trace"))
            page.locator("#login-password").fill(os.environ.get("TRACE_ADMIN_PASSWORD", "admin@123"))
            page.locator("#login-submit").click()
            expect(page.locator("#login-dialog")).not_to_be_visible()
            checks.append("real_ui_login")

            tab("skills")
            expect(page.locator("#add-skill")).to_be_visible()
            expect(page.locator("#import-skill")).to_be_visible()
            page.screenshot(path=str(args.output / "skills.png"))
            page.locator("#add-skill").click()
            page.locator("#skill-id").fill(skill_ids[0])
            page.locator("#skill-content").fill("# Live UI fixture\nInspect only the disposable local fixture.")
            page.locator('#skill-form button[value="default"]').click()
            expect(skill_row(skill_ids[0])).to_be_visible()
            skill_row(skill_ids[0]).get_by_role("checkbox", name="启用").uncheck()
            expect(skill_row(skill_ids[0]).get_by_role("checkbox", name="启用")).to_be_enabled()
            expect(skill_row(skill_ids[0]).get_by_role("checkbox", name="启用")).not_to_be_checked()
            assert any(
                item["resource_id"] == skill_ids[0] and not item["enabled"]
                for item in request("GET", "/api/skills")["skills"])
            skill_row(skill_ids[0]).get_by_role("checkbox", name="启用").check()
            expect(skill_row(skill_ids[0]).get_by_role("checkbox", name="启用")).to_be_enabled()
            expect(skill_row(skill_ids[0]).get_by_role("checkbox", name="启用")).to_be_checked()
            skill_row(skill_ids[0]).get_by_role("button", name="配置").click()
            expect(page.locator("#skill-content")).to_be_enabled()
            page.locator("#skill-content").fill("# Updated live fixture\nNo external requests.")
            page.locator('#skill-form button[value="default"]').click()
            expect(page.locator("#skill-dialog")).not_to_be_visible()
            checks.append("skill_create_disable_enable_edit")

            page.locator("#import-skill").click()
            page.locator('#skill-import-form input[name="file"]').set_input_files({
                "name": "SKILL.md", "mimeType": "text/markdown",
                "buffer": b"---\nname: live-import\ndescription: Local fixture\n---\n# Imported fixture\nRead local evidence.\n"})
            page.locator('#skill-import-form input[name="resource_id"]').fill(skill_ids[1])
            page.locator('#skill-import-form button[value="default"]').click()
            expect(skill_row(skill_ids[1])).to_be_visible()
            for identifier in reversed(skill_ids):
                skill_row(identifier).get_by_role("button", name="删除").click()
                expect(skill_row(identifier)).to_have_count(0)
            checks.append("skill_file_import_and_delete")

            created = request("POST", "/api/runs", {"session_id": marker, "objective": marker,
                "targets": ["http://127.0.0.1"], "max_actions": 0})
            run_id = created["runs"][0]["run"]["run_id"]
            tab("conversations")
            row = page.locator("#conversation-list .setting-row").filter(has_text=marker)
            expect(row).to_be_visible()
            page.screenshot(path=str(args.output / "conversations.png"))
            started = time.perf_counter()
            row.get_by_role("button", name="打开", exact=True).click()
            expect(page.locator("#delete-conversation")).to_be_visible()
            latency_ms = round((time.perf_counter() - started) * 1000, 2)
            page.locator("#delete-conversation").click()
            expect(page.locator("#delete-conversation")).not_to_be_visible()
            tab("conversations")
            expect(page.locator("#conversation-list .setting-row").filter(has_text=marker)).to_have_count(0)
            run_id = ""
            checks.append("conversation_open_delete_unlimited_budget")

            tab("mcp")
            expect(page.locator("#add-mcp")).to_be_visible()
            expect(page.locator("#mcp-list")).to_contain_text("playwright")
            mcp = request("GET", "/api/mcp")
            assert any(item["server_id"] == "playwright" and item["status"]["tool_count"] == 17
                       and item["status"]["callable"] for item in mcp["servers"])
            page.screenshot(path=str(args.output / "mcp.png"))
            checks.append("integrated_mcp_17_tools")
            tab("providers")
            expect(page.locator("#provider-list")).to_contain_text("deepseek-flash")
            page.screenshot(path=str(args.output / "providers.png"))
            tab("system")
            page.screenshot(path=str(args.output / "system.png"))
            assert not errors, errors
            print(json.dumps({"ok": True, "checks": checks, "open_latency_ms": latency_ms,
                              "page_errors": errors, "screenshots": str(args.output)}), flush=True)
        finally:
            for identifier in skill_ids:
                context.request.delete(args.url + "/api/skills/" + identifier)
            if run_id:
                context.request.delete(args.url + "/api/conversations/" + run_id)
            browser.close()


if __name__ == "__main__":
    main()
