package schedule

import (
	"testing"
	"time"
)

// at builds a local time for a given weekday-ish date. 2024-01-01 is a Monday,
// so 2024-01-01+n lands on a predictable weekday: +0 Mon, +1 Tue, ... +6 Sun.
func at(dayOffset, hour, min int) time.Time {
	return time.Date(2024, 1, 1+dayOffset, hour, min, 0, 0, time.Local)
}

func TestActiveWithinWindow(t *testing.T) {
	s := Schedule{Days: map[string][]string{"mon": {"08:00-18:00"}}}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"before start", at(0, 7, 59), false},
		{"at start", at(0, 8, 0), true},
		{"midday", at(0, 12, 0), true},
		{"just before end", at(0, 17, 59), true},
		{"at end is exclusive", at(0, 18, 0), false},
		{"after end", at(0, 18, 1), false},
		{"other weekday off", at(1, 12, 0), false}, // Tuesday, no window
	}
	for _, c := range cases {
		if got := s.Active(c.t); got != c.want {
			t.Errorf("%s: Active(%s)=%v, want %v", c.name, c.t.Format("Mon 15:04"), got, c.want)
		}
	}
}

func TestActiveWrapPastMidnight(t *testing.T) {
	// Friday 22:00 -> Saturday 02:00.
	s := Schedule{Days: map[string][]string{"fri": {"22:00-02:00"}}}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"fri before window", at(4, 21, 59), false}, // Fri = offset 4
		{"fri at start", at(4, 22, 0), true},
		{"fri late", at(4, 23, 30), true},
		{"sat early still armed", at(5, 1, 59), true}, // Sat = offset 5
		{"sat at wrap end exclusive", at(5, 2, 0), false},
		{"sat later off", at(5, 3, 0), false},
		{"sat evening off", at(5, 22, 0), false}, // no Saturday window of its own
	}
	for _, c := range cases {
		if got := s.Active(c.t); got != c.want {
			t.Errorf("%s: Active(%s)=%v, want %v", c.name, c.t.Format("Mon 15:04"), got, c.want)
		}
	}
}

func TestActiveWrapAcrossWeekBoundary(t *testing.T) {
	// Sunday 23:00 -> Monday 01:00 exercises the sun->mon (index 0->1) wrap.
	s := Schedule{Days: map[string][]string{"sun": {"23:00-01:00"}}}
	if !s.Active(at(6, 23, 30)) { // Sunday = offset 6
		t.Error("expected armed Sunday 23:30")
	}
	if !s.Active(at(0, 0, 30)) { // Monday = offset 0, early hours from Sunday's wrap
		t.Error("expected armed Monday 00:30 (wrapped from Sunday)")
	}
	if s.Active(at(0, 1, 0)) {
		t.Error("expected off Monday 01:00 (wrap end exclusive)")
	}
}

func TestActiveMultipleWindows(t *testing.T) {
	s := Schedule{Days: map[string][]string{"mon": {"08:00-12:00", "14:00-18:00"}}}
	if !s.Active(at(0, 9, 0)) {
		t.Error("expected armed in first window")
	}
	if s.Active(at(0, 13, 0)) {
		t.Error("expected off in the gap between windows")
	}
	if !s.Active(at(0, 15, 0)) {
		t.Error("expected armed in second window")
	}
}

func TestActiveFullDay(t *testing.T) {
	s := Schedule{Days: map[string][]string{"mon": {"00:00-24:00"}}}
	if !s.Active(at(0, 0, 0)) || !s.Active(at(0, 23, 59)) {
		t.Error("expected armed all Monday for 00:00-24:00")
	}
}

func TestActiveEmpty(t *testing.T) {
	if (Schedule{}).Active(at(0, 12, 0)) {
		t.Error("empty schedule should never be active")
	}
}

func TestValidate(t *testing.T) {
	if msg := Validate(map[string][]string{"mon": {"08:00-18:00"}}); msg != "" {
		t.Errorf("valid rules rejected: %s", msg)
	}
	if msg := Validate(map[string][]string{"funday": {"08:00-18:00"}}); msg == "" {
		t.Error("unknown day should be rejected")
	}
	bad := []string{"8-18", "08:00", "25:00-26:00", "08:00-08:00", "10:00-00:00"}
	for _, w := range bad {
		if msg := Validate(map[string][]string{"mon": {w}}); msg == "" {
			t.Errorf("window %q should be rejected", w)
		}
	}
}

func TestNormalize(t *testing.T) {
	out := Normalize(map[string][]string{
		"mon": {"14:00-18:00", " 8:00-12:00 "}, // out of order, sloppy spacing/format
		"tue": {},                              // dropped (empty)
		"wed": {"garbage"},                     // dropped (unparseable) -> day dropped
	})
	if got := out["mon"]; len(got) != 2 || got[0] != "08:00-12:00" || got[1] != "14:00-18:00" {
		t.Errorf("mon not normalized/sorted: %v", got)
	}
	if _, ok := out["tue"]; ok {
		t.Error("empty tue should be dropped")
	}
	if _, ok := out["wed"]; ok {
		t.Error("unparseable wed should be dropped")
	}
}
