import { $ } from "../util/dom.js";
import { toggleTheme } from "./theme.js";

function syncAudioMenuLabel() {
  // The menu item reuses the same audio-toggle handler as the topbar
  // button, so the label just needs to reflect the current state. The
  // topbar button shows a glyph (🔇/🔊); the menu shows a text label.
  const topbar = document.getElementById("audio-toggle");
  const menu = document.getElementById("audio-toggle-menu");
  if (!topbar || !menu) return;
  const muted = topbar.textContent.trim() === "🔇";
  menu.textContent = muted ? "Unmute all" : "Mute all";
}

function syncThemeMenuLabel() {
  const topbar = document.getElementById("theme-toggle");
  const menu = document.getElementById("theme-toggle-menu");
  if (!topbar || !menu) return;
  // The topbar shows ☀ in light mode, 🌓 in dark/auto.
  const isLight = topbar.textContent.trim() === "🌙";
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
  syncAudioMenuLabel();
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
  // The audio/theme menu items are duplicates of the topbar buttons
  // (visible only on mobile, where the topbar runs out of room). They
  // forward the same actions and close the menu after the click.
  document.getElementById("audio-toggle-menu")?.addEventListener("click", () => {
    document.getElementById("audio-toggle")?.click();
    close();
  });
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
