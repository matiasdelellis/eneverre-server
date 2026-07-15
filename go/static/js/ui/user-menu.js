import { $ } from "../util/dom.js";
import { currentTheme, toggleTheme } from "./theme.js";

function syncThemeMenuLabel() {
  const menu = document.getElementById("theme-toggle-menu");
  if (!menu) return;
  // The topbar shows the moon in light mode (so clicking goes to dark)
  // and the sun in dark/auto (so clicking goes to light).
  const isLight = currentTheme() === "light";
  menu.textContent = isLight ? "Switch to dark" : "Switch to light";
}

function close() {
  const list = document.getElementById("user-menu-list");
  if (!list || list.hidden) return;
  list.hidden = true;
  const trigger = document.getElementById("user-menu-trigger");
  if (trigger) trigger.setAttribute("aria-expanded", "false");
}

function toggle() {
  const list = document.getElementById("user-menu-list");
  if (!list) return;
  syncThemeMenuLabel();
  const willOpen = list.hidden;
  list.hidden = !willOpen;
  const trigger = document.getElementById("user-menu-trigger");
  if (trigger) trigger.setAttribute("aria-expanded", willOpen ? "true" : "false");
}

export function closeUserMenu() { close(); }

export function refreshUserMenu(user) {
  const nameEl = document.getElementById("user-menu-name");
  if (!nameEl) return;
  nameEl.textContent = user ? (user.displayName || user.username) : "";
  const usersBtn = document.getElementById("users-btn");
  if (usersBtn) usersBtn.hidden = !(user && user.is_admin);
  const camerasBtn = document.getElementById("cameras-btn");
  if (camerasBtn) camerasBtn.hidden = !(user && user.is_admin);
}

export function initUserMenu() {
  document.getElementById("user-menu-trigger")?.addEventListener("click", (e) => {
    e.stopPropagation();
    toggle();
  });
  // The theme menu item duplicates the topbar button (visible only on
  // mobile, where the topbar runs out of room). It forwards the same
  // action and closes the menu after the click.
  document.getElementById("theme-toggle-menu")?.addEventListener("click", () => {
    toggleTheme();
    close();
  });
  document.addEventListener("click", (e) => {
    const menu = document.getElementById("user-menu");
    if (!menu || menu.hidden) return;
    if (!menu.contains(e.target)) close();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") close();
  });
}
