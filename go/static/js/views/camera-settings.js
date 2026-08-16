// Per-camera live-settings dialog, opened from the topbar "sliders" button
// (next to talk/privacy) for the camera selected in live view. Admin-only:
// both the state source (GET /api/camera/{id}/settings) and the backing
// endpoint (PUT /api/camera/{id}/settings) are admin-gated, so the button
// never shows for non-admins even though the camera advertises the
// capability.
//
// Clicking a toggle PUTs the change and then re-reads the state endpoint —
// the server refreshes the cached camera state on success, so the dialog
// repaints with camera-confirmed values.

import { $, escapeHtml } from "../util/dom.js";
import { getState, on } from "../state.js";
import { fetchCameraSettings, setCameraSettings } from "../api.js";
import { loadJson, USER_KEY } from "../util/storage.js";
import { t } from "../i18n.js";

let currentCam = null;
// uiMode is the selector's client-side position. It starts from the
// snapshot but persists across repaints, because the heartbeat cannot tell
// "manual" apart from a fixed day/night mode (both report enabled=false +
// the current mode) — only the selector knows the operator chose manual.
let uiMode = null;

function isAdmin() {
  return !!loadJson(USER_KEY)?.is_admin;
}

// settingsFields returns the boolean toggle definitions for a camera's
// live-settings snapshot: [field, on, label]. The day/night mode is NOT a
// toggle — it is the 4-way selector rendered by renderModeSelect. Toggles
// are grouped by area: audio, motion, and — visible only when the selector
// sits on Manual — illumination (the IR LEDs/white light/filter are the
// operator's business only once the photosensor is out of the way).
function settingsFields(si) {
  return {
    audio: [
      ["mic_enabled", !!si.mic_enabled, t("status.tg_mic")],
      ["speaker_enabled", !!si.spk_enabled, t("status.tg_spk")],
    ],
    motion: [
      ["motion_enabled", !!si.motion_enabled, t("status.tg_motion")],
    ],
    light: [
      ["ircut", !!si.ircut, t("status.hb_ircut")],
      ["ir850", !!si.ir850, t("status.hb_ir850")],
      ["ir940", !!si.ir940, t("status.hb_ir940")],
      ["white", !!si.white, t("status.hb_white")],
    ],
  };
}

// settingsToggleBody maps a boolean toggle click to the PUT request body.
function settingsToggleBody(field, on) {
  return { [field]: !on };
}

// modeValue derives the selector position from the snapshot: Auto when the
// photosensor is enabled, otherwise the fixed mode. Manual is not
// distinguishable from a fixed mode in the snapshot (both report
// enabled=false + the current mode), so it only exists as an action.
function modeValue(si) {
  if (si.daynight_enabled) return "auto";
  return si.daynight_mode === "night" ? "night" : "day";
}

// modeBody maps a selector choice to the PUT request body: Auto turns the
// photosensor on; Day/Night/Manual all disable it (Manual keeps the sensor's
// current mode, mirroring the firmware's json-imp daynight command).
function modeBody(mode) {
  return mode === "auto"
    ? { daynight_auto: true }
    : { daynight_mode: mode };
}

function renderModeSelect(si) {
  const cur = uiMode || modeValue(si);
  const opts = ["auto", "day", "night", "manual"]
    .map((m) => `<option value="${m}"${m === cur ? " selected" : ""}>${escapeHtml(t("settings.mode_" + m))}</option>`)
    .join("");
  return `
    <label class="settings-mode">
      <span>${escapeHtml(t("settings.mode"))}</span>
      <select class="settings-mode-select">${opts}</select>
    </label>`;
}

function togglesFor(fields) {
  return fields.map(([field, on, label]) => `
    <button type="button" class="settings-toggle${on ? " on" : ""}" data-field="${field}"
      data-on="${on ? 1 : 0}" aria-pressed="${on ? "true" : "false"}">${escapeHtml(label)}</button>`).join("");
}

function section(title, fields) {
  return `
    <div class="settings-section">
      <h3>${escapeHtml(title)}</h3>
      <div class="settings-toggle-grid" role="group">${togglesFor(fields)}</div>
    </div>`;
}

