// Package playback serves recorded segments over HTTP: one-shot gapless clips
// (/get), an HLS VOD stream (CMAF fMP4), and the fMP4 (re)muxing that backs them.
package playback

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"eneverre/internal/media/index"
	"eneverre/internal/media/mtxi"
)

type writerWrapper struct {
	w       http.ResponseWriter
	written bool
}

func (ww *writerWrapper) Write(p []byte) (int, error) {
	if !ww.written {
		ww.written = true
		ww.w.Header().Set("Accept-Ranges", "none")
		ww.w.Header().Set("Content-Type", "video/mp4")
	}
	return ww.w.Write(p)
}

// seekAndMux stitches consecutive segments into a single continuous fMP4,
// using the mtxi box for gapless concatenation across files. When fillGaps is
// set, coverage gaps between segments are filled with a black "SIN GRABACIÓN"
// frame occupying the real gap time (so a download isn't silently trimmed),
// and the stream is emitted as avc3 (see muxerFMP4.useAVC3).
func seekAndMux(segments []index.Segment, start time.Time, duration time.Duration, m muxer, fillGaps bool, cacheDir, gapMessage string) error {
	f, err := os.Open(segments[0].Fpath)
	if err != nil {
		return err
	}
	defer f.Close()

	firstInit, _, err := readHeader(f)
	if err != nil {
		return err
	}

	m.writeInit(&fmp4.Init{Tracks: firstInit.Tracks})

	// Resolve the black-frame filler for the video track (best effort; if it
	// fails we fall back to truncating at gaps).
	var blackPayload []byte
	var vID int
	var vTS uint32
	var hasVideo bool
	for _, t := range firstInit.Tracks {
		if _, ok := t.Codec.(*mcodecs.H264); ok {
			vID = t.ID
			vTS = t.TimeScale
			hasVideo = true
			break
		}
	}
	if fillGaps && hasVideo {
		for _, t := range firstInit.Tracks {
			if h264, ok := t.Codec.(*mcodecs.H264); ok {
				if w, h, ok := spsDimensions(h264.SPS); ok {
					if p, gerr := blackFramePayload(cacheDir, gapMessage, w, h); gerr == nil {
						blackPayload = p
					}
				}
				break
			}
		}
	}

	firstMtxi := mtxi.Find(firstInit.UserData)
	// startOffset is the first segment's start relative to the requested start,
	// and doubles as the DTS base for that segment (see `dts := startOffset`).
	//   startOffset > 0 → footage begins AFTER the request → a LEADING gap.
	//   startOffset < 0 → footage begins before the request → seek into it.
	startOffset := segments[0].Start.Sub(start)

	// Leading gap: the requested window starts before the first available
	// segment. Fill [0, startOffset] with black so the clip isn't front-trimmed.
	if fillGaps && blackPayload != nil && startOffset > 0 {
		writeBlackGap(m, vID, vTS, blackPayload, 0, min(startOffset, duration))
	}

	segDur, err := muxParts(f, startOffset, duration, firstInit.Tracks, m)
	if err != nil {
		return err
	}
	segmentEnd := segments[0].Start.Add(segDur)
	prevInit := firstInit

	// processSegment stitches one follow-on segment onto the output. It opens
	// and closes the segment file within its own scope: muxParts/readHeader read
	// eagerly (writeSample resolves each payload before returning), so the file
	// is safe to close as soon as this returns. Keeping the open scoped per
	// segment is what stops a wide range (thousands of 60s segments) from holding
	// every file descriptor open at once and exhausting RLIMIT_NOFILE — which
	// would also starve the recorders that need FDs to create new segments.
	// It returns the advanced segmentEnd, the init to carry as prevInit, and a
	// stop flag telling the caller to end the stitch loop.
	processSegment := func(seg index.Segment, prevInit *fmp4.Init, segmentEnd time.Time) (time.Time, *fmp4.Init, bool, error) {
		sf, err := os.Open(seg.Fpath)
		if err != nil {
			return segmentEnd, prevInit, false, err
		}
		defer sf.Close()

		init, _, err := readHeader(sf)
		if err != nil {
			return segmentEnd, prevInit, false, err
		}

		concat := segmentCanBeConcatenated(prevInit, segmentEnd, init, seg.Start)
		if !concat {
			// gap or incompatible stream
			if blackPayload == nil {
				return segmentEnd, prevInit, true, nil // no filler: stop here (legacy behavior)
			}
			gapStart := segmentEnd.Sub(start)
			gapEnd := seg.Start.Sub(start)
			if gapEnd > duration {
				gapEnd = duration
			}
			writeBlackGap(m, vID, vTS, blackPayload, gapStart, gapEnd)
			if gapEnd >= duration {
				return start.Add(duration), prevInit, true, nil // window already filled to the end
			}
		}

		var dts time.Duration
		if concat && firstMtxi != nil {
			mi := mtxi.Find(init.UserData)
			dts = time.Duration(mi.DTS-firstMtxi.DTS) + startOffset
		} else {
			// after a filled gap (discontinuity) use the real wall-clock position
			dts = seg.Start.Sub(start)
		}

		segDur, err := muxParts(sf, dts, duration, firstInit.Tracks, m)
		if err != nil {
			return segmentEnd, prevInit, false, err
		}
		return seg.Start.Add(segDur), init, false, nil
	}

	for _, seg := range segments[1:] {
		var stop bool
		segmentEnd, prevInit, stop, err = processSegment(seg, prevInit, segmentEnd)
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}

	// Trailing gap: the window extends past the last available footage. Fill
	// [segmentEnd, duration] with black so the clip spans the full window.
	if fillGaps && blackPayload != nil {
		if tail := segmentEnd.Sub(start); tail < duration {
			writeBlackGap(m, vID, vTS, blackPayload, tail, duration)
		}
	}

	return m.flush()
}

