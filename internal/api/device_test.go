package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// deviceServer exercises the same-host device service handler directly,
// without bearer credentials, mirroring what a Unix-socket client sees.
var deviceHTTPServer *httptest.Server

// deviceDo issues an unauthenticated request against the device handler.
func deviceDo(t *testing.T, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, deviceHTTPServer.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return res, parsed
}

// sentinelAgentID loads the id of the row keyed by the local device service
// sentinel, if it exists.
func sentinelAgentID(t *testing.T) (id *uuid.UUID) {
	t.Helper()
	var raw uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM agents WHERE api_key_id = $1`, localDeviceServiceSentinel).Scan(&raw)
	if err != nil {
		return nil
	}
	return &raw
}

func TestDeviceServiceChannel(t *testing.T) {
	truncate(t)

	// Health probe it is reachable without credentials.
	res, body := deviceDo(t, "GET", "/api/v1/device/health", nil)
	if res.StatusCode != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("device health: %d %v", res.StatusCode, body)
	}

	// The node row is provisioned lazily on first use.
	res, body = deviceDo(t, "GET", "/api/v1/device/me", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("device me: %d %v", res.StatusCode, body)
	}
	if body["role"] != deviceServiceRole || body["name"] != deviceServiceNodeName {
		t.Fatalf("unexpected node identity: %v", body)
	}
	nodeID := sentinelAgentID(t)
	if nodeID == nil {
		t.Fatal("device service node not provisioned")
	}

	// Repeated calls (simulating restarts) must not duplicate the row.
	deviceDo(t, "GET", "/api/v1/device/me", nil)
	deviceDo(t, "GET", "/api/v1/device/me", nil)
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE api_key_id = $1`, localDeviceServiceSentinel).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 device service node, got %d", count)
	}

	// Devices reported over the socket are owned by the node, and a
	// device-scoped job for them can be claimed and completed over the
	// socket -- the same handlers the remote agents drive.
	deviceDo(t, "POST", "/api/v1/device/me/devices", map[string]any{
		"devices": []map[string]any{{
			"udid": "00008120-0000000000000901", "platform": "ios",
			"device_name": "socket phone", "model": "iPhone15,2",
			"os_version": "27.0", "pairing_status": "paired",
		}},
	})
	var deviceID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM devices WHERE udid = '00008120-0000000000000901'`).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	createJob(t, deviceID, "device-channel-key", "install")

	res, body = deviceDo(t, "POST", "/api/v1/device/jobs/claim", map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("socket claim: %d %v", res.StatusCode, body)
	}
	jobs := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 claimed job over socket, got %d", len(jobs))
	}
	jobID := jobs[0].(map[string]any)["id"].(string)
	res, body = deviceDo(t, "POST", "/api/v1/device/jobs/"+jobID+"/status", map[string]any{
		"state": "completed", "result": map[string]any{"installed": true},
	})
	if res.StatusCode != http.StatusOK || body["state"] != "completed" {
		t.Fatalf("socket job complete: %d %v", res.StatusCode, body)
	}

	// The signed-artifact download endpoint is reachable for the node too.
	var nodeID2 uuid.UUID
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM agents WHERE api_key_id = $1`, localDeviceServiceSentinel).Scan(&nodeID2)
	var gotAgent uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT agent_id FROM devices WHERE id = $1`, deviceID).Scan(&gotAgent); err != nil {
		t.Fatal(err)
	}
	if gotAgent != nodeID2 {
		t.Fatalf("device not owned by the socket node: agent=%s", gotAgent)
	}
}

func TestDeviceServiceRoleGating(t *testing.T) {
	truncate(t)
	deviceDo(t, "POST", "/api/v1/device/me/devices", map[string]any{
		"devices": []map[string]any{{
			"udid": "00008120-0000000000000902", "platform": "ios",
		}},
	})
	var deviceID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM devices WHERE udid = '00008120-0000000000000902'`).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	createJob(t, deviceID, "device-refresh-key", "refresh")
	createJob(t, deviceID, "device-sign-key", "sign")

	// The device service performs refreshes in the consolidated model, so its
	// own refresh job is claimable.
	res, body := deviceDo(t, "POST", "/api/v1/device/jobs/claim", map[string]any{
		"job_types": []string{"refresh"},
	})
	if res.StatusCode != http.StatusOK || len(body["jobs"].([]any)) != 1 {
		t.Fatalf("device service must claim its own refresh, got %d %v", res.StatusCode, body)
	}

	// sign is reserved for the signing worker: the device service is refused.
	res, body = deviceDo(t, "POST", "/api/v1/device/jobs/claim", map[string]any{
		"job_types": []string{"sign"},
	})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("device service must not claim sign jobs, got %d %v", res.StatusCode, body)
	}
}

