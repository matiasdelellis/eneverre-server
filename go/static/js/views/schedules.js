// Recording-schedules manager: create/edit/delete named programs (per-weekday
// recording windows) and populate the camera wizard's schedule dropdown. A
// camera references a program by id; the server pauses the camera's pipeline
// outside the program's armed hours (see internal/schedule).
import { escapeHtml } from "../util/dom.js";
import { fetchSchedules, createSchedule, updateSchedule, deleteSchedule } from "../api.js";
import { confirmModal } from "../ui/dialog.js";
import { t } from "../i18n.js";
import { trapFocus } from "../util/focus-trap.js";

// Display order Monday-first; matches the weekday keys the API uses.
const DAY_KEYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

let modalRelease = null;
let schedulesCache = [];
let editingId = null; // "" while creating, "<id>" while editing, null in list view
let onClosed = null; // called after close when something changed (refresh dropdown)
let dirty = false;

const el = (id) => document.getElementById(id);

function setErr(id, msg, kind) {
  const node = el(id);
  if (!node) return;
  if (!msg) { node.hidden = true; node.textContent = ""; return; }
  node.textContent = msg;
  node.className = kind === "ok" ? "ok" : "error";
  node.hidden = false;
}
const formStatus = (msg, kind) => setErr("schedule-status", msg, kind);
const listStatus = (msg, kind) => setErr("schedules-status", msg, kind);

// --- public API -----------------------------------------------------------

export function initSchedules() {
  el("schedule-new")?.addEventListener("click", () => openEditor(null));
  el("schedule-cancel")?.addEventListener("click", showList);
  el("schedules-close")?.addEventListener("click", closeSchedules);
  el("schedule-form")?.addEventListener("submit", onSave);
  el("schedule-delete")?.addEventListener("click", onDelete);
  const modal = el("schedules-modal");
  modal?.addEventListener("click", (e) => { if (e.target === modal) closeSchedules(); });
}

// openSchedules shows the manager modal. opts.onClosed runs after the modal
// closes if any change was made (the wizard uses it to refresh its dropdown).
export async function openSchedules(opts = {}) {
  onClosed = opts.onClosed || null;
  dirty = false;
  buildDayRows();
  const modal = el("schedules-modal");
  modal.hidden = false;
  if (modalRelease) modalRelease();
  modalRelease = trapFocus(modal);
  showList();
  await loadSchedules();
}

// populateScheduleSelect fills a <select> with the "Always" option plus one
// option per schedule, and selects selectedId (falling back to "Always" when it
// no longer exists). Used by the camera wizard.
export async function populateScheduleSelect(selectEl, selectedId) {
  if (!selectEl) return;
  let list = [];
  try { list = await fetchSchedules(); } catch { list = []; }
  selectEl.innerHTML = "";
  const always = document.createElement("option");
  always.value = "";
  always.textContent = t("wizard.schedule_always");
  selectEl.appendChild(always);
  for (const s of list) {
    const o = document.createElement("option");
    o.value = s.id;
    o.textContent = s.name || s.id;
    selectEl.appendChild(o);
  }
  selectEl.value = selectedId || "";
}

// --- internals ------------------------------------------------------------

function closeSchedules() {
  const modal = el("schedules-modal");
  if (modal) modal.hidden = true;
  if (modalRelease) { modalRelease(); modalRelease = null; }
  const cb = onClosed;
  const changed = dirty;
  onClosed = null;
  if (changed && cb) cb();
}

// buildDayRows injects one labelled text input per weekday. Rebuilt on every
// open so the day names track a language change between sessions.
function buildDayRows() {
  const wrap = el("sched-days");
  if (!wrap) return;
  wrap.innerHTML = DAY_KEYS.map((k) => `
    <label class="sched-day">
      <span class="sched-day-name">${escapeHtml(t("day." + k))}</span>
      <input name="day-${k}" data-day="${k}" data-i18n-placeholder="schedules.day_placeholder" placeholder="${escapeHtml(t("schedules.day_placeholder"))}" />
    </label>`).join("");
}

