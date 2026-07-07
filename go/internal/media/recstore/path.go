// Package recstore contains helpers to encode/decode recording segment paths.
// Ported and trimmed from MediaMTX's recordstore (fMP4 only).
package recstore

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

func leadingZeros(v int, size int) string {
	out := strconv.FormatInt(int64(v), 10)
	for len(out) < size {
		out = "0" + out
	}
	return out
}

func timeLocationEncode(t time.Time) string {
	_, off := t.Zone()
	if off == 0 {
		return "Z"
	}
	ret := "+"
	if off < 0 {
		ret = "-"
		off = -off
	}
	ret += leadingZeros(off/3600, 2)
	ret += leadingZeros((off/60)%60, 2)
	return ret
}

func timeLocationDecode(s string) *time.Location {
	if s == "Z" {
		return time.UTC
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	v1, _ := strconv.ParseInt(s[1:3], 10, 32)
	v2, _ := strconv.ParseInt(s[3:5], 10, 32)
	return time.FixedZone("myzone", sign*(int(v1)*3600+int(v2)*60))
}

// PathAddExtension adds the .mp4 extension (fMP4 only).
func PathAddExtension(path string) string {
	return path + ".mp4"
}

// CommonPath returns the fixed directory prefix (before any % specifier).
func CommonPath(v string) string {
	common := ""
	remaining := v
	for {
		i := strings.IndexAny(remaining, "\\/")
		if i < 0 {
			break
		}
		var part string
		part, remaining = remaining[:i+1], remaining[i+1:]
		if strings.Contains(part, "%") {
			break
		}
		common += part
	}
	if len(common) > 0 {
		common = common[:len(common)-1]
	}
	return common
}

// Path is a recording segment path with its start time.
type Path struct {
	Start time.Time
	Path  string
}

// Encode expands a format string with this path's fields.
func (p Path) Encode(format string) string {
	format = strings.ReplaceAll(format, "%path", p.Path)
	format = strings.ReplaceAll(format, "%Y", strconv.FormatInt(int64(p.Start.Year()), 10))
	format = strings.ReplaceAll(format, "%m", leadingZeros(int(p.Start.Month()), 2))
	format = strings.ReplaceAll(format, "%d", leadingZeros(p.Start.Day(), 2))
	format = strings.ReplaceAll(format, "%H", leadingZeros(p.Start.Hour(), 2))
	format = strings.ReplaceAll(format, "%M", leadingZeros(p.Start.Minute(), 2))
	format = strings.ReplaceAll(format, "%S", leadingZeros(p.Start.Second(), 2))
	format = strings.ReplaceAll(format, "%f", leadingZeros(p.Start.Nanosecond()/1000, 6))
	format = strings.ReplaceAll(format, "%z", timeLocationEncode(p.Start))
	format = strings.ReplaceAll(format, "%s", strconv.FormatInt(p.Start.Unix(), 10))
	return format
}

// Decode parses a filesystem path back into a Path, matching the format.
// Useful for bootstrapping the index from existing recordings.
func (p *Path) Decode(format string, v string) bool {
	re := format
	for _, ch := range []string{"\\", ".", "+", "*", "?", "^", "$", "(", ")", "[", "]", "{", "}", "|"} {
		re = strings.ReplaceAll(re, ch, "\\"+ch)
	}
	re = strings.ReplaceAll(re, "%path", "(.*?)")
	re = strings.ReplaceAll(re, "%Y", "([0-9]{4})")
	re = strings.ReplaceAll(re, "%m", "([0-9]{2})")
	re = strings.ReplaceAll(re, "%d", "([0-9]{2})")
	re = strings.ReplaceAll(re, "%H", "([0-9]{2})")
	re = strings.ReplaceAll(re, "%M", "([0-9]{2})")
	re = strings.ReplaceAll(re, "%S", "([0-9]{2})")
	re = strings.ReplaceAll(re, "%f", "([0-9]{6})")
	re = strings.ReplaceAll(re, "%z", "(Z|\\+[0-9]{4}|-[0-9]{4})")
	re = strings.ReplaceAll(re, "%s", "([0-9]{10})")
	r := regexp.MustCompile(re)

	var groupMapping []string
	cur := format
	for {
		i := strings.Index(cur, "%")
		if i < 0 {
			break
		}
		cur = cur[i:]
		for _, va := range []string{"%path", "%Y", "%m", "%d", "%H", "%M", "%S", "%f", "%z", "%s"} {
			if strings.HasPrefix(cur, va) {
				groupMapping = append(groupMapping, va)
			}
		}
		cur = cur[1:]
	}

	matches := r.FindStringSubmatch(v)
	if matches == nil {
		return false
	}

	values := make(map[string]string)
	for i, match := range matches[1:] {
		values[groupMapping[i]] = match
	}

	var year int
	var month time.Month = 1
	day := 1
	var hour, minute, second, micros int
	var unixSec int64 = -1
	loc := time.Local

	for k, val := range values {
		switch k {
		case "%path":
			p.Path = val
		case "%Y":
			t, _ := strconv.ParseInt(val, 10, 32)
			year = int(t)
		case "%m":
			t, _ := strconv.ParseInt(val, 10, 32)
			month = time.Month(int(t))
		case "%d":
			t, _ := strconv.ParseInt(val, 10, 32)
			day = int(t)
		case "%H":
			t, _ := strconv.ParseInt(val, 10, 32)
			hour = int(t)
		case "%M":
			t, _ := strconv.ParseInt(val, 10, 32)
			minute = int(t)
		case "%S":
			t, _ := strconv.ParseInt(val, 10, 32)
			second = int(t)
		case "%f":
			t, _ := strconv.ParseInt(val, 10, 32)
			micros = int(t)
		case "%z":
			loc = timeLocationDecode(val)
		case "%s":
			unixSec, _ = strconv.ParseInt(val, 10, 64)
		}
	}

	if unixSec > 0 {
		p.Start = time.Unix(unixSec, int64(micros)*1000)
	} else {
		p.Start = time.Date(year, month, day, hour, minute, second, micros*1000, loc)
	}
	return true
}
