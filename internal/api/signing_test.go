package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"sidey/internal/jobs"
)

// newApprovedArtifact uploads an IPA (with a unique bundle id) and approves
// it through quarantine.
func newApprovedArtifact(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	ipa := testIPA(t, fmt.Sprintf("com.example.Sign.%d", time.Now().UnixNano()))
	res, body := uploadIPA(t, ipa)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %v", res.StatusCode, body)
	}
	id, _ := uuid.Parse(body["id"].(string))
	res, _ = doJSON(t, "PATCH", "/api/v1/artifacts/"+id.String(), adminKey, map[string]any{"state": "approved"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	sha := sha256.Sum256(ipa)
	return id, hex.EncodeToString(sha[:])
}

// TestSignJobGating covers Phase F: sign jobs require an approved artifact
// and an existing device, and are refused otherwise.
func TestSignJobGating(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000101")
	artifactID, _ := newApprovedArtifact(t)

	// Missing fields are rejected.
	res, body := doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty sign job: %d %v", res.StatusCode, body)
	}

	// A quarantined artifact must be refused even if approved later.
	quarantinedIPA := testIPA(t, "com.example.Quarantine")
	res, body = uploadIPA(t, quarantinedIPA)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("quarantined upload: %d %v", res.StatusCode, body)
	}
	qID, _ := uuid.Parse(body["id"].(string))
	res, body = doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": qID.String(), "device_id": deviceID.String(),
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("sign job on quarantined artifact: %d %v", res.StatusCode, body)
	}

	// A sign job for a missing device is refused.
	res, body = doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": artifactID.String(), "device_id": uuid.Nil.String(),
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("sign job on missing device: %d %v", res.StatusCode, body)
	}

	// Valid creation.
	res, body = doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": artifactID.String(), "device_id": deviceID.String(),
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create sign job: %d %v", res.StatusCode, body)
	}
	jobID, _ := uuid.Parse(body["id"].(string))
	if body["job_type"] != "sign" {
		t.Fatalf("job_type = %v", body["job_type"])
	}

	// Idempotent: same artifact+device returns the same job.
	res, body2 := doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": artifactID.String(), "device_id": deviceID.String(),
	})
	if res.StatusCode != http.StatusCreated || body2["id"] != jobID.String() {
		t.Fatalf("sign job not idempotent: %d %v", res.StatusCode, body2)
	}
}

// TestSignJobClaimedBySigningAgent covers the claim-side of Phase F: a sign
// job is only delivered to agents that ask for job_types=["sign"], while the
// same job stays invisible to refresh agents.
func TestSignJobClaimedBySigningAgent(t *testing.T) {
	truncate(t)
	_, refreshKey := enrolAgent(t, "refresh-1")
	_, signKey := enrolAgent(t, "signing-1")
	deviceID := reportDevice(t, refreshKey, "00008120-0000000000000102")
	artifactID, _ := newApprovedArtifact(t)

	res, body := doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": artifactID.String(), "device_id": deviceID.String(),
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create sign job: %d %v", res.StatusCode, body)
	}

	// The refresh agent claiming without job_types must not receive it.
	res, body = doJSON(t, "POST", "/api/v1/jobs/claim", refreshKey, map[string]any{})
	if res.StatusCode != http.StatusOK || len(body["jobs"].([]any)) != 0 {
		t.Fatalf("refresh agent should not get sign jobs: %d %v", res.StatusCode, body)
	}

	// The signing worker claiming job_types=["sign"] gets it, even though the
	// device belongs to the refresh agent.
	res, body = doJSON(t, "POST", "/api/v1/jobs/claim", signKey, map[string]any{
		"job_types": []string{"sign"}, "limit": 1,
	})
	if res.StatusCode != http.StatusOK || len(body["jobs"].([]any)) != 1 {
		t.Fatalf("signing agent claim: %d %v", res.StatusCode, body)
	}
	if body["jobs"].([]any)[0].(map[string]any)["job_type"] != "sign" {
		t.Fatalf("claimed job type: %v", body["jobs"].([]any)[0])
	}
}

