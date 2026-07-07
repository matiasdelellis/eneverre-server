// Package media is the embedded NVR engine: it records each camera's RTSP
// stream to fMP4 segments, catalogs them in a shared SQLite index, re-serves
// the live streams over RTSP (multi-path passthrough), broadcasts them to
// browsers as MSE fMP4, and serves recorded playback over HTTP. The on-disk
// segment layout is MediaMTX-compatible (the `mtxi` box and fMP4 structure
// are byte-identical to what MediaMTX wrote when it was the supported
// external streamer), so existing tooling that inspects MediaMTX recordings
// can read the recorder's output too.
//
// One Engine supervises N cameras: per camera it runs a recorder.Recorder in a
// retry loop, wired to the shared liverelay.Relay and a per-camera
// live.Broadcaster. All recorders feed one index.Index (keyed by camera id in
// the `path` column), so playback and retention span every camera.
package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/pion/rtp"

	"eneverre/internal/camera"
	"eneverre/internal/config"
	"eneverre/internal/media/index"
	"eneverre/internal/media/live"
	"eneverre/internal/media/liverelay"
	"eneverre/internal/media/playback"
	"eneverre/internal/media/recorder"
	"eneverre/internal/media/retention"
)

// Options configures the embedded engine, resolved from the [media] INI section.
type Options struct {
	RecordDir       string        // base directory recordings live under
	IndexPath       string        // SQLite index file (default <RecordDir>/index.db)
	CacheDir        string        // generated-asset cache (gap-fill frames); default <RecordDir>/../cache
	GapMessage      string        // caption burned into gap-fill black frames (default "NO RECORDING")
	RecordPath      string        // segment path pattern (default <RecordDir>/%path/%Y-%m-%d/%H/<time>)
	SegmentDuration time.Duration // min segment length (default 60s)
	PartDuration    time.Duration // fMP4 fragment length (default 1s)
	Retain          time.Duration // delete recordings older than this; 0 = keep forever
	RTSPAddress     string        // relay listen address (default ":8554")
	Transport       string        // RTSP source transport: auto|tcp|udp (default auto)
	// RelayCredsFn supplies the currently-valid [user, pass] pairs the RTSP relay
	// accepts (current + grace), consulted per auth so credential rotation takes
	// effect without dropping readers. Nil leaves the relay open.
	RelayCredsFn func() [][2]string
}

// OptionsFromSection builds Options from a [media] config.Section, applying
// defaults. server/relay credentials are supplied separately by the caller.
func OptionsFromSection(sec config.Section) Options {
	recordDir := sec.Get("record_dir", "/var/lib/eneverre/recordings")
	o := Options{
		RecordDir:       recordDir,
		IndexPath:       sec.Get("index_path", filepath.Join(recordDir, "index.db")),
		CacheDir:        sec.Get("cache_dir", filepath.Join(filepath.Dir(recordDir), "cache")),
		GapMessage:      sec.Get("gap_message", "NO RECORDING"),
		// Default layout groups by camera then date then hour, so each
		// camera's day is one subtree (`<RecordDir>/<cam>/YYYY-MM-DD/HH/...`).
		// The filename embeds the full timestamp so segments stay uniquely
		// named even across day/hour boundaries. Operators can override the
		// whole pattern in [media].
		RecordPath:      sec.Get("record_path", filepath.Join(recordDir, "%path", "%Y-%m-%d", "%H", "%Y-%m-%d_%H-%M-%S-%f")),
		SegmentDuration: durationOr(sec.Get("segment_duration", ""), 60*time.Second),
		PartDuration:    durationOr(sec.Get("part_duration", ""), time.Second),
		Retain:          durationOr(sec.Get("retain", ""), 0),
		RTSPAddress:     sec.Get("rtsp_address", ":8554"),
		Transport:       sec.Get("transport", "auto"),
	}
	return o
}

func durationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		slog.Warn("media: invalid duration, using default", "value", s, "default", def)
		return def
	}
	return d
}

