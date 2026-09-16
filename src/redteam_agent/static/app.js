"use strict";

const {
  $,
  $$,
  appendEmpty,
  coreRun,
  formatStatus,
  goal,
  renderArtifactList,
  renderEventTimeline,
  renderRecordList,
  renderRunList,
  renderSearchGraph,
  renderTranscriptList,
  safeJson,
  terminalStatuses,
  valueOr,
} = globalThis.TraceUI;

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
  authRequired: false,
  authEpoch: 0,
  search: "",
  registryScroll: 0,
};

function commandId() {
  return (
    globalThis.crypto?.randomUUID?.() ||
    `cmd-${Date.now()}-${Math.random().toString(16).slice(2)}`
  );
}

function commandHeaders() {
  return { "X-Command-ID": commandId() };
}

async function api(path, options = {}) {
  const headers = { Accept: "application/json", ...(options.headers || {}) };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(path, {
    cache: "no-store",
    credentials: "same-origin",
    ...options,
    headers,
  });
  const type = response.headers.get("Content-Type") || "";
  const payload = type.includes("application/json")
    ? await response.json()
    : await response.text();
  if (response.status === 401 && !String(path).startsWith("/api/auth/")) {
    handleAuthExpired();
  }
  if (!response.ok || (payload && payload.ok === false)) {
    throw new Error(payload?.error || `HTTP ${response.status}`);
  }
  return payload;
}

function showNotice(message, error = false) {
  const notice = $("#notice");
  notice.textContent = String(message);
  notice.classList.toggle("error", error);
  clearTimeout(showNotice.timer);
  const dialog = $("dialog[open]");
  if (error && dialog) {
    let feedback = dialog.querySelector(".dialog-feedback");
    if (!feedback) {
      feedback = document.createElement("p");
      feedback.className = "form-error dialog-feedback";
      feedback.setAttribute("role", "alert");
      const form = dialog.querySelector("form");
      form.insertBefore(feedback, form.querySelector(".dialog-actions"));
    }
    feedback.textContent = String(message);
    notice.hidden = true;
    return;
  }
  notice.hidden = false;
  showNotice.timer = setTimeout(() => {
    notice.hidden = true;
  }, 5000);
}

function setConnection(text, online) {
  const node = $("#connection");
  node.setAttribute("aria-label", text);
  node.classList.toggle("online", online);
  node.classList.toggle("offline", !online);
  $("#connection-text").textContent = text;
}

function applyView(view) {
  const incoming = coreRun(view);
  const current = coreRun(state.view);
  if (
    current.run_id === incoming.run_id &&
    Number(incoming.state_version || 0) < Number(current.state_version || 0)
  )
    return false;
  state.view = view;
  return true;
}

async function loadSystem() {
  const epoch = state.authEpoch;
  try {
    const result = await api("/api/system");
    if (
      epoch !== state.authEpoch ||
      (state.authRequired && !state.authenticated)
    )
      return;
    const provider = result.provider || {};
    setConnection(
      provider.configured
        ? `${provider.name} / ${provider.model}`
        : "未配置模型 API",
      provider.configured,
    );
    $("#runtime-badge").textContent =
      `runtime / ${valueOr(result.platform, "-")}`;
    $("#api-badge").textContent =
      `API / v${valueOr(result.schema_version, "?")}`;
    const health = $("#control-health");
    if (health) {
      health.classList.toggle("online", true);
      health.classList.remove("offline");
      health.innerHTML = `<span class="connection-dot" aria-hidden="true"></span><span>${provider.configured ? "模型连接已配置" : "等待模型配置"}</span>`;
    }
    if (result.control_plane)
      $("#system-view").textContent = safeJson({
        ...result.control_plane,
        provider: result.provider,
        auth: result.auth,
      });
  } catch (error) {
    setConnection("服务连接失败", false);
    const health = $("#control-health");
    if (health) {
      health.classList.remove("online");
      health.classList.add("offline");
      health.innerHTML = `<span class="connection-dot" aria-hidden="true"></span><span>API 连接失败</span>`;
    }
    showNotice(error.message, true);
  }
}

