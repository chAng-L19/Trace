"use strict";

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const terminalStatuses = new Set(["completed", "failed", "cancelled"]);
const statusLabels = {
  created: "已创建",
  running: "运行中",
  waiting_worker: "等待 Worker",
  paused_budget: "已暂停",
  cancelling: "取消中",
  cancelled: "已取消",
  completed: "已完成",
  failed: "失败",
};

const state = {
  runs: [],
  selectedId: "",
  view: null,
  activeTab: "events",
  eventCursor: 0,
  events: [],
  eventSource: null,
  report: null,
  selectionVersion: 0,
  pendingCommands: new Set(),
  tabVersion: 0,
  collectionPages: {},
};

function commandId() {
  return globalThis.crypto?.randomUUID?.() || `cmd-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

async function api(path, options = {}) {
  const headers = { Accept: "application/json", ...(options.headers || {}) };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(path, { ...options, headers });
  const type = response.headers.get("Content-Type") || "";
  const payload = type.includes("application/json") ? await response.json() : await response.text();
  if (!response.ok || (payload && payload.ok === false)) {
    throw new Error(payload?.error || `HTTP ${response.status}`);
  }
  return payload;
}

function showNotice(message, error = false) {
  const notice = $("#notice");
  notice.textContent = String(message);
  notice.classList.toggle("error", error);
  notice.hidden = false;
  clearTimeout(showNotice.timer);
  showNotice.timer = setTimeout(() => { notice.hidden = true; }, 5000);
}

function setConnection(text, online) {
  const node = $("#connection");
  node.classList.toggle("online", online);
  node.classList.toggle("offline", !online);
  $("#connection-text").textContent = text;
}

function coreRun(view) { return view?.run || {}; }
function goal(view) { return view?.goal || {}; }
function formatStatus(status) { return statusLabels[status] || status || "未知"; }
function valueOr(value, fallback = "-") { return value === null || value === undefined || value === "" ? fallback : value; }
function safeJson(value) { return JSON.stringify(value ?? {}, null, 2); }
function applyView(view) {
  const incoming = coreRun(view);
  const current = coreRun(state.view);
  if (
    current.run_id === incoming.run_id
    && Number(incoming.state_version || 0) < Number(current.state_version || 0)
  ) return false;
  state.view = view;
  return true;
}

async function loadSystem() {
  try {
    const result = await api("/api/system");
    const provider = result.provider || {};
    setConnection(provider.configured ? `${provider.name} / ${provider.model}` : "未配置模型 API", provider.configured);
  } catch (error) {
    setConnection("服务连接失败", false);
    showNotice(error.message, true);
  }
}

async function loadRuns({ keepSelection = true } = {}) {
  const filter = $("#status-filter").value;
  const query = new URLSearchParams({ limit: "200" });
  if (filter) query.set("status", filter);
  try {
    const result = await api(`/api/runs?${query}`);
    state.runs = result.runs || [];
    renderRuns();
    if (!keepSelection || !state.selectedId) return;
    if (state.runs.some((item) => coreRun(item).run_id === state.selectedId)) await loadRun(state.selectedId, false);
  } catch (error) {
    showNotice(error.message, true);
  }
}

function renderRuns() {
  const list = $("#run-list");
  list.replaceChildren();
  if (!state.runs.length) {
    const empty = document.createElement("p");
    empty.className = "run-empty";
    empty.textContent = "没有匹配的运行";
    list.append(empty);
    return;
  }
  for (const view of state.runs) {
    const run = coreRun(view);
    const button = document.createElement("button");
    button.type = "button";
    button.className = `run-item${run.run_id === state.selectedId ? " selected" : ""}`;
    button.dataset.runId = run.run_id;
    const status = document.createElement("span");
    status.textContent = formatStatus(run.status);
    const title = document.createElement("strong");
    title.textContent = valueOr(goal(view).objective, run.session_id);
    const id = document.createElement("code");
    id.textContent = run.run_id;
    button.append(status, title, id);
    button.addEventListener("click", () => loadRun(run.run_id));
    list.append(button);
  }
}

async function loadRun(runId, resetEvents = true) {
  if (!resetEvents && state.selectedId !== runId) return;
  const selectionVersion = resetEvents ? ++state.selectionVersion : state.selectionVersion;
  state.selectedId = runId;
  renderRuns();
  if (resetEvents) {
    closeEvents();
    state.events = [];
    state.eventCursor = 0;
    state.report = null;
    state.view = null;
    state.collectionPages = {};
    renderEvents();
    renderView();
    $("#report-view").textContent = "";
    $("#search-view").textContent = "";
    for (const container of $$("#evidence-list, #artifact-list, #transcript-list")) container.replaceChildren();
    for (const button of $$("[data-more]")) button.hidden = true;
  }
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}`);
    if (state.selectedId !== runId || state.selectionVersion !== selectionVersion) return;
    applyView(result.run);
    if (resetEvents) {
      await loadInitialEvents(runId, selectionVersion);
      if (state.selectedId !== runId || state.selectionVersion !== selectionVersion) return;
      openEvents();
    }
    renderRuns();
    renderView();
    await loadTab(state.activeTab);
  } catch (error) {
    showNotice(error.message, true);
  }
}

