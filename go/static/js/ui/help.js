import { $ } from "../util/dom.js";

// Keyboard-shortcut help overlay plus the global `f` (fullscreen) shortcut.
// Opened with `?`, closed with Esc, the close button, or a backdrop click.

function isTyping(t) {
  return t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable);
}

function openHelp() {
  const ov = $("#help-overlay");
  if (ov) ov.hidden = false;
}

function closeHelp() {
  const ov = $("#help-overlay");
  if (ov) ov.hidden = true;
}

// The tile the `f` shortcut acts on: whatever the pointer is over, or the
// only tile when a single camera is shown.
function focusedTile() {
  const hovered = document.querySelector("#wall .wall-tile:hover");
  if (hovered) return hovered;
  const tiles = document.querySelectorAll("#wall .wall-tile");
  return tiles.length === 1 ? tiles[0] : null;
}

export function initHelp() {
  const ov = $("#help-overlay");
  $("#help-close")?.addEventListener("click", closeHelp);
  ov?.addEventListener("click", (e) => { if (e.target === ov) closeHelp(); });

  document.addEventListener("keydown", (e) => {
    if (isTyping(e.target)) return;
    if (e.key === "Escape") {
      if (ov && !ov.hidden) { closeHelp(); e.preventDefault(); }
      return;
    }
    if (e.key === "?") {
      if (ov) { ov.hidden = !ov.hidden; e.preventDefault(); }
      return;
    }
    if (e.key === "f" || e.key === "F") {
      if (document.fullscreenElement) { document.exitFullscreen?.(); e.preventDefault(); return; }
      const tile = focusedTile();
      if (tile) { tile.requestFullscreen?.(); e.preventDefault(); }
    }
  });
}
