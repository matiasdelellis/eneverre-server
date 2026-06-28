import { $ } from "../util/dom.js";
import { escapeHtml } from "../util/dom.js";
import { loadJson, saveJson, get, USER_KEY } from "../util/storage.js";
import { api } from "../api.js";
import { blankToNull, displayName } from "../util/format.js";
import { alertModal, confirmModal, promptModal } from "../ui/dialog.js";
import { refreshUserMenu, closeUserMenu } from "../ui/user-menu.js";

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

function isUsersViewOpen() {
  const v = document.getElementById("users-view");
  return v && !v.hidden;
}

export function enterUsersView() {
  if (isUsersViewOpen()) return;
  document.getElementById("app").hidden = true;
  const v = document.getElementById("users-view");
  v.hidden = false;
  const meData = me();
  document.getElementById("me-username").textContent = meData ? meData.username : "";
  document.getElementById("me-role").textContent = meData ? meData.role : "";
  const nameForm = document.getElementById("my-name-form");
  if (nameForm) {
    nameForm.elements.first_name.value = meData && meData.first_name ? meData.first_name : "";
    nameForm.elements.last_name.value = meData && meData.last_name ? meData.last_name : "";
  }
  document.getElementById("users-new").hidden = !meData || !meData.is_admin;
  document.getElementById("users-list-section").hidden = !meData || !meData.is_admin;
  const tasks = [loadMySessions()];
  if (meData && meData.is_admin) tasks.push(loadUsers());
  Promise.allSettled(tasks);
}

export function exitUsersView() {
  const v = document.getElementById("users-view");
  if (v) v.hidden = true;
  document.getElementById("app").hidden = false;
  const modal = document.getElementById("user-edit-modal");
  if (modal) modal.hidden = true;
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
    setUsersStatus(`Failed to load users: ${e.message}`);
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
    empty.textContent = "No users yet.";
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
      <div class="users-name" title="${escapeHtml(u.username)}">${escapeHtml(u.username)}${isMe ? ' <span class="muted">(you)</span>' : ""}</div>
      <div><span class="role-badge role-${escapeHtml(u.role)}">${escapeHtml(u.role)}</span></div>
      <div class="users-actions">
        <button data-act="name" title="Edit first/last name">Name</button>
        <button data-act="role" data-role="${u.role === "admin" ? "user" : "admin"}">
          ${u.role === "admin" ? "Demote" : "Promote"}
        </button>
        <button data-act="password">Password</button>
        <button data-act="delete" class="danger" ${isMe || isLastAdmin ? "disabled title='Cannot delete the only admin or yourself'" : ""}>Delete</button>
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
      const result = await promptModal(`Edit display name for ${u.username}.`, {
        title: `Edit name — ${u.username}`,
        inputLabel: "First name (leave blank to clear)",
        inputValue: u.first_name || "",
        input2Label: "Last name (leave blank to clear)",
        input2Value: u.last_name || "",
        okLabel: "Save",
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
      setUsersStatus(`Name updated: ${u.username}`, "ok");
      await loadUsers();
    } else if (act === "role") {
      const newRole = btn.dataset.role;
      if (newRole === "admin") {
        const ok = await confirmModal(`Promote ${u.username} to admin?`, {
          title: "Promote to admin",
          okLabel: "Promote",
        });
        if (!ok) return;
      } else {
        const typed = await promptModal(
          `You are about to DEMOTE admin "${u.username}" to a regular user.\n` +
          `Type the username exactly to confirm:`,
          {
            title: "Demote admin",
            inputLabel: "Username",
            inputValue: "",
            mustMatch: u.username,
            okLabel: "Demote",
          }
        );
        if (typed === null) return;
        if (typed !== u.username) {
          setUsersStatus("Confirmation did not match; role unchanged", "err");
          return;
        }
      }
      await api(`/api/users/${encodeURIComponent(u.username)}/role`, {
        method: "PUT",
        body: JSON.stringify({ role: newRole }),
      });
      setUsersStatus(`Role updated: ${u.username} → ${newRole}`, "ok");
      await loadUsers();
    } else if (act === "password") {
      const pw = await promptModal(`New password for ${u.username}`, {
        title: `Set password — ${u.username}`,
        inputLabel: "New password",
      });
      if (pw === null) return;
      if (!pw) {
        setUsersStatus("Password cannot be empty", "err");
        return;
      }
      await api(`/api/users/${encodeURIComponent(u.username)}/password`, {
        method: "PUT",
        body: JSON.stringify({ password: pw }),
      });
      setUsersStatus(`Password updated for ${u.username}`, "ok");
    } else if (act === "delete") {
      const myData = me();
      if (myData && myData.username === u.username) {
        setUsersStatus("You cannot delete your own account");
        return;
      }
      if (isLastAdmin) {
        setUsersStatus("Cannot delete the last admin");
        return;
      }
      const ok = await confirmModal(
        `Delete user ${u.username}? This cannot be undone.`,
        { title: "Delete user", okLabel: "Delete" }
      );
      if (!ok) return;
      await api(`/api/users/${encodeURIComponent(u.username)}`, {
        method: "DELETE",
      });
      setUsersStatus(`User deleted: ${u.username}`, "ok");
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
  document.getElementById("user-edit-title").textContent = "New user";
  document.getElementById("user-edit-status").hidden = true;
  modal.hidden = false;
  form.elements.username.focus();
}

function closeUserEditModal() {
  document.getElementById("user-edit-modal").hidden = true;
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
      }),
    });
    closeUserEditModal();
    setUsersStatus("User created", "ok");
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
    if (wrap) wrap.innerHTML = `<p class="error">Failed to load sessions: ${escapeHtml(e.message)}</p>`;
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
    activeWrap.innerHTML = "<p class='muted'>No active sessions.</p>";
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
    activeWrap.innerHTML = "<p class='muted'>No valid sessions.</p>";
  }
  if (expired.length) {
    const sortedExpired = expired.slice().sort((a, b) => (b.created_at || 0) - (a.created_at || 0));
    for (const s of sortedExpired) expiredWrap.appendChild(renderSessionRow(s, true));
  } else {
    expiredWrap.innerHTML = "<p class='muted'>No expired sessions.</p>";
  }
}

