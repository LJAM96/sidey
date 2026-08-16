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
// strictly bounded by the agent's server controlled role. An agent can only
// claim the job types its role authorizes: a refresh agent only gets refresh
// jobs, a signing worker only gets sign jobs, and device agents only get
// device-scoped jobs. If the client requests specific job_types, they must
// all be permitted by the role (or 403 Forbidden is returned); if no job_types
// are specified, the server automatically defaults to the role's allowed set.
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
	allowed := allowedJobTypesForRole(role)
	allowedMap := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedMap[a] = true
	}

	effectiveJobTypes := allowed
	if len(req.JobTypes) > 0 {
		for _, t := range req.JobTypes {
			if !allowedMap[t] {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error": "agent role " + role + " is not permitted to claim " + t + " jobs"})
				return
			}
		}
		effectiveJobTypes = req.JobTypes
	}

	claimed, err := s.jobs.Claim(r.Context(), agentID(r.Context()), req.DeviceIDs, effectiveJobTypes, req.Limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "claim failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": claimed})
}

// allowedJobTypesForRole reports the server-authorized job types for an agent role.
// sign is the only privileged global type: it is reserved for the signing
// worker. Every device-scoped role executes on devices it owns, and the
// device service (ADR-0008) additionally handles refresh, since it replaced
// the standalone refresh agent in the consolidated model. No device-scoped
// role ever claims sign.
func allowedJobTypesForRole(role string) []string {
	switch role {
	case "signing_worker":
		return []string{jobs.JobTypeSign, jobs.JobTypeExportP12}
	case "refresh_agent":
		return []string{jobs.JobTypeRefresh}
	case "device_service":
		// Same-host socket node or remote node: owns devices, performs
		// installs, verifies and refreshes. Sign stays with the worker.
		return []string{
			jobs.JobTypeInstall,
			jobs.JobTypeVerify,
			jobs.JobTypeRefresh,
			jobs.JobTypeUninstall,
			jobs.JobTypeInventory,
			jobs.JobTypeLiveContainerPush,
			jobs.JobTypeInstalledApps,
		}
	default: // device_agent, tvos_agent
		return []string{
			jobs.JobTypeInstall,
			jobs.JobTypeVerify,
			jobs.JobTypeUninstall,
			jobs.JobTypeInventory,
		}
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
