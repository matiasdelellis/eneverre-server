package backchannel

import (
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
