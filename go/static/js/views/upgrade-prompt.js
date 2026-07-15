// Detects whether the browser is on an Android device and, if so, fetches the
// latest build for the matching track from the API. When the server has a
// build available, shows a small dismissible banner at the bottom of the
// viewport that links to the APK so the user can install it via the system
// package installer.
//
// The two endpoints (GET /api/app/{tv,phone}/update) are anonymous; the
// banner is shown regardless of whether the user is logged in (they might
// want to install the app before logging in on a fresh TV/phone).
//
// The feature is silent: any non-200 (204, 503, network error) is treated as
// "no banner" and never surfaces to the user. The detection itself runs in
// a few regex passes and is essentially free.

import { sessionGet, sessionSet } from "../util/storage.js";
import { escapeHtml } from "../util/dom.js";
import { icon } from "../ui/icons.js";

const DISMISS_KEY = "eneverre.upgradePrompt.dismissedVersionCode";

// TV signals in the User-Agent. Covers Android TV (Leanback), Amazon Fire TV
// (AFT*), Sony BRAVIA, generic Smart TV labels, Chromecast (when running
// the web platform) and the "GoogleTV" rebranding. Order is irrelevant
// since the regex is a single alternation.
const TV_SIGNAL = /(?:Android\s*TV|;\s*TV\s|AFT(?:M|ST|B|MR)|Fire\s*TV|GoogleTV|Chromecast|SmartTV|BRAVIA|ADT-?\d|;\s*CrKey\b)/i;

const ANDROID = /Android/i;

/**
 * detectAndroidKind inspects navigator.userAgent and returns
 *   - "tv"    for Android TV / Fire TV / similar set-top boxes
 *   - "phone" for Android phones and tablets
 *   - null    for everything else (desktop, iOS, etc.)
 *
 * Heuristic, by design: the same APK is offered to "phones" and "tablets",
 * and the API exposes a single `phone` track for both. Android TV is the
 * one case where the package, signing key and version code are different
 * from the phone, so we have to tell them apart.
 */
export function detectAndroidKind(ua = (typeof navigator !== "undefined" && navigator.userAgent) || "") {
  if (!ANDROID.test(ua)) return null;
  if (TV_SIGNAL.test(ua)) return "tv";
  return "phone";
}

/**
 * fetchManifest queries the API for the current build of a track. Returns
 * the manifest object on 200, or null on 204/404/503/network error. The
 * caller never throws — the banner is best-effort.
 */
async function fetchManifest(kind) {
  try {
    const r = await fetch(`/api/app/${kind}/update`, {
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
    if (r.status !== 200) return null;
    const m = await r.json();
    if (!m || !m.url || !m.versionName || typeof m.versionCode !== "number") return null;
    return m;
  } catch {
    return null;
  }
}

function shouldDismiss(manifest) {
  // Per-session dismiss keyed by versionCode. A new release re-prompts even
  // within the same tab. localStorage would survive across sessions; we
  // intentionally pick sessionStorage so the prompt can re-appear the next
  // time the user reopens the page.
  return sessionGet(DISMISS_KEY) === String(manifest.versionCode);
}

function buildBanner(manifest, kind) {
  const el = document.createElement("div");
  el.className = "upgrade-prompt";
  el.setAttribute("role", "status");
  el.setAttribute("aria-live", "polite");

  const iconName = kind === "tv" ? "tv" : "smartphone";
  const label = kind === "tv" ? "Android TV" : "phone";

  const text = document.createElement("div");
  text.className = "upgrade-prompt-text";
  const iconEl = document.createElement("span");
  iconEl.className = "upgrade-prompt-icon";
  iconEl.setAttribute("aria-hidden", "true");
  iconEl.innerHTML = icon(iconName);
  const main = document.createElement("span");
  main.className = "upgrade-prompt-main";
  const title = document.createElement("strong");
  title.textContent = `Eneverre for ${label}`;
  const ver = document.createElement("span");
  ver.className = "upgrade-prompt-version";
  ver.textContent = `v${manifest.versionName}`;
  main.append(title, ver);
  text.append(iconEl, main);

  const actions = document.createElement("div");
  actions.className = "upgrade-prompt-actions";
  const dl = document.createElement("a");
  dl.className = "upgrade-prompt-dl primary";
  dl.href = manifest.url;
  // `download` hints the browser to save rather than navigate; on Android
  // it triggers the system download manager which then offers the package
  // installer for .apk files.
  dl.setAttribute("download", manifest.apkFilename || "");
  dl.textContent = "Download";
  const dismiss = document.createElement("button");
  dismiss.type = "button";
  dismiss.className = "upgrade-prompt-dismiss ghost";
  dismiss.setAttribute("aria-label", "Dismiss");
  dismiss.innerHTML = icon("x");
  actions.append(dl, dismiss);

  el.append(text, actions);

  dismiss.addEventListener("click", () => {
    sessionSet(DISMISS_KEY, String(manifest.versionCode));
    el.remove();
  });

  return el;
}

/**
 * initUpgradePrompt runs once on boot. It is fire-and-forget: callers do not
 * await it. The banner appears when (and only when) the user is on an
 * Android device AND the server has a build available AND the user has not
 * dismissed this exact versionCode in the current session.
 */
export async function initUpgradePrompt() {
  const kind = detectAndroidKind();
  if (!kind) return;
  const manifest = await fetchManifest(kind);
  if (!manifest) return;
  if (shouldDismiss(manifest)) return;
  const banner = buildBanner(manifest, kind);
  document.body.appendChild(banner);
  // The CSS transition depends on the element being attached first; force
  // a layout flush before adding the "shown" class so the slide-in plays.
  // Reading offsetWidth is the canonical way to do this.
  void banner.offsetWidth;
  banner.classList.add("upgrade-prompt-shown");
}
