import { escapeHtml } from "../util/dom.js";
import {
  api, createCamera, updateCamera, getCameraConfig, deleteCamera, probeCamera, invalidateCameras,
} from "../api.js";
import { getState } from "../state.js";
import { confirmModal } from "../ui/dialog.js";
import { closeUserMenu } from "../ui/user-menu.js";
import { moveGlobalControlsTo, closeOverlayViews, backLabel } from "./app-shell.js";
import { t } from "../i18n.js";
import { trapFocus } from "../util/focus-trap.js";

// Focus-trap release for the open wizard modal (null when closed).
let wizardRelease = null;

let camerasCache = null; // [Camera, ...] as returned by GET /api/cameras
let wizardStep = 1;
let editingId = null; // non-null when the wizard is editing an existing camera
const LAST_STEP = 5;

// --- view open/close ------------------------------------------------------

function isCamerasViewOpen() {
  const v = document.getElementById("cameras-view");
  return v && !v.hidden;
}

export function enterCamerasView() {
  if (isCamerasViewOpen()) return;
  closeOverlayViews(); // never stack on top of the Users panel
  document.getElementById("app").hidden = true;
  document.getElementById("cameras-view").hidden = false;
  const backEl = document.querySelector("#cameras-back .back-label");
  if (backEl) backEl.textContent = backLabel();
  moveGlobalControlsTo(document.querySelector("#cameras-view header.topbar"));
  document.getElementById("cameras-new").hidden = false;
  document.getElementById("cameras-list-section").hidden = false;
  loadCameras();
}

export function exitCamerasView() {
  const v = document.getElementById("cameras-view");
  if (v) v.hidden = true;
  closeWizard();
  // Hand the global topbar controls (theme + user menu) back to the
  // main app's topbar before showing it.
  moveGlobalControlsTo(document.querySelector("#app .app-main header.topbar"));
  document.getElementById("app").hidden = false;
}

function setStatus(msg, kind) {
  const el = document.getElementById("cameras-status");
  if (!el) return;
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.textContent = msg;
  el.className = kind === "ok" ? "ok" : "error";
  el.hidden = false;
}

// --- list -----------------------------------------------------------------

async function loadCameras() {
  try {
    camerasCache = await api("/api/cameras");
  } catch (e) {
    setStatus(t("cameras.failed_load", { msg: e.message }));
    camerasCache = [];
  }
  renderCameras();
}

function capsSummary(c) {
  const caps = c.capabilities || {};
  const on = [];
  if (caps.playback) on.push("playback");
  if (caps.ptz) on.push("PTZ");
  if (caps.talk) on.push("talk");
  if (caps.thumbnail) on.push("thumbnail");
  if (caps.privacy) on.push("privacy");
  return on.join(" · ");
}

function renderCameras() {
  const wrap = document.getElementById("cameras-rows");
  if (!wrap) return;
  wrap.innerHTML = "";
  const list = Array.isArray(camerasCache) ? camerasCache : [];
  if (!list.length) {
    const empty = document.createElement("div");
    empty.className = "users-row users-empty muted";
    empty.textContent = t("cameras.empty");
    wrap.appendChild(empty);
    return;
  }
  for (const c of list) {
    const row = document.createElement("div");
    row.className = "users-row cameras-row";
    row.dataset.id = c.id;
    const caps = capsSummary(c);
    row.innerHTML = `
      <div class="users-fullname" title="${escapeHtml(c.name || "—")}">${escapeHtml(c.name || "—")}${caps ? `<small class="muted cam-caps">${escapeHtml(caps)}</small>` : ""}</div>
      <div class="users-name" title="${escapeHtml(c.id)}">${escapeHtml(c.id)}</div>
      <div title="${escapeHtml(c.location || "—")}">${escapeHtml(c.location || "—")}</div>
      <div class="users-actions">
        <button data-act="edit">${t("cameras.edit")}</button>
        <button data-act="delete" class="danger">${t("cameras.delete")}</button>
      </div>
    `;
    row.addEventListener("click", (e) => onCameraActionClick(e, c));
    wrap.appendChild(row);
  }
}

