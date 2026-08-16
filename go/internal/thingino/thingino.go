// Package thingino makes the direct HTTP calls to Thingino cameras (PTZ move
// and JPEG snapshot). A single shared client with connection reuse keeps the
// connection pool warm across requests.
//
// Backend note: this package targets the prudynt/stable firmware surface —
// json-prudynt.cgi (config fragments), json-imp.cgi (IMP/illumination
// commands) and json-heartbeat-slow.cgi (state). Newer firmware (master /
// raptor) replaces it with a REST "agent" on :8080 (proxied through
// /x/agent.cgi): GET /api/v1/config, POST /api/v1/actions/{record,privacy,
// daynight,snapshot}, PATCH /api/v1/settings/{motion/enabled,
// audio/mic-enabled,audio/spk-enabled}, GET /api/v1/runtime/media, authed
// with the same API key. Verified against a live stable camera (2026-08):
// the agent there exposes ONLY POST /api/v1/config with the same
// fragment-apply semantics as json-prudynt.cgi — none of the REST routes
// exist yet. When cameras migrate to raptor, the setters below should be
// ported to the agent routes (the config-fragment shape remains the
// contract in the meantime).
package thingino

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

var client = &http.Client{}

// StatusError is returned when the camera is reached but responds with an HTTP
// error status. It lets callers tell an auth failure (401/403 — usually a stale
// or changed API token) apart from the camera being unreachable.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("status %d", e.Code)
}

// motorResponse captures the message block of the {code, result, message:
// {xpos, ypos, ...}} shape every json-motor.cgi reply uses — only xpos/ypos
// are read (the firmware returns them as strings; the parser below converts
// them to floats). The full response is relayed to HTTP clients unchanged by
// Move/MoveAbs/Recalibrate, so nothing else needs decoding.
type motorResponse struct {
	Message struct {
		XPos string `json:"xpos"`
		YPos string `json:"ypos"`
	} `json:"message"`
}

// ParseMotorPos extracts the firmware's (xpos, ypos) from a json-motor.cgi
// response body. Returns ok=false when the body is malformed, the message
// block is missing, or the position fields are not parseable as floats — the
// caller should treat that as "no position update" (some firmwares echo the
// position only on certain d= modes).
func ParseMotorPos(body []byte) (x, y float64, ok bool) {
	var r motorResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, 0, false
	}
	if r.Message.XPos == "" && r.Message.YPos == "" {
		return 0, 0, false
	}
	x, errX := strconv.ParseFloat(r.Message.XPos, 64)
	y, errY := strconv.ParseFloat(r.Message.YPos, 64)
	if errX != nil || errY != nil {
		return 0, 0, false
	}
	return x, y, true
}

// Position reads the current motor position (d=j). The returned (xpos, ypos)
// are in firmware-native steps; the caller is responsible for converting them
// to pan/tilt in degrees using the camera's calibration. Used at startup to
// prime the server-side position cache and on demand by /api/.../ptz/position
// when the cache is cold.
func Position(host, apiKey string) (x, y float64, err error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=j&token=%s", host, apiKey)
	body, err := doGet(url, apiKey, 3*time.Second)
	if err != nil {
		return 0, 0, err
	}
	x, y, ok := ParseMotorPos(body)
	if !ok {
		return 0, 0, fmt.Errorf("thingino: could not parse position from response")
	}
	return x, y, nil
}

// Move issues a relative PTZ move (d=g) — x/y are deltas from the current
// position — and returns the camera's raw JSON response. Used by the
// directional pad.
func Move(host, apiKey string, x, y float64) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=g&x=%s&y=%s&token=%s", host, formatCoord(x), formatCoord(y), apiKey)
	return doGet(url, apiKey, 3*time.Second)
}

// MoveAbs issues an absolute PTZ move (d=x) — x/y are target coordinates, not
// deltas. Used for fixed positions like home and privacy. The travel can span
// the full range, so it gets a longer timeout than a relative step.
func MoveAbs(host, apiKey string, x, y float64) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=x&x=%s&y=%s&token=%s", host, formatCoord(x), formatCoord(y), apiKey)
	return doGet(url, apiKey, 10*time.Second)
}

// Recalibrate runs the motor's recalibration routine (d=r). It physically homes
// the gimbal against its end stops, so it gets a longer timeout than a move.
func Recalibrate(host, apiKey string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=r&token=%s", host, apiKey)
	return doGet(url, apiKey, 10*time.Second)
}

// Heartbeat is the subset of json-heartbeat-slow.cgi we consume. The endpoint
// reports the camera's full live runtime state; besides privacy (which the
// server enforces) we decode the read-only status fields worth surfacing to
// the operator on /api/status: day/night mode and the illuminator states,
// motion detection, audio in/out and per-channel recording. None of these are
// editable through the API today — they mirror the camera's own live state
// (refreshed on a slow background loop, see server.heartbeatLoop).
type Heartbeat struct {
	PrivacyEnabled Bool `json:"privacy_enabled"`

	DaynightMode       string `json:"daynight_mode"` // "day" / "night"
	DaynightEnabled    Bool   `json:"daynight_enabled"`
	DaynightBrightness Num    `json:"daynight_brightness"`
	MotionEnabled      Bool   `json:"motion_enabled"`
	MicEnabled         Bool   `json:"mic_enabled"`
	SpkEnabled         Bool   `json:"spk_enabled"`
	RecCh0             Bool   `json:"rec_ch0"`
	RecCh1             Bool   `json:"rec_ch1"`
	IRCutState         Bool   `json:"ircut_state"`
	IR850State         Bool   `json:"ir850_state"`
	IR940State         Bool   `json:"ir940_state"`
	WhiteState         Bool   `json:"white_state"`
}

