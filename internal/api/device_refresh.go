package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"sidey/internal/jobs"
)

// Refresh orchestration (Phase I over the split signing pipeline).
//
// A refresh job names the ORIGINAL source artifact (resolved authoritatively
// by the scheduler through installation_records, never guessed from job
// history). The device service cannot sign (it must never see Apple
// credentials or signing keys), so the control plane runs the sign step on
// its behalf and the device service only ever installs:
//
//  1. POST /api/v1/device/refresh/{id}/sign ensures a sign job exists for
//     the refresh job (idempotent per refresh job) and returns its id.
//  2. The device service polls GET /api/v1/device/jobs/{id} until the sign
//     job completes.
//  3. POST /api/v1/device/refresh/{id}/install ensures the sign's install
//     child exists and hands it to the caller claimed (idempotent per
//     refresh+signed pair; a failed install is requeued, never duplicated).
//  4. The device service installs without re-signing and completes both
//     the install job and the refresh parent.
//
// All three endpoints live on the same-host Unix socket (ADR-0008) and trust
// their caller by filesystem permissions, like the rest of DeviceHandler.

// refreshContext loads a refresh job and its deployment context for the
// orchestration endpoints below.
type refreshContext struct {
	refreshID    uuid.UUID
	deploymentID *uuid.UUID
	deviceID     uuid.UUID
	purpose      string
	sourceID     uuid.UUID
	appleID      string
	udid         string
	platform     string
	deviceName   string
}

func (s *Server) loadRefreshContext(r *http.Request, refreshID uuid.UUID) (*refreshContext, int, map[string]any) {
	var (
		jobType   string
		deviceID  *uuid.UUID
		jobState  string
		params    json.RawMessage
		purposeDB *string
	)
	err := s.pool.QueryRow(r.Context(),
		`SELECT job_type, device_id, state, parameters, purpose
		 FROM jobs WHERE id = $1`, refreshID).
		Scan(&jobType, &deviceID, &jobState, &params, &purposeDB)
	if err != nil {
		return nil, http.StatusNotFound, map[string]any{"error": "refresh job not found"}
	}
	if jobType != jobs.JobTypeRefresh {
		return nil, http.StatusBadRequest, map[string]any{"error": "job is not a refresh job"}
	}
	if deviceID == nil {
		return nil, http.StatusBadRequest, map[string]any{"error": "refresh job has no device"}
	}
	switch jobState {
	case jobs.StateClaimed, jobs.StateInProgress:
	default:
		return nil, http.StatusConflict, map[string]any{"error": "refresh job is not active"}
	}
	var refreshParams struct {
		DeploymentID         *uuid.UUID `json:"deployment_id"`
		SourceArtifactID     *uuid.UUID `json:"source_artifact_id"`
		ArtifactID           uuid.UUID  `json:"artifact_id"`
		AppleID              string     `json:"apple_id"`
		PreviousSignedID     *uuid.UUID `json:"previous_signed_artifact_id"`
	}
	if err := json.Unmarshal(params, &refreshParams); err != nil {
		return nil, http.StatusBadRequest, map[string]any{"error": "refresh job parameters malformed"}
	}
	sourceID := refreshParams.ArtifactID
	if refreshParams.SourceArtifactID != nil && *refreshParams.SourceArtifactID != uuid.Nil {
		sourceID = *refreshParams.SourceArtifactID
	}

	// The refresh artifact must be an approved ORIGINAL IPA, never a signed
	// derivative: signing a derivative appends a second team suffix and
	// churns portal App IDs (0xe8008015 outage, Sep 2026).
	var quarantineState string
	err = s.pool.QueryRow(r.Context(),
		`SELECT quarantine_state FROM artifacts WHERE id = $1`, sourceID).
		Scan(&quarantineState)
	if err != nil {
		return nil, http.StatusBadRequest, map[string]any{
			"error": "refresh artifact is not an original IPA (was it a signed derivative?)",
		}
	}
	if quarantineState != "approved" {
		return nil, http.StatusBadRequest, map[string]any{"error": "artifact must be approved before signing"}
	}

	var deviceUDID, deviceName, devicePlatform string
	err = s.pool.QueryRow(r.Context(),
		`SELECT udid, COALESCE(device_name, ''), COALESCE(platform, 'ios') FROM devices WHERE id = $1`,
		deviceID).Scan(&deviceUDID, &deviceName, &devicePlatform)
	if err != nil {
		return nil, http.StatusNotFound, map[string]any{"error": "device not found"}
	}

	// The Apple account is threaded through the workflow, never guessed:
	// prefer the scheduler-resolved value, else resolve through the
	// deployment's current installation record. Refuse rather than pick an
	// arbitrary account (a sign run against the wrong account stalls on
	// 2FA and signs under the wrong team).
	appleID := refreshParams.AppleID
	if appleID == "" && refreshParams.DeploymentID != nil {
		_ = s.pool.QueryRow(r.Context(), `
			SELECT acc.label
			FROM installation_records ir
			JOIN signed_artifacts sa ON sa.id = ir.signed_artifact_id
			JOIN apple_accounts acc ON acc.id = sa.account_id
			WHERE ir.deployment_id = $1`, *refreshParams.DeploymentID).Scan(&appleID)
	}
	if appleID == "" {
		return nil, http.StatusConflict, map[string]any{
			"error": "no Apple account resolvable for this refresh (re-associate the deployment instead of guessing)",
		}
	}

	purpose := "refresh"
	if purposeDB != nil && *purposeDB != "" {
		purpose = *purposeDB
	}
	return &refreshContext{
		refreshID:    refreshID,
		deploymentID: refreshParams.DeploymentID,
		deviceID:     *deviceID,
		purpose:      purpose,
		sourceID:     sourceID,
		appleID:      appleID,
		udid:         deviceUDID,
		platform:     devicePlatform,
		deviceName:   deviceName,
	}, 0, nil
}

