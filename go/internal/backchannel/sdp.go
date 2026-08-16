package backchannel

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// sdpFormat describes one payload type's codec from an a=rtpmap line.
type sdpFormat struct {
	name      string // uppercased codec name, e.g. "PCMA", "MPEG4-GENERIC"
	clockRate int
	fmtp      string // raw a=fmtp parameters for the PT, "" when absent
}

// sdpMedia represents one m= line from an SDP block.
type sdpMedia struct {
	mediaType string
	port      int
	proto     string
	payloads  []int // payload types in m= order
	control   string
	direction string
	formats   map[int]sdpFormat // per-PT codec table from a=rtpmap lines
}

// parseSDP parses the raw SDP body into a slice of media descriptions. It
// extracts m=, a=control:, a=sendonly/recvonly/sendrecv, and every a=rtpmap:
// (kept as a per-payload-type table — several cameras, thingino among them,
// advertise a single backchannel track carrying several codecs).
func parseSDP(raw []byte) []sdpMedia {
	var medias []sdpMedia
	var current *sdpMedia

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "m=") {
			if current != nil {
				medias = append(medias, *current)
			}
			current = &sdpMedia{}
			parts := strings.Split(line[2:], " ")
			if len(parts) >= 4 {
				current.mediaType = parts[0]
				current.port, _ = strconv.Atoi(parts[1])
				current.proto = parts[2]
				for _, p := range parts[3:] {
					if pt, err := strconv.Atoi(p); err == nil {
						current.payloads = append(current.payloads, pt)
					}
				}
			}
		} else if current != nil {
			switch {
			case strings.HasPrefix(line, "a=control:"):
				current.control = line[10:]
			case strings.HasPrefix(line, "a=sendonly"):
				current.direction = "sendonly"
			case strings.HasPrefix(line, "a=recvonly"):
				current.direction = "recvonly"
			case strings.HasPrefix(line, "a=sendrecv"):
				current.direction = "sendrecv"
			case strings.HasPrefix(line, "a=rtpmap:"):
				// a=rtpmap:<pt> <codec>/<rate>[/<channels>]
				fields := strings.SplitN(line[9:], " ", 2)
				if len(fields) == 2 {
					pt, err := strconv.Atoi(strings.TrimSpace(fields[0]))
					if err != nil {
						continue
					}
					codecParts := strings.Split(fields[1], "/")
					f := sdpFormat{
						name: strings.ToUpper(strings.TrimSpace(codecParts[0])),
					}
					if len(codecParts) > 1 {
						f.clockRate, _ = strconv.Atoi(strings.TrimSpace(codecParts[1]))
					}
					if current.formats == nil {
						current.formats = make(map[int]sdpFormat)
					}
					current.formats[pt] = f
				}
			case strings.HasPrefix(line, "a=fmtp:"):
				// a=fmtp:<pt> <params>
				fields := strings.SplitN(line[7:], " ", 2)
				if len(fields) == 2 {
					pt, err := strconv.Atoi(strings.TrimSpace(fields[0]))
					if err != nil {
						continue
					}
					if current.formats == nil {
						current.formats = make(map[int]sdpFormat)
					}
					f := current.formats[pt]
					f.fmtp = strings.TrimSpace(fields[1])
					current.formats[pt] = f
				}
			}
		}
	}
	if current != nil {
		medias = append(medias, *current)
	}
	return medias
}

// format returns the codec table entry for pt.
func (m *sdpMedia) format(pt int) (sdpFormat, bool) {
	f, ok := m.formats[pt]
	return f, ok
}

// hasCodec reports whether the media advertises the codec (matched
// case-insensitively, with "AAC" covering both names) on any payload type.
func (m *sdpMedia) hasCodec(name string) bool {
	for _, pt := range m.payloads {
		if f, ok := m.formats[pt]; ok && codecMatches(f.name, name) {
			return true
		}
	}
	return false
}

// formatList renders the media's codec table for debug logs.
func (m *sdpMedia) formatList() []string {
	var out []string
	for _, pt := range m.payloads {
		if f, ok := m.formats[pt]; ok {
			out = append(out, fmt.Sprintf("%d=%s/%d", pt, f.name, f.clockRate))
		}
	}
	return out
}

