# Zabbix template

[`eneverre-nvr-by-http.yaml`](eneverre-nvr-by-http.yaml) monitors an Eneverre
server through its admin snapshot endpoint `GET /api/status`, using a
read-only **service token** (`status:read`). Everything is configured with
four macros — host, scheme, port and token — and no agent or script is
installed on the NVR.

The template is exported in the **7.0** format (validated against Zabbix
7.0's import schema), so it imports as-is on 7.0.x. On an older server, import
fails on the format version — export it from a 7.0 instance, or hand-edit
`version` down and move the top-level `triggers` block accordingly.

## Import

*Data collection → Templates → Import*, pick the YAML file, leave the default
*Create new / Update existing* rules. It lands in the **Templates/Applications**
group as **Eneverre NVR by HTTP**.

Then link it to the host that runs Eneverre and set the macros
(*Host → Macros → Inherited and host macros*):

| Macro                 | Default       | What it is                                              |
| --------------------- | ------------- | ------------------------------------------------------- |
| `{$ENEVERRE.HOST}`    | `{HOST.CONN}` | Host or IP serving the API — defaults to the interface   |
| `{$ENEVERRE.PORT}`    | `8080`        | API port (`443` behind a TLS reverse proxy)              |
| `{$ENEVERRE.SCHEME}`  | `http`        | `http` or `https`                                       |
| `{$ENEVERRE.TOKEN}`   | *(empty)*     | Service token with `status:read` — set as **Secret text** |

Two things to get right:

 * **Create a service token, not a user.** As an admin, `POST
   /api/service-tokens` with `{"name":"zabbix","scopes":["status:read"],
   "expires_in_days":365}` and copy the `token` value from the response. It is
   shown exactly once. The token is read-only, scoped to `/api/status`, and
   can be revoked without touching any user account.
 * **A token is not a password.** There is no HTTP Basic and no password
   throttle on this path. `401` means the token is wrong, revoked, or expired;
   `403` means it lacks `status:read`. Mint a replacement token and update the
   macro if it expires.

Everything else has a working default; the thresholds are
`{$ENEVERRE.STATUS.INTERVAL}` (poll, `1m`), `{$ENEVERRE.NODATA.TIMEOUT}`
(`5m`), `{$ENEVERRE.UPTIME.MIN}` (`10m`), `{$ENEVERRE.CAMERA.OFFLINE.TIME}`
(`5m`) and `{$ENEVERRE.STORAGE.PFREE.WARN}` / `.CRIT` (`10` / `5` percent).

## What it collects

One HTTP agent item polls `/api/status` and every other item is dependent on
it, so a host costs exactly **one request per interval** no matter how many
cameras it has.

 * **Service** — version (with a *version changed* event), uptime (with a
   *restarted* event), global recording toggle.
 * **Totals** — cameras configured / connected / recording / in privacy /
   disabled, plus *cameras in service but offline*: enabled, not paused by
   privacy or by a schedule, and still not connected.
 * **Storage** — record dir, total / free / used bytes, free percent, the
   engine's `min_free_bytes` threshold and its low-space alert (see
   [`../../MEDIA.md`](../../MEDIA.md) → Low-disk safety).
 * **Per camera**, discovered from the snapshot's `cameras` array
   (`{#CAMERA.ID}` / `{#CAMERA.NAME}`) — connected, recording, live viewers,
   privacy, off-hours, in service. Filter what is discovered with
   `{$ENEVERRE.CAMERA.NAME.MATCHES}` / `.NOT_MATCHES`.

Triggers: API not answering, service restarted, version changed, recording
globally disabled, low disk space (both the engine's own alert and the percent
thresholds), and per camera *disconnected* / *not recording*. The camera
triggers deliberately ignore a camera that is disabled, in privacy mode or
off-hours — those are operator decisions, not faults — and *not recording* only
fires while global recording is on.

Storage items stay empty rather than unsupported when recording is disabled:
`/api/status` omits the `storage` block entirely, and the preprocessing step
discards the value.

## Grafana

[`grafana-eneverre-nvr.json`](grafana-eneverre-nvr.json) is a dashboard that
reads the same items through Grafana's Zabbix datasource
(`alexanderzobnin-zabbix-datasource`, install it first — it also ships the
Problems panel the dashboard uses).

*Dashboards → New → Import*, paste the file, save. Nothing is hard-coded to one
server: the datasource, host group and host are dashboard variables, so pick
them from the pickers at the top. Re-importing the same file updates the
dashboard in place (uid `eneverre-nvr`).

 * **Service** — uptime, version, global recording toggle, cameras total /
   connected / offline, and the host's current Zabbix problems.
 * **Cameras** — one state timeline row per discovered camera for connected,
   recording, paused (privacy or off-hours) and live viewers. Rows are named
   after the camera, not the item.
 * **Storage** — free-space gauge, free / used / purge threshold, the engine's
   low-space alert, the volume over time and the recording directory.

Panels select items **by name** (the per-camera ones by regex over
`Camera [...]: <field>`), so renaming an item in the Zabbix template silently
empties the matching panel. Keep the two files in step.

## Prometheus instead

If you already scrape Prometheus, `/api/metrics` exposes the same engine
internals in far more detail (and `/api/metrics/json` the JSON flavour). It is
open to a scraper on loopback without auth — see `[server] metrics` in
[`../README.md`](../README.md). This template exists for the shops where Zabbix
is the only monitoring system.
