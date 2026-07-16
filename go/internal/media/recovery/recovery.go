// Package recovery re-indexes recording segments that are present on disk but
// missing from the SQLite index — the footage a hard crash leaves behind.
//
// During normal operation a segment is added to the index only when it is
// closed (recorder.segment.close → engine.enqueueIndex). Graceful stops
// (disconnect, SIGTERM) finalize and index the in-progress segment too. But a
// hard crash — power loss, OOM-kill (SIGKILL), panic — skips that path: the
// segment's fMP4 parts are already on disk, yet no index row exists, so
// playback can't see it and the footage is effectively lost.
//
// This package recovers it. Every segment file carries an "mtxi" box in its
// init (StreamID, SegmentNumber, and the wall-clock start as NTP), so a single
// file yields everything the index needs; the segment's real duration is summed
// from its fMP4 fragments (the mvhd duration is never patched on a crashed file,
// so it can't be trusted).
//
// Cost is bounded and independent of corpus size. Orphans from a crash are
// always the newest segment(s): everything older was closed and indexed in
// order, and nothing is written while the process is down. So Recover anchors a
// window at the last indexed segment (the "watermark") and only walks FORWARD
// from there, directory by directory, stopping after a few consecutive
// directories yield no new orphans. A ten-day tree of one-minute files is never
// walked in full — only the handful of directories around the crash point.
package recovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/google/uuid"

	"eneverre/internal/media/index"
	"eneverre/internal/media/mtxi"
	"eneverre/internal/media/recstore"
)

const (
	// maxForwardScan hard-bounds the forward walk so a corrupt watermark can
	// never turn recovery into an unbounded scan. The real orphan span is at
	// most the index write-back backlog (a handful of segments); this is a
	// safety ceiling far above that.
	maxForwardScan = 24 * time.Hour

	// dryDirs stops the forward walk after this many consecutive directories
	// contribute no new orphan. Orphans sit right at/after the watermark, so a
	// short dry run means we've passed them (and everything ahead is the empty
	// stretch from crash to restart).
	dryDirs = 3

	// minBackMargin (and 2×segmentDuration, whichever is larger) is how far
	// BEFORE the watermark the scan starts, to catch an orphan whose start
	// slightly precedes an out-of-order indexed neighbour.
	minBackMargin = 2 * time.Minute
)

