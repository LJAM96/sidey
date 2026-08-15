package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"sidey/internal/artifacts"
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

	// 1. Maintain accounts.json multi-account credential store
	accountsJSONPaths := []string{
		"/var/lib/sidey/signing-worker/accounts.json",
		"/run/sidey/accounts.json",
	}
	for _, p := range accountsJSONPaths {
		accounts := make(map[string]string)
		if data, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(data, &accounts)
		}
		accounts[req.AppleID] = req.ApplePassword
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		if data, err := json.MarshalIndent(accounts, "", "  "); err == nil {
			_ = os.WriteFile(p, data, 0o644)
		}
	}

	// 2. Also write legacy/default credentials file
	credsContent := fmt.Sprintf("SIDEY_APPLE_ID=%s\nSIDEY_APPLE_MAIN_PASSWORD=%s\n", req.AppleID, req.ApplePassword)
	paths := []string{
		"/var/lib/sidey/signing-worker/credentials",
		"/run/secrets/apple_credentials",
		"/home/ubuntu/git/sidey-server/deploy/secrets/apple_credentials",
	}
	for _, p := range paths {
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.WriteFile(p, []byte(credsContent), 0o600)
	}

	// 3. If 2FA code provided, write it for signonly
	if req.TwoFactorCode != "" {
		codePaths := []string{
			"/tmp/opencode/2fa-code.txt",
			"/var/lib/sidey/signing-worker/2fa-code.txt",
			"/run/sidey/signing-state/2fa-code.txt",
		}
		for _, cp := range codePaths {
			if dir := filepath.Dir(cp); dir != "" {
				_ = os.MkdirAll(dir, 0o755)
			}
			_ = os.WriteFile(cp, []byte(req.TwoFactorCode+"\n"), 0o644)
		}
	}

	// 4. Upsert apple_accounts record by label
	var existingID uuid.UUID
	err := s.pool.QueryRow(r.Context(), `SELECT id FROM apple_accounts WHERE label = $1`, req.AppleID).Scan(&existingID)
	if err == nil {
		_, err = s.pool.Exec(r.Context(), `UPDATE apple_accounts SET auth_state = 'authenticating', updated_at = now() WHERE id = $1`, existingID)
	} else {
		_, err = s.pool.Exec(r.Context(), `INSERT INTO apple_accounts (label, auth_state, updated_at) VALUES ($1, 'authenticating', now())`, req.AppleID)
	}
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

// handleDeleteAppleAccount removes an Apple account and cleans up credentials.
func (s *Server) handleDeleteAppleAccount(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid account id"})
		return
	}

	var label string
	err = s.pool.QueryRow(r.Context(), `SELECT label FROM apple_accounts WHERE id = $1`, id).Scan(&label)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}

	// Delete from DB
	_, err = s.pool.Exec(r.Context(), `DELETE FROM apple_accounts WHERE id = $1`, id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "delete account failed")
		return
	}

	// Remove from accounts.json
	accountsJSONPaths := []string{
		"/var/lib/sidey/signing-worker/accounts.json",
		"/run/sidey/accounts.json",
	}
	for _, accountsJSONPath := range accountsJSONPaths {
		accounts := make(map[string]string)
		if data, err := os.ReadFile(accountsJSONPath); err == nil {
			if err := json.Unmarshal(data, &accounts); err == nil {
				delete(accounts, label)
				if out, err := json.MarshalIndent(accounts, "", "  "); err == nil {
					_ = os.WriteFile(accountsJSONPath, out, 0o644)
				}
			}
		}
	}

	s.audit.Record(r.Context(), "admin", "apple_account.deleted", audit.WithData(map[string]any{
		"account_id": id,
		"apple_id":   label,
	}))

	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id.String(), "apple_id": label})
}

type adminDeployRequest struct {
	ArtifactID  uuid.UUID `json:"artifact_id"`
	DeviceID    uuid.UUID `json:"device_id"`
	AutoApprove bool      `json:"auto_approve"`
	AppleID     string    `json:"apple_id"`
}

