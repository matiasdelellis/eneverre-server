package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"eneverre/internal/auth"
	"eneverre/internal/backchannel"
	"eneverre/internal/camera"
)

// talkSubprotocol is the WebSocket subprotocol the browser client offers. The
// access token is offered as a second subprotocol value alongside it, keeping
// the token out of the URL (and therefore out of reverse-proxy access logs).
const talkSubprotocol = "eneverre-talk"

// Keepalive tuning for the talk WebSocket: ping the client every talkPingPeriod
// and drop the session if no pong (or audio) arrives within talkPongWait. This
// reclaims the per-camera slot when a client dies silently (half-open TCP)
// instead of leaking the RTSP session forever.
const (
	talkPongWait   = 60 * time.Second
	talkPingPeriod = 25 * time.Second
)

// talkUpgrader upgrades the push-to-talk WebSocket. Origin is not restricted
// here: auth is enforced by the access token before the upgrade, and the token
// is unforgeable so there is no CSRF vector. Advertising talkSubprotocol makes
// gorilla echo it back (and only it, never the token) in the handshake.
var talkUpgrader = websocket.Upgrader{
	Subprotocols: []string{talkSubprotocol},
	CheckOrigin:  func(r *http.Request) bool { return true },
}

// talkToken extracts the access token, preferring the Sec-WebSocket-Protocol
// carrier (the entry that is not talkSubprotocol) and falling back to the
// ?token= query param for non-browser clients. Our access tokens are base64url,
// so they are always valid subprotocol tokens.
func talkToken(r *http.Request) string {
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if p != "" && p != talkSubprotocol {
			return p
		}
	}
	return r.URL.Query().Get("token")
}

// handleTalk streams push-to-talk audio from a client to a camera's ONVIF
// Profile T backchannel. The client connects a WebSocket, sends a JSON handshake
// {"sampleRate": N}, then a stream of binary S16LE PCM frames; the server
// resamples to 8 kHz, encodes G.711 and relays RTP to the camera (see
// internal/backchannel). At most one talk session per camera.
//
// Auth: the access token comes via the Sec-WebSocket-Protocol carrier (or the
// ?token= query param, or a Bearer header for non-browser clients), validated
// before the upgrade. The camera must define a `backchannel` URL in its INI
// (Capabilities.Talk). Once the RTSP session is live the server sends a single
// text message {"status":"ready"} so the client's UI can switch from
// "connecting" to "talking"; thereafter the socket is receive-only apart from
// keepalive pings.
func (a *App) handleTalk(w http.ResponseWriter, r *http.Request) {
	u := auth.VerifyToken(a.db, talkToken(r))
	if u == nil {
		u = auth.Current(a.db, r)
	}
	if u == nil {
		a.unauthorized(w)
		return
	}

	cam := camera.Get(a.cameras, r.PathValue("cam_id"))
	if cam == nil || !cam.Capabilities.Talk || cam.Backchannel == "" {
		httpError(w, http.StatusNotFound, "Two-way audio not available")
		return
	}

	// Reserve the single talk slot for this camera before the (relatively slow)
	// RTSP setup, so two concurrent clients can never both open a backchannel to
	// it. A nil placeholder marks the reservation until Dial replaces it.
	a.talkMu.Lock()
	if _, busy := a.talk[cam.ID]; busy {
		a.talkMu.Unlock()
		httpError(w, http.StatusConflict, "An active talk session already exists for this camera")
		return
	}
	a.talk[cam.ID] = nil
	a.talkMu.Unlock()

	release := func() {
		a.talkMu.Lock()
		delete(a.talk, cam.ID)
		a.talkMu.Unlock()
	}

	conn, err := talkUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("talk websocket upgrade failed", "camera", cam.ID, "err", err)
		release()
		return
	}
	defer conn.Close()

	sess, err := backchannel.Dial(context.Background(), cam.Backchannel, "")
	if err != nil {
		slog.Warn("talk backchannel dial failed", "camera", cam.ID, "err", err)
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "RTSP error: "+err.Error()))
		release()
		return
	}

	a.talkMu.Lock()
	a.talk[cam.ID] = sess
	a.talkMu.Unlock()

	defer func() {
		sess.Close()
		release()
		slog.Debug("talk session closed", "camera", cam.ID)
	}()

	slog.Info("talk session started", "camera", cam.ID, "user", u.Username)

	// The camera backchannel is live: signal readiness so the client switches
	// from "connecting" to "talking". Sent before the ping goroutine starts, so
	// this is the only writer at this point (gorilla forbids concurrent writes).
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"status":"ready"}`)); err != nil {
		return
	}

	// Keepalive: pings from a dedicated goroutine (the only writer from here on),
	// pongs and audio both refresh the read deadline.
	conn.SetReadDeadline(time.Now().Add(talkPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(talkPongWait))
		return nil
	})
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(talkPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-pingDone:
				return
			}
		}
	}()
	defer close(pingDone)

	// Handshake: the first message is the JSON {"sampleRate": N}.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var init struct {
		SampleRate int `json:"sampleRate"`
	}
	if err := json.Unmarshal(msg, &init); err != nil || init.SampleRate < backchannel.TargetRate {
		return
	}

	// Audio: a stream of binary S16LE PCM messages at the negotiated rate.
	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(talkPongWait))
		if mt != websocket.BinaryMessage {
			continue
		}
		sess.FeedPCM(msg, init.SampleRate)
	}
}
