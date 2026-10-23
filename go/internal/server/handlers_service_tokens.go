package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"eneverre/internal/servicetoken"
)

// Service tokens are scoped, read-only bearer tokens for machine
// integrations (Zabbix, dashboards, scripts). They are managed by an admin
// session, shown once at creation, and stored hashed. They can only reach the
// read-only endpoints that check their scope; requireUser rejects them
// everywhere else.

type createServiceTokenRequest struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days"`
}

type createServiceTokenResponse struct {
	servicetoken.Token
	// Secret is the raw token and is returned exactly once. Never persisted.
	Secret string `json:"token"`
}

func (a *App) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.serviceTokens == nil {
		httpError(w, http.StatusServiceUnavailable, "Service tokens unavailable")
		return
	}
	list, err := a.serviceTokens.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []servicetoken.Token{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleCreateServiceToken(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.serviceTokens == nil {
		httpError(w, http.StatusServiceUnavailable, "Service tokens unavailable")
		return
	}
	var req createServiceTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		httpError(w, http.StatusUnprocessableEntity, "name is required (max 120 chars)")
		return
	}
	if len(req.Scopes) == 0 {
		// Least-privilege default: enough for the Zabbix template.
		req.Scopes = []string{servicetoken.ScopeStatus}
	}
	days := req.ExpiresInDays
	if days == 0 {
		days = 365
	}
	if days < 1 || days > 3650 {
		httpError(w, http.StatusUnprocessableEntity, "expires_in_days must be between 1 and 3650")
		return
	}
	expiresAt := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	raw, tok, err := a.serviceTokens.Create(req.Name, req.Scopes, expiresAt)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	slog.Info("service token created", "id", tok.ID, "name", tok.Name, "scopes", tok.Scopes,
		"expires_at", tok.ExpiresAt)
	writeJSON(w, http.StatusCreated, createServiceTokenResponse{Token: tok, Secret: raw})
}

func (a *App) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	if a.requireAdmin(w, r) == nil {
		return
	}
	if a.serviceTokens == nil {
		httpError(w, http.StatusServiceUnavailable, "Service tokens unavailable")
		return
	}
	id, ok := servicetoken.ParseID(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "Service token not found")
		return
	}
	if err := a.serviceTokens.Revoke(id); err != nil {
		if errors.Is(err, servicetoken.ErrNotFound) {
			httpError(w, http.StatusNotFound, "Service token not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("service token revoked", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "revoked"})
}