// writeBlackGap splices the black filler frame onto the video track from
// gapStart to gapEnd (both relative to the clip start), one keyframe per second
// so seeking within the gap works. No-op when the range is empty.
func writeBlackGap(m muxer, videoID int, timeScale uint32, payload []byte, gapStart, gapEnd time.Duration) {
	if gapEnd <= gapStart {
		return
	}
	m.setTrack(videoID)
	get := func() ([]byte, error) { return payload, nil }
	for t := gapStart; t < gapEnd; t += time.Second {
		m.writeSample(durationGoToMp4(t, timeScale), 0, false, uint32(len(payload)), get) //nolint:errcheck
	}
	m.writeFinalDTS(durationGoToMp4(gapEnd, timeScale))
}

// Handler serves the /get playback endpoint backed by the segment index.
type Handler struct {
	Index *index.Index
	// CacheDir is where generated assets (gap-fill black frames) are persisted,
	// under a per-purpose subfolder. Empty disables the on-disk cache.
	CacheDir string
	// GapMessage is the caption burned into the gap-fill black frame.
	GapMessage string
}

// HandleGet implements GET /get?path=&start=&duration=&format=fmp4
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pathName := q.Get("path")
	if pathName == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}

	start, err := time.Parse(time.RFC3339, q.Get("start"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid start: %v", err), http.StatusBadRequest)
		return
	}

	duration, err := parseDuration(q.Get("duration"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid duration: %v", err), http.StatusBadRequest)
		return
	}

	if f := q.Get("format"); f != "" && f != "fmp4" {
		http.Error(w, "only format=fmp4 is supported", http.StatusBadRequest)
		return
	}

	end := start.Add(duration)
	segments, err := h.Index.Range(pathName, &start, &end)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(segments) == 0 {
		http.Error(w, "no recordings found for the requested range", http.StatusNotFound)
		return
	}

	// Fill coverage gaps with a black "SIN GRABACIÓN" frame so a download that
	// spans a gap isn't silently trimmed. On by default; ?fill_gaps=false keeps
	// the legacy gapless (truncate-at-gap) behavior.
	fillGaps := q.Get("fill_gaps") != "false"

	ww := &writerWrapper{w: w}
	m := &muxerFMP4{w: ww, useAVC3: fillGaps}

	if err := seekAndMux(segments, start, duration, m, fillGaps, h.CacheDir, h.GapMessage); err != nil {
		if !ww.written {
			if errors.Is(err, errNoSamples) {
				http.Error(w, err.Error(), http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		// headers already sent: nothing else we can do but stop
		return
	}
}

func parseDuration(raw string) (time.Duration, error) {
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(secs * float64(time.Second)), nil
	}
	return time.ParseDuration(raw)
}
