package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"sidey/internal/audit"
)

type updateAppleCredentialsRequest struct {
	AppleID       string `json:"apple_id"`
	ApplePassword string `json:"apple_password"`
	TwoFactorCode string `json:"two_factor_code"`
}

// handleUpdateAppleCredentials saves Apple credentials for the signing worker
// and updates the apple_accounts state.
func (s *Server) handleUpdateAppleCredentials(w http.ResponseWriter, r *http.Request) {
	var req updateAppleCredentialsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.AppleID = strings.TrimSpace(req.AppleID)
	req.ApplePassword = strings.TrimSpace(req.ApplePassword)
	req.TwoFactorCode = strings.TrimSpace(req.TwoFactorCode)

	if req.AppleID == "" || req.ApplePassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "apple_id and apple_password are required"})
		return
	}

	// 1. Write credentials file for signing worker
	credsContent := fmt.Sprintf("SIDEY_APPLE_ID=%s\nSIDEY_APPLE_MAIN_PASSWORD=%s\n", req.AppleID, req.ApplePassword)
	paths := []string{
		"/var/lib/sidey/signing-worker/credentials",
		"/run/secrets/apple_credentials",
		"/home/ubuntu/git/sidey-server/deploy/secrets/apple_credentials",
	}
	for _, p := range paths {
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0o700)
		}
		_ = os.WriteFile(p, []byte(credsContent), 0o600)
	}

	// 2. If 2FA code provided, write it for signonly
	if req.TwoFactorCode != "" {
		codePaths := []string{
			"/tmp/opencode/2fa-code.txt",
			"/var/lib/sidey/signing-worker/2fa-code.txt",
		}
		for _, cp := range codePaths {
			if dir := filepath.Dir(cp); dir != "" {
				_ = os.MkdirAll(dir, 0o755)
			}
			_ = os.WriteFile(cp, []byte(req.TwoFactorCode+"\n"), 0o644)
		}
	}

	// 3. Upsert apple_accounts table
	_, err := s.pool.Exec(r.Context(), `
		INSERT INTO apple_accounts (label, team_identifier, auth_state, updated_at)
		VALUES ($1, '', 'authenticating', now())
		ON CONFLICT (label) DO UPDATE
		SET updated_at = now()`, req.AppleID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "saving account record failed")
		return
	}

	s.audit.Record(r.Context(), "admin", "apple_account.updated", audit.WithData(map[string]any{
		"apple_id": req.AppleID,
		"has_2fa":  req.TwoFactorCode != "",
	}))

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"apple_id": req.AppleID,
		"message":  "credentials saved; signing worker will authenticate on next sign job",
	})
}

type adminDeployRequest struct {
	ArtifactID  uuid.UUID `json:"artifact_id"`
	DeviceID    uuid.UUID `json:"device_id"`
	AutoApprove bool      `json:"auto_approve"`
}

// handleAdminDeploy approves an artifact and enqueues a sign job for the target device.
func (s *Server) handleAdminDeploy(w http.ResponseWriter, r *http.Request) {
	var req adminDeployRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.ArtifactID == uuid.Nil || req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "artifact_id and device_id are required"})
		return
	}

	// 1. Auto-approve artifact
	_, err := s.pool.Exec(r.Context(), `
		UPDATE artifacts
		SET quarantine_state = 'approved', state_changed_at = now()
		WHERE id = $1`, req.ArtifactID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "approving artifact failed")
		return
	}

	// 2. Verify target device exists
	var udid, deviceName, platform string
	err = s.pool.QueryRow(r.Context(), `
		SELECT udid, COALESCE(device_name, ''), platform
		FROM devices WHERE id = $1`, req.DeviceID).Scan(&udid, &deviceName, &platform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device not found"})
		return
	}

	// 3. Enqueue sign job
	params := map[string]any{
		"artifact_id":  req.ArtifactID.String(),
		"udid":         udid,
		"device_name":  deviceName,
		"device_type":  platform,
		"machine_name": "isideload-minimal",
	}
	var signJobID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO jobs (job_type, device_id, parameters, max_attempts)
		VALUES ('sign', $1, $2, 5)
		RETURNING id`, req.DeviceID, params).Scan(&signJobID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "enqueuing sign job failed")
		return
	}

	s.audit.Record(r.Context(), "admin", "deploy.started", audit.WithData(map[string]any{
		"artifact_id": req.ArtifactID,
		"device_id":   req.DeviceID,
		"sign_job_id": signJobID,
	}))

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":      "ok",
		"artifact_id": req.ArtifactID,
		"device_id":   req.DeviceID,
		"sign_job_id": signJobID,
	})
}
