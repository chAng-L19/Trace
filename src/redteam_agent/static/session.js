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
  $("#gate-state").textContent = "GATE_STATE: SESSION_EXPIRED";
  $("#logout").hidden = true;
  const dialog = $("#login-dialog");
  if (!dialog.open) dialog.showModal();
  showNotice("会话已过期，请重新登录", true);
}

async function loadAuth() {
  const result = await api("/api/auth/status");
  state.authRequired = Boolean(result.required);
  state.authenticated = Boolean(result.authenticated || !result.required);
  applyUser(result.user);
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
  const data = new FormData(form);
  const username = String(data.get("username") || "");
  const password = String(data.get("password") || "");
  const errorNode = $("#login-error");
  const submit = $("#login-submit");
  const label = $("#login-label");
  errorNode.hidden = true;
  submit.disabled = true;
  label.textContent = "VERIFYING CREDENTIALS...";
  form.classList.add("is-verifying");
  $("#gate-state").textContent = "GATE_STATE: VERIFYING";
  try {
    const result = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    });
    form.classList.add("is-authenticated");
    $("#gate-state").textContent = "GATE_STATE: AUTHENTICATED";
    await form.animate([{ opacity: 1 }, { opacity: 0, transform: "scale(.98)" }], {
      duration: matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 150,
      easing: "ease",
    }).finished;
    $("#login-dialog").close();
    form.reset();
    state.authenticated = true;
    state.authRequired = true;
    applyUser(result.user);
    state.authEpoch += 1;
    setAuthLocked(false);
    setView("runs");
    $("#logout").hidden = false;
    await Promise.all([loadSystem(), loadRuns({ keepSelection: false })]);
    showNotice("已登录 Trace");
  } catch (error) {
    errorNode.textContent = error.message === "invalid_credentials"
      ? "用户名或密码不正确，请重试。"
      : "登录未完成，请检查服务连接后重试。";
    errorNode.hidden = false;
    $("#gate-state").textContent = "GATE_STATE: ACCESS_DENIED";
  } finally {
    submit.disabled = false;
    label.textContent = "确认接入终端";
    form.classList.remove("is-verifying", "is-authenticated");
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
    $("#gate-state").textContent = "GATE_STATE: STANDBY";
    $("#logout").hidden = true;
    const dialog = $("#login-dialog");
    if (!dialog.open) dialog.showModal();
  }
}

function clearSessionView() {
  document.querySelectorAll("dialog[open]").forEach((dialog) => dialog.close());
  applyUser(null);
  $("#user-list").replaceChildren();
  $("#profile-form").reset();
  $("#password-form").reset();
  $("#user-form").reset();
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
