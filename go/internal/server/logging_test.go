package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRW is a ResponseWriter that records a SetWriteDeadline call, standing
// in for the real http.conn writer that supports write deadlines.
type deadlineRW struct {
	http.ResponseWriter
	setCalled bool
}

func (d *deadlineRW) SetWriteDeadline(time.Time) error { d.setCalled = true; return nil }

// TestStatusRecorderUnwrapForDeadline guards the fix for the live feed being cut
// every 30s by the server WriteTimeout: the access-log wrapper must expose
// Unwrap so http.ResponseController can reach the underlying writer and clear
// the deadline. If Unwrap is dropped, SetWriteDeadline silently stops reaching
// the connection and the 30s reconnect/rebuffer regression returns.
func TestStatusRecorderUnwrapForDeadline(t *testing.T) {
	base := &deadlineRW{ResponseWriter: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: base, status: http.StatusOK}

	if err := http.NewResponseController(rec).SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline through statusRecorder: %v", err)
	}
	if !base.setCalled {
		t.Error("SetWriteDeadline did not reach the underlying writer (statusRecorder.Unwrap missing?)")
	}
}