function renderSessionRow(s, isExpired) {
  const row = document.createElement("div");
  row.className = "session-row" + (isExpired ? " session-row-expired" : "");
  const created = s.created_at ? new Date(s.created_at * 1000).toLocaleString() : "unknown";
  const expires = s.expires_at ? new Date(s.expires_at * 1000).toLocaleString() : "unknown";
  const badges = [];
  if (s.is_current) badges.push('<span class="role-badge role-admin">this device</span>');
  if (isExpired) badges.push('<span class="role-badge role-user">expired</span>');
  const deviceName = s.device_name
    ? escapeHtml(s.device_name)
    : "Browser session";
  row.innerHTML = `
    <div>
      <div class="session-device">${deviceName}</div>
      ${badges.join(" ")}
      <div class="muted session-meta">created ${escapeHtml(created)} · expires ${escapeHtml(expires)}</div>
    </div>
    <button data-id="${s.id}" ${s.is_current ? "disabled title='Sign out from the topbar to revoke this session'" : ""}>
      Revoke
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

async function submitMyPassword(e) {
  e.preventDefault();
  const form = e.target;
  const status = document.getElementById("my-pass-status");
  status.hidden = true;
  const fd = new FormData(form);
  const newPw = fd.get("new_password");
  const confirmPw = fd.get("confirm_password");
  if (newPw !== confirmPw) {
    status.textContent = "New password and confirmation do not match.";
    status.className = "error users-inline-status";
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
    form.reset();
    status.textContent = "Password updated.";
    status.className = "ok users-inline-status";
    status.hidden = false;
  } catch (err) {
    status.textContent = err.message || String(err);
    status.className = "error users-inline-status";
    status.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

async function submitMyName(e) {
  e.preventDefault();
  const form = e.target;
  const status = document.getElementById("my-name-status");
  status.hidden = true;
  const fd = new FormData(form);
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    const data = await api("/api/users/me/name", {
      method: "PUT",
      body: JSON.stringify({
        first_name: blankToNull(fd.get("first_name")),
        last_name: blankToNull(fd.get("last_name")),
      }),
    });
    const u = loadJson(USER_KEY);
    if (u) {
      u.first_name = data.first_name || null;
      u.last_name = data.last_name || null;
      saveJson(USER_KEY, u);
      refreshUserMenu({ ...u, displayName: displayName(u) || u.username });
    }
    status.textContent = "Name updated.";
    status.className = "ok users-inline-status";
    status.hidden = false;
  } catch (err) {
    status.textContent = err.message || String(err);
    status.className = "error users-inline-status";
    status.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

export function initUsers() {
  document.getElementById("users-btn")?.addEventListener("click", () => { closeUserMenu(); enterUsersView(); });
  document.getElementById("users-back")?.addEventListener("click", exitUsersView);
  document.getElementById("users-new")?.addEventListener("click", openUserEditModal);
  document.getElementById("user-edit-cancel")?.addEventListener("click", closeUserEditModal);
  document.getElementById("user-edit-form")?.addEventListener("submit", submitNewUser);
  document.getElementById("my-pass-form")?.addEventListener("submit", submitMyPassword);
  document.getElementById("my-name-form")?.addEventListener("submit", submitMyName);
  const modal = document.getElementById("user-edit-modal");
  if (modal) {
    modal.addEventListener("click", (e) => {
      if (e.target === modal) closeUserEditModal();
    });
  }
}
