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

// formatBytes renders a byte count with a base-1024 unit (B/KiB/MiB/GiB/TiB),
// one decimal for the fractional units. Non-finite or negative input yields
// "—" so callers can pass a possibly-missing figure straight through.
export function formatBytes(n) {
  if (typeof n !== "number" || !Number.isFinite(n) || n < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  // Whole bytes read cleaner without a decimal; scaled units keep one.
  const s = i === 0 ? String(v) : v.toFixed(1);
  return `${s} ${units[i]}`;
}

// formatUptime turns a seconds count into a compact "3d 4h", "4h 12m" or
// "12m" / "45s" string. Used by the server-status screen.
export function formatUptime(seconds) {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds < 0) return "—";
  const s = Math.floor(seconds);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${s}s`;
}