async function loadSchedules() {
  try {
    schedulesCache = await fetchSchedules();
    if (!Array.isArray(schedulesCache)) schedulesCache = [];
  } catch (e) {
    schedulesCache = [];
    listStatus(t("schedules.failed_load", { msg: e.message || e }));
  }
  renderList();
}

// summarize renders a compact one-line view of a schedule's active days.
function summarize(s) {
  const parts = [];
  for (const k of DAY_KEYS) {
    const w = (s.days && s.days[k]) || [];
    if (w.length) parts.push(`${t("day." + k).slice(0, 3)} ${w.join(", ")}`);
  }
  return parts.length ? parts.join(" · ") : t("schedules.summary_off");
}

function renderList() {
  const wrap = el("schedules-list");
  if (!wrap) return;
  if (!schedulesCache.length) {
    wrap.innerHTML = `<div class="muted sched-empty">${escapeHtml(t("schedules.empty"))}</div>`;
    return;
  }
  wrap.innerHTML = schedulesCache.map((s) => `
    <div class="sched-row" data-id="${escapeHtml(s.id)}" role="button" tabindex="0">
      <div class="sched-row-main">
        <div class="sched-row-name">${escapeHtml(s.name || s.id)}</div>
        <div class="muted sched-row-sum">${escapeHtml(summarize(s))}</div>
      </div>
      <span class="sched-row-edit">${escapeHtml(t("cameras.edit"))}</span>
    </div>`).join("");
  for (const row of wrap.querySelectorAll(".sched-row")) {
    const open = () => {
      const s = schedulesCache.find((x) => x.id === row.dataset.id);
      if (s) openEditor(s);
    };
    row.addEventListener("click", open);
    row.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); } });
  }
}

function showList() {
  editingId = null;
  el("schedule-form").hidden = true;
  el("schedules-list-view").hidden = false;
  formStatus("");
}

function openEditor(s) {
  editingId = s ? s.id : "";
  el("schedules-list-view").hidden = true;
  const form = el("schedule-form");
  form.hidden = false;
  form.reset();
  formStatus("");
  listStatus("");
  const idInput = form.elements.id;
  idInput.readOnly = !!s;
  idInput.classList.toggle("readonly", !!s);
  el("schedule-delete").hidden = !s;
  if (s) {
    idInput.value = s.id;
    form.elements.name.value = s.name || "";
    for (const k of DAY_KEYS) {
      const inp = form.querySelector(`[data-day="${k}"]`);
      if (inp) inp.value = ((s.days && s.days[k]) || []).join(", ");
    }
  }
  (s ? form.elements.name : form.elements.id).focus();
}

// collect turns the editor form into the request body. Each day input is a
// comma-separated list of windows; the server validates and normalizes them.
function collect() {
  const form = el("schedule-form");
  const days = {};
  for (const k of DAY_KEYS) {
    const inp = form.querySelector(`[data-day="${k}"]`);
    const windows = (inp?.value || "").split(",").map((x) => x.trim()).filter(Boolean);
    if (windows.length) days[k] = windows;
  }
  return { id: form.elements.id.value.trim(), name: form.elements.name.value.trim(), days };
}

async function onSave(e) {
  e.preventDefault();
  const btn = el("schedule-save");
  btn.disabled = true;
  try {
    const body = collect();
    if (editingId) await updateSchedule(editingId, { name: body.name, days: body.days });
    else await createSchedule(body);
    dirty = true;
    await loadSchedules();
    showList();
    listStatus(t("schedules.saved"), "ok");
  } catch (err) {
    formStatus(err.message || String(err));
  } finally {
    btn.disabled = false;
  }
}

async function onDelete() {
  if (!editingId) return;
  const s = schedulesCache.find((x) => x.id === editingId);
  const label = s ? (s.name || s.id) : editingId;
  const ok = await confirmModal(
    t("schedules.delete_confirm", { label }),
    { title: t("schedules.delete_title"), okLabel: t("schedules.delete") }
  );
  if (!ok) return;
  try {
    await deleteSchedule(editingId);
    dirty = true;
    await loadSchedules();
    showList();
    listStatus(t("schedules.deleted"), "ok");
  } catch (err) {
    formStatus(err.message || String(err));
  }
}
