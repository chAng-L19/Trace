"use strict";

// Provider, Skill, MCP and conversation settings.
async function loadControl() {
  try {
    const tab = $(".control-tab.active").dataset.controlTab;
    await ({ providers: loadProviders, skills: loadSkills, mcp: loadMcp,
      conversations: loadConversations, system: loadSystemSettings,
      profile: loadProfile, users: loadUsers })[tab]();
  } catch (error) {
    showNotice(error.message, true);
  }
}
function renderSettingsList(container, items, renderItem, empty = "暂无配置") {
  container.replaceChildren();
  container.setAttribute("aria-busy", "false");
  if (!items.length) return appendEmpty(container, empty);
  items.forEach((item) => container.append(renderItem(item)));
}

function resourceToggle(enabled, update) {
  const label = document.createElement("label");
  label.className = "resource-toggle";
  const input = document.createElement("input");
  input.type = "checkbox";
  input.checked = enabled;
  input.setAttribute("aria-label", "启用");
  input.addEventListener("change", async () => {
    input.disabled = true;
    try { await update(input.checked); }
    catch (error) { input.checked = enabled; input.disabled = false; showNotice(error.message, true); }
  });
  label.append(input, "启用");
  return label;
}

function removeSetting(label, path, reload) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "danger";
  button.textContent = "删除";
  button.addEventListener("click", async () => {
    if (!confirm(`确认删除${label}？`)) return;
    try {
      await api(path, { method: "DELETE", headers: commandHeaders() });
      await reload();
    } catch (error) { showNotice(error.message, true); }
  });
  return button;
}

async function loadProviders() {
  const epoch = state.authEpoch;
  $("#provider-list").setAttribute("aria-busy", "true");
  const result = await api("/api/providers");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  const list = $("#provider-list");
  const providers = result.providers || [];
  list.replaceChildren();
  list.setAttribute("aria-busy", "false");
  $("#provider-empty").hidden = providers.length > 0;
  providers.forEach((item, index) => {
    const row = document.createElement("tr");
    const cell = (className, text) => {
      const node = document.createElement("td");
      node.className = className;
      node.textContent = text;
      row.append(node);
      return node;
    };
    cell("provider-number", String(index + 1).padStart(2, "0"));
    const identity = cell("provider-identity", item.name || item.provider_id);
    if (item.name && item.name !== item.provider_id) {
      const id = document.createElement("small");
      id.textContent = item.provider_id;
      identity.append(id);
    }
    cell("provider-endpoint", item.base_url);
    const model = document.createElement("span");
    model.className = "model-tag";
    model.textContent = item.model;
    cell("", "").append(model);
    const status = document.createElement("span");
    status.className = `provider-state${item.active ? " active" : ""}`;
    status.textContent = item.active ? "ACTIVE_STANDBY" : "STANDBY";
    const key = document.createElement("small");
    key.className = "provider-key";
    key.textContent = item.api_key_set ? "KEY: BOUND" : "KEY: UNBOUND";
    cell("", "").append(status, key);
    if (state.user?.role !== "admin") { list.append(row); return; }
    const actions = document.createElement("div");
    actions.className = "button-row";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.textContent = "编辑";
    edit.addEventListener("click", () => openProviderEditor(item));
    const activate = document.createElement("button");
    activate.type = "button";
    activate.textContent = item.active ? "当前模型" : "切换";
    activate.disabled = item.active;
    activate.addEventListener("click", async () => {
      await api("/api/providers/active", {
        method: "POST",
        headers: commandHeaders(),
        body: JSON.stringify({ provider_id: item.provider_id }),
      });
      showNotice("Provider 已切换");
      await loadControl();
      await loadSystem();
    });
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "danger";
    remove.textContent = "删除";
    remove.addEventListener("click", async () => {
      if (!confirm("确认删除此 Provider？")) return;
      await api(`/api/providers/${encodeURIComponent(item.provider_id)}`, {
        method: "DELETE",
        headers: commandHeaders(),
      });
      await loadProviders();
    });
    actions.append(edit, activate, remove);
    cell("provider-actions", "").append(actions);
    list.append(row);
  });
}

function openProviderEditor(item = null) {
  const form = $("#provider-form");
  form.reset();
  for (const [name, value] of Object.entries(item || {})) {
    const field = form.elements.namedItem(name);
    if (field && field.type !== "checkbox" && name !== "api_key_set")
      field.value = value ?? "";
  }
  $("#provider-clear-key").checked = false;
  form.elements.provider.onchange = () => {
    const anthropic = form.elements.provider.value === "anthropic";
    const url = form.elements.base_url;
    if (["https://api.openai.com/v1", "https://api.anthropic.com/v1"].includes(url.value))
      url.value = anthropic ? "https://api.anthropic.com/v1" : "https://api.openai.com/v1";
    form.elements.api_key_env.placeholder = anthropic ? "ANTHROPIC_API_KEY" : "OPENAI_API_KEY";
    if (!anthropic) {
      form.elements.thinking_type.value = "";
      form.elements.thinking_budget_tokens.value = "0";
    }
  };
  $("#provider-dialog").showModal();
}

