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
  page: "runs",
  authenticated: false,
};

function commandId() {
  return globalThis.crypto?.randomUUID?.() || `cmd-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function commandHeaders() { return { "X-Command-ID": commandId() }; }

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

async function loadAuth() {
  const result = await api("/api/auth/status");
  state.authenticated = Boolean(result.authenticated || !result.required);
  $("#logout").hidden = !state.authenticated;
  if (result.required && !state.authenticated) {
    $("#login-dialog").showModal();
    return false;
  }
  return true;
}

function setView(view) {
  state.page = view;
  $$(".top-nav .nav-button").forEach((button) => button.classList.toggle("active", button.dataset.view === view));
  $(".app-shell").hidden = view !== "runs";
  $("#control-plane").hidden = view !== "control";
  if (view === "control") loadControl();
}

async function loadControl() {
  try {
    await Promise.all([loadProviders(), loadSkills(), loadMcp(), loadConversations(), loadSystemSettings()]);
  } catch (error) { showNotice(error.message, true); }
}

function renderSettingsList(container, items, renderItem, empty = "暂无配置") {
  container.replaceChildren();
  if (!items.length) return appendEmpty(container, empty);
  items.forEach((item) => container.append(renderItem(item)));
}

async function loadProviders() {
  const result = await api("/api/providers");
  renderSettingsList($("#provider-list"), result.providers || [], (item) => {
    const row = document.createElement("article"); row.className = "setting-row";
    const main = document.createElement("div"); main.className = "setting-main";
    const title = document.createElement("strong"); title.textContent = `${item.name} · ${item.model}`;
    const meta = document.createElement("code"); meta.textContent = `${item.base_url} · key ${item.api_key_set ? "已绑定" : "未绑定"}`;
    main.append(title, meta);
    const actions = document.createElement("div"); actions.className = "button-row";
    const edit = document.createElement("button"); edit.type = "button"; edit.textContent = "编辑";
    edit.addEventListener("click", () => openProviderEditor(item));
    const activate = document.createElement("button"); activate.type = "button"; activate.textContent = item.active ? "当前模型" : "切换"; activate.disabled = item.active;
    activate.addEventListener("click", async () => { await api("/api/providers/active", { method: "POST", headers: commandHeaders(), body: JSON.stringify({ provider_id: item.provider_id }) }); showNotice("Provider 已切换"); await loadControl(); await loadSystem(); });
    const remove = document.createElement("button"); remove.type = "button"; remove.className = "danger"; remove.textContent = "删除";
    remove.addEventListener("click", async () => { if (!confirm("确认删除此 Provider？")) return; await api(`/api/providers/${encodeURIComponent(item.provider_id)}`, { method: "DELETE", headers: commandHeaders() }); await loadProviders(); });
    actions.append(edit, activate, remove); row.append(main, actions); return row;
  });
}

function openProviderEditor(item = null) {
  const form = $("#provider-form");
  form.reset();
  for (const [name, value] of Object.entries(item || {})) {
    const field = form.elements.namedItem(name);
    if (field && field.type !== "checkbox" && name !== "api_key_set") field.value = value ?? "";
  }
  $("#provider-clear-key").checked = false;
  $("#provider-dialog").showModal();
}

async function loadSkills() {
  const result = await api("/api/skills");
  renderSettingsList($("#skill-list"), result.skills || [], (item) => {
    const row = document.createElement("article"); row.className = "setting-row";
    const main = document.createElement("div"); main.className = "setting-main";
    const title = document.createElement("strong"); title.textContent = item.resource_id;
    const meta = document.createElement("code"); meta.textContent = `${item.byte_count} bytes · ${item.content_hash.slice(0, 12)}`;
    main.append(title, meta);
    const toggle = document.createElement("button"); toggle.type = "button"; toggle.textContent = item.enabled ? "已启用" : "已停用"; toggle.className = item.enabled ? "primary" : "quiet";
    toggle.addEventListener("click", async () => { await api(`/api/skills/${encodeURIComponent(item.resource_id)}`, { method: "POST", headers: commandHeaders(), body: JSON.stringify({ enabled: !item.enabled }) }); await loadSkills(); });
    const edit = document.createElement("button"); edit.type = "button"; edit.textContent = "配置";
    edit.addEventListener("click", () => openSkillEditor(item));
    const actions = document.createElement("div"); actions.className = "button-row"; actions.append(edit, toggle);
    row.append(main, actions); return row;
  });
}

function openSkillEditor(item) {
  $("#skill-id").value = item.resource_id || "";
  $("#skill-name").textContent = `${item.resource_id || "Skill"} · ${item.byte_count || 0} bytes`;
  $("#skill-config").value = safeJson(item.config || {});
  $("#skill-dialog").showModal();
}

async function submitSkill(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#skill-dialog").close();
  const id = String($("#skill-id").value || "");
  let config;
  try { config = JSON.parse(String($("#skill-config").value || "{}")); } catch (_) { showNotice("Skill 配置 JSON 无效", true); return; }
  if (!config || Array.isArray(config) || typeof config !== "object") { showNotice("Skill 配置必须是 JSON 对象", true); return; }
  try {
    await api(`/api/skills/${encodeURIComponent(id)}`, { method: "POST", headers: commandHeaders(), body: JSON.stringify({ config }) });
    $("#skill-dialog").close(); await loadSkills(); showNotice("Skill 配置已保存");
  } catch (error) { showNotice(error.message, true); }
}

async function loadMcp() {
  const result = await api("/api/mcp");
  renderSettingsList($("#mcp-list"), result.servers || [], (item) => {
    const row = document.createElement("article"); row.className = "setting-row";
    const main = document.createElement("div"); main.className = "setting-main";
    const title = document.createElement("strong"); title.textContent = item.server_id;
    const meta = document.createElement("code"); meta.textContent = `${item.transport} · ${item.status?.status || "configured"} · ${item.status?.tool_count || 0} tools`;
    main.append(title, meta);
    const actions = document.createElement("div"); actions.className = "button-row";
    const edit = document.createElement("button"); edit.type = "button"; edit.textContent = "编辑";
    edit.addEventListener("click", () => openMcpEditor(item));
    const remove = document.createElement("button"); remove.type = "button"; remove.className = "danger"; remove.textContent = "移除";
    remove.addEventListener("click", async () => { if (!confirm("确认移除 MCP 服务器？")) return; await api(`/api/mcp/${encodeURIComponent(item.server_id)}`, { method: "DELETE", headers: commandHeaders() }); await loadMcp(); });
    actions.append(edit, remove); row.append(main, actions); return row;
  });
}

function openMcpEditor(item = null) {
  const form = $("#mcp-form");
  form.reset();
  for (const [name, value] of Object.entries(item || {})) {
    const field = form.elements.namedItem(name);
    if (!field || ["env", "headers", "status", "enabled"].includes(name)) continue;
    field.value = name === "args" && Array.isArray(value) ? value.join("\n") : value ?? "";
  }
  $("#mcp-dialog").showModal();
}

async function loadConversations() {
  const result = await api("/api/conversations");
  renderSettingsList($("#conversation-list"), result.conversations || [], (item) => {
    const row = document.createElement("article"); row.className = "setting-row";
    const main = document.createElement("div"); main.className = "setting-main";
    const title = document.createElement("strong"); title.textContent = valueOr(item.run?.goal?.objective, item.run?.run?.session_id);
    const session = item.session || {};
    const activeBranch = String(session.active_branch_id || "main");
    const branches = session.branches && typeof session.branches === "object" ? session.branches : {};
    const meta = document.createElement("code"); meta.textContent = `${item.run?.run?.run_id || "-"} · ${item.message_count || 0} messages · 当前分支 ${activeBranch}`;
    main.append(title, meta);
    const branchHeads = document.createElement("small");
    branchHeads.className = "setting-detail";
    branchHeads.textContent = `分支头：${Object.entries(branches).map(([id, leaf]) => `${id}=${leaf || "空"}`).join(" · ") || "main=空"}`;
    main.append(branchHeads);
    const actions = document.createElement("div"); actions.className = "button-row";
    const open = document.createElement("button"); open.type = "button"; open.textContent = "打开"; open.addEventListener("click", () => { setView("runs"); loadRun(item.run?.run?.run_id); });
    const fork = document.createElement("button"); fork.type = "button"; fork.textContent = "分支";
    fork.addEventListener("click", () => openForkEditor(item.run?.run?.run_id));
    const checkout = document.createElement("select");
    checkout.setAttribute("aria-label", "切换会话分支");
    Object.keys(branches).sort().forEach((branchId) => {
      const option = document.createElement("option"); option.value = branchId; option.textContent = branchId; option.selected = branchId === activeBranch; checkout.append(option);
    });
    checkout.disabled = Object.keys(branches).length < 2;
    checkout.addEventListener("change", async () => {
      const runId = item.run?.run?.run_id;
      try {
        await api(`/api/conversations/${encodeURIComponent(runId)}/checkout`, {
          method: "POST", headers: commandHeaders(), body: JSON.stringify({ branch_id: checkout.value }),
        });
        await loadConversations();
        showNotice(`已切换到分支 ${checkout.value}`);
      } catch (error) { showNotice(error.message, true); checkout.value = activeBranch; }
    });
    actions.append(open, fork, checkout); row.append(main, actions); return row;
  });
}

async function openForkEditor(runId) {
  try {
    const result = await api(`/api/conversations/${encodeURIComponent(runId)}`);
    const nodes = result.tree?.nodes || {};
    const select = $("#fork-entry");
    select.replaceChildren();
    for (const [entryId, node] of Object.entries(nodes)) {
      const entry = node?.entry || {};
      const option = document.createElement("option");
      option.value = entryId;
      option.textContent = `#${entry.sequence ?? "?"} · ${entry.entry_type || "entry"} · ${entry.branch_id || "main"}`;
      select.append(option);
    }
    $("#fork-run-id").value = runId;
    $("#fork-branch-id").value = `branch-${Date.now().toString(36)}`;
    $("#fork-dialog").showModal();
  } catch (error) { showNotice(error.message, true); }
}

