package backchannel

import (
	"bufio"
	"net"
	"strings"
	"testing"
)

// pipeRTSPClient returns an rtspClient whose reads come from the returned
// channel; the test writes an RTSP response into it and closes it to end the
// stream.
func pipeRTSPClient(t *testing.T) (*rtspClient, chan []byte) {
	t.Helper()
	client, peer := net.Pipe()
	rc := make(chan []byte, 1)
	t.Cleanup(func() {
		client.Close()
		peer.Close()
	})
	go func() {
		for msg := range rc {
			if _, err := peer.Write(msg); err != nil {
				return
			}
		}
		peer.Close()
	}()
	c := &rtspClient{conn: client, br: bufio.NewReaderSize(client, 4096)}
	return c, rc
}

func TestReadResponseBodyWithinCap(t *testing.T) {
	c, rc := pipeRTSPClient(t)
	rc <- []byte("RTSP/1.0 200 OK\r\nContent-Length: 4\r\n\r\nbody")
	close(rc)

	code, headers, body, err := c.readResponse()
	if err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if code != 200 || string(body) != "body" {
		t.Errorf("readResponse = (%d, %q), want (200, \"body\")", code, body)
	}
	if headers["content-length"] != "4" {
		t.Errorf("content-length header = %q, want 4", headers["content-length"])
	}
}

// TestReadResponseBodyCapOverflow guards the OOM fix: a camera declaring a
// 4 GiB body must yield an error before any allocation is attempted, not a
// make([]byte, 4GiB) that blows up the process.
func TestReadResponseBodyCapOverflow(t *testing.T) {
	c, rc := pipeRTSPClient(t)
	rc <- []byte("RTSP/1.0 200 OK\r\nContent-Length: 4294967295\r\n\r\n")
	close(rc)

	code, _, _, err := c.readResponse()
	if err == nil {
		t.Fatal("readResponse = nil error, want cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q, want an exceeds-cap message", err)
	}
	if code != 0 {
		t.Errorf("status = %d, want 0 on error", code)
	}
}