// codecMatches compares an advertised codec name (already uppercased) with a
// wanted one; "AAC" is the canonical name for MPEG4-GENERIC.
func codecMatches(name, want string) bool {
	if strings.EqualFold(name, want) {
		return true
	}
	if strings.EqualFold(want, "AAC") && (strings.EqualFold(name, "MPEG4-GENERIC") || strings.EqualFold(name, "AAC")) {
		return true
	}
	return false
}

// canonicalCodec maps an advertised codec name to the session codec label.
func canonicalCodec(name string) string {
	if strings.EqualFold(name, "MPEG4-GENERIC") || strings.EqualFold(name, "AAC") {
		return "AAC"
	}
	return strings.ToUpper(name)
}

// findBackchannelMedia selects the best send-capable audio track from the SDP
// for the backchannel, returning the track and the codec to use on it. When
// forceCodec is "PCMA"/"PCMU"/"AAC" it narrows to tracks advertising that
// codec; when empty it prefers G.711 (PCMA over PCMU within a track) over AAC.
// The last fallback is any audio track, with the codec hint left empty for
// chooseCodec to resolve.
func findBackchannelMedia(medias []sdpMedia, forceCodec string) (*sdpMedia, string, error) {
	sendable := func(m sdpMedia) bool {
		return m.direction == "sendonly" || m.direction == "sendrecv"
	}
	audio := func(m sdpMedia) bool { return m.mediaType == "audio" }

	if forceCodec != "" {
		for _, m := range medias {
			if audio(m) && sendable(m) && m.hasCodec(forceCodec) {
				return &m, canonicalCodec(forceCodec), nil
			}
		}
		return nil, "", fmt.Errorf("no send-capable %s audio track in SDP", forceCodec)
	}

	for _, m := range medias {
		if audio(m) && sendable(m) {
			switch {
			case m.hasCodec("PCMA"):
				return &m, "PCMA", nil
			case m.hasCodec("PCMU"):
				return &m, "PCMU", nil
			}
		}
	}
	for _, m := range medias {
		if audio(m) && sendable(m) && m.hasCodec("AAC") {
			return &m, "AAC", nil
		}
	}
	for _, m := range medias {
		if audio(m) {
			return &m, "", nil
		}
	}
	return nil, "", fmt.Errorf("no backchannel audio track found in SDP")
}

// chooseCodec resolves the concrete codec and payload type for the selected
// SDP media. want is the codec hint from findBackchannelMedia ("PCMA"/"PCMU"/
// "AAC"); when empty it prefers PCMA, then PCMU, then AAC. Only payload types
// with a matching a=rtpmap entry are considered, so a multi-codec track picks
// the PT that actually belongs to the codec (e.g. thingino's track0). Without
// any rtpmap the static payload types are inferred (0=PCMU, 8=PCMA) and, as a
// last resort for unknown tracks, PCMA with the first payload type is returned
// to preserve the historical behavior.
func chooseCodec(m *sdpMedia, want string) (codec string, pt byte) {
	pt = 8
	if len(m.payloads) > 0 {
		pt = byte(m.payloads[0])
	}

	prefer := []string{"PCMA", "PCMU", "AAC"}
	if want != "" {
		prefer = []string{want}
	}
	for _, cand := range prefer {
		for _, p := range m.payloads {
			if f, ok := m.formats[p]; ok && codecMatches(f.name, cand) {
				return canonicalCodec(f.name), byte(p)
			}
		}
	}

	// No rtpmap at all: fall back on the static payload types.
	for _, cand := range prefer {
		if cand != "AAC" {
			for _, p := range m.payloads {
				if (cand == "PCMU" && p == 0) || (cand == "PCMA" && p == 8) {
					return cand, byte(p)
				}
			}
		}
	}
	return "PCMA", pt
}

// resolveControlURL resolves a (possibly relative) SDP a=control: URL against
// the session's base URL per RFC 2326.
func resolveControlURL(base *url.URL, control string) string {
	if control == "" || control == "*" {
		return base.String()
	}
	if strings.Contains(control, "://") {
		return control
	}
	b := base.String()
	if strings.HasPrefix(control, "/") {
		if u, err := url.Parse(b); err == nil {
			u.Path = control
			u.RawQuery = ""
			return u.String()
		}
	}
	if !strings.HasSuffix(b, "/") {
		b += "/"
	}
	return b + control
}
