package backchannel

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// AACFrameSamples is the default number of PCM samples one AAC-LC access unit
// represents (the AAC-hbr frame length). It is the RTP timestamp increment per
// forwarded AU in the AAC passthrough path when the track's fmtp carries no
// AudioSpecificConfig-derived length.
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

// aacParams is the parsed RFC 3640 framing configuration of an MPEG4-GENERIC
// backchannel track.
type aacParams struct {
	sizeLen      int // AU-header size field bits (sizelength)
	indexLen     int // AU-header index field bits (indexlength)
	frameSamples int // PCM samples per AU (RTP timestamp increment)
}

// parseAACParams parses the a=fmtp line of an MPEG4-GENERIC track (RFC 3640
// §4.1). config= (the AudioSpecificConfig) is required — it pins the frame
// length; sizelength/indexlength default to the AAC-hbr de-facto 13/3; mode
// must be AAC-hbr or absent. Anything else fails closed with a clear error
// instead of emitting mis-framed RTP the camera cannot decode.
func parseAACParams(fmtp string) (aacParams, error) {
	params := map[string]string{}
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
	}

	if mode := params["mode"]; mode != "" && mode != "AAC-hbr" {
		return aacParams{}, fmt.Errorf("aac fmtp: unsupported mode %q (want AAC-hbr)", mode)
	}

	p := aacParams{sizeLen: 13, indexLen: 3, frameSamples: AACFrameSamples}
	var err error
	if v, ok := params["sizelength"]; ok {
		if p.sizeLen, err = strconv.Atoi(v); err != nil {
			return aacParams{}, fmt.Errorf("aac fmtp: bad sizelength %q", v)
		}
	}
	if v, ok := params["indexlength"]; ok {
		if p.indexLen, err = strconv.Atoi(v); err != nil {
			return aacParams{}, fmt.Errorf("aac fmtp: bad indexlength %q", v)
		}
	}
	// One AU-header must fit in the 16-bit AU-headers-length field.
	if p.sizeLen < 1 || p.indexLen < 0 || p.sizeLen+p.indexLen > 16 {
		return aacParams{}, fmt.Errorf("aac fmtp: invalid AU-header size sizelength=%d indexlength=%d", p.sizeLen, p.indexLen)
	}

	config, ok := params["config"]
	if !ok || config == "" {
		return aacParams{}, fmt.Errorf("aac fmtp: config= missing (cannot determine AAC frame length)")
	}
	if p.frameSamples, err = ascFrameSamples(config); err != nil {
		return aacParams{}, err
	}
	return p, nil
}

// ascFrameSamples parses the AudioSpecificConfig hex and returns the PCM
// samples per access unit: 1024 for AAC-LC, 2048 for SBR/Parametric Stereo,
// 512 for AAC-LD. Only the audioObjectType matters here — the frame length
// follows from it, not from the sampling frequency.
func ascFrameSamples(hexStr string) (int, error) {
	raw, err := hex.DecodeString(hexStr)
	if err != nil || len(raw) < 1 {
		return 0, fmt.Errorf("aac fmtp: bad config=%q", hexStr)
	}
	objectType := (raw[0] >> 3) & 0x1F
	switch objectType {
	case 23: // AAC-LD
		return 512, nil
	case 5, 29: // SBR / Parametric Stereo
		return 2048, nil
	default: // AAC-LC and friends
		return 1024, nil
	}
}

// aacRTPPayload wraps one raw AAC access unit in a single-AU RFC 3640 AAC-hbr
// payload: a 2-byte AU-headers-length (in bits) followed by one AU-header
// (sizeLen-bit size + indexLen-bit index=0), then the AU. The framing lengths
// come from the track's a=fmtp (de-facto 13/3).
func aacRTPPayload(au []byte, sizeLen, indexLen int) []byte {
	headerBits := sizeLen + indexLen
	headerBytes := (headerBits + 7) / 8
	payload := make([]byte, 2+headerBytes+len(au))
	binary.BigEndian.PutUint16(payload[0:2], uint16(headerBits))
	hdr := uint64(len(au)) << indexLen
	for i := 0; i < headerBytes; i++ {
		payload[2+headerBytes-1-i] = byte(hdr >> (8 * i))
	}
	copy(payload[2+headerBytes:], au)
	return payload
}
