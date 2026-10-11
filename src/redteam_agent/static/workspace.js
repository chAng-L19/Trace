"use strict";

(function () {
  const state = { items: [], selectedId: "", detail: null, timer: 0, listVersion: 0, detailVersion: 0, nextOffset: null };
  const $ = (selector) => document.querySelector(selector);

  function select(workspaceId) {
    state.selectedId = workspaceId;
    state.detail = null;
    state.detailVersion += 1;
    const url = new URL(location.href);
    url.searchParams.delete("run");
    url.searchParams.set("workspace", workspaceId);
    history.replaceState(null, "", url);
  }

  function node(tag, text, className) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined) item.textContent = text;
    return item;
  }

  function statusLabel(status) {
    return { active: "进行中", stopped: "已暂停", failed_integrity: "证据完整性校验失败" }[status]
      || globalThis.TraceUI.formatStatus(status);
  }

  function formatDate(value) {
    if (!value) return "-";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString("zh-CN", { hour12: false });
  }

  async function api(path, options) {
    return globalThis.TraceApp.api(path, options);
  }

  function open() {
    $("#runs-index").hidden = true;
    $("#workbench").hidden = true;
    $("#workspace-index").hidden = false;
    document.body.classList.remove("run-focused");
    select(new URL(location.href).searchParams.get("workspace") || state.selectedId);
    render();
    return load();
  }

  async function load(append = false) {
    append = append === true;
    if (append && state.nextOffset === null) return;
    const version = ++state.listVersion;
    const query = new URLSearchParams({
      search: $("#workspace-search").value || "",
      status: $("#workspace-status").value || "",
      limit: "200",
      offset: String(append ? state.nextOffset : 0),
    });
    try {
      const result = await api(`/api/workspaces?${query}`);
      if (version !== state.listVersion) return;
      const items = result.workspaces || [];
      state.items = append ? [...new Map([...state.items, ...items].map((item) => [item.workspace_id, item])).values()] : items;
      state.nextOffset = result.next_offset ?? null;
      if ((query.get("search") || query.get("status")) && state.selectedId
          && !state.items.some((item) => item.workspace_id === state.selectedId)) select("");
      render();
      if (state.selectedId) await loadDetail(state.selectedId);
    } catch (error) {
      if (version === state.listVersion) globalThis.TraceApp.showNotice(error.message, true);
    }
  }

  async function loadDetail(workspaceId) {
    if (!workspaceId) return;
    if (state.selectedId !== workspaceId) select(workspaceId);
    const version = ++state.detailVersion;
    render();
    try {
      const detail = await api(`/api/workspaces/${encodeURIComponent(workspaceId)}`);
      if (version !== state.detailVersion || state.selectedId !== workspaceId) return;
      state.detail = detail;
      render();
    } catch (error) {
      if (version !== state.detailVersion) return;
      state.detail = null;
      render();
      globalThis.TraceApp.showNotice(error.message, true);
    }
  }

  function render() {
    $("#workspace-count").textContent = String(state.items.length);
    $("#workspace-more").hidden = state.nextOffset === null;
    const list = $("#workspace-list");
    list.replaceChildren();
    if (!state.items.length) {
      list.append(node("p", "还没有工作区。先创建一个目标明确的工作区。", "workspace-list-empty"));
    }
    for (const item of state.items) {
      const button = node("button", undefined, `workspace-item${item.workspace_id === state.selectedId ? " selected" : ""}`);
      button.type = "button";
      button.dataset.workspaceId = item.workspace_id;
      const heading = node("div", undefined, "workspace-item-heading");
      heading.append(node("strong", item.title), node("span", statusLabel(item.status), `workspace-status workspace-status-${item.status}`));
      const meta = node("div", undefined, "workspace-item-meta");
      meta.append(node("span", `${item.run_count} 个运行`), node("span", `${item.hint_count} 条线索`), node("time", formatDate(item.updated_at)));
      button.append(heading, node("p", item.goal || "未填写目标", "workspace-item-goal"), meta);
      button.addEventListener("click", () => loadDetail(item.workspace_id));
      list.append(button);
    }
    const detail = $("#workspace-detail");
    const empty = $("#workspace-empty");
    detail.hidden = !state.detail;
    empty.hidden = Boolean(state.detail) || !state.items.length;
    if (state.detail) renderDetail(state.detail);
    globalThis.lucide?.createIcons();
  }

  function renderDetail(data) {
    const workspace = data.workspace;
    $("#workspace-detail-status").textContent = statusLabel(workspace.status);
    $("#workspace-detail-status").dataset.status = workspace.status;
    $("#workspace-detail-name").textContent = workspace.title;
    $("#workspace-detail-id").textContent = workspace.workspace_id;
    const summary = $("#workspace-summary");
    summary.replaceChildren();
    for (const [label, value] of [["目标", workspace.goal], ["来源", workspace.origin || "未绑定"], ["更新时间", formatDate(workspace.updated_at)]]) {
      const item = node("div", undefined, "workspace-summary-item");
      item.append(node("span", label), node("strong", value || "-"));
      summary.append(item);
    }
    const actions = $("#workspace-actions");
    actions.replaceChildren();
    if (workspace.status === "active") {
      actions.append(actionButton("stopped", "暂停工作区", "pause"));
      actions.append(actionButton("completed", "标记完成", "check", data.runs.some((item) => !["completed", "failed", "failed_integrity", "cancelled"].includes(item.status))));
    } else if (workspace.status === "stopped") {
      actions.append(actionButton("active", "恢复工作区", "play"));
      actions.append(actionButton("completed", "标记完成", "check", data.runs.some((item) => !["completed", "failed", "failed_integrity", "cancelled"].includes(item.status))));
    } else {
      actions.append(actionButton("active", "重新打开", "rotate-ccw"));
    }
    if (workspace.reason_worker === "operator") {
      actions.append(reasonButton("heartbeat", "续租推演", "heart-pulse"));
      actions.append(reasonButton("release", "释放推演", "unlock"));
    } else if (!workspace.reason_worker && workspace.status === "active") {
      actions.append(reasonButton("claim", "认领推演", "brain"));
    } else if (workspace.reason_worker) {
      actions.append(node("span", `推演由 ${workspace.reason_worker} 占用`, "workspace-record-meta"));
    }
    renderCollection("#workspace-runs", data.runs, (item) => {
      const intent = (data.intents || []).find((candidate) => candidate.run_id === item.run_id);
      const row = node("div", undefined, "workspace-record");
      const heading = node("div", undefined, "workspace-record-heading");
      heading.append(node("strong", item.objective || "未命名运行"), node("span", statusLabel(item.status)));
      row.append(heading, node("p", `${item.actions_used} / ${item.action_limit === 0 ? "无上限" : item.action_limit} 动作 · ${formatDate(item.updated_at)}`, "workspace-record-meta"));
      const button = node("button", "打开运行", "secondary small workspace-record-action");
      button.type = "button";
      button.addEventListener("click", () => { leave(); globalThis.TraceApp.setView("runs"); globalThis.TraceApp.openConversation(item.run_id); });
      row.append(button);
      if (intent && intent.status === "open") {
        const lifecycle = node("div", undefined, "workspace-record-action");
        if (!intent.worker) {
          lifecycle.append(intentButton(intent, "claim", "认领意图", "user-check"));
        } else if (intent.worker === "operator") {
          lifecycle.append(intentButton(intent, "heartbeat", "续租", "heart-pulse"));
          lifecycle.append(intentButton(intent, "release", "释放", "unlock"));
          if (["completed", "failed", "failed_integrity", "cancelled"].includes(item.status)) {
            lifecycle.append(intentButton(intent, "conclude", "记录结论", "check-check"));
          }
        } else {
          lifecycle.append(node("span", `已由 ${intent.worker} 认领`, "workspace-record-meta"));
        }
        row.append(lifecycle);
      }
      if (intent?.status === "concluded") row.append(node("p", "已有结论", "workspace-record-meta"));
      return row;
    }, "暂无运行");
    renderCollection("#workspace-facts", data.facts, (item) => {
      const row = node("div", undefined, "workspace-record");
      const source = { evidence: "来自已验证证据", workspace: "工作区设定",
        operator_conclusion: `操作员结论（未经证据验证） · ${item.creator || "operator"}` }[item.source] || item.source;
      row.append(node("strong", item.description || "未命名事实"), node("p", source, "workspace-record-meta"));
      return row;
    }, "还没有事实或已验证证据");
    renderCollection("#workspace-hints", data.hints, (item) => {
      const row = node("div", undefined, "workspace-record");
      row.append(node("p", item.content), node("span", `${item.creator} · ${formatDate(item.created_at)}`, "workspace-record-meta"));
      return row;
    }, "还没有操作线索");
  }

  function renderCollection(selector, values, renderItem, emptyText) {
    const container = $(selector);
    container.replaceChildren();
    if (!values?.length) { container.append(node("p", emptyText, "workspace-collection-empty")); return; }
    values.forEach((value) => container.append(renderItem(value)));
  }

  function actionButton(status, label, icon, disabled = false) {
    const button = node("button", undefined, "secondary small");
    button.type = "button";
    button.disabled = disabled;
    if (disabled) button.title = "请先结束工作区中的所有运行";
    button.append(node("i", undefined));
    button.firstChild.dataset.lucide = icon;
    button.append(node("span", label));
    button.addEventListener("click", () => setStatus(status));
    return button;
  }

  function intentButton(intent, action, label, icon) {
    const button = node("button", undefined, "secondary small");
    button.type = "button";
    button.append(node("i", undefined));
    button.firstChild.dataset.lucide = icon;
    button.append(node("span", label));
    button.addEventListener("click", () => intentAction(intent, action));
    return button;
  }

  function reasonButton(action, label, icon) {
    const button = node("button", undefined, "secondary small");
    button.type = "button";
    button.append(node("i", undefined));
    button.firstChild.dataset.lucide = icon;
    button.append(node("span", label));
    button.addEventListener("click", () => reasonAction(action));
    return button;
  }

  async function reasonAction(action) {
    const workspaceId = state.selectedId;
    const trigger = action === "claim" ? (window.prompt("这次推演要解决什么问题", "继续分析未验证路径") || "") : "heartbeat";
    if (action === "claim" && !trigger.trim()) return;
    try {
      await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/reason/${action}`, {
        method: "POST", body: JSON.stringify({ worker: "operator", trigger }),
      });
      if (workspaceId === state.selectedId) await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  async function intentAction(intent, action) {
    const workspaceId = state.selectedId;
    let description = "";
    if (action === "conclude") {
      description = window.prompt("记录操作员结论（不会标记为已验证证据）", "") || "";
      if (!description.trim()) return;
    }
    try {
      await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/intents/${encodeURIComponent(intent.id)}/${action}`, {
        method: "POST", body: JSON.stringify({ worker: "operator", description }),
      });
      if (workspaceId === state.selectedId) await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  async function setStatus(status) {
    const workspaceId = state.selectedId;
    if (!workspaceId) return;
    try {
      await api(`/api/workspaces/${encodeURIComponent(workspaceId)}`, { method: "POST", body: JSON.stringify({ action: status }) });
      if (workspaceId === state.selectedId) await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  async function submitCreate(event) {
    if (!beginSubmit(event)) return;
    const version = state.detailVersion;
    const form = event.currentTarget;
    const data = new FormData(form);
    const origin = String(data.get("origin") || "").trim();
    try {
      const result = await api("/api/workspaces", { method: "POST", body: JSON.stringify({
        title: data.get("title"), origin, goal: data.get("goal"),
        targets: String(data.get("targets") || "").split(/\r?\n/).map((item) => item.trim()).filter(Boolean),
        max_actions: data.get("unlimited_actions") ? 0 : 64,
      }) });
      if (version !== state.detailVersion || !form.closest("dialog").open) return;
      form.closest("dialog").close();
      select(result.workspace.workspace.workspace_id);
      $("#workspace-search").value = "";
      $("#workspace-status").value = "";
      form.reset();
      await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
    finally { form.dataset.pending = ""; form.querySelector('[value="default"]').disabled = false; }
  }

  async function submitHint(event) {
    if (!beginSubmit(event)) return;
    const version = state.detailVersion;
    const workspaceId = event.currentTarget.dataset.workspaceId;
    const form = event.currentTarget;
    try {
      await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/hints`, { method: "POST", body: JSON.stringify({ content: new FormData(form).get("content") }) });
      if (version !== state.detailVersion || !form.closest("dialog").open) return;
      form.closest("dialog").close(); form.reset(); if (workspaceId === state.selectedId) await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
    finally { form.dataset.pending = ""; form.querySelector('[value="default"]').disabled = false; }
  }

  async function submitIntent(event) {
    if (!beginSubmit(event)) return;
    const version = state.detailVersion;
    const workspaceId = event.currentTarget.dataset.workspaceId;
    const form = event.currentTarget; const data = new FormData(form);
    try {
      await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/intents`, { method: "POST", body: JSON.stringify({ description: data.get("description"), targets: String(data.get("targets") || "").split(/\r?\n/).map((item) => item.trim()).filter(Boolean), max_actions: data.get("unlimited_actions") ? 0 : 64 }) });
      if (version !== state.detailVersion || !form.closest("dialog").open) return;
      form.closest("dialog").close(); form.reset(); if (workspaceId === state.selectedId) await load();
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
    finally { form.dataset.pending = ""; form.querySelector('[value="default"]').disabled = false; }
  }

  async function rename() {
    const workspaceId = state.selectedId;
    if (!state.detail) return;
    const title = window.prompt("工作区名称", state.detail.workspace.title);
    if (!title?.trim()) return;
    try { await api(`/api/workspaces/${encodeURIComponent(workspaceId)}`, { method: "POST", body: JSON.stringify({ action: "rename", title }) }); if (workspaceId === state.selectedId) await load(); }
    catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  async function remove() {
    const workspaceId = state.selectedId;
    if (!workspaceId || !window.confirm("确认删除此工作区及其全部运行、对话和证据？此操作不可恢复。")) return;
    try { await api(`/api/workspaces/${encodeURIComponent(workspaceId)}`, { method: "DELETE" }); state.listVersion += 1; if (state.selectedId === workspaceId) select(""); await load(); }
    catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  async function download(format) {
    const workspaceId = state.selectedId;
    if (!workspaceId) return;
    try {
      const response = await fetch(`/api/workspaces/${encodeURIComponent(workspaceId)}/export?format=${format}`, { credentials: "same-origin" });
      if (!response.ok) throw new Error(`导出失败：HTTP ${response.status}`);
      const blob = await response.blob(); const url = URL.createObjectURL(blob); const link = node("a");
      link.href = url; link.download = `trace-workspace.${format === "markdown" ? "md" : "json"}`; link.click(); URL.revokeObjectURL(url);
    } catch (error) { globalThis.TraceApp.showNotice(error.message, true); }
  }

  function beginSubmit(event) {
    event.preventDefault();
    const form = event.currentTarget;
    if (event.submitter?.value === "cancel") {
      state.detailVersion += 1;
      form.closest("dialog").close();
      return false;
    }
    if (form.dataset.pending) return false;
    form.dataset.pending = "true";
    form.querySelector('[value="default"]').disabled = true;
    return true;
  }

  function leave() {
    state.listVersion += 1;
    state.detailVersion += 1;
    clearTimeout(state.timer);
    $("#workspace-index").hidden = true;
    const url = new URL(location.href);
    url.searchParams.delete("workspace");
    history.replaceState(null, "", url);
  }

  function showForm(kind) {
    const form = $(`#workspace-${kind}-form`);
    form.dataset.workspaceId = state.selectedId;
    form.closest("dialog").showModal();
  }

  function bind() {
    for (const id of ["workspace-dialog", "workspace-hint-dialog", "workspace-intent-dialog"]) {
      $(`#${id}`).addEventListener("cancel", () => { state.detailVersion += 1; });
    }
    $("#new-workspace").addEventListener("click", () => $("#workspace-dialog").showModal());
    $("#workspace-form").addEventListener("submit", submitCreate);
    $("#workspace-hint-form").addEventListener("submit", submitHint);
    $("#workspace-intent-form").addEventListener("submit", submitIntent);
    $("#workspace-add-hint").addEventListener("click", () => showForm("hint"));
    $("#workspace-add-intent").addEventListener("click", () => showForm("intent"));
    $("#workspace-rename").addEventListener("click", rename);
    $("#workspace-delete").addEventListener("click", remove);
    $("#workspace-export-md").addEventListener("click", () => download("markdown"));
    $("#workspace-export-json").addEventListener("click", () => download("json"));
    $("#refresh-workspaces").addEventListener("click", load);
    $("#workspace-more").addEventListener("click", () => load(true));
    $("#workspace-status").addEventListener("change", load);
    $("#workspace-search").addEventListener("input", () => { state.listVersion += 1; state.detailVersion += 1; clearTimeout(state.timer); state.timer = setTimeout(load, 180); });
  }

  // Reuse the existing startup/login restoration point, after authentication.
  const restoreSession = globalThis.restoreSessionUrl;
  globalThis.restoreSessionUrl = function () {
    if (new URL(location.href).searchParams.has("workspace")) {
      globalThis.TraceApp.setView("workspaces");
      open();
    } else restoreSession();
  };
  document.addEventListener("click", (event) => {
    const navigation = event.target.closest(".nav-button, .control-tab");
    if (navigation && navigation.dataset.view !== "workspaces") leave();
  }, true);
  new MutationObserver(() => {
    if (document.documentElement.dataset.traceAuth === "required") {
      state.listVersion += 1; state.detailVersion += 1;
      state.items = []; state.detail = null; state.selectedId = ""; state.nextOffset = null;
      render();
    }
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-trace-auth"] });
  bind();
  globalThis.TraceWorkspace = { open, load };
})();
