// Desktop workspace regression against the real frontend; all API data is a fixture.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { chromiumLaunchOptions } from "./playwright_smoke_support.mjs";

const { chromium } = createRequire(import.meta.url)("playwright");
const staticRoot = new URL("../src/redteam_agent/static/", import.meta.url);
const server = createServer(async (request, response) => {
  try {
    const path = new URL(request.url, "http://local").pathname;
    const file = new URL(path === "/" ? "index.html" : path.slice(1), staticRoot);
    const contentType = file.pathname.endsWith(".js") ? "text/javascript" : file.pathname.endsWith(".css") ? "text/css" : "text/html";
    response.writeHead(200, { "Content-Type": contentType });
    response.end(await readFile(file));
  } catch { response.writeHead(404); response.end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const browser = await chromium.launch(chromiumLaunchOptions());
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
const errors = [], writes = [];
page.on("pageerror", (error) => errors.push(error.message));
const makeWorkspace = (id, status = "cancelled") => ({
  workspace: { workspace_id: id, title: id, status: "active", goal: `Goal ${id}`, origin: "local",
    run_count: 1, hint_count: 0, reason_worker: "", updated_at: "2026-10-11T08:00:00Z" },
  runs: [{ run_id: `run-${id}`, objective: `Task ${id}`, status, actions_used: 1, action_limit: 0 }],
  intents: [{ id: `intent-${id}`, run_id: `run-${id}`, status: "open", worker: null }],
  facts: [{ id: "goal", description: `Goal ${id}`, source: "workspace" }], hints: [],
});
const records = new Map([["ws-a", makeWorkspace("ws-a")], ["ws-b", makeWorkspace("ws-b", "running")]]);
for (let index = 0; index < 203; index += 1) records.set(`ws-extra-${index}`, makeWorkspace(`ws-extra-${index}`));
let held = null;
function hold(match) {
  let release, seen;
  const ready = new Promise((resolve) => { seen = resolve; });
  const gate = new Promise((resolve) => { release = resolve; });
  held = { match, gate, seen };
  return { ready, release };
}
await page.route("**/api/**", async (route) => {
  const request = route.request(), url = new URL(request.url()), path = url.pathname;
  const method = request.method();
  const body = request.postDataJSON() || {};
  if (method !== "GET") writes.push({ path, method, body });
  let payload = {};
  if (path === "/api/auth/status") payload = { required: true, authenticated: true, user: { role: "admin", username: "fixture" } };
  else if (path === "/api/system") payload = { platform: "fixture", provider: { configured: false } };
  else if (path === "/api/runs") payload = { runs: [] };
  else if (path === "/api/workspaces" && method === "GET") {
    const values = [...records.values()].map((value) => value.workspace).filter((value) =>
      value.title.includes(url.searchParams.get("search") || "") && (!url.searchParams.get("status") || value.status === url.searchParams.get("status")));
    const offset = Number(url.searchParams.get("offset") || 0), limit = Number(url.searchParams.get("limit") || 200);
    payload = { workspaces: values.slice(offset, offset + limit),
      next_offset: offset + limit < values.length ? offset + limit : null };
  } else if (path === "/api/workspaces" && method === "POST") {
    const data = makeWorkspace("ws-created"); data.workspace.title = body.title;
    records.set("ws-created", data); payload = { workspace: data };
  } else if (path.startsWith("/api/workspaces/")) {
    const [, , , id, resource, target, action] = path.split("/");
    const data = records.get(id);
    if (!data) return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ ok: false, error: "workspace_not_found" }) });
    if (method === "DELETE") { records.delete(id); payload = { deleted: id }; }
    else if (method === "POST") {
      if (!resource) {
        if (body.action === "rename") data.workspace.title = body.title;
        else data.workspace.status = body.action;
        if (["stopped", "completed"].includes(body.action)) data.workspace.reason_worker = "";
        payload = { workspace: data };
      } else if (resource === "reason") {
        data.workspace.reason_worker = target === "release" ? "" : "operator"; payload = data;
      } else if (resource === "hints") {
        data.hints.push({ content: body.content, creator: "operator" }); data.workspace.hint_count += 1; payload = { workspace: data };
      } else if (resource === "intents" && action) {
        data.intents[0].worker = action === "release" ? null : "operator";
        if (action === "conclude") {
          data.intents[0].status = "concluded";
          data.facts.push({ id: "fact-user", description: body.description, source: "operator_conclusion", creator: "operator", verified: false });
        }
        payload = data;
      } else if (resource === "intents") {
        const runId = `added-${id}`;
        data.runs.push({ run_id: runId, objective: body.description, status: "running", actions_used: 0, action_limit: 0 });
        data.intents.push({ id: `intent-${runId}`, run_id: runId, status: "open", worker: null });
        payload = data;
      } else payload = data;
    } else payload = data;
  }
  const serialized = JSON.stringify({ ok: true, ...payload });
  if (held?.match(url, method)) { const current = held; held = null; current.seen(); await current.gate; }
  await route.fulfill({ contentType: "application/json", body: serialized });
});
const detailIs = (name) => page.waitForFunction((value) => document.querySelector("#workspace-detail-name").textContent === value && !document.querySelector("#workspace-detail").hidden, name);
const button = (text) => page.locator("#workspace-detail").getByRole("button", { name: text, exact: true });
async function promptClick(locator, text) {
  page.once("dialog", (dialog) => dialog.accept(text));
  await locator.click();
}

