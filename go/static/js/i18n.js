// Lightweight i18n engine for the vanilla-JS frontend. No build step, no deps:
// a flat key -> string dictionary per language, a t() lookup, and an
// attribute-driven pass that fills static markup in index.html.
//
// The dictionaries live one-per-language under ./i18n/ and are imported
// statically below, so everything is resolved by the browser at load and
// stays synchronous (applyI18n() runs at boot with no async timing).
//
// Adding a language = create ./i18n/<code>.js (copy en.js, translate the
// values), import it here, and add it to `dict` + `LANG_NAMES`.
// Adding a string = add the key to EVERY ./i18n/*.js and reference it via
// t("key") in JS, or a data-i18n* attribute in the HTML (see applyI18n).
import { get, set, LANG_KEY } from "./util/storage.js";
import en from "./i18n/en.js";
import es from "./i18n/es.js";

const dict = { en, es };

// Endonyms — each language named in itself, so the same in every UI language.
const LANG_NAMES = {
  en: "English",
  es: "Español",
};

const supported = Object.keys(dict);

export function langName(code) {
  return LANG_NAMES[code] || code;
}

function detect() {
  const saved = get(LANG_KEY);
  if (saved && supported.includes(saved)) return saved;
  const nav = (navigator.language || "en").slice(0, 2).toLowerCase();
  return supported.includes(nav) ? nav : "en";
}

let lang = detect();

export function getLang() {
  return lang;
}

export function getSupportedLangs() {
  return supported.slice();
}

// Switch language, persist it, and re-run the static pass so already-rendered
// markup updates in place. Dynamic views re-read t() on their next render.
export function setLang(next) {
  if (!supported.includes(next) || next === lang) return;
  lang = next;
  set(LANG_KEY, next);
  document.documentElement.lang = lang;
  applyI18n();
}

// Translate a key. Falls back to English, then to the key itself so a missing
// translation is visible rather than blank. `vars` fills {name}-style holes.
export function t(key, vars) {
  let s = (dict[lang] && dict[lang][key]) ?? dict.en[key] ?? key;
  if (vars) {
    for (const k in vars) s = s.replaceAll("{" + k + "}", vars[k]);
  }
  return s;
}

// Maps each data-i18n* attribute to how its value is applied. `data-i18n`
// replaces textContent, so use it only on elements whose whole content is
// text (wrap a label's text in a <span> when it also holds an <input>).
const ATTR_MAP = {
  "data-i18n": (el, v) => { el.textContent = v; },
  "data-i18n-html": (el, v) => { el.innerHTML = v; },
  "data-i18n-placeholder": (el, v) => { el.setAttribute("placeholder", v); },
  "data-i18n-title": (el, v) => { el.setAttribute("title", v); },
  "data-i18n-aria-label": (el, v) => { el.setAttribute("aria-label", v); },
};

// Fill every element carrying a data-i18n* attribute under `root`. Safe to
// re-run (idempotent) — called at boot and again on each language switch.
export function applyI18n(root = document) {
  for (const attr in ATTR_MAP) {
    root.querySelectorAll("[" + attr + "]").forEach((el) => {
      ATTR_MAP[attr](el, t(el.getAttribute(attr)));
    });
  }
}
