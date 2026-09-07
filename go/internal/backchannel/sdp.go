package backchannel

import (
	"fmt"
	"strings"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// The negotiated backchannel codec labels, as reported by Session.Codec and
// accepted by Dial's forceCodec argument.
const (
	codecPCMA = "PCMA"
	codecPCMU = "PCMU"
	codecAAC  = "AAC"
	codecOPUS = "OPUS"
)

// backchannelPick is the outcome of choosing what to talk on: the media to
// SETUP, the format to packetize with, and the codec label for the client.
type backchannelPick struct {
	media *description.Media
	forma format.Format
	codec string
}

// selectBackchannel chooses the backchannel media and codec from a parsed
// session description: forceCodec ("PCMA"/"PCMU"/"AAC"/"OPUS") narrows to
// tracks advertising that codec, and empty prefers G.711 (PCMA over PCMU
// within a track), then AAC, then Opus.
//
// Only tracks the SDP advertises as send-capable are considered. The
// hand-rolled parser had one more step — talk to any audio track at all when
// nothing was advertised — which was dropped: a camera that never says it
// accepts audio (a TP-Link, for one) accepts the RTP and does nothing with it,
// so the fallback bought a working-looking session that plays no sound.
//
// gortsplib decides "is this a back channel?" from the `a=sendonly` attribute
// alone, and additionally unmarks every back channel when an SDP contains
// nothing else (pkg/description/session.go). Both are stricter than what
// cameras do in practice, so send-capability is re-derived here from the raw
// SDP directions and the pick is force-marked in prepareMedia.
func selectBackchannel(desc *description.Session, rawSDP, forceCodec string) (*backchannelPick, error) {
	dirs := sdpDirections(rawSDP)
	// Directions are matched to medias positionally; gortsplib keeps m=
	// sections 1:1 with desc.Medias, but if anything ever diverges, fall back
	// to what gortsplib parsed rather than mislabelling tracks.
	if len(dirs) != len(desc.Medias) {
		dirs = nil
	}

	var sendable []*description.Media
	for i, m := range desc.Medias {
		if m.Type != description.MediaTypeAudio {
			continue
		}
		dir := ""
		if dirs != nil {
			dir = dirs[i]
		}
		if m.IsBackChannel || dir == "sendonly" || dir == "sendrecv" {
			sendable = append(sendable, m)
		}
	}
	if len(sendable) == 0 {
		return nil, fmt.Errorf("camera advertises no send-capable audio track (no a=sendonly or a=sendrecv)")
	}

	if want := canonicalCodec(forceCodec); want != "" {
		for _, m := range sendable {
			if f := formatFor(m, want); f != nil {
				return &backchannelPick{media: m, forma: f, codec: want}, nil
			}
		}
		return nil, fmt.Errorf("no send-capable %s audio track in SDP", want)
	}

	// G.711 first (and PCMA before PCMU within one track), then AAC, then Opus
	// — each pass scanning every send-capable track before the next codec.
	for _, pass := range [][]string{{codecPCMA, codecPCMU}, {codecAAC}, {codecOPUS}} {
		for _, m := range sendable {
			for _, want := range pass {
				if f := formatFor(m, want); f != nil {
					return &backchannelPick{media: m, forma: f, codec: want}, nil
				}
			}
		}
	}

	// A send-capable track whose payload types carry no usable a=rtpmap parses
	// into format.Generic, which has no encoder. The camera did say it accepts
	// audio, so assume G.711 on it rather than giving up.
	for _, m := range sendable {
		if f, codec := assumeG711(m); f != nil {
			return &backchannelPick{media: m, forma: f, codec: codec}, nil
		}
	}
	return nil, fmt.Errorf("no send-capable audio track offers a codec we can encode")
}

// prepareMedia pins the media to the chosen format and marks it as a back
// channel. IsBackChannel is a plain public field, and gortsplib reads it at
// SETUP time to send the ONVIF Require header and build an RTP *sender* for
// the media instead of a receiver — so setting it here is what lets the
// tolerant selection above survive gortsplib's stricter parse. Narrowing
// Formats to the single chosen one also guarantees Client.WritePacketRTP
// resolves our payload type (it panics on an unknown one).
func (p *backchannelPick) prepareMedia() {
	p.media.IsBackChannel = true
	p.media.Formats = []format.Format{p.forma}
}

// formatFor returns the media's format for the wanted codec, or nil.
func formatFor(m *description.Media, want string) format.Format {
	for _, f := range m.Formats {
		if formatCodec(f) == want {
			return f
		}
	}
	return nil
}

// formatCodec maps a parsed RTP format to our codec label, or "" for anything
// we cannot packetize.
func formatCodec(f format.Format) string {
	switch v := f.(type) {
	case *format.G711:
		if v.MULaw {
			return codecPCMU
		}
		return codecPCMA
	case *format.MPEG4Audio:
		return codecAAC
	case *format.Opus:
		return codecOPUS
	}
	return ""
}

// assumeG711 synthesizes a G.711 format for a track whose payload type carries
// no usable a=rtpmap — gortsplib parses those into format.Generic, which has no
// encoder. Static payload types are read per RFC 3551 (0=PCMU, 8=PCMA) and
// anything else is assumed to be PCMA, preserving the behavior of the
// hand-rolled parser. The synthesized format replaces the Generic one in
// prepareMedia, so the RTP clock and payload type stay consistent.
func assumeG711(m *description.Media) (format.Format, string) {
	if len(m.Formats) == 0 {
		return nil, ""
	}
	pt := m.Formats[0].PayloadType()
	if pt == 0 {
		return &format.G711{PayloadTyp: 0, MULaw: true, SampleRate: 8000, ChannelCount: 1}, codecPCMU
	}
	return &format.G711{PayloadTyp: pt, MULaw: false, SampleRate: 8000, ChannelCount: 1}, codecPCMA
}

// canonicalCodec normalizes a caller-supplied codec hint to one of our labels,
// accepting the SDP spellings cameras and clients use. Unknown names return ""
// (auto-select).
func canonicalCodec(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case codecPCMA, "ALAW", "G711A":
		return codecPCMA
	case codecPCMU, "ULAW", "MULAW", "G711U":
		return codecPCMU
	case codecAAC, "MPEG4-GENERIC":
		return codecAAC
	case codecOPUS:
		return codecOPUS
	}
	return ""
}

