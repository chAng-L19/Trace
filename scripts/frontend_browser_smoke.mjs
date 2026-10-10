// Run against trace-web: node scripts/frontend_browser_smoke.mjs [URL].
// Requires Playwright on Node's module path; API fixtures never modify server state.
import assert from "node:assert/strict";
import { mkdirSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const { chromium } = createRequire(import.meta.url)("playwright");
const base = process.argv[2] || "http://127.0.0.1:8766";
const output = fileURLToPath(new URL("../playwright-output/", import.meta.url));
mkdirSync(output, { recursive: true });
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const errors = [];
const posts = [];
page.on("pageerror", (error) => errors.push(error.message));
page.on("console", (message) => { if (message.type() === "error" && /Content Security Policy|font/i.test(message.text())) errors.push(message.text()); });
let authenticated = false;
let role = "admin";
let providers = [{ provider_id: "primary", name: "TRACE_PRIMARY", model: "gpt-4o", provider: "openai-compatible", base_url: "https://api.openai.com/v1", active: true, api_key_set: true }];
const user = () => ({ user_id: "fixture", username: "researcher", display_name: "Researcher", role });
const view = (runId, status) => ({
  run: { run_id: runId, session_id: "trace-fixture", status, state_version: 1,
    updated_at: "2026-10-10T08:30:00Z", budget: { action_limit: 64, actions_used: 12, input_tokens_used: 900, output_tokens_used: 300, token_limit: 32000 } },
  goal: { objective: "审计本地 Web 应用的身份验证与证据链", targets: ["http://127.0.0.1:8000"] }, next_action: "await_model",
});
const runs = [view("created", "created"), view("paused", "paused_budget"), view("complete", "completed"), view("running", "running")];
const conversations = [{ run: runs[0], message_count: 2, session: { active_branch_id: "main", branches: { main: "entry-1", alternate: "entry-2" } } }];
await page.route("**/api/**", async (route) => {
  const request = route.request();
  const url = new URL(request.url());
  const path = url.pathname;
  const body = request.postDataJSON();
  if (request.method() !== "GET") posts.push({ path, body, headers: request.headers() });
  const reply = (payload, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify({ ok: status < 400, ...payload }) });
  if (path === "/api/auth/status") return reply({ required: true, authenticated, user: authenticated ? user() : null });
  if (path === "/api/auth/login") {
    if (body.password === "wrong") return reply({ error: "invalid_credentials" }, 401);
    authenticated = true;
    return reply({ user: user(), authenticated: true });
  }
  if (!authenticated) return reply({ error: "authentication_required" }, 401);
  if (path === "/api/auth/logout") { authenticated = false; return reply({ authenticated: false }); }
  if (path === "/api/auth/profile") return reply({ user: user() });
  if (path === "/api/auth/users") return reply({ users: [user()] });
  if (path === "/api/system") return reply({ platform: "Windows", schema_version: 1, provider: { configured: true, name: "TRACE_PRIMARY", model: "gpt-4o" }, control_plane: { status: "ready" } });
  if (path === "/api/providers") {
    if (request.method() === "POST") providers = [{ ...body, active: true, api_key_set: false }];
    return reply({ providers });
  }
  if (path === "/api/skills") return reply({ skills: [{ resource_id: "web/recon", byte_count: 1200, content_hash: "a".repeat(64), enabled: true, config: {} }] });
  if (path === "/api/mcp") return reply({ servers: [{ server_id: "playwright", source: "catalog", transport: "stdio", preset: "playwright", enabled: false, status: { status: "available", tool_count: 0 } }] });
  if (path === "/api/conversations") return reply({ conversations });
  if (path === "/api/conversations/created") return reply({ tree: { nodes: { "entry-1": { entry: { sequence: 1, entry_type: "user", branch_id: "main" } } } } });
  if (path.endsWith("/fork")) return reply({ active_branch_id: body.branch_id });
  if (path.endsWith("/checkout")) return reply({ error: "fixture_checkout_failed" }, 409);
  if (path === "/api/runs") {
    if (request.method() === "POST") return reply({ runs: [runs[0]] });
    return reply({ runs });
  }
  const match = path.match(/^\/api\/runs\/([^/]+)(?:\/(.*))?$/);
  if (!match) return reply({});
  const run = runs.find((item) => item.run.run_id === match[1]) || runs[0];
  const endpoint = match[2];
  if (!endpoint || request.method() === "POST") return reply({ run });
  if (endpoint === "events") {
    if (url.searchParams.has("channel")) return route.fulfill({ contentType: "text/event-stream", body: ": fixture stream\n\n" });
    return reply({ events: [{ run_id: run.run.run_id, sequence: 1, event_type: "run.created", created_at: "2026-10-10T08:30:00Z", payload: { summary: "Fixture event" } }], next_sequence: 1 });
  }
  if (endpoint === "search-graph") return reply({ records: [{ record_id: "hypothesis-1", record_kind: "hypothesis", status: "active", statement: "验证会话边界", target: "localhost", confidence: .5 }], search_graph: { record_count: 1 } });
  if (endpoint === "evidence-graph") return reply({ nodes: [{ evidence_id: "evidence-1", artifact_type: "verified-response" }], truncated: false });
  if (endpoint === "artifacts") return reply({ artifacts: [{ artifact_id: "artifact-1", artifact_type: "http-response" }], truncated: false });
  if (endpoint === "transcript") return reply({ messages: [{ entry_id: "message-1", role: "assistant", content: "证据记录已生成。", sequence: 1 }], truncated: false });
  if (endpoint === "report") return reply({ run: { run }, findings: [] });
  return reply({ fixture: endpoint });
});

