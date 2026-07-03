// Package backchannel sends real-time audio to an RTSP camera's ONVIF Profile T
// two-way-audio backchannel. It is a library port of the standalone web2rtsp
// proof of concept: the single-session globals, HTTP server, WebSocket handling
// and flag parsing are gone — a Session is opened per camera by the caller.
//
// The pipeline is: caller feeds native-rate mono S16LE PCM via FeedPCM →
// anti-alias low-pass + linear resample to 8 kHz → G.711 (A-law/µ-law) encode →
// 160-sample RTP frames every 20 ms → RTSP interleaved ($-framing, channel 0)
// over the same TCP connection as the RTSP control messages.
//
// Everything is hand-implemented with the standard library (no external RTSP or
// G.711 dependency). Trace logging goes through slog at debug level, so run with
// ENEVERRE_LOG_LEVEL=debug to see the RTSP exchange.
package backchannel

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TargetRate is the G.711 sample rate; FrameSamples is one 20 ms RTP frame.
const (
	TargetRate   = 8000
	FrameSamples = 160
)

// AACFrameSamples is the number of PCM samples one AAC-LC access unit
// represents (the AAC-hbr frame length). It is the RTP timestamp increment per
// forwarded AU in the AAC passthrough path.
const AACFrameSamples = 1024

// ----- G.711 encoding (CCITT reference implementations) -----

var segAEnd = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

func alawSegment(val int) int {
	for i, end := range segAEnd {
		if val <= end {
			return i
		}
	}
	return len(segAEnd)
}

func linearToALaw(pcm int16) byte {
	pcmVal := int(pcm) >> 3
	var mask int
	if pcmVal >= 0 {
		mask = 0xD5
	} else {
		mask = 0x55
		pcmVal = -pcmVal - 1
	}
	seg := alawSegment(pcmVal)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	aval := seg << 4
	if seg < 2 {
		aval |= (pcmVal >> 1) & 0x0F
	} else {
		aval |= (pcmVal >> seg) & 0x0F
	}
	return byte(aval ^ mask)
}

var ulawExpLut = [256]byte{
	0, 0, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}

func linearToULaw(pcm int16) byte {
	const bias = 0x84
	const clip = 32635
	pcmVal := int(pcm)
	sign := 0
	if pcmVal < 0 {
		pcmVal = -pcmVal
		sign = 0x80
	}
	if pcmVal > clip {
		pcmVal = clip
	}
	pcmVal += bias
	exponent := int(ulawExpLut[(pcmVal>>7)&0xFF])
	mantissa := (pcmVal >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | mantissa))
}

func encodeALaw(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = linearToALaw(s)
	}
	return out
}

func encodeULaw(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = linearToULaw(s)
	}
	return out
}

// ----- RTP packet construction -----

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

// ----- AAC (RFC 3640 MPEG4-GENERIC / AAC-hbr) framing -----

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

// ----- RTSP client -----

type rtspClient struct {
	conn      net.Conn
	baseURL   *url.URL
	cseq      int
	sessionID string

	username string
	password string
	realm    string
	nonce    string
	opaque   string
	useAuth  bool

	readerStop chan struct{}
	readerDone chan struct{}
}

func dialRTSP(rawURL string) (*rtspClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":554"
	}

	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	c := &rtspClient{
		conn:    conn,
		baseURL: u,
	}

	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
	}

	return c, nil
}

func (c *rtspClient) request(method, uri string, extraHeaders map[string]string, body []byte) (int, map[string]string, []byte, error) {
	c.cseq++

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&sb, "CSeq: %d\r\n", c.cseq)
	if body != nil {
		fmt.Fprintf(&sb, "Content-Length: %d\r\n", len(body))
		sb.WriteString("Content-Type: application/sdp\r\n")
	}
	if c.sessionID != "" {
		fmt.Fprintf(&sb, "Session: %s\r\n", c.sessionID)
	}
	for k, v := range extraHeaders {
		fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
	}
	sb.WriteString("\r\n")
	if body != nil {
		sb.Write(body)
	}

	slog.Debug("rtsp request", "method", method, "uri", uri)

	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, nil, fmt.Errorf("write: %w", err)
	}

	return c.readResponse()
}

