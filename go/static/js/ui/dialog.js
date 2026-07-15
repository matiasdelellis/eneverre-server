import { t } from "../i18n.js";

const dlg = {
  modal: null, card: null, titleEl: null, bodyEl: null,
  inputWrap: null, inputLabel: null, inputEl: null,
  input2Wrap: null, input2Label: null, input2El: null,
  errorEl: null, okBtn: null, cancelBtn: null,
  _active: null,
};

function resolve(value) {
  if (!dlg._active) return;
  const r = dlg._active.resolve;
  dlg._active = null;
  dlg.modal.hidden = true;
  dlg.errorEl.hidden = true;
  dlg.inputEl.value = "";
  dlg.input2El.value = "";
  r(value);
}

function onOk() {
  if (!dlg._active) return;
  const { kind, mustMatch, hasSecond } = dlg._active;
  if (kind === "prompt") {
    const v = dlg.inputEl.value;
    if (mustMatch !== undefined && v !== mustMatch) {
      dlg.errorEl.textContent = t("dlg.must_match", { value: mustMatch });
      dlg.errorEl.hidden = false;
      dlg.inputEl.focus();
      dlg.inputEl.select();
      return;
    }
    if (hasSecond) {
      resolve({ first: dlg.inputEl.value, second: dlg.input2El.value });
    } else {
      resolve(dlg.inputEl.value);
    }
  } else {
    resolve(true);
  }
}

function open({ title, body, kind, inputLabel, inputValue, input2Label, input2Value, mustMatch, okLabel, cancelLabel, dismissOnBackdrop }) {
  return new Promise((resolveP) => {
    if (!dlg.modal) init();
    const hasSecond = input2Label !== undefined;
    dlg._active = { resolve: resolveP, kind, mustMatch, hasSecond, dismissOnBackdrop: dismissOnBackdrop !== false };
    dlg.titleEl.textContent = title || t("dlg.confirm");
    dlg.bodyEl.textContent  = body || "";
    dlg.okBtn.textContent     = okLabel || t("dlg.ok");
    dlg.cancelBtn.textContent = cancelLabel || t("dlg.cancel");
    if (kind === "prompt") {
      dlg.inputWrap.hidden = false;
      dlg.inputLabel.textContent = inputLabel || "";
      dlg.inputEl.value = inputValue || "";
      if (input2Label !== undefined) {
        dlg.input2Wrap.hidden = false;
        dlg.input2Label.textContent = input2Label || "";
        dlg.input2El.value = input2Value || "";
      } else {
        dlg.input2Wrap.hidden = true;
        dlg.input2El.value = "";
      }
    } else {
      dlg.inputWrap.hidden = true;
      dlg.input2Wrap.hidden = true;
    }
    dlg.errorEl.hidden = true;
    dlg.modal.hidden = false;
    setTimeout(() => {
      if (kind === "prompt") { dlg.inputEl.focus(); dlg.inputEl.select(); }
      else dlg.okBtn.focus();
    }, 0);
  });
}

function init() {
  dlg.modal = document.getElementById("dlg-modal");
  if (!dlg.modal) return;
  dlg.card       = dlg.modal.querySelector(".dlg-card");
  dlg.titleEl    = document.getElementById("dlg-title");
  dlg.bodyEl     = document.getElementById("dlg-body");
  dlg.inputWrap  = document.getElementById("dlg-input-wrap");
  dlg.inputLabel = document.getElementById("dlg-input-label");
  dlg.inputEl    = document.getElementById("dlg-input");
  dlg.input2Wrap = document.getElementById("dlg-input2-wrap");
  dlg.input2Label= document.getElementById("dlg-input2-label");
  dlg.input2El   = document.getElementById("dlg-input2");
  dlg.errorEl    = document.getElementById("dlg-error");
  dlg.okBtn      = document.getElementById("dlg-ok");
  dlg.cancelBtn  = document.getElementById("dlg-cancel");
  dlg.modal.addEventListener("click", (e) => {
    if (e.target === dlg.modal && dlg._active && dlg._active.dismissOnBackdrop) {
      resolve(null);
    }
  });
  dlg.cancelBtn.addEventListener("click", () => resolve(null));
  dlg.okBtn.addEventListener("click", () => onOk());
  for (const el of [dlg.inputEl, dlg.input2El]) {
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); onOk(); }
    });
  }
  document.addEventListener("keydown", (e) => {
    if (dlg.modal.hidden) return;
    if (e.key === "Escape") resolve(null);
  });
}

export function alertModal(message, opts = {}) {
  return open({ title: opts.title || t("dlg.notice"), body: message, kind: "alert", okLabel: opts.okLabel || t("dlg.ok") });
}

export function confirmModal(message, opts = {}) {
  return open({
    title: opts.title || t("dlg.confirm"),
    body: message,
    kind: "confirm",
    okLabel: opts.okLabel || t("dlg.ok"),
    cancelLabel: opts.cancelLabel || t("dlg.cancel"),
  });
}

export function promptModal(message, opts = {}) {
  return open({
    title: opts.title || t("dlg.input"),
    body: message,
    kind: "prompt",
    inputLabel: opts.inputLabel || "",
    inputValue: opts.inputValue || "",
    input2Label: opts.input2Label,
    input2Value: opts.input2Value,
    mustMatch: opts.mustMatch,
    okLabel: opts.okLabel || t("dlg.ok"),
    cancelLabel: opts.cancelLabel || t("dlg.cancel"),
  });
}

init();
