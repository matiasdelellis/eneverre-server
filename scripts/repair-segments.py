#!/usr/bin/env python3
"""Repair fMP4 segments whose last sample was stretched over a camera stall.

The recorder writes each sample's duration as the delta to the next sample. When
a camera stops sending frames for minutes and later resumes, the last sample
before the gap gets that whole gap as its duration: the segment claims to be
minutes long while it only holds a second or two of media. Players then seek
into the stretched sample and show nothing.

This tool rewrites, in place, the duration metadata only:

  * the stretched sample's duration in its `trun` (the previous sample's
    duration, or one frame when there is none),
  * the `mvhd` duration of the segment's `moov`,
  * the segment's `duration_ns` in the recordings index.

Audio recorded past the end of the video (the camera stopped sending video but
kept its audio flowing) is kept: the segment still lasts as long as its longest
track, and playback fills the span without video with a placeholder frame.

It does NOT touch the media payload and does not need ffmpeg. Run it with the
server stopped (or at least with the recorder not writing the files it repairs).

Usage:
  # one file, dry run
  ./repair-segments.py --file segment.mp4 --dry-run

  # one file, write
  ./repair-segments.py --file segment.mp4

  # every segment the index says is longer than 3 minutes
  ./repair-segments.py --db /path/index.db --min-duration 180 --dry-run
  ./repair-segments.py --db /path/index.db --min-duration 180
"""

import argparse
import mmap
import os
import shutil
import sqlite3
import struct
import sys
import time

# A last-sample duration above this is a stall, not a frame interval.
DEFAULT_GAP_SECONDS = 5.0
# Fallback duration for a track whose stretched sample is its only one, or whose
# previous sample is implausibly short to stand for a frame interval.
FALLBACK_FPS = 25.0
# A previous duration below this (in seconds) is a timing glitch, not a frame
# interval (one field camera has a 1 ms frame right before the stall).
MIN_FRAME_SECONDS = 1.0 / 120

VIDEO_HANDLERS = ("vide",)


