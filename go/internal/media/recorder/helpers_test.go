package recorder

import (
	"testing"
	"time"
)

func TestTimestampToDuration(t *testing.T) {
	cases := []struct {
		name      string
		ts        int64
		clockRate int
		want      time.Duration
	}{
		{"one second at 90kHz", 90000, 90000, time.Second},
		{"half second at 90kHz", 45000, 90000, 500 * time.Millisecond},
		{"one second at 8kHz", 8000, 8000, time.Second},
		{"one 90kHz tick", 1, 90000, time.Duration(1_000_000_000 / 90000)},
		{"zero", 0, 90000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := timestampToDuration(c.ts, c.clockRate); got != c.want {
				t.Fatalf("timestampToDuration(%d, %d) = %v, want %v", c.ts, c.clockRate, got, c.want)
			}
		})
	}
}

// TestMultiplyAndDivideNoOverflow checks the split secs/remainder form avoids
// the int64 overflow a naive v*m/d would hit for large dts values.
func TestMultiplyAndDivideNoOverflow(t *testing.T) {
	// 1e5 seconds of 90kHz ticks: naive 9e9 * 1e9 overflows int64 (~9.2e18).
	const ticks = int64(9_000_000_000) // 1e5 s at 90kHz
	got := timestampToDuration(ticks, 90000)
	want := 100_000 * time.Second
	if got != want {
		t.Fatalf("timestampToDuration(%d, 90000) = %v, want %v", ticks, got, want)
	}
}
