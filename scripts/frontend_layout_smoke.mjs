import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const root = fileURLToPath(new URL("../src/redteam_agent/static/", import.meta.url));
const html = readFileSync(`${root}/index.html`, "utf8");
const source = readFileSync(`${root}/app.js`, "utf8");
const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
assert.equal(new Set(ids).size, ids.length);
assert.equal(ids.filter((id) => id === "new-run").length, 1);
assert.equal((html.match(/data-control-tab=/g) || []).length, 5);
assert.doesNotMatch(html, /data-open-control|control-tabs|rail-history|empty-new-run|back-to-runs/);
for (const name of readdirSync(root).filter((name) => name.endsWith(".js"))) {
  const script = readFileSync(`${root}/${name}`, "utf8");
  for (const match of script.matchAll(/\$\("#([\w-]+)"\)/g)) {
    assert.ok(ids.includes(match[1]), `${name}: missing #${match[1]}`);
  }
}

const nodes = new Map();
function node(selector, dataset = {}) {
  if (nodes.has(selector)) return nodes.get(selector);
  const classes = new Set();
  const value = {
    dataset, hidden: false, disabled: false, textContent: "", attributes: {}, listeners: {},
    classList: {
      contains: (name) => classes.has(name),
      toggle(name, enabled) { enabled ? classes.add(name) : classes.delete(name); },
    },
    setAttribute(name, text) { this.attributes[name] = text; },
    removeAttribute(name) { delete this.attributes[name]; },
    addEventListener(name, callback) { this.listeners[name] = callback; },
  };
  nodes.set(selector, value);
  return value;
}
const runNav = node("runNav", { view: "runs" });
const tabs = ["providers", "skills", "mcp", "conversations", "system"].map((name) => node(name, { controlTab: name }));
const panels = tabs.map((tab) => node(`panel-${tab.dataset.controlTab}`, { controlPanel: tab.dataset.controlTab }));
const commands = ["run", "pause", "resume", "observation", "cancel"].map((name) => node(`[data-command="${name}"]`, { command: name }));
const state = { page: "runs", view: null, runs: [], pendingCommands: new Set(), eventCursor: 0 };
let loads = 0;
let returns = 0;
const context = vm.createContext({
  state, $: node,
  $$: (selector) => ({
    ".top-nav .nav-button": [runNav], ".control-tab": tabs,
    "[data-control-panel]": panels, "[data-command]": commands,
  })[selector],
  document: { body: node("body") },
  TraceLayout: { renderContext() {} },
  loadControl: async () => { loads += 1; },
  showRunRegistry: () => { returns += 1; },
  coreRun: (view) => view?.run || {}, goal: (view) => view.goal,
  formatStatus: (status) => status,
  valueOr: (value, fallback = "-") => value ?? fallback,
  terminalStatuses: new Set(["completed", "failed", "cancelled"]),
});
vm.runInContext(source.slice(source.indexOf("function setView("), source.indexOf("function showRunRegistry(")), context);
vm.runInContext(source.slice(source.indexOf("function renderView("), source.indexOf("async function postCommand(")), context);
const binding = source.slice(source.indexOf("function bind("));
vm.runInContext(binding.slice(binding.indexOf('  $$(".top-nav .nav-button").forEach'), binding.indexOf('  $("#logout")')), context);

for (const tab of tabs) {
  const prior = loads;
  await tab.listeners.click();
  assert.equal(loads, prior + 1, "one settings load per click");
  assert.equal(state.page, "control");
  assert.equal(node("#control-plane").hidden, false);
  assert.equal(panels.filter((panel) => !panel.hidden)[0].dataset.controlPanel, tab.dataset.controlTab);
}
runNav.listeners.click();
assert.equal(state.page, "runs");
assert.equal(returns, 1);
assert.equal(node("#control-plane").hidden, true);
assert.ok(tabs.every((tab) => tab.attributes["aria-pressed"] === "false"));

const objective = "Long task objective ".repeat(80);
for (const status of ["paused_budget", "waiting_worker", "completed"]) {
  state.view = { run: { run_id: "fixture", status, budget: {} }, goal: { objective, targets: ["target-a", "target-b"] } };
  vm.runInContext("renderView()", context);
  assert.equal(node("#run-objective").textContent, objective);
  assert.equal(node("#run-targets").textContent, "target-a\ntarget-b");
  assert.equal(node('[data-command="run"]').hidden, status === "paused_budget");
  assert.equal(node('[data-command="resume"]').hidden, status !== "paused_budget");
}
assert.equal(node('[data-command="cancel"]').disabled, true);
console.log("PASS: unique entry points, navigation, one settings load, task text, command states");
