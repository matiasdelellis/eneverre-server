package backchannel

import "github.com/bluenviron/gortsplib/v5/pkg/format"

// AACFrameSamples is the default number of PCM samples one AAC-LC access unit
// represents (the AAC-hbr frame length). It is the RTP timestamp increment per
// forwarded AU in the AAC passthrough path.
const AACFrameSamples = 1024

const adtsHeaderSize = 7

// isADTS reports whether b starts with an ADTS syncword. Android's MediaCodec
// emits raw access units, but some clients wrap each frame in ADTS; RTP
// MPEG4-GENERIC carries raw AUs, so an ADTS header must be stripped first.
func isADTS(b []byte) bool {
	return len(b) >= adtsHeaderSize && b[0] == 0xFF && b[1]&0xF6 == 0xF0
}

// adtsHeaderLen returns the ADTS header length: 9 bytes when a CRC is present
// (protection-absent bit = 0), 7 otherwise.
func adtsHeaderLen(b []byte) int {
	if b[1]&0x01 == 0 {
		return 9
	}
	return adtsHeaderSize
}

// aacFrameSamples returns the PCM samples per access unit of an MPEG4-GENERIC
// track: 1024 for AAC-LC, 2048 for SBR/Parametric Stereo, 512 for AAC-LD. Only
// the audio object type matters — the frame length follows from it, not from
// the sampling frequency. The RFC 3640 AU-header framing itself (sizelength,
// indexlength) comes straight off the format and is applied by gortsplib's
// rtpmpeg4audio encoder.
func aacFrameSamples(f *format.MPEG4Audio) uint32 {
	if f.Config == nil {
		return AACFrameSamples
	}
	switch int(f.Config.Type) {
	case 23: // AAC-LD
		return 512
	case 5, 29: // SBR / Parametric Stereo
		return 2048
	}
	return AACFrameSamples
}
