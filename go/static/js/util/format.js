export function blankToNull(v) {
  if (v === null || v === undefined) return null;
  const s = String(v).trim();
  return s.length ? s : null;
}

export function displayName(u) {
  if (!u) return "";
  const n = [u.first_name, u.last_name].filter(Boolean).join(" ").trim();
  return n;
}

export function pad2(n) {
  return String(n).padStart(2, "0");
}
