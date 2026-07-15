import { $ } from "../util/dom.js";
import { set, saveJson, loadJson, USER_KEY, TOKEN_KEY } from "../util/storage.js";
import { api } from "../api.js";
import { t } from "../i18n.js";

export function finishBoot() {
  document.documentElement.classList.remove("booting");
  const boot = document.getElementById("boot");
  if (boot) boot.hidden = true;
}

export function showLogin() {
  $("#login").hidden = false;
  $("#app").hidden = true;
  const u = loadJson(USER_KEY);
  if (u) $("#login-form [name=username]").value = u.username || "";
  $("#login-form [name=password]").focus();
  finishBoot();
}

export function initLogin() {
  $("#login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const err = $("#login-error");
    err.hidden = true;
    const fd = new FormData(e.target);
    const submit = e.target.querySelector("button");
    submit.disabled = true;
    try {
      const data = await api("/api/auth/login", {
        method: "POST",
        body: JSON.stringify({
          username: fd.get("username"),
          password: fd.get("password"),
        }),
      });
      set(TOKEN_KEY, data.token);
      saveJson(USER_KEY, {
        username: data.username,
        first_name: data.first_name || null,
        last_name: data.last_name || null,
        role: data.role,
        is_admin: data.is_admin !== undefined ? data.is_admin : data.role === "admin",
        must_change_password: !!data.must_change_password,
      });
      // A flagged account (seeded admin, or one an admin required to reset)
      // must change its password before reaching the app. Prefill the current
      // password from what was just typed so the user only enters the new one.
      if (data.must_change_password) {
        const { showForcePasswordChange } = await import("./force-password.js");
        showForcePasswordChange(fd.get("password"));
        return;
      }
      // Dynamic import avoids the showApp() <-> showLogin() cycle
      // (app-shell imports showLogin for logout).
      const { showApp } = await import("./app-shell.js");
      showApp();
    } catch (e) {
      err.textContent = e.message || t("login.failed");
      err.hidden = false;
    } finally {
      submit.disabled = false;
    }
  });
}
