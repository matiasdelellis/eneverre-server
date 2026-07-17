import { $ } from "../util/dom.js";
import { set, saveJson, loadJson, USER_KEY, TOKEN_KEY, REFRESH_KEY } from "../util/storage.js";
import { api } from "../api.js";
import { t, getSupportedLangs, getLang, setLang, langName } from "../i18n.js";

// Language picker at the foot of the login card — the user menu (the other
// place to switch language) only exists after signing in. A native <select>
// so it stays compact as languages are added. setLang() re-runs the static
// i18n pass, so the login text updates in place on change.
function buildLoginLangs() {
  const sel = document.getElementById("login-lang-select");
  if (!sel || sel.dataset.built) return;
  sel.dataset.built = "1";
  for (const code of getSupportedLangs()) {
    const opt = document.createElement("option");
    opt.value = code;
    opt.textContent = langName(code);
    sel.appendChild(opt);
  }
  sel.value = getLang();
  sel.addEventListener("change", () => setLang(sel.value));
}

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
  buildLoginLangs();
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
      // The refresh token keeps a wall left on a monitor logged in past the
      // access token's TTL: api() renews the pair on the first 401.
      if (data.refresh_token) set(REFRESH_KEY, data.refresh_token);
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
