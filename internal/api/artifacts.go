package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/google/uuid"

	"sidey/internal/artifacts"
	"sidey/internal/audit"
)

// handleUploadArtifact ingests an IPA body, stores it content-addressed and
// records its metadata. Uploading the same bytes twice returns the existing
// artifact instead of duplicating it. The request body is capped before it
// reaches disk so an oversized upload cannot exhaust the artifact volume.
func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	filename := filepath.Base(r.URL.Query().Get("filename"))
	if filename == "." || filename == "" {
		filename = "app.ipa"
	}
	if filepath.Ext(filename) != ".ipa" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "filename must end in .ipa"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxArtifactBytes)
	sha256, tmp, err := s.artifacts.SaveToTemp(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return
		}
		s.writeError(w, http.StatusInternalServerError, "storing upload failed")
		return
	}
	defer s.artifacts.DiscardTemp(tmp)

	// Validate the archive against the temp file before publishing it under
	// its content hash, so a malformed IPA never lands in the store. Reject
	// malformed archives and quarantine the bytes by simply not publishing.
	meta, metaErr := artifacts.Inspect(tmp)
	if metaErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": metaErr.Error()})
		return
	}
	extensions, err := json.Marshal(meta.Extensions)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "metadata encode failed")
		return
	}

	// Publish + row insert are one transaction under a per-hash advisory
	// lock so retention can never delete this blob between publication and
	// the row commit (and a concurrent retention pass cannot see it as
	// unreferenced while it is being published).
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

	// Dedupe: same content -> same artifact row.
	var id uuid.UUID
	var state string
	existing := false
	lookupErr := tx.QueryRow(r.Context(),
		`SELECT id, quarantine_state FROM artifacts WHERE sha256 = $1`, sha256).Scan(&id, &state)
	if lookupErr == nil {
		existing = true
	} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
		s.writeError(w, http.StatusInternalServerError, "artifact lookup failed")
		return
	}

	created, err := s.artifacts.Publish(sha256, tmp)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing upload failed")
		return
	}
	_ = created

	if existing {
		s.audit.Record(r.Context(), "admin", "artifact.uploaded", audit.WithData(map[string]any{"sha256": sha256, "existing": true}))
		if err := tx.Commit(r.Context()); err != nil {
			s.writeError(w, http.StatusInternalServerError, "transaction failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "sha256": sha256, "existing": true,
			"bundle_identifier": meta.BundleIdentifier, "version": meta.Version,
			"build_number": meta.BuildNumber, "min_os_version": meta.MinOSVersion,
			"platform": meta.Platform, "extensions": meta.Extensions,
			"quarantine_state": state,
		})
		return
	}

	err = tx.QueryRow(r.Context(), `
		INSERT INTO artifacts (sha256, filename, version, build_number,
			bundle_identifier, platform, min_os_version, extensions, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'upload')
		RETURNING id`,
		sha256, filename, nullString(meta.Version), nullString(meta.BuildNumber),
		meta.BundleIdentifier, nullString(meta.Platform), nullString(meta.MinOSVersion),
		extensions).Scan(&id)
	if err != nil {
		// The insert failed after publishing: drop the blob only if nothing
		// references it (it cannot, we hold the lock and no row exists yet).
		s.artifacts.Remove(sha256)
		s.writeError(w, http.StatusInternalServerError, "artifact insert failed")
		return
	}
	if err := audit.RecordTx(r.Context(), tx, "admin", "artifact.uploaded",
		audit.WithData(map[string]any{"sha256": sha256, "artifact_id": id})); err != nil {
		s.writeError(w, http.StatusInternalServerError, "audit write failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "sha256": sha256, "existing": false,
		"bundle_identifier": meta.BundleIdentifier, "version": meta.Version,
		"build_number": meta.BuildNumber, "min_os_version": meta.MinOSVersion,
		"platform": meta.Platform, "extensions": meta.Extensions,
		"quarantine_state": "quarantined",
	})
}

// handleSetArtifactState moves an artifact through the quarantine workflow.
func (s *Server) handleSetArtifactState(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid artifact id"})
		return
	}
	var req struct {
		State string `json:"state"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	switch req.State {
	case "quarantined", "approved", "rejected":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state must be quarantined, approved or rejected"})
		return
	}
	// Artifact approval is security sensitive (it decides what may be signed
	// and installed); the audit event commits with the state change so it
	// cannot be lost independently.
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(),
		`UPDATE artifacts SET quarantine_state = $2 WHERE id = $1`, id, req.State)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "state update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	if err := audit.RecordTx(r.Context(), tx, "admin", "artifact.state",
		audit.WithData(map[string]any{"artifact_id": id, "state": req.State})); err != nil {
		s.writeError(w, http.StatusInternalServerError, "audit write failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeError(w, http.StatusInternalServerError, "transaction failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "quarantine_state": req.State})
}

// handleDownloadArtifact serves the original IPA by content hash.
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid artifact id"})
		return
	}
	var sha256, filename string
	err = s.pool.QueryRow(r.Context(),
		`SELECT sha256, filename FROM artifacts WHERE id = $1`, id).Scan(&sha256, &filename)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	path := s.artifacts.Path(sha256)
	if !s.artifacts.Exists(sha256) {
		s.writeError(w, http.StatusInternalServerError, "artifact blob missing")
		return
	}
	s.audit.Record(r.Context(), "admin", "artifact.downloaded",
		audit.WithData(map[string]any{"artifact_id": id, "sha256": sha256}))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// nullString converts empty strings to NULL for nullable columns.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
