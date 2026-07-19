// Eneverre frontend entry. Vanilla JS, no build step; modules below
// are served directly from go/static/js/ and resolved by the browser.
// The boot order below mirrors the original top-down init in app.js.

import { token } from "./js/api.js";
import { applyI18n, getLang } from "./js/i18n.js";
import { hydrateIcons } from "./js/ui/icons.js";
import { initLogin, showLogin } from "./js/views/login.js";
import { initForcePasswordChange } from "./js/views/force-password.js";
import { initTheme } from "./js/ui/theme.js";
import { initPasswordReveal } from "./js/ui/password.js";
import { initUserMenu } from "./js/ui/user-menu.js";
import { initAppShell, showApp } from "./js/views/app-shell.js";
import { initDeviceAuth } from "./js/views/device-auth.js";
import { initSidebar } from "./js/views/sidebar.js";
import { initPtz } from "./js/views/ptz.js";
import { initTalk } from "./js/views/talk.js";
import { initUsers } from "./js/views/users.js";
import { initCameras } from "./js/views/cameras.js";
import { initStatus } from "./js/views/status.js";
import { initWall } from "./js/views/wall.js";
import { setupPlaybackBar, initPlaybackKeys } from "./js/views/playback.js";
import { initHelp } from "./js/ui/help.js";
import { initUpgradePrompt } from "./js/views/upgrade-prompt.js";

// Fill static [data-icon] buttons/spans from the icon set before wiring up
// handlers, so the markup carries no duplicated SVG path data.
hydrateIcons();
// Fill static [data-i18n*] markup for the detected/saved language before any
// view renders, and reflect the language on <html lang>.
document.documentElement.lang = getLang();
applyI18n();
initTheme();
initPasswordReveal();
initUserMenu();
initAppShell();
initDeviceAuth();
initSidebar();
initPtz();
initTalk();
initUsers();
initCameras();
initStatus();
initWall();
initLogin();
initForcePasswordChange();
setupPlaybackBar();
initPlaybackKeys();
initHelp();
// Non-blocking: shows a bottom banner on Android (TV or phone) when the
// server has a build available. Fire-and-forget by design.
initUpgradePrompt();

if (token()) showApp();
else showLogin();