async function onCameraActionClick(e, c) {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  if (btn.dataset.act === "edit") {
    try {
      const config = await getCameraConfig(c.id);
      openWizard(config);
    } catch (err) {
      setStatus(err.message || String(err));
    }
    return;
  }
  if (btn.dataset.act !== "delete") return;
  const label = c.name ? `${c.name} (${c.id})` : c.id;
  const ok = await confirmModal(
    t("cameras.delete_confirm", { label }),
    { title: t("cameras.delete_title"), okLabel: t("cameras.delete_ok") }
  );
  if (!ok) return;
  try {
    await deleteCamera(c.id);
    invalidateCameras();
    setStatus(t("cameras.deleted", { id: c.id }), "ok");
    await loadCameras();
    refreshUnderlyingViews();
  } catch (err) {
    setStatus(err.message || String(err));
  }
}

// --- wizard ---------------------------------------------------------------

// openWizard opens the create wizard, or — when passed a camera config (the
// full spec from GET .../config) — the edit wizard: fields prefilled, the id
// locked (it is the recording path and cannot change), and submit doing a PUT.
function openWizard(config = null) {
  wizardStep = 1;
  editingId = config ? config.id : null;
  const form = document.getElementById("cam-wizard-form");
  form.reset();
  document.getElementById("cam-wizard-status").hidden = true;
  document.getElementById("cam-probe-result").textContent = "";
  document.getElementById("cam-wizard-title").textContent = editingId ? t("cameras.edit_title") : t("cameras.add_title");
  document.getElementById("cam-wizard-create").textContent = editingId ? t("cameras.save_changes") : t("cameras.create");
  const idInput = form.elements.id;
  idInput.readOnly = !!editingId;
  idInput.classList.toggle("readonly", !!editingId);
  if (config) fillForm(config);
  const modal = document.getElementById("cam-wizard-modal");
  modal.hidden = false;
  // Release a stale trap first: closeOverlayViews (overlay switch / logout)
  // hides the modal directly without calling closeWizard, so a leftover trap
  // could otherwise stack on reopen.
  if (wizardRelease) wizardRelease();
  wizardRelease = trapFocus(modal);
  showStep(1);
  (editingId ? form.elements.name : form.elements.id).focus();
}

// fillForm prefills the wizard from a stored camera config (spec JSON). The
// thingino coordinates use -1 as the "unset" sentinel; show those blank.
// The PTZ calibration fields also show blank when the stored value matches
// the field's own placeholder (the server default, per index.html) — the
// placeholder then reveals the default — so the wizard stays compact unless
// the operator customised them. Comparing against the placeholder (rather
// than a number re-declared here) means the Go default and the wizard can
// never drift apart.
function fillForm(c) {
  const f = document.getElementById("cam-wizard-form").elements;
  const text = (name, v) => { f[name].value = v == null ? "" : String(v); };
  const check = (name, v) => { f[name].checked = !!v; };
  const coord = (name, v) => { f[name].value = (v == null || v < 0) ? "" : String(v); };
  const calib = (name, v) => {
    f[name].value = (v == null || String(v) === f[name].placeholder) ? "" : String(v);
  };
  text("id", c.id);
  text("name", c.name);
  text("location", c.location);
  text("comment", c.comment);
  text("source", c.source);
  f.transport.value = c.transport || "";
  text("backchannel", c.backchannel);
  text("snapshot_url", c.snapshot_url);
  check("record", c.record);
  check("mse", c.mse);
  check("relay", c.relay);
  check("privacy", c.privacy);
  check("playback", c.playback);
  text("width", c.width);
  text("height", c.height);
  text("thingino_url", c.thingino_url);
  text("thingino_api_key", c.thingino_api_key);
  check("ptz", c.ptz);
  coord("home_x", c.home_x);
  coord("home_y", c.home_y);
  coord("privacy_x", c.privacy_x);
  coord("privacy_y", c.privacy_y);
  calib("pan_steps", c.pan_steps);
  calib("pan_degrees", c.pan_degrees);
  calib("tilt_steps", c.tilt_steps);
  calib("tilt_degrees", c.tilt_degrees);
  calib("fov_h", c.fov_h);
}

function closeWizard() {
  editingId = null;
  const m = document.getElementById("cam-wizard-modal");
  if (m) m.hidden = true;
  if (wizardRelease) { wizardRelease(); wizardRelease = null; }
}

