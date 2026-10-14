package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"sidey/internal/auth"
	"sidey/internal/audit"
)

// Device API tokens (Phase 0, mobile companion surface).
//
// Long-lived, independently revocable bearer credentials with narrow scopes,
// issued per device/app from the dashboard pairing flow. Unlike the admin
// key they never grant account, certificate or deployment management —
// route wiring (api.go) decides which handlers accept which scope:
//
//	read     metadata: devices, jobs, refresh calendar, updates, catalog
//	control  read + refresh triggers, imports, approvals, 2FA, pushes
//
// Secrets are bcrypt hashes behind a sha256 key id (same scheme as agent
// keys); only the key id is indexed, never the secret.

// tokenHasScope reports whether a token's scopes satisfy a required scope.
// "control" implies "read".
func tokenHasScope(have []string, need string) bool {
	for _, s := range have {
		if s == need {
			return true
		}
		if need == "read" && s == "control" {
			return true
		}
	}
	return false
}

// checkDeviceToken validates a presented device token against one required
// scope. Returns the token id for audit on success.
func (s *Server) checkDeviceToken(ctx context.Context, secret, scope string) (uuid.UUID, bool) {
	var (
		id       uuid.UUID
		hash     string
		scopes   []string
		revoked  *time.Time
		expires  *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT id, token_hash, scopes, revoked_at, expires_at
		FROM device_api_tokens WHERE key_id = $1`, auth.KeyID(secret)).
		Scan(&id, &hash, &scopes, &revoked, &expires); err != nil {
		return uuid.Nil, false
	}
	if revoked != nil {
		return uuid.Nil, false
	}
	if expires != nil && !time.Now().Before(*expires) {
		return uuid.Nil, false
	}
	if !auth.VerifySecret(secret, hash) {
		return uuid.Nil, false
	}
	if !tokenHasScope(scopes, scope) {
		return uuid.Nil, false
	}
	_, _ = s.pool.Exec(ctx, `UPDATE device_api_tokens SET last_used_at = now() WHERE id = $1`, id)
	return id, true
}

var validTokenScopes = map[string]bool{"read": true, "control": true}

// handleCreateDeviceToken mints a device token. The plaintext secret is
// returned exactly once (dashboard pairing flow shows it as QR); only the
// bcrypt hash is stored.
func (s *Server) handleCreateDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string   `json:"name"`
		Scopes       []string `json:"scopes"`
		DeviceID     *uuid.UUID `json:"device_id"`
		ExpiresDays  *int     `json:"expires_days"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"read"}
	}
	for _, sc := range scopes {
		if !validTokenScopes[sc] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown scope: " + sc})
			return
		}
	}
	var expires *time.Time
	if req.ExpiresDays != nil && *req.ExpiresDays > 0 {
		t := time.Now().Add(time.Duration(*req.ExpiresDays) * 24 * time.Hour)
		expires = &t
	}
	id, secret, created, err := s.mintDeviceTokenRow(r.Context(), req.Name, scopes, req.DeviceID, expires)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token save failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "device_token.created", audit.WithData(map[string]any{
		"token_id": id.String(), "name": req.Name, "scopes": scopes,
	}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id.String(), "name": req.Name, "scopes": scopes,
		"token": secret, "created_at": created,
		"note": "token is shown once; store it in the iOS Keychain",
	})
}

// handleListDeviceTokens lists tokens without secrets.
func (s *Server) handleListDeviceTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, scopes, device_id, expires_at, revoked_at, last_used_at, created_at
		FROM device_api_tokens ORDER BY created_at`)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "tokens lookup failed")
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var (
			id                     uuid.UUID
			name                   string
			scopes                 []string
			deviceID               *uuid.UUID
			expires, revoked       *time.Time
			lastUsed               *time.Time
			createdAt              time.Time
		)
		if err := rows.Scan(&id, &name, &scopes, &deviceID, &expires, &revoked, &lastUsed, &createdAt); err != nil {
			continue
		}
		entry := map[string]any{
			"id": id.String(), "name": name, "scopes": scopes,
			"created_at": createdAt.UTC().Format(time.RFC3339),
		}
		if deviceID != nil {
			entry["device_id"] = deviceID.String()
		}
		if expires != nil {
			entry["expires_at"] = expires.UTC().Format(time.RFC3339)
		}
		if revoked != nil {
			entry["revoked_at"] = revoked.UTC().Format(time.RFC3339)
		}
		if lastUsed != nil {
			entry["last_used_at"] = lastUsed.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleRevokeDeviceToken revokes a token immediately.
func (s *Server) handleRevokeDeviceToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid token id"})
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE device_api_tokens SET revoked_at = now() WHERE id = $1`, id); err != nil {
		s.writeError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "device_token.revoked", audit.WithData(map[string]any{
		"token_id": id.String(),
	}))
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "id": id.String()})
}

// pairingPayload is the QR/paste payload the iOS app scans: server URL plus
// a device token. Rendered by the dashboard pairing modal, consumed once.
func pairingPayload(serverURL, token string) string {
	body, _ := json.Marshal(map[string]any{"v": 1, "url": serverURL, "token": token})
	return string(body)
}