// handleAdminDeploy approves an artifact, routes to an available Apple account,
// and enqueues a sign job for the target device.
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

	// 3. Select Apple Account (explicit or auto-balance by available slots)
	appleID := strings.TrimSpace(req.AppleID)
	if appleID == "" || appleID == "auto" {
		var chosenLabel string
		err := s.pool.QueryRow(r.Context(), `
			SELECT label FROM apple_accounts
			WHERE auth_state IN ('authenticated', 'authenticating')
			ORDER BY registered_app_id_count ASC, last_auth_at DESC
			LIMIT 1`).Scan(&chosenLabel)
		if err == nil && chosenLabel != "" {
			appleID = chosenLabel
		}
	}

	// 4. Enqueue sign job
	params := map[string]any{
		"artifact_id":  req.ArtifactID.String(),
		"udid":         udid,
		"device_name":  deviceName,
		"device_type":  platform,
		"machine_name": "isideload-minimal",
		"apple_id":     appleID,
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
		"apple_id":    appleID,
		"sign_job_id": signJobID,
	}))

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":      "ok",
		"mode":        "native",
		"artifact_id": req.ArtifactID,
		"device_id":   req.DeviceID,
		"apple_id":    appleID,
		"sign_job_id": signJobID,
	})
}

type liveContainerInstallRequest struct {
	DeviceID uuid.UUID `json:"device_id"`
	AppleID  string    `json:"apple_id"`
}