def boxes(data, start, end):
    """Yield (offset, size, type, header_size) for the boxes in [start, end)."""
    p = start
    while p + 8 <= end:
        size, typ = struct.unpack(">I4s", data[p:p + 8])
        hdr = 8
        if size == 1:
            size = struct.unpack(">Q", data[p + 8:p + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - p
        if size < hdr or p + size > end:
            return
        yield p, size, typ.decode("latin1"), hdr
        p += size


CONTAINERS = {"moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "mvex", "edts"}


def walk(data, start, end, path=()):
    """Yield (path, offset, size, type, header_size) recursively."""
    for p, size, typ, hdr in boxes(data, start, end):
        yield path + (typ,), p, size, typ, hdr
        if typ in CONTAINERS:
            yield from walk(data, p + hdr, p + size, path + (typ,))


def u32(data, off):
    return struct.unpack(">I", data[off:off + 4])[0]


def u64(data, off):
    return struct.unpack(">Q", data[off:off + 8])[0]


def parse_tracks(data):
    """Map track id -> {timescale, handler} from the moov."""
    tracks = {}
    current = None
    for path, p, size, typ, hdr in walk(data, 0, len(data)):
        if typ == "trak":
            current = {}
        elif typ == "tkhd" and current is not None:
            version = data[p + hdr]
            current["id"] = u32(data, p + hdr + (20 if version == 1 else 12))
        elif typ == "mdhd" and current is not None:
            version = data[p + hdr]
            current["timescale"] = u32(data, p + hdr + (20 if version == 1 else 12))
        elif typ == "hdlr" and current is not None:
            current["handler"] = data[p + hdr + 8:p + hdr + 12].decode("latin1")
        if current is not None and "id" in current and "timescale" in current \
                and "handler" in current:
            tracks[current["id"]] = {
                "timescale": current["timescale"],
                "handler": current["handler"],
            }
            current = None
    return tracks


def parse_tfhd(data, p, hdr):
    flags = int.from_bytes(data[p + hdr + 1:p + hdr + 4], "big")
    off = p + hdr + 4
    track = u32(data, off)
    off += 4
    if flags & 0x1:
        off += 8
    if flags & 0x2:
        off += 4
    default_duration = u32(data, off) if flags & 0x8 else None
    return track, default_duration


def trun_entries(data, p, hdr, default_duration):
    """Yield (duration, offset_of_duration_or_None, composition_offset) per sample."""
    version = data[p + hdr]
    flags = int.from_bytes(data[p + hdr + 1:p + hdr + 4], "big")
    count = u32(data, p + hdr + 4)
    off = p + hdr + 8
    if flags & 0x1:  # data_offset_present
        off += 4
    if flags & 0x4:  # first_sample_flags_present
        off += 4
    entry = 0
    cto_at = None
    for bit in (0x100, 0x200, 0x400, 0x800):
        if flags & bit:
            if bit == 0x800:
                cto_at = entry
            entry += 4
    if entry == 0:
        return
    has_duration = bool(flags & 0x100)
    for i in range(count):
        dur_off = off if has_duration else None
        duration = u32(data, off) if has_duration else (default_duration or 0)
        cto = 0
        if cto_at is not None:
            fmt = ">i" if version == 1 else ">I"
            cto = struct.unpack(fmt, data[off + cto_at:off + cto_at + 4])[0]
        yield duration, dur_off, cto
        off += entry


def parse_tfdt(data, p, hdr):
    if data[p + hdr] == 1:
        return u64(data, p + hdr + 4)
    return u32(data, p + hdr + 4)


def scan(data, tracks):
    """Collect per-track fragments in file order: {track: [(tfdt, entries)]}."""
    frags = {tid: [] for tid in tracks}
    for path, p, size, typ, hdr in walk(data, 0, len(data)):
        if typ != "traf":
            continue
        track = None
        default_duration = None
        base = 0
        entries = []
        for cpath, cp, csize, ctyp, chdr in walk(data, p + hdr, p + size):
            if ctyp == "tfhd":
                track, default_duration = parse_tfhd(data, cp, chdr)
            elif ctyp == "tfdt":
                base = parse_tfdt(data, cp, chdr)
            elif ctyp == "trun":
                entries.extend(trun_entries(data, cp, chdr, default_duration))
        if track is not None:
            frags.setdefault(track, []).append((base, entries))
    return frags


def mvhd_duration_field(data):
    """(offset, struct format) of the moov/mvhd duration."""
    for path, p, size, typ, hdr in walk(data, 0, len(data)):
        if typ == "mvhd" and path[-2:-1] == ("moov",):
            if data[p + hdr] == 1:
                return p + hdr + 24, ">Q"
            return p + hdr + 16, ">I"
    return None, None


def repair_file(path, dry_run=False, gap_seconds=DEFAULT_GAP_SECONDS, verbose=True):
    # Mapped, not read: a long 4K segment can be hundreds of MB, and only a few
    # 4-byte fields are ever patched (in place; each patch is one aligned word).
    with open(path, "rb" if dry_run else "r+b") as f:
        access = mmap.ACCESS_READ if dry_run else mmap.ACCESS_WRITE
        with mmap.mmap(f.fileno(), 0, access=access) as data:
            return _repair_mapped(path, data, dry_run, gap_seconds, verbose)


def _repair_mapped(path, data, dry_run, gap_seconds, verbose):
    tracks = parse_tracks(data)
    frags = scan(data, tracks)
    if not tracks or not any(frags.values()):
        if verbose:
            print(f"  skip (no tracks): {path}")
        return None

    changed = False
    end_ns = 0
    for tid, track_frags in frags.items():
        track = tracks.get(tid)
        if not track or not track_frags:
            continue
        timescale = track["timescale"]
        limit = int(gap_seconds * timescale)
        floor = MIN_FRAME_SECONDS * timescale
        video = track["handler"] in VIDEO_HANDLERS
        previous = 0  # last sane duration, carried across fragments
        capped = 0
        end = 0  # track end in ticks from the segment start
        n = sum(len(e) for _, e in track_frags)
        i = 0
        for base, entries in track_frags:
            dts = base  # tfdt: the fragment's position in the segment
            for duration, off, cto in entries:
                i += 1
                if duration > limit and off is not None:
                    # The stretched sample sits right after the last real frame:
                    # give it that frame's interval, or one frame at FALLBACK_FPS.
                    new_duration = previous if previous > 0 else int(timescale / FALLBACK_FPS)
                    new_duration = max(new_duration, 1)
                    if verbose:
                        print(f"  track {tid} ({track['handler']}): sample {i}/{n} "
                              f"{duration / timescale:.1f}s -> {new_duration / timescale:.3f}s")
                    if not dry_run:
                        struct.pack_into(">I", data, off, new_duration)
                    duration = new_duration
                    capped += 1
                    changed = True
                elif duration >= floor:
                    previous = duration
                # A segment ends where its last sample ends; video is shown
                # until its presentation end (DTS + composition offset).
                end = max(end, dts + duration, dts + cto + duration if video else 0)
                dts += duration
        if capped and verbose:
            print(f"  track {tid}: {capped} stretched sample(s)")
        end_ns = max(end_ns, int(end * 1_000_000_000 / timescale))

    if not changed:
        if verbose:
            print(f"  ok (no stretched sample): {path}")
        return None

    mvhd_off, mvhd_fmt = mvhd_duration_field(data)
    if mvhd_off is not None and not dry_run:
        struct.pack_into(mvhd_fmt, data, mvhd_off, int(end_ns / 1_000_000))
    if not dry_run:
        data.flush()

    if verbose:
        print(f"  media duration: {end_ns / 1e9:.3f}s"
              + (" (dry run)" if dry_run else ""))
    return end_ns


def repair_db(db_path, min_duration_s, dry_run, gap_seconds):
    backup = f"{db_path}.bak-{time.strftime('%Y%m%d-%H%M%S')}"
    if not dry_run:
        src = sqlite3.connect(db_path)
        dst = sqlite3.connect(backup)
        src.backup(dst)
        dst.close()
        src.close()
        print(f"index backup: {backup}")

    con = sqlite3.connect(db_path)
    rows = con.execute(
        "SELECT fpath, duration_ns FROM segments WHERE duration_ns > ? ORDER BY start_ns",
        (int(min_duration_s * 1_000_000_000),),
    ).fetchall()
    print(f"{len(rows)} segment(s) over {min_duration_s}s")
    repaired = 0
    for fpath, duration_ns in rows:
        if not os.path.isfile(fpath):
            print(f"  missing: {fpath}")
            continue
        print(f"{fpath} (index {duration_ns / 1e9:.1f}s)")
        new_ns = repair_file(fpath, dry_run=dry_run, gap_seconds=gap_seconds)
        if new_ns is not None:
            repaired += 1
            if not dry_run:
                con.execute("UPDATE segments SET duration_ns=? WHERE fpath=?", (new_ns, fpath))
    if not dry_run:
        con.commit()
    con.close()
    print(f"{repaired} segment(s) repaired" + (" (dry run)" if dry_run else ""))


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--file", help="repair a single segment file")
    ap.add_argument("--db", help="recordings index.db")
    ap.add_argument("--min-duration", type=float, default=180.0,
                    help="only segments whose index duration exceeds this (seconds, default 180)")
    ap.add_argument("--gap-seconds", type=float, default=DEFAULT_GAP_SECONDS,
                    help="a last sample longer than this is a stall (default 5)")
    ap.add_argument("--dry-run", action="store_true", help="print without writing")
    args = ap.parse_args()

    if args.file:
        print(f"{args.file}")
        repair_file(args.file, dry_run=args.dry_run, gap_seconds=args.gap_seconds)
    elif args.db:
        repair_db(args.db, args.min_duration, args.dry_run, args.gap_seconds)
    else:
        ap.error("use --file or --db")


if __name__ == "__main__":
    main()