// Engine supervises recording, live relay/broadcast and playback for all cameras.
type Engine struct {
	opts     Options
	idx      *index.Index
	relay    *liverelay.Relay
	playback *playback.Handler

	mu           sync.RWMutex
	broadcasters map[string]*live.Broadcaster // camera id -> live MSE broadcaster
	recorders    []*recorder.Recorder

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New opens the index and initializes the RTSP relay, but does not start
// recording; call Start with the camera list.
func New(opts Options) (*Engine, error) {
	idx, err := index.Open(opts.IndexPath)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	relay := &liverelay.Relay{
		Address: opts.RTSPAddress,
		CredsFn: opts.RelayCredsFn,
		Logf:    func(f string, a ...any) { slog.Debug("media/relay: " + fmt.Sprintf(f, a...)) },
	}
	if err := relay.Initialize(); err != nil {
		idx.Close()
		return nil, fmt.Errorf("start rtsp relay: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:         opts,
		idx:          idx,
		relay:        relay,
		playback:     &playback.Handler{Index: idx, CacheDir: opts.CacheDir, GapMessage: opts.GapMessage},
		broadcasters: map[string]*live.Broadcaster{},
		ctx:          ctx,
		cancel:       cancel,
	}
	return e, nil
}

// Start begins recording every camera that has a Source URL. Each camera runs
// in its own goroutine (retry loop); Start returns immediately.
func (e *Engine) Start(cams []camera.Camera) {
	for _, cam := range cams {
		if cam.Source == "" {
			continue
		}
		e.startCamera(cam)
	}

	if e.opts.Retain > 0 {
		cl := &retention.Cleaner{
			Index:      e.idx,
			Retain:     e.opts.Retain,
			RecordPath: e.opts.RecordPath,
			Logf:       func(f string, a ...any) { slog.Info("media/retention: " + fmt.Sprintf(f, a...)) },
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			cl.Run(e.ctx)
		}()
		slog.Info("media: retention enabled", "older_than", e.opts.Retain)
	}
	slog.Info("media engine started", "cameras", len(e.recorders), "rtsp", e.opts.RTSPAddress, "record_dir", e.opts.RecordDir, "cache_dir", e.opts.CacheDir)
}

func (e *Engine) startCamera(cam camera.Camera) {
	id := cam.ID

	lb := &live.Broadcaster{Logf: func(f string, a ...any) { slog.Debug("media/live[" + id + "]: " + fmt.Sprintf(f, a...)) }}
	lb.Initialize()
	e.mu.Lock()
	e.broadcasters[id] = lb
	e.mu.Unlock()

	// Per-camera transport override (INI `transport`) falls back to the global
	// [media] transport when unset.
	transport := cam.Transport
	if transport == "" {
		transport = e.opts.Transport
	}
	slog.Info("media/recorder starting", "camera", id, "transport", transport, "per_camera", cam.Transport != "")

	rec := &recorder.Recorder{
		URL:             cam.Source,
		Transport:       transport,
		PathName:        id,
		PathFormat:      e.opts.RecordPath,
		SegmentDuration: e.opts.SegmentDuration,
		PartDuration:    e.opts.PartDuration,
		Logf:            func(f string, a ...any) { slog.Debug("media/recorder[" + id + "]: " + fmt.Sprintf(f, a...)) },
		OnSegment: func(s recorder.SegmentInfo) {
			if err := e.idx.Insert(index.Segment{
				Fpath:         s.Path,
				Path:          id,
				Start:         s.Start,
				Duration:      s.Duration.Seconds(),
				SegmentNumber: s.SegmentNumber,
				StreamID:      s.StreamID,
			}); err != nil {
				slog.Error("media/index insert failed", "camera", id, "err", err)
				return
			}
			slog.Debug("media/segment indexed", "camera", id, "seg", s.SegmentNumber, "dur_s", s.Duration.Seconds(), "path", s.Path)
		},
		// live RTSP relay (raw RTP passthrough)
		OnSource: func(desc *description.Session) error { return e.relay.SetSource(id, desc) },
		OnRTP:    func(m *description.Media, pkt *rtp.Packet) { e.relay.WritePacketRTP(id, m, pkt) },
		OnSourceLost: func() {
			e.relay.ClearSource(id)
			lb.Stop()
		},
		// live web (MSE fMP4)
		OnLiveTracks: func(its []*fmp4.InitTrack) error {
			if err := lb.SetTracks(its); err != nil {
				slog.Warn("media/live web disabled", "camera", id, "err", err)
			}
			return nil // a broadcaster issue must never stop recording
		},
		OnLiveSample: func(trackID int, s *fmp4.Sample, dts int64) { lb.WriteSample(trackID, s, dts) },
	}

	e.mu.Lock()
	e.recorders = append(e.recorders, rec)
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			// rec.Start blocks until the stream ends; gortsplib's read timeout
			// (10s) turns a silent stall on a flaky/far camera into an error, so
			// this loop reconnects instead of hanging forever.
			backoff := time.Second
			if err := rec.Start(); err != nil {
				if errors.Is(err, recorder.ErrNoSupportedVideo) {
					// Permanent until the camera's codec changes (e.g. an H265-only
					// camera): report it prominently and retry slowly instead of
					// spamming a reconnect every second.
					slog.Warn("media/recorder: camera codec not supported (recording/live disabled for it)", "camera", id, "err", err)
					backoff = 30 * time.Second
				} else {
					slog.Warn("media/recorder stopped, reconnecting", "camera", id, "err", err)
				}
			} else {
				slog.Info("media/recorder source ended, reconnecting", "camera", id)
			}
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
}

// Broadcaster returns the live MSE broadcaster for a camera id, or nil.
func (e *Engine) Broadcaster(id string) *live.Broadcaster {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.broadcasters[id]
}

// Playback returns the HTTP playback handler backed by the shared index.
func (e *Engine) Playback() *playback.Handler { return e.playback }

// Index returns the shared segment index (for metadata queries).
func (e *Engine) Index() *index.Index { return e.idx }

// Close stops all recorders, the relay, the retention loop, and the index.
func (e *Engine) Close() {
	e.cancel()
	e.mu.RLock()
	recs := append([]*recorder.Recorder(nil), e.recorders...)
	e.mu.RUnlock()
	for _, r := range recs {
		r.Close()
	}
	e.wg.Wait()
	e.relay.Close()
	e.idx.Close()
}