// Recover scans one camera's recordings for orphaned segments and inserts them
// into the index. It returns how many were recovered and their total duration.
// A nil logf is tolerated. Must run before the camera's recorder starts, so no
// live writer races the scan of this camera's directories.
func Recover(idx *index.Index, recordPath, camera string, segmentDuration time.Duration, logf func(string, ...any)) (recovered int, total time.Duration, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	back := 2 * segmentDuration
	if back < minBackMargin {
		back = minBackMargin
	}

	tl, err := idx.Timeline(camera)
	if err != nil {
		return 0, 0, fmt.Errorf("timeline: %w", err)
	}

	var from, hardTo time.Time
	now := time.Now()
	if tl.Count > 0 && !tl.End.IsZero() {
		from = tl.End.Add(-back)
	} else {
		// No watermark for this camera: only a first segment orphaned by a crash
		// moments after start could exist. Scan the recent window without
		// touching any history.
		from = now.Add(-maxForwardScan)
		if from.Before(now.Add(-1 * time.Hour)) {
			from = now.Add(-1 * time.Hour)
		}
	}
	hardTo = from.Add(back + maxForwardScan)

	// Files already indexed within the window — skipped without opening them.
	fromP, toP := from, hardTo
	indexedSegs, err := idx.Range(camera, &fromP, &toP)
	if err != nil {
		return 0, 0, fmt.Errorf("range: %w", err)
	}
	indexed := make(map[string]struct{}, len(indexedSegs))
	for _, s := range indexedSegs {
		indexed[s.Fpath] = struct{}{}
	}

	// The directories the window maps to, in chronological order. Stepping by a
	// minute and de-duplicating covers hour/day layouts alike; the finest sane
	// directory granularity is a minute, so no directory is skipped.
	fmtExt := recstore.PathAddExtension(recordPath)
	var dirs []string
	seen := map[string]struct{}{}
	for t := from.Truncate(time.Minute); !t.After(hardTo); t = t.Add(time.Minute) {
		full := recstore.Path{Start: t, Path: camera}.Encode(fmtExt)
		dir := filepath.Dir(full)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}

	consecutiveDry := 0
	for _, dir := range dirs {
		entries, rderr := os.ReadDir(dir)
		if rderr != nil {
			// A missing directory is expected (the window runs past the crash
			// point into never-written time); other errors are logged and skipped.
			if !os.IsNotExist(rderr) {
				logf("read dir %s: %v", dir, rderr)
			}
			continue
		}

		newInDir := 0
		// Deterministic order within a directory keeps logs stable.
		names := make([]string, 0, len(entries))
		for _, ent := range entries {
			if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".mp4") {
				names = append(names, ent.Name())
			}
		}
		sort.Strings(names)

		for _, name := range names {
			full := filepath.Join(dir, name)
			if _, ok := indexed[full]; ok {
				continue
			}
			seg, perr := probe(full, camera)
			if perr != nil {
				// A file with no valid init/fragments (e.g. crashed before the
				// first part flushed) can't be recovered — leave it for retention.
				logf("skip unrecoverable %s: %v", full, perr)
				continue
			}
			if err := idx.Insert(seg); err != nil {
				logf("index insert %s: %v", full, err)
				continue
			}
			recovered++
			newInDir++
			total += time.Duration(seg.Duration * float64(time.Second))
			logf("recovered %s (start=%s dur=%.1fs)", full, seg.Start.Format(time.RFC3339), seg.Duration)
		}

		if newInDir == 0 {
			consecutiveDry++
			if consecutiveDry >= dryDirs {
				break
			}
		} else {
			consecutiveDry = 0
		}
	}

	return recovered, total, nil
}

// Reindex rebuilds the index for one camera by walking its ENTIRE recording
// subtree and inserting every segment not already indexed. Unlike Recover (a
// cheap forward scan for crash orphans), this is a full walk — meant for
// recovering from a lost or corrupt index, where the watermark is gone and
// footage on disk is otherwise invisible. Inserts are idempotent
// (INSERT OR REPLACE by path), so it is safe to run alongside the live recorder
// and to re-run. It honours ctx for cancellation (e.g. shutdown) and never
// aborts on a single unreadable file — only skips it.
func Reindex(ctx context.Context, idx *index.Index, recordPath, camera string, logf func(string, ...any)) (recovered int, total time.Duration, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	root := cameraRoot(recordPath, camera)

	// Everything already indexed for this camera — skipped without opening.
	existing, err := idx.Range(camera, nil, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("range: %w", err)
	}
	indexed := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		indexed[s.Fpath] = struct{}{}
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if werr != nil {
			// Missing subtree = no footage; other per-entry errors are logged but
			// must not abort the whole rebuild.
			if !errors.Is(werr, fs.ErrNotExist) {
				logf("walk %s: %v", path, werr)
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".mp4") {
			return nil
		}
		if _, ok := indexed[path]; ok {
			return nil
		}
		seg, perr := probe(path, camera)
		if perr != nil {
			// A file still being written (the live recorder's open segment) or a
			// truncated one can't be parsed yet; the recorder indexes it at close.
			logf("skip unreadable %s: %v", path, perr)
			return nil
		}
		if ierr := idx.Insert(seg); ierr != nil {
			logf("index insert %s: %v", path, ierr)
			return nil
		}
		recovered++
		total += time.Duration(seg.Duration * float64(time.Second))
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return recovered, total, walkErr
	}
	return recovered, total, nil
}

