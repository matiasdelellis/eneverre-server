export const TOKEN_KEY = "eneverre.token";
export const USER_KEY = "eneverre.user";
export const VIEW_KEY = "eneverre.view";
export const SIDEBAR_KEY = "eneverre.sidebar";
export const WALL_FILTER_KEY = "eneverre.wallFilter";
export const THEME_KEY = "eneverre.theme";
export const USERCODE_KEY = "eneverre.pendingUsercode";
export const USERCODE_NAME_KEY = "eneverre.pendingUsercodeName";
export const PTZ_MODAL_POS_KEY = "eneverre.ptzModalPos";

export function get(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

export function set(key, value) {
  try { localStorage.setItem(key, value); } catch {}
}

export function remove(key) {
  try { localStorage.removeItem(key); } catch {}
}

export function sessionGet(key) {
  try { return sessionStorage.getItem(key); } catch { return null; }
}

export function sessionSet(key, value) {
  try { sessionStorage.setItem(key, value); } catch {}
}

export function sessionRemove(key) {
  try { sessionStorage.removeItem(key); } catch {}
}

export function loadJson(key) {
  const raw = get(key);
  if (!raw) return null;
  try { return JSON.parse(raw); } catch { return null; }
}

export function saveJson(key, value) {
  set(key, JSON.stringify(value));
}