function renderView() {
  const view = state.view;
  $("#empty-state").hidden = Boolean(view);
  $("#workbench").hidden = !view;
  if (!view) return;
  const run = coreRun(view);
  const runGoal = goal(view);
  const budget = run.budget || {};
  const knownUsage = budget.input_tokens_used != null || budget.output_tokens_used != null;
  const unknownUsage = !knownUsage || Number(budget.token_usage_missing || 0) > 0;
  const usedTokens = (budget.input_tokens_used || 0) + (budget.output_tokens_used || 0);
  $("#run-status").textContent = formatStatus(run.status);
  $("#run-status").dataset.status = run.status || "";
  $("#run-id").textContent = run.run_id;
  $("#run-objective").textContent = valueOr(runGoal.objective);
  $("#run-targets").textContent = runGoal.targets?.length ? runGoal.targets.join(" · ") : "未绑定目标";
  $("#metric-actions").textContent = `${budget.actions_used || 0} / ${budget.action_limit || 0}`;
  $("#actions-progress").max = Math.max(1, budget.action_limit || 1);
  $("#actions-progress").value = budget.actions_used || 0;
  const tokenValue = unknownUsage ? (knownUsage ? `${usedTokens}+（部分未知）` : "未知") : String(usedTokens);
  $("#metric-tokens").textContent = `${tokenValue} / ${budget.token_limit || "未设置"}`;
  $("#tokens-progress").max = Math.max(1, budget.token_limit || usedTokens || 1);
  if (unknownUsage) $("#tokens-progress").removeAttribute("value");
  else $("#tokens-progress").value = usedTokens;
  $("#metric-next").textContent = valueOr(view.next_action);
  $("#metric-version").textContent = valueOr(run.state_version, 0);
  const terminal = terminalStatuses.has(run.status);
  const paused = run.status === "paused_budget";
  const waiting = run.status === "waiting_worker";
  $('[data-command="run"]').disabled = terminal || paused || run.status === "cancelling";
  $('[data-command="pause"]').disabled = terminal || paused || run.status === "created" || run.status === "cancelling";
  $('[data-command="resume"]').disabled = terminal || !paused;
  $('[data-command="cancel"]').disabled = terminal || run.status === "cancelling";
  $("#open-budget").disabled = terminal;
  for (const button of $$('[data-command]')) {
    if (state.pendingCommands.has(`${run.run_id}:${button.dataset.command}`)) button.disabled = true;
  }
  if (state.pendingCommands.has(`${run.run_id}:budget`)) $("#open-budget").disabled = true;
  if (waiting) $('[data-command="run"]').textContent = "继续";
  else $('[data-command="run"]').textContent = "运行";
  $("#event-cursor").textContent = `序号 ${state.eventCursor}`;
}

