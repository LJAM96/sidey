package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"sidey/internal/audit"
	"sidey/internal/auth"
)

// Valid agent roles. The role an agent receives is decided server side: it is
// bound to the admin-issued enrolment token, never taken from capability data
// supplied by the enrolling client. A compromised agent can therefore only
// claim the authority its operator granted the token.
var validAgentRoles = map[string]bool{
	"device_agent":   true,
	"refresh_agent":  true,
	"signing_worker": true,
	"tvos_agent":     true,
	// device_service is the ADR-0008 same-host/remote-node device service:
	// a public, non-secret sentinel identity on the same host, and a normal
	// enrolled role for remote nodes.
	"device_service": true,
}

const defaultAgentRole = "device_agent"

// validRoleList renders the allowed roles for error messages.
func validRoleList() string {
	roles := make([]string, 0, len(validAgentRoles))
	for role := range validAgentRoles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return strings.Join(roles, ", ")
}

type createEnrolmentTokenRequest struct {
	Label            string `json:"label"`
	Role             string `json:"role"`
	ExpiresInSeconds *int   `json:"expires_in_seconds"`
}

// handleCreateEnrolmentToken issues an enrolment token bound to a server
// controlled role. The plaintext token is returned exactly once; only its
// hash (and a public key id derived from the sha256) are stored.
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
	role := req.Role
	if role == "" {
		role = defaultAgentRole
	}
	if !validAgentRoles[role] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "role must be one of " + validRoleList()})
		return
	}
	secret, err := auth.GenerateSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token generation failed")
		return
	}
	hash, err := auth.HashSecret(secret)
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
		INSERT INTO agent_enrolment_tokens (token_hash, token_key, label, role, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		hash, auth.KeyID(secret), req.Label, role, "admin", expiresAt)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token storage failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "enrolment_token.created",
		audit.WithData(map[string]any{"label": req.Label, "role": role}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": auth.FormatEnrolmentToken(secret), "label": req.Label, "role": role})
}

type enrolAgentRequest struct {
	Name            string         `json:"name"`
	Architecture    string         `json:"architecture"`
	OperatingSystem string         `json:"operating_system"`
	SoftwareVersion string         `json:"software_version"`
	TailnetIdentity string         `json:"tailnet_identity"`
	Capabilities    map[string]any `json:"capabilities"`
}

// handleEnrolAgent consumes a one time token and creates an agent with a
// fresh API key. The API key and the agent's role are returned exactly once.
//
// Consumption is atomic and shielded from bcrypt amplification: the token
// carries a public key id that locates a single candidate row (indexed), the
// row is locked FOR UPDATE inside the transaction, exactly one bcrypt
// verification runs, and the consume update requires an unused row
// (AND used_at IS NULL) with an affected-row-count of one. Two concurrent
// enrolment requests with the same token therefore cannot both succeed, and
// N outstanding tokens cannot turn one unauthenticated request into N
// bcrypts. The agent's role is taken from the token, not the request body.
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

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback(r.Context())

	var (
		tokenID    uuid.UUID
		storedHash string
		role       string
		expiresAt  *time.Time
		usedAt     *time.Time
	)
	keyID, secret, keyed := auth.ParseEnrolmentToken(token)
	if keyed {
		// New format: one indexed candidate row, locked for the rest of the
		// transaction so a concurrent request cannot observe it unused.
		err := tx.QueryRow(r.Context(), `
			SELECT id, token_hash, role, expires_at, used_at
			FROM agent_enrolment_tokens
			WHERE token_key = $1
			FOR UPDATE`, keyID).Scan(&tokenID, &storedHash, &role, &expiresAt, &usedAt)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired enrolment token"})
			return
		}
		if usedAt != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enrolment token already used"})
			return
		}
		if expiresAt != nil && expiresAt.Before(time.Now()) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired enrolment token"})
			return
		}
		if !auth.VerifySecret(secret, storedHash) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired enrolment token"})
			return
		}
	} else {
		// Legacy tokens (created before token_key existed) carry no public id
		// and can only be located by scanning their hashes. The scan is
		// bounded to legacy rows only and runs one bcrypt per outstanding
		// legacy token; issuing new tokens via the format above removes the
		// amplification path. Grant the default role.
		role = defaultAgentRole
		var found bool
		rows, err := tx.Query(r.Context(), `
			SELECT id, token_hash, expires_at, used_at
			FROM agent_enrolment_tokens
			WHERE token_key IS NULL AND used_at IS NULL
			FOR UPDATE`)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "token lookup failed")
			return
		}
		for rows.Next() {
			var id uuid.UUID
			var hash string
			var exp *time.Time
			var used *time.Time
			if err := rows.Scan(&id, &hash, &exp, &used); err != nil {
				rows.Close()
				s.writeError(w, http.StatusInternalServerError, "token lookup failed")
				return
			}
			if exp != nil && exp.Before(time.Now()) {
				continue
			}
			if auth.VerifySecret(token, hash) {
				tokenID, storedHash = id, hash
				expiresAt, usedAt = exp, used
				found = true
				break
			}
		}
		rows.Close()
		if !found {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or expired enrolment token"})
			return
		}
		if usedAt != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enrolment token already used"})
			return
		}
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

	var agentID uuid.UUID
	capabilities := req.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	err = tx.QueryRow(r.Context(), `
		INSERT INTO agents (name, architecture, operating_system, software_version,
			tailnet_identity, connection_state, last_heartbeat_at, capabilities, role,
			api_key_hash, api_key_id)
		VALUES ($1, $2, $3, $4, $5, 'online', now(), $6, $7, $8, $9)
		RETURNING id`,
		req.Name, req.Architecture, req.OperatingSystem, req.SoftwareVersion,
		req.TailnetIdentity, capabilities, role, apiKeyHash, auth.KeyID(apiKey)).Scan(&agentID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "agent creation failed")
		return
	}
	// Conditional consume: only an unused token row may be marked used. If
	// a concurrent request already consumed it, the affected row count is
	// zero and the whole transaction (agent creation included) rolls back.
	res, err := tx.Exec(r.Context(), `
		UPDATE agent_enrolment_tokens
		SET used_at = now(), used_by_agent = $1
		WHERE id = $2 AND used_at IS NULL`,
		agentID, tokenID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "token consumption failed")
		return
	}
	if res.RowsAffected() != 1 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrolment token already used"})
		return
	}
	if err := audit.RecordTx(r.Context(), tx, "admin", "agent.enrolled",
		audit.WithData(map[string]any{"agent_id": agentID, "name": req.Name, "role": role})); err != nil {
		s.writeError(w, http.StatusInternalServerError, "audit write failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": agentID,
		"api_key":  apiKey,
		"role":     role,
	})
}

