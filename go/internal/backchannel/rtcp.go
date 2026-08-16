package backchannel

import (
	"encoding/binary"
	"time"
)

// ntpEpochOffset is the seconds between the Unix and NTP epochs (1900-01-01).
const ntpEpochOffset = 2208988800

// buildRTCPReport builds one RTCP Sender Report packet (RFC 3550 §6.4.1):
// V=2, P=0, RC=0, PT=200, the sender SSRC, an NTP timestamp, the RTP
// timestamp corresponding to that wall-clock instant, the sender's packet and
// octet counters, and no report blocks — the backchannel is send-only, we
// receive nothing on this session.
func buildRTCPReport(ssrc uint32, ntp uint64, rtpTS, packets, octets uint32) []byte {
	p := make([]byte, 28)
	p[0] = 0x80                           // V=2, no padding, report count 0
	p[1] = 200                            // PT=SR
	binary.BigEndian.PutUint16(p[2:4], 6) // length in 32-bit words minus one
	binary.BigEndian.PutUint32(p[4:8], ssrc)
	binary.BigEndian.PutUint64(p[8:16], ntp)
	binary.BigEndian.PutUint32(p[16:20], rtpTS)
	binary.BigEndian.PutUint32(p[20:24], packets)
	binary.BigEndian.PutUint32(p[24:28], octets)
	return p
}

// ntpNow returns the current wall clock as a 64-bit RFC 3550 NTP timestamp:
// seconds since 1900 in the high 32 bits, the fractional second (232 units)
// in the low 32.
func ntpNow() uint64 {
	now := time.Now()
	secs := uint64(now.Unix()) + ntpEpochOffset
	frac := uint64(now.Nanosecond()) * (1 << 32) / 1e9
	return secs<<32 | frac
}