async function postCommand(command, body = {}) {
  if (!state.selectedId) return;
  const runId = state.selectedId;
  const pendingKey = `${runId}:${command}`;
  if (state.pendingCommands.has(pendingKey)) return;
  state.pendingCommands.add(pendingKey);
  const button = $(`[data-command="${command}"]`);
  if (button) button.disabled = true;
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}/${command}`, {
      method: "POST",
      headers: { "X-Command-ID": commandId() },
      body: JSON.stringify(body),
    });
    if (state.selectedId !== runId) return;
    applyView(result.run);
    renderView();
    await loadRuns({ keepSelection: false });
    showNotice(`${formatStatus(coreRun(state.view).status)}：命令已提交`);
  } catch (error) {
    showNotice(error.message, true);
    if (state.selectedId === runId) await loadRun(runId, false);
  } finally {
    state.pendingCommands.delete(pendingKey);
    renderView();
  }
}

async function loadInitialEvents(runId = state.selectedId, selectionVersion = state.selectionVersion) {
  if (!runId) return;
  const result = await api(`/api/runs/${encodeURIComponent(runId)}/events?limit=200`);
  if (state.selectedId !== runId || state.selectionVersion !== selectionVersion) return;
  for (const event of result.events || []) appendEvent(event);
  state.eventCursor = Math.max(result.next_sequence || 0, state.eventCursor);
  renderEvents();
}

function openEvents() {
  if (!state.selectedId || !globalThis.EventSource) return;
  closeEvents();
  const query = new URLSearchParams({ after_sequence: String(state.eventCursor), wait_seconds: "20", channel: "ui" });
  const source = new EventSource(`/api/runs/${encodeURIComponent(state.selectedId)}/events?${query}`);
  source.addEventListener("trace-event", (message) => {
    try {
      const event = JSON.parse(message.data);
      if (event.run_id !== state.selectedId || event.sequence <= state.eventCursor) return;
      state.eventCursor = event.sequence;
      appendEvent(event);
      renderEvents();
      clearTimeout(openEvents.refreshTimer);
      openEvents.refreshTimer = setTimeout(() => loadRun(state.selectedId, false), 150);
    } catch (error) {
      showNotice(`事件解析失败：${error.message}`, true);
    }
  });
  source.onopen = () => $("#event-cursor").textContent = `序号 ${state.eventCursor} · 实时`;
  state.eventSource = source;
}

function closeEvents() {
  clearTimeout(openEvents.refreshTimer);
  state.eventSource?.close();
  state.eventSource = null;
}

function appendEvent(event) {
  if (event.run_id !== state.selectedId) return;
  if (state.events.some((item) => item.sequence === event.sequence)) return;
  state.events.push(event);
  state.events.sort((left, right) => left.sequence - right.sequence);
  if (state.events.length > 300) state.events.splice(0, state.events.length - 300);
  state.eventCursor = Math.max(state.eventCursor, Number(event.sequence) || 0);
}

function renderEvents() {
  $("#event-cursor").textContent = `序号 ${state.eventCursor}`;
  const list = $("#event-list");
  list.replaceChildren();
  for (const event of [...state.events].reverse()) {
    const item = document.createElement("li");
    item.className = "event-item";
    const sequence = document.createElement("span");
    sequence.className = "event-sequence";
    sequence.textContent = `#${event.sequence}`;
    const name = document.createElement("span");
    name.className = "event-name";
    name.textContent = event.event_type;
    const summary = document.createElement("span");
    summary.className = "event-summary";
    summary.textContent = compactPayload(event.payload);
    item.append(sequence, name, summary);
    list.append(item);
  }
}

function compactPayload(payload) {
  if (!payload || !Object.keys(payload).length) return "-";
  const text = JSON.stringify(payload);
  return text.length > 260 ? `${text.slice(0, 257)}...` : text;
}

