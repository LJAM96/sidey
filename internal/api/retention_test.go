package api

import (
	"bytes"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"sidey/internal/artifacts"
	"sidey/internal/audit"
	"sidey/internal/retention"
)

// TestPruneSignedArtifacts covers signed-derivative retention: rows whose
// embedded profile expired beyond the retention window are deleted together
// with their files, while still-valid derivatives and the originals survive.
func TestPruneSignedArtifacts(t *testing.T) {
	truncate(t)

	store := artifacts.NewStore(t.TempDir())
	if err := store.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	auditClient := audit.New(pool, logger)

	// One original artifact (immutable, never pruned).
	origSha, err := store.Save(bytes.NewReader([]byte("original-ipa-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	var sourceID uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO artifacts (sha256, filename, quarantine_state)
		VALUES ($1, 'orig.ipa', 'approved') RETURNING id`, origSha).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}

	var accountID, deviceID uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO apple_accounts (label, team_identifier) VALUES ('ret@example.com', 'TEAMRET')
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO devices (udid, platform, device_name)
		VALUES ('00008120-50000000000301', 'ios', 'retention-probe') RETURNING id`).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}

	addSigned := func(bundleID string, expiry time.Time) string {
		t.Helper()
		sha, err := store.Save(bytes.NewReader([]byte("signed-" + bundleID)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(t.Context(), `
			INSERT INTO signed_artifacts (source_artifact_id, device_id, account_id,
				signed_bundle_identifier, profile_expiry_at, signed_ipa_sha256)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			sourceID, deviceID, accountID, bundleID, expiry, sha)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}

	expiredLongAgo := addSigned("stale", time.Now().UTC().Add(-40*24*time.Hour))
	stillValid := addSigned("fresh", time.Now().UTC().Add(2*24*time.Hour))

	// An expired derivative sharing its file with a still-valid one must
	// retain the file even after its row is pruned (content addressing).
	shared := addSigned("shared-stale", time.Now().UTC().Add(-40*24*time.Hour))
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO signed_artifacts (source_artifact_id, device_id, account_id,
			signed_bundle_identifier, profile_expiry_at, signed_ipa_sha256)
		VALUES ($1, $2, $3, 'com.example.shared-fresh', now() + interval '2 days', $4)`,
		sourceID, deviceID, accountID, shared); err != nil {
		t.Fatal(err)
	}

	pruned, err := retention.PruneSignedArtifacts(t.Context(), pool, auditClient, store, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned %d rows, want 2", pruned)
	}

	if store.Exists(expiredLongAgo) {
		t.Error("stale signed file was not removed")
	}
	if !store.Exists(stillValid) || !store.Exists(shared) {
		t.Error("still-valid signed file was removed")
	}
	if !store.Exists(origSha) {
		t.Error("original artifact file was removed")
	}
	var remaining int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM signed_artifacts`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining signed rows %d, want 2", remaining)
	}
	_ = os.PathSeparator
}

func bytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}