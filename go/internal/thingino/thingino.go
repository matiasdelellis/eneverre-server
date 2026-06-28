// Package thingino makes the direct HTTP calls to Thingino cameras (PTZ move
// and JPEG snapshot), porting app/services/thingino_service.py. A single shared
// client with connection reuse stands in for the requests.Session.
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

// Move issues a relative PTZ move (d=g) — x/y are deltas from the current
// position — and returns the camera's raw JSON response. Used by the
// directional pad.
func Move(host, apiKey string, x, y float64) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=g&x=%s&y=%s&token=%s", host, formatCoord(x), formatCoord(y), apiKey)
	return doGet(url, 3*time.Second)
}

// MoveAbs issues an absolute PTZ move (d=x) — x/y are target coordinates, not
// deltas. Used for fixed positions like home and privacy. The travel can span
// the full range, so it gets a longer timeout than a relative step.
func MoveAbs(host, apiKey string, x, y float64) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=x&x=%s&y=%s&token=%s", host, formatCoord(x), formatCoord(y), apiKey)
	return doGet(url, 10*time.Second)
}

// Recalibrate runs the motor's recalibration routine (d=r). It physically homes
// the gimbal against its end stops, so it gets a longer timeout than a move.
func Recalibrate(host, apiKey string) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-motor.cgi?d=r&token=%s", host, apiKey)
	return doGet(url, 10*time.Second)
}

// Heartbeat is the subset of json-heartbeat-slow.cgi we consume. The endpoint
// reports the camera's full live runtime state; we only decode privacy today.
//
// TODO: the heartbeat carries more state/config worth surfacing to the user
// later. Notable fields (see the raw payload for the full set):
//   - daynight_mode ("day"/"night"), daynight_enabled, daynight_brightness — IR/day-night
//   - motion_enabled — motion detection on/off
//   - mic_enabled, spk_enabled — audio in/out
//   - rec_ch0, rec_ch1 — per-channel recording state
//   - ircut_state, ir850_state, ir940_state, white_state — illuminators
//
// Some are read-only status, others map to existing config toggles, so adding
// them means deciding which become user-visible state vs. editable settings.
type Heartbeat struct {
	PrivacyEnabled bool `json:"privacy_enabled"`
}

// State fetches the camera's slow heartbeat. This is a heavy call on the camera
// (~1s), so use it sparingly (e.g. once at startup), never on a hot path.
func State(host, apiKey string) (*Heartbeat, error) {
	url := fmt.Sprintf("%s/x/json-heartbeat-slow.cgi?token=%s", host, apiKey)
	body, err := doGet(url, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var hb Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

// SetPrivacy toggles prudynt's privacy mode (lens blackout) on the camera.
func SetPrivacy(host, apiKey string, enabled bool) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/x/json-prudynt.cgi?token=%s", host, apiKey)
	body := fmt.Sprintf(`{"privacy":{"enabled":%t}}`, enabled)
	return doPost(url, []byte(body), 5*time.Second)
}

// formatCoord renders a coordinate without a trailing ".0" so whole numbers
// stay integer-shaped on the wire (e.g. "50", not "50.0").
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Thumb fetches a JPEG snapshot as raw bytes.
func Thumb(host, apiKey string) ([]byte, error) {
	url := fmt.Sprintf("%s/x/ch0.jpg?token=%s", host, apiKey)
	return doGet(url, 10*time.Second)
}

func doGet(url string, timeout time.Duration) ([]byte, error) {
	return do(http.MethodGet, url, nil, timeout)
}

func doPost(url string, body []byte, timeout time.Duration) ([]byte, error) {
	return do(http.MethodPost, url, body, timeout)
}

func do(method, url string, payload []byte, timeout time.Duration) ([]byte, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return body, nil
}
