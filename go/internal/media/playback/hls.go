package playback

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"eneverre/internal/media/index"
)

// splitWriter routes the muxer's first Write (the ftyp+moov init) separately
// from the rest (moof+mdat media). muxerFMP4 always emits the init in a single
// Write, followed by one Write per fragment, so dropping the first Write yields
// a CMAF media segment (moof+mdat only).
type splitWriter struct {
	w         http.ResponseWriter
	keepFirst bool
	firstDone bool
	started   bool
}

func (s *splitWriter) Write(p []byte) (int, error) {
	if !s.firstDone {
		s.firstDone = true
		if !s.keepFirst {
			return len(p), nil // drop the init segment
		}
	}
	if !s.started {
		s.started = true
		s.w.Header().Set("Content-Type", "video/mp4")
	}
	return s.w.Write(p)
}

// HandleHLSPlaylist serves a VOD playlist: GET /hls/playlist.m3u8?path=&from=&to=
// Each recorded segment becomes an HLS fMP4 (CMAF) segment. Gaps are collapsed
// into a continuous timeline; EXT-X-PROGRAM-DATE-TIME preserves wall-clock.
func (h *Handler) HandleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	from := parseHLSTime(q.Get("from"))
	to := parseHLSTime(q.Get("to"))

	segs, err := h.Index.Range(path, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(segs) == 0 {
		http.Error(w, "no recordings found", http.StatusNotFound)
		return
	}

	maxDur := 0.0
	for _, s := range segs {
		if s.Duration > maxDur {
			maxDur = s.Duration
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxDur))))

	// init segment (track config) taken from the first segment of the range
	initURL := "init.mp4?path=" + url.QueryEscape(path) +
		"&start=" + url.QueryEscape(segs[0].Start.Format(time.RFC3339Nano))
	b.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=%q\n", initURL))

	base := 0.0 // cumulative presentation time (gaps collapsed)
	var prevEnd time.Time
	for i, s := range segs {
		// A coverage gap (wall-clock jump beyond tolerance from the
		// previous segment's end) is a real discontinuity: the next
		// segment has different decoder params and a wall-clock jump, so
		// we emit EXT-X-DISCONTINUITY per the HLS spec. Generic players
		// (VLC, ExoPlayer, AVPlayer, hls.js) reset decoder state and
		// typically seek to the next keyframe. Matches
		// segmentCanBeConcatenated's tolerance.
		if i > 0 && s.Start.Sub(prevEnd) > concatenationTolerance {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		segURL := "segment.m4s?path=" + url.QueryEscape(path) +
			"&start=" + url.QueryEscape(s.Start.Format(time.RFC3339Nano)) +
			"&base=" + strconv.FormatFloat(base, 'f', -1, 64)
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:" + s.Start.Format(time.RFC3339Nano) + "\n")
		b.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", s.Duration))
		b.WriteString(segURL + "\n")
		base += s.Duration
		prevEnd = s.Start.Add(time.Duration(s.Duration * float64(time.Second)))
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(b.String())) //nolint:errcheck
}

// HandleHLSInit serves the CMAF init segment: GET /hls/init.mp4?path=&start=
func (h *Handler) HandleHLSInit(w http.ResponseWriter, r *http.Request) {
	seg, ok := h.hlsFindSegment(w, r)
	if !ok {
		return
	}

	f, err := os.Open(seg.Fpath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	init, _, err := readHeader(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf seekablebuffer.Buffer
	clean := fmp4.Init{Tracks: init.Tracks}
	if err := clean.Marshal(&buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Write(buf.Bytes()) //nolint:errcheck
}

// HandleHLSSegment serves a CMAF media segment: GET /hls/segment.m4s?path=&start=&base=
// It re-muxes the segment with baseMediaDecodeTime = base so the whole VOD
// presentation has a continuous timeline.
func (h *Handler) HandleHLSSegment(w http.ResponseWriter, r *http.Request) {
	seg, ok := h.hlsFindSegment(w, r)
	if !ok {
		return
	}

	base := 0.0
	if v := r.URL.Query().Get("base"); v != "" {
		base, _ = strconv.ParseFloat(v, 64)
	}

	f, err := os.Open(seg.Fpath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	init, _, err := readHeader(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sw := &splitWriter{w: w} // keepFirst=false: drop init, emit moof+mdat only
	m := &muxerFMP4{w: sw}
	m.writeInit(&fmp4.Init{Tracks: init.Tracks})

	baseDur := time.Duration(base * float64(time.Second))
	segDur := time.Duration(seg.Duration * float64(time.Second))

	// duration generous enough to never cut the segment short
	if _, err := muxParts(f, baseDur, baseDur+segDur+2*time.Second, init.Tracks, m); err != nil {
		if !sw.started {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	m.flush() //nolint:errcheck
}

// hlsFindSegment resolves the exact recorded segment identified by ?path=&start=
func (h *Handler) hlsFindSegment(w http.ResponseWriter, r *http.Request) (seg index.Segment, ok bool) {
	q := r.URL.Query()
	path := q.Get("path")
	start, err := time.Parse(time.RFC3339Nano, q.Get("start"))
	if err != nil {
		http.Error(w, "invalid start", http.StatusBadRequest)
		return seg, false
	}

	to := start.Add(time.Millisecond)
	segs, err := h.Index.Range(path, &start, &to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return seg, false
	}
	// pick the segment whose start matches exactly
	for _, s := range segs {
		if s.Start.Equal(start) {
			return s, true
		}
	}
	http.Error(w, "segment not found", http.StatusNotFound)
	return seg, false
}

// parseHLSTime parses the from/to query params of an HLS playlist request.
// Deliberately strict RFC3339 (not the lenient timeutil.ParseISO used by the
// public API): these values come from URIs this server generates, so anything
// other than the exact format it emits is a bug, not input to tolerate.
// Returns nil on empty or malformed input; the caller treats nil as "unbounded".
func parseHLSTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
