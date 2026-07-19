package diskmonitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStatfs is a StatfsFn driven by a slice of "samples": each Tick reads
// the head of the slice (or loops back to the tail) and returns the value.
// Tests use it to script the free-bytes timeline without touching a real
// filesystem.
type fakeStatfs struct {
	mu      sync.Mutex
	samples []uint64
	idx     int
	err     error
	calls   atomic.Int64
}

func (f *fakeStatfs) next(string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.Add(1)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.samples) == 0 {
		return 0, nil
	}
	v := f.samples[f.idx%len(f.samples)]
	f.idx++
	return v, nil
}

func (f *fakeStatfs) callCount() int64 { return f.calls.Load() }

func TestWatcherTransitions(t *testing.T) {
	fs := &fakeStatfs{}
	// 1 GiB low water. Script: 1.5G (above), 0.5G (below, fire OnLow),
	// 0.7G (still below, no event), 2.5G (above high-water 2G, fire OnRecovered),
	// 0.3G (below again, fire OnLow again).
	low := uint64(1 << 30)
	fs.samples = []uint64{
		3 * low / 2, // 1.5G: not low
		low / 2,     // 0.5G: low -> OnLow
		low * 7 / 10, // 0.7G: still low, no event
		5 * low / 2, // 2.5G: above 2G -> OnRecovered
		low / 4,     // 0.25G: low again
	}

	var (
		mu             sync.Mutex
		lowEvents      []uint64
		recoveredEvents []uint64
	)
	w := New("/some/path", low)
	w.Interval = time.Hour // disable the ticker; we drive Tick manually
	w.Statfs = fs.next
	w.OnLow = func(free uint64) {
		mu.Lock()
		defer mu.Unlock()
		lowEvents = append(lowEvents, free)
	}
	w.OnRecovered = func(free uint64) {
		mu.Lock()
		defer mu.Unlock()
		recoveredEvents = append(recoveredEvents, free)
	}

	ctx := context.Background()
	for range fs.samples {
		w.Tick(ctx)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(lowEvents), 2; got != want {
		t.Fatalf("OnLow fires = %d, want %d (events=%v)", got, want, lowEvents)
	}
	if got, want := lowEvents[0], uint64(low/2); got != want {
		t.Errorf("OnLow[0] = %d, want %d", got, want)
	}
	if got, want := lowEvents[1], uint64(low/4); got != want {
		t.Errorf("OnLow[1] = %d, want %d", got, want)
	}
	if got, want := len(recoveredEvents), 1; got != want {
		t.Fatalf("OnRecovered fires = %d, want %d (events=%v)", got, want, recoveredEvents)
	}
	if got, want := recoveredEvents[0], uint64(5*low/2); got != want {
		t.Errorf("OnRecovered[0] = %d, want %d", got, want)
	}
	if !w.Paused() {
		t.Error("watcher should be paused after the last (low) sample")
	}
	if w.LastFree() != low/4 {
		t.Errorf("LastFree = %d, want %d", w.LastFree(), low/4)
	}
}

// TestWatcherStatfsError verifies a statfs failure is logged and does not
// transition state (so a transient EAGAIN doesn't flap the system).
func TestWatcherStatfsError(t *testing.T) {
	fs := &fakeStatfs{err: errors.New("fake statfs boom")}
	w := New("/some/path", 1<<30)
	w.Interval = time.Hour
	w.Statfs = fs.next
	lowFired := false
	w.OnLow = func(uint64) { lowFired = true }

	w.Tick(context.Background())

	if lowFired {
		t.Error("OnLow fired on statfs error")
	}
	if w.Paused() {
		t.Error("watcher paused on statfs error")
	}
}

// TestWatcherDisabled verifies a zero LowWater means the check is inert.
// (The engine's caller is expected to skip starting the watcher entirely in
// that case; this test guards the low-level invariant.)
func TestWatcherDisabledByZero(t *testing.T) {
	fs := &fakeStatfs{}
	fs.samples = []uint64{0, 0, 0}
	w := New("/some/path", 0)
	w.Interval = time.Hour
	w.Statfs = fs.next
	lowFired := false
	w.OnLow = func(uint64) { lowFired = true }

	for range fs.samples {
		w.Tick(context.Background())
	}

	if lowFired {
		t.Error("OnLow fired with LowWater=0")
	}
	if w.Paused() {
		t.Error("watcher paused with LowWater=0")
	}
}