function showStep(n) {
  wizardStep = n;
  for (const el of document.querySelectorAll("#cam-wizard-form .cam-step")) {
    el.hidden = Number(el.dataset.step) !== n;
  }
  for (const li of document.querySelectorAll("#cam-wizard-steps li")) {
    const s = Number(li.dataset.step);
    li.classList.toggle("active", s === n);
    li.classList.toggle("done", s < n);
  }
  document.getElementById("cam-wizard-back").hidden = n === 1;
  document.getElementById("cam-wizard-next").hidden = n === LAST_STEP;
  document.getElementById("cam-wizard-create").hidden = n !== LAST_STEP;
  if (n === LAST_STEP) buildReview();
}

// validateStep enforces the minimal per-step requirements before advancing.
// Returns "" when ok, or a message for the wizard status line.
function validateStep(n) {
  const form = document.getElementById("cam-wizard-form");
  if (n === 1) {
    const id = form.elements.id.value.trim();
    if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(id)) {
      return t("cameras.id_error");
    }
  }
  if (n === 2 && !form.elements.source.value.trim()) {
    return t("cameras.source_required");
  }
  return "";
}

function wizardStatus(msg, kind) {
  const el = document.getElementById("cam-wizard-status");
  if (!msg) { el.hidden = true; return; }
  el.textContent = msg;
  el.className = kind === "ok" ? "ok" : "error";
  el.hidden = false;
}

function onNext() {
  const err = validateStep(wizardStep);
  if (err) { wizardStatus(err); return; }
  wizardStatus("");
  showStep(Math.min(wizardStep + 1, LAST_STEP));
}

function onBack() {
  wizardStatus("");
  showStep(Math.max(wizardStep - 1, 1));
}

async function onProbe() {
  const form = document.getElementById("cam-wizard-form");
  const source = form.elements.source.value.trim();
  const transport = form.elements.transport.value;
  const result = document.getElementById("cam-probe-result");
  const btn = document.getElementById("cam-probe-btn");
  if (!source) { result.textContent = t("cameras.probe_empty"); result.className = "cam-probe-result error"; return; }
  btn.disabled = true;
  result.className = "cam-probe-result muted";
  result.textContent = t("cameras.probe_testing");
  try {
    const r = await probeCamera(source, transport);
    if (!r.ok) {
      result.className = "cam-probe-result error";
      result.textContent = t("cameras.probe_failed", { error: r.error || "unreachable" });
      return;
    }
    result.className = "cam-probe-result ok";
    const codecs = (r.codecs || []).join(", ") || "no codecs reported";
    const dims = r.width && r.height ? ` · ${r.width}×${r.height}` : "";
    result.textContent = t("cameras.probe_connected", { codecs, dims });
    // Prefill resolution when the probe found it and the fields are empty.
    if (r.width && !form.elements.width.value) form.elements.width.value = r.width;
    if (r.height && !form.elements.height.value) form.elements.height.value = r.height;
  } catch (err) {
    result.className = "cam-probe-result error";
    result.textContent = t("cameras.probe_error", { msg: err.message || err });
  } finally {
    btn.disabled = false;
  }
}

// collectForm turns the form into the create-request body. Booleans come from
// checkboxes; numeric fields are sent only when filled so the server applies
// its defaults otherwise.
function collectForm() {
  const f = document.getElementById("cam-wizard-form").elements;
  const trim = (name) => f[name].value.trim();
  const num = (name) => {
    const v = f[name].value.trim();
    return v === "" ? undefined : Number(v);
  };
  const body = {
    id: trim("id"),
    name: trim("name"),
    location: trim("location"),
    comment: trim("comment"),
    source: trim("source"),
    transport: f.transport.value,
    backchannel: trim("backchannel"),
    snapshot_url: trim("snapshot_url"),
    record: f.record.checked,
    mse: f.mse.checked,
    relay: f.relay.checked,
    privacy: f.privacy.checked,
    playback: f.playback.checked,
    ptz: f.ptz.checked,
    thingino_url: trim("thingino_url"),
    thingino_api_key: trim("thingino_api_key"),
  };
  const w = num("width"), h = num("height");
  if (w !== undefined) body.width = w;
  if (h !== undefined) body.height = h;
  // Sent only when the operator typed a value, so an empty field (the
  // placeholder default) keeps the server default.
  for (const k of ["home_x", "home_y", "privacy_x", "privacy_y",
                   "pan_steps", "pan_degrees", "tilt_steps", "tilt_degrees", "fov_h"]) {
    const v = num(k);
    if (v !== undefined) body[k] = v;
  }
  return body;
}

