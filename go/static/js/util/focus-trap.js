// Focus management for modal dialogs. trapFocus keeps Tab within `container`
// while it is open and, on release, returns focus to whatever was focused
// before it opened — so keyboard and screen-reader users can't tab out into the
// (still-present, non-inert) background and land back where they were on close.
//
// Usage: const release = trapFocus(cardEl);  // on open
//        release();                          // on close

const FOCUSABLE = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(",");

// Visible, enabled, focusable descendants in DOM order. getClientRects() is
// empty for elements hidden via display:none or a [hidden] ancestor, which is
// how the dialog hides its optional inputs — so those are correctly skipped.
function focusables(container) {
  return Array.from(container.querySelectorAll(FOCUSABLE))
    .filter((el) => !el.hasAttribute("disabled") && el.getClientRects().length > 0);
}

export function trapFocus(container) {
  const previouslyFocused = document.activeElement;

  const onKeydown = (e) => {
    if (e.key !== "Tab") return;
    const els = focusables(container);
    if (!els.length) return;
    const first = els[0];
    const last = els[els.length - 1];
    // Wrap at both ends; also pull focus in if it somehow escaped the container.
    if (e.shiftKey) {
      if (document.activeElement === first || !container.contains(document.activeElement)) {
        e.preventDefault();
        last.focus();
      }
    } else if (document.activeElement === last || !container.contains(document.activeElement)) {
      e.preventDefault();
      first.focus();
    }
  };

  container.addEventListener("keydown", onKeydown);

  return function release() {
    container.removeEventListener("keydown", onKeydown);
    if (previouslyFocused && typeof previouslyFocused.focus === "function") {
      try { previouslyFocused.focus(); } catch {}
    }
  };
}
