package backchannel

import (
	"encoding/binary"
	"net/url"
	"strings"
	"testing"
	"time"
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

// --- AAC (RFC 3640 framing) -----------------------------------------------

func TestAACRTPPayload(t *testing.T) {
	au := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	p := aacRTPPayload(au, 13, 3)

	// 2-byte AU-headers-length + one 2-byte AU-header + the AU.
	if len(p) != 4+len(au) {
		t.Fatalf("payload length = %d, want %d", len(p), 4+len(au))
	}
	// AU-headers-length is 16 bits (one 2-byte header): high byte 0, low byte 16.
	if p[0] != 0x00 || p[1] != 16 {
		t.Errorf("AU-headers-length = %#x %#x, want 0x00 0x10", p[0], p[1])
	}
	// AU-header: 13-bit size (left-shifted 3) with a 3-bit index of 0.
	if got := binary.BigEndian.Uint16(p[2:4]); got != uint16(len(au))<<3 {
		t.Errorf("AU-header = %#x, want %#x", got, uint16(len(au))<<3)
	}
	for i, b := range au {
		if p[4+i] != b {
			t.Errorf("AU byte[%d] = %#x, want %#x", i, p[4+i], b)
		}
	}
}

func TestAACRTPPayloadCustomFraming(t *testing.T) {
	// sizelength=6;indexlength=2 → one 8-bit AU-header.
	au := []byte{0xAA, 0xBB}
	p := aacRTPPayload(au, 6, 2)
	if len(p) != 3+len(au) {
		t.Fatalf("payload length = %d, want %d", len(p), 3+len(au))
	}
	if binary.BigEndian.Uint16(p[0:2]) != 8 {
		t.Errorf("AU-headers-length = %d, want 8", binary.BigEndian.Uint16(p[0:2]))
	}
	if p[2] != byte(len(au))<<2 {
		t.Errorf("AU-header byte = %#x, want %#x", p[2], byte(len(au))<<2)
	}
}

func TestParseAACParams(t *testing.T) {
	cases := []struct {
		name    string
		fmtp    string
		want    aacParams
		wantErr bool
	}{
		{
			"thingino AAC-hbr defaults",
			"streamtype=5;profile-level-id=15;mode=AAC-hbr;config=1188;sizelength=13;indexlength=3;indexdeltalength=3",
			aacParams{sizeLen: 13, indexLen: 3, frameSamples: 1024}, false,
		},
		{
			"bare minimum",
			"mode=AAC-hbr;config=1188",
			aacParams{sizeLen: 13, indexLen: 3, frameSamples: 1024}, false,
		},
		{
			"HE-AAC (SBR) frame length",
			"mode=AAC-hbr;config=2B92",
			aacParams{sizeLen: 13, indexLen: 3, frameSamples: 2048}, false,
		},
		{
			"AAC-LD frame length",
			"mode=AAC-hbr;config=BF00",
			aacParams{sizeLen: 13, indexLen: 3, frameSamples: 512}, false,
		},
		{
			"custom AU-header size",
			"mode=AAC-hbr;config=1188;sizelength=6;indexlength=2",
			aacParams{sizeLen: 6, indexLen: 2, frameSamples: 1024}, false,
		},
		{"no mode is allowed", "config=1188", aacParams{sizeLen: 13, indexLen: 3, frameSamples: 1024}, false},
		{"missing config", "mode=AAC-hbr;sizelength=13", aacParams{}, true},
		{"ADTS mode rejected", "mode=AAC-hbr-adts;config=1188", aacParams{}, true},
		{"bad config hex", "mode=AAC-hbr;config=zz", aacParams{}, true},
		{"AU-header too wide", "mode=AAC-hbr;config=1188;sizelength=13;indexlength=5", aacParams{}, true},
	}
	for _, c := range cases {
		got, err := parseAACParams(c.fmtp)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: parseAACParams(%q) succeeded, want error", c.name, c.fmtp)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseAACParams(%q): %v", c.name, c.fmtp, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: parseAACParams(%q) = %+v, want %+v", c.name, c.fmtp, got, c.want)
		}
	}
}

