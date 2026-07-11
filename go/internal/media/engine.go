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
//
// The engine has three independent on/off switches:
//
//   - MSEEnabled: live MSE (fMP4) browser feed. Default true.
//     Each camera's `mse = false` INI key opts that camera out individually.
//     Disable globally with [media] mse = false.
//   - RelayEnabled: RTSP relay passthrough. Default true.
//     Each camera's `relay = false` INI key opts that camera out individually.
//     Disable globally with [media] relay = false.
//   - RecordEnabled: writes segments to disk. Default false — must be
//     turned on explicitly with [media] record = true. Per-camera
//     `record = false` opts that camera out individually.
//
// When [media] is absent MSE and relay keep their defaults (both on),
// recording stays off.
type Options struct {
	MSEEnabled    bool   // default true; from [media] mse
	RelayEnabled  bool   // default true; from [media] relay
	RecordEnabled bool   // default false; from [media] record (explicit on)
	RecordDir     string // base directory recordings live under
	IndexPath     string // SQLite index file (default <RecordDir>/index.db)
	CacheDir      string // generated-asset cache (gap-fill frames); default <RecordDir>/../cache
	GapMessage    string // caption burned into gap-fill black frames (default "NO RECORDING")
	RecordPath    string // segment path pattern (default <RecordDir>/%path/%Y-%m-%d/%H/<time>)
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

// DefaultOptions returns the defaults used when no [media] section is
// configured: MSE and relay on, recording off, relay on the default port.
// Cameras with a `source` URL get the live MSE feed and/or RTSP relay
// according to their per-camera mse/relay flags.
func DefaultOptions() Options {
	return Options{
		MSEEnabled:   true,
		RelayEnabled: true,
		// RecordEnabled: false (zero value) — recording is off by default.
		RTSPAddress: ":8554",
		Transport:   "auto",
	}
}

// OptionsFromSection builds Options from a [media] config.Section, applying
// defaults. server/relay credentials are supplied separately by the caller.
func OptionsFromSection(sec config.Section) Options {
	recordDir := sec.Get("record_dir", "/var/lib/eneverre/recordings")
	o := DefaultOptions()
	o.MSEEnabled = sec.GetBool("mse", true)
	o.RelayEnabled = sec.GetBool("relay", true)
	o.RecordEnabled = sec.GetBool("record", false)
	o.RecordDir = recordDir
	o.IndexPath = sec.Get("index_path", filepath.Join(recordDir, "index.db"))
	o.CacheDir = sec.Get("cache_dir", filepath.Join(filepath.Dir(recordDir), "cache"))
	o.GapMessage = sec.Get("gap_message", "NO RECORDING")
	// Default layout groups by camera then date then hour, so each
	// camera's day is one subtree (`<RecordDir>/<cam>/YYYY-MM-DD/HH/...`).
	// The filename embeds the full timestamp so segments stay uniquely
	// named even across day/hour boundaries. Operators can override the
	// whole pattern in [media].
	o.RecordPath = sec.Get("record_path", filepath.Join(recordDir, "%path", "%Y-%m-%d", "%H", "%Y-%m-%d_%H-%M-%S-%f"))
	o.SegmentDuration = durationOr(sec.Get("segment_duration", ""), 60*time.Second)
	o.PartDuration = durationOr(sec.Get("part_duration", ""), time.Second)
	o.Retain = durationOr(sec.Get("retain", ""), 0)
	o.RTSPAddress = sec.Get("rtsp_address", ":8554")
	o.Transport = sec.Get("transport", "auto")
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
	ctrls        map[string]*camCtrl // camera id -> retry-loop pause control

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// camCtrl lets the privacy endpoint park a camera's retry loop. While paused the
// recorder is disconnected from the source (so it neither records nor transmits)
// and the loop waits instead of reconnecting; resuming wakes it to reconnect
// with a fresh session.
type camCtrl struct {
	rec      *recorder.Recorder
	mu       sync.Mutex
	paused   bool
	resumeCh chan struct{} // closed to wake waiters; replaced on each resume
}

func (c *camCtrl) isPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// setPaused updates the paused flag and wakes any waiter when resuming.
// Returns true if the state actually changed.
func (c *camCtrl) setPaused(p bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == p {
		return false
	}
	c.paused = p
	if !p {
		close(c.resumeCh)
		c.resumeCh = make(chan struct{})
	}
	return true
}

// waitWhilePaused blocks until the camera is resumed or the engine shuts down.
func (c *camCtrl) waitWhilePaused(ctx context.Context) {
	for {
		c.mu.Lock()
		if !c.paused {
			c.mu.Unlock()
			return
		}
		ch := c.resumeCh
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
	}
}

// New opens the index (only when recording is enabled) and initializes the
// RTSP relay (only when the relay is enabled), but does not start recording;
// call Start with the camera list. When the relay is globally disabled the
// listener is never bound, so `[media] relay = false` actually frees :8554.
func New(opts Options) (*Engine, error) {
	var idx *index.Index
	var pb *playback.Handler
	if opts.RecordEnabled {
		var err error
		idx, err = index.Open(opts.IndexPath)
		if err != nil {
			return nil, fmt.Errorf("open index: %w", err)
		}
		pb = &playback.Handler{Index: idx, CacheDir: opts.CacheDir, GapMessage: opts.GapMessage}
	}
	var relay *liverelay.Relay
	if opts.RelayEnabled {
		relay = &liverelay.Relay{
			Address: opts.RTSPAddress,
			CredsFn: opts.RelayCredsFn,
			Logf:    func(f string, a ...any) { slog.Debug("media/relay: " + fmt.Sprintf(f, a...)) },
		}
		if err := relay.Initialize(); err != nil {
			if idx != nil {
				idx.Close()
			}
			return nil, fmt.Errorf("start rtsp relay: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:         opts,
		idx:          idx,
		relay:        relay,
		playback:     pb,
		broadcasters: map[string]*live.Broadcaster{},
		ctrls:        map[string]*camCtrl{},
		ctx:          ctx,
		cancel:       cancel,
	}
	return e, nil
}

// Start begins serving every camera that has a Source URL and at least one of
// MSE, relay, or recording enabled. Each camera runs in its own goroutine
// (retry loop); Start returns immediately. MSE, relay, and recording are gated
// independently: a camera can have any subset on (e.g. MSE-only, relay-only, or
// record-only). The recorder always connects to the source when any of the
// three is on — the RTP it demuxes feeds all three sinks.
func (e *Engine) Start(cams []camera.Camera) {
	engaged, mseCount, relayCount, recording := 0, 0, 0, 0
	for _, cam := range cams {
		if cam.Source == "" {
			continue
		}
		f := cam.ResolveFeatures(e.opts.MSEEnabled, e.opts.RelayEnabled, e.opts.RecordEnabled)
		// Engage the camera if any sink is on. Recording counts on its own so a
		// record-only camera (MSE + relay off) still connects and writes to disk.
		if !f.MSE && !f.Relay && !f.Record {
			continue
		}
		e.startCamera(cam, f.MSE, f.Relay, f.Record)
		engaged++
		if f.MSE {
			mseCount++
		}
		if f.Relay {
			relayCount++
		}
		if f.Record {
			recording++
		}
	}

	if e.opts.Retain > 0 && e.idx != nil {
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
	mode := "live-only"
	if e.opts.RecordEnabled {
		mode = "recording"
	}
	rtspAddr := "disabled"
	if e.relay != nil {
		rtspAddr = e.opts.RTSPAddress
	}
	slog.Info("media engine started",
		"mode", mode,
		"cameras", engaged,
		"mse", mseCount,
		"relay", relayCount,
		"recording", recording,
		"rtsp", rtspAddr)
	if e.opts.RecordEnabled {
		slog.Info("media recording paths", "record_dir", e.opts.RecordDir, "cache_dir", e.opts.CacheDir)
	}
}

func (e *Engine) startCamera(cam camera.Camera, mseOn, relayOn, record bool) {
	id := cam.ID

	var lb *live.Broadcaster
	if mseOn {
		lb = &live.Broadcaster{Logf: func(f string, a ...any) { slog.Debug("media/live[" + id + "]: " + fmt.Sprintf(f, a...)) }}
		lb.Initialize()
		e.mu.Lock()
		e.broadcasters[id] = lb
		e.mu.Unlock()
	}

	// Per-camera transport override (INI `transport`) falls back to the global
	// [media] transport when unset.
	transport := cam.Transport
	if transport == "" {
		transport = e.opts.Transport
	}
	slog.Info("media/recorder starting", "camera", id, "transport", transport, "per_camera", cam.Transport != "", "mse", mseOn, "relay", relayOn, "record", record)

	rec := &recorder.Recorder{
		URL:             cam.Source,
		Transport:       transport,
		PathName:        id,
		PathFormat:      e.opts.RecordPath,
		SegmentDuration: e.opts.SegmentDuration,
		Record:          record,
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
	}

	if relayOn {
		rec.OnSource = func(desc *description.Session) error { return e.relay.SetSource(id, desc) }
		rec.OnRTP = func(m *description.Media, pkt *rtp.Packet) { e.relay.WritePacketRTP(id, m, pkt) }
	}

	if mseOn && lb != nil {
		rec.OnLiveTracks = func(its []*fmp4.InitTrack) error {
			if err := lb.SetTracks(its); err != nil {
				slog.Warn("media/live web disabled", "camera", id, "err", err)
			}
			return nil // a broadcaster issue must never stop recording
		}
		rec.OnLiveSample = func(trackID int, s *fmp4.Sample, dts int64) { lb.WriteSample(trackID, s, dts) }
	}

	rec.OnSourceLost = func() {
		if relayOn {
			e.relay.ClearSource(id)
		}
		if lb != nil {
			lb.Stop()
		}
	}

	ctrl := &camCtrl{rec: rec, resumeCh: make(chan struct{})}

	e.mu.Lock()
	e.recorders = append(e.recorders, rec)
	e.ctrls[id] = ctrl
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			// Park here while the camera is in privacy: the loop must not
			// reconnect until privacy is turned off (SetPrivacy resumes it).
			ctrl.waitWhilePaused(e.ctx)
			if e.ctx.Err() != nil {
				return
			}
			// rec.Start blocks until the stream ends; gortsplib's read timeout
			// (10s) turns a silent stall on a flaky/far camera into an error, so
			// this loop reconnects instead of hanging forever.
			backoff := time.Second
			err := rec.Start()
			// If privacy paused us, rec.Close() unblocked Start; skip the
			// reconnect log/backoff and loop straight back to park.
			if ctrl.isPaused() {
				slog.Info("media/recorder paused (privacy)", "camera", id)
				continue
			}
			if err != nil {
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

// SetPrivacy pauses or resumes a camera's media pipeline at runtime. Pausing
// (on=true) disconnects the recorder from the source: recording stops and the
// OnSourceLost hook tears down the live MSE broadcast and RTSP relay, so the
// camera neither records nor transmits until resumed. Resuming reconnects with a
// fresh session. Returns false when the camera isn't supervised by the engine
// (e.g. `privacy = false`, or no Source / all sinks off). Idempotent.
func (e *Engine) SetPrivacy(camID string, on bool) bool {
	e.mu.RLock()
	ctrl := e.ctrls[camID]
	e.mu.RUnlock()
	if ctrl == nil {
		return false
	}
	if ctrl.setPaused(on) && on {
		// Disconnect now; the loop then parks at waitWhilePaused. Closing
		// finalizes and indexes the in-progress segment (recorder.Close),
		// so nothing recorded so far is lost.
		ctrl.rec.Close()
	}
	return true
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

// RecordingEnabled reports whether the engine is writing segments to disk
// (i.e. [media] record = true). Playback endpoints use this to short-circuit
// to 404 instead of crashing on a nil index/playback.
func (e *Engine) RecordingEnabled() bool { return e.opts.RecordEnabled }

// GlobalToggles reports the engine's global [media] on/off switches. The API
// combines them with each camera's per-camera flags via camera.ResolveFeatures
// to advertise only the feeds the engine actually serves.
func (e *Engine) GlobalToggles() (mse, relay, record bool) {
	return e.opts.MSEEnabled, e.opts.RelayEnabled, e.opts.RecordEnabled
}

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
	if e.relay != nil {
		e.relay.Close()
	}
	if e.idx != nil {
		e.idx.Close()
	}
}
