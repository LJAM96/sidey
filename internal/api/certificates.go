package api

import (
	"net/http"

	"github.com/google/uuid"
	"sidey/internal/audit"
)

// handleListCertificates returns certificates with their owning account for
// the dashboard. Revoked certificates are shown (with a separate reason and
// timestamp) so an operator can see what was retired and why.
func (s *Server) handleListCertificates(w http.ResponseWriter, r *http.Request) {
	s.queryTable(w, r, `
		SELECT c.id, c.serial_number, c.expiry_at, c.revoked, c.revoked_at,
		       c.revoked_reason, c.key_ref, c.created_at,
		       a.label, a.team_identifier
		FROM certificates c
		JOIN apple_accounts a ON a.id = c.account_id
		ORDER BY c.created_at DESC`)
}

type revokeCertificateRequest struct {
	Reason string `json:"reason"`
}

// handleRevokeCertificate marks a certificate revoked in the control plane.
// The signing worker will not reuse a revoked certificate; the next sign job
// for the account creates a replacement from the same identity. Revocation
// still consumes a free-team certificate slot on Apple's side until the
// replacement is created, so this is an explicit operator action when a
// certificate is compromised or decommissioned.
func (s *Server) handleRevokeCertificate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid certificate id"})
		return
	}
	var reason string
	if r.ContentLength > 0 {
		var req revokeCertificateRequest
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		reason = req.Reason
	}

	res, err := s.pool.Exec(r.Context(), `
		UPDATE certificates
		SET revoked = true, revoked_at = now(), revoked_reason = $2
		WHERE id = $1`, id, reason)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	if res.RowsAffected() != 1 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "certificate not found"})
		return
	}
	s.audit.Record(r.Context(), "admin", "certificate.revoked",
		audit.WithData(map[string]any{"certificate_id": id, "reason": reason}))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              id.String(),
		"revoked":         true,
		"revoked_reason":  reason,
		"next_sign_renews": true,
	})
}