const visible = async (selector) => page.locator(selector).first().waitFor({ state: "visible" });
const hidden = async (selector) => page.locator(selector).waitFor({ state: "hidden" });
async function login(password = "fixture") {
  await page.fill("#login-username", "researcher");
  await page.fill("#login-password", password);
  await page.click("#login-submit");
  if (password === "wrong") await visible("#login-error");
  else await hidden("#login-dialog");
}
async function navigate(tab) {
  await page.click(`[data-control-tab="${tab}"]`);
  await visible(`[data-control-panel="${tab}"]`);
}
async function screenshot(name) {
  await page.mouse.move(1400, 800);
  await page.screenshot({ path: `${output}/${name}.png`, animations: "disabled" });
}
async function noOverflow() {
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "document overflow");
  const overlaps = await page.evaluate(() => {
    const header = document.querySelector(".app-header").getBoundingClientRect();
    const main = document.querySelector("main:not([hidden])").getBoundingClientRect();
    const terminal = document.querySelector(".trace-terminal").getBoundingClientRect();
    return main.top < header.bottom || main.bottom > terminal.top + 1;
  });
  assert.equal(overlaps, false, "main overlaps header or console");
}

try {
  await page.goto(base);
  await visible("#login-dialog");
  await page.evaluate(() => document.fonts.ready);
  await screenshot("login-light-desktop");
  await login("wrong");
  assert.match(await page.textContent("#login-error"), /用户名或密码/);
  assert.equal(await page.locator("#login-submit").isEnabled(), true);
  assert.equal(await page.locator("#login-label").count(), 1);
  await page.keyboard.press("Escape");
  assert.equal(await page.locator("#login-dialog").isVisible(), true);
  await login();
  await navigate("providers");
  await visible("#provider-list tr");
  assert.equal(await page.locator("#provider-list tr td").count(), 6);
  assert.equal(await page.locator("#provider-empty").isVisible(), false);
  assert.equal(await page.locator("#workspace-title").textContent(), "模型与 API");
  await noOverflow();
  const dimensions = await page.evaluate(() => ({ rail: document.querySelector(".lab-rail").getBoundingClientRect().width, header: document.querySelector(".app-header").getBoundingClientRect().height }));
  assert.deepEqual(dimensions, { rail: 236, header: 52 });
  await screenshot("providers-light-desktop");
  await page.click("#theme-toggle");
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  await screenshot("providers-dark-desktop");
  await page.click('#provider-list button[aria-label="编辑"]');
  await visible("#provider-dialog");
  assert.equal(await page.inputValue('#provider-form [name="provider_id"]'), "primary");
  await page.click('#provider-form button[value="cancel"]');
  await hidden("#provider-dialog");
  await page.click("#add-provider");
  await page.fill('#provider-form [name="provider_id"]', "test-provider");
  await page.fill('#provider-form [name="name"]', "Test Provider");
  await page.fill('#provider-form [name="model"]', "test-model");
  await page.click('#provider-form button[value="default"]');
  await hidden("#provider-dialog");
  await page.waitForFunction(() => document.querySelector("#provider-list").textContent.includes("Test Provider"));
  assert.ok(posts.find((post) => post.path === "/api/providers").headers["x-command-id"]);
  await page.click("#terminal-toggle");
  await visible("#terminal-body");
  await noOverflow();
  await page.click("#terminal-toggle");
  for (const tab of ["skills", "mcp", "conversations", "system", "profile", "users"]) { await navigate(tab); await noOverflow(); }
  await navigate("conversations");
  await page.click('#conversation-list button[aria-label="分支"]');
  await visible("#fork-dialog");
  await page.click('#fork-form button[value="default"]');
  await hidden("#fork-dialog");
  assert.ok(posts.find((post) => post.path.endsWith("/fork")).body.from_entry_id);
  await page.selectOption('#conversation-list select', "alternate");
  await page.waitForFunction(() => document.querySelector("#conversation-list select").value === "main");
  await page.click('.top-nav .nav-button');
  await visible("#run-list .run-item");
  await page.fill("#run-search", "missing-run");
  await page.waitForFunction(() => document.querySelectorAll(".run-item").length === 0);
  await page.fill("#run-search", "");
  await page.click('[data-run-id="created"]');
  await visible("#workbench");
  await page.waitForFunction(() => document.querySelector("#terminal-events").textContent.includes("run.created"));
  await screenshot("run-desktop");
  for (const tab of ["search", "evidence", "artifacts", "transcript", "tools", "attack", "transparency", "report", "events"]) {
    await page.click(`[data-tab="${tab}"]`);
    await visible(`[data-panel="${tab}"]`);
    await noOverflow();
  }
  await page.click('[data-tab="search"]');
  await page.click('.search-node');
  assert.match(await page.textContent("#search-inspector"), /验证会话边界/);
  for (const [runId, command, enabled] of [["paused", "resume", true], ["complete", "cancel", false]]) {
    await page.click('.top-nav .nav-button');
    await page.click(`[data-run-id="${runId}"]`);
    await page.waitForFunction((id) => document.querySelector("#run-id").textContent === id, runId);
    assert.equal(await page.locator(`[data-command="${command}"]`).isEnabled(), enabled);
  }
  await page.click("#new-run");
  await visible("#create-dialog");
  await page.fill('#create-form [name="objective"]', "Browser fixture goal");
  await page.fill('#create-form [name="targets"]', "localhost\n127.0.0.1");
  await page.click("#create-submit");
  await hidden("#create-dialog");
  assert.deepEqual(posts.find((post) => post.path === "/api/runs").body.targets, ["localhost", "127.0.0.1"]);
  await page.click("#logout");
  await visible("#login-dialog");
  await screenshot("login-dark-desktop");
  role = "member";
  await login();
  await navigate("providers");
  await page.waitForFunction(() => document.querySelectorAll("#provider-list td").length === 5);
  assert.equal(await page.locator("#provider-list td").count(), 5);
  assert.equal(await page.locator("#add-provider").isVisible(), false);
  assert.equal(await page.locator('[data-control-tab="users"]').isVisible(), false);
  authenticated = false;
  await page.click("#new-run");
  await page.fill('#create-form [name="objective"]', "Expired fixture");
  await page.click("#create-submit");
  await visible("#login-dialog");
  assert.equal(await page.textContent("#gate-state"), "GATE_STATE: SESSION_EXPIRED");
  assert.equal(await page.locator("#create-dialog").isVisible(), false);
  assert.deepEqual(errors, []);
  console.log("PASS: login failures/success/expiry, admin/member, Provider editing, settings, fork, checkout rollback, nine run tabs, events, create payload, theme, 1440 desktop layout and CSP");
} finally {
  await browser.close();
}
