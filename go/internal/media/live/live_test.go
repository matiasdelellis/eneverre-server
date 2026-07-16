package live

import (
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// primeConnected white-box-initializes a broadcaster into the "connected" state
// without a real H264 SPS: the async marshal path (flushPartLocked) only needs
// the track IDs, timescales and initBytes — the SPS is only required to marshal
// the fMP4 init, which SetTracks does and this test doesn't exercise.
func primeConnected(b *Broadcaster) {
	b.mu.Lock()
	b.tracks = []*fmp4.InitTrack{{ID: 1, TimeScale: 90000}}
	b.timeScale = map[int]uint32{1: 90000}
	b.videoID = 1
	b.initBytes = []byte("fakeinit")
	b.mime = `video/mp4; codecs="avc1.42e01e"`
	b.running = true
	b.mu.Unlock()
}

// addSub registers a subscriber directly (bypassing HandleStream's HTTP loop) so
// the test can observe the fan-out.
func addSub(b *Broadcaster) *subscriber {
	sub := &subscriber{ch: make(chan []byte, 128)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

func vsample(keyframe bool, dur uint32) *fmp4.Sample {
	return &fmp4.Sample{
		Payload:         []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xaa, 0xbb},
		Duration:        dur,
		IsNonSyncSample: !keyframe,
	}
}

// TestBroadcasterAsyncFanout drives the async path exactly as the recorder does:
// Submit from a separate goroutine, drained by run(), marshalled off the caller's
// stack, and fanned out to a subscriber. Run under -race, it covers the channel
// handoff and the b.mu coordination the decoupling introduced.
func TestBroadcasterAsyncFanout(t *testing.T) {
	b := &Broadcaster{}
	b.Initialize()
	defer b.Close()
	primeConnected(b)
	sub := addSub(b)

	// Two keyframes: the second forces a part boundary that flushes the first.
	b.Submit(1, vsample(true, 3000), 0)
	b.Submit(1, vsample(true, 3000), 3000)

	select {
	case data := <-sub.ch:
		if len(data) == 0 {
			t.Fatal("subscriber received an empty part")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received no part within 2s (async marshaler stalled?)")
	}
}

// TestBroadcasterCloseIdempotentAndSubmitSafe verifies the teardown contract:
// Close can be called more than once (RemoveCamera then engine Close) without
// panicking, and a Submit racing in after Close never panics (it targets a
// buffered channel that is never closed).
func TestBroadcasterCloseIdempotentAndSubmitSafe(t *testing.T) {
	b := &Broadcaster{}
	b.Initialize()

	b.Close()
	b.Close() // second Close must be a no-op, not a panic

	// Submit after Close must not panic.
	b.Submit(1, vsample(true, 3000), 0)
}

// TestBroadcasterSubmitNeverBlocks ensures a burst far larger than the channel
// buffer never blocks the caller (the recorder calls Submit under its RTP mutex).
func TestBroadcasterSubmitNeverBlocks(t *testing.T) {
	b := &Broadcaster{}
	b.Initialize()
	defer b.Close()
	// Not primed: writeSample early-returns on !running, but Submit must stay
	// non-blocking regardless of whether the marshaler keeps up.

	done := make(chan struct{})
	go func() {
		for i := 0; i < submitBuffer*10; i++ {
			b.Submit(1, vsample(i%5 == 0, 3000), int64(i*3000))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked under a burst larger than the buffer")
	}
}

// TestBroadcasterConcurrentSubmitAndReset stresses the lock coordination:
// concurrent Submits (recorder side) against SetTracks/Stop (connect/disconnect
// side). Meaningful under -race; it must simply not deadlock or race.
func TestBroadcasterConcurrentSubmitAndReset(t *testing.T) {
	b := &Broadcaster{}
	b.Initialize()
	defer b.Close()
	primeConnected(b)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			b.Submit(1, vsample(i%10 == 0, 3000), int64(i*3000))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			b.Stop()
			primeConnected(b)
		}
	}()
	wg.Wait()
}
