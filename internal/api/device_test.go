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