// agentRole returns the server controlled role for an agent. Unknown agents
// fall back to the default (device-scoped) role so a delete-race cannot
// accidentally elevate an attacker.
func (s *Server) agentRole(r *http.Request, agentID uuid.UUID) string {
	var role, name string
	err := s.pool.QueryRow(r.Context(),
		`SELECT role, name FROM agents WHERE id = $1`, agentID).Scan(&role, &name)
	if err == nil {
		if name == "signing-worker" && role != "signing_worker" {
			_, _ = s.pool.Exec(r.Context(), `UPDATE agents SET role = 'signing_worker' WHERE id = $1`, agentID)
			return "signing_worker"
		}
		if role != "" {
			return role
		}
	}
	return defaultAgentRole
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
		          last_heartbeat_at, role`,
		capabilities, agentID)
	var (
		id          uuid.UUID
		name        string
		software    *string
		tailnet     *string
		state       string
		heartbeatAt time.Time
		role        string
	)
	if err := row.Scan(&id, &name, &software, &tailnet, &state, &heartbeatAt, &role); err != nil {
		s.writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id": id, "name": name, "software_version": software,
			"tailnet_identity": tailnet, "connection_state": state,
			"last_heartbeat_at": heartbeatAt, "role": role,
		},
		"server_time": time.Now(),
	})
}

type reportedDevice struct {
	Udid                 string `json:"udid"`
	Platform             string `json:"platform"`
	DeviceName           string `json:"device_name"`
	Model                string `json:"model"`
	OsVersion            string `json:"os_version"`
	PairingStatus        string `json:"pairing_status"`
	DeveloperModeEnabled *bool  `json:"developer_mode_enabled"`
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
	type reported struct {
		Udid string    `json:"udid"`
		ID   uuid.UUID `json:"id"`
	}
	ids := make([]reported, 0, len(req.Devices))
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
		res, err := s.pool.Query(r.Context(), `
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
				last_connected_at = now()
			RETURNING id`,
			d.Udid, platform, d.DeviceName, d.Model, d.OsVersion,
			agentID, pairingStatus, d.DeveloperModeEnabled)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "device upsert failed")
			return
		}
		var id uuid.UUID
		if res.Next() {
			if err := res.Scan(&id); err != nil {
				res.Close()
				s.writeError(w, http.StatusInternalServerError, "device upsert failed")
				return
			}
			ids = append(ids, reported{Udid: d.Udid, ID: id})
		}
		res.Close()
		upserted++
	}
	s.audit.Record(r.Context(), "agent:"+agentID.String(), "devices.reported")
	writeJSON(w, http.StatusOK, map[string]any{"reported": len(req.Devices), "upserted": upserted, "devices": ids})
}
