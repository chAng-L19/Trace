"use strict";

const THEME_KEY = "trace-theme";

function makeIcon(name) {
  const icon = lucide.createElement(lucide.icons[name]);
  icon.classList.add("lucide");
  icon.setAttribute("aria-hidden", "true");
  icon.setAttribute("focusable", "false");
  return icon;
}

function setTheme(theme) {
  const dark = theme === "dark";
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  document.querySelector('meta[name="theme-color"]').content = dark ? "#0E0E0E" : "#F7F7F5";
  document.querySelector('meta[name="color-scheme"]').content = dark ? "dark" : "light";
  document.querySelectorAll("[data-theme-toggle]").forEach((button) => {
    const label = dark ? "切换浅色模式" : "切换深色模式";
    button.setAttribute("aria-label", label);
    button.title = label;
    button.replaceChildren(makeIcon(dark ? "Sun" : "Moon"));
  });
}

lucide.createIcons({ attrs: { "aria-hidden": "true", focusable: "false" } });
setTheme(localStorage.getItem(THEME_KEY) || "light");
document.querySelectorAll("[data-theme-toggle]").forEach((button) => {
  button.addEventListener("click", () => {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    localStorage.setItem(THEME_KEY, next);
    setTheme(next);
  });
});

const terminalToggle = document.querySelector("#terminal-toggle");
terminalToggle.addEventListener("click", () => {
  const expanded = terminalToggle.getAttribute("aria-expanded") === "true";
  terminalToggle.setAttribute("aria-expanded", String(!expanded));
  document.querySelector("#terminal-body").hidden = expanded;
  document.querySelector("#terminal-state").textContent = expanded ? "展开" : "收起";
  terminalToggle.querySelector(".terminal-state .lucide").replaceWith(makeIcon(expanded ? "ChevronUp" : "ChevronDown"));
});

document.querySelectorAll(".icon-button[aria-label='关闭']").forEach((button) => {
  button.replaceChildren(makeIcon("X"));
  button.title = "关闭";
});
const settingIcons = {
  编辑: "Pencil", 删除: "Trash2", 移除: "Trash2", 配置: "Settings2",
  打开: "ArrowUpRight", 分支: "GitBranch", 切换: "ArrowRightLeft",
  配置接入: "Plug", 查看工具: "Wrench",
};
function decorateSettingActions() {
  document.querySelectorAll(".setting-row button:not([data-themed]), #provider-list button:not([data-themed])").forEach((button) => {
    const label = button.textContent.trim();
    const icon = settingIcons[label];
    if (!icon) return;
    button.dataset.themed = "true";
    button.title = label;
    button.setAttribute("aria-label", label);
    if (["编辑", "删除", "移除", "配置"].includes(label)) {
      button.classList.add("icon-only");
      button.replaceChildren(makeIcon(icon));
    } else {
      button.prepend(makeIcon(icon));
    }
  });
}
const settingsObserver = new MutationObserver(decorateSettingActions);
document.querySelectorAll(".settings-list, #provider-list").forEach((list) => {
  settingsObserver.observe(list, { childList: true, subtree: true });
});
decorateSettingActions();
