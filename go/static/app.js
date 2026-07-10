// Eneverre frontend entry. Vanilla JS, no build step; modules below
// are served directly from go/static/js/ and resolved by the browser.
// The boot order below mirrors the original top-down init in app.js.

import { token } from "./js/api.js";
import { initLogin, showLogin } from "./js/views/login.js";
import { initTheme } from "./js/ui/theme.js";
import { initPasswordReveal } from "./js/ui/password.js";
import { initUserMenu } from "./js/ui/user-menu.js";
import { initAppShell, showApp } from "./js/views/app-shell.js";
import { initDeviceAuth } from "./js/views/device-auth.js";
import { initSidebar } from "./js/views/sidebar.js";
import { initPtz } from "./js/views/ptz.js";
import { initUsers } from "./js/views/users.js";
import { initWall } from "./js/views/wall.js";
import { setupPlaybackBar, initPlaybackKeys } from "./js/views/playback.js";
import { initHelp } from "./js/ui/help.js";
import { initUpgradePrompt } from "./js/views/upgrade-prompt.js";

initTheme();
initPasswordReveal();
initUserMenu();
initAppShell();
initDeviceAuth();
initSidebar();
initPtz();
initUsers();
initWall();
initLogin();
setupPlaybackBar();
initPlaybackKeys();
initHelp();
// Non-blocking: shows a bottom banner on Android (TV or phone) when the
// server has a build available. Fire-and-forget by design.
initUpgradePrompt();

if (token()) showApp();
else showLogin();
