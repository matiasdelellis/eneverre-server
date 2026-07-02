package backchannel

import (
	"encoding/binary"
	"net/url"
	"testing"
)

// --- G.711 encoding -------------------------------------------------------

func TestG711Silence(t *testing.T) {
	// Canonical G.711 silence bytes: A-law 0xD5, µ-law 0xFF. These are fixed by
	// the standard, so they anchor the encoders against an external reference.
	if got := linearToALaw(0); got != 0xD5 {
		t.Errorf("linearToALaw(0) = %#x, want 0xD5", got)
	}
	if got := linearToULaw(0); got != 0xFF {
		t.Errorf("linearToULaw(0) = %#x, want 0xFF", got)
	}
}

func TestEncodeLength(t *testing.T) {
	in := make([]int16, 160)
	if got := len(encodeALaw(in)); got != 160 {
		t.Errorf("encodeALaw length = %d, want 160", got)
	}
	if got := len(encodeULaw(in)); got != 160 {
		t.Errorf("encodeULaw length = %d, want 160", got)
	}
	if got := len(encodeALaw(nil)); got != 0 {
		t.Errorf("encodeALaw(nil) length = %d, want 0", got)
	}
}

func TestEncodeSilenceBlock(t *testing.T) {
	in := make([]int16, 8) // all zero
	for i, b := range encodeALaw(in) {
		if b != 0xD5 {
			t.Errorf("encodeALaw silence[%d] = %#x, want 0xD5", i, b)
		}
	}
	for i, b := range encodeULaw(in) {
		if b != 0xFF {
			t.Errorf("encodeULaw silence[%d] = %#x, want 0xFF", i, b)
		}
	}
}

func TestEncodeDeterministicAndSigned(t *testing.T) {
	// Same input → same output, and a value differs from its negation (the sign
	// is actually encoded, not dropped).
	if linearToALaw(1000) != linearToALaw(1000) {
		t.Error("linearToALaw not deterministic")
	}
	if linearToALaw(1000) == linearToALaw(-1000) {
		t.Error("linearToALaw(1000) should differ from linearToALaw(-1000)")
	}
	if linearToULaw(1000) == linearToULaw(-1000) {
		t.Error("linearToULaw(1000) should differ from linearToULaw(-1000)")
	}
}

func TestAlawSegment(t *testing.T) {
	cases := []struct {
		val, want int
	}{
		{0, 0}, {0x1F, 0}, {0x20, 1}, {0xFFF, 7}, {0x1000, 8},
	}
	for _, c := range cases {
		if got := alawSegment(c.val); got != c.want {
			t.Errorf("alawSegment(%#x) = %d, want %d", c.val, got, c.want)
		}
	}
}

// --- Resampling -----------------------------------------------------------

func TestResampleLinearIdentity(t *testing.T) {
	in := []int16{1, 2, 3, 4}
	out := resampleLinear(in, 8000, 8000)
	if len(out) != len(in) {
		t.Fatalf("identity resample length = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("identity resample[%d] = %d, want %d", i, out[i], in[i])
		}
	}
}

func TestResampleLinearLengths(t *testing.T) {
	in := make([]int16, 100)
	if got := len(resampleLinear(in, 16000, 8000)); got != 50 {
		t.Errorf("downsample 16k→8k length = %d, want 50", got)
	}
	if got := len(resampleLinear(in, 8000, 16000)); got != 200 {
		t.Errorf("upsample 8k→16k length = %d, want 200", got)
	}
	if got := len(resampleLinear(nil, 48000, 8000)); got != 0 {
		t.Errorf("resample(nil) length = %d, want 0", got)
	}
}

func TestResampleLinearConstant(t *testing.T) {
	// Linear interpolation of a constant signal is that same constant.
	in := make([]int16, 48)
	for i := range in {
		in[i] = 1234
	}
	for i, v := range resampleLinear(in, 48000, 8000) {
		if v != 1234 {
			t.Errorf("constant resample[%d] = %d, want 1234", i, v)
		}
	}
}

func TestLowPassForDecimation(t *testing.T) {
	in := []int16{5, 5, 5, 5, 5, 5}
	// Anti-alias only applies when downsampling; equal/higher target is a no-op.
	if out := lowPassForDecimation(in, 8000, 8000); &out[0] != &in[0] {
		t.Error("lowPassForDecimation should return input unchanged when toRate >= fromRate")
	}
	// Moving average of a constant is that constant, and length is preserved.
	out := lowPassForDecimation(in, 48000, 8000)
	if len(out) != len(in) {
		t.Fatalf("lowPass length = %d, want %d", len(out), len(in))
	}
	for i, v := range out {
		if v != 5 {
			t.Errorf("lowPass constant[%d] = %d, want 5", i, v)
		}
	}
}

// --- RTP ------------------------------------------------------------------

