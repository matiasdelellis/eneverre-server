package playback

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"eneverre/internal/media/index"
)

// recMuxer records the samples written per track.
type recMuxer struct {
	cur     int
	samples map[int][]int64 // DTS
	pts     map[int][]int64
}

func (m *recMuxer) writeInit(*fmp4.Init) {}
func (m *recMuxer) setTrack(id int)      { m.cur = id }
func (m *recMuxer) writeFinalDTS(int64)  {}
func (m *recMuxer) flush() error         { return nil }
func (m *recMuxer) writeSample(dts int64, ptsOffset int32, _ bool, _ uint32, _ func() ([]byte, error)) error {
	if m.samples == nil {
		m.samples = map[int][]int64{}
		m.pts = map[int][]int64{}
	}
	m.samples[m.cur] = append(m.samples[m.cur], dts)
	m.pts[m.cur] = append(m.pts[m.cur], dts+int64(ptsOffset))
	return nil
}

// A segment with 2s of video and 20s of audio gets the placeholder over the
// audio-only span, interleaved with the audio, in DTS order, and a real frame
// that lands inside an already-filled span is dropped.
func TestVideoFillCoversAudioOnlySpan(t *testing.T) {
	const ts = 90000
	m := &recMuxer{}
	f := &videoFill{trackID: 1, timeScale: ts, payload: []byte{1}}

	// first fragment: 2s of video at 25 fps, audio alongside
	m.setTrack(1)
	for i := 0; i < 50; i++ {
		dts := int64(i) * 3600
		if f.video(m, dts, 0, 3600, i%25 == 0) {
			m.writeSample(dts, 0, false, 0, nil) //nolint:errcheck
		}
	}
	f.other(2 * time.Second)
	// following fragments: audio only, one per second
	for s := 3; s <= 20; s++ {
		f.catchUp(m)
		f.other(time.Duration(s) * time.Second)
	}
	// a late fragment holding the last frame before the stall, stretched
	m.setTrack(1)
	if f.video(m, 50*3600, 0, 18*ts, false) {
		t.Error("a frame inside the filled span was written out of order")
	}
	f.finish(m, time.Hour)

	v := m.samples[1]
	for i := 1; i < len(v); i++ {
		if v[i] <= v[i-1] {
			t.Fatalf("video DTS not increasing at %d: %d after %d", i, v[i], v[i-1])
		}
	}
	if last := time.Duration(v[len(v)-1]) * time.Second / ts; last < 19*time.Second {
		t.Errorf("video ends at %s, want the placeholder up to ~20s", last)
	}
	for i := 50; i < len(v); i++ {
		if gap := time.Duration(v[i]-v[i-1]) * time.Second / ts; gap > time.Second+time.Millisecond {
			t.Fatalf("hole of %s in the video at sample %d", gap, i)
		}
	}
}

// Full-video segments are untouched.
func TestVideoFillLeavesContinuousVideo(t *testing.T) {
	m := &recMuxer{}
	f := &videoFill{trackID: 1, timeScale: 90000, payload: []byte{1}}
	m.setTrack(1)
	for i := 0; i < 250; i++ {
		if i%25 == 0 {
			f.catchUp(m)
			f.other(time.Duration(i/25) * time.Second)
			m.setTrack(1)
		}
		dts := int64(i) * 3600
		if f.video(m, dts, 0, 3600, i%25 == 0) {
			m.writeSample(dts, 0, false, 0, nil) //nolint:errcheck
		}
	}
	f.other(10 * time.Second)
	f.finish(m, time.Hour)
	if n := len(m.samples[1]); n != 250 {
		t.Fatalf("got %d video samples, want the 250 real ones", n)
	}
}

