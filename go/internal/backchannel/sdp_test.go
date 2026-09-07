package backchannel

import (
	"strings"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	psdp "github.com/bluenviron/gortsplib/v5/pkg/sdp"
)

// Real SDP from a thingino camera (prudynt): the backchannel is track0 and it
// carries four payload types on one m= line.
const thinginoSDP = `v=0
o=- 624968532 1 IN IP4 192.168.1.91
s=thingino prudynt (unknown)
t=0 0
b=AS:4000
a=control:*
m=video 0 RTP/AVP 96
a=control:track1
a=rtpmap:96 H264/90000
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

// Real SDP from a TP-Link camera: no direction attribute anywhere, so nothing
// is advertised as send-capable and only the last-resort fallback can pick.
const tplinkSDP = `v=0
o=- 14665860 31787219 1 IN IP4 192.168.1.92
s=Session streamed by "TP-Link RTSP Server"
t=0 0
m=video 0 RTP/AVP 96
c=IN IP4 0.0.0.0
b=AS:4096
a=control:track1
a=rtpmap:96 H264/90000
m=audio 0 RTP/AVP 8
a=rtpmap:8 PCMA/8000
a=control:track2
`

func parseSession(t *testing.T, raw string) *description.Session {
	t.Helper()
	var ssd psdp.SessionDescription
	if err := ssd.Unmarshal([]byte(raw)); err != nil {
		t.Fatalf("parse raw SDP: %v", err)
	}
	var desc description.Session
	if err := desc.Unmarshal(&ssd); err != nil {
		t.Fatalf("gortsplib parse: %v", err)
	}
	return &desc
}

func TestSelectBackchannelThingino(t *testing.T) {
	for _, tc := range []struct {
		force   string
		codec   string
		wantPT  uint8
		control string
	}{
		{"", codecPCMA, 8, "track0"},
		{"PCMA", codecPCMA, 8, "track0"},
		{"PCMU", codecPCMU, 0, "track0"},
		{"AAC", codecAAC, 97, "track0"},
		{"OPUS", codecOPUS, 102, "track0"},
	} {
		desc := parseSession(t, thinginoSDP)
		pick, err := selectBackchannel(desc, thinginoSDP, tc.force)
		if err != nil {
			t.Fatalf("force=%q: %v", tc.force, err)
		}
		if pick.codec != tc.codec {
			t.Errorf("force=%q: codec = %q, want %q", tc.force, pick.codec, tc.codec)
		}
		if got := pick.forma.PayloadType(); got != tc.wantPT {
			t.Errorf("force=%q: payload type = %d, want %d", tc.force, got, tc.wantPT)
		}
		if pick.media.Control != tc.control {
			t.Errorf("force=%q: control = %q, want %q", tc.force, pick.media.Control, tc.control)
		}
	}
}

// The listen-only audio track comes first in thingino's SDP; picking it would
// send audio into the camera's microphone stream.
func TestSelectBackchannelSkipsListenTrack(t *testing.T) {
	desc := parseSession(t, thinginoSDP)
	pick, err := selectBackchannel(desc, thinginoSDP, "AAC")
	if err != nil {
		t.Fatal(err)
	}
	if pick.media == desc.Medias[1] {
		t.Fatal("picked the recvonly track2, want the sendonly track0")
	}
}

func TestSelectBackchannelForceCodecAbsent(t *testing.T) {
	desc := parseSession(t, tplinkSDP)
	if _, err := selectBackchannel(desc, tplinkSDP, "OPUS"); err == nil {
		t.Fatal("want an error when no send-capable track offers the forced codec")
	}
}

// A camera that advertises no send-capable track has no back channel, even
// when it does carry audio. The hand-rolled parser used to talk to that track
// anyway; the TP-Link that motivated the fallback accepts the RTP and plays
// nothing, so the session is refused instead of looking like it works.
func TestSelectBackchannelRejectsUndeclared(t *testing.T) {
	desc := parseSession(t, tplinkSDP)
	if _, err := selectBackchannel(desc, tplinkSDP, ""); err == nil {
		t.Fatal("want an error when no track is advertised as send-capable")
	}
}

// gortsplib only reads a=sendonly, so a sendrecv track has to be recognized
// from the raw SDP directions.
func TestSelectBackchannelSendrecv(t *testing.T) {
	const raw = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 0 RTP/AVP 0
a=control:listen
a=recvonly
a=rtpmap:0 PCMU/8000
m=audio 0 RTP/AVP 8
a=control:talk
a=sendrecv
a=rtpmap:8 PCMA/8000
`
	desc := parseSession(t, raw)
	if desc.Medias[1].IsBackChannel {
		t.Fatal("precondition: gortsplib should not mark a sendrecv track as a back channel")
	}
	pick, err := selectBackchannel(desc, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if pick.media.Control != "talk" {
		t.Fatalf("picked control=%q, want talk", pick.media.Control)
	}
}