async function submitFork(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#fork-dialog").close();
  const data = new FormData(event.currentTarget);
  const runId = String(data.get("run_id") || "");
  try {
    const result = await api(`/api/conversations/${encodeURIComponent(runId)}/fork`, {
      method: "POST",
      headers: commandHeaders(),
      body: JSON.stringify({ from_entry_id: data.get("from_entry_id"), branch_id: data.get("branch_id") }),
    });
    $("#fork-dialog").close();
    await loadConversations();
    showNotice(`会话分支已创建：${result.active_branch_id || data.get("branch_id")}`);
  } catch (error) { showNotice(error.message, true); }
}

async function loadSystemSettings() {
  const result = await api("/api/system");
  $("#system-view").textContent = safeJson({ ...result.control_plane, provider: result.provider, auth: result.auth });
}

async function login(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const password = String(new FormData(form).get("password") || "");
  try {
    await api("/api/auth/login", { method: "POST", body: JSON.stringify({ password }) });
    $("#login-dialog").close(); form.reset(); state.authenticated = true; $("#logout").hidden = false;
    await Promise.all([loadSystem(), loadRuns({ keepSelection: false })]);
    showNotice("已登录 Trace");
  } catch (error) { showNotice("登录失败：" + error.message, true); }
}

async function logout() {
  await api("/api/auth/logout", { method: "POST", body: "{}" });
  clearSessionView();
  state.authenticated = false;
  setView("runs");
  $("#logout").hidden = true;
  $("#login-dialog").showModal();
}