func TestIsADTS(t *testing.T) {
	adts := []byte{0xFF, 0xF1, 0x00, 0x00, 0x00, 0x00, 0x00} // MPEG-4, no CRC
	if !isADTS(adts) {
		t.Error("isADTS should detect a valid ADTS syncword")
	}
	if isADTS([]byte{0xFF, 0x00, 0, 0, 0, 0, 0}) {
		t.Error("isADTS should reject a bad second byte")
	}
	if isADTS([]byte{0xFF, 0xF1}) {
		t.Error("isADTS should reject a too-short buffer")
	}
	// Raw AAC AU (no syncword) must not be mistaken for ADTS.
	if isADTS([]byte{0x21, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00}) {
		t.Error("isADTS should reject a raw access unit")
	}
}

func TestADTSHeaderLen(t *testing.T) {
	// Protection-absent bit set (…F1) → no CRC → 7-byte header.
	if got := adtsHeaderLen([]byte{0xFF, 0xF1, 0, 0, 0, 0, 0}); got != 7 {
		t.Errorf("adtsHeaderLen(no CRC) = %d, want 7", got)
	}
	// Protection-absent bit clear (…F0) → CRC present → 9-byte header.
	if got := adtsHeaderLen([]byte{0xFF, 0xF0, 0, 0, 0, 0, 0, 0, 0}); got != 9 {
		t.Errorf("adtsHeaderLen(CRC) = %d, want 9", got)
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
	f, ok := audio.format(8)
	if !ok {
		t.Fatal("no format for pt 8")
	}
	if f.name != "PCMA" {
		t.Errorf("format name = %q, want PCMA", f.name)
	}
	if f.clockRate != 8000 {
		t.Errorf("format clockRate = %d, want 8000", f.clockRate)
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
	m, codec, err := findBackchannelMedia(medias, "")
	if err != nil {
		t.Fatalf("findBackchannelMedia auto: %v", err)
	}
	if m.direction != "sendonly" || codec != "PCMA" {
		t.Errorf("auto-selected %s/%s, want sendonly/PCMA", m.direction, codec)
	}

	// Forced codec that isn't present → error.
	if _, _, err := findBackchannelMedia(medias, "PCMU"); err == nil {
		t.Error("findBackchannelMedia(PCMU) should error when no PCMU track exists")
	}

	// No audio at all → error.
	videoOnly := parseSDP([]byte("m=video 0 RTP/AVP 96\na=rtpmap:96 H264/90000\na=recvonly\n"))
	if _, _, err := findBackchannelMedia(videoOnly, ""); err == nil {
		t.Error("findBackchannelMedia should error when there is no audio track")
	}
}

// aacSDP mirrors the real camera: a video track plus recvonly AAC, then three
// send-capable backchannels (AAC, then G.711 µ-law and A-law).
const aacSDP = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=Backchannel
m=video 0 RTP/AVP 96
a=rtpmap:96 H264/90000
a=control:track1
m=audio 0 RTP/AVP 97
a=rtpmap:97 MPEG4-GENERIC/16000
a=control:track2
m=audio 0 RTP/AVP 97
a=rtpmap:97 MPEG4-GENERIC/16000
a=sendonly
a=control:track3
m=audio 0 RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendonly
a=control:track4
m=audio 0 RTP/AVP 8
a=rtpmap:8 PCMA/8000
a=sendonly
a=control:track5
`

func TestFindBackchannelMediaAAC(t *testing.T) {
	medias := parseSDP([]byte(aacSDP))

	// Auto-select still prefers G.711 (the first supported send-capable track).
	m, codec, err := findBackchannelMedia(medias, "")
	if err != nil {
		t.Fatalf("auto-select: %v", err)
	}
	if codec != "PCMU" {
		t.Errorf("auto-selected %q, want PCMU (G.711 preferred over AAC)", codec)
	}

	// Forcing AAC picks the send-capable MPEG4-GENERIC track, not the recvonly one.
	m, codec, err = findBackchannelMedia(medias, "AAC")
	if err != nil {
		t.Fatalf("force AAC: %v", err)
	}
	if codec != "AAC" || m.direction != "sendonly" || m.control != "track3" {
		t.Errorf("AAC-selected %s/%s/%s, want AAC/sendonly/track3",
			codec, m.direction, m.control)
	}
	if f, ok := m.format(97); !ok || f.clockRate != 16000 {
		t.Errorf("AAC clockRate = %+v, want 16000 for pt 97", f)
	}

	// Forcing AAC where no AAC track exists → error.
	g711Only := parseSDP([]byte(sampleSDP))
	if _, _, err := findBackchannelMedia(g711Only, "AAC"); err == nil {
		t.Error("findBackchannelMedia(AAC) should error when no AAC track exists")
	}
}

func TestChooseCodec(t *testing.T) {
	// thingino's real backchannel track: one m= line advertising four payload
	// types (AAC 97, Opus 102, PCMU 0, PCMA 8).
	thingino := sdpMedia{
		payloads: []int{97, 102, 0, 8},
		formats: map[int]sdpFormat{
			97:  {"MPEG4-GENERIC", 48000, ""},
			102: {"OPUS", 48000, ""},
			0:   {"PCMU", 8000, ""},
			8:   {"PCMA", 8000, ""},
		},
	}
	cases := []struct {
		name      string
		media     sdpMedia
		want      string
		wantCodec string
		wantPT    byte
	}{
		{"single PCMA", sdpMedia{payloads: []int{8}, formats: map[int]sdpFormat{8: {"PCMA", 8000, ""}}}, "", "PCMA", 8},
		{"single PCMU", sdpMedia{payloads: []int{0}, formats: map[int]sdpFormat{0: {"PCMU", 8000, ""}}}, "", "PCMU", 0},
		{"infer PCMU from static PT", sdpMedia{payloads: []int{0}}, "", "PCMU", 0},
		{"infer PCMA from static PT", sdpMedia{payloads: []int{8}}, "", "PCMA", 8},
		{"dynamic PT PCMA", sdpMedia{payloads: []int{18}, formats: map[int]sdpFormat{18: {"PCMA", 8000, ""}}}, "", "PCMA", 18},
		{"AAC dynamic PT", sdpMedia{payloads: []int{97}, formats: map[int]sdpFormat{97: {"MPEG4-GENERIC", 16000, ""}}}, "", "AAC", 97},
		{"thingino auto", thingino, "", "PCMA", 8},
		{"thingino forced PCMA", thingino, "PCMA", "PCMA", 8},
		{"thingino forced PCMU", thingino, "PCMU", "PCMU", 0},
		{"thingino forced AAC", thingino, "AAC", "AAC", 97},
		{"unsupported falls back", sdpMedia{payloads: []int{99}, formats: map[int]sdpFormat{99: {"OPUS", 48000, ""}}}, "", "PCMA", 99},
	}
	for _, c := range cases {
		codec, pt := chooseCodec(&c.media, c.want)
		if codec != c.wantCodec || pt != c.wantPT {
			t.Errorf("%s: chooseCodec(%+v, %q) = %s/%d, want %s/%d",
				c.name, c.media, c.want, codec, pt, c.wantCodec, c.wantPT)
		}
	}
}

// thinginoCh0SDP is the verbatim DESCRIBE body of a thingino prudynt camera
// (rtsp://thingino:thingino@192.168.1.91/ch0) after the ONVIF backchannel
// Require negotiation: track0 is one sendonly audio track carrying four
// payload types. Before the per-PT codec table this selected PCMA with the
// AAC payload type 97.
const thinginoCh0SDP = `v=0
o=- 2067521371 1 IN IP4 192.168.1.91
s=thingino prudynt (unknown)
t=0 0
b=AS:4000
a=control:*
m=video 0 RTP/AVP 96
a=control:track1
a=rtpmap:96 H264/90000
a=framerate:25
a=framesize:96 1920-1080
a=fmtp:96 packetization-mode=1;profile-level-id=4d0029
m=audio 0 RTP/AVP 97
a=control:track2
a=rtpmap:97 mpeg4-generic/48000/1
a=fmtp:97 streamtype=5;profile-level-id=15;mode=AAC-hbr;config=1188;sizelength=13;indexlength=3;indexdeltalength=3
m=audio 0 RTP/AVP 97 102 0 8
a=control:track0
a=sendonly
a=rtpmap:97 mpeg4-generic/48000
a=fmtp:97 streamtype=5;profile-level-id=15;mode=AAC-hbr;config=1188;sizelength=13;indexlength=3;indexdeltalength=3
a=rtpmap:102 OPUS/48000/2
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
`

func TestThinginoMultiCodecBackchannel(t *testing.T) {
	medias := parseSDP([]byte(thinginoCh0SDP))
	if len(medias) != 3 {
		t.Fatalf("parsed %d media, want 3", len(medias))
	}

	// Auto: the G.711-capable backchannel track, with PCMA on its own PT.
	m, codec, err := findBackchannelMedia(medias, "")
	if err != nil {
		t.Fatalf("auto-select: %v", err)
	}
	if m.control != "track0" || codec != "PCMA" {
		t.Errorf("auto-selected %s/%s, want track0/PCMA", m.control, codec)
	}
	got, pt := chooseCodec(m, codec)
	if got != "PCMA" || pt != 8 {
		t.Errorf("auto chooseCodec = %s/%d, want PCMA/8 (the AAC PT 97 must never leak)", got, pt)
	}

	for _, want := range []struct {
		codec string
		pt    byte
	}{
		{"PCMA", 8},
		{"PCMU", 0},
		{"AAC", 97},
	} {
		m, codec, err := findBackchannelMedia(medias, want.codec)
		if err != nil {
			t.Fatalf("force %s: %v", want.codec, err)
		}
		got, pt := chooseCodec(m, codec)
		if got != want.codec || pt != want.pt {
			t.Errorf("force %s chooseCodec = %s/%d, want %s/%d",
				want.codec, got, pt, want.codec, want.pt)
		}
	}
}

func TestProbeLabels(t *testing.T) {
	cases := []struct {
		name string
		sdp  string
		want []string
	}{
		{"single G.711 track", sampleSDP, []string{"g711"}},
		{"multi-codec thingino track", thinginoCh0SDP, []string{"aac", "g711"}},
		{"separate AAC and G.711 tracks", aacSDP, []string{"aac", "g711"}},
	}
	for _, c := range cases {
		got := probeLabels(parseSDP([]byte(c.sdp)))
		if len(got) != len(c.want) {
			t.Errorf("%s: probeLabels = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: probeLabels = %v, want %v", c.name, got, c.want)
				break
			}
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

// --- RTCP -----------------------------------------------------------------

func TestBuildRTCPReport(t *testing.T) {
	// Fixed inputs → fixed binary output (RFC 3550 §6.4.1 layout, 28 bytes:
	// header, sender info, empty report block list).
	p := buildRTCPReport(0x01020304, 0xE329732AC318DE00, 0xDEADBEEF, 42, 6720)
	if len(p) != 28 {
		t.Fatalf("SR length = %d, want 28", len(p))
	}
	if p[0] != 0x80 {
		t.Errorf("byte0 = %#x, want 0x80 (V=2, P=0, RC=0)", p[0])
	}
	if p[1] != 200 {
		t.Errorf("byte1 = %d, want 200 (PT=SR)", p[1])
	}
	if got := binary.BigEndian.Uint16(p[2:4]); got != 6 {
		t.Errorf("length = %d, want 6 (28 bytes in 32-bit words minus one)", got)
	}
	if got := binary.BigEndian.Uint32(p[4:8]); got != 0x01020304 {
		t.Errorf("ssrc = %#x, want 0x01020304", got)
	}
	if got := binary.BigEndian.Uint64(p[8:16]); got != 0xE329732AC318DE00 {
		t.Errorf("ntp = %#x, want 0xE329732AC318DE00", got)
	}
	if got := binary.BigEndian.Uint32(p[16:20]); got != 0xDEADBEEF {
		t.Errorf("rtp ts = %#x, want 0xDEADBEEF", got)
	}
	if got := binary.BigEndian.Uint32(p[20:24]); got != 42 {
		t.Errorf("packets = %d, want 42", got)
	}
	if got := binary.BigEndian.Uint32(p[24:28]); got != 6720 {
		t.Errorf("octets = %d, want 6720", got)
	}
}

func TestNTPNow(t *testing.T) {
	now := time.Now()
	got := ntpNow()
	secs := got >> 32
	if secs < uint64(now.Unix())+ntpEpochOffset || secs > uint64(now.Unix())+ntpEpochOffset+1 {
		t.Errorf("ntp seconds = %d, want ~%d", secs, uint64(now.Unix())+ntpEpochOffset)
	}
	if frac := got & 0xFFFFFFFF; frac >= 1<<32 {
		t.Errorf("ntp fraction = %d, want < 2^32", frac)
	}
}

// --- RTSP digest auth -------------------------------------------------------

func TestAuthHeaderDigestQop(t *testing.T) {
	// RFC 2617 §3.5 MD5 qop=auth vector.
	c := &rtspClient{
		username: "Mufasa",
		password: "Circle Of Life",
		realm:    "testrealm@host.com",
		nonce:    "dcd98b7102dd2f0e8b11d0f600bfb0c093",
		opaque:   "5ccc069c403ebaf9f0171e9517f40e41",
		qop:      "auth",
		nc:       1,
		cnonce:   "0a4f113b",
		useAuth:  true,
	}
	auth := c.authHeader("GET", "/dir/index.html")
	for _, want := range []string{
		`response="6629fae49393a05397450978507c4ef1"`,
		`qop=auth`, `nc=00000001`, `cnonce="0a4f113b"`,
		`opaque="5ccc069c403ebaf9f0171e9517f40e41"`,
		`username="Mufasa"`, `realm="testrealm@host.com"`,
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("auth header %q missing %s", auth, want)
		}
	}
	if c.nc != 2 {
		t.Errorf("nc after request = %d, want 2", c.nc)
	}
}

func TestAuthHeaderDigestLegacy(t *testing.T) {
	// RFC 2069 §2.1.2 MD5 vector (no qop).
	c := &rtspClient{
		username: "Mufasa",
		password: "Circle Of Life",
		realm:    "testrealm@host.com",
		nonce:    "dcd98b7102dd2f0e8b11d0f600bfb0c093",
		useAuth:  true,
	}
	auth := c.authHeader("GET", "/dir/index.html")
	if !strings.Contains(auth, `response="670fd8c2df070c60b045671b8b24ff02"`) {
		t.Errorf("auth header %q missing legacy response vector", auth)
	}
	if strings.Contains(auth, "qop=") {
		t.Errorf("legacy header %q must not carry qop", auth)
	}
}

func TestHandleAuthQop(t *testing.T) {
	c := &rtspClient{username: "u", password: "p"}

	// First challenge: qop=auth, a nonce. → retry, qop state armed.
	if c.handleAuth(401, map[string]string{
		"www-authenticate": `Digest realm="r", nonce="n1", qop="auth"`,
	}) {
		t.Error("first 401 should request a retry")
	}
	if c.qop != "auth" || c.nonce != "n1" || c.realm != "r" {
		t.Errorf("state = %+v, want qop=auth nonce=n1 realm=r", c)
	}
	if c.useAuth != true {
		t.Error("useAuth should be set by the challenge")
	}

	// Stale 401 with the same nonce: nc/cnonce must survive (nc keeps counting).
	h1 := c.authHeader("GET", "/x")
	if !strings.Contains(h1, "nc=00000001") {
		t.Errorf("first qop header should carry nc=00000001: %s", h1)
	}
	oldCnonce := c.cnonce
	if c.handleAuth(401, map[string]string{
		"www-authenticate": `Digest realm="r", nonce="n1", qop="auth", stale=true`,
	}) {
		t.Error("stale 401 should request a retry")
	}
	h2 := c.authHeader("GET", "/x")
	if !strings.Contains(h2, "nc=00000002") {
		t.Errorf("stale retry should carry nc=00000002: %s", h2)
	}
	if c.cnonce != oldCnonce {
		t.Error("stale retry must keep the client nonce")
	}

	// Fresh nonce: nc restarts at 1 and cnonce regenerates.
	if c.handleAuth(401, map[string]string{
		"www-authenticate": `Digest realm="r", nonce="n2", qop="auth"`,
	}) {
		t.Error("fresh nonce 401 should request a retry")
	}
	h3 := c.authHeader("GET", "/x")
	if !strings.Contains(h3, "nc=00000001") {
		t.Errorf("fresh nonce should restart nc at 00000001: %s", h3)
	}
	if c.cnonce == oldCnonce {
		t.Error("fresh nonce should regenerate the client nonce")
	}
}
