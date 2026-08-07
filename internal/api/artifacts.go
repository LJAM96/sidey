package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"sidey/internal/artifacts"
	"sidey/internal/audit"
)

// handleUploadArtifact ingests an IPA body, stores it content-addressed and
// records its metadata. Uploading the same bytes twice returns the existing
// artifact instead of duplicating it.
func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	filename := filepath.Base(r.URL.Query().Get("filename"))
	if filename == "." || filename == "" {
		filename = "app.ipa"
	}
	if filepath.Ext(filename) != ".ipa" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "filename must end in .ipa"})
		return
	}

	sha256, err := s.artifacts.Save(r.Body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "storing upload failed")
		return
	}

	// Dedupe: same content -> same artifact row.
	var id uuid.UUID
	var state string
	var existing bool
	err = s.pool.QueryRow(r.Context(),
		`SELECT id, quarantine_state FROM artifacts WHERE sha256 = $1`, sha256).Scan(&id, &state)
	if err == nil {
		existing = true
	}

	// Extract metadata from the stored file; reject malformed archives and
	// remove their blob.
	meta, metaErr := artifacts.Inspect(s.artifacts.Path(sha256))
	if metaErr != nil {
		if !existing {
			if err := removeIfExists(s.artifacts.Path(sha256)); err != nil {
				s.logger.Warn("failed to remove rejected artifact", "error", err)
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": metaErr.Error()})
		return
	}
	extensions, err := json.Marshal(meta.Extensions)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "metadata encode failed")
		return
	}

	if existing {
		s.audit.Record(r.Context(), "admin", "artifact.uploaded", audit.WithData(map[string]any{"sha256": sha256, "existing": true}))
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "sha256": sha256, "existing": true,
			"bundle_identifier": meta.BundleIdentifier, "version": meta.Version,
			"build_number": meta.BuildNumber, "min_os_version": meta.MinOSVersion,
			"platform": meta.Platform, "extensions": meta.Extensions,
			"quarantine_state": state,
		})
		return
	}

	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO artifacts (sha256, filename, version, build_number,
			bundle_identifier, platform, min_os_version, extensions, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'upload')
		RETURNING id`,
		sha256, filename, nullString(meta.Version), nullString(meta.BuildNumber),
		meta.BundleIdentifier, nullString(meta.Platform), nullString(meta.MinOSVersion),
		extensions).Scan(&id)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "artifact insert failed")
		return
	}
	s.audit.Record(r.Context(), "admin", "artifact.uploaded", audit.WithData(map[string]any{"sha256": sha256, "artifact_id": id}))
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
	tag, err := s.pool.Exec(r.Context(),
		`UPDATE artifacts SET quarantine_state = $2 WHERE id = $1`, id, req.State)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "state update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	s.audit.Record(r.Context(), "admin", "artifact.state",
		audit.WithData(map[string]any{"artifact_id": id, "state": req.State}))
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

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// nullString converts empty strings to NULL for nullable columns.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
