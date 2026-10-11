"use strict";

const {
  $,
  $$,
  appendEmpty,
  coreRun,
  formatStatus,
  formatTime,
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
  user: null,
  search: "",
  registryScroll: 0,
  drafts: new Map(),
};

function syncSessionUrl() {
  const url = new URL(location.href);
  if (state.selectedId) url.searchParams.set("run", state.selectedId);
  else url.searchParams.delete("run");
  history.replaceState(null, "", url);
}

function restoreSessionUrl() {
  const runId = new URL(location.href).searchParams.get("run");
  if (runId) openConversation(runId);
}

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
  const epoch = state.authEpoch;
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
  if (epoch === state.authEpoch && response.status === 401 && !["/api/auth/login", "/api/auth/status"].includes(String(path))) {
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
  state.runs = state.runs.map((item) => coreRun(item).run_id === incoming.run_id ? view : item);
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
    if (result.control_plane)
      TracePresentation.renderSystem($("#system-view"), result);
  } catch (error) {
    setConnection("服务连接失败", false);
    showNotice(error.message, true);
  }
}

function setView(view) {
  document.body.classList.toggle("control-open", view === "control");
  $("#workspace-title").textContent = view === "control"
    ? $$(".control-tab").find((button) => button.classList.contains("active"))?.textContent.trim() || "控制面"
    : view === "workspaces" ? "工作区" : "所有对话";
  state.page = view;
  $$(".top-nav .nav-button").forEach((button) => {
    const active = button.dataset.view === view;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  $(".app-shell").hidden = !["runs", "workspaces"].includes(view);
  $("#control-plane").hidden = view !== "control";
  $$(".control-tab").forEach((button) => button.setAttribute("aria-pressed", String(view === "control" && button.classList.contains("active"))));
}

function showRunRegistry() {
  if (state.selectedId) state.drafts.set(state.selectedId, $("#message-text").value);
  state.registryScroll = Math.max(0, state.registryScroll || 0);
  state.selectionVersion += 1;
  state.selectedId = "";
  syncSessionUrl();
  state.view = null;
  state.report = null;
  state.collectionPages = {};
  closeEvents();
  renderRuns();
  renderView();
  requestAnimationFrame(() =>
    $(".app-shell").scrollTo({ top: state.registryScroll, behavior: "instant" }),
  );
}

async function loadRuns({ keepSelection = true, append = false } = {}) {
  const epoch = state.authEpoch;
  const version = state.runsVersion = (state.runsVersion || 0) + 1;
  const more = $("#more-runs");
  more.disabled = true;
  const query = new URLSearchParams({ limit: "200", offset: String(append ? state.runs.length : 0), search: state.search, status: $("#status-filter").value });
  try {
    const result = await api(`/api/runs?${query}`);
    if (
      epoch !== state.authEpoch ||
      version !== state.runsVersion ||
      (state.authRequired && !state.authenticated)
    )
      return;
    state.runs = append ? [...state.runs, ...(result.runs || [])] : result.runs || [];
    more.hidden = !result.truncated;
    more.disabled = false;
    renderRuns();
    if (!keepSelection || !state.selectedId) return;
    if (state.runs.some((item) => coreRun(item).run_id === state.selectedId))
      await loadRun(state.selectedId, false);
  } catch (error) {
    showNotice(error.message, true);
    more.disabled = false;
  }
}

function renderRuns() {
  renderRunList({
    views: state.runs,
    selectedId: state.selectedId,
    search: "",
    status: $("#status-filter").value,
    onSelect: (runId) => {
      state.registryScroll = $(".app-shell").scrollTop;
      openConversation(runId);
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
  syncSessionUrl();
  renderRuns();
  if (resetEvents) {
    $(".tab-content").scrollTop = 0;
    $(".app-shell").scrollTop = 0;
    closeEvents();
    state.events = [];
    state.eventCursor = 0;
    state.report = null;
    state.view = state.runs.find((item) => coreRun(item).run_id === runId) || null;
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
    for (const container of $$("#tools-view, #attack-view, #transparency-view")) container.replaceChildren();
  }
  try {
    const current = () => epoch === state.authEpoch && state.selectedId === runId && state.selectionVersion === selectionVersion;
    await Promise.all([
      api(`/api/runs/${encodeURIComponent(runId)}`).then((result) => {
        if (!current()) return;
        applyView(result.run);
        renderRuns();
        renderView();
      }),
      resetEvents ? loadInitialEvents(runId, selectionVersion).then(() => { if (current()) openEvents(); }) : Promise.resolve(),
      loadTab(state.activeTab),
    ]);
  } catch (error) {
    showNotice(error.message, true);
  }
}

function renderView() {
  const view = state.view;
  TraceLayout.renderContext(view);
  document.body.classList.toggle("run-focused", Boolean(view));
  $("#runs-index").hidden = Boolean(view);
  $("#empty-state").hidden = Boolean(view) || state.runs.length > 0;
  $("#workbench").hidden = !view;
  if (!view) return;
  const run = coreRun(view);
  const runGoal = goal(view);
  const budget = run.budget || {};
  const knownUsage =
    budget.tokens_used != null || budget.input_tokens_used != null || budget.output_tokens_used != null;
  const unknownUsage = Number(budget.token_usage_missing || 0) > 0;
  const usedTokens =
    budget.tokens_used ?? (budget.input_tokens_used || 0) + (budget.output_tokens_used || 0);
  $("#run-status").textContent = formatStatus(run.status);
  $("#run-status").dataset.status = run.status || "";
  $("#run-id").textContent = run.run_id;
  $("#run-objective").textContent = valueOr(runGoal.objective);
  $("#run-targets").textContent = runGoal.targets?.length
    ? runGoal.targets.join("\n")
    : "未绑定目标";
  $("#metric-actions").textContent =
    `${budget.actions_used || 0} / ${budget.action_limit === 0 ? "无上限" : budget.action_limit || 64}`;
  $("#actions-progress").hidden = budget.action_limit === 0;
  $("#actions-progress").max = Math.max(1, budget.action_limit || 1);
  $("#actions-progress").value = budget.actions_used || 0;
  const tokenValue = unknownUsage
    ? knownUsage
      ? `${usedTokens}（部分调用未返回用量）`
      : "未返回 Token 用量"
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
  $("#metric-time").textContent = budget.time_limit_seconds ? `${budget.time_limit_seconds} 秒` : "未设置";
  $("#metric-deadline").textContent = budget.deadline ? formatTime(budget.deadline) : "未设置";
  const pauses = { user_requested: "已由你暂停", operator_pause: "已由你暂停", action_limit_exhausted: "动作预算已耗尽", token_limit_exhausted: "Token 预算已耗尽", token_usage_unknown: "模型未返回用量，预算核算已暂停", time_limit_exhausted: "时间预算已耗尽", cycle_action_limit: "本轮动作已完成", service_shutdown: "服务重启后已暂停" };
  $("#run-pause-reason").hidden = run.status !== "paused_budget";
  $("#run-pause-reason").textContent = pauses[budget.pause_reason] || "运行已暂停";
  const terminal = terminalStatuses.has(run.status);
  const paused = run.status === "paused_budget";
  const waiting = run.status === "waiting_worker";
  $('[data-command="run"]').hidden = paused;
  $('[data-command="resume"]').hidden = !paused;
  $('[data-command="pause"]').hidden = terminal || paused || run.status === "created" || run.status === "cancelling";
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
  renderComposer();
  for (const button of $$("[data-command]")) {
    if (state.pendingCommands.has(`${run.run_id}:${button.dataset.command}`))
      button.disabled = true;
  }
  if (state.pendingCommands.has(`${run.run_id}:budget`))
    $("#open-budget").disabled = true;
  $('[data-command="run"] span').textContent = waiting ? "继续" : "运行";
  $("#event-cursor").textContent = `序号 ${state.eventCursor}`;
}

async function postCommand(command, body = {}, runId = state.selectedId) {
  if (!runId) return;
  const epoch = state.authEpoch;
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
    if (epoch !== state.authEpoch || state.selectedId !== runId) return;
    applyView(result.run);
    renderView();
    await loadRuns({ keepSelection: false });
    showNotice(`${formatStatus(coreRun(state.view).status)}：命令已提交`);
  } catch (error) {
    if (epoch !== state.authEpoch) return;
    showNotice(error.message, true);
    if (state.selectedId === runId) await loadRun(runId, false);
  } finally {
    if (epoch === state.authEpoch) {
      state.pendingCommands.delete(pendingKey);
      renderView();
    }
  }
}

async function loadInitialEvents(
  runId = state.selectedId,
  selectionVersion = state.selectionVersion,
) {
  if (!runId) return;
  const result = await api(
    `/api/runs/${encodeURIComponent(runId)}/events?limit=200&channel=trajectory`,
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
    channel: "trajectory",
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
  source.addEventListener("trace-cursor", (message) => {
    try {
      const event = JSON.parse(message.data);
      if (event.run_id !== state.selectedId) return;
      const sequence = Number(event.sequence) || 0;
      if (sequence > state.eventCursor) {
        state.eventCursor = sequence;
        renderEvents();
      }
    } catch (error) {
      showNotice(`轨迹游标解析失败：${error.message}`, true);
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
  $("#workbench").dataset.activeTab = tab;
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
      const prior = state.collectionPages[tab] || [];
      const scroll = $(".tab-content");
      const oldTop = scroll.scrollTop, oldHeight = scroll.scrollHeight;
      const atBottom = oldHeight - oldTop - scroll.clientHeight < 64;
      const moreButton = $(`[data-more="${tab}"]`);
      moreButton.disabled = true;
      const offsets = append ? [prior.length] : Array.from({ length: Math.max(1, Math.ceil(prior.length / 200)) }, (_, index) => index * 200);
      const pages = await Promise.all(offsets.map((offset) => api(`/api/runs/${id}/${endpoint}?limit=200&offset=${offset}${tab === "transcript" ? "&latest=1" : tab === "evidence" ? "&include_payload=1" : ""}`)));
      if (!current()) return;
      const idKey = { evidence: "evidence_id", artifacts: "artifact_id", transcript: "message_id" }[tab];
      const merged = new Map(prior.map((item) => [item[idKey] || item.entry_id, item]));
      for (const item of pages.flatMap((page) => page[key] || [])) merged.set(item[idKey] || item.entry_id, item);
      const records = [...merged.values()];
      if (tab === "transcript") records.sort((left, right) => Number(left.sequence) - Number(right.sequence));
      state.collectionPages[tab] = records;
      moreButton.hidden = !pages.at(-1).truncated;
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
      scroll.scrollTop = tab === "transcript" && append ? oldTop + scroll.scrollHeight - oldHeight
        : tab === "transcript" && (!prior.length || atBottom) ? scroll.scrollHeight : oldTop;
    } else if (tab === "tools") {
      const result = await api(`/api/runs/${id}/tools`);
      if (!current()) return;
      TracePresentation.renderTools($("#tools-view"), result);
    } else if (tab === "attack") {
      const prior = state.collectionPages.attack;
      const offset = append ? prior?.next_offset || 0 : 0;
      const button = $('[data-more="attack"]');
      button.disabled = true;
      let result = await api(`/api/runs/${id}/asset-attack-graph?limit=200&offset=${offset}`);
      if (!current()) return;
      if (append && prior) {
        for (const [key, identity] of [["assets", "asset_id"], ["findings", "finding_id"], ["attack_paths", "path_id"]])
          result[key] = [...new Map([...(prior[key] || []), ...(result[key] || [])].map((item) => [item[identity], item])).values()];
        result.edges = [...new Map([...(prior.edges || []), ...(result.edges || [])].map((item) => [JSON.stringify(item), item])).values()];
        result.source_evidence_ids = [...new Set([...(prior.source_evidence_ids || []), ...(result.source_evidence_ids || [])])];
      }
      state.collectionPages.attack = result;
      button.hidden = !result.truncated;
      button.disabled = false;
      TracePresentation.renderAttack($("#attack-view"), result);
    } else if (tab === "transparency") {
      const result = await api(`/api/runs/${id}/transparency?limit=1000`);
      if (!current()) return;
      TracePresentation.renderTransparency($("#transparency-view"), result);
    } else if (tab === "report") {
      const result = await api(`/api/runs/${id}/report`);
      if (!current()) return;
      state.report = result;
      TracePresentation.renderReport($("#report-view"), state.report);
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

function bind() {
  bindRunControls();
  document.addEventListener("click", (event) => {
    const link = event.target.closest("[data-evidence-ref]");
    if (link) { event.preventDefault(); openEvidence(link.dataset.evidenceRef); }
  });
  $$(".top-nav .nav-button, .control-tab").forEach((button) =>
    button.setAttribute("aria-pressed", String(button.classList.contains("active") && (button.dataset.view === state.page || state.page === "control"))),
  );
  $("#login-dialog").addEventListener("cancel", (event) => event.preventDefault());
  $$("dialog").forEach((dialog) => dialog.addEventListener("close", () => {
    dialog.querySelector(".dialog-feedback")?.remove();
  }));
  $$(".top-nav .nav-button").forEach((button) =>
    button.addEventListener("click", () => {
      if (button.dataset.view === "workspaces") {
        setView("workspaces");
        globalThis.TraceWorkspace?.open();
        return;
      }
      setView("runs");
      showRunRegistry();
    }),
  );
  $$(".control-tab").forEach((button) =>
    button.addEventListener("click", async () => {
      setView("control");
      $$(".control-tab").forEach((item) => {
        item.classList.toggle("active", item === button);
        item.setAttribute("aria-pressed", String(item === button));
      });
      $$("[data-control-panel]").forEach((panel) => {
        panel.hidden = panel.dataset.controlPanel !== button.dataset.controlTab;
      });
      $("#workspace-title").textContent = button.textContent.trim();
      await loadControl();
    }),
  );
  $("#logout").addEventListener("click", logout);
  $("#login-form").addEventListener("submit", login);
  $("#profile-form").addEventListener("submit", saveAccount);
  $("#password-form").addEventListener("submit", saveAccount);
  $("#user-form").addEventListener("submit", submitUser);
  $("#add-user").addEventListener("click", () => { $("#user-form").reset(); $("#user-dialog").showModal(); });
  $("#add-provider").addEventListener("click", () => openProviderEditor());
  $("#configure-provider").addEventListener("click", () => openProviderEditor());
  $("#provider-form").addEventListener("submit", submitProvider);
  $("#skill-form").addEventListener("submit", submitSkill);
  $("#add-skill").addEventListener("click", () => openSkillEditor());
  $("#skill-import-form").addEventListener("submit", submitSkillImport);
  $("#import-skill").addEventListener("click", openSkillImportDialog);
  $("#add-mcp").addEventListener("click", () => openMcpEditor());
  $("#mcp-form").addEventListener("submit", submitMcp);
  $("#mcp-form").elements.transport.addEventListener("change", syncMcpTransport);
  $("#refresh-skills").addEventListener("click", loadSkills);
  $("#refresh-mcp").addEventListener("click", loadMcp);
  $("#refresh-conversations").addEventListener("click", loadConversations);
  $("#more-conversations").addEventListener("click", () => loadConversations(true));
  $("#conversation-search").addEventListener("input", () => {
    clearTimeout(loadConversations.searchTimer);
    loadConversations.searchTimer = setTimeout(() => loadConversations().catch((error) => showNotice(error.message, true)), 180);
  });
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
  $("#refresh-runs").addEventListener("click", () => loadRuns());
  $("#more-runs").addEventListener("click", () => loadRuns({ keepSelection: false, append: true }));
  $("#run-search").addEventListener("input", (event) => {
    state.search = String(event.target.value || "");
    clearTimeout(loadRuns.searchTimer);
    loadRuns.searchTimer = setTimeout(() => loadRuns({ keepSelection: false }), 180);
  });
  $("#status-filter").addEventListener("change", () =>
    loadRuns({ keepSelection: false }),
  );
  $("#create-form").addEventListener("submit", submitCreate);
  $("#open-budget").addEventListener("click", () => {
    const form = $("#budget-form");
    form.elements.unlimited_actions.checked = coreRun(state.view).budget?.action_limit === 0;
    form.elements.actions.disabled = form.elements.unlimited_actions.checked;
    $("#budget-dialog").showModal();
  });
  for (const [selector, field] of [["#create-form", "max_actions"], ["#budget-form", "actions"]]) {
    const form = $(selector);
    form.elements.unlimited_actions.addEventListener("change", (event) => { form.elements[field].disabled = event.target.checked; });
  }
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
      $(".tab-content").scrollTop = 0;
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
      restoreSessionUrl();
    }
  } catch (error) {
    showNotice(error.message, true);
  }
}

start();

globalThis.TraceApp = { api, showNotice, setView, commandHeaders, openConversation };

async function openEvidence(evidenceId) {
  const runId = state.selectedId, epoch = state.authEpoch;
  if (!runId) return;
  const dialog = $("#evidence-dialog"), body = $("#evidence-detail");
  const requestId = commandId();
  dialog.dataset.requestId = requestId;
  const current = () => runId === state.selectedId && epoch === state.authEpoch && dialog.open && dialog.dataset.requestId === requestId;
  body.textContent = "正在读取证据";
  if (!dialog.open) dialog.showModal();
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}/evidence-graph/${encodeURIComponent(evidenceId)}`);
    if (!current()) return;
    TracePresentation.renderEvidence(body, [result.node]);
    for (const [key, title] of [["ancestors", "上游证据"], ["descendants", "下游证据"]]) {
      const heading = document.createElement("h3");
      heading.textContent = title;
      body.append(heading, TracePresentation.readableValue((result[key] || []).map((item) => item.evidence_id), "evidence_ids"));
    }
  } catch (error) {
    if (current()) body.textContent = error.message;
  }
}