func (c *rtspClient) writeRequest(method, uri string) error {
	c.cseq++
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&sb, "CSeq: %d\r\n", c.cseq)
	if c.sessionID != "" {
		fmt.Fprintf(&sb, "Session: %s\r\n", c.sessionID)
	}
	if auth := c.authHeader(method, uri); auth != "" {
		fmt.Fprintf(&sb, "Authorization: %s\r\n", auth)
	}
	sb.WriteString("\r\n")

	slog.Debug("rtsp request (no-wait)", "method", method, "uri", uri)
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write([]byte(sb.String()))
	return err
}

func (c *rtspClient) readResponse() (int, map[string]string, []byte, error) {
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	var headerBuf strings.Builder
	buf := make([]byte, 1)
	for {
		if _, err := c.conn.Read(buf); err != nil {
			return 0, nil, nil, fmt.Errorf("read: %w", err)
		}
		headerBuf.Write(buf)
		if strings.HasSuffix(headerBuf.String(), "\r\n\r\n") {
			break
		}
		if headerBuf.Len() > 16384 {
			return 0, nil, nil, fmt.Errorf("response too large")
		}
	}

	raw := headerBuf.String()
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	headerLines := strings.Split(parts[0], "\r\n")

	if len(headerLines) < 1 {
		return 0, nil, nil, fmt.Errorf("empty response")
	}
	statusParts := strings.SplitN(headerLines[0], " ", 3)
	if len(statusParts) < 2 {
		return 0, nil, nil, fmt.Errorf("bad status: %s", headerLines[0])
	}
	statusCode, _ := strconv.Atoi(statusParts[1])
	reason := ""
	if len(statusParts) >= 3 {
		reason = statusParts[2]
	}

	headers := make(map[string]string)
	for _, line := range headerLines[1:] {
		if idx := strings.Index(line, ": "); idx > 0 {
			headers[strings.ToLower(line[:idx])] = line[idx+2:]
		}
	}

	var body []byte
	if contentLenStr := headers["content-length"]; contentLenStr != "" {
		contentLen, _ := strconv.Atoi(contentLenStr)
		if contentLen > 0 {
			if len(parts) > 1 {
				body = []byte(parts[1])
			}
			for len(body) < contentLen {
				chunk := make([]byte, contentLen-len(body))
				n, err := c.conn.Read(chunk)
				if err != nil {
					return 0, nil, nil, fmt.Errorf("read body: %w", err)
				}
				body = append(body, chunk[:n]...)
			}
		}
	} else if len(parts) > 1 {
		body = []byte(parts[1])
	}

	slog.Debug("rtsp response", "status", statusCode, "reason", reason)

	return statusCode, headers, body, nil
}

func (c *rtspClient) authHeader(method, uri string) string {
	if !c.useAuth || c.username == "" {
		return ""
	}

	if c.realm != "" && c.nonce != "" {
		h1 := md5.Sum([]byte(c.username + ":" + c.realm + ":" + c.password))
		h2 := md5.Sum([]byte(method + ":" + uri))
		response := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%x:%s:%x", h1, c.nonce, h2))))
		auth := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
			c.username, c.realm, c.nonce, uri, response)
		if c.opaque != "" {
			auth += fmt.Sprintf(`, opaque="%s"`, c.opaque)
		}
		return auth
	}

	cred := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	return "Basic " + cred
}

func (c *rtspClient) handleAuth(statusCode int, headers map[string]string) bool {
	if statusCode != 401 || c.username == "" {
		return true
	}

	wwwAuth := headers["www-authenticate"]
	if wwwAuth == "" {
		return true
	}

	c.useAuth = true

	if strings.HasPrefix(wwwAuth, "Digest") {
		for _, part := range strings.Split(wwwAuth[len("Digest"):], ",") {
			part = strings.TrimSpace(part)
			switch {
			case strings.HasPrefix(part, "realm="):
				c.realm = strings.Trim(part[6:], "\"")
			case strings.HasPrefix(part, "nonce="):
				c.nonce = strings.Trim(part[6:], "\"")
			case strings.HasPrefix(part, "opaque="):
				c.opaque = strings.Trim(part[7:], "\"")
			}
		}
		return false
	}

	if strings.HasPrefix(wwwAuth, "Basic") {
		return false
	}

	return true
}

func (c *rtspClient) options(uri string) error {
	code, _, _, err := c.request("OPTIONS", uri, nil, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("OPTIONS returned %d", code)
	}
	return nil
}

