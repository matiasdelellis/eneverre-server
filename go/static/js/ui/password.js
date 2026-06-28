const EYE_OPEN =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8S1 12 1 12z"/><circle cx="12" cy="12" r="3"/></svg>';
const EYE_OFF =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17.94 17.94A10.94 10.94 0 0 1 12 20c-7 0-11-8-11-8a19.83 19.83 0 0 1 4.22-5.39"/><path d="M9.9 4.24A10.94 10.94 0 0 1 12 4c7 0 11 8 11 8a19.86 19.86 0 0 1-3.17 4.19"/><path d="M14.12 14.12a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg>';

function wrapPasswordInput(input) {
  if (!input || input.type !== "password" || input.dataset.pwWrapped) return;
  input.dataset.pwWrapped = "1";

  const wrap = document.createElement("span");
  wrap.className = "pw-wrap";
  input.parentNode.insertBefore(wrap, input);
  wrap.appendChild(input);

  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "pw-toggle";
  btn.setAttribute("aria-label", "Show password");
  btn.setAttribute("aria-pressed", "false");
  btn.title = "Show password";
  btn.innerHTML = EYE_OFF;

  btn.addEventListener("click", () => {
    const showing = input.type === "text";
    input.type = showing ? "password" : "text";
    btn.innerHTML = showing ? EYE_OFF : EYE_OPEN;
    btn.setAttribute("aria-label", showing ? "Show password" : "Hide password");
    btn.setAttribute("aria-pressed", showing ? "false" : "true");
    btn.title = showing ? "Show password" : "Hide password";
  });

  wrap.appendChild(btn);
}

function initPasswordToggles(root = document) {
  root.querySelectorAll('input[type="password"]:not([data-pw-wrapped])')
    .forEach(wrapPasswordInput);
}

export function initPasswordReveal() {
  initPasswordToggles();
  new MutationObserver((muts) => {
    for (const m of muts) {
      m.addedNodes.forEach((n) => {
        if (n.nodeType !== 1) return;
        if (n.matches && n.matches('input[type="password"]')) wrapPasswordInput(n);
        initPasswordToggles(n);
      });
    }
  }).observe(document.documentElement, { childList: true, subtree: true });
}
