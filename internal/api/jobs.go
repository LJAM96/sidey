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

// handleClaimJobs atomically claims pending jobs for the agent's devices.
func (s *Server) handleClaimJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []uuid.UUID `json:"device_ids"`
		Limit     int         `json:"limit"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	claimed, err := s.jobs.Claim(r.Context(), agentID(r.Context()), req.DeviceIDs, req.Limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "claim failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": claimed})
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
