package backchannel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// testCamera is a minimal RTSP server advertising a listen-only audio track and
// a PCMA back channel, recording what a session sends it.
type testCamera struct {
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	addr   string

	mu      sync.Mutex
	packets []*rtp.Packet
	played  chan struct{}
	once    sync.Once
}

func newTestCamera(t *testing.T) *testCamera {
	t.Helper()

	// Take a free port and hand it to the server. The gap between closing the
	// probe listener and the server binding is a test-only race we accept.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cam := &testCamera{addr: addr, played: make(chan struct{})}
	cam.server = &gortsplib.Server{Handler: cam, RTSPAddress: addr}
	if err := cam.server.Start(); err != nil {
		t.Fatalf("start test camera: %v", err)
	}

	cam.stream = &gortsplib.ServerStream{
		Server: cam.server,
		Desc: &description.Session{Medias: []*description.Media{
			{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{&format.G711{PayloadTyp: 8, SampleRate: 8000, ChannelCount: 1}},
			},
			{
				Type:          description.MediaTypeAudio,
				IsBackChannel: true,
				Formats:       []format.Format{&format.G711{PayloadTyp: 8, SampleRate: 8000, ChannelCount: 1}},
			},
		}},
	}
	if err := cam.stream.Initialize(); err != nil {
		t.Fatalf("init test stream: %v", err)
	}

	t.Cleanup(func() {
		cam.stream.Close()
		cam.server.Close()
	})
	return cam
}

func (c *testCamera) url() string { return fmt.Sprintf("rtsp://%s/talk", c.addr) }

func (c *testCamera) received() []*rtp.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*rtp.Packet(nil), c.packets...)
}

func (c *testCamera) OnConnOpen(*gortsplib.ServerHandlerOnConnOpenCtx)         {}
func (c *testCamera) OnConnClose(*gortsplib.ServerHandlerOnConnCloseCtx)       {}
func (c *testCamera) OnSessionOpen(*gortsplib.ServerHandlerOnSessionOpenCtx)   {}
func (c *testCamera) OnSessionClose(*gortsplib.ServerHandlerOnSessionCloseCtx) {}

func (c *testCamera) OnDescribe(
	*gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *testCamera) OnSetup(
	*gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *testCamera) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	ctx.Session.OnPacketRTPAny(func(_ *description.Media, _ format.Format, pkt *rtp.Packet) {
		c.mu.Lock()
		c.packets = append(c.packets, pkt)
		n := len(c.packets)
		c.mu.Unlock()
		if n >= 3 {
			c.once.Do(func() { close(c.played) })
		}
	})
	ctx.Session.OnPacketRTCPAny(func(*description.Media, rtcp.Packet) {})
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// A session negotiates the back channel, streams silence to keep it warm, and
// carries fed audio — all through gortsplib's client.
func TestSessionTalksToCamera(t *testing.T) {
	cam := newTestCamera(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := Dial(ctx, cam.url(), "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	if sess.Codec() != codecPCMA {
		t.Errorf("Codec() = %q, want %q", sess.Codec(), codecPCMA)
	}

	// 8 kHz mono S16LE: 320 bytes is one 20 ms frame.
	sess.FeedPCM(make([]byte, 320), TargetRate)

	select {
	case <-cam.played:
	case <-time.After(5 * time.Second):
		t.Fatal("camera received no RTP within 5s")
	}

	pkts := cam.received()
	if pkts[0].PayloadType != 8 {
		t.Errorf("payload type = %d, want 8", pkts[0].PayloadType)
	}
	if !pkts[0].Marker {
		t.Error("first packet has no marker bit (RFC 3550: start of talkspurt)")
	}
	if len(pkts[0].Payload) != FrameSamples {
		t.Errorf("payload = %d bytes, want %d", len(pkts[0].Payload), FrameSamples)
	}
	for i := 1; i < len(pkts); i++ {
		if pkts[i].SequenceNumber != pkts[i-1].SequenceNumber+1 {
			t.Errorf("sequence jumped %d -> %d", pkts[i-1].SequenceNumber, pkts[i].SequenceNumber)
		}
		if pkts[i].Timestamp != pkts[i-1].Timestamp+FrameSamples {
			t.Errorf("timestamp stepped %d -> %d, want +%d",
				pkts[i-1].Timestamp, pkts[i].Timestamp, FrameSamples)
		}
		if pkts[i].SSRC != pkts[0].SSRC {
			t.Error("SSRC changed mid-stream")
		}
	}
}

// Two owners can race to close the same session — the talk handler's deferred
// Close and the shutdown path's CloseAllTalk.
func TestSessionCloseIdempotent(t *testing.T) {
	cam := newTestCamera(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := Dial(ctx, cam.url(), "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			sess.Close()
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return")
		}
	}

	// Feeding a closed session must not panic or block.
	sess.FeedPCM(make([]byte, 320), TargetRate)
}

// Dial must not outlive a cancelled context even when the camera stalls.
func TestDialRespectsContext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-make(chan struct{}) // never answer
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Dial(ctx, "rtsp://"+l.Addr().String()+"/talk", ""); err == nil {
		t.Fatal("want an error from a camera that never answers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Dial took %s, want it bounded by the context", elapsed)
	}
}
