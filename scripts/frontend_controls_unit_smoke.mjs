// Offline checks of owned controls; no browser, network, backend or existing-test changes.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const source = (name) => readFileSync(new URL(`../src/redteam_agent/static/${name}`, import.meta.url), "utf8");
const html = source("index.html");
const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
assert.equal(new Set(ids).size, ids.length);
for (const id of ["message-resume", "provider-test", "provider-test-result", "fork-more", "fork-submit", "evidence-dialog", "evidence-title", "evidence-detail", "evidence-close", "attack-more", "import-skill", "skill-import-dialog", "skill-import-form"])
  assert.ok(ids.includes(id), id);
assert.match(html, /id="provider-test"[^>]*type="button"/);
assert.match(html, /id="attack-more"[^>]*data-more="attack"/);
assert.match(html, /id="import-skill"[^>]*data-admin-only/);

function element() {
  const classes = new Set();
  return {
    value: "", textContent: "", dataset: {}, children: [], listeners: {}, disabled: false, hidden: false,
    classList: { add: (x) => classes.add(x), remove: (x) => classes.delete(x),
      toggle: (x, on) => on ? classes.add(x) : classes.delete(x), contains: (x) => classes.has(x) },
    append(...items) { this.children.push(...items); },
    replaceChildren(...items) { this.children = items; },
    get options() { return this.children; },
    setAttribute() {}, addEventListener(name, fn) { this.listeners[name] = fn; },
    showModal() { this.open = true; }, close() { this.open = false; },
    reportValidity: () => true, reset() {}, click() { this.clicked = true; },
  };
}
function fixture(status = "paused_budget") {
  const nodes = new Map();
  const $ = (id) => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
  const state = { selectedId: "A", view: { run: { run_id: "A", status } }, runs: [],
    drafts: new Map(), pendingCommands: new Set(), authEpoch: 0, user: { role: "admin" } };
  const requests = [], commands = [], notices = [], links = [];
  const context = vm.createContext({ state, $, $$: () => [], console,
    terminalStatuses: new Set(["completed", "failed", "cancelled"]),
    coreRun: (view) => view?.run || {}, commandHeaders: () => ({ "X-Command-ID": "test" }),
    formatTime: (value) => value, confirm: () => true, setView() {}, showRunRegistry() {},
    showNotice: (message, error) => notices.push({ message, error }),
    applyView: (view) => { state.view = view; },
    loadRun: (id) => { state.selectedId = id; state.view = { run: { run_id: id, status } }; },
    loadTab: async () => {}, loadSystem: async () => {},
    api: async (path, options) => { requests.push({ path, options }); return { run: state.view }; },
    postCommand: async (...args) => commands.push(args),
    document: { createElement: (tag) => { const node = element(); if (tag === "a") links.push(node); return node; } },
    URL: { createObjectURL: () => "blob:test", revokeObjectURL() {} },
    FormData: class {
      constructor(form) { this.data = new Map(Object.entries(form.fields)); }
      entries() { return this.data.entries(); } get(key) { return this.data.get(key); }
    },
  });
  vm.runInContext(source("run-controls.js"), context);
  vm.runInContext(source("control.js"), context);
  $("#message-text").value = "  draft A  ";
  context.bindRunControls();
  const send = (resume = false) => context.submitMessage({ preventDefault() {}, submitter: { id: resume ? "message-resume" : "message-submit" } });
  return { context, state, $, requests, commands, notices, links, send };
}
const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; };