// cameraRoot returns the directory subtree that holds one camera's segments,
// derived from the record-path format by substituting the camera id into the
// %path component. For the default layout `<dir>/%path/%Y-.../…` this is
// `<dir>/<camera>`, so a rebuild walks only that camera. If %path is not a clean
// directory component (unusual custom layout), it falls back to the fixed prefix
// (the whole record root), which still works — just less selectively.
func cameraRoot(recordPath, camera string) string {
	sep := string(filepath.Separator)
	parts := strings.Split(recordPath, sep)
	for i, p := range parts {
		if strings.Contains(p, "%path") {
			if p == "%path" {
				parts[i] = camera
				return strings.Join(parts[:i+1], sep)
			}
			break // %path shares a component with other specifiers — can't isolate
		}
	}
	return recstore.CommonPath(recordPath)
}

// probe reads a segment file and derives its index row: start/StreamID/segment
// number from the mtxi box, and duration summed from the video track's fMP4
// fragment sample durations (the on-disk mvhd duration is unreliable — a crashed
// segment never had it written).
func probe(path, camera string) (index.Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return index.Segment{}, err
	}
	defer f.Close()

	init, err := readInit(f)
	if err != nil {
		return index.Segment{}, err
	}
	m := mtxi.Find(init.UserData)
	if m == nil {
		return index.Segment{}, fmt.Errorf("no mtxi box (not a recorder segment)")
	}

	// Identify the video track (its timescale converts fragment ticks to time).
	var videoID int
	var videoTS uint32
	for _, t := range init.Tracks {
		if t.Codec.IsVideo() {
			videoID = t.ID
			videoTS = t.TimeScale
			break
		}
	}
	if videoTS == 0 {
		return index.Segment{}, fmt.Errorf("no video track in init")
	}

	dur, err := videoDuration(path, videoID, videoTS)
	if err != nil {
		return index.Segment{}, err
	}
	if dur <= 0 {
		return index.Segment{}, fmt.Errorf("no complete fragments (zero duration)")
	}

	return index.Segment{
		Fpath:         path,
		Path:          camera,
		Start:         time.Unix(0, m.NTP),
		Duration:      dur.Seconds(),
		SegmentNumber: m.SegmentNumber,
		StreamID:      uuid.UUID(m.StreamID).String(),
	}, nil
}

// readInit parses the ftyp+moov init at the head of a segment file into an
// fmp4.Init (tracks + mtxi user-data). Mirrors playback.readHeader but ignores
// the mvhd duration, which is not written on a crashed segment.
func readInit(f *os.File) (*fmp4.Init, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	if string(buf[4:]) != "ftyp" {
		return nil, fmt.Errorf("ftyp box not found")
	}
	ftypSize := binary.BigEndian.Uint32(buf)

	if _, err := f.Seek(int64(ftypSize), io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	if string(buf[4:]) != "moov" {
		return nil, fmt.Errorf("moov box not found")
	}
	moovSize := binary.BigEndian.Uint32(buf)

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	head := make([]byte, uint64(ftypSize)+uint64(moovSize))
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}

	var init fmp4.Init
	if err := init.Unmarshal(bytes.NewReader(head)); err != nil {
		return nil, err
	}
	return &init, nil
}

// videoDuration sums the video track's sample durations across every fragment
// (moof/traf/trun) and converts the total to wall-clock time via the track
// timescale. This is the segment's true recorded length regardless of the
// unwritten mvhd duration.
func videoDuration(path string, videoID int, timeScale uint32) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var curTrackID uint32
	var totalTicks int64
	_, err = amp4.ReadBoxStructure(f, func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof", "traf":
			return h.Expand()
		case "tfhd":
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			curTrackID = box.(*amp4.Tfhd).TrackID
		case "trun":
			if int(curTrackID) != videoID {
				return nil, nil
			}
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			for _, e := range box.(*amp4.Trun).Entries {
				totalTicks += int64(e.SampleDuration)
			}
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	if totalTicks <= 0 {
		return 0, nil
	}
	ts := int64(timeScale)
	return time.Duration(totalTicks/ts)*time.Second +
		time.Duration(totalTicks%ts)*time.Second/time.Duration(ts), nil
}
