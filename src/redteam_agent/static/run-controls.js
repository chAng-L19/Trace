"use strict";

function openConversation(runId, view = null) {
  if (state.selectedId) state.drafts.set(state.selectedId, $("#message-text").value);
  setView("runs");
  if (view && !state.runs.some((item) => coreRun(item).run_id === runId)) state.runs.push(view);
  state.activeTab = "transcript";
  $$('[role="tab"]').forEach((button) => {
    button.setAttribute("aria-selected", String(button.dataset.tab === "transcript"));
    button.tabIndex = button.dataset.tab === "transcript" ? 0 : -1;
  });
  $$('[role="tabpanel"]').forEach((panel) => { panel.hidden = panel.dataset.panel !== "transcript"; });
  $("#message-text").value = state.drafts.get(runId) || "";
  loadRun(runId);
}

function renderComposer() {
  const run = coreRun(state.view);
  const terminal = terminalStatuses.has(run.status);
  const blocked = !state.selectedId || run.status === "cancelling";
  const pending = state.pendingCommands.has(`${run.run_id}:message`);
  const paused = run.status === "paused_budget";
  $("#message-text").disabled = blocked || pending;
  $("#message-submit").disabled = blocked || pending;
  $("#message-submit").title = pending ? "正在发送" : terminal ? "追问结果" : "发送消息";
  $("#message-resume").hidden = !paused && run.status !== "created";
  $("#message-resume").disabled = blocked || pending;
  $("#message-state").textContent = pending ? "正在发送" : terminal ? "结果讨论，不执行工具" : paused ? "已暂停 · 发送消息不会恢复执行" : "";
}

async function submitMessage(event) {
  event.preventDefault();
  const runId = state.selectedId;
  const input = $("#message-text");
  const draft = input.value;
  const text = draft.trim();
  const status = coreRun(state.view).status;
  const resume = event.submitter?.id === "message-resume";
  const pendingKey = `${runId}:message`;
  if (!runId || !text || status === "cancelling" || state.pendingCommands.has(pendingKey)) return;
  if (resume && !["created", "paused_budget"].includes(status)) return;
  const epoch = state.authEpoch;
  const discussion = terminalStatuses.has(status);
  state.drafts.set(runId, draft);
  state.pendingCommands.add(pendingKey);
  renderComposer();
  let accepted = false;
  try {
    const result = await api(`/api/runs/${encodeURIComponent(runId)}/${discussion ? "discussion" : "message"}`, {
      method: "POST", headers: commandHeaders(), body: JSON.stringify({ text }),
    });
    if (epoch !== state.authEpoch) return;
    accepted = true;
    const discussionFailed = discussion && result.response?.status === "failed";
    if (!discussionFailed && state.drafts.get(runId) === draft) state.drafts.delete(runId);
    if (state.selectedId === runId) {
      input.value = state.drafts.get(runId) || "";
      applyView(result.run);
    }
    const nextStatus = coreRun(result.run).status;
    if (!discussion && (nextStatus === "created" || (resume && nextStatus === "paused_budget"))) {
      await postCommand(nextStatus === "created" ? "run" : "resume", { execute: true }, runId);
      if (epoch !== state.authEpoch) return;
    }
    if (state.selectedId === runId) {
      await loadTab("transcript");
      if (epoch !== state.authEpoch || state.selectedId !== runId) return;
      showNotice(discussionFailed
        ? `讨论回复失败：${result.response.error || result.response.text || "模型未返回回复"}。消息已记录，草稿已保留，可重试。`
        : discussion ? "结果讨论已更新" : "消息已加入当前会话", discussionFailed);
    }
  } catch (error) {
    if (epoch !== state.authEpoch) return;
    if (!accepted && !state.drafts.has(runId)) state.drafts.set(runId, draft);
    if (state.selectedId === runId) {
      input.value = state.drafts.get(runId) || "";
      showNotice(error.message, true);
    }
  } finally {
    if (epoch === state.authEpoch) {
      state.pendingCommands.delete(pendingKey);
      renderComposer();
    }
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
  $("#message-text").addEventListener("input", (event) => {
    if (state.selectedId) state.drafts.set(state.selectedId, event.target.value);
  });
  $("#copy-conversation-link").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(location.href); showNotice("会话地址已复制"); }
    catch (error) { showNotice(`复制失败：${error.message}`, true); }
  });
  $("#export-report-html").addEventListener("click", () => exportReport("html"));
  $("#provider-test").addEventListener("click", testProvider);
  $("#fork-more").addEventListener("click", () => loadForkEntries(true));
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

async function exportReport(format = "markdown") {
  if (typeof format !== "string") format = "markdown";
  const runId = state.selectedId;
  const epoch = state.authEpoch;
  if (!runId) return;
  try {
    const response = await fetch(`/api/runs/${encodeURIComponent(runId)}/report?format=${format}`, {
      credentials: "same-origin", cache: "no-store", headers: { Accept: format === "html" ? "text/html" : "text/markdown" },
    });
    if (epoch !== state.authEpoch) return;
    if (response.status === 401) { handleAuthExpired(); return; }
    if (!response.ok) throw new Error(`报告导出失败：HTTP ${response.status}`);
    const blob = await response.blob();
    if (epoch !== state.authEpoch) return;
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `trace-${runId}-report.${format === "html" ? "html" : "md"}`;
    link.click();
    URL.revokeObjectURL(url);
  } catch (error) {
    if (epoch === state.authEpoch) showNotice(error.message, true);
  }
}
