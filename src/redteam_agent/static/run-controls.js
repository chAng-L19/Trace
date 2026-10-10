"use strict";

function openConversation(runId, view = null) {
  setView("runs");
  if (view && !state.runs.some((item) => coreRun(item).run_id === runId)) state.runs.push(view);
  state.activeTab = "transcript";
  $$('[role="tab"]').forEach((button) => {
    button.setAttribute("aria-selected", String(button.dataset.tab === "transcript"));
    button.tabIndex = button.dataset.tab === "transcript" ? 0 : -1;
  });
  $$('[role="tabpanel"]').forEach((panel) => { panel.hidden = panel.dataset.panel !== "transcript"; });
  if (state.selectedId !== runId) $("#message-text").value = "";
  loadRun(runId);
}

function renderComposer() {
  const run = coreRun(state.view);
  const ended = terminalStatuses.has(run.status) || run.status === "cancelling";
  const pending = state.pendingCommands.has(`${run.run_id}:message`);
  $("#message-text").disabled = ended || pending;
  $("#message-submit").disabled = ended || pending;
  $("#message-submit").title = pending ? "正在发送" : "发送消息";
  $("#message-state").textContent = ended ? "此运行已结束" : pending ? "正在发送" : run.status === "paused_budget" ? "已暂停" : "";
}

async function submitMessage(event) {
  event.preventDefault();
  const runId = state.selectedId;
  const input = $("#message-text");
  const text = input.value.trim();
  if (!runId || !text) return;
  const epoch = state.authEpoch;
  const pendingKey = `${runId}:message`;
  state.pendingCommands.add(pendingKey);
  renderComposer();
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}/message`, {
      method: "POST", headers: commandHeaders(), body: JSON.stringify({ text }),
    });
    if (epoch !== state.authEpoch || state.selectedId !== runId) return;
    input.value = "";
    applyView(result.run);
    await loadTab("transcript");
    const status = coreRun(result.run).status;
    if (status === "created" || status === "paused_budget") {
      postCommand(status === "paused_budget" ? "resume" : "run", { execute: true });
    } else showNotice("消息已加入当前会话");
  } catch (error) {
    if (epoch === state.authEpoch && state.selectedId === runId) showNotice(error.message, true);
  } finally {
    state.pendingCommands.delete(pendingKey);
    renderComposer();
  }
}

async function deleteConversation(runId) {
  if (!runId || !confirm("确认删除此对话及其运行记录？")) return;
  try {
    await api(`/api/conversations/${encodeURIComponent(runId)}`, { method: "DELETE", headers: commandHeaders() });
    if (state.selectedId === runId) showRunRegistry();
    await Promise.all([loadConversations(), loadRuns({ keepSelection: false })]);
    showNotice("对话已删除");
  } catch (error) { showNotice(error.message, true); }
}

function bindRunControls() {
  $("#message-form").addEventListener("submit", submitMessage);
  $("#back-runs").addEventListener("click", showRunRegistry);
  $("#delete-conversation").addEventListener("click", () => deleteConversation(state.selectedId));
}

function openCreateDialog() {
  $("#create-form").elements.max_actions.disabled = $("#create-form").elements.unlimited_actions.checked;
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
    max_actions: data.get("unlimited_actions") === "on" ? 0 : Number(data.get("max_actions") || 64),
  };
  if (data.get("token_limit")) payload.token_limit = Number(data.get("token_limit"));
  if (data.get("time_limit_seconds")) payload.time_limit_seconds = Number(data.get("time_limit_seconds"));
  $("#create-submit").disabled = true;
  try {
    const result = await api("/api/runs", { method: "POST", headers: commandHeaders(), body: JSON.stringify(payload) });
    $("#create-dialog").close();
    form.reset();
    await loadRuns({ keepSelection: false });
    const view = result.runs?.[0];
    if (view) openConversation(coreRun(view).run_id, view);
    showNotice("运行已创建");
  } catch (error) { showNotice(error.message, true); }
  finally { $("#create-submit").disabled = false; }
}

async function submitBudget(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#budget-dialog").close();
  const data = new FormData(event.currentTarget);
  $("#budget-dialog").close();
  await postCommand("budget", {
    actions: Number(data.get("actions") || 0), tokens: Number(data.get("tokens") || 0),
    time_seconds: Number(data.get("time_seconds") || 0),
    acknowledge_missing_usage: data.get("acknowledge_missing_usage") === "on",
    unlimited_actions: data.get("unlimited_actions") === "on",
  });
}

function openObservationEditor() {
  if (!state.selectedId) return;
  $("#observation-json").value = safeJson({ action_id: valueOr(state.view?.handoff?.action_id, state.view?.next_action, ""),
    output: {}, tool: "host-agent", continue_run: true });
  $("#observation-dialog").showModal();
}

async function submitObservation(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#observation-dialog").close();
  let observation;
  try { observation = JSON.parse(String($("#observation-json").value || "{}")); }
  catch (_) { showNotice("观察 JSON 无效", true); return; }
  if (!observation || Array.isArray(observation) || typeof observation !== "object") {
    showNotice("观察必须是 JSON 对象", true); return;
  }
  $("#observation-dialog").close();
  await postCommand("observation", { observation: { ...observation, idempotency_key: observation.idempotency_key || commandId() } });
}

function exportReport() {
  if (!state.report || state.report.run?.run?.run_id !== state.selectedId) return;
  const url = URL.createObjectURL(new Blob([safeJson(state.report)], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = `trace-${state.selectedId}-report.json`;
  link.click();
  URL.revokeObjectURL(url);
}
