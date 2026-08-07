package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"sidey/internal/audit"
	"sidey/internal/jobs"
)

type createSignJobRequest struct {
	ArtifactID     uuid.UUID `json:"artifact_id"`
	DeviceID       uuid.UUID `json:"device_id"`
	MachineName    string    `json:"machine_name"`
	IdempotencyKey string    `json:"idempotency_key"`
}

// handleCreateSignJob enqueues a signing job for an approved artifact and a
// device. The device must be registered with the signing team (or get
// registered by the worker during signing).
func (s *Server) handleCreateSignJob(w http.ResponseWriter, r *http.Request) {
	var req createSignJobRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.ArtifactID == uuid.Nil || req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "artifact_id and device_id are required"})
		return
	}

	var quarantineState string
	err := s.pool.QueryRow(r.Context(),
		`SELECT quarantine_state FROM artifacts WHERE id = $1`, req.ArtifactID).Scan(&quarantineState)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	if quarantineState != "approved" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "artifact must be approved before signing"})
		return
	}

	var deviceID uuid.UUID
	err = s.pool.QueryRow(r.Context(),
		`SELECT id FROM devices WHERE id = $1`, req.DeviceID).Scan(&deviceID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}

	machineName := req.MachineName
	if machineName == "" {
		machineName = "isideload-minimal"
	}
	parameters, err := json.Marshal(map[string]any{
		"artifact_id":   req.ArtifactID,
		"machine_name":  machineName,
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "parameter encoding failed")
		return
	}
	key := req.IdempotencyKey
	if key == "" {
		key = fmt.Sprintf("sign:%s:%s", req.ArtifactID, req.DeviceID)
	}
	job, err := s.jobs.Create(r.Context(), "admin", jobs.CreateRequest{
		JobType:        jobs.JobTypeSign,
		DeviceID:       &req.DeviceID,
		Parameters:     parameters,
		IdempotencyKey: key,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// handleAgentDownloadArtifact lets agents fetch the original IPA of an
// approved artifact (the signing worker needs the bytes to sign them).
func (s *Server) handleAgentDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid artifact id"})
		return
	}
	var sha256, filename, state string
	err = s.pool.QueryRow(r.Context(),
		`SELECT sha256, filename, quarantine_state FROM artifacts WHERE id = $1`, id).Scan(&sha256, &filename, &state)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	if state != "approved" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "artifact is not approved"})
		return
	}
	path := s.artifacts.Path(sha256)
	if !s.artifacts.Exists(sha256) {
		s.writeError(w, http.StatusInternalServerError, "artifact blob missing")
		return
	}
	s.audit.Record(r.Context(), "agent:"+agentID(r.Context()).String(), "artifact.downloaded",
		audit.WithData(map[string]any{"artifact_id": id, "sha256": sha256}))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

type signedArtifactUploadRequest struct {
	SourceArtifactID       uuid.UUID `json:"source_artifact_id"`
	DeviceID               uuid.UUID `json:"device_id"`
	AccountEmail           string    `json:"account_email"`
	TeamID                 string    `json:"team_id"`
	CertSerial             string    `json:"cert_serial"`
	ProfileExpiryAt        string    `json:"profile_expiry_at"`
	SignedBundleIdentifier string    `json:"signed_bundle_identifier"`
	SignedIPASha256        string    `json:"signed_ipa_sha256"`
	ProvisioningProfileRef string    `json:"provisioning_profile_ref"`
	DeviceCount            int       `json:"device_count"`
	AppIDCount             int       `json:"app_id_count"`
}

