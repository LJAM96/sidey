package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"sidey/internal/jobs"
)

// handleCreateJob enqueues a job idempotently by idempotency_key.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobs.CreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	job, err := s.jobs.Create(r.Context(), "admin", req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// handleClaimJobs atomically claims pending jobs for the agent's devices,
// optionally filtered by job type (the signing worker claims sign jobs).
// The claim is restricted by the agent's server controlled role: an agent
// can only claim the job types its enrolment token granted, so a refresh
// agent can never claim sign jobs and the two privileged global types
// (sign, refresh) stay bound to their workers.
func (s *Server) handleClaimJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []uuid.UUID `json:"device_ids"`
		JobTypes  []string    `json:"job_types"`
		Limit     int         `json:"limit"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	role := s.agentRole(r, agentID(r.Context()))
	for _, t := range req.JobTypes {
		if !roleAllows(role, t) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "agent role " + role + " is not permitted to claim " + t + " jobs"})
			return
		}
	}
	claimed, err := s.jobs.Claim(r.Context(), agentID(r.Context()), req.DeviceIDs, req.JobTypes, req.Limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "claim failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": claimed})
}

// roleAllows reports whether the agent role may claim a given job type.
// sign is the only privileged global type: it is reserved for the signing
// worker. Every device-scoped role executes on devices it owns, and the
// device service (ADR-0008) additionally handles refresh, since it replaced
// the standalone refresh agent in the consolidated model. No device-scoped
// role ever claims sign.
func roleAllows(role, jobType string) bool {
	switch role {
	case "signing_worker":
		return jobType == "sign"
	case "refresh_agent":
		return jobType == "refresh"
	case "device_service":
		// Same-host socket node or remote node: owns devices, performs
		// installs, verifies and refreshes. Sign stays with the worker.
		return jobType != "sign"
	default: // device_agent, tvos_agent
		return jobType != "sign" && jobType != "refresh"
	}
}

// handleUpdateJob applies a status update from the claiming agent.
func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid job id"})
		return
	}
	var req jobs.UpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	job, err := s.jobs.Update(r.Context(), agentID(r.Context()), jobID, req)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotClaimed):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "job is not claimed by this agent"})
		case errors.Is(err, jobs.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "invalid job state transition"})
		default:
			s.writeError(w, http.StatusInternalServerError, "job update failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, job)
}
