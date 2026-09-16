"use strict";

// Authentication and session lifecycle.
function setAuthLocked(locked) {
  document.body.classList.toggle("auth-locked", Boolean(locked));
  document.documentElement.dataset.traceAuth = locked ? "required" : "ready";
}

function handleAuthExpired() {
  if (!state.authRequired || !state.authenticated) return;
  state.authEpoch += 1;
  clearSessionView();
  state.authenticated = false;
  setAuthLocked(true);
  $("#logout").hidden = true;
  const dialog = $("#login-dialog");
  if (!dialog.open) dialog.showModal();
  showNotice("会话已过期，请重新登录", true);
}

async function loadAuth() {
  const result = await api("/api/auth/status");
  state.authRequired = Boolean(result.required);
  state.authenticated = Boolean(result.authenticated || !result.required);
  $("#logout").hidden = !state.authRequired || !state.authenticated;
  if (result.required && !state.authenticated) {
    setAuthLocked(true);
    const dialog = $("#login-dialog");
    if (!dialog.open) dialog.showModal();
    return false;
  }
  setAuthLocked(false);
  return true;
}

async function login(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const password = String(new FormData(form).get("password") || "");
  const errorNode = $("#login-error");
  const submit = $("#login-submit");
  errorNode.hidden = true;
  submit.disabled = true;
  submit.textContent = "正在进入";
  try {
    await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    $("#login-dialog").close();
    form.reset();
    state.authenticated = true;
    state.authRequired = true;
    state.authEpoch += 1;
    setAuthLocked(false);
    $("#logout").hidden = false;
    await Promise.all([loadSystem(), loadRuns({ keepSelection: false })]);
    showNotice("已登录 Trace");
  } catch (error) {
    errorNode.textContent = error.message === "invalid_credentials"
      ? "密码不正确，请重试。"
      : "登录未完成，请检查服务连接后重试。";
    errorNode.hidden = false;
  } finally {
    submit.disabled = false;
    submit.textContent = "进入工作台";
  }
}

async function logout() {
  try {
    await api("/api/auth/logout", { method: "POST", body: "{}" });
  } finally {
    clearSessionView();
    state.authenticated = false;
    state.authRequired = true;
    state.authEpoch += 1;
    setView("runs");
    setAuthLocked(true);
    $("#logout").hidden = true;
    const dialog = $("#login-dialog");
    if (!dialog.open) dialog.showModal();
  }
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