// handleUploadSignedArtifact stores the signed IPA produced by the signing
// worker and records it against the source artifact, device and signing team.
// The IPA is sent as multipart form field "ipa"; metadata travels in the same
// form.
func (s *Server) handleUploadSignedArtifact(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart form"})
		return
	}
	var req signedArtifactUploadRequest
	req.SourceArtifactID, _ = uuid.Parse(r.FormValue("source_artifact_id"))
	req.DeviceID, _ = uuid.Parse(r.FormValue("device_id"))
	req.AccountEmail = r.FormValue("account_email")
	req.TeamID = r.FormValue("team_id")
	req.CertSerial = r.FormValue("cert_serial")
	req.ProfileExpiryAt = r.FormValue("profile_expiry_at")
	req.SignedBundleIdentifier = r.FormValue("signed_bundle_identifier")
	req.SignedIPASha256 = r.FormValue("signed_ipa_sha256")
	req.ProvisioningProfileRef = r.FormValue("provisioning_profile_ref")
	fmt.Sscanf(r.FormValue("device_count"), "%d", &req.DeviceCount)
	fmt.Sscanf(r.FormValue("app_id_count"), "%d", &req.AppIDCount)

	if req.SourceArtifactID == uuid.Nil || req.DeviceID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source_artifact_id and device_id are required"})
		return
	}

	file, _, err := r.FormFile("ipa")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing ipa file"})
		return
	}
	defer file.Close()

	var preExisting bool
	if req.SignedIPASha256 != "" {
		preExisting = s.artifacts.Exists(req.SignedIPASha256)
	}
	computed, err := s.artifacts.Save(file)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing signed ipa failed")
		return
	}
	// Any failure from here on must not leave an unreferenced file behind;
	// the signed_artifacts row is the only reference to it. Only remove files
	// this upload created (content addressing may reuse an existing file).
	fail := func(status int, message string) {
		if !preExisting || req.SignedIPASha256 == "" || computed != req.SignedIPASha256 {
			s.artifacts.Remove(computed)
		}
		s.writeError(w, status, message)
	}
	if req.SignedIPASha256 != "" && req.SignedIPASha256 != computed {
		fail(http.StatusBadRequest, "signed_ipa_sha256 does not match uploaded file")
		return
	}
	sha256 := computed

	var profileExpiry *time.Time
	if req.ProfileExpiryAt != "" {
		t, err := time.Parse(time.RFC3339, req.ProfileExpiryAt)
		if err != nil {
			fail(http.StatusBadRequest, "profile_expiry_at must be RFC3339")
			return
		}
		profileExpiry = &t
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		fail(http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback(r.Context())

	// Upsert the Apple account by team identifier.
	var accountID uuid.UUID
	err = tx.QueryRow(r.Context(), `
		INSERT INTO apple_accounts (label, team_identifier, auth_state, last_auth_at,
			registered_device_count, registered_app_id_count)
		VALUES ($1, $2, 'authenticated', now(), $3, $4)
		ON CONFLICT (team_identifier) WHERE team_identifier IS NOT NULL
		DO UPDATE SET
			auth_state = 'authenticated',
			last_auth_at = now(),
			registered_device_count = $3,
			registered_app_id_count = $4
		RETURNING id`,
		req.AccountEmail, req.TeamID, req.DeviceCount, req.AppIDCount).Scan(&accountID)
	if err != nil {
		fail(http.StatusInternalServerError, "account upsert failed")
		return
	}

	// Upsert the certificate by serial number.
	var certID *uuid.UUID
	if req.CertSerial != "" {
		var id uuid.UUID
		err = tx.QueryRow(r.Context(), `
			INSERT INTO certificates (serial_number, account_id, key_ref, revoked)
			VALUES ($1, $2, $3, false)
			ON CONFLICT (serial_number) DO UPDATE SET
				account_id = $2,
				revoked = false
			RETURNING id`,
			req.CertSerial, accountID, "signing-worker").Scan(&id)
		if err != nil {
			fail(http.StatusInternalServerError, "certificate upsert failed")
			return
		}
		certID = &id
	}

	var signedID uuid.UUID
	err = tx.QueryRow(r.Context(), `
		INSERT INTO signed_artifacts (source_artifact_id, device_id, account_id,
			certificate_id, provisioning_profile_ref, signed_bundle_identifier,
			profile_expiry_at, signed_ipa_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		req.SourceArtifactID, req.DeviceID, accountID, certID,
		nullString(req.ProvisioningProfileRef), req.SignedBundleIdentifier,
		profileExpiry, sha256).Scan(&signedID)
	if err != nil {
		fail(http.StatusInternalServerError, "signed artifact insert failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		fail(http.StatusInternalServerError, "transaction failed")
		return
	}
	s.audit.Record(r.Context(), "agent:"+agentID(r.Context()).String(), "signed_artifact.uploaded",
		audit.WithData(map[string]any{"signed_artifact_id": signedID, "sha256": sha256}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": signedID, "sha256": sha256,
	})
}

// handleListSignedArtifacts returns signed artifacts joined with their source
// artifact, device and signing team for the dashboard.
func (s *Server) handleListSignedArtifacts(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT sa.id, sa.signed_ipa_sha256, sa.signed_bundle_identifier,
		       sa.profile_expiry_at, sa.signed_at,
		       a.filename, a.version, a.bundle_identifier AS source_bundle,
		       d.device_name, d.udid,
		       acc.label, acc.team_identifier,
		       c.serial_number
		FROM signed_artifacts sa
		JOIN artifacts a ON a.id = sa.source_artifact_id
		JOIN devices d ON d.id = sa.device_id
		JOIN apple_accounts acc ON acc.id = sa.account_id
		LEFT JOIN certificates c ON c.id = sa.certificate_id
		ORDER BY sa.signed_at DESC
		LIMIT 200`)
}

// handleListAccounts returns Apple accounts with slot usage for the
// dashboard (D12: warn before a free team's limits are exhausted).
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT a.id, a.label, a.team_identifier, a.team_type, a.auth_state,
		       a.last_auth_at, a.registered_app_id_count, a.registered_device_count,
		       a.locked, a.failure_count,
		       COUNT(c.id) AS cert_count
		FROM apple_accounts a
		LEFT JOIN certificates c ON c.account_id = a.id AND NOT c.revoked
		GROUP BY a.id
		ORDER BY a.created_at`)
}
