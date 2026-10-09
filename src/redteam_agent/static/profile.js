"use strict";

function applyUser(user) {
  state.user = user || null;
  const admin = user?.role === "admin";
  document.querySelectorAll("[data-admin-only]").forEach((node) => { node.hidden = !admin; });
  $("#current-user").textContent = user?.display_name || user?.username || "";
  $("#current-user").title = user?.username || "";
  $("#profile-role").textContent = admin ? "管理员" : "成员";
  const form = $("#profile-form");
  form.elements.username.value = user?.username || "";
  form.elements.display_name.value = user?.display_name || "";
}

async function loadProfile() {
  const epoch = state.authEpoch;
  const result = await api("/api/auth/profile");
  if (epoch === state.authEpoch && state.authenticated) applyUser(result.user);
}

async function loadUsers() {
  if (state.user?.role !== "admin") return;
  const epoch = state.authEpoch;
  const result = await api("/api/auth/users");
  if (epoch !== state.authEpoch || !state.authenticated) return;
  renderSettingsList($("#user-list"), result.users || [], (user) => {
    const row = document.createElement("article");
    row.className = "setting-row";
    const main = document.createElement("div");
    main.className = "setting-main";
    const name = document.createElement("strong");
    name.textContent = user.display_name;
    const username = document.createElement("code");
    username.textContent = user.username;
    const role = document.createElement("span");
    role.className = "status-badge";
    role.textContent = user.role === "admin" ? "管理员" : "成员";
    main.append(name, username);
    row.append(main, role);
    return row;
  });
}

async function saveAccount(event) {
  event.preventDefault();
  const epoch = state.authEpoch;
  const form = event.currentTarget;
  const data = Object.fromEntries(new FormData(form));
  const passwordChange = form.id === "password-form";
  if (passwordChange && data.password !== data.password_confirm) {
    showNotice("两次输入的新密码不一致", true);
    return;
  }
  delete data.password_confirm;
  const submit = form.querySelector('button[type="submit"]');
  submit.disabled = true;
  try {
    const result = await api("/api/auth/profile", { method: "POST", body: JSON.stringify(data) });
    if (epoch !== state.authEpoch || !state.authenticated) return;
    form.reset();
    applyUser(result.user);
    showNotice(passwordChange ? "密码已更新" : "个人资料已保存");
  } catch (error) {
    if (epoch !== state.authEpoch) return;
    showNotice(error.message, true);
  } finally {
    submit.disabled = false;
  }
}

async function submitUser(event) {
  event.preventDefault();
  const epoch = state.authEpoch;
  const form = event.currentTarget;
  if (event.submitter?.value === "cancel") return $("#user-dialog").close();
  const submit = form.querySelector('button[value="default"]');
  submit.disabled = true;
  try {
    await api("/api/auth/users", { method: "POST", body: JSON.stringify(Object.fromEntries(new FormData(form))) });
    if (epoch !== state.authEpoch || !state.authenticated) return;
    $("#user-dialog").close();
    form.reset();
    await loadUsers();
    showNotice("用户已创建");
  } catch (error) {
    if (epoch !== state.authEpoch) return;
    showNotice(error.message, true);
  } finally {
    submit.disabled = false;
  }
}
