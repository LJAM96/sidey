package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
	"sidey/internal/audit"
	"sidey/internal/jobs"
	"sidey/internal/storage"
)

// Integration tests against a real PostgreSQL database (sidey_test). Set
// DATABASE_URL or the suite is skipped. Migrations in ../../migrations are
// applied before the suite runs and tables are truncated between tests.

var (
	pool       *pgxpool.Pool
	server     *Server
	adminKey   = "test-admin-key"
	httpServer *httptest.Server
)

func TestMain(m *testing.M) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Println("skipping integration tests: DATABASE_URL not set")
		os.Exit(0)
	}
	ctx := context.Background()
	p, err := storage.Open(ctx, databaseURL)
	if err != nil {
		fmt.Println("database unavailable:", err)
		os.Exit(0)
	}
	applyMigrations(ctx, p)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auditClient := audit.New(p, logger)
	jobService := jobs.NewService(p, auditClient, 60*time.Second)
	server = NewServer(p, logger, auditClient, jobService, adminKey)
	httpServer = httptest.NewServer(server.Handler())
	pool = p

	code := m.Run()
	httpServer.Close()
	p.Close()
	os.Exit(code)
}

// applyMigrations runs the SQL files in ../../migrations in lexical order
// against the test database, tracking them in schema_migrations so repeated
// runs are safe. The real sidey database is never touched.
func applyMigrations(ctx context.Context, p *pgxpool.Pool) {
	_, err := p.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		panic(err)
	}
	migrationDir, err := filepath.Abs("../../migrations")
	if err != nil {
		panic(err)
	}
	entries, err := os.ReadDir(migrationDir)
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		var applied bool
		if err := p.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`,
			entry.Name()).Scan(&applied); err != nil {
			panic(err)
		}
		if applied {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(migrationDir, entry.Name()))
		if err != nil {
			panic(err)
		}
		if _, err := p.Exec(ctx, string(sql)); err != nil {
			panic(fmt.Sprintf("migration %s: %v", entry.Name(), err))
		}
		if _, err := p.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, entry.Name()); err != nil {
			panic(err)
		}
	}
}

func truncate(t *testing.T) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE audit_events, jobs, deployments, installation_records,
			signed_artifacts, artifacts, certificates, devices,
			agent_enrolment_tokens, agents, application_channels,
			applications, apple_accounts, users CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func doJSON(t *testing.T, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, httpServer.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func newEnrolmentToken(t *testing.T) string {
	t.Helper()
	res, body := doJSON(t, "POST", "/api/v1/admin/enrolment-tokens", adminKey, map[string]any{
		"label": "test-token",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d %v", res.StatusCode, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("empty token")
	}
	return token
}

func enrolAgent(t *testing.T, name string) (uuid.UUID, string) {
	t.Helper()
	token := newEnrolmentToken(t)
	res, body := doJSON(t, "POST", "/api/v1/agents/enrol", token, map[string]any{
		"name":            name,
		"architecture":    "aarch64",
		"operating_system": "linux",
		"software_version": "0.1.0",
		"capabilities":    map[string]any{"usb": true},
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("enrol: %d %v", res.StatusCode, body)
	}
	agentID, err := uuid.Parse(body["agent_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := body["api_key"].(string)
	if apiKey == "" {
		t.Fatal("empty api key")
	}
	return agentID, apiKey
}

func reportDevice(t *testing.T, agentKey, udid string) uuid.UUID {
	t.Helper()
	res, body := doJSON(t, "POST", "/api/v1/agents/me/devices", agentKey, map[string]any{
		"devices": []map[string]any{{
			"udid": udid, "platform": "ios", "device_name": "test phone",
			"model": "iPhone15,2", "os_version": "27.0",
			"pairing_status": "paired", "developer_mode_enabled": true,
		}},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("report devices: %d %v", res.StatusCode, body)
	}
	var deviceID uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM devices WHERE udid = $1`, udid).Scan(&deviceID)
	if err != nil {
		t.Fatal(err)
	}
	return deviceID
}

