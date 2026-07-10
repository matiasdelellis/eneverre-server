// Lightweight, dependency-free toast notifications. A single container is
// lazily created and appended to <body>; each toast auto-dismisses. Used for
// non-blocking feedback (clip saved, snapshot downloaded, PTZ errors, …).

let container = null;

function ensureContainer() {
  if (container) return container;
  container = document.createElement("div");
  container.className = "toast-container";
  container.setAttribute("aria-live", "polite");
  container.setAttribute("role", "status");
  document.body.appendChild(container);
  return container;
}

// toast(message, { type, duration }) — type is "info" (default), "success",
// or "error"; duration is milliseconds before auto-dismiss (default 3000).
export function toast(message, { type = "info", duration = 3000 } = {}) {
  const root = ensureContainer();
  const el = document.createElement("div");
  el.className = `toast toast-${type}`;
  const icon = type === "success" ? "✓" : type === "error" ? "✕" : "ℹ";
  el.innerHTML = `<span class="toast-icon" aria-hidden="true">${icon}</span><span class="toast-text"></span>`;
  el.querySelector(".toast-text").textContent = message;
  root.appendChild(el);
  // Force a reflow so the entry transition runs from the initial state.
  requestAnimationFrame(() => el.classList.add("toast-show"));
  const remove = () => {
    el.classList.remove("toast-show");
    el.addEventListener("transitionend", () => el.remove(), { once: true });
    // Fallback in case the transition never fires (e.g. reduced motion).
    setTimeout(() => el.remove(), 400);
  };
  setTimeout(remove, duration);
  return remove;
}
