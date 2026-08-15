package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestUpdateAppleCredentials(t *testing.T) {
	truncate(t)

	// Missing fields rejected
	res, body := doJSON(t, "POST", "/api/v1/admin/apple-accounts/credentials", adminKey, map[string]any{
		"apple_id": "", "apple_password": "",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty creds should be 400: %d %v", res.StatusCode, body)
	}

	// Valid creds accepted
	res, body = doJSON(t, "POST", "/api/v1/admin/apple-accounts/credentials", adminKey, map[string]any{
		"apple_id":        "test@example.com",
		"apple_password":  "supersecret",
		"two_factor_code": "123456",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("update creds failed: %d %v", res.StatusCode, body)
	}

	// Verify database record
	var label, authState string
	err := pool.QueryRow(t.Context(),
		`SELECT label, auth_state FROM apple_accounts WHERE label = 'test@example.com'`).Scan(&label, &authState)
	if err != nil {
		t.Fatalf("query apple_accounts: %v", err)
	}
	if label != "test@example.com" || authState != "authenticating" {
		t.Fatalf("unexpected account record: %q %q", label, authState)
	}
}

func TestAdminDeploy(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-deploy")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000991")
	artifactID, _ := uploadQuarantinedArtifact(t, "com.example.deployapp")

	// Missing fields
	res, _ := doJSON(t, "POST", "/api/v1/admin/deploy", adminKey, map[string]any{})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty deploy should be 400: %d", res.StatusCode)
	}

	// Successful deploy (auto-approves artifact + creates sign job)
	res, body := doJSON(t, "POST", "/api/v1/admin/deploy", adminKey, map[string]any{
		"artifact_id":  artifactID.String(),
		"device_id":    deviceID.String(),
		"auto_approve": true,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("admin deploy failed: %d %v", res.StatusCode, body)
	}
	signJobID, err := uuid.Parse(body["sign_job_id"].(string))
	if err != nil || signJobID == uuid.Nil {
		t.Fatalf("invalid sign_job_id: %v", body)
	}

	// Verify artifact quarantine_state changed to approved
	var state string
	err = pool.QueryRow(t.Context(), `SELECT quarantine_state FROM artifacts WHERE id = $1`, artifactID).Scan(&state)
	if err != nil || state != "approved" {
		t.Fatalf("artifact not approved: state=%q err=%v", state, err)
	}

	// Verify sign job created in DB
	var jobType string
	err = pool.QueryRow(t.Context(), `SELECT job_type FROM jobs WHERE id = $1`, signJobID).Scan(&jobType)
	if err != nil || jobType != "sign" {
		t.Fatalf("sign job not created: type=%q err=%v", jobType, err)
	}
}

func uploadQuarantinedArtifact(t *testing.T, bundleID string) (uuid.UUID, string) {
	t.Helper()
	ipa := testIPA(t, bundleID)
	res, body := uploadIPA(t, ipa)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", res.StatusCode, body)
	}
	id, _ := uuid.Parse(body["id"].(string))
	return id, body["sha256"].(string)
}
