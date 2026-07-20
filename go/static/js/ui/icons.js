// Inline SVG icon set in Lucide/Feather style: 24x24 viewBox, 2px stroke,
// currentColor for theming. All icons are returned as SVG strings by the
// icon() helper so they can be dropped into template literals (innerHTML)
// and updated in place with btn.innerHTML = icon("...") without depending
// on a build step or external asset.
//
// Path data is original to this project (geometric primitives, MIT-licensed).
// Each entry is the inner markup of an <svg viewBox="0 0 24 24"> element —
// the helper applies the standard stroke/fill/linecap/linejoin attributes
// and adds aria-hidden by default so screen readers skip the icon (the
// button's own title/aria-label conveys the action).

const STROKE = 'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"';

const PATHS = {
  // Navigation
  menu:           '<line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="18" x2="21" y2="18"/>',
  x:              '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  "chevron-left": '<path d="m15 18-6-6 6-6"/>',
  "chevron-down": '<path d="m6 9 6 6 6-6"/>',
  "arrow-left":   '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
  "arrow-up":     '<path d="m5 12 7-7 7 7"/><path d="M12 19V5"/>',
  "arrow-right":  '<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/>',
  "arrow-down":   '<path d="M12 5v14"/><path d="m19 12-7 7-7-7"/>',
  "grip-vertical":'<circle cx="9" cy="5"  r="1"/><circle cx="9" cy="12" r="1"/><circle cx="9" cy="19" r="1"/><circle cx="15" cy="5"  r="1"/><circle cx="15" cy="12" r="1"/><circle cx="15" cy="19" r="1"/>',
  "layout-grid":  '<rect x="3"  y="3"  width="7" height="7" rx="1"/><rect x="14" y="3"  width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3"  y="14" width="7" height="7" rx="1"/>',

  // Media
  play:           '<polygon points="6 3 20 12 6 21 6 3" fill="currentColor" stroke="none"/>',
  pause:          '<rect x="6"  y="4" width="4" height="16" rx="1" fill="currentColor" stroke="none"/><rect x="14" y="4" width="4" height="16" rx="1" fill="currentColor" stroke="none"/>',
  camera:         '<path d="M14.5 4h-5L7 7H4a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V9a2 2 0 0 0-2-2h-3l-2.5-3z"/><circle cx="12" cy="13" r="3"/>',
  "camera-off":   '<line x1="2" x2="22" y1="2" y2="22"/><path d="M7 7H4a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h16"/><path d="M9.5 4h5L17 7h3a2 2 0 0 1 2 2v7.5"/><path d="M14.12 15.12A3 3 0 1 1 9.88 10.88"/>',
  "volume-x":     '<polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><line x1="22" x2="16" y1="9" y2="15"/><line x1="16" x2="22" y1="9" y2="15"/>',
  "volume-2":     '<polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><path d="M15.54 8.46a5 5 0 0 1 0 7.07"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14"/>',
  mic:            '<rect x="9" y="2" width="6" height="12" rx="3"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/>',
  "mic-off":      '<line x1="2" x2="22" y1="2" y2="22"/><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"/><path d="M5 10v2a7 7 0 0 0 12 5"/><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"/><path d="M9 9v3a3 3 0 0 0 5.12 2.12"/><line x1="12" x2="12" y1="19" y2="22"/>',
  "maximize":     '<path d="M8 3H5a2 2 0 0 0-2 2v3"/><path d="M21 8V5a2 2 0 0 0-2-2h-3"/><path d="M3 16v3a2 2 0 0 0 2 2h3"/><path d="M16 21h3a2 2 0 0 0 2-2v-3"/>',
  // PTZ / move: four chevrons pointing outward with crosshair lines, the
  // standard NVR "open pan-tilt-zoom controls" glyph.
  move:           '<path d="M5 9l-3 3 3 3"/><path d="M9 5l3-3 3 3"/><path d="M15 19l-3 3-3-3"/><path d="M19 9l3 3-3 3"/><line x1="2" x2="22" y1="12" y2="12"/><line x1="12" x2="12" y1="2" y2="22"/>',
  save:           '<path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/><polyline points="7 3 7 8 15 8"/>',
  share:          '<circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><line x1="8.59" x2="15.42" y1="13.51" y2="17.49"/><line x1="15.41" x2="8.59" y1="6.51" y2="10.49"/>',
  tv:             '<rect x="2" y="7" width="20" height="15" rx="2"/><polyline points="17 2 12 7 7 2"/>',
  smartphone:     '<rect x="6" y="2" width="12" height="20" rx="2"/><line x1="12" x2="12" y1="18" y2="18.01"/>',
  clock:          '<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>',

  // State
  lock:           '<rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
  "lock-open":    '<rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 9.9-1"/>',
  check:          '<polyline points="20 6 9 17 4 12"/>',
  "x-circle":     '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/>',
  "circle-dot":   '<circle cx="12" cy="12" r="10" fill="currentColor" stroke="none"/>',
  info:           '<circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="16" y2="12"/><line x1="12" x2="12.01" y1="8" y2="8"/>',
  "alert-triangle":'<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" x2="12" y1="9" y2="13"/><line x1="12" x2="12.01" y1="17" y2="17"/>',
  "hard-drive":   '<line x1="22" x2="2" y1="12" y2="12"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/><line x1="6" x2="6.01" y1="16" y2="16"/><line x1="10" x2="10.01" y1="16" y2="16"/>',
  // "No signal" / "no data here" — used in the playback gap overlay to
  // signal a stretch with no recorded footage.
  "signal-off":   '<path d="M2 20h20"/><path d="m2 16 4-4"/><path d="m6 12 4-4"/><path d="m10 8 4-4"/><path d="m14 4 4-4"/><line x1="2" x2="22" y1="20" y2="4"/>',
  loader:         '<line x1="12" x2="12"     y1="2"  y2="4"/><line x1="12" x2="12"     y1="20" y2="22"/><line x1="4.93"  x2="6.34"  y1="4.93"  y2="6.34"/><line x1="17.66" x2="19.07" y1="17.66" y2="19.07"/><line x1="2"  x2="4"  y1="12" y2="12"/><line x1="20" x2="22" y1="12" y2="12"/><line x1="4.93"  x2="6.34"  y1="19.07" y2="17.66"/><line x1="17.66" x2="19.07" y1="6.34"  y2="4.93"/>',
  moon:           '<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>',
  sun:            '<circle cx="12" cy="12" r="4"/><line x1="12" y1="2"  x2="12" y2="4"/><line x1="12" y1="20" x2="12" y2="22"/><line x1="4.93"  y1="4.93"  x2="6.34"  y2="6.34"/><line x1="17.66" y1="17.66" x2="19.07" y2="19.07"/><line x1="2"  y1="12" x2="4"  y2="12"/><line x1="20" y1="12" x2="22" y2="12"/><line x1="4.93"  y1="19.07" x2="6.34"  y2="17.66"/><line x1="17.66" y1="6.34"  x2="19.07" y2="4.93"/>',
  circle:         '<circle cx="12" cy="12" r="1" fill="currentColor" stroke="none"/>',
  home:           '<path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V20a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9.5"/><path d="M9.5 21v-6h5v6"/>',
};

