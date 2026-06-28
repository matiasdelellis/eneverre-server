package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"eneverre/internal/camera"
	"eneverre/internal/events"
)

func (a *App) eventsPreSeconds() int  { return a.cfg.Events.GetInt("pre_seconds", 5) }
func (a *App) eventsPostSeconds() int { return a.cfg.Events.GetInt("post_seconds", 5) }
func (a *App) webhookSecret() string {
	return strings.TrimSpace(a.cfg.Events.Get("webhook_secret", ""))
}

type parsedWebhook struct {
	timestamp *int64
	event     *string
	duration  *int64
}

func (a *App) handleWebhookEvent(w http.ResponseWriter, r *http.Request) {
	expected := a.webhookSecret()
	if expected == "" {
		httpError(w, http.StatusServiceUnavailable,
			"Webhook auth not configured: set [events] webhook_secret")
		return
	}
	if !validWebhookSecret(r, expected) {
		httpError(w, http.StatusUnauthorized, "Invalid webhook secret")
		return
	}

	camID := r.PathValue("cam_id")
	if camera.Get(a.cameras, camID) == nil {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}

	raw, _ := io.ReadAll(r.Body)
	rawStr := string(raw)

	var parsed *parsedWebhook
	var parseError string
	if strings.TrimSpace(rawStr) != "" {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			parseError = "json: " + err.Error()
		} else if obj, ok := v.(map[string]any); ok {
			pb, verr := parseWebhookObject(obj)
			if verr != "" {
				parseError = "schema: " + verr
			} else {
				parsed = pb
			}
		}
	}

	ts := time.Now().Unix()
	if parsed != nil && parsed.timestamp != nil && *parsed.timestamp != 0 {
		ts = *parsed.timestamp
	}
	eventType := "motion"
	if parsed != nil && parsed.event != nil && *parsed.event != "" {
		eventType = *parsed.event
	}
	var duration *int64
	if parsed != nil {
		duration = parsed.duration
	}
	source := "webhook"
	if parseError != "" {
		snippet := rawStr
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		source = strings.TrimSpace("webhook:raw (" + parseError + "): " + snippet)
	}

	ev, err := events.RecordMotion(a.db, camID, ts, a.eventsPreSeconds(), a.eventsPostSeconds(),
		duration, eventType, source)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Could not record event")
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func validWebhookSecret(r *http.Request, expected string) bool {
	candidates := []string{r.Header.Get("X-Webhook-Secret"), r.URL.Query().Get("token")}
	for _, c := range candidates {
		if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

// parseWebhookObject mirrors the Pydantic WebhookBody: timestamp (int or
// RFC3339 string), event (string), duration_seconds (int >= 0). Returns a
// non-empty error string on a field-level validation failure.
func parseWebhookObject(obj map[string]any) (*parsedWebhook, string) {
	pb := &parsedWebhook{}

	if v, ok := obj["timestamp"]; ok && v != nil {
		switch t := v.(type) {
		case float64:
			n := int64(t)
			pb.timestamp = &n
		case string:
			if strings.TrimSpace(t) != "" {
				n, ok := events.ParseTimestamp(t)
				if !ok {
					return nil, "invalid timestamp"
				}
				pb.timestamp = &n
			}
		default:
			return nil, "invalid timestamp"
		}
	}

	if v, ok := obj["event"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return nil, "invalid event"
		}
		pb.event = &s
	}

	if v, ok := obj["duration_seconds"]; ok && v != nil {
		f, ok := v.(float64)
		if !ok {
			return nil, "invalid duration_seconds"
		}
		n := int64(f)
		if n < 0 {
			return nil, "duration_seconds must be >= 0"
		}
		pb.duration = &n
	}

	return pb, ""
}

type listEventsResponse struct {
	Events []events.Event `json:"events"`
	Total  int            `json:"total"`
}

func (a *App) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	camID := r.PathValue("cam_id")
	if camera.Get(a.cameras, camID) == nil {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}

	since, ok := queryTimestamp(w, r, "since")
	if !ok {
		return
	}
	until, ok := queryTimestamp(w, r, "until")
	if !ok {
		return
	}

	limit, ok := queryBoundedInt(w, r, "limit", 100, 1, 1000)
	if !ok {
		return
	}
	offset, ok := queryBoundedInt(w, r, "offset", 0, 0, 1<<31)
	if !ok {
		return
	}

	evs, total, err := events.List(a.db, camID, since, until, limit, offset)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "Query failed")
		return
	}
	writeJSON(w, http.StatusOK, listEventsResponse{Events: evs, Total: total})
}

func (a *App) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	if a.requireUser(w, r) == nil {
		return
	}
	camID := r.PathValue("cam_id")
	if camera.Get(a.cameras, camID) == nil {
		httpError(w, http.StatusNotFound, "Camera not found")
		return
	}
	eventID, err := strconv.ParseInt(r.PathValue("event_id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "Event not found")
		return
	}
	if _, found, _ := events.Get(a.db, camID, eventID); !found {
		httpError(w, http.StatusNotFound, "Event not found")
		return
	}
	if ok, _ := events.Delete(a.db, camID, eventID); !ok {
		httpError(w, http.StatusNotFound, "Event not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Event deleted"})
}

// queryTimestamp parses an optional since/until param (unix or RFC3339).
// Returns (nil, true) when absent, (nil, false) after writing 400 on a bad
// value.
func queryTimestamp(w http.ResponseWriter, r *http.Request, key string) (*int64, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, true
	}
	n, ok := events.ParseTimestamp(v)
	if !ok {
		httpError(w, http.StatusBadRequest,
			"invalid timestamp '"+v+"': expected unix seconds or RFC3339")
		return nil, false
	}
	return &n, true
}

func queryBoundedInt(w http.ResponseWriter, r *http.Request, key string, def, lo, hi int) (int, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		httpError(w, http.StatusUnprocessableEntity, "invalid "+key)
		return 0, false
	}
	return n, true
}