func TestBuildRTPPacket(t *testing.T) {
	payload := []byte{0xAA, 0xBB, 0xCC}
	pkt := buildRTPPacket(8, 0x1234, 0x89ABCDEF, 0x01020304, true, payload)

	if len(pkt) != 12+len(payload) {
		t.Fatalf("packet length = %d, want %d", len(pkt), 12+len(payload))
	}
	if pkt[0] != 0x80 {
		t.Errorf("byte0 = %#x, want 0x80 (version 2)", pkt[0])
	}
	if pkt[1] != (0x80 | 8) {
		t.Errorf("byte1 = %#x, want %#x (marker + PT 8)", pkt[1], 0x80|8)
	}
	if got := binary.BigEndian.Uint16(pkt[2:4]); got != 0x1234 {
		t.Errorf("seq = %#x, want 0x1234", got)
	}
	if got := binary.BigEndian.Uint32(pkt[4:8]); got != 0x89ABCDEF {
		t.Errorf("timestamp = %#x, want 0x89ABCDEF", got)
	}
	if got := binary.BigEndian.Uint32(pkt[8:12]); got != 0x01020304 {
		t.Errorf("ssrc = %#x, want 0x01020304", got)
	}
	for i, b := range payload {
		if pkt[12+i] != b {
			t.Errorf("payload[%d] = %#x, want %#x", i, pkt[12+i], b)
		}
	}

	// No marker bit when marker=false.
	if pkt := buildRTPPacket(0, 1, 1, 1, false, nil); pkt[1] != 0 {
		t.Errorf("byte1 without marker = %#x, want 0x00", pkt[1])
	}
}

// --- SDP ------------------------------------------------------------------

const sampleSDP = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=Backchannel
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=recvonly
a=control:track1
m=audio 0 RTP/AVP 8
a=rtpmap:8 PCMA/8000
a=sendonly
a=control:rtsp://cam/track2
`

func TestParseSDP(t *testing.T) {
	medias := parseSDP([]byte(sampleSDP))
	if len(medias) != 2 {
		t.Fatalf("parsed %d media, want 2", len(medias))
	}
	audio := medias[1]
	if audio.mediaType != "audio" {
		t.Errorf("mediaType = %q, want audio", audio.mediaType)
	}
	if audio.direction != "sendonly" {
		t.Errorf("direction = %q, want sendonly", audio.direction)
	}
	if audio.codecName != "PCMA" {
		t.Errorf("codecName = %q, want PCMA", audio.codecName)
	}
	if audio.clockRate != 8000 {
		t.Errorf("clockRate = %d, want 8000", audio.clockRate)
	}
	if len(audio.payloads) != 1 || audio.payloads[0] != 8 {
		t.Errorf("payloads = %v, want [8]", audio.payloads)
	}
	if audio.control != "rtsp://cam/track2" {
		t.Errorf("control = %q, want rtsp://cam/track2", audio.control)
	}
}

func TestFindBackchannelMedia(t *testing.T) {
	medias := parseSDP([]byte(sampleSDP))

	// Auto-select: the send-capable G.711 audio track.
	m, err := findBackchannelMedia(medias, "")
	if err != nil {
		t.Fatalf("findBackchannelMedia auto: %v", err)
	}
	if m.codecName != "PCMA" || m.direction != "sendonly" {
		t.Errorf("auto-selected %s/%s, want PCMA/sendonly", m.codecName, m.direction)
	}

	// Forced codec that isn't present → error.
	if _, err := findBackchannelMedia(medias, "PCMU"); err == nil {
		t.Error("findBackchannelMedia(PCMU) should error when no PCMU track exists")
	}

	// No audio at all → error.
	videoOnly := parseSDP([]byte("m=video 0 RTP/AVP 96\na=rtpmap:96 H264/90000\na=recvonly\n"))
	if _, err := findBackchannelMedia(videoOnly, ""); err == nil {
		t.Error("findBackchannelMedia should error when there is no audio track")
	}
}

func TestChooseCodec(t *testing.T) {
	cases := []struct {
		media     sdpMedia
		wantCodec string
		wantPT    byte
	}{
		{sdpMedia{codecName: "PCMA", payloads: []int{8}}, "PCMA", 8},
		{sdpMedia{codecName: "PCMU", payloads: []int{0}}, "PCMU", 0},
		{sdpMedia{codecName: "", payloads: []int{0}}, "PCMU", 0},   // infer from PT 0
		{sdpMedia{codecName: "", payloads: []int{8}}, "PCMA", 8},   // infer from PT 8
		{sdpMedia{codecName: "PCMA", payloads: []int{18}}, "PCMA", 18}, // dynamic PT
	}
	for _, c := range cases {
		codec, pt := chooseCodec(&c.media)
		if codec != c.wantCodec || pt != c.wantPT {
			t.Errorf("chooseCodec(%+v) = %s/%d, want %s/%d",
				c.media, codec, pt, c.wantCodec, c.wantPT)
		}
	}
}

func TestResolveControlURL(t *testing.T) {
	base, _ := url.Parse("rtsp://cam:554/stream")
	cases := []struct {
		control, want string
	}{
		{"", "rtsp://cam:554/stream"},
		{"*", "rtsp://cam:554/stream"},
		{"rtsp://other/track2", "rtsp://other/track2"},
		{"track2", "rtsp://cam:554/stream/track2"},
		{"/track2", "rtsp://cam:554/track2"},
	}
	for _, c := range cases {
		if got := resolveControlURL(base, c.control); got != c.want {
			t.Errorf("resolveControlURL(%q) = %q, want %q", c.control, got, c.want)
		}
	}
}