/**
 * icon(name, attrs?) returns an <svg> string. The default attributes
 * render the icon at the parent's font-size (1em) and hide it from
 * assistive tech (the surrounding button carries the label). Pass
 * `class` to address it from CSS, `width`/`height` to override, or
 * `aria-hidden="false"` plus an aria-label to make it the label.
 */
export function icon(name, attrs = {}) {
  const path = PATHS[name];
  if (!path) throw new Error(`Unknown icon: ${name}`);
  const merged = {
    viewBox: "0 0 24 24",
    width: "1em",
    height: "1em",
    "aria-hidden": "true",
    ...attrs,
  };
  const attrStr = Object.entries(merged)
    .map(([k, v]) => `${k}="${v}"`)
    .join(" ");
  return `<svg ${attrStr} ${STROKE}>${path}</svg>`;
}

/**
 * hydrateIcons(root?) fills every `[data-icon="name"]` element in `root`
 * with its SVG, keeping index.html as the single source of layout while
 * icons.js stays the single source of the glyph paths (no drift, no
 * repeated stroke attributes in markup). The SVG is prepended, so buttons
 * with a text label (e.g. "← Back to cameras") keep their text after the
 * icon. Runs once at boot; dynamic buttons overwrite their glyph later
 * with `el.innerHTML = icon(...)`. Idempotent — skips already-hydrated
 * elements so a second call is harmless.
 */
export function hydrateIcons(root = document) {
  for (const el of root.querySelectorAll("[data-icon]")) {
    if (el.firstElementChild?.tagName === "svg") continue;
    el.insertAdjacentHTML("afterbegin", icon(el.dataset.icon));
  }
}
