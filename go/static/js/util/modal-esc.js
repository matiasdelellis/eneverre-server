// Universal Escape-to-close for the app's custom modals. Each view registers
// its selector + close function once at init; a single capture-phase listener
// closes the topmost open modal and stops the event so other document-level
// Escape handlers (the wall's filter-walk) don't also act on the same press.
//
// #dlg-modal and #help-overlay are deliberately NOT registered: they own
// their own Escape listeners (dialog.js / help.js), and this handler stands
// aside while either is open so those keep working unchanged.

const handlers = []; // { selector, close }, in registration order

/** Register (or replace) the Escape handler for one modal selector. */
export function registerModalEsc(selector, close) {
  const existing = handlers.find((h) => h.selector === selector);
  if (existing) existing.close = close;
  else handlers.push({ selector, close });
}

// Among the open registered modals, pick the one that paints on top: for
// equal z-index (the common case — schedules over the camera wizard) that is
// the last in DOM order.
function topmostOpen() {
  let best = null;
  for (const h of handlers) {
    const m = document.querySelector(h.selector);
    if (!m || m.hidden) continue;
    if (!best || (best.modal.compareDocumentPosition(m) & Node.DOCUMENT_POSITION_FOLLOWING)) {
      best = { modal: m, close: h.close };
    }
  }
  return best;
}

document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  // Shared dialog / help own Escape — let their bubble-phase listeners run.
  if (document.querySelector("#dlg-modal:not([hidden]), #help-overlay:not([hidden])")) return;
  const top = topmostOpen();
  if (!top) return;
  // Capture phase + stopPropagation: neither the target nor any bubble-phase
  // listener (wall filter-walk, user menu) sees this Escape.
  e.preventDefault();
  e.stopPropagation();
  top.close();
}, true);
