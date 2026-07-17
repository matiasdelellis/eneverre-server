package backchannel

import (
	"net"
	"testing"
	"time"
)

// TestSessionCloseIdempotent guards the double-close fix: two owners can race
// to close the same session (the talk handler's deferred Close and the
// shutdown path's CloseAllTalk), and before the sync.Once the second
// close(s.stop) panicked. This builds a minimal Session over an in-memory pipe
// (no real RTSP) and asserts a second Close is a harmless no-op.
func TestSessionCloseIdempotent(t *testing.T) {
	client, peer := net.Pipe()
	// Drain the peer end so the TEARDOWN write inside Close doesn't block on the
	// unbuffered pipe.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()

	s := &Session{
		rtspClient:    &rtspClient{conn: client},
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		keepaliveDone: make(chan struct{}),
		uri:           "/",
	}
	// Stand in for the send loop: signal done once stop is closed, which is what
	// Close waits on.
	go func() { <-s.stop; close(s.done) }()

	returned := make(chan struct{})
	go func() {
		s.Close()
		s.Close() // must be a no-op, not a panic on the already-closed channels
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Session.Close did not return (deadlock or blocked write)")
	}
}