function syncSettingsButton() {
  const btn = $("#settings-toggle");
  if (!btn) return;
  const { viewMode, wallFilter, camerasCache } = getState();
  let cam = null;
  if (viewMode === "live" && wallFilter.type === "cam" && isAdmin()) {
    cam = (camerasCache || []).find((c) => c.id === wallFilter.value) || null;
    if (cam?.capabilities?.settings !== true) cam = null;
  }
  currentCam = cam;
  btn.hidden = !cam;
  // A selection change that drops the capability (or admin leaving the view)
  // closes the dialog so it never shows a camera it can't control.
  const modal = $("#settings-modal");
  if (modal && !modal.hidden && !cam) closeSettings();
}

function renderToggles(si) {
  const body = $("#settings-modal-body");
  if (!si) {
    body.innerHTML = `<p class="muted">${escapeHtml(t("settings.unavailable"))}</p>`;
    return;
  }
  const groups = settingsFields(si);
  body.innerHTML = `
    ${renderModeSelect(si)}
    ${uiMode === "manual" ? section(t("settings.section_light"), groups.light) : ""}
    ${section(t("settings.section_audio"), groups.audio)}
    ${section(t("settings.section_motion"), groups.motion)}`;
}

async function refresh() {
  const body = $("#settings-modal-body");
  const err = $("#settings-modal-error");
  err.hidden = true;
  body.innerHTML = `<p class="muted">${escapeHtml(t("settings.loading"))}</p>`;
  try {
    renderToggles(await fetchCameraSettings(currentCam.id));
  } catch (e) {
    body.innerHTML = "";
    err.textContent = t("settings.failed", { msg: e.message || e });
    err.hidden = false;
  }
}

async function onToggleClick(e) {
  const btn = e.target.closest("button.settings-toggle");
  if (!btn || !currentCam) return;
  const field = btn.dataset.field;
  const on = btn.dataset.on === "1";
  btn.disabled = true;
  try {
    await setCameraSettings(currentCam.id, settingsToggleBody(field, on));
    await refresh();
  } catch (err) {
    btn.disabled = false;
    const errEl = $("#settings-modal-error");
    errEl.textContent = t("settings.failed", { msg: err.message || err });
    errEl.hidden = false;
  }
}

async function onModeChange(e) {
  const sel = e.target.closest("select.settings-mode-select");
  if (!sel || !currentCam) return;
  sel.disabled = true;
  try {
    await setCameraSettings(currentCam.id, modeBody(sel.value));
    // Stick to the operator's choice: the snapshot reports manual as a
    // fixed mode (enabled=false + current mode), so only uiMode remembers.
    uiMode = sel.value;
    await refresh();
  } catch (err) {
    sel.disabled = false;
    const errEl = $("#settings-modal-error");
    errEl.textContent = t("settings.failed", { msg: err.message || err });
    errEl.hidden = false;
  }
}

export function openSettings() {
  const modal = $("#settings-modal");
  if (!modal || !currentCam) return;
  // Re-derive the mode from the camera's snapshot on every open; uiMode
  // only survives inside a session while the dialog stays open.
  uiMode = null;
  $("#settings-modal-sub").textContent = currentCam.name || currentCam.id;
  modal.hidden = false;
  refresh();
}

export function closeSettings() {
  const modal = $("#settings-modal");
  if (modal) modal.hidden = true;
}

export function initSettings() {
  $("#settings-toggle")?.addEventListener("click", openSettings);
  $("#settings-modal-close")?.addEventListener("click", closeSettings);
  // Backdrop click closes; clicks inside the card don't bubble to the
  // backdrop check because the handler tests the target.
  $("#settings-modal")?.addEventListener("click", (e) => {
    if (e.target === e.currentTarget) closeSettings();
  });
  $("#settings-modal-body")?.addEventListener("click", onToggleClick);
  $("#settings-modal-body")?.addEventListener("change", onModeChange);
  on("wallFilter", syncSettingsButton);
  on("viewMode", syncSettingsButton);
  on("wallRendered", syncSettingsButton);
  syncSettingsButton();
}