// sdpDirections extracts the direction attribute of every m= section of a raw
// SDP, in order, as "sendonly"/"recvonly"/"sendrecv"/"inactive" or "" when the
// section declares none. Only the directions are read here; everything else
// about the SDP comes from gortsplib's parse.
func sdpDirections(raw string) []string {
	var dirs []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			dirs = append(dirs, "")
		case len(dirs) == 0:
			// session-level attribute, not ours to read
		case strings.HasPrefix(line, "a="):
			switch strings.TrimSpace(strings.TrimPrefix(line, "a=")) {
			case "sendonly", "recvonly", "sendrecv", "inactive":
				dirs[len(dirs)-1] = strings.TrimSpace(strings.TrimPrefix(line, "a="))
			}
		}
	}
	return dirs
}

// probeLabels maps the send-capable audio tracks of a session description to
// the client-facing codec labels: "aac" for an MPEG4-GENERIC track, "g711" for
// PCMA/PCMU, "opus" for Opus. One track can yield several labels (thingino
// advertises a single backchannel track carrying AAC, Opus and G.711 payload
// types); labels are deduplicated and ordered as they appear in the SDP.
// Unsupported codecs are skipped.
func probeLabels(desc *description.Session, rawSDP string) []string {
	dirs := sdpDirections(rawSDP)
	if len(dirs) != len(desc.Medias) {
		dirs = nil
	}

	var labels []string
	seen := map[string]bool{}
	for i, m := range desc.Medias {
		if m.Type != description.MediaTypeAudio {
			continue
		}
		dir := ""
		if dirs != nil {
			dir = dirs[i]
		}
		if !m.IsBackChannel && dir != "sendonly" && dir != "sendrecv" {
			continue
		}
		for _, f := range m.Formats {
			var label string
			switch formatCodec(f) {
			case codecAAC:
				label = "aac"
			case codecPCMA, codecPCMU:
				label = "g711"
			case codecOPUS:
				label = "opus"
			default:
				continue
			}
			if !seen[label] {
				seen[label] = true
				labels = append(labels, label)
			}
		}
	}
	return labels
}
