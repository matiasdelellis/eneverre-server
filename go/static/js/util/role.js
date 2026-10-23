// Client-side mirror of the server's role capabilities. The UI uses this to
// hide controls the role cannot use; the server still enforces every request.
import { loadJson, USER_KEY } from "./storage.js";

const CAPS = {
  admin: new Set(["playback", "live", "ptz", "privacy", "talk", "status", "metrics"]),
  user: new Set(["playback", "live", "ptz", "privacy", "talk"]),
  viewer: new Set(["live"]),
  monitor: new Set(["status", "metrics"]),
};

export function currentRole() {
  const u = loadJson(USER_KEY);
  return u && u.role ? u.role : null;
}

export function can(cap) {
  const role = currentRole();
  return !!(role && CAPS[role] && CAPS[role].has(cap));
}