func (c *rtspClient) describe(uri string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		headers := map[string]string{
			"Accept":  "application/sdp",
			"Require": "www.onvif.org/ver20/backchannel",
		}
		if auth := c.authHeader("DESCRIBE", uri); auth != "" {
			headers["Authorization"] = auth
		}

		code, respHeaders, body, err := c.request("DESCRIBE", uri, headers, nil)
		if err != nil {
			return nil, err
		}

		c.captureSession(respHeaders, false)

		if c.handleAuth(code, respHeaders) {
			if code != 200 {
				return nil, fmt.Errorf("DESCRIBE returned %d", code)
			}
			return body, nil
		}
	}

	return nil, fmt.Errorf("DESCRIBE failed after auth retry")
}

func (c *rtspClient) setup(uri, transport string) error {
	for attempt := 0; attempt < 3; attempt++ {
		headers := map[string]string{"Transport": transport}
		if auth := c.authHeader("SETUP", uri); auth != "" {
			headers["Authorization"] = auth
		}

		code, respHeaders, _, err := c.request("SETUP", uri, headers, nil)
		if err != nil {
			if attempt < 2 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return err
		}

		c.captureSession(respHeaders, true)

		if c.handleAuth(code, respHeaders) {
			if code != 200 {
				if attempt < 2 {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				return fmt.Errorf("SETUP returned %d", code)
			}
			return nil
		}
	}
	return fmt.Errorf("SETUP failed after retries")
}

func (c *rtspClient) play(uri string) error {
	for attempt := 0; attempt < 3; attempt++ {
		headers := map[string]string{}
		if auth := c.authHeader("PLAY", uri); auth != "" {
			headers["Authorization"] = auth
		}

		code, respHeaders, _, err := c.request("PLAY", uri, headers, nil)
		if err != nil {
			if attempt < 2 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return err
		}
		if c.handleAuth(code, respHeaders) {
			if code != 200 {
				if attempt < 2 {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				return fmt.Errorf("PLAY returned %d", code)
			}
			return nil
		}
	}
	return fmt.Errorf("PLAY failed after retries")
}

func (c *rtspClient) captureSession(headers map[string]string, onlyIfEmpty bool) {
	s := headers["session"]
	if s == "" || (onlyIfEmpty && c.sessionID != "") {
		return
	}
	if idx := strings.IndexByte(s, ';'); idx > 0 {
		s = s[:idx]
	}
	c.sessionID = strings.TrimSpace(s)
}

func (c *rtspClient) writeInterleaved(channel byte, data []byte) error {
	chunk := make([]byte, 4+len(data))
	chunk[0] = '$'
	chunk[1] = channel
	binary.BigEndian.PutUint16(chunk[2:4], uint16(len(data)))
	copy(chunk[4:], data)

	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(chunk)
	return err
}

func (c *rtspClient) startReader() {
	c.readerStop = make(chan struct{})
	c.readerDone = make(chan struct{})
	go func() {
		defer close(c.readerDone)
		buf := make([]byte, 65536)
		for {
			select {
			case <-c.readerStop:
				return
			default:
			}
			c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			_, err := c.conn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}
		}
	}()
}

func (c *rtspClient) stopReader() {
	if c.readerStop == nil {
		return
	}
	close(c.readerStop)
	select {
	case <-c.readerDone:
	case <-time.After(2 * time.Second):
	}
}

// ----- SDP parsing -----

type sdpMedia struct {
	mediaType string
	port      int
	proto     string
	payloads  []int
	control   string
	direction string
	codecName string
	clockRate int
}

func parseSDP(raw []byte) []sdpMedia {
	var medias []sdpMedia
	var current *sdpMedia

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "m=") {
			if current != nil {
				medias = append(medias, *current)
			}
			current = &sdpMedia{}
			parts := strings.Split(line[2:], " ")
			if len(parts) >= 4 {
				current.mediaType = parts[0]
				current.port, _ = strconv.Atoi(parts[1])
				current.proto = parts[2]
				for _, p := range parts[3:] {
					if pt, err := strconv.Atoi(p); err == nil {
						current.payloads = append(current.payloads, pt)
					}
				}
			}
		} else if current != nil {
			switch {
			case strings.HasPrefix(line, "a=control:"):
				current.control = line[10:]
			case strings.HasPrefix(line, "a=sendonly"):
				current.direction = "sendonly"
			case strings.HasPrefix(line, "a=recvonly"):
				current.direction = "recvonly"
			case strings.HasPrefix(line, "a=sendrecv"):
				current.direction = "sendrecv"
			case strings.HasPrefix(line, "a=rtpmap:"):
				fields := strings.SplitN(line[9:], " ", 2)
				if len(fields) == 2 {
					codecParts := strings.Split(fields[1], "/")
					current.codecName = strings.ToUpper(strings.TrimSpace(codecParts[0]))
					if len(codecParts) > 1 {
						current.clockRate, _ = strconv.Atoi(strings.TrimSpace(codecParts[1]))
					}
				}
			}
		}
	}
	if current != nil {
		medias = append(medias, *current)
	}
	return medias
}

