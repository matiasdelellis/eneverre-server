import { $ } from "../util/dom.js";
import { escapeHtml } from "../util/dom.js";
import { loadJson, saveJson, get, USER_KEY } from "../util/storage.js";
import { api } from "../api.js";
import { blankToNull, displayName } from "../util/format.js";
import { alertModal, confirmModal, promptModal } from "../ui/dialog.js";
import { trapFocus } from "../util/focus-trap.js";

// Focus-trap release for the open user-edit / my-password modals (null when closed).
let userEditRelease = null;
let myPasswordRelease = null;
import { refreshUserMenu, closeUserMenu } from "../ui/user-menu.js";
import { moveGlobalControlsTo, closeOverlayViews, backLabel } from "./app-shell.js";
import { t } from "../i18n.js";

let usersCache = null;     // [{ username, role, first_name, last_name }, ...]
let sessionsCache = null;  // { active: [...], expired: [...] }

export function me() {
  const u = loadJson(USER_KEY);
  if (!u) return null;
  return {
    username: u.username,
    first_name: u.first_name || null,
    last_name: u.last_name || null,
    role: u.role,
    is_admin: u.is_admin !== undefined ? u.is_admin : u.role === "admin",
    displayName: displayName(u) || u.username,
  };
}

// enterUsersView opens one of two distinct panels that share the same
// <section id="users-view"> shell (topbar + body) but never show their cards
// at the same time:
//   "account" (default) — the signed-in user's own settings only (name,
//      password, sessions). Reachable by every user via the "My account"
//      menu item. The admin user-management section stays hidden.
//   "manage" — the admin user-management section only (list + "New user").
//      Personal settings are NOT shown here; the admin reaches their own
//      account through the separate "My account" item. Only takes effect for
//      admins; a non-admin who somehow requests it falls back to "account".
export function enterUsersView(mode = "account") {
  closeOverlayViews(); // never stack on top of the Cameras panel
  document.getElementById("app").hidden = true;
  const v = document.getElementById("users-view");
  v.hidden = false;
  const backEl = v.querySelector("#users-back .back-label");
  if (backEl) backEl.textContent = backLabel();
  moveGlobalControlsTo(v.querySelector("header.topbar"));
  const meData = me();
  const manage = mode === "manage" && !!meData && meData.is_admin;
  const titleEl = document.getElementById("users-view-title");
  if (titleEl) titleEl.textContent = manage ? t("users.title") : t("users.my_account");
  document.getElementById("account-section").hidden = manage;
  document.getElementById("users-new").hidden = !manage;
  document.getElementById("users-list-section").hidden = !manage;
  setUsersStatus(null);
  const tasks = [];
  if (manage) {
    tasks.push(loadUsers());
  } else {
    document.getElementById("me-signed-in-as").textContent = t("users.signed_in_as", {
      username: meData ? meData.username : "",
      role: meData ? meData.role : "",
    });
    renderMyName(meData);
    tasks.push(loadMySessions());
  }
  Promise.allSettled(tasks);
}

export function exitUsersView() {
  const v = document.getElementById("users-view");
  if (v) v.hidden = true;
  // Hand the global topbar controls (theme + user menu) back to the
  // main app's topbar before showing it, so the user can reach them.
  moveGlobalControlsTo(document.querySelector("#app .app-main header.topbar"));
  document.getElementById("app").hidden = false;
  closeUserEditModal();
  closeMyPasswordModal();
}

function renderMyName(meData) {
  const el = document.getElementById("my-name-display");
  if (el) el.textContent = (meData && displayName(meData)) || "—";
}

function setUsersStatus(msg, kind) {
  const el = document.getElementById("users-status");
  if (!el) return;
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.textContent = msg;
  el.className = kind === "ok" ? "ok" : "error";
  el.hidden = false;
}

async function loadUsers() {
  try {
    usersCache = await api("/api/users");
  } catch (e) {
    setUsersStatus(t("users.failed_load", { msg: e.message }));
    usersCache = [];
  }
  renderUsers();
}

