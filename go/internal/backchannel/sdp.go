package backchannel

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// sdpMedia represents one m= line from an SDP block.
type sdpMedia struct {
	mediaType string
	port      int
	proto     string
	payloads  []int
	control   string
	direction string
	codecName string
	clockRate int
}

// parseSDP parses the raw SDP body into a slice of media descriptions. Only
// m=, a=control:, a=sendonly/recvonly/sendrecv, and a=rtpmap: are extracted.
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
				fields := strings.SplitN(line[9:], " ", 2)
				if len(fields) == 2 {
					codecParts := strings.Split(fields[1], "/")
					current.codecName = strings.ToUpper(strings.TrimSpace(codecParts[0]))
					if len(codecParts) > 1 {
						current.clockRate, _ = strconv.Atoi(strings.TrimSpace(codecParts[1]))
					}
				}
			}
		}
	}
	if current != nil {
		medias = append(medias, *current)
	}
	return medias
}

// findBackchannelMedia selects the best send-capable audio track from the SDP
// for the backchannel. When forceCodec is "PCMA"/"PCMU"/"AAC" it narrows to
// that codec; when empty it prefers G.711 (PCMA/PCMU) over AAC. The last
// fallback is any audio track.
func findBackchannelMedia(medias []sdpMedia, forceCodec string) (*sdpMedia, error) {
	sendable := func(m sdpMedia) bool {
		return m.direction == "sendonly" || m.direction == "sendrecv"
	}
	supported := func(m sdpMedia) bool {
		return m.codecName == "PCMA" || m.codecName == "PCMU"
	}

	if forceCodec == "AAC" {
		for _, m := range medias {
			if m.mediaType == "audio" && sendable(m) &&
				(m.codecName == "MPEG4-GENERIC" || m.codecName == "AAC") {
				return &m, nil
			}
		}
		return nil, fmt.Errorf("no send-capable AAC audio track in SDP")
	}

	if forceCodec != "" {
		for _, m := range medias {
			if m.mediaType == "audio" && sendable(m) && strings.EqualFold(m.codecName, forceCodec) {
				return &m, nil
			}
		}
		return nil, fmt.Errorf("no send-capable %s audio track in SDP", forceCodec)
	}

	for _, m := range medias {
		if m.mediaType == "audio" && sendable(m) && supported(m) {
			return &m, nil
		}
	}
	for _, m := range medias {
		if m.mediaType == "audio" && sendable(m) {
			return &m, nil
		}
	}
	for _, m := range medias {
		if m.mediaType == "audio" {
			return &m, nil
		}
	}
	return nil, fmt.Errorf("no backchannel audio track found in SDP")
}

// chooseCodec picks the codec string and payload type from the selected SDP
// media. Inferrence from static payload types (0=PCMU, 8=PCMA) is a fallback
// for SDPs that omit a=rtpmap for well-known types.
func chooseCodec(m *sdpMedia) (codec string, pt byte) {
	pt = 8
	if len(m.payloads) > 0 {
		pt = byte(m.payloads[0])
	}
	switch m.codecName {
	case "PCMA":
		return "PCMA", pt
	case "PCMU":
		return "PCMU", pt
	case "MPEG4-GENERIC", "AAC":
		return "AAC", pt
	}
	if len(m.payloads) > 0 {
		switch m.payloads[0] {
		case 0:
			return "PCMU", 0
		case 8:
			return "PCMA", 8
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
