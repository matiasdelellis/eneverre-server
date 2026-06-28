import { get, set, remove, THEME_KEY } from "../util/storage.js";

export function currentTheme() {
  return get(THEME_KEY);
}

export function applyTheme(theme) {
  if (theme === "light" || theme === "dark") {
    document.documentElement.dataset.theme = theme;
  } else {
    delete document.documentElement.dataset.theme;
  }
  const btn = document.getElementById("theme-toggle");
  if (btn) btn.textContent = theme === "light" ? "🌙" : "☀";
}

export function setTheme(theme) {
  if (theme === "light" || theme === "dark") set(THEME_KEY, theme);
  else remove(THEME_KEY);
  applyTheme(theme);
}

export function toggleTheme() {
  const next = currentTheme() === "light" ? "dark" : "light";
  setTheme(next);
}

export function initTheme() {
  applyTheme(currentTheme());
  document.getElementById("theme-toggle")?.addEventListener("click", toggleTheme);
  if (window.matchMedia) {
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const onSystemChange = (e) => {
      if (!currentTheme()) applyTheme(e.matches ? "light" : "dark");
    };
    if (mq.addEventListener) mq.addEventListener("change", onSystemChange);
    else if (mq.addListener) mq.addListener(onSystemChange);
  }
}