func findBackchannelMedia(medias []sdpMedia, forceCodec string) (*sdpMedia, error) {
	sendable := func(m sdpMedia) bool {
		return m.direction == "sendonly" || m.direction == "sendrecv"
	}
	supported := func(m sdpMedia) bool {
		return m.codecName == "PCMA" || m.codecName == "PCMU"
	}

	if forceCodec == "AAC" {
		for _, m := range medias {
			if m.mediaType == "audio" && sendable(m) &&
				(m.codecName == "MPEG4-GENERIC" || m.codecName == "AAC") {
				return &m, nil
			}
		}
		return nil, fmt.Errorf("no send-capable AAC audio track in SDP")
	}

	if forceCodec != "" {
		for _, m := range medias {
			if m.mediaType == "audio" && sendable(m) && strings.EqualFold(m.codecName, forceCodec) {
				return &m, nil
			}
		}
		return nil, fmt.Errorf("no send-capable %s audio track in SDP", forceCodec)
	}

	for _, m := range medias {
		if m.mediaType == "audio" && sendable(m) && supported(m) {
			return &m, nil
		}
	}
	for _, m := range medias {
		if m.mediaType == "audio" && sendable(m) {
			return &m, nil
		}
	}
	for _, m := range medias {
		if m.mediaType == "audio" {
			return &m, nil
		}
	}
	return nil, fmt.Errorf("no backchannel audio track found in SDP")
}

func chooseCodec(m *sdpMedia) (codec string, pt byte) {
	pt = 8
	if len(m.payloads) > 0 {
		pt = byte(m.payloads[0])
	}
	switch m.codecName {
	case "PCMA":
		return "PCMA", pt
	case "PCMU":
		return "PCMU", pt
	case "MPEG4-GENERIC", "AAC":
		return "AAC", pt
	}
	if len(m.payloads) > 0 {
		switch m.payloads[0] {
		case 0:
			return "PCMU", 0
		case 8:
			return "PCMA", 8
		}
	}
	return "PCMA", pt
}

func keepalive(c *rtspClient, uri string, done chan struct{}) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			slog.Debug("rtsp keepalive OPTIONS")
			_ = c.writeRequest("OPTIONS", uri)
		case <-done:
			return
		}
	}
}

func resolveControlURL(base *url.URL, control string) string {
	if control == "" || control == "*" {
		return base.String()
	}
	if strings.Contains(control, "://") {
		return control
	}
	b := base.String()
	if strings.HasPrefix(control, "/") {
		if u, err := url.Parse(b); err == nil {
			u.Path = control
			u.RawQuery = ""
			return u.String()
		}
	}
	if !strings.HasSuffix(b, "/") {
		b += "/"
	}
	return b + control
}

// ----- Resampling -----

func lowPassForDecimation(samples []int16, fromRate, toRate int) []int16 {
	if toRate >= fromRate || len(samples) == 0 {
		return samples
	}
	win := fromRate / toRate
	if win < 2 {
		return samples
	}
	out := make([]int16, len(samples))
	var acc int32
	for i := range samples {
		acc += int32(samples[i])
		if i >= win {
			acc -= int32(samples[i-win])
		}
		n := int32(win)
		if i+1 < win {
			n = int32(i + 1)
		}
		out[i] = int16(acc / n)
	}
	return out
}

func resampleLinear(samples []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate || len(samples) == 0 {
		return samples
	}
	ratio := float64(toRate) / float64(fromRate)
	outLen := int(float64(len(samples)) * ratio)
	if outLen < 1 {
		outLen = 1
	}
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		fpos := float64(i) / ratio
		idx := int(fpos)
		frac := fpos - float64(idx)
		if idx >= len(samples)-1 {
			out[i] = samples[len(samples)-1]
		} else {
			out[i] = int16(float64(samples[idx])*(1-frac) + float64(samples[idx+1])*frac)
		}
	}
	return out
}

