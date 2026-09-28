// Shared camera-thumbnail cache: one Map (camId -> { dataUrl, at }) plus the
// localStorage mirror (`thumb_<id>` / `thumbts_<id>`), used by both the
// sidebar (live-frame publishes + Thingino pulls) and the wall (poster on
// tile render). Before this lived only in sidebar.js, so every wall re-render
// (filter click, mode switch) re-fetched N JPEGs the sidebar already had.
//
// Ages: an entry older than maxAge is treated as stale and the caller should
// refetch. localStorage entries written before thumbts_<id> existed have no
// timestamp — treated as stale so the first read after upgrade refreshes once
// and re-stamps.

const cache = new Map(); // camId -> { dataUrl, at }
const TS_PREFIX = "thumbts_";
const DATA_PREFIX = "thumb_";

/** Read a cached thumbnail data URL, or null when missing/older than maxAgeMs. */
export function getCachedThumb(camId, maxAgeMs = Infinity) {
  const hit = cache.get(camId);
  if (hit && Date.now() - hit.at <= maxAgeMs) return hit.dataUrl;
  try {
    const dataUrl = localStorage.getItem(DATA_PREFIX + camId);
    if (!dataUrl) return null;
    const at = Number(localStorage.getItem(TS_PREFIX + camId)) || 0;
    if (Date.now() - at > maxAgeMs) return null;
    cache.set(camId, { dataUrl, at });
    return dataUrl;
  } catch {
    return null;
  }
}

/**
 * Store a thumbnail. `persist` writes through to localStorage (throttled by
 * the caller for high-frequency live-frame publishes); `at` lets a caller
 * backdate the entry (not used today, kept for symmetry).
 */
export function setCachedThumb(camId, dataUrl, { persist = true, at = Date.now() } = {}) {
  if (!camId || !dataUrl) return;
  cache.set(camId, { dataUrl, at });
  if (!persist) return;
  try {
    localStorage.setItem(DATA_PREFIX + camId, dataUrl);
    localStorage.setItem(TS_PREFIX + String(camId), String(at));
  } catch { /* quota: in-memory copy still serves this session */ }
}

/** Drop one camera's thumbnail (camera deleted, or a forced refresh). */
export function deleteCachedThumb(camId) {
  cache.delete(camId);
  try {
    localStorage.removeItem(DATA_PREFIX + camId);
    localStorage.removeItem(TS_PREFIX + camId);
  } catch {}
}

/** Drop everything (logout). Also removes any legacy thumb_* keys. */
export function clearThumbCache() {
  cache.clear();
  try {
    const doomed = [];
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (k && (k.startsWith(DATA_PREFIX) || k.startsWith(TS_PREFIX))) doomed.push(k);
    }
    for (const k of doomed) localStorage.removeItem(k);
  } catch {}
}
