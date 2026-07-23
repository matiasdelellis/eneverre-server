// Server-status overlay (admin-only) plus the low-disk banner poller. Both
// read GET /api/status: the overlay renders the full snapshot (version,
// uptime, totals, storage headroom, per-camera state) and the poller keeps a
// persistent banner in sync while the recording volume is low.

import { $, escapeHtml } from "../util/dom.js";
import { loadJson, USER_KEY } from "../util/storage.js";
import { fetchStatus } from "../api.js";
import { formatBytes, formatUptime } from "../util/format.js";
import { icon as svgIcon } from "../ui/icons.js";
import { moveGlobalControlsTo, closeOverlayViews, backLabel } from "./app-shell.js";
import { closeUserMenu } from "../ui/user-menu.js";
import { setOverlay } from "../state.js";
import { t } from "../i18n.js";

const REFRESH_MS = 10_000; // status view auto-refresh cadence while open
const POLL_MS = 30_000;    // banner poll cadence (matches the server monitor)

let viewTimer = null; // auto-refresh interval while the overlay is open
let pollTimer = null; // background banner poll interval

function isAdmin() {
  return !!loadJson(USER_KEY)?.is_admin;
}

// ---- Low-disk banner ------------------------------------------------------

// applyDiskAlert toggles the persistent banner from a /api/status storage
// block. Shows it only when the monitor flagged the volume low; hides it
// otherwise (recording off, monitor disabled, or recovered).
function applyDiskAlert(storage) {
  const el = document.getElementById("disk-alert");
  if (!el) return;
  const low = !!(storage && storage.low_space);
  if (!low) {
    el.hidden = true;
    return;
  }
  const txt = document.getElementById("disk-alert-text");
  if (txt) {
    txt.textContent = t("status.disk_low_banner", {
      free: formatBytes(storage.free_bytes),
    });
  }
  el.hidden = false;
}

async function pollDiskOnce() {
  if (!isAdmin()) return;
  try {
    const data = await fetchStatus();
    applyDiskAlert(data.storage);
  } catch {
    // Transient failure (or a 403 after a role change): leave the banner as
    // is and try again on the next tick. Never surface an error for a
    // background poll.
  }
}

export function startDiskAlertPolling() {
  if (!isAdmin()) return;
  if (pollTimer) return; // already running
  pollDiskOnce();
  pollTimer = setInterval(pollDiskOnce, POLL_MS);
}

export function stopDiskAlertPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
  const el = document.getElementById("disk-alert");
  if (el) el.hidden = true;
}

// ---- Status overlay -------------------------------------------------------

function setStatusError(msg) {
  const el = document.getElementById("status-error");
  if (!el) return;
  if (!msg) {
    el.hidden = true;
    el.textContent = "";
    return;
  }
  el.textContent = msg;
  el.hidden = false;
}

// dot renders a filled/hollow status indicator with an accessible label.
function dot(on) {
  const cls = on ? "status-dot status-dot-on" : "status-dot status-dot-off";
  const label = on ? t("status.yes") : t("status.no");
  return `<span class="${cls}" role="img" aria-label="${escapeHtml(label)}">${svgIcon("circle-dot")}</span>`;
}

function tile(label, value) {
  return `<div class="status-tile"><div class="status-tile-value">${escapeHtml(String(value))}</div><div class="status-tile-label">${escapeHtml(label)}</div></div>`;
}

function renderStorage(storage) {
  if (!storage) {
    return `<section class="users-card"><h2>${escapeHtml(t("status.storage"))}</h2><p class="muted">${escapeHtml(t("status.storage_na"))}</p></section>`;
  }
  const total = storage.total_bytes || 0;
  const used = storage.used_bytes || 0;
  const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
  const low = !!storage.low_space;
  const barCls = low ? "status-bar-fill status-bar-fill-low" : "status-bar-fill";
  const alert = low
    ? `<span class="status-badge status-badge-low">${svgIcon("alert-triangle")}<span>${escapeHtml(t("status.low"))}</span></span>`
    : "";
  const minFree = storage.min_free_bytes
    ? `<p class="muted status-storage-min">${escapeHtml(t("status.min_free", { size: formatBytes(storage.min_free_bytes) }))}</p>`
    : "";
  return `
    <section class="users-card">
      <h2>${escapeHtml(t("status.storage"))} ${alert}</h2>
      <p class="muted status-storage-dir">${escapeHtml(storage.record_dir || "")}</p>
      <div class="status-bar" role="img" aria-label="${escapeHtml(t("status.disk_usage", { pct }))}">
        <div class="${barCls}" style="width:${pct}%"></div>
      </div>
      <p class="status-storage-line">${escapeHtml(t("status.disk_line", {
        free: formatBytes(storage.free_bytes),
        total: formatBytes(total),
        pct,
      }))}</p>
      ${minFree}
    </section>`;
}

