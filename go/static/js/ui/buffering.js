// Shared "Loading…" overlay for wall tiles. Live (mse.js) and Playback
// (playback.js) used to each build their own — same .wall-buffering CSS but
// a different glyph (icon("loader") vs a literal "⟳") and untranslated text
// on the playback side. Both now come here so the two views can't drift.
import { icon } from "./icons.js";
import { t } from "../i18n.js";

// ensureTileBuffering returns the tile's buffering overlay, creating it (in
// the "loading" state, inserted under .wall-overlay so the name/actions stay
// on top) when missing. Null tile → null.
export function ensureTileBuffering(tile) {
  if (!tile) return null;
  let el = tile.querySelector(".wall-buffering");
  if (!el) {
    el = document.createElement("div");
    el.className = "wall-buffering";
    el.setAttribute("role", "status");
    el.setAttribute("aria-live", "polite");
    el.innerHTML = `<span class="spin-icon wall-buffering-icon" aria-hidden="true">${icon("loader")}</span><span class="wall-buffering-text">${t("loading")}</span>`;
    const overlay = tile.querySelector(".wall-overlay");
    if (overlay) tile.insertBefore(el, overlay);
    else tile.appendChild(el);
  }
  return el;
}

// removeTileBuffering removes the overlay if present. Safe on null tiles.
export function removeTileBuffering(tile) {
  const el = tile && tile.querySelector(".wall-buffering");
  if (el) el.remove();
}

// loadingStatus builds a full-panel "Loading…" message that spins the same
// loader glyph as the per-tile overlay. Used for wall-level states (e.g.
// fetching the recording list) that replace the whole grid, so they don't
// drift into a plain static line while tiles get an animated spinner.
export function loadingStatus(text) {
  const p = document.createElement("p");
  p.className = "wall-status wall-loading-status";
  p.setAttribute("role", "status");
  p.setAttribute("aria-live", "polite");
  p.innerHTML = `<span class="spin-icon spinner-icon" aria-hidden="true">${icon("loader")}</span><span>${text}</span>`;
  return p;
}