// Num decodes a number that different Thingino firmwares spell either as a
// JSON number or as a quoted string ("80"). An unparseable value decodes as 0
// rather than failing the whole payload, mirroring Bool — a single odd field
// shouldn't make an otherwise reachable camera look offline.
type Num int

func (n *Num) UnmarshalJSON(data []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(data), `"`))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if v, err := strconv.Atoi(s); err == nil {
		*n = Num(v)
	}
	return nil
}

// Bool decodes a boolean that different Thingino firmwares spell differently: a
// real JSON bool, a number (0/1), or a quoted one ("0"/"1", "true"/"false").
// Anything non-zero / non-empty counts as true; an unparseable value decodes as
// false rather than failing the whole payload, since a single odd field
// shouldn't make an otherwise reachable camera look offline.
type Bool bool

func (b *Bool) UnmarshalJSON(data []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(data), `"`))
	switch s {
	case "", "null":
		*b = false
		return nil
	}
	if v, err := strconv.ParseBool(s); err == nil {
		*b = Bool(v)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*b = f != 0
		return nil
	}
	*b = false
	return nil
}

// State fetches the camera's slow heartbeat. This is a heavy call on the camera
// (~1s), so use it sparingly (e.g. once at startup), never on a hot path.
func State(host, apiKey string) (*Heartbeat, error) {
	url := fmt.Sprintf("%s/x/json-heartbeat-slow.cgi?token=%s", host, apiKey)
	body, err := doGet(url, apiKey, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var hb Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

// MotorParams is the subset of json-motor-params.cgi we consume: the total
// firmware step count per axis for this gimbal (steps_pan/steps_tilt — the
// mechanical range Position never reveals, since it only reports the current
// position) and the firmware's own configured home position (pos_0_x/
// pos_0_y), in the same step units.
type MotorParams struct {
	StepsPan  int `json:"steps_pan"`
	StepsTilt int `json:"steps_tilt"`
	Pos0X     int `json:"pos_0_x"`
	Pos0Y     int `json:"pos_0_y"`
}

// Params fetches the motor's step calibration and home position. Used by the
// wizard's Thingino test to prefill the PTZ step calibration and home
// position instead of the operator hand-typing firmware step counts.
func Params(host, apiKey string) (*MotorParams, error) {
	url := fmt.Sprintf("%s/x/json-motor-params.cgi?token=%s", host, apiKey)
	body, err := doGet(url, apiKey, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var p MotorParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPrivacy toggles prudynt's privacy mode (lens blackout) on the camera.
func SetPrivacy(host, apiKey string, enabled bool) (json.RawMessage, error) {
	return SetPrudynt(host, apiKey, map[string]any{"privacy": map[string]any{"enabled": enabled}})
}

// SetPrudynt posts a config fragment to json-prudynt.cgi — the prudynt
// configuration document, applied as a partial update (the camera merges the
// fragment into its live config, exactly like the thingino web UI does).
// Top-level keys mirror prudynt.cfg: privacy, motion, audio, streams, ... The
// caller builds the fragment; only the keys present are changed.
func SetPrudynt(host, apiKey string, fragment map[string]any) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-prudynt.cgi?token=%s", host, apiKey)
	body, err := json.Marshal(fragment)
	if err != nil {
		return nil, err
	}
	return doPost(url, apiKey, body, 5*time.Second)
}

// Imp sends a command to json-imp.cgi, the IMP sensor/illumination control
// endpoint of the thingino web UI. The command vocabulary (verified against
// the firmware's own web UI): "auto" (day/night auto, val 0/1), "daynight"
// (val "day"|"night"), "ircut", "ir850", "ir940", "white", "color" (val 0/1).
// cmd must be on the caller's allowlist — the firmware's vocabulary is broad
// and partly board-specific, so nothing is forwarded blindly. val is omitted
// from the JSON body when nil (some commands take no value); it may be a
// number or a string depending on the command.
func Imp(host, apiKey, cmd string, val any) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-imp.cgi?token=%s", host, apiKey)
	payload := map[string]any{"cmd": cmd}
	if val != nil {
		payload["val"] = val
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return doPost(url, apiKey, body, 5*time.Second)
}

// formatCoord renders a coordinate without a trailing ".0" so whole numbers
// stay integer-shaped on the wire (e.g. "50", not "50.0").
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Thumb fetches a JPEG snapshot as raw bytes.
func Thumb(host, apiKey string) ([]byte, error) {
	url := fmt.Sprintf("%s/x/ch0.jpg?token=%s", host, apiKey)
	return doGet(url, apiKey, 10*time.Second)
}

func doGet(url, apiKey string, timeout time.Duration) ([]byte, error) {
	return do(http.MethodGet, url, apiKey, nil, timeout)
}

func doPost(url, apiKey string, body []byte, timeout time.Duration) ([]byte, error) {
	return do(http.MethodPost, url, apiKey, body, timeout)
}

// maxResponseBytes caps how much of a camera response is buffered, matching
// the server's generic snapshot cap (8 MiB).
const maxResponseBytes = 8 << 20

func do(method, url, apiKey string, payload []byte, timeout time.Duration) ([]byte, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Two auth channels for two firmware generations: the `token` query
	// parameter (already embedded in the URL by the callers, accepted by
	// older firmwares) and the X-API-Key header (what the current web UI
	// sends — newer firmwares reject ?token= with a 401). Sending both is
	// harmless: each firmware ignores the one it doesn't know.
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Cap the read: a camera with broken firmware (or a compromised one) that
	// streams an endless body must not exhaust the NVR's RAM. 8 MiB matches
	// the generic snapshot cap and comfortably fits any thumbnail/JSON reply.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("thingino: response from %s exceeds %d bytes", url, maxResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return body, nil
}
