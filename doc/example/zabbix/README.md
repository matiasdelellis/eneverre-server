# Zabbix template

[`eneverre-nvr-by-http.yaml`](eneverre-nvr-by-http.yaml) monitors an Eneverre
server through its admin snapshot endpoint `GET /api/status`, using HTTP Basic.
Everything is configured with three macros — host, user, password — and no
agent or script is installed on the NVR.

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
| `{$ENEVERRE.USER}`    | `admin`       | Username for HTTP Basic                                  |
| `{$ENEVERRE.PASSWORD}`| *(empty)*     | Password — set it on the host as **Secret text**         |

Two things to get right:

 * **The account must be an admin.** `/api/status` answers `403` for a
   non-admin user, so the master item goes unsupported with a 403 body. A
   read-only operator account is not enough.
 * **Wrong passwords are throttled.** Repeated `401`s from the same source trip
   the server's failed-password throttle (`429` with `Retry-After`), which is
   the same one that guards the login form — so a typo in
   `{$ENEVERRE.PASSWORD}` is visible as `429`, not just `401`.

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
