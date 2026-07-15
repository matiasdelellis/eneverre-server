// Reflects a camera's live-stream connection state as a colored dot on every
// element carrying `.cam-status-dot[data-cam="<id>"]` — currently the wall
// tile and the sidebar thumbnail. Reads fresh translations at each call so
// the labels update on language switch.
import { t } from "../i18n.js";

export function setCamStatus(camId, status) {
  const labels = { online: t("online"), offline: t("offline"), connecting: t("connecting") };
  const dots = document.querySelectorAll(`.cam-status-dot[data-cam="${CSS.escape(camId)}"]`);
  for (const d of dots) {
    d.classList.remove("online", "offline", "connecting");
    d.classList.add(status);
    d.title = labels[status] || "";
    d.setAttribute("aria-label", d.title);
  }
}
