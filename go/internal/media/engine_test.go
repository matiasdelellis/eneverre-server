package media

import (
	"context"
	"testing"
	"time"
)

// TestCamCtrlPauseResume exercises the retry-loop pause control the privacy
// endpoint drives: waitWhilePaused blocks while paused and unblocks on resume,
// and setPaused reports whether the state actually changed.
func TestCamCtrlPauseResume(t *testing.T) {
	c := &camCtrl{resumeCh: make(chan struct{})}

	if c.isPaused() {
		t.Fatal("new ctrl should not be paused")
	}
	// waitWhilePaused returns immediately when not paused.
	done := make(chan struct{})
	go func() { c.waitWhilePaused(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused blocked while not paused")
	}

	if !c.setPaused(true) {
		t.Fatal("setPaused(true) should report a change")
	}
	if c.setPaused(true) {
		t.Fatal("setPaused(true) again should report no change")
	}
	if !c.isPaused() {
		t.Fatal("ctrl should be paused")
	}

	// waitWhilePaused now blocks until resumed.
	blocked := make(chan struct{})
	go func() { c.waitWhilePaused(context.Background()); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("waitWhilePaused returned while still paused")
	case <-time.After(50 * time.Millisecond):
	}

	if !c.setPaused(false) {
		t.Fatal("setPaused(false) should report a change")
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused did not wake on resume")
	}
}

// TestCamCtrlWaitCancels ensures a paused waiter unblocks on context cancel so
// engine shutdown never hangs on a camera parked in privacy.
func TestCamCtrlWaitCancels(t *testing.T) {
	c := &camCtrl{resumeCh: make(chan struct{})}
	c.setPaused(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.waitWhilePaused(ctx); close(done) }()

	select {
	case <-done:
		t.Fatal("waitWhilePaused returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitWhilePaused did not return on context cancel")
	}
}