// ----- Session -----

// Session is a live audio backchannel to one camera. Open one with Dial, push
// audio with FeedPCM, and release it with Close. It is safe to call FeedPCM and
// Close from different goroutines than the one that opened it.
type Session struct {
	*rtspClient
	codec         string
	pt            byte
	clockRate     int
	uri           string
	audioIn       chan []int16 // G.711 path: native-rate PCM to resample + encode
	auIn          chan []byte  // AAC path: raw access units to forward
	stop          chan struct{}
	done          chan struct{}
	keepaliveDone chan struct{}
}

// ProbeCodecs opens a short-lived RTSP session (OPTIONS + DESCRIBE with the
// ONVIF backchannel Require header) and returns the client-facing talk codec
// labels for every send-capable audio track the camera advertises: "aac" for an
// MPEG4-GENERIC track, "g711" for PCMA/PCMU. Labels are deduplicated and ordered
// as they appear in the SDP. No RTP is set up; the connection is closed before
// returning. Used at startup to populate camera capabilities so clients need not
// guess which codecs a camera accepts.
func ProbeCodecs(rawURL string) ([]string, error) {
	c, err := dialRTSP(rawURL)
	if err != nil {
		return nil, err
	}
	defer c.conn.Close()

	uri := c.baseURL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	if err := c.options(uri); err != nil {
		return nil, fmt.Errorf("OPTIONS: %w", err)
	}
	sdpRaw, err := c.describe(uri)
	if err != nil {
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}

	var codecs []string
	seen := map[string]bool{}
	for _, m := range parseSDP(sdpRaw) {
		if m.mediaType != "audio" || (m.direction != "sendonly" && m.direction != "sendrecv") {
			continue
		}
		var label string
		switch m.codecName {
		case "MPEG4-GENERIC", "AAC":
			label = "aac"
		case "PCMA", "PCMU":
			label = "g711"
		default:
			continue
		}
		if !seen[label] {
			seen[label] = true
			codecs = append(codecs, label)
		}
	}
	return codecs, nil
}

// Dial opens the RTSP backchannel to rawURL (rtsp://user:pass@host:port/path)
// and starts the RTP send loop. forceCodec may be "PCMA"/"PCMU" to pin a G.711
// track, "AAC" to pin an MPEG4-GENERIC track (raw AUs are fed via FeedAU), or ""
// to auto-select G.711 from the SDP. In the G.711 path the returned Session
// sends silence until the caller feeds audio; in the AAC path it stays quiet
// until the first AU arrives.
func Dial(ctx context.Context, rawURL, forceCodec string) (*Session, error) {
	c, err := dialRTSP(rawURL)
	if err != nil {
		return nil, err
	}

	s := &Session{
		rtspClient: c,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	uri := c.baseURL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	s.uri = uri

	slog.Debug("backchannel connected", "host", c.baseURL.Host)

	if err := c.options(uri); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("OPTIONS: %w", err)
	}

	sdpRaw, err := c.describe(uri)
	if err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("DESCRIBE: %w", err)
	}

	medias := parseSDP(sdpRaw)
	for _, m := range medias {
		slog.Debug("sdp media", "type", m.mediaType, "dir", m.direction,
			"control", m.control, "codec", m.codecName, "clock", m.clockRate, "pt", m.payloads)
	}

	bcMedia, err := findBackchannelMedia(medias, forceCodec)
	if err != nil {
		c.conn.Close()
		return nil, err
	}

	codec, pt := chooseCodec(bcMedia)
	s.codec = codec
	s.pt = pt
	s.clockRate = bcMedia.clockRate

	controlURL := resolveControlURL(c.baseURL, bcMedia.control)
	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"
	if err := c.setup(controlURL, transport); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("SETUP: %w", err)
	}
	slog.Debug("backchannel SETUP ok", "session", c.sessionID)

	time.Sleep(100 * time.Millisecond)

	if err := c.play(uri); err != nil {
		c.conn.Close()
		return nil, fmt.Errorf("PLAY: %w", err)
	}

	time.Sleep(1 * time.Second)

	c.startReader()
	s.keepaliveDone = make(chan struct{})
	go keepalive(c, uri, s.keepaliveDone)

	if codec == "AAC" {
		s.auIn = make(chan []byte, 64)
		go s.sendLoopAAC()
	} else {
		s.audioIn = make(chan []int16, 64)
		go s.sendLoop()
	}

	slog.Debug("backchannel live", "codec", codec, "pt", pt, "clock", s.clockRate)

	return s, nil
}

