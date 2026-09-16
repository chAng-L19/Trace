"use strict";

// Provider, Skill, MCP and conversation settings.
async function loadControl() {
  const epoch = state.authEpoch;
  try {
    await Promise.all([
      loadProviders(),
      loadSkills(),
      loadMcp(),
      loadConversations(),
      loadSystemSettings(),
    ]);
    if (
      epoch !== state.authEpoch ||
      (state.authRequired && !state.authenticated)
    )
      return;
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

async function loadProviders() {
  const epoch = state.authEpoch;
  $("#provider-list").setAttribute("aria-busy", "true");
  const result = await api("/api/providers");
  if (epoch !== state.authEpoch || (state.authRequired && !state.authenticated))
    return;
  renderSettingsList($("#provider-list"), result.providers || [], (item) => {
    const row = document.createElement("article");
    row.className = `setting-row${item.active ? " is-active" : ""}`;
    const main = document.createElement("div");
    main.className = "setting-main";
    const title = document.createElement("strong");
    title.textContent = `${item.name} · ${item.model}`;
    const meta = document.createElement("code");
    meta.textContent = `${item.base_url} · key ${item.api_key_set ? "已绑定" : "未绑定"}`;
    main.append(title, meta);
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
    row.append(main, actions);
    return row;
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
    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.textContent = item.enabled ? "已启用" : "已停用";
    toggle.className = item.enabled ? "primary" : "quiet";
    toggle.addEventListener("click", async () => {
      await api(`/api/skills/${encodeURIComponent(item.resource_id)}`, {
        method: "POST",
        headers: commandHeaders(),
        body: JSON.stringify({ enabled: !item.enabled }),
      });
      await loadSkills();
    });
    const edit = document.createElement("button");
    edit.type = "button";
    edit.textContent = "配置";
    edit.addEventListener("click", () => openSkillEditor(item));
    const actions = document.createElement("div");
    actions.className = "button-row";
    actions.append(edit, toggle);
    row.append(main, actions);
    return row;
  });
}

function openSkillEditor(item) {
  $("#skill-id").value = item.resource_id || "";
  $("#skill-name").textContent =
    `${item.resource_id || "Skill"} · ${item.byte_count || 0} bytes`;
  $("#skill-config").value = safeJson(item.config || {});
  $("#skill-dialog").showModal();
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
    await api(`/api/skills/${encodeURIComponent(id)}`, {
      method: "POST",
      headers: commandHeaders(),
      body: JSON.stringify({ config }),
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
    title.textContent = item.server_id;
    const meta = document.createElement("code");
    meta.textContent = `${item.transport} · ${item.status?.status || "configured"} · ${item.status?.tool_count || 0} tools`;
    main.append(title, meta);
    const actions = document.createElement("div");
    actions.className = "button-row";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.textContent = "编辑";
    edit.addEventListener("click", () => openMcpEditor(item));
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "danger";
    remove.textContent = "移除";
    remove.addEventListener("click", async () => {
      if (!confirm("确认移除 MCP 服务器？")) return;
      await api(`/api/mcp/${encodeURIComponent(item.server_id)}`, {
        method: "DELETE",
        headers: commandHeaders(),
      });
      await loadMcp();
    });
    actions.append(edit, remove);
    row.append(main, actions);
    return row;
  });
}

function openMcpEditor(item = null) {
  const form = $("#mcp-form");
  form.reset();
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
      meta.textContent = `${item.run?.run?.run_id || "-"} · ${item.message_count || 0} messages · 当前分支 ${activeBranch}`;
      main.append(title, meta);
      const branchHeads = document.createElement("small");
      branchHeads.className = "setting-detail";
      branchHeads.textContent = `分支头：${
        Object.entries(branches)
          .map(([id, leaf]) => `${id}=${leaf || "空"}`)
          .join(" · ") || "main=空"
      }`;
      main.append(branchHeads);
      const actions = document.createElement("div");
      actions.className = "button-row";
      const open = document.createElement("button");
      open.type = "button";
      open.textContent = "打开";
      open.addEventListener("click", () => {
        setView("runs");
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
          option.textContent = branchId;
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
      actions.append(open, fork, checkout);
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
      option.textContent = `#${entry.sequence ?? "?"} · ${entry.entry_type || "entry"} · ${entry.branch_id || "main"}`;
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
  $("#system-view").textContent = safeJson({
    ...result.control_plane,
    provider: result.provider,
    auth: result.auth,
  });
}

async function submitProvider(event) {
  event.preventDefault();
  if (event.submitter?.value === "cancel") return $("#provider-dialog").close();
  const form = event.currentTarget;
  const data = new FormData(form);
  const payload = Object.fromEntries(data.entries());
  payload.timeout_seconds = Number(payload.timeout_seconds);
  payload.max_context_tokens = Number(payload.max_context_tokens);
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