// handleDeviceRefreshSign ensures a signing-worker sign job exists for a
// refresh job held by the same-host device service. Safe to retry: the sign
// job's idempotency key is derived from the refresh job id, so repeated
// calls (e.g. after a device-service restart) return the same sign job and
// a completed sign is reused rather than re-run.
func (s *Server) handleDeviceRefreshSign(w http.ResponseWriter, r *http.Request) {
	refreshID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid refresh job id"})
		return
	}
	rc, status, errBody := s.loadRefreshContext(r, refreshID)
	if errBody != nil {
		writeJSON(w, status, errBody)
		return
	}

	deviceType := "ios"
	if rc.platform == "tvos" || rc.platform == "tvOS" {
		deviceType = "tvos"
	}
	signFields := map[string]any{
		"artifact_id":    rc.sourceID,
		"machine_name":   "isideload-minimal",
		"udid":           rc.udid,
		"device_name":    rc.deviceName,
		"device_type":    deviceType,
		"apple_id":       rc.appleID,
		"refresh_job_id": rc.refreshID.String(),
	}
	if rc.deploymentID != nil {
		signFields["deployment_id"] = rc.deploymentID.String()
	}
	signParams, err := json.Marshal(signFields)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "parameter encoding failed")
		return
	}
	job, err := s.jobs.Create(r.Context(), "device-service", jobs.CreateRequest{
		JobType:        jobs.JobTypeSign,
		DeviceID:       &rc.deviceID,
		Parameters:     signParams,
		IdempotencyKey: fmt.Sprintf("refresh-sign:%s", refreshID),
		ParentJobID:    &rc.refreshID,
		Purpose:        &rc.purpose,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sign_job_id": job.ID,
		"state":       job.State,
		"device_id":   rc.deviceID,
		"apple_id":    rc.appleID,
	})
}

// handleDeviceEnsureInstall hands the sign job's install child to the
// orchestrator claimed and ready to execute. If no install child exists it
// is created with a deterministic key (refresh-install:<refresh>:<signed>);
// a failed install is requeued rather than duplicated, so install retries
// reuse the signed artifact instead of signing again.
func (s *Server) handleDeviceEnsureInstall(w http.ResponseWriter, r *http.Request) {
	refreshID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid refresh job id"})
		return
	}
	rc, status, errBody := s.loadRefreshContext(r, refreshID)
	if errBody != nil {
		writeJSON(w, status, errBody)
		return
	}

	// Locate the sign child created by handleDeviceRefreshSign.
	var signID uuid.UUID
	err = s.pool.QueryRow(r.Context(),
		`SELECT id FROM jobs WHERE idempotency_key = $1`, fmt.Sprintf("refresh-sign:%s", refreshID)).
		Scan(&signID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no sign job for this refresh yet"})
		return
	}
	var signState string
	var signResult json.RawMessage
	err = s.pool.QueryRow(r.Context(),
		`SELECT state, result FROM jobs WHERE id = $1`, signID).Scan(&signState, &signResult)
	if err != nil || signState != jobs.StateCompleted {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "sign job is not completed", "sign_job_id": signID, "sign_state": signState,
		})
		return
	}
	var signOut struct {
		SignedArtifactID uuid.UUID `json:"signed_artifact_id"`
	}
	if err := json.Unmarshal(signResult, &signOut); err != nil || signOut.SignedArtifactID == uuid.Nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "sign job completed without a signed artifact"})
		return
	}

	var signedBundle, deviceUdid, devicePlatform string
	err = s.pool.QueryRow(r.Context(), `
		SELECT sa.signed_bundle_identifier, d.udid, d.platform
		FROM signed_artifacts sa, devices d
		WHERE sa.id = $1 AND d.id = $2`,
		signOut.SignedArtifactID, rc.deviceID).
		Scan(&signedBundle, &deviceUdid, &devicePlatform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signed artifact or device not found"})
		return
	}
	installFields := map[string]any{
		"artifact_id":        signOut.SignedArtifactID.String(),
		"device_udid":        deviceUdid,
		"platform":           devicePlatform,
		"bundle_id":          signedBundle,
		"source_job_id":      signID.String(),
		"refresh_job_id":     rc.refreshID.String(),
		"signed_artifact_id": signOut.SignedArtifactID.String(),
	}
	if rc.deploymentID != nil {
		installFields["deployment_id"] = rc.deploymentID.String()
	}
	installParams, err := json.Marshal(installFields)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "parameter encoding failed")
		return
	}
	install, err := s.jobs.Create(r.Context(), "device-service", jobs.CreateRequest{
		JobType:        jobs.JobTypeInstall,
		DeviceID:       &rc.deviceID,
		Parameters:     installParams,
		IdempotencyKey: fmt.Sprintf("refresh-install:%s:%s", refreshID, signOut.SignedArtifactID),
		ParentJobID:    &rc.refreshID,
		Purpose:        &rc.purpose,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	claimed, err := s.jobs.ClaimSpecific(r.Context(), agentID(r.Context()), install.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "install_job_id": install.ID, "install_state": install.State,
		})
		return
	}
	writeJSON(w, http.StatusOK, claimed)
}

// handleDeviceGetJob reports a job's status to the same-host device service
// so it can follow a sign job it requested. The socket caller is trusted;
// only status fields are exposed.
func (s *Server) handleDeviceGetJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid job id"})
		return
	}
	job, err := s.jobs.Get(r.Context(), jobID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}
