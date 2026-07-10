// Reflects a camera's live-stream connection state as a colored dot on every
// element carrying `.cam-status-dot[data-cam="<id>"]` — currently the wall
// tile and the sidebar thumbnail. Dependency-free so both views and the MSE
// layer can import it without creating a cycle.

const LABELS = { online: "Online", offline: "Offline", connecting: "Connecting…" };

export function setCamStatus(camId, status) {
  const dots = document.querySelectorAll(`.cam-status-dot[data-cam="${CSS.escape(camId)}"]`);
  for (const d of dots) {
    d.classList.remove("online", "offline", "connecting");
    d.classList.add(status);
    d.title = LABELS[status] || "";
    d.setAttribute("aria-label", d.title);
  }
}