func TestDeviceServiceSentinelCannotAuthenticateOverHTTP(t *testing.T) {
	truncate(t)
	// The public API must reject the sentinel as a bearer key: the fast path
	// matches on auth.KeyID (a sha256 hex string, never the sentinel) and the
	// legacy scan skips rows with a non-null api_key_id, so the node row
	// cannot be reached without the Unix socket.
	res, _ := doJSON(t, "POST", "/api/v1/agents/me/heartbeat", localDeviceServiceSentinel, map[string]any{})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sentinel must not authenticate over HTTP, got %d", res.StatusCode)
	}
}

func TestDeviceServiceEmptyDeviceReporting(t *testing.T) {
	truncate(t)
	res, body := deviceDo(t, "POST", "/api/v1/device/me/devices", map[string]any{
		"devices": []map[string]any{},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("empty device reporting failed: %d %v", res.StatusCode, body)
	}
	if body["reported"] != float64(0) {
		t.Fatalf("expected reported = 0, got %v", body["reported"])
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM devices`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 devices in database, got %d", count)
	}
}

// TestDeviceRefreshWorkflow covers the refresh parent orchestration over the
// device socket: sign ensure is idempotent per refresh, signed derivatives
// are rejected as refresh inputs, and the install child is created once
// (deterministic key) then requeued — never duplicated — across retries.
func TestDeviceRefreshWorkflow(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	deviceDo(t, "GET", "/api/v1/device/me", nil)
	nodeID := sentinelAgentID(t)
	if nodeID == nil {
		t.Fatal("device service node not provisioned")
	}
	deviceDo(t, "POST", "/api/v1/device/me/devices", map[string]any{
		"devices": []map[string]any{{
			"udid": "00008120-0000000000000902", "platform": "ios",
			"device_name": "refresh phone", "model": "iPhone15,2",
			"os_version": "27.0", "pairing_status": "paired",
		}},
	})
	var deviceID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM devices WHERE udid = '00008120-0000000000000902'`).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	sourceID, _ := newApprovedArtifactWithBundle(t, "com.example.Refresh")

	var accountID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO apple_accounts (label, team_identifier, auth_state)
		 VALUES ('refresh-acct@example.com', 'REFRESHTEAM', 'authenticated')
		 RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	var appID, chanID, depID, signedID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO applications (name) VALUES ('RefreshApp') RETURNING id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO application_channels (application_id, platform) VALUES ($1, 'ios') RETURNING id`,
		appID).Scan(&chanID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deployments (device_id, channel_id) VALUES ($1, $2) RETURNING id`,
		deviceID, chanID).Scan(&depID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO signed_artifacts (source_artifact_id, device_id, account_id,
			signed_bundle_identifier, profile_expiry_at, signed_ipa_sha256)
		VALUES ($1, $2, $3, 'com.example.Refresh.REFRESHTEAM', now() + interval '7 days', 'abc')
		RETURNING id`, sourceID, deviceID, accountID).Scan(&signedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installation_records (deployment_id, provisioning_expiry_at,
			signed_artifact_id, installed_version)
		VALUES ($1, now() + interval '7 days', $2, '1.0')`, depID, signedID); err != nil {
		t.Fatal(err)
	}

	newRefresh := func(key string, extra map[string]any) uuid.UUID {
		t.Helper()
		paramsMap := map[string]any{
			"deployment_id": depID.String(), "source_artifact_id": sourceID.String(),
			"apple_id": "refresh-acct@example.com", "artifact_id": sourceID.String(),
			"udid": "00008120-0000000000000902",
		}
		for k, v := range extra {
			if v == nil {
				delete(paramsMap, k)
			} else {
				paramsMap[k] = v
			}
		}
		params, _ := json.Marshal(paramsMap)
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO jobs (job_type, device_id, parameters, idempotency_key,
				state, claimed_by, purpose)
			VALUES ('refresh', $1, $2, $3, 'claimed', $4, 'refresh')
			RETURNING id`, deviceID, params, key, *nodeID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	refreshID := newRefresh("test-refresh-1", nil)

	// Sign ensure returns a sign job, idempotently.
	res, body := deviceDo(t, "POST", "/api/v1/device/refresh/"+refreshID.String()+"/sign", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("refresh sign: %d %v", res.StatusCode, body)
	}
	signID, _ := uuid.Parse(body["sign_job_id"].(string))
	if body["apple_id"] != "refresh-acct@example.com" {
		t.Fatalf("expected threaded apple id, got %v", body)
	}
	res, body2 := deviceDo(t, "POST", "/api/v1/device/refresh/"+refreshID.String()+"/sign", nil)
	if res.StatusCode != http.StatusOK || body2["sign_job_id"] != body["sign_job_id"] {
		t.Fatalf("sign ensure not idempotent: %d %v", res.StatusCode, body2)
	}

	// A signed derivative as refresh input is rejected, not re-signed.
	// (Legacy poisoned shape: artifact_id only, no source_artifact_id.)
	badRefresh := newRefresh("test-refresh-2", map[string]any{
		"source_artifact_id": nil, "artifact_id": signedID.String(),
	})
	res, body = deviceDo(t, "POST", "/api/v1/device/refresh/"+badRefresh.String()+"/sign", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("derivative refresh input should be rejected: %d %v", res.StatusCode, body)
	}

	// Complete the sign with a worker-style result, then the install child
	// is created once, claimed, and stable across calls.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET state = 'completed',
		 result = $2 WHERE id = $1`,
		signID, `{"signed_artifact_id": "`+signedID.String()+`"}`); err != nil {
		t.Fatal(err)
	}
	res, install := deviceDo(t, "POST", "/api/v1/device/refresh/"+refreshID.String()+"/install", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ensure install: %d %v", res.StatusCode, install)
	}
	installID, _ := uuid.Parse(install["id"].(string))
	if install["state"] != "claimed" {
		t.Fatalf("install child should be claimed, got %v", install["state"])
	}
	if install["parent_job_id"] != refreshID.String() {
		t.Fatalf("install child missing parent link: %v", install)
	}
	res, install2 := deviceDo(t, "POST", "/api/v1/device/refresh/"+refreshID.String()+"/install", nil)
	if res.StatusCode != http.StatusOK || install2["id"] != install["id"] {
		t.Fatalf("install ensure not stable: %d %v", res.StatusCode, install2)
	}

	// A failed install is requeued under the same id, never duplicated.
	res, _ = deviceDo(t, "POST", "/api/v1/device/jobs/"+installID.String()+"/status", map[string]any{
		"state": "failed", "error_category": "install_failed", "error_details": "tunnel down",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fail install: %d", res.StatusCode)
	}
	res, install3 := deviceDo(t, "POST", "/api/v1/device/refresh/"+refreshID.String()+"/install", nil)
	if res.StatusCode != http.StatusOK || install3["id"] != install["id"] {
		t.Fatalf("failed install not requeued under same id: %d %v", res.StatusCode, install3)
	}
	if install3["state"] != "claimed" {
		t.Fatalf("requeued install should be claimed, got %v", install3["state"])
	}
	var installCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE parent_job_id = $1 AND job_type = 'install'`,
		refreshID).Scan(&installCount); err != nil {
		t.Fatal(err)
	}
	if installCount != 1 {
		t.Fatalf("expected exactly 1 install child, got %d", installCount)
	}
}