// When every media in an SDP is a back channel, gortsplib unmarks them all
// ("some cameras mark medias as back channels even though they are not").
// The raw directions have to rescue that case.
func TestSelectBackchannelAllTracksSendonly(t *testing.T) {
	const raw = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 0 RTP/AVP 8
a=control:talk
a=sendonly
a=rtpmap:8 PCMA/8000
`
	desc := parseSession(t, raw)
	if desc.Medias[0].IsBackChannel {
		t.Fatal("precondition: gortsplib unmarks back channels when there is nothing else")
	}
	pick, err := selectBackchannel(desc, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if pick.codec != codecPCMA {
		t.Fatalf("codec = %q, want %q", pick.codec, codecPCMA)
	}
}

func TestSelectBackchannelNoAudio(t *testing.T) {
	const raw = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=-
t=0 0
m=video 0 RTP/AVP 96
a=control:track1
a=rtpmap:96 H264/90000
`
	desc := parseSession(t, raw)
	if _, err := selectBackchannel(desc, raw, ""); err == nil {
		t.Fatal("want an error when the SDP has no audio at all")
	}
}

// prepareMedia is what makes gortsplib treat our pick as a back channel and
// guarantees WritePacketRTP can resolve the payload type.
func TestPrepareMediaPinsFormatAndMarks(t *testing.T) {
	desc := parseSession(t, thinginoSDP)
	pick, err := selectBackchannel(desc, thinginoSDP, "OPUS")
	if err != nil {
		t.Fatal(err)
	}
	if len(pick.media.Formats) != 4 {
		t.Fatalf("precondition: track0 should carry 4 formats, got %d", len(pick.media.Formats))
	}
	pick.prepareMedia()
	if !pick.media.IsBackChannel {
		t.Error("IsBackChannel = false, want true")
	}
	if len(pick.media.Formats) != 1 || pick.media.Formats[0] != pick.forma {
		t.Errorf("Formats = %v, want just the chosen one", pick.media.Formats)
	}
}

// A dynamic payload type with no a=rtpmap parses into format.Generic, which has
// no encoder; the historical behavior is to assume PCMA on it.
func TestAssumeG711(t *testing.T) {
	for _, tc := range []struct {
		pt        uint8
		wantCodec string
		wantMULaw bool
	}{
		{0, codecPCMU, true},
		{8, codecPCMA, false},
		{98, codecPCMA, false},
	} {
		m := &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{&format.Generic{PayloadTyp: tc.pt}},
		}
		f, codec := assumeG711(m)
		if codec != tc.wantCodec {
			t.Errorf("pt %d: codec = %q, want %q", tc.pt, codec, tc.wantCodec)
		}
		g, ok := f.(*format.G711)
		if !ok {
			t.Fatalf("pt %d: got %T, want *format.G711", tc.pt, f)
		}
		if g.PayloadType() != tc.pt {
			t.Errorf("pt %d: synthesized payload type = %d", tc.pt, g.PayloadType())
		}
		if g.MULaw != tc.wantMULaw {
			t.Errorf("pt %d: MULaw = %v, want %v", tc.pt, g.MULaw, tc.wantMULaw)
		}
	}
}

func TestProbeLabelsThingino(t *testing.T) {
	desc := parseSession(t, thinginoSDP)
	got := probeLabels(desc, thinginoSDP)
	want := []string{"aac", "opus", "g711"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// The TP-Link advertises no send-capable track, so it reports no talk codecs
// even though it has an audio track.
func TestProbeLabelsNoBackchannel(t *testing.T) {
	desc := parseSession(t, tplinkSDP)
	if got := probeLabels(desc, tplinkSDP); len(got) != 0 {
		t.Errorf("labels = %v, want none", got)
	}
}

func TestSDPDirections(t *testing.T) {
	got := sdpDirections(thinginoSDP)
	want := []string{"", "", "sendonly"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directions = %v, want %v", got, want)
	}
	if got := sdpDirections("no media sections here"); len(got) != 0 {
		t.Errorf("directions = %v, want none", got)
	}
}

func TestCanonicalCodec(t *testing.T) {
	for in, want := range map[string]string{
		"pcma": codecPCMA, "PCMA": codecPCMA, " alaw ": codecPCMA,
		"pcmu": codecPCMU, "mulaw": codecPCMU,
		"aac": codecAAC, "mpeg4-generic": codecAAC,
		"opus": codecOPUS,
		"":     "", "vorbis": "",
	} {
		if got := canonicalCodec(in); got != want {
			t.Errorf("canonicalCodec(%q) = %q, want %q", in, got, want)
		}
	}
}

// A track the camera does advertise as send-capable, but whose payload type
// carries no a=rtpmap, parses into format.Generic — which has no encoder.
// That one keeps the assume-G.711 tolerance: the camera said it accepts audio.
func TestSelectBackchannelDeclaredWithoutRtpmap(t *testing.T) {
	const raw = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 0 RTP/AVP 0
a=control:listen
a=recvonly
a=rtpmap:0 PCMU/8000
m=audio 0 RTP/AVP 8
a=control:talk
a=sendonly
`
	desc := parseSession(t, raw)
	pick, err := selectBackchannel(desc, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if pick.media.Control != "talk" {
		t.Fatalf("control = %q, want talk", pick.media.Control)
	}
	if pick.codec != codecPCMA || pick.forma.PayloadType() != 8 {
		t.Errorf("pick = %s/pt %d, want PCMA/pt 8", pick.codec, pick.forma.PayloadType())
	}
	if _, ok := pick.forma.(*format.G711); !ok {
		t.Errorf("forma = %T, want *format.G711 (Generic has no encoder)", pick.forma)
	}
}