function buildReview() {
  const b = collectForm();
  const dl = document.getElementById("cam-review");
  const rows = [
    [t("cameras.review_id"), b.id],
    [t("cameras.review_name"), b.name || "—"],
    [t("cameras.review_location"), b.location || "—"],
    [t("cameras.review_source"), maskSource(b.source)],
    [t("cameras.review_transport"), b.transport || "auto"],
    [t("cameras.review_sinks"), [b.record && "record", b.mse && "MSE", b.relay && "relay"].filter(Boolean).join(", ") || "none"],
    [t("cameras.review_resolution"), b.width && b.height ? `${b.width}×${b.height}` : "default"],
    [t("cameras.review_privacy"), b.privacy ? t("cameras.yes") : t("cameras.no")],
    [t("cameras.review_talk"), b.backchannel ? t("cameras.yes") : t("cameras.no")],
    [t("cameras.review_snapshot"), b.snapshot_url ? t("cameras.yes") : t("cameras.no")],
    [t("cameras.review_thingino"), b.thingino_url ? `${b.thingino_url}${b.ptz ? " (PTZ)" : ""}` : t("cameras.no")],
  ];
  dl.innerHTML = rows
    .map(([k, v]) => `<div><dt>${escapeHtml(k)}</dt><dd>${escapeHtml(String(v))}</dd></div>`)
    .join("");
}

// maskSource hides the password in an rtsp URL for display in the review.
function maskSource(url) {
  return url.replace(/(rtsp:\/\/[^:/@]+:)[^@/]*(@)/i, "$1••••$2");
}

async function onSubmit(e) {
  e.preventDefault();
  const btn = document.getElementById("cam-wizard-create");
  const wasEditing = editingId;
  wizardStatus("");
  btn.disabled = true;
  try {
    const body = collectForm();
    const cam = wasEditing ? await updateCamera(wasEditing, body) : await createCamera(body);
    invalidateCameras();
    closeWizard();
    const action = wasEditing ? t("cameras.updated") : t("cameras.created_action");
    setStatus(t("cameras.created", { action, id: cam.id }), "ok");
    await loadCameras();
    refreshUnderlyingViews();
  } catch (err) {
    wizardStatus(err.message || String(err));
  } finally {
    btn.disabled = false;
  }
}

// refreshUnderlyingViews rebuilds the sidebar and wall (behind the cameras
// view) so a created/deleted camera shows up when the user goes back.
async function refreshUnderlyingViews() {
  const side = document.getElementById("viewer-side-scroll");
  if (side) delete side.dataset.loaded;
  const [{ loadSidebar }, wall] = await Promise.all([
    import("./sidebar.js"),
    import("./wall.js"),
  ]);
  await loadSidebar();
  const mode = getState().viewMode;
  if (mode === "live" || mode === "playback") wall.loadWall(mode);
}

// --- init -----------------------------------------------------------------

export function initCameras() {
  document.getElementById("cameras-btn")?.addEventListener("click", () => { closeUserMenu(); enterCamerasView(); });
  document.getElementById("cameras-back")?.addEventListener("click", exitCamerasView);
  document.getElementById("cameras-new")?.addEventListener("click", () => openWizard());
  document.getElementById("cam-wizard-cancel")?.addEventListener("click", closeWizard);
  document.getElementById("cam-wizard-next")?.addEventListener("click", onNext);
  document.getElementById("cam-wizard-back")?.addEventListener("click", onBack);
  document.getElementById("cam-probe-btn")?.addEventListener("click", onProbe);
  document.getElementById("cam-wizard-form")?.addEventListener("submit", onSubmit);
  const modal = document.getElementById("cam-wizard-modal");
  if (modal) {
    modal.addEventListener("click", (e) => { if (e.target === modal) closeWizard(); });
  }
}