async function loadSkills() {
  const epoch = state.authEpoch;
  $("#skill-list").setAttribute("aria-busy", "true");
  const result = await api("/api/skills");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  renderSettingsList($("#skill-list"), result.skills || [], (item) => {
    const row = document.createElement("article");
    row.className = "setting-row";
    const main = document.createElement("div");
    main.className = "setting-main";
    const title = document.createElement("strong");
    title.textContent = item.resource_id;
    const meta = document.createElement("code");
    meta.textContent = `${item.byte_count} bytes · ${item.content_hash.slice(0, 12)}`;
    main.append(title, meta);
    if (state.user?.role !== "admin") { row.append(main); return row; }
    const toggle = resourceToggle(item.enabled, async (enabled) => {
      await api(`/api/skills/${encodeURIComponent(item.resource_id)}`, {
        method: "POST",
        headers: commandHeaders(),
        body: JSON.stringify({ enabled }),
      });
      await loadSkills();
    });
    const edit = document.createElement("button");
    edit.type = "button";
    edit.textContent = "配置";
    edit.addEventListener("click", () => openSkillEditor(item));
    const actions = document.createElement("div");
    actions.className = "button-row";
    actions.append(toggle, edit, removeSetting(` Skill ${item.resource_id}`, `/api/skills/${encodeURIComponent(item.resource_id)}`, loadSkills));
    row.append(main, actions);
    return row;
  });
}

async function openSkillEditor(item = null) {
  const epoch = state.authEpoch;
  const form = $("#skill-form");
  form.reset();
  form.dataset.mode = item ? "edit" : "create";
  $("#skill-id").value = item?.resource_id || "";
  $("#skill-id").readOnly = Boolean(item);
  $("#skill-name").textContent =
    item ? `${item.resource_id} · ${item.byte_count} bytes` : "";
  $("#skill-config").value = safeJson(item?.config || {});
  $("#skill-content").value = "";
  $("#skill-content").disabled = Boolean(item);
  const submit = form.querySelector('button[value="default"]');
  submit.disabled = Boolean(item);
  $("#skill-dialog").showModal();
  if (item) {
    try {
      const result = await api(`/api/skills/${encodeURIComponent(item.resource_id)}`);
      if (epoch !== state.authEpoch || form.dataset.mode !== "edit" || $("#skill-id").value !== item.resource_id) return;
      $("#skill-content").value = result.skill.content;
      $("#skill-content").disabled = false;
      submit.disabled = false;
    } catch (error) { showNotice(error.message, true); }
  }
}

