// Package timeutil holds the shared timestamp parsing used across the API so
// the accepted formats can't drift between the recordings handlers and the
// events store (they used to keep near-identical, separately-maintained layout
// lists).
package timeutil

import "time"

// isoLayouts is the accepted ISO-8601 / RFC3339 set, most specific first:
// zoned with fractional seconds, zoned, then the naive (zone-less) forms which
// ParseISO reads as UTC.
var isoLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// ParseISO parses an ISO-8601 / RFC3339 timestamp, accepting an optional
// fractional-second part and a naive (no-zone) form treated as UTC. It returns
// the instant normalized to UTC and true, or the zero time and false. It does
// NOT accept bare unix seconds — callers that need that (see
// events.ParseTimestamp) handle it before delegating here.
func ParseISO(s string) (time.Time, bool) {
	for _, layout := range isoLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