// Codec returns the negotiated backchannel codec: "PCMA", "PCMU", or "AAC".
func (s *Session) Codec() string { return s.codec }

func (s *Session) sendLoop() {
	defer close(s.done)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	var buf []int16
	maxBuf := TargetRate * 4

	ssrc := rand.Uint32()
	seq := uint16(rand.Intn(65536))
	ts := uint32(rand.Intn(65536))

	for {
		select {
		case <-s.stop:
			return
		case samples := <-s.audioIn:
			buf = append(buf, samples...)
			if len(buf) > maxBuf {
				buf = buf[len(buf)-maxBuf:]
			}
		case <-ticker.C:
			var frame []int16
			if len(buf) >= FrameSamples {
				frame = buf[:FrameSamples]
				buf = buf[FrameSamples:]
			} else {
				frame = make([]int16, FrameSamples)
				copy(frame, buf)
				buf = buf[:0]
			}

			var payload []byte
			if s.codec == "PCMU" {
				payload = encodeULaw(frame)
			} else {
				payload = encodeALaw(frame)
			}

			packet := buildRTPPacket(s.pt, seq, ts, ssrc, seq == 0, payload)
			if err := s.writeInterleaved(0, packet); err != nil {
				slog.Warn("backchannel RTP send failed", "err", err)
				return
			}
			seq++
			ts += FrameSamples
		}
	}
}

// FeedPCM decodes native-rate mono S16LE PCM, resamples it to 8 kHz when needed,
// and queues it for transmission. Oversized bursts are dropped rather than
// blocking the caller (the RTP loop paces at a fixed 20 ms).
func (s *Session) FeedPCM(pcm []byte, nativeRate int) {
	if len(pcm) < 2 {
		return
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	if nativeRate > TargetRate {
		samples = lowPassForDecimation(samples, nativeRate, TargetRate)
		samples = resampleLinear(samples, nativeRate, TargetRate)
	}
	select {
	case s.audioIn <- samples:
	default:
		slog.Debug("backchannel buffer full, dropping samples", "n", len(samples))
	}
}

// sendLoopAAC forwards raw AAC access units to the camera. Unlike the G.711
// loop it is not clocked: push-to-talk audio arrives in real time from the
// client's encoder, so each AU is wrapped in RFC 3640 AAC-hbr framing and sent
// as it comes, with the RTP timestamp advanced by one AAC frame (1024 samples).
func (s *Session) sendLoopAAC() {
	defer close(s.done)

	ssrc := rand.Uint32()
	seq := uint16(rand.Intn(65536))
	ts := uint32(rand.Intn(65536))

	for {
		select {
		case <-s.stop:
			return
		case au := <-s.auIn:
			if isADTS(au) {
				au = au[adtsHeaderLen(au):]
			}
			if len(au) == 0 {
				continue
			}
			packet := buildRTPPacket(s.pt, seq, ts, ssrc, true, aacRTPPayload(au))
			if err := s.writeInterleaved(0, packet); err != nil {
				slog.Warn("backchannel AAC send failed", "err", err)
				return
			}
			seq++
			ts += AACFrameSamples
		}
	}
}

// FeedAU queues one raw AAC-LC access unit (optionally ADTS-wrapped) for
// transmission on the AAC backchannel. It is a no-op on a G.711 session. The AU
// must match the track's advertised config (AAC-LC, the SDP clock rate, mono);
// oversized bursts are dropped rather than blocking the caller.
func (s *Session) FeedAU(au []byte) {
	if s.auIn == nil || len(au) == 0 {
		return
	}
	b := make([]byte, len(au))
	copy(b, au)
	select {
	case s.auIn <- b:
	default:
		slog.Debug("backchannel AAC buffer full, dropping AU", "n", len(au))
	}
}

// Close stops the send loop, tears down the RTSP session and closes the TCP
// connection. It is idempotent-safe to call once per Session.
func (s *Session) Close() {
	close(s.stop)
	<-s.done
	close(s.keepaliveDone)
	_ = s.writeRequest("TEARDOWN", s.uri)
	time.Sleep(200 * time.Millisecond)
	s.stopReader()
	s.conn.Close()
}
