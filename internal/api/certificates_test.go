package api

import (
	"net/http"
	"testing"
)

// TestCertificateRevokeAndDashboard covers the certificate lifecycle control:
// an operator revokes a certificate with a reason, the dashboard lists both
// live and revoked certs, and a second revocation of the same cert is idempotent.
func TestCertificateRevokeAndDashboard(t *testing.T) {
	truncate(t)
	// Insert a certificate directly (the signing worker upserts these during
	// a signed-artifact upload, covered in signing_test.go).
	var accountID string
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO apple_accounts (label, team_identifier)
		VALUES ('revoke@example.com', 'TEAMREV')
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	var certID string
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO certificates (serial_number, account_id, key_ref, revoked)
		VALUES ('DEADBEEFREV', $1, 'signing-worker', false)
		RETURNING id`, accountID).Scan(&certID); err != nil {
		t.Fatal(err)
	}

	// Non-admin cannot revoke.
	res, body := doJSON(t, "POST", "/api/v1/certificates/"+certID+"/revoke", "wrong-key", map[string]any{
		"reason": "compromised",
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoke with bad key: %d %v", res.StatusCode, body)
	}

	// Admin revokes with a reason.
	res, body = doJSON(t, "POST", "/api/v1/certificates/"+certID+"/revoke", adminKey, map[string]any{
		"reason": "compromised",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %v", res.StatusCode, body)
	}

	var revoked bool
	var reason string
	if err := pool.QueryRow(t.Context(),
		`SELECT revoked, COALESCE(revoked_reason, '') FROM certificates WHERE id = $1`,
		certID).Scan(&revoked, &reason); err != nil {
		t.Fatal(err)
	}
	if !revoked || reason != "compromised" {
		t.Fatalf("cert state: revoked=%v reason=%q", revoked, reason)
	}

	// Revoke again is harmless (idempotent update).
	res, _ = doJSON(t, "POST", "/api/v1/certificates/"+certID+"/revoke", adminKey, map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-revoke: %d", res.StatusCode)
	}

	// Dashboard lists revoked certs.
	res, body = doJSON(t, "GET", "/api/v1/dashboard/certificates", adminKey, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d %v", res.StatusCode, body)
	}
	found := false
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		if row["serial_number"] == "DEADBEEFREV" {
			found = true
			if row["revoked"] != true {
				t.Fatalf("dashboard shows cert as not revoked: %v", row)
			}
		}
	}
	if !found {
		t.Error("revoked certificate not listed on dashboard")
	}

	// An audit trail entry records the revocation.
	var auditCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM audit_events WHERE action = 'certificate.revoked'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 1 {
		t.Errorf("expected at least one certificate.revoked audit event, got %d", auditCount)
	}
}