for (const status of ["paused_budget", "running", "waiting_worker", "created"]) {
  const f = fixture(status);
  await f.send();
  assert.equal(f.requests[0].path, "/api/runs/A/message");
  assert.equal(f.commands.length, status === "created" ? 1 : 0, status);
  if (status === "created") assert.equal(JSON.stringify(f.commands[0]), JSON.stringify(["run", { execute: true }, "A"]));
  assert.equal(f.$("#message-text").value, "");
  assert.equal(f.state.drafts.has("A"), false);
}
for (const status of ["completed", "failed", "cancelled"]) {
  const f = fixture(status);
  f.context.renderComposer();
  assert.equal(f.$("#message-text").disabled, false);
  assert.equal(f.$("#message-resume").hidden, true);
  await f.send();
  assert.equal(f.requests[0].path, "/api/runs/A/discussion");
  assert.equal(f.commands.length, 0);
}
{
  const f = fixture("completed");
  f.context.api = async () => ({ run: f.state.view, response: { status: "failed", error: "model_failed" } });
  await f.send();
  assert.equal(f.state.drafts.get("A"), "  draft A  ");
  assert.equal(f.$("#message-text").value, "  draft A  ");
  assert.equal(f.notices.at(-1).error, true);
  assert.match(f.notices.at(-1).message, /model_failed.*草稿已保留/);
}
{
  const f = fixture();
  const response = deferred();
  f.context.api = () => response.promise;
  const sending = f.send(true);
  f.context.openConversation("B");
  f.$("#message-text").value = "draft B";
  f.$("#message-text").listeners.input({ target: f.$("#message-text") });
  response.resolve({ run: { run: { run_id: "A", status: "paused_budget" } } });
  await sending;
  assert.equal(f.commands[0][0], "resume");
  assert.equal(f.commands[0][2], "A");
  assert.equal(f.$("#message-text").value, "draft B");
  assert.equal(f.state.drafts.has("A"), false);
  assert.equal(f.state.drafts.get("B"), "draft B");
}
{
  const f = fixture();
  const transcript = deferred();
  f.context.loadTab = () => transcript.promise;
  const sending = f.send(true);
  await Promise.resolve(); await Promise.resolve();
  f.context.openConversation("B");
  transcript.resolve();
  await sending;
  assert.equal(f.commands.length, 1);
  assert.equal(f.commands[0][2], "A");
  assert.equal(f.notices.length, 0);
}
{
  const f = fixture("created"), response = deferred();
  f.context.api = () => response.promise;
  const sending = f.send();
  f.state.authEpoch++;
  f.state.drafts.clear();
  f.state.pendingCommands.clear();
  response.resolve({ run: f.state.view });
  await sending;
  assert.equal(f.commands.length, 0);
  assert.equal(f.state.drafts.size, 0);
}
{
  const f = fixture();
  f.context.openConversation("B");
  f.$("#message-text").value = "draft B";
  f.context.openConversation("A");
  assert.equal(f.$("#message-text").value, "  draft A  ");
  f.context.api = async () => { throw new Error("message_failed"); };
  await f.send();
  assert.equal(f.state.drafts.get("A"), "  draft A  ");
  assert.equal(f.$("#message-text").disabled, false);
  assert.equal(f.notices.at(-1).message, "message_failed");
}
{
  const f = fixture();
  f.context.loadTab = async () => { throw new Error("refresh_failed"); };
  await f.send();
  assert.equal(f.state.drafts.has("A"), false, "accepted messages must not become retry drafts");
}
{
  const f = fixture("running");
  await f.send(true);
  assert.equal(f.requests.length, 0, "send-and-resume only for paused/created");
}
{
  const f = fixture();
  f.context.api = async (path) => {
    if (path === "/api/providers") return { providers: [{ provider_id: "p", model: "m", active: false }] };
    throw new Error("provider_failed");
  };
  await f.context.loadProviders();
  const row = f.$("#provider-list").children[0];
  const actions = row.children.at(-1).children[0];
  await actions.children[1].listeners.click();
  await actions.children[2].listeners.click();
  assert.equal(f.notices.length, 2);
  assert.ok(f.notices.every((notice) => notice.error && notice.message === "provider_failed"));
}
{
  const f = fixture();
  f.$("#provider-dialog").showModal();
  f.$("#provider-form").fields = { provider_id: "p", api_key: "current-key", provider: "anthropic", model: "new-model",
    base_url: "https://example.invalid", timeout_seconds: "42", max_context_tokens: "1000", max_output_tokens: "50", thinking_budget_tokens: "0" };
  f.context.api = async (path, options) => { f.requests.push({ path, options }); return { success: true, model: "new-model", latency_ms: 12, reply: "pong" }; };
  await f.context.testProvider();
  assert.equal(f.requests.length, 1);
  assert.equal(f.requests[0].path, "/api/providers/test");
  const payload = JSON.parse(f.requests[0].options.body);
  assert.equal(payload.api_key, "current-key");
  assert.equal(payload.provider_id, "p");
  assert.equal(payload.timeout_seconds, 42);
  assert.match(f.$("#provider-test-result").textContent, /连接成功.*\n模型：new-model.*12 ms\npong/);
  f.context.api = async () => ({ success: false, error: "bad-key", latency_ms: 5 });
  await f.context.testProvider();
  assert.match(f.$("#provider-test-result").textContent, /bad-key/);
  assert.equal(f.$("#provider-test-result").classList.contains("error"), true);
}
{
  const f = fixture();
  let page = 0;
  f.context.api = async (path) => {
    f.requests.push({ path }); page++;
    if (page === 2) throw new Error("page_failed");
    return { entries: [{ entry_id: `e${page}`, sequence: page, role: "assistant", text: "实际回复 摘要", created_at: "2026-10-10", branch_id: "main" }], truncated: page === 1, next_offset: page === 1 ? 200 : null };
  };
  await f.context.openForkEditor("A");
  assert.equal(f.requests[0].path, "/api/conversations/A/entries?limit=200&offset=0");
  assert.match(f.$("#fork-entry").children[0].textContent, /助手回复.*实际回复 摘要.*2026-10-10.*main/);
  await f.context.loadForkEntries(true);
  assert.equal(f.$("#fork-more").disabled, false);
  await f.context.loadForkEntries(true);
  assert.equal(f.requests[1].path, f.requests[2].path);
  assert.equal(f.$("#fork-entry").children.length, 2);
  assert.equal(f.$("#fork-more").hidden, true);
}
{
  const f = fixture(), response = deferred();
  f.context.fetch = (path, options) => { f.requests.push({ path, options }); return response.promise; };
  const downloading = f.context.exportReport();
  f.state.selectedId = "B";
  response.resolve({ ok: true, status: 200, blob: async () => ({}) });
  await downloading;
  assert.equal(f.requests[0].path, "/api/runs/A/report?format=markdown");
  assert.equal(f.requests[0].options.credentials, "same-origin");
  assert.equal(f.links[0].download, "trace-A-report.md");
}
console.log("frontend_controls_unit_smoke: PASS (send/drafts/races/auth/terminal/providers/entries/export/DOM)");