// handleInstallLiveContainer creates/prepares the LiveContainer package and enqueues installation.
func (s *Server) handleInstallLiveContainer(w http.ResponseWriter, r *http.Request) {
	var req liveContainerInstallRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device_id is required"})
		return
	}

	// 1. Verify target device exists
	var udid, deviceName, platform string
	err := s.pool.QueryRow(r.Context(), `
		SELECT udid, COALESCE(device_name, ''), platform
		FROM devices WHERE id = $1`, req.DeviceID).Scan(&udid, &deviceName, &platform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device not found"})
		return
	}

	// 2. Resolve or download LiveContainer IPA
	var artifactID uuid.UUID
	var sha256Hex string
	err = s.pool.QueryRow(r.Context(), `
		SELECT id, sha256 FROM artifacts
		WHERE (bundle_identifier = 'com.kdt.livecontainer' OR filename ILIKE '%livecontainer%')
		  AND quarantine_state = 'approved' AND sha256 NOT LIKE 'livecontainer-%'
		ORDER BY created_at DESC LIMIT 1`).Scan(&artifactID, &sha256Hex)

	if err != nil {
		// Download fresh LiveContainer.ipa from GitHub
		client := &http.Client{Timeout: 90 * time.Second}
		dlURL := "https://github.com/khanhduytran0/LiveContainer/releases/latest/download/LiveContainer.ipa"
		dlReq, dlErr := http.NewRequest("GET", dlURL, nil)
		if dlErr != nil {
			s.writeError(w, http.StatusInternalServerError, "failed to create LiveContainer download request")
			return
		}
		dlReq.Header.Set("User-Agent", "Sidey-ControlPlane/1.0")
		dlResp, dlRespErr := client.Do(dlReq)
		if dlRespErr != nil || dlResp.StatusCode != http.StatusOK {
			s.writeError(w, http.StatusBadGateway, "failed to download LiveContainer IPA from GitHub")
			return
		}
		defer dlResp.Body.Close()

		shaHex, tmp, saveErr := s.artifacts.SaveToTemp(dlResp.Body)
		if saveErr != nil {
			s.writeError(w, http.StatusInternalServerError, "saving LiveContainer download failed")
			return
		}
		defer s.artifacts.DiscardTemp(tmp)

		meta, metaErr := artifacts.Inspect(tmp)
		if metaErr != nil {
			meta = &artifacts.Metadata{
				BundleIdentifier: "com.kdt.livecontainer",
				Version:          "3.8.0",
				Platform:         "iPhoneOS",
			}
		}

		_, pubErr := s.artifacts.Publish(shaHex, tmp)
		if pubErr != nil {
			s.writeError(w, http.StatusInternalServerError, "publishing LiveContainer artifact failed")
			return
		}

		err = s.pool.QueryRow(r.Context(), `
			INSERT INTO artifacts (sha256, filename, bundle_identifier, version, platform, quarantine_state, state_changed_at)
			VALUES ($1, 'LiveContainer.ipa', $2, $3, $4, 'approved', now())
			ON CONFLICT (sha256) DO UPDATE SET quarantine_state = 'approved', state_changed_at = now()
			RETURNING id`,
			shaHex, meta.BundleIdentifier, meta.Version, meta.Platform).Scan(&artifactID)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "saving LiveContainer artifact record failed")
			return
		}
	}

	// 3. Resolve Apple Account
	appleID := strings.TrimSpace(req.AppleID)
	if appleID == "" || appleID == "auto" {
		var chosenLabel string
		err := s.pool.QueryRow(r.Context(), `
			SELECT label FROM apple_accounts
			WHERE auth_state IN ('authenticated', 'authenticating')
			ORDER BY registered_app_id_count ASC, last_auth_at DESC
			LIMIT 1`).Scan(&chosenLabel)
		if err == nil && chosenLabel != "" {
			appleID = chosenLabel
		}
	}

	params := map[string]any{
		"artifact_id":  artifactID.String(),
		"udid":         udid,
		"device_name":  deviceName,
		"device_type":  platform,
		"machine_name": "isideload-minimal",
		"apple_id":     appleID,
		"is_container": true,
	}
	var signJobID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO jobs (job_type, device_id, parameters, max_attempts)
		VALUES ('sign', $1, $2, 5)
		RETURNING id`, req.DeviceID, params).Scan(&signJobID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "enqueuing LiveContainer install job failed")
		return
	}

	s.audit.Record(r.Context(), "admin", "livecontainer.install_queued", audit.WithData(map[string]any{
		"device_id":   req.DeviceID,
		"sign_job_id": signJobID,
	}))

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":      "ok",
		"artifact_id": artifactID.String(),
		"device_id":   req.DeviceID.String(),
		"sign_job_id": signJobID.String(),
		"message":     "LiveContainer installation queued for device",
	})
}

type liveContainerPushRequest struct {
	ArtifactID uuid.UUID `json:"artifact_id"`
	DeviceID   uuid.UUID `json:"device_id"`
	BundleID   string    `json:"bundle_id"`
}

// handleLiveContainerPush pushes a guest IPA directly into LiveContainer on device.
func (s *Server) handleLiveContainerPush(w http.ResponseWriter, r *http.Request) {
	var req liveContainerPushRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.ArtifactID == uuid.Nil || req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "artifact_id and device_id are required"})
		return
	}
	bundleID := strings.TrimSpace(req.BundleID)
	if bundleID == "" {
		bundleID = "com.kdt.livecontainer"
	}

	var udid, deviceName, platform string
	err := s.pool.QueryRow(r.Context(), `
		SELECT udid, COALESCE(device_name, ''), platform
		FROM devices WHERE id = $1`, req.DeviceID).Scan(&udid, &deviceName, &platform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device not found"})
		return
	}

	_, _ = s.pool.Exec(r.Context(), `
		UPDATE artifacts SET quarantine_state = 'approved', state_changed_at = now()
		WHERE id = $1`, req.ArtifactID)

	params := map[string]any{
		"artifact_id": req.ArtifactID.String(),
		"udid":        udid,
		"device_name": deviceName,
		"device_type": platform,
		"target":      "livecontainer",
		"bundle_id":   bundleID,
	}
	var jobID uuid.UUID
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO jobs (job_type, device_id, parameters, max_attempts)
		VALUES ('livecontainer_push', $1, $2, 3)
		RETURNING id`, req.DeviceID, params).Scan(&jobID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "enqueuing LiveContainer push job failed")
		return
	}

	s.audit.Record(r.Context(), "admin", "livecontainer.push_queued", audit.WithData(map[string]any{
		"artifact_id": req.ArtifactID,
		"device_id":   req.DeviceID,
		"job_id":      jobID,
	}))

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":      "ok",
		"mode":        "livecontainer",
		"artifact_id": req.ArtifactID.String(),
		"device_id":   req.DeviceID.String(),
		"job_id":      jobID.String(),
		"message":     "Guest IPA queued for wireless LiveContainer HouseArrest transfer",
	})
}
