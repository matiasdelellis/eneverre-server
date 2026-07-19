// Package diskmonitor polls the free space on the recording volume and fires
// callbacks when it crosses a low-water mark. The media engine uses it to
// trigger an emergency purge of the oldest segments when space runs low, so a
// full disk drops the oldest footage instead of hitting a hard ENOSPC.
//
// Design notes:
//   - The watcher doesn't decide what "low" means beyond a byte threshold; the
//     engine owns the policy (kick off an emergency purge of the oldest
//     segments).
//   - Hysteresis: enter the low state when free < LowWater; exit only when
//     free >= 2*LowWater. Without the high-water gap the watcher would
//     oscillate at the threshold, re-firing OnLow on every poll.
//   - Statfs is injected so tests can drive transitions without touching a
//     real filesystem. In production it defaults to diskfree.Available.
//   - The state read is lock-free (atomics) so /api/status can read it without
//     contending on a mutex.
package diskmonitor

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"eneverre/internal/diskfree"
)

// StatfsFn returns the bytes available to the current process on the
// filesystem that holds path. Implementations: diskfree.Available in
// production; a closure in tests.
type StatfsFn func(path string) (uint64, error)

// Watcher polls the recording volume and reports a low-disk transition.
type Watcher struct {
	Path     string
	LowWater uint64        // bytes; below this, OnLow fires
	Interval time.Duration // poll cadence; defaults to 30s when zero
	Statfs   StatfsFn      // injected; defaults to diskfree.Available

	// OnLow is called once when free drops below LowWater. The watcher stays
	// in the low state (and won't fire OnLow again) until free recovers above
	// 2*LowWater. OnLow is invoked from the watcher's poll goroutine, so
	// blocking work should be dispatched elsewhere.
	OnLow func(free uint64)

	// OnRecovered is called once when free climbs back above 2*LowWater.
	OnRecovered func(free uint64)

	paused      atomic.Bool
	pausedSince atomic.Int64 // unix nano; 0 when not paused
	lastFree    atomic.Uint64
}

// New returns a Watcher for path with the given low-water mark. Statfs
// defaults to diskfree.Available; Interval defaults to 30s.
func New(path string, lowWater uint64) *Watcher {
	return &Watcher{
		Path:     path,
		LowWater: lowWater,
		Interval: 30 * time.Second,
		Statfs:   diskfree.Available,
	}
}

// Paused reports whether the watcher is currently in the low-disk state.
// Lock-free; safe to call from the camera retry loop's hot path.
func (w *Watcher) Paused() bool { return w.paused.Load() }

// PausedSince returns the time the watcher entered the low-disk state, or
// the zero time if it has never been paused.
func (w *Watcher) PausedSince() time.Time {
	ns := w.pausedSince.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// LastFree returns the most recent free-bytes sample, or 0 if Run has not
// polled yet.
func (w *Watcher) LastFree() uint64 { return w.lastFree.Load() }

// LowWaterBytes returns the configured low-water mark (for status output).
func (w *Watcher) LowWaterBytes() uint64 { return w.LowWater }

// Run polls Statfs at the configured Interval until ctx is cancelled. It
// transitions the paused state with hysteresis (enter when free < LowWater,
// exit when free >= 2*LowWater) and fires OnLow / OnRecovered exactly once
// per transition. A statfs error is logged and treated as "free unknown" —
// the watcher does not transition on it, so a transient statfs failure
// doesn't flap the system.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Take an initial sample so LastFree is non-zero from the first status
	// poll, without waiting a full interval.
	w.Tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs a single poll: statfs, transition the paused state, fire the
// matching callback. Exposed for tests; Run calls it on each interval.
func (w *Watcher) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if w.Statfs == nil {
		w.Statfs = diskfree.Available
	}
	free, err := w.Statfs(w.Path)
	if err != nil {
		slog.Warn("media: disk monitor statfs failed", "path", w.Path, "err", err)
		return
	}
	w.lastFree.Store(free)

	wasPaused := w.paused.Load()
	// Hysteresis: clear only when above the high-water mark (2x low).
	shouldPause := free < w.LowWater
	shouldResume := free >= 2*w.LowWater

	switch {
	case !wasPaused && shouldPause:
		w.paused.Store(true)
		w.pausedSince.Store(time.Now().UnixNano())
		if w.OnLow != nil {
			w.OnLow(free)
		}
	case wasPaused && shouldResume:
		w.paused.Store(false)
		w.pausedSince.Store(0)
		if w.OnRecovered != nil {
			w.OnRecovered(free)
		}
	}
}
