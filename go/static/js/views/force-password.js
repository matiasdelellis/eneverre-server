import { $ } from "../util/dom.js";
import { loadJson, saveJson, USER_KEY } from "../util/storage.js";
import { api } from "../api.js";

// Mandatory password-change gate. Shown after login (or on reload) when the
// account carries the server's `must_change_password` flag — set for the
// seeded initial admin and whenever an admin flags an account. There is no
// cancel: the user cannot reach the app until the change succeeds (which
// clears the flag server-side in the same request).

export function showForcePasswordChange(currentPassword = "") {
  $("#login").hidden = true;
  $("#app").hidden = true;
  const screen = $("#force-pw");
  screen.hidden = false;
  const form = $("#force-pw-form");
  form.reset();
  $("#force-pw-error").hidden = true;
  // Coming straight from a successful login we already know the current
  // password, so prefill it and jump to the new-password field. On a reload
  // we don't have it, so ask for it.
  form.elements.current_password.value = currentPassword || "";
  (currentPassword ? form.elements.new_password : form.elements.current_password).focus();
  // Clear the boot overlay on the reload path (login.js owns finishBoot).
  import("./login.js").then(({ finishBoot }) => finishBoot());
}

export function initForcePasswordChange() {
  const form = $("#force-pw-form");
  if (!form) return;
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const err = $("#force-pw-error");
    err.hidden = true;
    const fd = new FormData(form);
    const newPw = fd.get("new_password");
    if (newPw !== fd.get("confirm_password")) {
      err.textContent = "New password and confirmation do not match.";
      err.hidden = false;
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
      const u = loadJson(USER_KEY);
      if (u) {
        u.must_change_password = false;
        saveJson(USER_KEY, u);
      }
      form.reset();
      $("#force-pw").hidden = true;
      // Dynamic import avoids a static app-shell <-> force-password cycle.
      const { showApp } = await import("./app-shell.js");
      showApp();
    } catch (e2) {
      err.textContent = e2.message || "Could not update password";
      err.hidden = false;
    } finally {
      submit.disabled = false;
    }
  });
}
