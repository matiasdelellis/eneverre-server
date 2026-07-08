package backchannel

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// rtspClient manages a single TCP connection to an RTSP server and implements
// the RTSP 1.0 methods needed to set up an ONVIF backchannel session.
type rtspClient struct {
	conn      net.Conn
	br        *bufio.Reader
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

func dialRTSP(ctx context.Context, rawURL string) (*rtspClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":554"
	}

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	c := &rtspClient{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 4096),
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
	for {
		b, err := c.br.ReadByte()
		if err != nil {
			return 0, nil, nil, fmt.Errorf("read: %w", err)
		}
		headerBuf.WriteByte(b)
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
			if len(body) < contentLen {
				remaining := contentLen - len(body)
				chunk := make([]byte, remaining)
				if _, err := io.ReadFull(c.br, chunk); err != nil {
					return 0, nil, nil, fmt.Errorf("read body: %w", err)
				}
				body = append(body, chunk...)
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
			// Read through c.br, not c.conn: readResponse's bufio read-ahead may
			// have left bytes buffered, and those are invisible to a direct
			// conn.Read. One consistent read path keeps the connection drained.
			_, err := c.br.Read(buf)
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
