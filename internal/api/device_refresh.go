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
// A refresh job carries the ORIGINAL source artifact, but the device service
// cannot sign (it must never see Apple credentials or signing keys). It
// therefore asks the control plane to run the sign step on its behalf:
//
//  1. POST /api/v1/device/refresh/{id}/sign ensures a sign job exists for
//     the refresh job's device and source artifact (idempotent per refresh
//     job) and returns its id.
//  2. The device service polls GET /api/v1/device/jobs/{id} until the sign
//     job completes, then downloads the signed derivative and installs it
//     WITHOUT re-signing (SKIP_SIGN=1), and completes the refresh job with
//     the new profile expiry.
//
// Both endpoints live on the same-host Unix socket (ADR-0008) and trust
// their caller by filesystem permissions, like the rest of DeviceHandler.

// handleDeviceRefreshSign ensures a signing-worker sign job exists for a
// refresh job held by the same-host device service. Safe to retry: the sign
// job's idempotency key is derived from the refresh job id, so repeated
// calls (e.g. after a device-service restart) return the same sign job.
func (s *Server) handleDeviceRefreshSign(w http.ResponseWriter, r *http.Request) {
	refreshID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid refresh job id"})
		return
	}

	var (
		jobType  string
		deviceID *uuid.UUID
		jobState string
		params   json.RawMessage
		claimed  *uuid.UUID
	)
	err = s.pool.QueryRow(r.Context(),
		`SELECT job_type, device_id, state, parameters, claimed_by
		 FROM jobs WHERE id = $1`, refreshID).
		Scan(&jobType, &deviceID, &jobState, &params, &claimed)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "refresh job not found"})
		return
	}
	if jobType != jobs.JobTypeRefresh {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "job is not a refresh job"})
		return
	}
	if deviceID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "refresh job has no device"})
		return
	}
	switch jobState {
	case jobs.StateClaimed, jobs.StateInProgress:
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "refresh job is not active"})
		return
	}
	var refreshParams struct {
		ArtifactID uuid.UUID `json:"artifact_id"`
	}
	if err := json.Unmarshal(params, &refreshParams); err != nil || refreshParams.ArtifactID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "refresh job carries no source artifact (refusing to guess)",
		})
		return
	}

	// The refresh artifact must be an approved ORIGINAL IPA, never a signed
	// derivative: signing a derivative appends a second team suffix and
	// churns portal App IDs (0xe8008015 outage, Sep 2026).
	var quarantineState, bundleID string
	err = s.pool.QueryRow(r.Context(),
		`SELECT quarantine_state, bundle_identifier FROM artifacts WHERE id = $1`,
		refreshParams.ArtifactID).Scan(&quarantineState, &bundleID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "refresh artifact is not an original IPA (was it a signed derivative?)",
		})
		return
	}
	if quarantineState != "approved" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "artifact must be approved before signing"})
		return
	}

	var deviceUDID, deviceName, devicePlatform string
	err = s.pool.QueryRow(r.Context(),
		`SELECT udid, COALESCE(device_name, ''), COALESCE(platform, 'ios') FROM devices WHERE id = $1`,
		deviceID).Scan(&deviceUDID, &deviceName, &devicePlatform)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	deviceType := "ios"
	if devicePlatform == "tvos" || devicePlatform == "tvOS" {
		deviceType = "tvos"
	}

	signParams, err := json.Marshal(map[string]any{
		"artifact_id":  refreshParams.ArtifactID,
		"machine_name": "isideload-minimal",
		"udid":         deviceUDID,
		"device_name":  deviceName,
		"device_type":  deviceType,
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "parameter encoding failed")
		return
	}
	job, err := s.jobs.Create(r.Context(), "device-service", jobs.CreateRequest{
		JobType:        jobs.JobTypeSign,
		DeviceID:       deviceID,
		Parameters:     signParams,
		IdempotencyKey: fmt.Sprintf("refresh-sign:%s", refreshID),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sign_job_id": job.ID,
		"state":       job.State,
	})
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
