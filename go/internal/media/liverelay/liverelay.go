// Package liverelay re-serves live camera streams over RTSP to many clients,
// so viewers connect to this server instead of the cameras. It is a pure RTP
// passthrough (no decode, no remux): the recorder forwards each incoming RTP
// packet to a gortsplib ServerStream, which fans it out to readers.
//
// The relay is multi-path: one listen address serves N cameras, each published
// under its own path (the camera id). Readers connect to
// rtsp://host:port/<id>/<exp>.<sig>; Authorize validates the signed token and
// returns the camera id used to find the stream.
package liverelay

import (
	"net"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"
)

// Relay is an RTSP server that republishes many live sources to many readers,
// one path per source.
type Relay struct {
	Address string // e.g. ":8554"
	// Authorize, when set, validates the request path and returns the stream
	// path (camera id). The expected public form is <id>/<exp>.<sig>. A nil
	// callback leaves the relay open (tests/integrated trusted setups).
	Authorize func(path string) (streamPath string, ok bool)
	// OnAuthFailure, when set, receives a rejected path: the peer IP, an empty
	// username (the relay uses path tokens, not RTSP credentials) and the
	// camera id only — never the token.
	OnAuthFailure func(ip, user, path string)
	Logf          func(string, ...any)

	srv     *gortsplib.Server
	mu      sync.RWMutex
	streams map[string]*gortsplib.ServerStream // path -> stream
}

// Initialize starts the RTSP server.
func (r *Relay) Initialize() error {
	if r.Logf == nil {
		r.Logf = func(string, ...any) {}
	}
	r.streams = map[string]*gortsplib.ServerStream{}
	r.srv = &gortsplib.Server{
		Handler:     r,
		RTSPAddress: r.Address,
	}
	if err := r.srv.Start(); err != nil {
		return err
	}
	auth := "no auth"
	if r.Authorize != nil {
		auth = "signed path token"
	}
	r.Logf("RTSP relay listening on %s (%s)", r.Address, auth)
	return nil
}

// Close stops the server and drops all streams.
func (r *Relay) Close() {
	r.mu.Lock()
	for p, st := range r.streams {
		st.Close()
		delete(r.streams, p)
	}
	r.mu.Unlock()
	if r.srv != nil {
		r.srv.Close()
	}
}

// SetSource creates (or replaces) the served stream for a path from a source
// description. Called by a recorder whenever it (re)connects to its camera.
func (r *Relay) SetSource(path string, desc *description.Session) error {
	path = normalize(path)
	promoteH264PacketizationMode(desc)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.streams[path]; st != nil {
		st.Close()
		delete(r.streams, path)
	}
	st := &gortsplib.ServerStream{Server: r.srv, Desc: desc}
	if err := st.Initialize(); err != nil {
		return err
	}
	r.streams[path] = st
	r.Logf("live source ready: %s (%d media(s))", path, len(desc.Medias))
	return nil
}

// promoteH264PacketizationMode rewrites any H264 format advertising
// packetization-mode=0 to mode 1, in place, before the description reaches the
// ServerStream. gortsplib's server refuses to serve mode 0
// (ErrServerH264PacketizationMode0), which would fail SetSource and take the
// whole camera down. Many cameras (notably Hikvision/Dahua) advertise mode 0 in
// their SDP yet actually send fragmented FU-A packets — i.e. mode 1 behaviour.
// Mode 1 is a strict superset of mode 0 (it also allows single NAL units and
// STAP-A), so promoting it is always safe: the RTP is passed through untouched
// and readers just see a more permissive SDP.
//
// The format is mutated in place rather than cloned on purpose: the ServerStream
// routes writes by *description.Media pointer identity (st.medias is keyed by
// the pointer), and the recorder forwards packets with the same Media pointer it
// passed here via WritePacketRTP. Cloning the media would break that routing and
// silently drop every packet. Changing only PacketizationMode is harmless to the
// recorder, whose H264 decoder accepts FU-A/STAP-A/single-NAL regardless of mode.
func promoteH264PacketizationMode(desc *description.Session) {
	for _, medi := range desc.Medias {
		for _, forma := range medi.Formats {
			if h264, ok := forma.(*format.H264); ok && h264.PacketizationMode == 0 {
				h264.PacketizationMode = 1
			}
		}
	}
}

// ClearSource drops the stream for a path (camera disconnected). Readers get
// closed.
func (r *Relay) ClearSource(path string) {
	path = normalize(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.streams[path]; st != nil {
		st.Close()
		delete(r.streams, path)
	}
}

// WritePacketRTP forwards one packet to readers of a path. No-op when there is
// no source for it.
func (r *Relay) WritePacketRTP(path string, media *description.Media, pkt *rtp.Packet) {
	path = normalize(path)
	r.mu.RLock()
	st := r.streams[path]
	r.mu.RUnlock()
	if st != nil {
		st.WritePacketRTP(media, pkt) //nolint:errcheck
	}
}

func (r *Relay) stream(path string) *gortsplib.ServerStream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.streams[normalize(path)]
}

// authorize validates the request path and returns the camera stream path.
// The public path is <camera-id>/<exp>.<sig>; only the camera id is used for
// stream lookup and logging.
func (r *Relay) authorize(conn *gortsplib.ServerConn, path string) (string, bool) {
	p := normalize(path)
	if r.Authorize == nil {
		return p, true
	}
	cam, ok := r.Authorize(p)
	if !ok {
		r.reportAuthFailure(conn, p)
		return "", false
	}
	return normalize(cam), true
}

// reportAuthFailure forwards a rejected path to OnAuthFailure. The token is
// stripped: only the camera id (the first segment) reaches the security log.
func (r *Relay) reportAuthFailure(conn *gortsplib.ServerConn, path string) {
	if r.OnAuthFailure == nil {
		return
	}
	ip := ""
	if nc := conn.NetConn(); nc != nil {
		if host, _, err := net.SplitHostPort(nc.RemoteAddr().String()); err == nil {
			ip = host
		}
	}
	r.OnAuthFailure(ip, "", relayLogPath(path))
}

// relayLogPath returns the camera id part of a public path (<id>/<token>),
// never the token. A path with no second segment is malformed and logs no
// path at all, so a request that accidentally puts a token alone in the path
// cannot leak it into the security log.
func relayLogPath(path string) string {
	p := normalize(path)
	i := strings.IndexByte(p, '/')
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// normalize strips the leading slash gortsplib includes in ctx.Path so callers
// can pass either "cam" or "/cam" and match what readers request.
func normalize(path string) string { return strings.TrimPrefix(path, "/") }

// --- ServerHandler (read-only) ---

// OnConnOpen implements the handler.
func (r *Relay) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx) {}

// OnConnClose implements the handler.
func (r *Relay) OnConnClose(_ *gortsplib.ServerHandlerOnConnCloseCtx) {}

// OnSessionOpen implements the handler.
func (r *Relay) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {}

// OnSessionClose implements the handler.
func (r *Relay) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {}

// OnDescribe implements the handler.
func (r *Relay) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (
	*base.Response, *gortsplib.ServerStream, error,
) {
	streamPath, ok := r.authorize(ctx.Conn, ctx.Path)
	if !ok {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	st := r.stream(streamPath)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnSetup implements the handler.
func (r *Relay) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (
	*base.Response, *gortsplib.ServerStream, error,
) {
	streamPath, ok := r.authorize(ctx.Conn, ctx.Path)
	if !ok {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	st := r.stream(streamPath)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnPlay implements the handler.
func (r *Relay) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}