function setView(view) {
  state.page = view;
  $$(".top-nav .nav-button").forEach((button) => {
    const active = button.dataset.view === view;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  $(".app-shell").hidden = view !== "runs";
  $("#control-plane").hidden = view !== "control";
  if (view === "control") loadControl();
}

function showRunRegistry() {
  state.registryScroll = Math.max(0, state.registryScroll || 0);
  state.selectionVersion += 1;
  state.selectedId = "";
  state.view = null;
  state.report = null;
  state.collectionPages = {};
  closeEvents();
  renderRuns();
  renderView();
  requestAnimationFrame(() =>
    window.scrollTo({ top: state.registryScroll, behavior: "instant" }),
  );
}

async function loadRuns({ keepSelection = true } = {}) {
  const epoch = state.authEpoch;
  const filter = $("#status-filter").value;
  const query = new URLSearchParams({ limit: "200" });
  if (filter) query.set("status", filter);
  try {
    const result = await api(`/api/runs?${query}`);
    if (
      epoch !== state.authEpoch ||
      (state.authRequired && !state.authenticated)
    )
      return;
    state.runs = result.runs || [];
    renderRuns();
    if (!keepSelection || !state.selectedId) return;
    if (state.runs.some((item) => coreRun(item).run_id === state.selectedId))
      await loadRun(state.selectedId, false);
  } catch (error) {
    showNotice(error.message, true);
  }
}

function renderRuns() {
  renderRunList({
    views: state.runs,
    selectedId: state.selectedId,
    search: state.search,
    onSelect: (runId) => {
      state.registryScroll = window.scrollY;
      loadRun(runId);
    },
  });
}

async function loadRun(runId, resetEvents = true) {
  if (!resetEvents && state.selectedId !== runId) return;
  const selectionVersion = resetEvents
    ? ++state.selectionVersion
    : state.selectionVersion;
  const epoch = state.authEpoch;
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
    $("#search-map").replaceChildren();
    delete $("#search-map").dataset.selectedRecord;
    $("#search-inspector").replaceChildren();
    $("#search-raw").textContent = "";
    $("#search-count").textContent = "0 条记录";
    for (const container of $$(
      "#evidence-list, #artifact-list, #transcript-list",
    ))
      container.replaceChildren();
    for (const button of $$("[data-more]")) button.hidden = true;
  }
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}`);
    if (
      epoch !== state.authEpoch ||
      (state.authRequired && !state.authenticated)
    )
      return;
    if (
      state.selectedId !== runId ||
      state.selectionVersion !== selectionVersion
    )
      return;
    applyView(result.run);
    if (resetEvents) {
      await loadInitialEvents(runId, selectionVersion);
      if (
        state.selectedId !== runId ||
        state.selectionVersion !== selectionVersion
      )
        return;
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
  document.body.classList.toggle("run-focused", Boolean(view));
  $("#runs-index").hidden = Boolean(view);
  $("#empty-state").hidden = Boolean(view) || state.runs.length > 0;
  $("#workbench").hidden = !view;
  if (!view) return;
  const run = coreRun(view);
  const runGoal = goal(view);
  const budget = run.budget || {};
  const knownUsage =
    budget.input_tokens_used != null || budget.output_tokens_used != null;
  const unknownUsage =
    !knownUsage || Number(budget.token_usage_missing || 0) > 0;
  const usedTokens =
    (budget.input_tokens_used || 0) + (budget.output_tokens_used || 0);
  $("#run-status").textContent = formatStatus(run.status);
  $("#run-status").dataset.status = run.status || "";
  $("#run-id").textContent = run.run_id;
  $("#run-objective").textContent = valueOr(runGoal.objective);
  $("#run-targets").textContent = runGoal.targets?.length
    ? runGoal.targets.join(" · ")
    : "未绑定目标";
  $("#metric-actions").textContent =
    `${budget.actions_used || 0} / ${budget.action_limit || 0}`;
  $("#actions-progress").max = Math.max(1, budget.action_limit || 1);
  $("#actions-progress").value = budget.actions_used || 0;
  const tokenValue = unknownUsage
    ? knownUsage
      ? `${usedTokens}+（部分未知）`
      : "未知"
    : String(usedTokens);
  $("#metric-tokens").textContent =
    `${tokenValue} / ${budget.token_limit || "未设置"}`;
  $("#tokens-progress").max = Math.max(
    1,
    budget.token_limit || usedTokens || 1,
  );
  if (unknownUsage) $("#tokens-progress").removeAttribute("value");
  else $("#tokens-progress").value = usedTokens;
  $("#metric-next").textContent = valueOr(view.next_action);
  $("#metric-version").textContent = valueOr(run.state_version, 0);
  const terminal = terminalStatuses.has(run.status);
  const paused = run.status === "paused_budget";
  const waiting = run.status === "waiting_worker";
  $('[data-command="run"]').disabled =
    terminal || paused || run.status === "cancelling";
  $('[data-command="pause"]').disabled =
    terminal ||
    paused ||
    run.status === "created" ||
    run.status === "cancelling";
  $('[data-command="resume"]').disabled = terminal || !paused;
  $('[data-command="observation"]').disabled =
    terminal || run.status === "created" || run.status === "cancelling";
  $('[data-command="cancel"]').disabled =
    terminal || run.status === "cancelling";
  $("#open-budget").disabled = terminal;
  for (const button of $$("[data-command]")) {
    if (state.pendingCommands.has(`${run.run_id}:${button.dataset.command}`))
      button.disabled = true;
  }
  if (state.pendingCommands.has(`${run.run_id}:budget`))
    $("#open-budget").disabled = true;
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
    const result = await api(
      `/api/runs/${encodeURIComponent(runId)}/${command}`,
      {
        method: "POST",
        headers: { "X-Command-ID": commandId() },
        body: JSON.stringify(body),
      },
    );
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

async function loadInitialEvents(
  runId = state.selectedId,
  selectionVersion = state.selectionVersion,
) {
  if (!runId) return;
  const result = await api(
    `/api/runs/${encodeURIComponent(runId)}/events?limit=200`,
  );
  if (state.selectedId !== runId || state.selectionVersion !== selectionVersion)
    return;
  for (const event of result.events || []) appendEvent(event);
  state.eventCursor = Math.max(result.next_sequence || 0, state.eventCursor);
  renderEvents();
}

function openEvents() {
  if (!state.selectedId || !globalThis.EventSource) return;
  closeEvents();
  const query = new URLSearchParams({
    after_sequence: String(state.eventCursor),
    wait_seconds: "20",
    channel: "ui",
  });
  const source = new EventSource(
    `/api/runs/${encodeURIComponent(state.selectedId)}/events?${query}`,
  );
  source.addEventListener("trace-event", (message) => {
    try {
      const event = JSON.parse(message.data);
      if (
        event.run_id !== state.selectedId ||
        event.sequence <= state.eventCursor
      )
        return;
      state.eventCursor = event.sequence;
      appendEvent(event);
      renderEvents();
      clearTimeout(openEvents.refreshTimer);
      openEvents.refreshTimer = setTimeout(
        () => loadRun(state.selectedId, false),
        150,
      );
    } catch (error) {
      showNotice(`事件解析失败：${error.message}`, true);
    }
  });
  source.onerror = async () => {
    if (state.eventSource !== source) return;
    try {
      const auth = await api("/api/auth/status");
      if (auth.required && !auth.authenticated) {
        handleAuthExpired();
      }
    } catch (_) {
      /* reconnect keeps the live view available during transient failures */
    }
  };
  source.onopen = () =>
    ($("#event-cursor").textContent = `序号 ${state.eventCursor} · 实时`);
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
  if (state.events.length > 300)
    state.events.splice(0, state.events.length - 300);
  state.eventCursor = Math.max(state.eventCursor, Number(event.sequence) || 0);
}

function renderEvents() {
  $("#event-cursor").textContent = `序号 ${state.eventCursor}`;
  renderEventTimeline($("#event-list"), state.events);
}

async function loadTab(tab, append = false) {
  state.activeTab = tab;
  const tabVersion = ++state.tabVersion;
  if (!state.selectedId || tab === "events") return;
  const runId = state.selectedId;
  const selectionVersion = state.selectionVersion;
  const id = encodeURIComponent(runId);
  const current = () =>
    state.selectedId === runId &&
    state.selectionVersion === selectionVersion &&
    state.tabVersion === tabVersion;
  try {
    if (tab === "search") {
      const result = await api(`/api/runs/${id}/search-graph`);
      if (!current()) return;
      renderSearchGraph(
        $("#search-map"),
        $("#search-inspector"),
        $("#search-raw"),
        result,
      );
    } else if (["evidence", "artifacts", "transcript"].includes(tab)) {
      const endpoint = tab === "evidence" ? "evidence-graph" : tab;
      const key = {
        evidence: "nodes",
        artifacts: "artifacts",
        transcript: "messages",
      }[tab];
      const prior = append ? state.collectionPages[tab] || [] : [];
      const moreButton = $(`[data-more="${tab}"]`);
      moreButton.disabled = true;
      const result = await api(
        `/api/runs/${id}/${endpoint}?limit=200&offset=${prior.length}`,
      );
      if (!current()) return;
      const records = [...prior, ...(result[key] || [])];
      state.collectionPages[tab] = records;
      moreButton.hidden = !result.truncated;
      moreButton.disabled = false;
      if (tab === "evidence")
        renderRecords(
          $("#evidence-list"),
          records,
          "evidence_id",
          "artifact_type",
        );
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
    if (!current()) return;
    if (current() && moreButton) moreButton.disabled = false;
    showNotice(error.message, true);
  }
}

function renderRecords(container, records, idKey, typeKey) {
  renderRecordList(container, records, idKey, typeKey);
}

function renderArtifacts(artifacts) {
  renderArtifactList($("#artifact-list"), artifacts, state.selectedId);
}

function renderTranscript(messages) {
  renderTranscriptList($("#transcript-list"), messages);
}

function openCreateDialog() {
  $("#session-id").value =
    `trace-${new Date().toISOString().replace(/[:.]/g, "-")}`;
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
    targets: String(data.get("targets") || "")
      .split(/\r?\n/)
      .map((item) => item.trim())
      .filter(Boolean),
    max_actions: Number(data.get("max_actions") || 64),
  };
  if (data.get("token_limit"))
    payload.token_limit = Number(data.get("token_limit"));
  if (data.get("time_limit_seconds"))
    payload.time_limit_seconds = Number(data.get("time_limit_seconds"));
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
    action_id: valueOr(
      state.view?.handoff?.action_id,
      state.view?.next_action,
      "",
    ),
    output: {},
    tool: "host-agent",
    continue_run: true,
  });
  $("#observation-dialog").showModal();
}

async function submitObservation(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel")
    return $("#observation-dialog").close();
  let observation;
  try {
    observation = JSON.parse(String($("#observation-json").value || "{}"));
  } catch (_) {
    showNotice("观察 JSON 无效", true);
    return;
  }
  if (
    !observation ||
    Array.isArray(observation) ||
    typeof observation !== "object"
  ) {
    showNotice("观察必须是 JSON 对象", true);
    return;
  }
  $("#observation-dialog").close();
  await postCommand("observation", {
    observation: {
      ...observation,
      idempotency_key: observation.idempotency_key || commandId(),
    },
  });
}

function exportReport() {
  if (!state.report || state.report.run?.run?.run_id !== state.selectedId)
    return;
  const blob = new Blob([safeJson(state.report)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `trace-${state.selectedId}-report.json`;
  link.click();
  URL.revokeObjectURL(url);
}

function bind() {
  $$(".top-nav .nav-button, .control-tab").forEach((button) =>
    button.setAttribute("aria-pressed", String(button.classList.contains("active"))),
  );
  $("#login-dialog").addEventListener("cancel", (event) => event.preventDefault());
  $$("dialog").forEach((dialog) => dialog.addEventListener("close", () => {
    dialog.querySelector(".dialog-feedback")?.remove();
  }));
  $$(".top-nav .nav-button").forEach((button) =>
    button.addEventListener("click", () => setView(button.dataset.view)),
  );
  $$(".control-tab").forEach((button) =>
    button.addEventListener("click", async () => {
      $$(".control-tab").forEach((item) => {
        item.classList.toggle("active", item === button);
        item.setAttribute("aria-pressed", String(item === button));
      });
      $$("[data-control-panel]").forEach((panel) => {
        panel.hidden = panel.dataset.controlPanel !== button.dataset.controlTab;
      });
      await loadControl();
    }),
  );
  $("#logout").addEventListener("click", logout);
  $("#login-form").addEventListener("submit", login);
  $("#add-provider").addEventListener("click", () => openProviderEditor());
  $("#provider-form").addEventListener("submit", submitProvider);
  $("#skill-form").addEventListener("submit", submitSkill);
  $("#add-mcp").addEventListener("click", () => openMcpEditor());
  $("#mcp-form").addEventListener("submit", submitMcp);
  $("#refresh-skills").addEventListener("click", loadSkills);
  $("#refresh-mcp").addEventListener("click", loadMcp);
  $("#refresh-conversations").addEventListener("click", loadConversations);
  $("#fork-form").addEventListener("submit", submitFork);
  $("#observation-form").addEventListener("submit", submitObservation);
  $("#reload-system").addEventListener("click", async () => {
    await api("/api/system/reload", {
      method: "POST",
      headers: commandHeaders(),
      body: "{}",
    });
    await loadControl();
    showNotice("MCP 配置已重载");
  });
  $("#new-run").addEventListener("click", openCreateDialog);
  $("#empty-new-run").addEventListener("click", openCreateDialog);
  $("#back-to-runs").addEventListener("click", showRunRegistry);
  $("#refresh-runs").addEventListener("click", () => loadRuns());
  $("#run-search").addEventListener("input", (event) => {
    state.search = String(event.target.value || "");
    renderRuns();
  });
  $("#status-filter").addEventListener("change", () =>
    loadRuns({ keepSelection: false }),
  );
  $("#create-form").addEventListener("submit", submitCreate);
  $("#open-budget").addEventListener("click", () =>
    $("#budget-dialog").showModal(),
  );
  $("#budget-form").addEventListener("submit", submitBudget);
  $("#clear-events").addEventListener("click", () => {
    state.events = [];
    renderEvents();
  });
  $("#export-report").addEventListener("click", exportReport);
  $$("[data-more]").forEach((button) =>
    button.addEventListener("click", () => loadTab(button.dataset.more, true)),
  );
  $$("[data-command]").forEach((button) =>
    button.addEventListener("click", () => {
      const command = button.dataset.command;
      if (command === "observation") return openObservationEditor();
      if (command === "cancel" && !globalThis.confirm("确认取消此运行？"))
        return;
      postCommand(command, command === "resume" ? { execute: true } : {});
    }),
  );
  const tabs = $$('[role="tab"]');
  tabs.forEach((button, index) => {
    button.id = `tab-${button.dataset.tab}`;
    button.setAttribute("aria-controls", `panel-${button.dataset.tab}`);
    button.tabIndex = button.getAttribute("aria-selected") === "true" ? 0 : -1;
    const panel = $(`[data-panel="${button.dataset.tab}"]`);
    panel.id = `panel-${button.dataset.tab}`;
    panel.setAttribute("aria-labelledby", button.id);
    button.addEventListener("keydown", (event) => {
      const next = {
        ArrowRight: (index + 1) % tabs.length,
        ArrowLeft: (index + tabs.length - 1) % tabs.length,
        Home: 0,
        End: tabs.length - 1,
      }[event.key];
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
      $$('[role="tabpanel"]').forEach((panel) => {
        panel.hidden = panel.dataset.panel !== button.dataset.tab;
      });
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