// TestSignJobReapedLikeRefresh covers the reaper: failed sign jobs are
// re-queued just like refresh jobs.
func TestSignJobReapedLikeRefresh(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "signing-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000103")
	artifactID, _ := newApprovedArtifact(t)

	res, body := doJSON(t, "POST", "/api/v1/sign-jobs", adminKey, map[string]any{
		"artifact_id": artifactID.String(), "device_id": deviceID.String(),
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create sign job: %d %v", res.StatusCode, body)
	}
	jobID, _ := uuid.Parse(body["id"].(string))

	if _, err := pool.Exec(t.Context(),
		`UPDATE jobs SET state = 'failed', error_category = 'auth' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	reaped, err := server.jobs.Reap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped sign job, got %d", reaped)
	}
	var state string
	if err := pool.QueryRow(t.Context(),
		`SELECT state FROM jobs WHERE id = $1`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != jobs.StatePending {
		t.Fatalf("sign job not re-queued, state=%s", state)
	}
}

// TestAgentArtifactDownloadAuthz covers the agent-facing download endpoint:
// only approved artifacts are downloadable, and only with agent auth.
func TestAgentArtifactDownloadAuthz(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	quarantinedID, _ := newApprovedArtifact(t)
	approvedID, _ := newApprovedArtifact(t)

	// Quarantine the first one so it cannot be downloaded. It must stay
	// distinct from the second artifact (different bundle ids).
	res, _ := doJSON(t, "PATCH", "/api/v1/artifacts/"+quarantinedID.String(), adminKey, map[string]any{"state": "quarantined"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-quarantine: %d", res.StatusCode)
	}
	if quarantinedID == approvedID {
		t.Fatal("artifacts collided: identical upload bytes deduped")
	}

	req, _ := http.NewRequest("GET",
		httpServer.URL+"/api/v1/agents/artifacts/"+quarantinedID.String()+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res2, _ := http.DefaultClient.Do(req)
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("quarantined download should be forbidden, got %d", res2.StatusCode)
	}
	res2.Body.Close()

	req, _ = http.NewRequest("GET",
		httpServer.URL+"/api/v1/agents/artifacts/"+approvedID.String()+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res2, _ = http.DefaultClient.Do(req)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("approved download: %d", res2.StatusCode)
	}
	res2.Body.Close()

	// No auth: refused.
	req, _ = http.NewRequest("GET",
		httpServer.URL+"/api/v1/agents/artifacts/"+approvedID.String()+"/download", nil)
	res2, _ = http.DefaultClient.Do(req)
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated download: %d", res2.StatusCode)
	}
	res2.Body.Close()
}

// TestSignedArtifactUpload covers the worker->control-plane handoff: the
// multipart upload verifies the declared sha256, records the signed
// derivative and upserts the account and certificate.
func TestSignedArtifactUpload(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "signing-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000104")
	sourceID, _ := newApprovedArtifact(t)

	signed := testIPA(t, "com.example.Sign.SIGNED")
	sha := sha256.Sum256(signed)
	sha256hex := hex.EncodeToString(sha[:])

	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	fw, err := w.CreateFormFile("ipa", "signed.ipa")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(signed)
	for k, v := range map[string]string{
		"source_artifact_id":       sourceID.String(),
		"device_id":                deviceID.String(),
		"account_email":            "signer@example.com",
		"team_id":                  "TEAM123",
		"cert_serial":              "DEADBEEF",
		"profile_expiry_at":        "2026-08-14T15:07:45Z",
		"signed_bundle_identifier": "com.example.Sign.SIGNED",
		"signed_ipa_sha256":        sha256hex,
		"device_count":             "1",
		"app_id_count":             "2",
	} {
		w.WriteField(k, v)
	}
	w.Close()

	req, err := http.NewRequest("POST", httpServer.URL+"/api/v1/signed-artifacts", &form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("signed upload: %d %v", res.StatusCode, parsed)
	}

	// The account was upserted by team identifier with slot counts.
	var label, teamID string
	var appIDs, devices int
	if err := pool.QueryRow(t.Context(),
		`SELECT label, team_identifier, registered_app_id_count, registered_device_count
		 FROM apple_accounts`).Scan(&label, &teamID, &appIDs, &devices); err != nil {
		t.Fatal(err)
	}
	if label != "signer@example.com" || teamID != "TEAM123" || appIDs != 2 || devices != 1 {
		t.Fatalf("account upsert: %q %q %d %d", label, teamID, appIDs, devices)
	}

	// The certificate was upserted by serial.
	var certSerial, keyRef string
	var revoked bool
	if err := pool.QueryRow(t.Context(),
		`SELECT serial_number, key_ref, revoked FROM certificates`).Scan(&certSerial, &keyRef, &revoked); err != nil {
		t.Fatal(err)
	}
	if certSerial != "DEADBEEF" || keyRef != "signing-worker" || revoked {
		t.Fatalf("cert upsert: %q %q %v", certSerial, keyRef, revoked)
	}

	// The signed artifact is recorded and listed on the dashboard.
	var signedID string
	if err := pool.QueryRow(t.Context(),
		`SELECT id FROM signed_artifacts`).Scan(&signedID); err != nil {
		t.Fatal(err)
	}
	if parsed["id"] != signedID {
		t.Fatalf("uploaded id %v != row %s", parsed["id"], signedID)
	}
	res, dash := doJSON(t, "GET", "/api/v1/dashboard/signed-artifacts", adminKey, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d %v", res.StatusCode, dash)
	}
	found := false
	for _, r := range dash["rows"].([]any) {
		row := r.(map[string]any)
		if row["signed_ipa_sha256"] == sha256hex {
			found = true
		}
	}
	if !found {
		t.Error("signed artifact not listed on dashboard")
	}

	// A mismatching sha256 declaration is rejected.
	req, _ = http.NewRequest("POST", httpServer.URL+"/api/v1/signed-artifacts", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res2, _ := http.DefaultClient.Do(req)
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed upload: %d", res2.StatusCode)
	}
	res2.Body.Close()
}
