import { get, set, remove, THEME_KEY } from "../util/storage.js";
import { icon } from "./icons.js";

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
  if (btn) btn.innerHTML = effectiveTheme() === "light" ? icon("moon") : icon("sun");
  // Components that render theme-dependent colors directly to a canvas
  // (or any non-CSS surface) can't pick up the change via a stylesheet
  // re-render — they have to re-read the custom properties and redraw.
  // The playback timeline is the main consumer.
  document.dispatchEvent(new CustomEvent("eneverre:themechange"));
}

export function setTheme(theme) {
  if (theme === "light" || theme === "dark") set(THEME_KEY, theme);
  else remove(THEME_KEY);
  applyTheme(theme);
}

// The effective theme is what's actually rendered right now: an explicit
// stored choice, or — on first session, when nothing is stored — whatever
// the system preference is currently resolving to.
export function effectiveTheme() {
  const stored = currentTheme();
  if (stored === "light" || stored === "dark") return stored;
  return window.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

export function toggleTheme() {
  const next = effectiveTheme() === "light" ? "dark" : "light";
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
