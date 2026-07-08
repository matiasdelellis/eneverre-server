package backchannel

import "encoding/binary"

// AACFrameSamples is the number of PCM samples one AAC-LC access unit
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

// aacRTPPayload wraps one raw AAC access unit in a single-AU RFC 3640 AAC-hbr
// payload: a 2-byte AU-headers-length (16 bits) followed by one 2-byte AU-header
// (13-bit size + 3-bit index=0), then the AU. Matches sizelength=13;indexlength=3.
func aacRTPPayload(au []byte) []byte {
	payload := make([]byte, 4+len(au))
	payload[1] = 16 // AU-headers-length, in bits
	binary.BigEndian.PutUint16(payload[2:4], uint16(len(au))<<3)
	copy(payload[4:], au)
	return payload
}
