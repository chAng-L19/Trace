"use strict";

lucide.createIcons({ attrs: { "aria-hidden": "true", focusable: "false" } });
function makeIcon(name) {
  const icon = lucide.createElement(lucide.icons[name]);
  icon.classList.add("lucide");
  icon.setAttribute("aria-hidden", "true");
  icon.setAttribute("focusable", "false");
  return icon;
}
document.querySelectorAll(".icon-button[aria-label='关闭']").forEach((button) => {
  button.replaceChildren(makeIcon("X"));
  button.title = "关闭";
});

const settingIcons = { 编辑: "Pencil", 移除: "Trash2", 打开: "ArrowUpRight", 切换: "ArrowRightLeft", 配置接入: "Plug" };
function decorateSettingActions() {
  document.querySelectorAll(".setting-row button:not([data-themed])").forEach((button) => {
    const label = button.textContent.trim();
    const icon = settingIcons[label];
    if (!icon) return;
    button.dataset.themed = "true";
    button.title = label;
    button.setAttribute("aria-label", label);
    const image = makeIcon(icon);
    if (["编辑", "移除"].includes(label)) {
      button.classList.add("icon-only");
      button.replaceChildren(image);
    } else {
      button.prepend(image);
    }
  });
}
const settingsObserver = new MutationObserver(decorateSettingActions);
document.querySelectorAll(".settings-list").forEach((list) => {
  settingsObserver.observe(list, { childList: true, subtree: true });
});
decorateSettingActions();
