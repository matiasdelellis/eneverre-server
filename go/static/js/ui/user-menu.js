import { $ } from "../util/dom.js";
import { currentTheme, toggleTheme } from "./theme.js";
import { getSupportedLangs, getLang, setLang, langName, t } from "../i18n.js";

// Build one radio-style menu item per supported language, inserted right
// after the language heading. Called once at init; the active marker is
// refreshed on each open via syncLangItems().
function buildLangItems() {
  const heading = document.getElementById("user-menu-lang-heading");
  if (!heading) return;
  let anchor = heading; // each new item is inserted after the previous one
  for (const code of getSupportedLangs()) {
    const li = document.createElement("li");
    li.setAttribute("role", "none");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "user-menu-item";
    btn.setAttribute("role", "menuitemradio");
    btn.dataset.lang = code;
    btn.textContent = langName(code);
    btn.addEventListener("click", () => {
      setLang(code);
      syncLangItems();
      close();
    });
    li.appendChild(btn);
    anchor.after(li);
    anchor = li;
  }
  syncLangItems();
}

function syncLangItems() {
  const active = getLang();
  document.querySelectorAll("#user-menu-list [data-lang]").forEach((btn) => {
    btn.setAttribute("aria-checked", btn.dataset.lang === active ? "true" : "false");
  });
}

function syncThemeMenuLabel() {
  const menu = document.getElementById("theme-toggle-menu");
  if (!menu) return;
  // The topbar shows the moon in light mode (so clicking goes to dark)
  // and the sun in dark/auto (so clicking goes to light).
  const isLight = currentTheme() === "light";
  menu.textContent = isLight ? t("menu.theme_dark") : t("menu.theme_light");
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
  syncLangItems();
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
  buildLangItems();
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
