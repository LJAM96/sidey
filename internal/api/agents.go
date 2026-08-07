package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"sidey/internal/audit"
	"sidey/internal/auth"
)

type createEnrolmentTokenRequest struct {
	Label             string `json:"label"`
	ExpiresInSeconds  *int   `json:"expires_in_seconds"`
}

// handleCreateEnrolmentToken issues a one time enrolment token. The plaintext
// token is returned exactly once; only its hash is stored.
func (s *Server) handleCreateEnrolmentToken(w http.ResponseWriter, r *http.Request) {
	var req createEnrolmentTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "label is required"})
		return
	}
	token, err := auth.GenerateSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token generation failed")
		return
	}
	hash, err := auth.HashSecret(token)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token hashing failed")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresInSeconds != nil && *req.ExpiresInSeconds > 0 {
		t := time.Now().Add(time.Duration(*req.ExpiresInSeconds) * time.Second)
		expiresAt = &t
	}
	_, err = s.pool.Exec(r.Context(), `
		INSERT INTO agent_enrolment_tokens (token_hash, label, created_by, expires_at)
		VALUES ($1, $2, $3, $4)`,
		hash, req.Label, "admin", expiresAt)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token storage failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "enrolment_token.created")
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "label": req.Label})
}

type enrolAgentRequest struct {
	Name             string          `json:"name"`
	Architecture     string          `json:"architecture"`
	OperatingSystem  string          `json:"operating_system"`
	SoftwareVersion  string          `json:"software_version"`
	TailnetIdentity  string          `json:"tailnet_identity"`
	Capabilities     map[string]any  `json:"capabilities"`
}

// handleEnrolAgent consumes a one time token and creates an agent with a
// fresh API key. The API key is returned exactly once.
func (s *Server) handleEnrolAgent(w http.ResponseWriter, r *http.Request) {
	var req enrolAgentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	token := enrolmentSecret(r.Context())

	// bcrypt hashes are salted, so the incoming token must be verified
	// against stored hashes rather than re-hashed.
	var tokenID *uuid.UUID
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, token_hash, expires_at
		FROM agent_enrolment_tokens WHERE used_at IS NULL`)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token lookup failed")
		return
	}
	for rows.Next() {
		var id uuid.UUID
		var storedHash string
		var expiresAt *time.Time
		if err := rows.Scan(&id, &storedHash, &expiresAt); err != nil {
			rows.Close()
			s.writeError(w, http.StatusInternalServerError, "token lookup failed")
			return
		}
		if expiresAt != nil && expiresAt.Before(time.Now()) {
			continue
		}
		if auth.VerifySecret(token, storedHash) {
			t := id
			tokenID = &t
			break
		}
	}
	rows.Close()
	if tokenID == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired enrolment token"})
		return
	}

	apiKey, err := auth.GenerateSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	apiKeyHash, err := auth.HashSecret(apiKey)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "key hashing failed")
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback(r.Context())

	var agentID uuid.UUID
	capabilities := req.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	err = tx.QueryRow(r.Context(), `
		INSERT INTO agents (name, architecture, operating_system, software_version,
			tailnet_identity, connection_state, last_heartbeat_at, capabilities, api_key_hash)
		VALUES ($1, $2, $3, $4, $5, 'online', now(), $6, $7)
		RETURNING id`,
		req.Name, req.Architecture, req.OperatingSystem, req.SoftwareVersion,
		req.TailnetIdentity, capabilities, apiKeyHash).Scan(&agentID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "agent creation failed")
		return
	}
	if _, err := tx.Exec(r.Context(), `
		UPDATE agent_enrolment_tokens SET used_at = now(), used_by_agent = $1 WHERE id = $2`,
		agentID, *tokenID); err != nil {
		s.writeError(w, http.StatusInternalServerError, "token consumption failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "agent.enrolled",
		audit.WithData(map[string]any{"agent_id": agentID, "name": req.Name}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": agentID,
		"api_key":  apiKey,
	})
}

type heartbeatRequest struct {
	Capabilities map[string]any `json:"capabilities"`
}

// handleHeartbeat updates the agent's connection state and capability report.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	agentID := agentID(r.Context())
	capabilities := req.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	row := s.pool.QueryRow(r.Context(), `
		UPDATE agents
		SET connection_state = 'online', last_heartbeat_at = now(),
		    capabilities = $1, updated_at = now()
		WHERE id = $2
		RETURNING id, name, software_version, tailnet_identity, connection_state,
		          last_heartbeat_at`,
		capabilities, agentID)
	var (
		id            uuid.UUID
		name          string
		software      *string
		tailnet       *string
		state         string
		heartbeatAt   time.Time
	)
	if err := row.Scan(&id, &name, &software, &tailnet, &state, &heartbeatAt); err != nil {
		s.writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id": id, "name": name, "software_version": software,
			"tailnet_identity": tailnet, "connection_state": state,
			"last_heartbeat_at": heartbeatAt,
		},
		"server_time": time.Now(),
	})
}

type reportedDevice struct {
	Udid                  string `json:"udid"`
	Platform              string `json:"platform"`
	DeviceName            string `json:"device_name"`
	Model                 string `json:"model"`
	OsVersion             string `json:"os_version"`
	PairingStatus         string `json:"pairing_status"`
	DeveloperModeEnabled  *bool  `json:"developer_mode_enabled"`
}

// handleReportDevices upserts the devices currently visible to the agent.
func (s *Server) handleReportDevices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Devices []reportedDevice `json:"devices"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	agentID := agentID(r.Context())
	upserted := 0
	for _, d := range req.Devices {
		if d.Udid == "" {
			continue
		}
		platform := d.Platform
		if platform == "" {
			platform = "ios"
		}
		pairingStatus := d.PairingStatus
		if pairingStatus == "" {
			pairingStatus = "unknown"
		}
		res, err := s.pool.Exec(r.Context(), `
			INSERT INTO devices (udid, platform, device_name, model, os_version,
				agent_id, pairing_status, developer_mode_enabled, last_connected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
			ON CONFLICT (udid) DO UPDATE SET
				platform = EXCLUDED.platform,
				device_name = EXCLUDED.device_name,
				model = EXCLUDED.model,
				os_version = EXCLUDED.os_version,
				agent_id = CASE WHEN devices.agent_id IS NULL OR devices.agent_id = $6
					THEN $6 ELSE devices.agent_id END,
				pairing_status = EXCLUDED.pairing_status,
				developer_mode_enabled = EXCLUDED.developer_mode_enabled,
				last_connected_at = now()`,
			d.Udid, platform, d.DeviceName, d.Model, d.OsVersion,
			agentID, pairingStatus, d.DeveloperModeEnabled)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "device upsert failed")
			return
		}
		upserted += int(res.RowsAffected())
	}
	s.audit.Record(r.Context(), "agent:"+agentID.String(), "devices.reported")
	writeJSON(w, http.StatusOK, map[string]any{"reported": len(req.Devices), "upserted": upserted})
}
