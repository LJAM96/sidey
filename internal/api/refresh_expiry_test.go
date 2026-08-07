package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"sidey/internal/jobs"
)

// newDeployment plants an application, channel, deployment and installation
// record for a given device, returning the deployment id.
func newDeployment(t *testing.T, deviceID uuid.UUID, suffix string) uuid.UUID {
	t.Helper()
	appID := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO applications (id, name) VALUES ($1, $2)`, appID, "probe-"+suffix); err != nil {
		t.Fatal(err)
	}
	var channelID uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO application_channels (application_id, platform, release_channel)
		VALUES ($1, 'ios', 'stable') RETURNING id`, appID).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	depID := uuid.New()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO deployments (id, device_id, channel_id)
		VALUES ($1, $2, $3)`, depID, deviceID, channelID); err != nil {
		t.Fatal(err)
	}
	return depID
}

// completeRefresh claims and completes a refresh job in three steps, mimicking
// the refresh agent: in_progress, then completed with an optional result.
func completeRefresh(t *testing.T, apiKey string, deviceID, depID uuid.UUID, result any) {
	t.Helper()
	params, err := json.Marshal(map[string]any{"deployment_id": depID.String()})
	if err != nil {
		t.Fatal(err)
	}
	job, err := server.jobs.Create(t.Context(), "test", jobs.CreateRequest{
		JobType:        jobs.JobTypeRefresh,
		DeviceID:       &deviceID,
		Parameters:     params,
		IdempotencyKey: "test:" + depID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := serverAgentID(t, apiKey)
	if _, err := server.jobs.Claim(t.Context(), agent, []uuid.UUID{deviceID}, []string{"refresh"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := server.jobs.Update(t.Context(), agent, job.ID, jobs.UpdateRequest{State: jobs.StateInProgress}); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if result != nil {
		raw, err = json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := jobs.UpdateRequest{State: jobs.StateCompleted}
	if len(raw) > 0 {
		req.Result = raw
	}
	if _, err := server.jobs.Update(t.Context(), agent, job.ID, req); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshCompletedUsesReportedExpiry covers the F2 correctness change:
// a refresh job that reports the real new provisioning expiry (read from the
// signed bundle after a wireless install) drives the deployment's scheduling
// and the installation record, instead of the assumed 7-day window.
func TestRefreshCompletedUsesReportedExpiry(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "refresh-1")
	deviceID := reportDevice(t, apiKey, "00008120-40000000000201")
	depID := newDeployment(t, deviceID, "expiry")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO installation_records (deployment_id, provisioning_expiry_at)
		VALUES ($1, $2)`, depID, time.Now()); err != nil {
		t.Fatal(err)
	}

	reported := time.Now().UTC().Add(96 * time.Hour).Truncate(time.Second)
	completeRefresh(t, apiKey, deviceID, depID, map[string]any{
		"verified":          true,
		"duration_seconds":  42,
		"profile_expiry_at": reported.Format(time.RFC3339),
	})

	// The installation record must reflect the reported expiry, and the next
	// refresh must be scheduled lead-window before it.
	var storedExpiry time.Time
	var nextDue time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT ir.provisioning_expiry_at, dep.next_refresh_due_at
		 FROM installation_records ir JOIN deployments dep ON dep.id = ir.deployment_id
		 WHERE ir.deployment_id = $1`, depID).Scan(&storedExpiry, &nextDue); err != nil {
		t.Fatal(err)
	}
	if !storedExpiry.Equal(reported) {
		t.Fatalf("recorded expiry %s != reported %s", storedExpiry, reported)
	}
	wantDue := reported.Add(-48 * time.Hour)
	if !nextDue.Equal(wantDue) {
		t.Fatalf("next due %s != %s", nextDue, wantDue)
	}
}

// TestRefreshFallbackWindowWithoutExpiry covers the legacy path: a refresh
// completing without a reported profile expiry falls back to the assumed
// free-team validity window (7 days) so the deployment is still scheduled.
func TestRefreshFallbackWindowWithoutExpiry(t *testing.T) {
	truncate(t)
	_, apiKey := enrolAgent(t, "refresh-2")
	deviceID := reportDevice(t, apiKey, "00008120-40000000000202")
	depID := newDeployment(t, deviceID, "fallback")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO installation_records (deployment_id, provisioning_expiry_at)
		VALUES ($1, now())`, depID); err != nil {
		t.Fatal(err)
	}

	completeRefresh(t, apiKey, deviceID, depID, nil)

	var storedExpiry time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT provisioning_expiry_at FROM installation_records WHERE deployment_id = $1`,
		depID).Scan(&storedExpiry); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if storedExpiry.Before(now.Add(6*24*time.Hour)) || storedExpiry.After(now.Add(8*24*time.Hour)) {
		t.Fatalf("fallback expiry %s outside assumed 7d window %s..%s",
			storedExpiry, now.Add(6*24*time.Hour), now.Add(8*24*time.Hour))
	}
}

func serverAgentID(t *testing.T, apiKey string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT id FROM agents ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	return id
}