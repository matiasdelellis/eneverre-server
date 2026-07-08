package backchannel

import "encoding/binary"

// buildRTPPacket builds a bare-bones RTP header + payload (no CSRC, no extension).
func buildRTPPacket(pt byte, seq uint16, ts uint32, ssrc uint32, marker bool, payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))
	pkt[0] = 0x80
	pkt[1] = pt
	if marker {
		pkt[1] |= 0x80
	}
	binary.BigEndian.PutUint16(pkt[2:4], seq)
	binary.BigEndian.PutUint32(pkt[4:8], ts)
	binary.BigEndian.PutUint32(pkt[8:12], ssrc)
	copy(pkt[12:], payload)
	return pkt
}