function clearSessionView() {
  state.authenticated = false;
  state.selectedId = "";
  state.runs = [];
  state.view = null;
  state.events = [];
  state.eventCursor = 0;
  state.report = null;
  state.collectionPages = {};
  closeEvents();
  renderRuns();
  renderEvents();
  renderView();
}

async function submitProvider(event) {
  event.preventDefault(); if (event.submitter?.value === "cancel") return $("#provider-dialog").close();
  const form = event.currentTarget; const data = new FormData(form); const payload = Object.fromEntries(data.entries());
  payload.timeout_seconds = Number(payload.timeout_seconds); payload.max_context_tokens = Number(payload.max_context_tokens);
  payload.clear_api_key = data.get("clear_api_key") === "on";
  try { await api("/api/providers", { method: "POST", headers: commandHeaders(), body: JSON.stringify(payload) }); $("#provider-dialog").close(); form.reset(); await loadProviders(); showNotice("Provider 已保存"); } catch (error) { showNotice(error.message, true); }
}

async function submitMcp(event) {
  event.preventDefault(); if (event.submitter?.value === "cancel") return $("#mcp-dialog").close();
  const form = event.currentTarget; const data = new FormData(form); const payload = Object.fromEntries(data.entries()); payload.args = String(payload.args || "").split(/\r?\n/).map((x) => x.trim()).filter(Boolean);
  for (const field of ["env", "headers"]) {
    const raw = String(payload[field] || "").trim();
    if (!raw) { delete payload[field]; continue; }
    try { payload[field] = JSON.parse(raw); } catch (error) { showNotice(`${field} JSON 无效`, true); return; }
  }
  try { await api("/api/mcp", { method: "POST", headers: commandHeaders(), body: JSON.stringify(payload) }); $("#mcp-dialog").close(); form.reset(); await loadMcp(); showNotice("MCP 已保存"); } catch (error) { showNotice(error.message, true); }
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
  $('[data-command="observation"]').disabled = terminal || run.status === "created" || run.status === "cancelling";
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
  source.onerror = async () => {
    if (state.eventSource !== source) return;
    try {
      const auth = await api("/api/auth/status");
      if (auth.required && !auth.authenticated) {
        clearSessionView();
        setView("runs");
        state.authenticated = false;
        $("#logout").hidden = true;
        $("#login-dialog").showModal();
        showNotice("会话已过期，请重新登录", true);
      }
    } catch (_) { /* reconnect keeps the live view available during transient failures */ }
  };
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
    } else if (tab === "tools") {
      const result = await api(`/api/runs/${id}/tools`);
      if (!current()) return;
      $("#tools-view").textContent = safeJson(result);
    } else if (tab === "attack") {
      const result = await api(`/api/runs/${id}/asset-attack-graph`);
      if (!current()) return;
      $("#attack-view").textContent = safeJson(result);
    } else if (tab === "transparency") {
      const result = await api(`/api/runs/${id}/transparency?limit=1000`);
      if (!current()) return;
      $("#transparency-view").textContent = safeJson(result);
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

function openObservationEditor() {
  if (!state.selectedId) return;
  $("#observation-json").value = safeJson({
    action_id: valueOr(state.view?.handoff?.action_id, state.view?.next_action, ""),
    output: {},
    tool: "host-agent",
    continue_run: true,
  });
  $("#observation-dialog").showModal();
}

async function submitObservation(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#observation-dialog").close();
  let observation;
  try { observation = JSON.parse(String($("#observation-json").value || "{}")); } catch (_) { showNotice("观察 JSON 无效", true); return; }
  if (!observation || Array.isArray(observation) || typeof observation !== "object") { showNotice("观察必须是 JSON 对象", true); return; }
  $("#observation-dialog").close();
  await postCommand("observation", { observation: { ...observation, idempotency_key: observation.idempotency_key || commandId() } });
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
  $$(".top-nav .nav-button").forEach((button) => button.addEventListener("click", () => setView(button.dataset.view)));
  $$(".control-tab").forEach((button) => button.addEventListener("click", async () => {
    $$(".control-tab").forEach((item) => item.classList.toggle("active", item === button));
    $$("[data-control-panel]").forEach((panel) => { panel.hidden = panel.dataset.controlPanel !== button.dataset.controlTab; });
    await loadControl();
  }));
  $("#logout").addEventListener("click", logout);
  $("#login-form").addEventListener("submit", login);
  $("#add-provider").addEventListener("click", () => openProviderEditor());
  $("#provider-form").addEventListener("submit", submitProvider);
  $("#skill-form").addEventListener("submit", submitSkill);
  $("#add-mcp").addEventListener("click", () => $("#mcp-dialog").showModal());
  $("#mcp-form").addEventListener("submit", submitMcp);
  $("#refresh-skills").addEventListener("click", loadSkills);
  $("#refresh-mcp").addEventListener("click", loadMcp);
  $("#refresh-conversations").addEventListener("click", loadConversations);
  $("#fork-form").addEventListener("submit", submitFork);
  $("#observation-form").addEventListener("submit", submitObservation);
  $("#reload-system").addEventListener("click", async () => { await api("/api/system/reload", { method: "POST", headers: commandHeaders(), body: "{}" }); await loadControl(); showNotice("MCP 配置已重载"); });
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
    if (command === "observation") return openObservationEditor();
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
  try {
    if (await loadAuth()) {
      await Promise.all([loadSystem(), loadRuns({ keepSelection: false })]);
    }
  } catch (error) {
    showNotice(error.message, true);
  }
}

start();
