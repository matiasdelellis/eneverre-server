# Dynamic gap-fill plan (date/time in the "no recording" frame)

Status: **planned, not implemented.** Today the gap-fill in downloads (`/get`)
splices a **static** black frame captioned with a fixed message
(`[media] gap_message`, default `NO RECORDING`). This document plans making the
fill carry the **date/time of the missing recording** (e.g. the gap's time
range, or a running clock).

See [`doc/MEDIA.md`](MEDIA.md) for how gap-fill works today.

## Why it's not trivial

The current frame is generated **once per resolution+message** and cached
(`<cache_dir>/gapfill/<WxH>-<msghash>.h264`), then repeated 1/s across the gap.
Baking in the gap's timestamp makes the content **unique per gap**, so it can no
longer be cached across gaps — the design has to balance "useful info" against
"generation cost".

`seekAndMux` already has what's needed: at each gap, `gapStart = segmentEnd` and
`gapEnd = seg.Start` (both wall-clock).

## Design: placeholders in `gap_message`

Backwards-compatible; the message content auto-selects the mode:

| `gap_message` | Mode | Cost | Cache |
|---|---|---|---|
| `NO RECORDING` (no placeholder) | **static** (current) | 1 frame, once | by res+msg (exists) |
| `NO RECORDING\n{start} → {end}` | **per-gap frame** | 1 frame per gap | optional per-gap |
| `NO RECORDING\n{clock}` | **running clock** | clip per gap (N frames) | not cacheable |

Placeholders:
- `{start}`, `{end}`, `{date}` — substituted **once** with the gap's times → a
  static per-gap frame (repeated to fill, like today).
- `{clock}` — a clock that **advances** through the gap → requires a generated
  clip (one frame per tick), not a repeated single frame.

## Changes by component

1. **Config (`[media]`)**: extend the meaning of `gap_message` (placeholders).
   New optional keys:
   - `gap_time_format` — Go/strftime-style layout (default `2006-01-02 15:04:05`).
   - `gap_timezone` — `utc` (default, matches the RFC3339 API/index) or `local`.
   - `gap_clock_max` — cap (duration) above which `{clock}` degrades to a
     per-gap `{start} → {end}` frame (see edge cases).
2. **`seekAndMux`** (`internal/media/playback/server.go`): pass the gap's
   wall-clock `gapStart`/`gapEnd` to the filler resolver (already computed).
3. **`gapfill.go`**:
   - **Per-gap frame** (`{start}/{end}/{date}`): substitute the times into the
     message and reuse `ffmpegBlackFrame` (1 frame) + `writeBlackGap` (repeat).
     Only the text varies per gap → **reuses the whole existing machinery**.
   - **Running clock** (`{clock}`): a new function generates a **clip of
     duration = gap** with
     `ffmpeg -f lavfi -i color=black -vf "drawtext=text='%{pts\:gmtime\:<epoch>\:<fmt>}'"`
     (the clock advances with the PTS, starting at the gap's wall-clock), at a
     **low fps (1–2)**; convert to AVCC samples and splice them (with correct
     DTS) instead of repeating one frame.
4. **avc3 / muxing**: unchanged — generated samples carry in-band SPS and avc3
   is already enabled for gap-filled downloads.

## Caching

- **No placeholder** → still cached (cheap, current behavior).
- **Per-gap frame** → 1 frame per gap; optionally cache per-gap
  (key = `streamID` + `segNumber` of the boundary + resolution + format) so a
  re-download of the same gap reuses it. The configurable `cache_dir` already
  exists.
- **Running clock** → not cacheable (unique timestamps); ffmpeg runs per gap per
  download.

## Edge cases

- **Long gaps** (e.g. the 117-min outage seen in testing): running-clock mode
  would encode thousands of frames. Mitigation: `gap_clock_max` — above the
  threshold, degrade `{clock}` to a per-gap `{start} → {end}` static frame; or
  force fps=1. (Black + digits H264-compress to a few KB/frame, but a cap is
  still wise.)
- **Timezone**: default UTC (matches the index / RFC3339 timestamps elsewhere in
  the API); `local` optional (ffmpeg `localtime` uses the server TZ). Document
  which is shown.
- **ffmpeg unavailable**: fall back to the cached static frame, or to truncating
  at the gap (as today).
- **Escaping**: the per-gap frame keeps using `drawtext textfile=` (any chars).
  The running clock uses the `%{pts:gmtime:...}` expression inline in the
  filtergraph → escape the date format (`:` → `\:`).

## Work phases

1. **Placeholder parser + `{start}/{end}/{date}` substitution** (per-gap frame).
   Low effort, reuses `ffmpegBlackFrame`. Verifiable: `/get` across a gap shows
   the gap's timestamps burned in.
2. **`{clock}` running-clock mode** + `gap_clock_max` degradation for long gaps.
   Medium effort (new clip-generation function).
3. **Config** `gap_time_format` / `gap_timezone` / `gap_clock_max` + docs
   (openapi.yaml, MEDIA.md).

## Recommendation

Start with **Phase 1** (per-gap frame, `gap_message = "NO RECORDING\n{start} → {end}"`):
it satisfies "include the date/time of the missing recording" at **minimum cost
(1 frame per gap)**, reuses almost everything, and is cacheable per gap. The
**running clock (Phase 2)** stays an option for short gaps, auto-degrading on
long ones.
