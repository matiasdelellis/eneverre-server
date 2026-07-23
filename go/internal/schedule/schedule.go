// Package schedule defines named recording programs: per weekday, the time
// windows during which a camera is "armed" (recording + transmitting). A camera
// references a schedule by id; the server's scheduler pauses the camera's media
// pipeline (via the same mechanism as the privacy toggle) outside the armed
// windows. A camera with no schedule is armed 24/7 — the historical default.
//
// Windows are evaluated in the server's local time zone. Each window is a
// half-open range [start, end): a sample at exactly the end minute is already
// outside the window. A window whose end is not after its start wraps past
// midnight into the next day (e.g. 22:00–02:00 arms from 22:00 today through
// 02:00 tomorrow).
package schedule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DayKeys are the JSON keys for each weekday, indexed by time.Weekday
// (Sunday = 0). The rules map uses these keys; unknown keys are rejected by
// Validate and ignored by the compiler.
var DayKeys = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// dayIndex maps a weekday key back to its time.Weekday index, or -1.
func dayIndex(key string) int {
	for i, k := range DayKeys {
		if k == key {
			return i
		}
	}
	return -1
}

// Schedule is a named recording program. Days maps a weekday key (see DayKeys)
// to the armed windows for that day, each formatted "HH:MM-HH:MM". A day absent
// from the map (or with an empty list) is off all day. CreatedAt is unix seconds
// and is set by the store on create.
type Schedule struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Days      map[string][]string `json:"days"`
	CreatedAt int64               `json:"created_at,omitempty"`
}

// interval is a compiled half-open window in minutes from midnight. start is in
// [0,1440); end is in (0,1440]. wrap is true when the window crosses midnight
// (end <= start), meaning it arms from start to 1440 on its own day and from 0
// to end on the following day.
type interval struct {
	start, end int
	wrap       bool
}

// compiled is a Schedule parsed into per-weekday intervals, ready for Active.
type compiled [7][]interval

// Active reports whether the schedule arms recording at time t (evaluated in
// t's location). A window that wraps past midnight also arms the early hours of
// the following day. An empty/nil schedule (no windows anywhere) is never
// active — a camera assigned an all-empty schedule records at no time.
func (s Schedule) Active(t time.Time) bool {
	c := s.compile()
	wd := int(t.Weekday())
	minute := t.Hour()*60 + t.Minute()

	// Windows starting on the current weekday.
	for _, iv := range c[wd] {
		if iv.wrap {
			if minute >= iv.start { // from start through midnight
				return true
			}
		} else if minute >= iv.start && minute < iv.end {
			return true
		}
	}
	// Windows that started yesterday and wrapped into today's early hours.
	prev := (wd + 6) % 7
	for _, iv := range c[prev] {
		if iv.wrap && minute < iv.end {
			return true
		}
	}
	return false
}

// compile parses the Days map into per-weekday intervals, silently dropping any
// malformed entry (Validate is the gate that rejects those on write, so a stored
// schedule is already well-formed; this stays defensive for hand-edited DBs).
func (s Schedule) compile() compiled {
	var c compiled
	for key, windows := range s.Days {
		di := dayIndex(strings.ToLower(key))
		if di < 0 {
			continue
		}
		for _, w := range windows {
			if iv, err := parseInterval(w); err == nil {
				c[di] = append(c[di], iv)
			}
		}
	}
	return c
}

// parseInterval parses one "HH:MM-HH:MM" window into a compiled interval.
func parseInterval(s string) (interval, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return interval{}, fmt.Errorf("window %q must be HH:MM-HH:MM", s)
	}
	start, err := parseMinutes(parts[0])
	if err != nil {
		return interval{}, err
	}
	end, err := parseMinutes(parts[1])
	if err != nil {
		return interval{}, err
	}
	if start >= 1440 {
		return interval{}, fmt.Errorf("window %q: start must be before 24:00", s)
	}
	if end == 0 {
		return interval{}, fmt.Errorf("window %q: end 00:00 is ambiguous (use 24:00 for end of day)", s)
	}
	if start == end {
		return interval{}, fmt.Errorf("window %q: start and end are equal", s)
	}
	return interval{start: start, end: end, wrap: end < start}, nil
}

// parseMinutes parses "HH:MM" into minutes from midnight. Hours 0–24 and
// minutes 0–59 are accepted; 24:00 (=1440) is allowed as an end-of-day marker.
func parseMinutes(s string) (int, error) {
	s = strings.TrimSpace(s)
	hm := strings.SplitN(s, ":", 2)
	if len(hm) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(hm[0]))
	if err != nil || h < 0 || h > 24 {
		return 0, fmt.Errorf("time %q: hour out of range", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(hm[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q: minute out of range", s)
	}
	total := h*60 + m
	if total > 1440 {
		return 0, fmt.Errorf("time %q: past 24:00", s)
	}
	return total, nil
}

// Validate checks a rules map for well-formedness: every key must be a known
// weekday, and every window must parse. It returns a human-readable message for
// the first problem (for a 422), or "" when the rules are valid. An empty map is
// valid (a schedule that records at no time — the operator's choice).
func Validate(days map[string][]string) string {
	for key, windows := range days {
		if dayIndex(strings.ToLower(key)) < 0 {
			return fmt.Sprintf("unknown day %q (use %s)", key, strings.Join(DayKeys[:], ", "))
		}
		for _, w := range windows {
			if _, err := parseInterval(w); err != nil {
				return err.Error()
			}
		}
	}
	return ""
}

// Normalize returns a cleaned copy of the rules: only known days with at least
// one window are kept, windows are trimmed and reformatted to a canonical
// "HH:MM-HH:MM", and each day's windows are sorted by start time. It assumes the
// input already passed Validate. This keeps what is stored and echoed back
// stable regardless of how the client formatted its input.
func Normalize(days map[string][]string) map[string][]string {
	out := make(map[string][]string, len(days))
	for _, key := range DayKeys {
		windows := days[key]
		if len(windows) == 0 {
			continue
		}
		type iv struct {
			start int
			text  string
		}
		parsed := make([]iv, 0, len(windows))
		for _, w := range windows {
			p, err := parseInterval(w)
			if err != nil {
				continue
			}
			parsed = append(parsed, iv{start: p.start, text: formatMinutes(p.start) + "-" + formatMinutes(p.end)})
		}
		if len(parsed) == 0 {
			continue
		}
		sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].start < parsed[j].start })
		texts := make([]string, len(parsed))
		for i, p := range parsed {
			texts[i] = p.text
		}
		out[key] = texts
	}
	return out
}

// formatMinutes renders minutes-from-midnight as "HH:MM" (1440 -> "24:00").
func formatMinutes(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}
