package recorder

import (
	"bufio"
	"net"
	"net/textproto"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fake404RTSP answers every RTSP request with 404, keeping the connection
// open — the shape of a camera with a wrong stream path.
func fake404RTSP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tp := textproto.NewReader(bufio.NewReader(c))
				for {
					if _, err := tp.ReadLine(); err != nil { // request line
						return
					}
					hdr, err := tp.ReadMIMEHeader()
					if err != nil {
						return
					}
					cseq := hdr.Get("Cseq")
					if _, err := c.Write([]byte("RTSP/1.0 404 Not Found\r\nCSeq: " + cseq + "\r\n\r\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return "rtsp://" + ln.Addr().String() + "/missing"
}

// A failed connect must release the gortsplib client: its run() goroutine
// and TCP socket otherwise outlive Start, and the engine's retry loop (about
// one attempt per second on a misconfigured camera) leaks one of each per try.
func TestStartReleasesClientOnFailedDescribe(t *testing.T) {
	url := fake404RTSP(t)
	settle := func() int {
		time.Sleep(200 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	r := &Recorder{URL: url, Transport: "tcp", PathName: "cam", PathFormat: t.TempDir() + "/%path/%s"}
	if err := r.Start(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Start = %v, want a 404 error", err)
	}
	base := settle()
	for i := 0; i < 20; i++ {
		_ = r.Start()
	}
	if got := settle(); got > base+3 {
		t.Errorf("goroutines grew from %d to %d over 20 failed connects; the RTSP client is leaking", base, got)
	}
}
