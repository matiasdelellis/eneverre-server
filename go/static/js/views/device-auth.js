import { $ } from "../util/dom.js";
import { sessionGet, sessionRemove, VIEW_KEY } from "../util/storage.js";
import { api } from "../api.js";

let deviceAuthTimer = null;

function clearDeviceAuthTimer() {
  if (deviceAuthTimer !== null) {
    clearInterval(deviceAuthTimer);
    deviceAuthTimer = null;
  }
}

export function showDeviceAuth(code, deviceName) {
  const wall = $("#wall");
  const sidebar = $("#viewer-side");
  const pbBar = $("#playback-bar");
  const topbar = document.querySelector(".topbar");
  if (wall) wall.hidden = true;
  if (sidebar) sidebar.hidden = true;
  if (pbBar) pbBar.hidden = true;
  if (topbar) topbar.hidden = true;
  $("#ptz-modal").hidden = true;
  $("#users-view").hidden = true;

  $("#device-auth-code").textContent = code;
  const nameEl = $("#device-auth-name");
  if (nameEl) {
    nameEl.textContent = deviceName || "Unknown device";
    nameEl.hidden = false;
  }
  const result = $("#device-auth-result");
  result.hidden = true;
  result.textContent = "";
  $("#device-auth-view").hidden = false;

  const expiresAt = Date.now() + 5 * 60 * 1000;
  clearDeviceAuthTimer();
  const tick = () => {
    const left = Math.max(0, expiresAt - Date.now());
    const mm = Math.floor(left / 60000);
    const ss = Math.floor((left % 60000) / 1000);
    const el = $("#device-auth-expires");
    if (el) el.textContent = `${mm}:${String(ss).padStart(2, "0")}`;
    if (left <= 0) {
      clearDeviceAuthTimer();
      finishDeviceAuth("This code has expired. Generate a new one on the device.");
    }
  };
  tick();
  deviceAuthTimer = setInterval(tick, 1000);
}

export function hideDeviceAuth() {
  clearDeviceAuthTimer();
  $("#device-auth-view").hidden = true;
  const topbar = document.querySelector(".topbar");
  if (topbar) topbar.hidden = false;
  sessionRemove("eneverre.pendingUsercode");
  sessionRemove("eneverre.pendingUsercodeName");
  const u = new URL(window.location.href);
  let dirty = false;
  for (const k of ["usercode", "device_name"]) {
    if (u.searchParams.has(k)) { u.searchParams.delete(k); dirty = true; }
  }
  if (dirty) {
    history.replaceState(null, "", u.pathname + (u.search ? u.search : "") + u.hash);
  }
}

function finishDeviceAuth(message, kind = "ok") {
  clearDeviceAuthTimer();
  const result = $("#device-auth-result");
  result.textContent = message;
  result.className = kind === "error" ? "error" : "muted";
  result.hidden = false;
  $("#device-auth-allow").disabled = true;
  $("#device-auth-deny").disabled = true;
  if (kind === "ok") {
    setTimeout(async () => {
      hideDeviceAuth();
      const { setViewMode } = await import("./app-shell.js");
      const saved = sessionGet(VIEW_KEY);
      setViewMode(saved === "playback" ? "playback" : "live");
    }, 1500);
  }
}

async function verifyDevice(code) {
  try {
    const data = await api("/api/auth/device/verify", {
      method: "POST",
      body: JSON.stringify({ user_code: code }),
    });
    if (data.status === "approved") {
      finishDeviceAuth("Device authorized. You can close this window.", "ok");
    } else if (data.status === "expired") {
      finishDeviceAuth("This code has expired. Generate a new one on the device.", "error");
    } else {
      finishDeviceAuth(`Unexpected response: ${data.status}`, "error");
    }
  } catch (e) {
    finishDeviceAuth(e.message || "Authorization failed.", "error");
  }
}

export function initDeviceAuth() {
  $("#device-auth-allow")?.addEventListener("click", () => {
    const code = $("#device-auth-code").textContent.trim();
    if (!code || code === "—") return;
    verifyDevice(code);
  });
  $("#device-auth-deny")?.addEventListener("click", async () => {
    hideDeviceAuth();
    const { setViewMode } = await import("./app-shell.js");
    const saved = sessionGet(VIEW_KEY);
    setViewMode(saved === "playback" ? "playback" : "live");
  });
}