function renderUsers() {
  const wrap = document.getElementById("users-rows");
  if (!wrap) return;
  wrap.innerHTML = "";
  const list = Array.isArray(usersCache) ? usersCache : [];
  if (!list.length) {
    const empty = document.createElement("div");
    empty.className = "users-row users-empty muted";
    empty.textContent = t("users.no_users");
    wrap.appendChild(empty);
    return;
  }
  const adminCount = list.filter((u) => u.role === "admin").length;
  const myData = me();
  for (const u of list) {
    const row = document.createElement("div");
    row.className = "users-row";
    row.dataset.username = u.username;
    const isMe = myData && myData.username === u.username;
    const isLastAdmin = u.role === "admin" && adminCount === 1;
    const fullName = displayName(u);
    row.innerHTML = `
      <div class="users-fullname${fullName ? "" : " muted"}" title="${escapeHtml(fullName || "—")}">${escapeHtml(fullName || "—")}</div>
      <div class="users-name" title="${escapeHtml(u.username)}">${escapeHtml(u.username)}${isMe ? ' <span class="muted">' + t("users.you") + '</span>' : ""}</div>
      <div><span class="role-badge role-${escapeHtml(u.role)}">${escapeHtml(u.role)}</span></div>
      <div class="users-actions">
        <button data-act="name" title="${escapeHtml(t("users.edit_name_title", { username: u.username }))}">${t("users.edit_name")}</button>
        <button data-act="role" data-role="${u.role === "admin" ? "user" : "admin"}">
          ${u.role === "admin" ? t("users.demote") : t("users.promote")}
        </button>
        <button data-act="password">${t("users.password_btn")}</button>
        <button data-act="delete" class="danger" ${isMe || isLastAdmin ? "disabled title='" + t("users.delete_disabled") + "'" : ""}>${t("users.delete_btn")}</button>
      </div>
    `;
    row.addEventListener("click", (e) => onUserActionClick(e, u, isLastAdmin));
    wrap.appendChild(row);
  }
}

async function onUserActionClick(e, u, isLastAdmin) {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  const act = btn.dataset.act;
  try {
    if (act === "name") {
      const result = await promptModal(t("users.edit_name_title", { username: u.username }), {
        title: t("users.edit_name_modal_title", { username: u.username }),
        inputLabel: t("users.first_name_label"),
        inputValue: u.first_name || "",
        input2Label: t("users.last_name_label"),
        input2Value: u.last_name || "",
        okLabel: t("users.save"),
      });
      if (result === null) return;
      const first = blankToNull(result.first);
      const last  = blankToNull(result.second);
      await api(`/api/users/${encodeURIComponent(u.username)}/name`, {
        method: "PUT",
        body: JSON.stringify({ first_name: first, last_name: last }),
      });
      const myData = me();
      if (myData && myData.username === u.username) {
        myData.first_name = first;
        myData.last_name = last;
        saveJson(USER_KEY, myData);
        refreshUserMenu(myData);
      }
      setUsersStatus(t("users.name_updated_status", { username: u.username }), "ok");
      await loadUsers();
    } else if (act === "role") {
      const newRole = btn.dataset.role;
      if (newRole === "admin") {
        const ok = await confirmModal(t("users.promote_confirm", { username: u.username }), {
          title: t("users.promote_title"),
          okLabel: t("users.promote_ok"),
        });
        if (!ok) return;
      } else {
        const typed = await promptModal(
          t("users.demote_confirm", { username: u.username }),
          {
            title: t("users.demote_title"),
            inputLabel: t("users.col_username"),
            inputValue: "",
            mustMatch: u.username,
            okLabel: t("users.demote_ok"),
          }
        );
        if (typed === null) return;
        if (typed !== u.username) {
          setUsersStatus(t("users.demote_mismatch"), "err");
          return;
        }
      }
      await api(`/api/users/${encodeURIComponent(u.username)}/role`, {
        method: "PUT",
        body: JSON.stringify({ role: newRole }),
      });
      setUsersStatus(t("users.role_updated", { username: u.username, role: newRole }), "ok");
      await loadUsers();
    } else if (act === "password") {
      const pw = await promptModal(t("users.new_password_for", { username: u.username }), {
        title: t("users.set_password_title", { username: u.username }),
        inputLabel: t("users.password_input_label"),
      });
      if (pw === null) return;
      if (!pw) {
        setUsersStatus(t("users.password_empty"), "err");
        return;
      }
      const force = await confirmModal(
        t("users.force_change_confirm", { username: u.username }),
        { title: t("users.force_change_title"), okLabel: t("users.require_change"), cancelLabel: t("users.no_keep") }
      );
      await api(`/api/users/${encodeURIComponent(u.username)}/password`, {
        method: "PUT",
        body: JSON.stringify({ password: pw, must_change_password: force === true }),
      });
      setUsersStatus(t("users.password_updated_for", { username: u.username }), "ok");
    } else if (act === "delete") {
      const myData = me();
      if (myData && myData.username === u.username) {
        setUsersStatus(t("users.cannot_delete_self"));
        return;
      }
      if (isLastAdmin) {
        setUsersStatus(t("users.cannot_delete_last_admin"));
        return;
      }
      const ok = await confirmModal(
        t("users.delete_confirm", { username: u.username }),
        { title: t("users.delete_title"), okLabel: t("users.delete_btn") }
      );
      if (!ok) return;
      await api(`/api/users/${encodeURIComponent(u.username)}`, {
        method: "DELETE",
      });
      setUsersStatus(t("users.deleted", { username: u.username }), "ok");
      await loadUsers();
    }
  } catch (err) {
    setUsersStatus(err.message || String(err));
  }
}

