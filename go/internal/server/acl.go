package server

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"

	"eneverre/internal/auth"
	"eneverre/internal/camera"
)

// Per-camera ACL. acl_restricted = 0 means "all cameras"; = 1 means only the
// camera_acl rows for that username. Admins bypass the ACL. Service tokens are
// linked to a user, so they inherit exactly this state.

type cameraACLView struct {
	Username   string   `json:"username"`
	Restricted bool     `json:"restricted"`
	CameraIDs  []string `json:"camera_ids"`
}

type cameraACLRequest struct {
	Restricted bool     `json:"restricted"`
	CameraIDs  []string `json:"camera_ids"`
}

// aclState returns whether username is restricted and, when it is, the camera
// ids it may access.
func (a *App) aclState(username string) (bool, map[string]bool, error) {
	var restricted int
	if err := a.db.QueryRow(
		"SELECT acl_restricted FROM users WHERE username = ?", username,
	).Scan(&restricted); err != nil {
		return false, nil, err
	}
	if restricted == 0 {
		return false, nil, nil
	}
	rows, err := a.db.Query("SELECT camera_id FROM camera_acl WHERE username = ?", username)
	if err != nil {
		return true, nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return true, nil, err
		}
		ids[id] = true
	}
	return true, ids, rows.Err()
}

// canAccessCamera checks the role-independent camera scope. Admins bypass it.
func (a *App) canAccessCamera(u *auth.CurrentUser, camID string) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin() {
		return true
	}
	restricted, ids, err := a.aclState(u.Username)
	if err != nil {
		// Fail closed: an ACL read error must not widen access.
		return false
	}
	if !restricted {
		return true
	}
	return ids[camID]
}

// filterCameras returns only the cameras u may see. It is used by the camera
// list and the status snapshot so a restricted account cannot infer camera
// names/state from them.
func (a *App) filterCameras(u *auth.CurrentUser, cams []camera.Camera) []camera.Camera {
	if u == nil {
		return nil
	}
	if u.IsAdmin() {
		return cams
	}
	restricted, ids, err := a.aclState(u.Username)
	if err != nil || !restricted {
		if err != nil {
			return nil // fail closed
		}
		return cams
	}
	out := make([]camera.Camera, 0, len(cams))
	for _, c := range cams {
		if ids[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// requireCameraAccess writes 404 (not 403) when a camera exists but is outside
// the principal's ACL, so the response does not reveal camera existence.
func (a *App) requireCameraAccess(w http.ResponseWriter, u *auth.CurrentUser, camID string) bool {
	if a.canAccessCamera(u, camID) {
		return true
	}
	httpError(w, http.StatusNotFound, "Camera not found")
	return false
}

func (a *App) cameraACLView(username string) (cameraACLView, error) {
	restricted, ids, err := a.aclState(username)
	if err != nil {
		return cameraACLView{}, err
	}
	view := cameraACLView{Username: username, Restricted: restricted, CameraIDs: []string{}}
	if restricted {
		for id := range ids {
			view.CameraIDs = append(view.CameraIDs, id)
		}
		sort.Strings(view.CameraIDs)
	}
	return view, nil
}

func (a *App) handleGetUserCameras(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if a.requireHumanAdmin(w, r) == nil {
		return
	}
	var exists int
	if err := a.db.QueryRow("SELECT 1 FROM users WHERE username = ?", username).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpError(w, http.StatusNotFound, "User not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view, err := a.cameraACLView(username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *App) handlePutUserCameras(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if a.requireHumanAdmin(w, r) == nil {
		return
	}
	var exists int
	if err := a.db.QueryRow("SELECT 1 FROM users WHERE username = ?", username).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpError(w, http.StatusNotFound, "User not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req cameraACLRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := make([]string, 0, len(req.CameraIDs))
	seen := make(map[string]bool, len(req.CameraIDs))
	if req.Restricted {
		for _, id := range req.CameraIDs {
			if seen[id] {
				continue
			}
			if _, ok := a.getCamera(id); !ok {
				httpError(w, http.StatusUnprocessableEntity, "Unknown camera: "+id)
				return
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	flag := 0
	if req.Restricted {
		flag = 1
	}
	if _, err := tx.Exec("UPDATE users SET acl_restricted = ? WHERE username = ?", flag, username); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec("DELETE FROM camera_acl WHERE username = ?", username); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, id := range ids {
		if _, err := tx.Exec(
			"INSERT INTO camera_acl (username, camera_id) VALUES (?, ?)", username, id); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view, err := a.cameraACLView(username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}