async function submitSkill(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#skill-dialog").close();
  const id = String($("#skill-id").value || "");
  let config;
  try {
    config = JSON.parse(String($("#skill-config").value || "{}"));
  } catch (_) {
    showNotice("Skill 配置 JSON 无效", true);
    return;
  }
  if (!config || Array.isArray(config) || typeof config !== "object") {
    showNotice("Skill 配置必须是 JSON 对象", true);
    return;
  }
  try {
    await api($("#skill-form").dataset.mode === "create" ? "/api/skills" : `/api/skills/${encodeURIComponent(id)}`, {
      method: "POST",
      headers: commandHeaders(),
      body: JSON.stringify({ resource_id: id, content: $("#skill-content").value, config }),
    });
    $("#skill-dialog").close();
    await loadSkills();
    showNotice("Skill 配置已保存");
  } catch (error) {
    showNotice(error.message, true);
  }
}
async function loadMcp() {
  const epoch = state.authEpoch;
  $("#mcp-list").setAttribute("aria-busy", "true");
  const result = await api("/api/mcp");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  renderSettingsList($("#mcp-list"), result.servers || [], (item) => {
    const row = document.createElement("article");
    row.className = "setting-row";
    const main = document.createElement("div");
    main.className = "setting-main";
    const title = document.createElement("strong");
    title.textContent = item.source === "catalog" && item.preset === "playwright"
      ? "Playwright MCP" : item.server_id;
    const meta = document.createElement("code");
    const statuses = { available: "待接入", configured: "已配置", connected: "已连接",
      catalogued: "按运行连接", disabled: "已停用", failed: "连接失败", duplicate: "重复配置" };
    const status = item.status?.status || "configured";
    const source = { managed: "网页托管", config: "配置文件", catalog: "公开 MCP" }[item.source] || "网页托管";
    const toolCount = Number(item.status?.tool_count || 0);
    meta.textContent = `${item.transport || "-"} · ${statuses[status] || status} · ${toolCount} tools · ${source}`;
    main.append(title, meta);
    const detail = document.createElement("small");
    detail.className = `setting-detail mcp-state mcp-state-${status}`;
    detail.textContent = item.enabled
      ? (item.status?.callable ? `运行中，可调用 ${toolCount} 个工具` : "已开启，等待连接")
      : "已停用，不会注入运行";
    main.append(detail);
    if (item.status?.error) {
      const error = document.createElement("small");
      error.className = "setting-detail";
      error.textContent = item.status.error;
      main.append(error);
    }
    const actions = document.createElement("div");
    actions.className = "button-row";
    if (state.user?.role === "admin") {
      const toggle = resourceToggle(item.enabled, async (enabled) => {
        await api(`/api/mcp/${encodeURIComponent(item.server_id)}`, {
          method: "POST", headers: commandHeaders(), body: JSON.stringify({ enabled }),
        });
        await loadMcp();
      });
      const edit = document.createElement("button");
      edit.type = "button";
      edit.textContent = "编辑";
      edit.addEventListener("click", () => openMcpEditor(item));
      const remove = removeSetting(` MCP ${item.server_id}`, `/api/mcp/${encodeURIComponent(item.server_id)}`, loadMcp);
      const inspect = document.createElement("button");
      inspect.type = "button";
      inspect.textContent = "查看工具";
      inspect.disabled = !toolCount;
      inspect.addEventListener("click", () => {
        showNotice(`${title.textContent} 已发现 ${toolCount} 个工具；启动运行后可在“工具”页查看可见目录。`);
      });
      actions.append(toggle, inspect, edit, remove);
    }
    row.append(main, actions);
    return row;
  });
}

function openMcpEditor(item = null) {
  const form = $("#mcp-form");
  form.reset();
  form.elements.server_id.readOnly = Boolean(item);
  form.elements.enabled.checked = item?.source === "catalog" || item === null || item.enabled;
  for (const [name, value] of Object.entries(item || {})) {
    const field = form.elements.namedItem(name);
    if (!field || ["env", "headers", "status", "enabled"].includes(name))
      continue;
    field.value =
      name === "args" && Array.isArray(value)
        ? value.join("\n")
        : (value ?? "");
  }
  $("#mcp-dialog").showModal();
}