function openUserEditModal() {
  const modal = document.getElementById("user-edit-modal");
  const form = document.getElementById("user-edit-form");
  form.reset();
  document.getElementById("user-edit-title").textContent = t("user-edit.title");
  document.getElementById("user-edit-status").hidden = true;
  modal.hidden = false;
  // Release a stale trap first (closeOverlayViews can hide the modal without
  // calling closeUserEditModal), so reopening never stacks listeners.
  if (userEditRelease) userEditRelease();
  userEditRelease = trapFocus(modal);
  form.elements.username.focus();
}

function closeUserEditModal() {
  document.getElementById("user-edit-modal").hidden = true;
  if (userEditRelease) { userEditRelease(); userEditRelease = null; }
}

async function submitNewUser(e) {
  e.preventDefault();
  const form = e.target;
  const status = document.getElementById("user-edit-status");
  status.hidden = true;
  const fd = new FormData(form);
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    await api("/api/users", {
      method: "POST",
      body: JSON.stringify({
        username: fd.get("username"),
        password: fd.get("password"),
        first_name: blankToNull(fd.get("first_name")),
        last_name: blankToNull(fd.get("last_name")),
        role: fd.get("role"),
        must_change_password: fd.get("must_change_password") === "on",
      }),
    });
    closeUserEditModal();
    setUsersStatus(t("users.created"), "ok");
    await loadUsers();
  } catch (err) {
    status.textContent = err.message || String(err);
    status.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

async function loadMySessions() {
  try {
    sessionsCache = await api("/api/users/me/sessions");
  } catch (e) {
    sessionsCache = { active: [], expired: [] };
    const wrap = document.getElementById("sessions-list-active");
    if (wrap) wrap.innerHTML = `<p class="error">${t("users.failed_sessions", { msg: escapeHtml(e.message) })}</p>`;
    document.getElementById("sessions-list-expired").innerHTML = "";
    return;
  }
  renderSessions();
}

function renderSessions() {
  const activeWrap = document.getElementById("sessions-list-active");
  const expiredWrap = document.getElementById("sessions-list-expired");
  if (!activeWrap || !expiredWrap) return;
  activeWrap.innerHTML = "";
  expiredWrap.innerHTML = "";
  const cache = sessionsCache || { active: [], expired: [] };
  const active = Array.isArray(cache.active) ? cache.active : [];
  const expired = Array.isArray(cache.expired) ? cache.expired : [];
  if (!active.length && !expired.length) {
    activeWrap.innerHTML = `<p class='muted'>${t("users.no_active")}</p>`;
    expiredWrap.innerHTML = "";
    return;
  }
  if (active.length) {
    const sortedActive = active.slice().sort((a, b) => {
      if (a.is_current && !b.is_current) return -1;
      if (b.is_current && !a.is_current) return 1;
      return (b.created_at || 0) - (a.created_at || 0);
    });
    for (const s of sortedActive) activeWrap.appendChild(renderSessionRow(s, false));
  } else {
    activeWrap.innerHTML = `<p class='muted'>${t("users.no_valid")}</p>`;
  }
  if (expired.length) {
    const sortedExpired = expired.slice().sort((a, b) => (b.created_at || 0) - (a.created_at || 0));
    for (const s of sortedExpired) expiredWrap.appendChild(renderSessionRow(s, true));
  } else {
    expiredWrap.innerHTML = `<p class='muted'>${t("users.no_expired")}</p>`;
  }
}

function renderSessionRow(s, isExpired) {
  const row = document.createElement("div");
  row.className = "session-row" + (isExpired ? " session-row-expired" : "");
  const created = s.created_at ? new Date(s.created_at * 1000).toLocaleString() : "unknown";
  const expires = s.expires_at ? new Date(s.expires_at * 1000).toLocaleString() : "unknown";
  const badges = [];
  if (s.is_current) badges.push(`<span class="role-badge role-admin">${t("users.this_device")}</span>`);
  if (isExpired) badges.push(`<span class="role-badge role-user">${t("users.expired")}</span>`);
  const deviceName = s.device_name
    ? escapeHtml(s.device_name)
    : t("users.browser_session");
  row.innerHTML = `
    <div>
      <div class="session-device">${deviceName}</div>
      ${badges.join(" ")}
      <div class="muted session-meta">created ${escapeHtml(created)} · expires ${escapeHtml(expires)}</div>
    </div>
    <button data-id="${s.id}" ${s.is_current ? "disabled title='" + t("users.revoke_title") + "'" : ""}>
      ${t("users.revoke")}
    </button>
  `;
  row.addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-id]");
    if (!btn) return;
    try {
      await api(`/api/users/me/sessions/${btn.dataset.id}`, { method: "DELETE" });
      await loadMySessions();
    } catch (err) {
      setUsersStatus(err.message || String(err));
    }
  });
  return row;
}

