package server

import (
	"testing"

	"eneverre/internal/backchannel"
)

// TestCloseAllTalkClearsPlaceholders checks that shutdown teardown skips the
// nil placeholders a reservation leaves during Dial and clears the map without
// panicking. (Sessions with a live backchannel need a real RTSP peer, covered
// by the idempotency test in the backchannel package.)
func TestCloseAllTalkClearsPlaceholders(t *testing.T) {
	a := &App{talk: map[string]*backchannel.Session{"cam1": nil, "cam2": nil}}
	a.CloseAllTalk()
	if len(a.talk) != 0 {
		t.Fatalf("talk map not cleared: %d entries left", len(a.talk))
	}
}
