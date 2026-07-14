package liverelay

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// TestPromoteH264PacketizationMode verifies that mode-0 H264 (which gortsplib's
// server refuses to serve) is rewritten to mode 1 in place, that the Media
// pointers are preserved (ServerStream routes writes by pointer identity), and
// that other formats / modes are left untouched.
func TestPromoteH264PacketizationMode(t *testing.T) {
	mode0 := &format.H264{PayloadTyp: 96, PacketizationMode: 0}
	mode1 := &format.H264{PayloadTyp: 97, PacketizationMode: 1}
	audio := &format.G711{PayloadTyp: 0, MULaw: true, SampleRate: 8000, ChannelCount: 1}

	videoMedia := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{mode0}}
	otherVideo := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{mode1}}
	audioMedia := &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{audio}}
	desc := &description.Session{Medias: []*description.Media{videoMedia, otherVideo, audioMedia}}

	promoteH264PacketizationMode(desc)

	if mode0.PacketizationMode != 1 {
		t.Fatalf("mode-0 H264 not promoted: got PacketizationMode=%d, want 1", mode0.PacketizationMode)
	}
	if mode1.PacketizationMode != 1 {
		t.Fatalf("mode-1 H264 changed unexpectedly: got PacketizationMode=%d, want 1", mode1.PacketizationMode)
	}
	// Media pointers must be preserved so WritePacketRTP routing keeps working.
	if desc.Medias[0] != videoMedia || desc.Medias[1] != otherVideo || desc.Medias[2] != audioMedia {
		t.Fatal("Media pointers were replaced; ServerStream write routing would break")
	}
	if videoMedia.Formats[0] != mode0 {
		t.Fatal("H264 format pointer was replaced instead of mutated in place")
	}
}
