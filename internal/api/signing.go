package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"sidey/internal/artifacts"
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
// registered by the worker during signing). The job carries the target
// device's UDID and name so the signing worker signs for the right device,
// never a hardcoded default.
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

	var deviceUDID, deviceName, devicePlatform string
	err = s.pool.QueryRow(r.Context(),
		`SELECT udid, COALESCE(device_name, ''), COALESCE(platform, 'ios') FROM devices WHERE id = $1`,
		req.DeviceID).Scan(&deviceUDID, &deviceName, &devicePlatform)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	// The same source IPA must be signable independently for iOS and tvOS
	// (Phase F): platform maps to DEVICE_TYPE so device registration, App ID
	// registration and provisioning profile downloads hit the right portal
	// endpoints. Unknown platforms default to iOS.
	deviceType := "ios"
	if devicePlatform == "tvos" || devicePlatform == "tvOS" {
		deviceType = "tvos"
	}

	// Platform family guard (Phase G): reject signing an artifact whose
	// family does not match the target device before a job is created.
	var artifactPlatform any
	err = s.pool.QueryRow(r.Context(),
		`SELECT platform FROM artifacts WHERE id = $1`, req.ArtifactID).Scan(&artifactPlatform)
	if err == nil && artifactPlatform != nil {
		if platform, ok := artifactPlatform.(string); ok && platform != "" {
			artifactType := "ios"
			if platform == "AppleTVOS" {
				artifactType = "tvos"
			}
			if artifactType != deviceType {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": fmt.Sprintf("artifact platform %s does not match device platform %s", platform, devicePlatform),
				})
				return
			}
		}
	}

	machineName := req.MachineName
	if machineName == "" {
		machineName = "isideload-minimal"
	}
	parameters, err := json.Marshal(map[string]any{
		"artifact_id":  req.ArtifactID,
		"machine_name": machineName,
		"udid":         deviceUDID,
		"device_name":  deviceName,
		"device_type":  deviceType,
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
// approved artifact. Access is bound to job ownership: the caller must hold
// an active job referencing the artifact — a sign job it claimed (signing
// worker) or a job on a device it owns. A mere (stolen or shared) agent key
// no longer grants access to arbitrary approved IPAs without an active job
// establishing the right to the artifact.
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
	if !s.agentMayAccessArtifact(r, id) {
		writeJSON(w, http.StatusForbidden,
			map[string]any{"error": "no active job held by this agent references this artifact"})
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

// agentMayAccessArtifact reports whether the caller holds an active job that
// establishes the right to download the artifact: a claimed/in-progress sign
// job for a signing worker, or a claimed/in-progress job on a device the
// agent owns (device agents, refresh agents).
func (s *Server) agentMayAccessArtifact(r *http.Request, artifactID uuid.UUID) bool {
	agent := agentID(r.Context())
	var bound bool
	var err error
	if s.agentRole(r, agent) == "signing_worker" {
		err = s.pool.QueryRow(r.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM jobs j
				WHERE j.claimed_by = $1
				  AND j.job_type = 'sign'
				  AND j.state IN ('claimed', 'in_progress')
				  AND j.parameters->>'artifact_id' = $2::text
			)`, agent, artifactID).Scan(&bound)
	} else {
		err = s.pool.QueryRow(r.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM jobs j
				JOIN devices d ON d.id = j.device_id
				WHERE d.agent_id = $1
				  AND j.state IN ('claimed', 'in_progress')
				  AND j.parameters->>'artifact_id' = $2::text
			)`, agent, artifactID).Scan(&bound)
	}
	return err == nil && bound
}

type signedArtifactUploadRequest struct {
	JobID                  uuid.UUID `json:"job_id"`
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
// form. The upload is bound to the sign job that produced it: the caller must
// hold the job (claimed_by), and the job must target the same device and
// source artifact, or the upload is refused.
func (s *Server) handleUploadSignedArtifact(w http.ResponseWriter, r *http.Request) {
	// Cap the whole request (multipart overhead included) so an oversized
	// body cannot spill unbounded data to disk.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxArtifactBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart form"})
		return
	}
	var req signedArtifactUploadRequest
	req.JobID, _ = uuid.Parse(r.FormValue("job_id"))
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

	if req.SourceArtifactID == uuid.Nil || req.DeviceID == uuid.Nil || req.JobID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "job_id, source_artifact_id and device_id are required"})
		return
	}

	// The upload must be the derivative of the caller's own sign job for this
	// device and source artifact. Validate before consuming the IPA body so an
	// invalid binding costs nothing.
	bound := s.validateSignUploadBinding(w, r, req)
	if !bound {
		return
	}

	file, _, err := r.FormFile("ipa")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing ipa file"})
		return
	}
	defer file.Close()

	// Stream to a temp file first; the signed-derivative record is inserted
	// in the same transaction as its publication (under a per-hash advisory
	// lock) so retention can never delete this blob between publication and
	// the row commit.
	computed, tmp, err := s.artifacts.SaveToTemp(file)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing signed ipa failed")
		return
	}
	defer s.artifacts.DiscardTemp(tmp)
	if req.SignedIPASha256 != "" && req.SignedIPASha256 != computed {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signed_ipa_sha256 does not match uploaded file"})
		return
	}
	sha256 := computed

	// Independent signed IPA inspection: parse Info.plist and embedded.mobileprovision
	// directly from the uploaded file rather than trusting agent claims.
	signedMeta, err := artifacts.InspectSigned(tmp)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signed IPA validation failed: " + err.Error()})
		return
	}

	// Fetch device UDID/platform and source artifact bundle ID/platform for validation.
	var (
		deviceUdid     string
		devicePlatform string
		sourceBundleID string
		sourcePlatform string
	)
	err = s.pool.QueryRow(r.Context(), `
		SELECT d.udid, d.platform, a.bundle_identifier, a.platform
		FROM devices d, artifacts a
		WHERE d.id = $1 AND a.id = $2`, req.DeviceID, req.SourceArtifactID).
		Scan(&deviceUdid, &devicePlatform, &sourceBundleID, &sourcePlatform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "target device or source artifact not found"})
		return
	}

	// 1. Authoritative profile fields (fail closed).
	if signedMeta.TeamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile lacks TeamIdentifier"})
		return
	}
	if req.TeamID != "" && req.TeamID != signedMeta.TeamID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "team_id does not match embedded provisioning profile"})
		return
	}
	teamID := signedMeta.TeamID

	if signedMeta.ProfileExpiry.IsZero() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile lacks ExpirationDate"})
		return
	}
	if signedMeta.ProfileExpiry.Before(time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile is expired"})
		return
	}
	profileExpiry := &signedMeta.ProfileExpiry

	if signedMeta.AppIdentifier == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile lacks application-identifier entitlement"})
		return
	}
	profileRef := signedMeta.ProfileUUID
	if profileRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile lacks UUID"})
		return
	}

	// 2. Validate target device UDID in provisioned devices (fail closed for personal/dev profiles).
	if signedMeta.ProvisionsAllDevices {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "wildcard/enterprise profile (ProvisionsAllDevices) not permitted for personal signing"})
		return
	}
	if len(signedMeta.ProvisionedDevices) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "embedded provisioning profile contains no ProvisionedDevices"})
		return
	}
	deviceFound := false
	for _, u := range signedMeta.ProvisionedDevices {
		if strings.EqualFold(u, deviceUdid) {
			deviceFound = true
			break
		}
	}
	if !deviceFound {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "target device UDID (" + deviceUdid + ") is not authorized by embedded provisioning profile",
		})
		return
	}

	// 3. Validate platform compatibility against device and source artifact.
	if signedMeta.DevicePlatform != devicePlatform || (sourcePlatform != "" && signedMeta.DevicePlatform != sourcePlatform) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "signed IPA platform (" + signedMeta.DevicePlatform + ") does not match target device (" + devicePlatform + ") or source artifact (" + sourcePlatform + ")",
		})
		return
	}

	// 4. Validate source bundle identifier relationship and app identifier entitlement.
	signedBundleID := signedMeta.BundleIdentifier
	if !isValidSignedBundleID(sourceBundleID, signedBundleID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "signed bundle identifier (" + signedBundleID + ") is not a valid derivative of source bundle (" + sourceBundleID + ")",
		})
		return
	}
	if req.SignedBundleIdentifier != "" && req.SignedBundleIdentifier != signedBundleID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signed_bundle_identifier does not match Info.plist"})
		return
	}
	expectedAppID := teamID + "." + signedBundleID
	if signedMeta.AppIdentifier != expectedAppID && signedMeta.AppIdentifier != teamID+".*" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "embedded profile application-identifier (" + signedMeta.AppIdentifier + ") does not match signed bundle (" + expectedAppID + ")",
		})
		return
	}

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext($1))`, sha256); err != nil {
		s.writeError(w, http.StatusInternalServerError, "lock acquisition failed")
		return
	}
	created, err := s.artifacts.Publish(sha256, tmp)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing signed ipa failed")
		return
	}
	// Any failure from here on must not leave an unreferenced file behind. We
	// still hold the advisory lock, so removal is race-free: no retention pass
	// and no concurrent upload can observe or touch these bytes. Only remove
	// what this request created; borrowed (pre-existing) blobs stay.
	fail := func(status int, message string) {
		if created {
			s.artifacts.Remove(sha256)
		}
		s.writeError(w, status, message)
	}

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
		req.AccountEmail, teamID, req.DeviceCount, req.AppIDCount).Scan(&accountID)
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
			profile_expiry_at, signed_ipa_sha256, job_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		req.SourceArtifactID, req.DeviceID, accountID, certID,
		nullString(profileRef), signedBundleID,
		profileExpiry, sha256, req.JobID).Scan(&signedID)
	if err != nil {
		fail(http.StatusInternalServerError, "signed artifact insert failed")
		return
	}
	// The signed-derivative record is security sensitive (it is the artifact
	// that gets installed on devices); its audit event commits in the same
	// transaction so it cannot be lost independently of the record.
	if err := audit.RecordTx(r.Context(), tx, "agent:"+agentID(r.Context()).String(),
		"signed_artifact.uploaded",
		audit.WithData(map[string]any{"signed_artifact_id": signedID, "sha256": sha256, "job_id": req.JobID})); err != nil {
		fail(http.StatusInternalServerError, "audit write failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		fail(http.StatusInternalServerError, "transaction failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": signedID, "sha256": sha256,
	})
}

// validateSignUploadBinding checks that the uploader holds a sign job for the
// exact device and source artifact named in the upload. It writes the HTTP
// response and returns false when the binding is invalid.
func (s *Server) validateSignUploadBinding(w http.ResponseWriter, r *http.Request, req signedArtifactUploadRequest) bool {
	var (
		jobType    string
		jobDevice  *uuid.UUID
		jobClaimed *uuid.UUID
		jobState   string
		params     json.RawMessage
	)
	err := s.pool.QueryRow(r.Context(),
		`SELECT job_type, device_id, claimed_by, state, parameters
		 FROM jobs WHERE id = $1`, req.JobID).
		Scan(&jobType, &jobDevice, &jobClaimed, &jobState, &params)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return false
	}
	if jobType != jobs.JobTypeSign || jobClaimed == nil || *jobClaimed != agentID(r.Context()) {
		writeJSON(w, http.StatusForbidden,
			map[string]any{"error": "upload is not bound to a sign job held by this agent"})
		return false
	}
	if jobDevice == nil || *jobDevice != req.DeviceID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sign job does not target this device"})
		return false
	}
	switch jobState {
	case jobs.StateClaimed, jobs.StateInProgress:
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "sign job is not in progress"})
		return false
	}
	var jobParams struct {
		ArtifactID uuid.UUID `json:"artifact_id"`
	}
	if err := json.Unmarshal(params, &jobParams); err != nil || jobParams.ArtifactID != req.SourceArtifactID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sign job does not reference this source artifact"})
		return false
	}
	return true
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

// isValidSignedBundleID verifies that a signed IPA's bundle identifier is a
// legitimate derivative of the source artifact's bundle identifier (exact match
// or with a valid signer suffix like <source>.<random/team-suffix>).
func isValidSignedBundleID(source, signed string) bool {
	if source == "" || signed == "" {
		return false
	}
	if source == signed {
		return true
	}
	if strings.HasPrefix(signed, source+".") {
		return true
	}
	return false
}
