// Real frontend, paginated API fixtures; no server business data is modified.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
const { chromium } = createRequire(import.meta.url)("playwright");
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
page.on("pageerror", (error) => errors.push(error.message));
const views = Array.from({ length: 245 }, (_, index) => ({
  run: { run_id: `archive-${index}`, session_id: `session-${index}`, status: "paused_budget",
    state_version: 1, budget: { action_limit: 64, actions_used: 64, pause_reason: "action_limit_exhausted",
      time_limit_seconds: 600, deadline: "2026-10-10T09:30:00Z" } },
  goal: { objective: `Archive task ${index}`, targets: [`fixture-${index}.invalid`] }, next_action: "await_model",
}));
const messages = Array.from({ length: 450 }, (_, index) => ({ message_id: `message-${index + 1}`,
  sequence: index + 1, role: "assistant", content: { text: `History message ${index + 1}` } }));
const historyRequests = [];
await page.route("**/api/**", async (route) => {
  const request = route.request(), url = new URL(request.url()), path = url.pathname;
  const reply = (payload) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ ok: true, ...payload }) });
  if (path === "/api/auth/status") return reply({ required: true, authenticated: true, user: { role: "admin", username: "fixture" } });
  if (path === "/api/system") return reply({ platform: "Fixture", provider: { configured: false } });
  if (path === "/api/runs" || path === "/api/conversations") {
    const search = url.searchParams.get("search") || "";
    const limit = Number(url.searchParams.get("limit") || 100), offset = Number(url.searchParams.get("offset") || 0);
    const filtered = views.filter((view) => JSON.stringify(view).includes(search));
    const data = filtered.slice(offset, offset + limit);
    return reply(path === "/api/runs" ? { runs: data, truncated: filtered.length > offset + limit }
      : { conversations: data.map((view) => ({ run: view, message_count: 450, session: { branches: { main: "entry" } } })), truncated: filtered.length > offset + limit });
  }
  if (path.startsWith("/api/conversations/") && request.method() === "DELETE") {
    views.splice(views.findIndex((view) => view.run.run_id === path.split("/").at(-1)), 1);
    return reply({ deleted: true });
  }
  const match = path.match(/^\/api\/runs\/([^/]+)(?:\/(.*))?$/);
  if (!match) return reply({});
  const view = views.find((item) => item.run.run_id === match[1]);
  if (!match[2]) return reply({ run: view });
  if (match[2] === "events") {
    if (url.searchParams.has("channel")) return route.fulfill({ contentType: "text/event-stream", body: ": keep-alive\n\n" });
    return reply({ events: [], next_sequence: 0 });
  }
  if (match[2] === "transcript") {
    const offset = Number(url.searchParams.get("offset") || 0), limit = Number(url.searchParams.get("limit") || 200);
    historyRequests.push({ offset, limit, latest: url.searchParams.get("latest") });
    const newest = [...messages].reverse().slice(offset, offset + limit).reverse();
    return reply({ messages: newest, truncated: messages.length > offset + limit });
  }
  return reply({});
});

try {
  await page.goto(process.argv[2] || "http://127.0.0.1:8780");
  await page.waitForFunction(() => document.querySelectorAll(".run-item").length === 200);
  await page.click("#more-runs");
  await page.waitForFunction(() => document.querySelectorAll(".run-item").length === 245);
  await page.fill("#run-search", "fixture-244.invalid");
  await page.waitForFunction(() => document.querySelectorAll(".run-item").length === 1);
  await page.click('[data-run-id="archive-244"]');
  await page.waitForFunction(() => document.querySelectorAll(".transcript-message").length === 200);
  assert.match(await page.locator(".transcript-message").first().textContent(), /History message 251/);
  assert.match(await page.locator(".transcript-message").last().textContent(), /History message 450/);
  assert.match(await page.textContent("#run-pause-reason"), /动作预算已耗尽/);
  assert.equal(await page.textContent("#metric-time"), "600 秒");
  await page.click('[data-more="transcript"]');
  await page.waitForFunction(() => document.querySelectorAll(".transcript-message").length === 400);
  await page.evaluate(() => { document.querySelector(".tab-content").scrollTop = 50; });
  for (let index = 451; index <= 460; index += 1) messages.push({ message_id: `message-${index}`, sequence: index,
    role: "assistant", content: { text: `History message ${index}` } });
  await page.evaluate(() => loadRun(state.selectedId, false));
  assert.equal(await page.locator(".transcript-message").count(), 410, "live refresh keeps old pages and new messages");
  assert.match(await page.locator(".transcript-message").first().textContent(), /History message 51/);
  assert.match(await page.locator(".transcript-message").last().textContent(), /History message 460/);
  assert.equal(await page.evaluate(() => document.querySelector(".tab-content").scrollTop), 50);
  await page.click('[data-more="transcript"]');
  await page.waitForFunction(() => document.querySelectorAll(".transcript-message").length === 460);
  assert.equal(await page.locator('[data-more="transcript"]').isVisible(), false);
  assert.ok(historyRequests.every((request) => request.latest === "1"));
  await page.screenshot({ path: "playwright-output/history-fixture-desktop.png" });
  await page.click('[data-control-tab="conversations"]');
  await page.waitForFunction(() => document.querySelectorAll("#conversation-list .setting-row").length === 100);
  await page.click("#more-conversations");
  await page.waitForFunction(() => document.querySelectorAll("#conversation-list .setting-row").length === 200);
  await page.fill("#conversation-search", "fixture-244.invalid");
  await page.waitForFunction(() => document.querySelectorAll("#conversation-list .setting-row").length === 1);
  page.once("dialog", (dialog) => dialog.accept());
  await page.click('#conversation-list button[aria-label="删除"]');
  await page.waitForFunction(() => document.querySelectorAll("#conversation-list .setting-row").length === 0);
  assert.deepEqual(errors, []);
  console.log("PASS: full history search/pagination, latest conversation, older pages, live merge, scroll retention, pause/time state, old conversation deletion");
} finally { await browser.close(); }