function renderCameras(cams) {
  const rows = (cams || []).map((c) => `
    <div class="users-row status-cam-row">
      <div class="status-cam-name">${escapeHtml(c.name || c.id)}</div>
      <div>${dot(c.connected)}</div>
      <div>${dot(c.recording)}</div>
      <div>${dot(c.mse_active)}</div>
      <div>${dot(c.privacy)}</div>
    </div>`).join("");
  return `
    <section class="users-card">
      <h2>${escapeHtml(t("status.cameras"))}</h2>
      <div class="users-table status-cam-table">
        <div class="users-row users-header status-cam-row">
          <div>${escapeHtml(t("status.col_camera"))}</div>
          <div>${escapeHtml(t("status.col_connected"))}</div>
          <div>${escapeHtml(t("status.col_recording"))}</div>
          <div>${escapeHtml(t("status.col_mse"))}</div>
          <div>${escapeHtml(t("status.col_privacy"))}</div>
        </div>
        ${rows || `<div class="users-row status-cam-row"><div class="muted">${escapeHtml(t("status.no_cameras"))}</div></div>`}
      </div>
    </section>`;
}

function render(data) {
  const body = document.getElementById("status-body");
  if (!body) return;
  const totals = data.totals || {};
  const recBadge = data.recording_enabled
    ? `<span class="status-badge status-badge-on">${escapeHtml(t("status.recording_on"))}</span>`
    : `<span class="status-badge">${escapeHtml(t("status.recording_off"))}</span>`;
  body.innerHTML = `
    <section class="users-card">
      <h2>${escapeHtml(t("status.overview"))} ${recBadge}</h2>
      <p class="muted">${escapeHtml(t("status.version", { version: data.version || "—" }))} ·
        ${escapeHtml(t("status.uptime", { uptime: formatUptime(data.uptime_seconds) }))}</p>
      <div class="status-grid">
        ${tile(t("status.total_cameras"), totals.cameras ?? 0)}
        ${tile(t("status.connected"), totals.connected ?? 0)}
        ${tile(t("status.recording"), totals.recording ?? 0)}
        ${tile(t("status.privacy"), totals.privacy ?? 0)}
      </div>
    </section>
    ${renderStorage(data.storage)}
    ${renderCameras(data.cameras)}`;
}

async function load() {
  setStatusError(null);
  try {
    const data = await fetchStatus();
    render(data);
    applyDiskAlert(data.storage); // keep the banner consistent with what's shown
  } catch (e) {
    setStatusError(e.message || t("status.load_failed"));
  }
}

export function enterStatusView() {
  if (!isAdmin()) return;
  closeOverlayViews(); // never stack on top of Users/Cameras
  document.getElementById("app").hidden = true;
  const v = document.getElementById("status-view");
  v.hidden = false;
  const backEl = v.querySelector("#status-back .back-label");
  if (backEl) backEl.textContent = backLabel();
  moveGlobalControlsTo(v.querySelector("header.topbar"));
  load();
  // Auto-refresh while the panel is open so an operator watching a purge
  // sees free space climb without clicking Refresh.
  if (viewTimer) clearInterval(viewTimer);
  viewTimer = setInterval(load, REFRESH_MS);
}

export function exitStatusView() {
  const v = document.getElementById("status-view");
  if (v) v.hidden = true;
  moveGlobalControlsTo(document.querySelector("#app .app-main header.topbar"));
  document.getElementById("app").hidden = false;
  if (viewTimer) {
    clearInterval(viewTimer);
    viewTimer = null;
  }
}

export function initStatus() {
  document.getElementById("status-btn")?.addEventListener("click", () => {
    closeUserMenu();
    setOverlay("status");
  });
  document.getElementById("status-back")?.addEventListener("click", () => setOverlay(null));
  document.getElementById("status-refresh")?.addEventListener("click", () => load());
  document.getElementById("disk-alert-open")?.addEventListener("click", () => setOverlay("status"));
}