async function loadTab(tab, append = false) {
  state.activeTab = tab;
  const tabVersion = ++state.tabVersion;
  if (!state.selectedId || tab === "events") return;
  const runId = state.selectedId;
  const selectionVersion = state.selectionVersion;
  const id = encodeURIComponent(runId);
  const current = () => state.selectedId === runId && state.selectionVersion === selectionVersion && state.tabVersion === tabVersion;
  try {
    if (tab === "search") {
      const result = await api(`/api/runs/${id}/search-graph`);
      if (!current()) return;
      $("#search-view").textContent = safeJson(result.search_graph);
    } else if (["evidence", "artifacts", "transcript"].includes(tab)) {
      const endpoint = tab === "evidence" ? "evidence-graph" : tab;
      const key = { evidence: "nodes", artifacts: "artifacts", transcript: "messages" }[tab];
      const prior = append ? state.collectionPages[tab] || [] : [];
      const moreButton = $(`[data-more="${tab}"]`);
      moreButton.disabled = true;
      const result = await api(`/api/runs/${id}/${endpoint}?limit=200&offset=${prior.length}`);
      if (!current()) return;
      const records = [...prior, ...(result[key] || [])];
      state.collectionPages[tab] = records;
      moreButton.hidden = !result.truncated;
      moreButton.disabled = false;
      if (tab === "evidence") renderRecords($("#evidence-list"), records, "evidence_id", "artifact_type");
      else if (tab === "artifacts") renderArtifacts(records);
      else renderTranscript(records);
    } else if (tab === "report") {
      const result = await api(`/api/runs/${id}/report`);
      if (!current()) return;
      state.report = result;
      $("#report-view").textContent = safeJson(state.report);
    }
  } catch (error) {
    const moreButton = $(`[data-more="${tab}"]`);
    if (current() && moreButton) moreButton.disabled = false;
    showNotice(error.message, true);
  }
}

function renderRecords(container, records, idKey, typeKey) {
  container.replaceChildren();
  if (!records.length) return appendEmpty(container, "暂无记录");
  for (const value of records) {
    const record = document.createElement("article");
    record.className = "record";
    const head = document.createElement("div");
    head.className = "record-head";
    const title = document.createElement("strong");
    title.textContent = valueOr(value[typeKey], "记录");
    const id = document.createElement("code");
    id.textContent = valueOr(value[idKey]);
    const body = document.createElement("p");
    body.textContent = safeJson(value);
    head.append(title, id);
    record.append(head, body);
    container.append(record);
  }
}

function renderArtifacts(artifacts) {
  const container = $("#artifact-list");
  container.replaceChildren();
  if (!artifacts.length) return appendEmpty(container, "暂无产物");
  for (const artifact of artifacts) {
    const record = document.createElement("article");
    record.className = "record";
    const head = document.createElement("div");
    head.className = "record-head";
    const title = document.createElement("strong");
    title.textContent = valueOr(artifact.artifact_type, "artifact");
    const link = document.createElement("a");
    const artifactId = artifact.artifact_id || artifact.artifact_ref;
    link.href = `/api/runs/${encodeURIComponent(state.selectedId)}/artifacts/${encodeURIComponent(artifactId)}/content`;
    link.download = artifactId;
    link.textContent = "下载";
    const detail = document.createElement("p");
    detail.textContent = safeJson(artifact);
    head.append(title, link);
    record.append(head, detail);
    container.append(record);
  }
}

function renderTranscript(messages) {
  const container = $("#transcript-list");
  container.replaceChildren();
  if (!messages.length) return appendEmpty(container, "暂无会话");
  for (const message of messages) {
    const record = document.createElement("article");
    record.className = "record";
    const role = document.createElement("span");
    role.className = "transcript-role";
    role.textContent = valueOr(message.role, "unknown");
    const body = document.createElement("p");
    body.textContent = typeof message.content === "string" ? message.content : safeJson(message.content);
    record.append(role, body);
    container.append(record);
  }
}

function appendEmpty(container, text) {
  const node = document.createElement("p");
  node.className = "run-empty";
  node.textContent = text;
  container.append(node);
}

function openCreateDialog() {
  $("#session-id").value = `trace-${new Date().toISOString().replace(/[:.]/g, "-")}`;
  $("#create-dialog").showModal();
}