func createJob(t *testing.T, deviceID uuid.UUID, key, jobType string) uuid.UUID {
	t.Helper()
	res, body := doJSON(t, "POST", "/api/v1/jobs", adminKey, map[string]any{
		"job_type": jobType, "device_id": deviceID.String(),
		"parameters": map[string]any{"ipa_sha256": "abc"},
		"idempotency_key": key,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create job: %d %v", res.StatusCode, body)
	}
	id, err := uuid.Parse(body["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnrolmentFlow(t *testing.T) {
	truncate(t)
	agentID, apiKey := enrolAgent(t, "edge-1")

	res, body := doJSON(t, "POST", "/api/v1/agents/me/heartbeat", apiKey,
		map[string]any{"capabilities": map[string]any{"usb": true, "tailscale": true}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %v", res.StatusCode, body)
	}
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000001")

	var state string
	var reportedAgent *uuid.UUID
	err := pool.QueryRow(context.Background(), `
		SELECT a.connection_state, d.agent_id
		FROM agents a JOIN devices d ON d.agent_id = a.id
		WHERE a.id = $1`, agentID).Scan(&state, &reportedAgent)
	if err != nil {
		t.Fatal(err)
	}
	if state != "online" || reportedAgent == nil || *reportedAgent != agentID {
		t.Fatalf("unexpected state: state=%s agent=%v", state, reportedAgent)
	}
	if deviceID == uuid.Nil {
		t.Fatal("device not upserted")
	}
}

func TestTokenSingleUse(t *testing.T) {
	truncate(t)
	token := newEnrolmentToken(t)
	body := map[string]any{"name": "a"}
	res, _ := doJSON(t, "POST", "/api/v1/agents/enrol", token, body)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("first use: %d", res.StatusCode)
	}
	res, _ = doJSON(t, "POST", "/api/v1/agents/enrol", token, body)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second use should be rejected, got %d", res.StatusCode)
	}
}

func TestJobIdempotentCreate(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000002")

	first := createJob(t, deviceID, "same-key", "install")
	second := createJob(t, deviceID, "same-key", "install")
	if first != second {
		t.Fatalf("idempotency violated: %s != %s", first, second)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 job, got %d", count)
	}
}

func TestClaimAndComplete(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000003")
	jobID := createJob(t, deviceID, "claim-key", "verify")

	res, body := doJSON(t, "POST", "/api/v1/jobs/claim", apiKey, map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %v", res.StatusCode, body)
	}
	jobs := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 claimed job, got %d", len(jobs))
	}
	claimed := jobs[0].(map[string]any)
	if claimed["id"] != jobID.String() || claimed["state"] != "claimed" || claimed["attempt"].(float64) != 1 {
		t.Fatalf("unexpected claim payload: %v", claimed)
	}

	// Second claim must return nothing for the same device.
	res, body = doJSON(t, "POST", "/api/v1/jobs/claim", apiKey, map[string]any{})
	if len(body["jobs"].([]any)) != 0 {
		t.Fatalf("second claim should be empty: %v", body)
	}

	progress := 50
	res, body = doJSON(t, "POST", "/api/v1/jobs/"+jobID.String()+"/status", apiKey, map[string]any{
		"state": "in_progress", "progress": &progress,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("in_progress: %d %v", res.StatusCode, body)
	}
	if body["state"] != "in_progress" || body["progress"].(float64) != 50 {
		t.Fatalf("unexpected in_progress: %v", body)
	}
	if body["lease_expires_at"] == nil {
		t.Fatal("lease missing on in_progress job")
	}

	res, body = doJSON(t, "POST", "/api/v1/jobs/"+jobID.String()+"/status", apiKey, map[string]any{
		"state": "completed", "result": map[string]any{"verified": true},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("completed: %d %v", res.StatusCode, body)
	}
	if body["state"] != "completed" {
		t.Fatalf("expected completed, got %v", body)
	}
}

func TestForeignAgentCannotUpdate(t *testing.T) {
	truncate(t)
	_, apiKeyA := enrolAgent(t, "edge-a")
	_, apiKeyB := enrolAgent(t, "edge-b")
	deviceID := reportDevice(t, apiKeyA, "00008120-0000000000000004")
	jobID := createJob(t, deviceID, "foreign-key", "install")

	res, _ := doJSON(t, "POST", "/api/v1/jobs/claim", apiKeyA, map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatal("claim failed")
	}
	res, body := doJSON(t, "POST", "/api/v1/jobs/"+jobID.String()+"/status", apiKeyB, map[string]any{
		"state": "completed",
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("foreign agent update should conflict, got %d %v", res.StatusCode, body)
	}
}

func TestTwoAgentsCannotClaimSameDevice(t *testing.T) {
	truncate(t)
	_, apiKeyA := enrolAgent(t, "edge-a")
	_, apiKeyB := enrolAgent(t, "edge-b")
	deviceID := reportDevice(t, apiKeyA, "00008120-0000000000000005")
	createJob(t, deviceID, "exclusive-key", "install")

	// Agent B must not see the device owned by A, even by explicit id.
	res, body := doJSON(t, "POST", "/api/v1/jobs/claim", apiKeyB,
		map[string]any{"device_ids": []string{deviceID.String()}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim B: %d %v", res.StatusCode, body)
	}
	if len(body["jobs"].([]any)) != 0 {
		t.Fatalf("agent B claimed a job for agent A's device: %v", body)
	}

	res, body = doJSON(t, "POST", "/api/v1/jobs/claim", apiKeyA, map[string]any{})
	if res.StatusCode != http.StatusOK || len(body["jobs"].([]any)) != 1 {
		t.Fatalf("agent A claim: %d %v", res.StatusCode, body)
	}

	// Concurrent claims from two agents must never both succeed.
	createJob(t, deviceID, "exclusive-key-2", "install")
	type claimResult struct{ count int }
	results := make(chan claimResult, 2)
	for _, key := range []string{apiKeyA, apiKeyA} {
		key := key
		go func() {
			res, body := doJSON(t, "POST", "/api/v1/jobs/claim", key, map[string]any{})
			if res.StatusCode != http.StatusOK {
				results <- claimResult{-1}
				return
			}
			results <- claimResult{len(body["jobs"].([]any))}
		}()
	}
	total := 0
	for i := 0; i < 2; i++ {
		total += (<-results).count
	}
	if total != 1 {
		t.Fatalf("concurrent claims delivered %d jobs, expected 1", total)
	}
}

func TestLeaseExpiryReclaimsJob(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000006")
	jobID := createJob(t, deviceID, "lease-key", "install")

	res, _ := doJSON(t, "POST", "/api/v1/jobs/claim", apiKey, map[string]any{})
	if res.StatusCode != http.StatusOK {
		t.Fatal("claim failed")
	}

	// Simulate a worker crash: expire the lease immediately.
	if _, err := pool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}

	reaped, err := server.jobs.Reap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped job, got %d", reaped)
	}

	var state string
	var retryAt *time.Time
	var claimedBy *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT state, retry_at, claimed_by FROM jobs WHERE id = $1`, jobID).Scan(&state, &retryAt, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if state != jobs.StatePending || claimedBy != nil || retryAt == nil {
		t.Fatalf("job not reclaimed: state=%s claimed_by=%v retry_at=%v", state, claimedBy, retryAt)
	}

	// A new claim must succeed again after the retry delay.
	if _, err := pool.Exec(context.Background(),
		`UPDATE jobs SET retry_at = now() - interval '1 second' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	res, body := doJSON(t, "POST", "/api/v1/jobs/claim", apiKey, map[string]any{})
	if res.StatusCode != http.StatusOK || len(body["jobs"].([]any)) != 1 {
		t.Fatalf("reclaim after reap: %d %v", res.StatusCode, body)
	}
	if body["jobs"].([]any)[0].(map[string]any)["attempt"].(float64) != 2 {
		t.Fatalf("attempt should be 2 after reclaim, got %v", body["jobs"].([]any)[0].(map[string]any)["attempt"])
	}
}

func TestAuditTrailWritten(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "edge-1")
	deviceID := reportDevice(t, apiKey, "00008120-0000000000000007")
	createJob(t, deviceID, "audit-key", "install")

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action IN
		 ('enrolment_token.created', 'agent.enrolled', 'job.created')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 3 {
		t.Fatalf("expected at least 3 audit events, got %d", count)
	}
}

func TestAgentGoesOfflineWithoutHeartbeat(t *testing.T) {
	truncate(t)
	agentID, apiKey := enrolAgent(t, "edge-1")
	doJSON(t, "POST", "/api/v1/agents/me/heartbeat", apiKey, map[string]any{})

	if _, err := pool.Exec(context.Background(),
		`UPDATE agents SET last_heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.jobs.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT connection_state FROM agents WHERE id = $1`, agentID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "offline" {
		t.Fatalf("agent should be offline, got %q", state)
	}
}