// writeInitFile writes a segment file holding just an init with one track of
// the given codec: enough for the playlist to read the codec.
func writeInitFile(t *testing.T, path string, codec mcodecs.Codec) {
	t.Helper()
	var buf seekablebuffer.Buffer
	init := fmp4.Init{Tracks: []*fmp4.InitTrack{{ID: 1, TimeScale: 90000, Codec: codec}}}
	if err := init.Marshal(&buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The playlist gives a segment with an audio-only span its own avc3 init and
// fill flag, bracketed by discontinuities; full and unknown segments keep the
// plain init, and so does a partial segment whose video is not H264 (the
// placeholder is H264-only).
func TestHLSPlaylistFlagsVideoFill(t *testing.T) {
	h := hlsTestHandler(t)
	dir := t.TempDir()
	h264 := filepath.Join(dir, "h264.mp4")
	writeInitFile(t, h264, &mcodecs.H264{
		SPS: []byte{
			0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
			0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
			0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20,
		},
		PPS: []byte{0x08, 0x06, 0x07, 0x08},
	})
	other := filepath.Join(dir, "other.mp4")
	writeInitFile(t, other, &mcodecs.LPCM{BitDepth: 16, SampleRate: 8000, ChannelCount: 1})

	base := time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)
	ins := func(i int, stream, file string, video *index.VideoExtent) {
		start := base.Add(time.Duration(i) * time.Minute)
		if err := h.Index.Insert(index.Segment{
			Fpath: file, Path: "cam", Start: start,
			Duration: 60, StreamID: stream, Video: video,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// distinct fpaths are the primary key: copy the init per segment
	seg := func(i int, src string) string {
		b, _ := os.ReadFile(src)
		p := filepath.Join(dir, fmt.Sprintf("seg%d.mp4", i))
		os.WriteFile(p, b, 0o644) //nolint:errcheck
		return p
	}
	ins(0, "a", seg(0, h264), &index.VideoExtent{Duration: 60})
	ins(1, "a", seg(1, h264), &index.VideoExtent{Duration: 1.5})
	ins(2, "a", seg(2, h264), &index.VideoExtent{Duration: 60})
	ins(3, "a", seg(3, h264), nil)

	rec := httptest.NewRecorder()
	h.HandleHLSPlaylist(rec, hlsPlaylistReq(base.Format(time.RFC3339), base.Add(4*time.Minute).Format(time.RFC3339)))
	pl := rec.Body.String()

	if n := strings.Count(pl, "#EXT-X-MAP"); n != 3 {
		t.Errorf("got %d EXT-X-MAP, want 3 (plain, fill, plain)\n%s", n, pl)
	}
	if n := strings.Count(pl, "#EXT-X-DISCONTINUITY"); n != 2 {
		t.Errorf("got %d discontinuities, want 2\n%s", n, pl)
	}
	if n := strings.Count(pl, "fill=1"); n != 2 {
		t.Errorf("got %d fill=1 (init + segment), want 2\n%s", n, pl)
	}

	// a partial segment of a stream with no H264 track is left alone
	ins(10, "b", seg(10, other), &index.VideoExtent{Duration: 60})
	ins(11, "b", seg(11, other), &index.VideoExtent{Duration: 1.5})
	from := base.Add(10 * time.Minute)
	rec = httptest.NewRecorder()
	h.HandleHLSPlaylist(rec, hlsPlaylistReq(from.Format(time.RFC3339), from.Add(2*time.Minute).Format(time.RFC3339)))
	if pl := rec.Body.String(); strings.Contains(pl, "fill=1") {
		t.Errorf("non-H264 segment flagged for fill\n%s", pl)
	}
}

// After a fill, real frames are dropped until a keyframe: a P-frame decoded
// against the placeholder would be garbage.
func TestVideoFillWaitsForKeyframe(t *testing.T) {
	const ts = 90000
	m := &recMuxer{}
	f := &videoFill{trackID: 1, timeScale: ts, payload: []byte{1}}
	m.setTrack(1)
	f.other(30 * time.Second)
	// video resumes at 20s mid-GOP; keyframe at 21s
	for i := 0; i < 50; i++ {
		dts := int64(20*ts) + int64(i)*3600
		if f.video(m, dts, 0, 3600, i == 25) {
			m.writeSample(dts, 0, false, 0, nil) //nolint:errcheck
		}
	}
	var real int
	for _, d := range m.samples[1] {
		if d >= 20*ts && (d-20*ts)%3600 == 0 && d != 20*ts {
			real++
		}
	}
	if real != 25 {
		t.Fatalf("wrote %d real frames after the fill, want the 25 from the keyframe on", real)
	}
}

// With B-frames a frame is shown after it is decoded: the placeholder must
// start after the last real frame's presentation, not its decode time, or the
// picture steps back in time. Mirrors the field camera (PTS = DTS + 0.6s).
func TestVideoFillAfterBFramePresentation(t *testing.T) {
	const ts = 90000
	const offset = int32(0.6 * ts)
	m := &recMuxer{}
	f := &videoFill{trackID: 1, timeScale: ts, payload: []byte{1}}
	m.setTrack(1)
	for i := 0; i < 50; i++ {
		dts := int64(i) * 3600
		if f.video(m, dts, offset, 3600, i%25 == 0) {
			m.writeSample(dts, offset, false, 0, nil) //nolint:errcheck
		}
	}
	f.other(30 * time.Second)
	f.finish(m, time.Hour)

	dts, pts := m.samples[1], m.pts[1]
	var maxRealPTS int64
	for i := 0; i < 50; i++ {
		maxRealPTS = max(maxRealPTS, pts[i])
	}
	if len(pts) <= 50 {
		t.Fatal("no placeholder written")
	}
	if pts[50] <= maxRealPTS {
		t.Errorf("first placeholder shown at %d, before the last real frame (%d)", pts[50], maxRealPTS)
	}
	for i := 1; i < len(dts); i++ {
		if dts[i] <= dts[i-1] {
			t.Fatalf("DTS not increasing at %d", i)
		}
	}
}