try {
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.click('[data-view="workspaces"]');
  await page.waitForSelector('[data-workspace-id="ws-a"]');
  assert.equal(await page.locator(".workspace-item").count(), 200);
  await page.click("#workspace-more");
  await page.waitForFunction(() => document.querySelectorAll(".workspace-item").length === 205);
  await page.click('[data-workspace-id="ws-extra-202"]'); await detailIs("ws-extra-202");
  await page.reload(); await detailIs("ws-extra-202");
  assert.equal(await page.locator(".workspace-item").count(), 200);
  // Cancel and close must never turn into POST, even with valid form data.
  for (const [open, form, field] of [["#new-workspace", "#workspace-form", "title"],
    ["#workspace-add-hint", "#workspace-hint-form", "content"], ["#workspace-add-intent", "#workspace-intent-form", "description"]]) {
    if (open !== "#new-workspace") { await page.click('[data-workspace-id="ws-a"]'); await detailIs("ws-a"); }
    await page.click(open);
    await page.fill(`${form} [name="${field}"]`, "Must not submit");
    const count = writes.length;
    await page.locator(`${form} button[value="cancel"]`).last().click();
    assert.equal(await page.locator(`${form}`).evaluate((form) => form.closest("dialog").open), false);
    assert.equal(writes.length, count);
    await page.click(open);
    await page.locator(`${form} button[value="cancel"]`).first().click();
    assert.equal(writes.length, count);
  }
  await page.click('[data-workspace-id="ws-a"]'); await detailIs("ws-a");
  assert.match(await page.textContent("#workspace-runs"), /已取消/);
  assert.equal(await page.locator("#workspace-detail i[data-lucide]").count(), 0);
  assert.ok(await page.locator("#workspace-actions svg").count());
  await button("认领意图").click();
  await button("续租").waitFor(); await button("续租").click();
  await button("释放").click(); await button("认领意图").waitFor();
  await button("认领意图").click(); await button("记录结论").waitFor();
  await promptClick(button("记录结论"), "User assertion");
  await page.waitForFunction(() => document.querySelector("#workspace-facts").textContent.includes("User assertion"));
  assert.match(await page.textContent("#workspace-facts"), /操作员结论（未经证据验证）/);
  assert.doesNotMatch(await page.textContent("#workspace-hints"), /User assertion/);
  assert.equal(await button("释放").count(), 0);
  await promptClick(button("认领推演"), "Inspect");
  await button("续租推演").waitFor(); await button("续租推演").click();
  await button("释放推演").click(); await button("认领推演").waitFor();
  await button("暂停工作区").click(); await button("恢复工作区").waitFor();
  await button("恢复工作区").click(); await button("标记完成").waitFor();
  await button("标记完成").click(); await button("重新打开").waitFor();
  await button("重新打开").click(); await button("暂停工作区").waitFor();
  // A late response for A must not replace a newer selection B.
  let pending = hold((url, method) => url.pathname.endsWith("/ws-a") && method === "GET");
  await page.click('[data-workspace-id="ws-a"]'); await pending.ready;
  await page.click('[data-workspace-id="ws-b"]'); await detailIs("ws-b");
  const staleDone = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/ws-a"));
  pending.release(); await staleDone;
  await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.textContent("#workspace-detail-name"), "ws-b");
  assert.equal(await button("标记完成").isDisabled(), true);
  await page.reload(); await detailIs("ws-b");
  assert.equal(await page.locator("#workspace-index").isVisible(), true);
  // A late unfiltered list must not overwrite the new filter or restore detail.
  pending = hold((url) => url.pathname === "/api/workspaces" && !url.searchParams.get("search"));
  await page.click("#refresh-workspaces"); await pending.ready;
  await page.fill("#workspace-search", "no-match");
  await page.waitForFunction(() => document.querySelector("#workspace-count").textContent === "0");
  const listDone = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/workspaces" && !new URL(response.url()).searchParams.get("search"));
  pending.release(); await listDone; await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.locator(".workspace-item").count(), 0);
  assert.equal(await page.locator("#workspace-detail").isVisible(), false);
  await page.fill("#workspace-search", ""); await page.waitForSelector('[data-workspace-id="ws-a"]');
  await page.click('[data-workspace-id="ws-a"]'); await detailIs("ws-a");
  await page.click("#workspace-add-hint"); await page.fill('#workspace-hint-form [name="content"]', "A hint");
  await page.click('#workspace-hint-form [value="default"]');
  await page.waitForFunction(() => document.querySelector("#workspace-hints").textContent.includes("A hint"));
  // Switching workspace while rename is pending must preserve the new selection.
  pending = hold((url, method) => url.pathname.endsWith("/ws-a") && method === "POST");
  await promptClick(page.locator("#workspace-rename"), "Renamed A"); await pending.ready;
  await page.click('[data-workspace-id="ws-b"]'); await detailIs("ws-b");
  const renameDone = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/ws-a"));
  pending.release(); await renameDone; await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.textContent("#workspace-detail-name"), "ws-b");
  page.once("dialog", (dialog) => dialog.dismiss());
  await page.click("#workspace-delete");
  assert.equal(writes.filter((item) => item.method === "DELETE").length, 0);
  await promptClick(page.locator("#workspace-delete"));
  await page.waitForFunction(() => !document.querySelector('[data-workspace-id="ws-b"]'));
  assert.equal(await page.locator("#workspace-detail").isVisible(), false);
  await page.click("#new-workspace");
  await page.fill('#workspace-form [name="title"]', "Created title");
  await page.fill('#workspace-form [name="goal"]', "Created goal");
  await page.click('#workspace-form [value="default"]'); await detailIs("Created title");
  assert.equal(new URL(page.url()).searchParams.get("workspace"), "ws-created");
  await page.click("#workspace-add-intent");
  await page.fill('#workspace-intent-form [name="description"]', "Additional task");
  await page.click('#workspace-intent-form [value="default"]');
  await page.waitForFunction(() => document.querySelector("#workspace-runs").textContent.includes("Additional task"));
  for (const [selector, filename] of [["#workspace-export-md", "trace-workspace.md"], ["#workspace-export-json", "trace-workspace.json"]]) {
    const download = page.waitForEvent("download");
    await page.click(selector);
    assert.equal((await download).suggestedFilename(), filename);
  }
  // Closing an in-flight create must not reopen/select its late response.
  pending = hold((url, method) => url.pathname === "/api/workspaces" && method === "POST");
  await page.click("#new-workspace");
  await page.fill('#workspace-form [name="title"]', "Cancelled pending selection");
  await page.fill('#workspace-form [name="goal"]', "Pending goal");
  await page.click('#workspace-form [value="default"]'); await pending.ready;
  await page.locator('#workspace-form [value="cancel"]').last().click();
  const createDone = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/api/workspaces"));
  pending.release(); await createDone; await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.textContent("#workspace-detail-name"), "Created title");
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ ok: true, checks: ["cancel-close-no-write", "readable-status", "icons", "intent-lifecycle",
    "unverified-conclusion-label", "reason-lifecycle", "stop-resume-complete", "stale-detail", "refresh-restore",
    "stale-list", "filter-clears-detail", "hint", "stale-mutation", "delete-cancel-confirm", "create", "add-intent",
    "exports", "cancel-pending-create", "desktop-layout", "workspace-pagination", "deep-link-beyond-first-page"] }));
} finally {
  held = null;
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