async function submitCreate(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#create-dialog").close();
  const form = event.currentTarget;
  const data = new FormData(form);
  const payload = {
    session_id: String(data.get("session_id") || "").trim(),
    objective: String(data.get("objective") || "").trim(),
    targets: String(data.get("targets") || "").split(/\r?\n/).map((item) => item.trim()).filter(Boolean),
    max_actions: Number(data.get("max_actions") || 64),
  };
  if (data.get("token_limit")) payload.token_limit = Number(data.get("token_limit"));
  if (data.get("time_limit_seconds")) payload.time_limit_seconds = Number(data.get("time_limit_seconds"));
  $("#create-submit").disabled = true;
  try {
    const result = await api("/api/runs", {
      method: "POST",
      headers: { "X-Command-ID": commandId() },
      body: JSON.stringify(payload),
    });
    $("#create-dialog").close();
    form.reset();
    const runId = result.runs?.[0]?.run?.run_id;
    await loadRuns({ keepSelection: false });
    if (runId) await loadRun(runId);
    showNotice("运行已创建");
  } catch (error) {
    showNotice(error.message, true);
  } finally {
    $("#create-submit").disabled = false;
  }
}

async function submitBudget(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#budget-dialog").close();
  const data = new FormData(event.currentTarget);
  const body = {
    actions: Number(data.get("actions") || 0),
    tokens: Number(data.get("tokens") || 0),
    time_seconds: Number(data.get("time_seconds") || 0),
    acknowledge_missing_usage: data.get("acknowledge_missing_usage") === "on",
  };
  $("#budget-dialog").close();
  await postCommand("budget", body);
}

function exportReport() {
  if (!state.report || state.report.run?.run?.run_id !== state.selectedId) return;
  const blob = new Blob([safeJson(state.report)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `trace-${state.selectedId}-report.json`;
  link.click();
  URL.revokeObjectURL(url);
}

function bind() {
  $("#new-run").addEventListener("click", openCreateDialog);
  $("#empty-new-run").addEventListener("click", openCreateDialog);
  $("#refresh-runs").addEventListener("click", () => loadRuns());
  $("#status-filter").addEventListener("change", () => loadRuns({ keepSelection: false }));
  $("#create-form").addEventListener("submit", submitCreate);
  $("#open-budget").addEventListener("click", () => $("#budget-dialog").showModal());
  $("#budget-form").addEventListener("submit", submitBudget);
  $("#clear-events").addEventListener("click", () => { state.events = []; renderEvents(); });
  $("#export-report").addEventListener("click", exportReport);
  $$('[data-more]').forEach((button) => button.addEventListener("click", () => loadTab(button.dataset.more, true)));
  $$('[data-command]').forEach((button) => button.addEventListener("click", () => {
    const command = button.dataset.command;
    if (command === "cancel" && !globalThis.confirm("确认取消此运行？")) return;
    postCommand(command, command === "resume" ? { execute: true } : {});
  }));
  const tabs = $$('[role="tab"]');
  tabs.forEach((button, index) => {
    button.id = `tab-${button.dataset.tab}`;
    button.setAttribute("aria-controls", `panel-${button.dataset.tab}`);
    button.tabIndex = button.getAttribute("aria-selected") === "true" ? 0 : -1;
    const panel = $(`[data-panel="${button.dataset.tab}"]`);
    panel.id = `panel-${button.dataset.tab}`;
    panel.setAttribute("aria-labelledby", button.id);
    button.addEventListener("keydown", (event) => {
      const next = { ArrowRight: (index + 1) % tabs.length, ArrowLeft: (index + tabs.length - 1) % tabs.length, Home: 0, End: tabs.length - 1 }[event.key];
      if (next === undefined) return;
      event.preventDefault();
      tabs[next].click();
      tabs[next].focus();
    });
    button.addEventListener("click", async () => {
      tabs.forEach((item) => {
        item.setAttribute("aria-selected", String(item === button));
        item.tabIndex = item === button ? 0 : -1;
      });
      $$('[role="tabpanel"]').forEach((panel) => { panel.hidden = panel.dataset.panel !== button.dataset.tab; });
      await loadTab(button.dataset.tab);
    });
  });
  window.addEventListener("beforeunload", closeEvents);
}

async function start() {
  bind();
  await Promise.all([loadSystem(), loadRuns({ keepSelection: false })]);
}

start();