async function loadConversations() {
  const epoch = state.authEpoch;
  $("#conversation-list").setAttribute("aria-busy", "true");
  const result = await api("/api/conversations");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  renderSettingsList(
    $("#conversation-list"),
    result.conversations || [],
    (item) => {
      const row = document.createElement("article");
      row.className = "setting-row";
      const main = document.createElement("div");
      main.className = "setting-main";
      const title = document.createElement("strong");
      title.textContent = valueOr(
        item.run?.goal?.objective,
        item.run?.run?.session_id,
      );
      const session = item.session || {};
      const activeBranch = String(session.active_branch_id || "main");
      const branches =
        session.branches && typeof session.branches === "object"
          ? session.branches
          : {};
      const meta = document.createElement("code");
      meta.textContent = `${item.message_count || 0} 条消息 · ${formatStatus(item.run?.run?.status)} · ${formatTime(item.run?.run?.updated_at)}`;
      main.append(title, meta);
      const branchHeads = document.createElement("small");
      branchHeads.className = "setting-detail";
      branchHeads.textContent = `${Object.keys(branches).length || 1} 个分支 · 当前：${activeBranch === "main" ? "主线" : activeBranch}`;
      main.append(branchHeads);
      const actions = document.createElement("div");
      actions.className = "button-row";
      const open = document.createElement("button");
      open.type = "button";
      open.textContent = "打开";
      open.addEventListener("click", () => {
        setView("runs");
        if (!state.runs.some((view) => coreRun(view).run_id === item.run?.run?.run_id)) state.runs.push(item.run);
        state.activeTab = "transcript";
        $$('[role="tab"]').forEach((button) => {
          button.setAttribute("aria-selected", String(button.dataset.tab === "transcript"));
          button.tabIndex = button.dataset.tab === "transcript" ? 0 : -1;
        });
        $$('[role="tabpanel"]').forEach((panel) => { panel.hidden = panel.dataset.panel !== "transcript"; });
        loadRun(item.run?.run?.run_id);
      });
      const fork = document.createElement("button");
      fork.type = "button";
      fork.textContent = "分支";
      fork.addEventListener("click", () =>
        openForkEditor(item.run?.run?.run_id),
      );
      const checkout = document.createElement("select");
      checkout.setAttribute("aria-label", "切换会话分支");
      Object.keys(branches)
        .sort()
        .forEach((branchId) => {
          const option = document.createElement("option");
          option.value = branchId;
          option.textContent = branchId === "main" ? "主线" : branchId;
          option.selected = branchId === activeBranch;
          checkout.append(option);
        });
      checkout.disabled = Object.keys(branches).length < 2;
      checkout.addEventListener("change", async () => {
        const runId = item.run?.run?.run_id;
        try {
          await api(
            `/api/conversations/${encodeURIComponent(runId)}/checkout`,
            {
              method: "POST",
              headers: commandHeaders(),
              body: JSON.stringify({ branch_id: checkout.value }),
            },
          );
          await loadConversations();
          showNotice(`已切换到分支 ${checkout.value}`);
        } catch (error) {
          showNotice(error.message, true);
          checkout.value = activeBranch;
        }
      });
      const remove = removeSetting("此对话及其运行记录", `/api/conversations/${encodeURIComponent(item.run?.run?.run_id)}`, async () => {
        if (state.selectedId === item.run?.run?.run_id) showRunRegistry();
        await Promise.all([loadConversations(), loadRuns({ keepSelection: false })]);
      });
      actions.append(open, fork, checkout, remove);
      row.append(main, actions);
      return row;
    },
  );
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
      const roles = { user: "用户消息", assistant: "助手回复", tool: "工具结果", observation: "观察结果" };
      option.textContent = `第 ${entry.sequence ?? "?"} 条 · ${roles[entry.entry_type] || "会话记录"}`;
      select.append(option);
    }
    $("#fork-run-id").value = runId;
    $("#fork-branch-id").value = `branch-${Date.now().toString(36)}`;
    $("#fork-dialog").showModal();
  } catch (error) {
    showNotice(error.message, true);
  }
}
async function submitFork(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#fork-dialog").close();
  const data = new FormData(event.currentTarget);
  const runId = String(data.get("run_id") || "");
  try {
    const result = await api(
      `/api/conversations/${encodeURIComponent(runId)}/fork`,
      {
        method: "POST",
        headers: commandHeaders(),
        body: JSON.stringify({
          from_entry_id: data.get("from_entry_id"),
          branch_id: data.get("branch_id"),
        }),
      },
    );
    $("#fork-dialog").close();
    await loadConversations();
    showNotice(
      `会话分支已创建：${result.active_branch_id || data.get("branch_id")}`,
    );
  } catch (error) {
    showNotice(error.message, true);
  }
}

async function loadSystemSettings() {
  const epoch = state.authEpoch;
  const result = await api("/api/system");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  TracePresentation.renderSystem($("#system-view"), result);
}

async function submitProvider(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#provider-dialog").close();
  const form = event.currentTarget;
  const data = new FormData(form);
  const payload = Object.fromEntries(data.entries());
  payload.timeout_seconds = Number(payload.timeout_seconds);
  payload.max_context_tokens = Number(payload.max_context_tokens);
  payload.max_output_tokens = Number(payload.max_output_tokens);
  payload.thinking_budget_tokens = Number(payload.thinking_budget_tokens);
  payload.clear_api_key = data.get("clear_api_key") === "on";
  try {
    await api("/api/providers", {
      method: "POST",
      headers: commandHeaders(),
      body: JSON.stringify(payload),
    });
    $("#provider-dialog").close();
    form.reset();
    await loadProviders();
    showNotice("Provider 已保存");
  } catch (error) {
    showNotice(error.message, true);
  }
}

async function submitMcp(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#mcp-dialog").close();
  const form = event.currentTarget;
  const data = new FormData(form);
  const payload = Object.fromEntries(data.entries());
  payload.args = String(payload.args || "")
    .split(/\r?\n/)
    .map((x) => x.trim())
    .filter(Boolean);
  payload.enabled = data.get("enabled") === "on";
  for (const field of ["env", "headers"]) {
    const raw = String(payload[field] || "").trim();
    if (!raw) {
      delete payload[field];
      continue;
    }
    try {
      payload[field] = JSON.parse(raw);
    } catch (error) {
      showNotice(`${field} JSON 无效`, true);
      return;
    }
  }
  try {
    await api("/api/mcp", {
      method: "POST",
      headers: commandHeaders(),
      body: JSON.stringify(payload),
    });
    $("#mcp-dialog").close();
    form.reset();
    await loadMcp();
    showNotice("MCP 已保存");
  } catch (error) {
    showNotice(error.message, true);
  }
}