// "Update name" (My account): same 2-field promptModal already used by
// admins to rename other users (see onUserActionClick, act === "name"), but
// hitting the /me endpoint and refreshing our own cached user + topbar menu.
async function openMyNameModal() {
  const meData = me();
  const result = await promptModal(t("users.my_name"), {
    title: t("users.my_name"),
    inputLabel: t("users.first_name"),
    inputValue: meData && meData.first_name ? meData.first_name : "",
    input2Label: t("users.last_name"),
    input2Value: meData && meData.last_name ? meData.last_name : "",
    okLabel: t("users.save"),
  });
  if (result === null) return;
  try {
    const data = await api("/api/users/me/name", {
      method: "PUT",
      body: JSON.stringify({
        first_name: blankToNull(result.first),
        last_name: blankToNull(result.second),
      }),
    });
    const u = loadJson(USER_KEY);
    if (u) {
      u.first_name = data.first_name || null;
      u.last_name = data.last_name || null;
      saveJson(USER_KEY, u);
      refreshUserMenu({ ...u, displayName: displayName(u) || u.username });
    }
    renderMyName(me());
    setUsersStatus(t("users.name_updated"), "ok");
  } catch (err) {
    setUsersStatus(err.message || String(err));
  }
}

// "Update password" (My account): promptModal only supports plain-text
// inputs (max two), so a dedicated modal (#my-password-modal, styled like
// #user-edit-modal) carries the three password fields instead.
function openMyPasswordModal() {
  const modal = document.getElementById("my-password-modal");
  const form = document.getElementById("my-pass-form");
  form.reset();
  document.getElementById("my-pass-status").hidden = true;
  modal.hidden = false;
  if (myPasswordRelease) myPasswordRelease();
  myPasswordRelease = trapFocus(modal);
  form.elements.current_password.focus();
}

function closeMyPasswordModal() {
  document.getElementById("my-password-modal").hidden = true;
  if (myPasswordRelease) { myPasswordRelease(); myPasswordRelease = null; }
}

async function submitMyPassword(e) {
  e.preventDefault();
  const form = e.target;
  const status = document.getElementById("my-pass-status");
  status.hidden = true;
  const fd = new FormData(form);
  const newPw = fd.get("new_password");
  const confirmPw = fd.get("confirm_password");
  if (newPw !== confirmPw) {
    status.textContent = t("users.password_mismatch");
    status.hidden = false;
    return;
  }
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    await api("/api/users/me/password", {
      method: "PUT",
      body: JSON.stringify({
        current_password: fd.get("current_password"),
        new_password: newPw,
      }),
    });
    closeMyPasswordModal();
    setUsersStatus(t("users.password_updated"), "ok");
  } catch (err) {
    status.textContent = err.message || String(err);
    status.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

export function initUsers() {
  document.getElementById("account-btn")?.addEventListener("click", () => { closeUserMenu(); enterUsersView("account"); });
  document.getElementById("users-btn")?.addEventListener("click", () => { closeUserMenu(); enterUsersView("manage"); });
  document.getElementById("users-back")?.addEventListener("click", exitUsersView);
  document.getElementById("users-new")?.addEventListener("click", openUserEditModal);
  document.getElementById("user-edit-cancel")?.addEventListener("click", closeUserEditModal);
  document.getElementById("user-edit-form")?.addEventListener("submit", submitNewUser);
  document.getElementById("my-name-btn")?.addEventListener("click", openMyNameModal);
  document.getElementById("my-pass-btn")?.addEventListener("click", openMyPasswordModal);
  document.getElementById("my-pass-cancel")?.addEventListener("click", closeMyPasswordModal);
  document.getElementById("my-pass-form")?.addEventListener("submit", submitMyPassword);
  const modal = document.getElementById("user-edit-modal");
  if (modal) {
    modal.addEventListener("click", (e) => {
      if (e.target === modal) closeUserEditModal();
    });
  }
  const pwModal = document.getElementById("my-password-modal");
  if (pwModal) {
    pwModal.addEventListener("click", (e) => {
      if (e.target === pwModal) closeMyPasswordModal();
    });
  }
}
