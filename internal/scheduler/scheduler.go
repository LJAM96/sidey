// Package scheduler creates `refresh` jobs when a deployment's profile is
// about to expire (Phase I). It runs inside the control plane process; the
// actual signing/installation is performed by a refresh agent that claims the
// jobs through the regular job protocol.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sidey/internal/audit"
	"sidey/internal/jobs"
)

// DefaultLead is the default window (before profile expiry) in which a
// deployment is considered due for refresh. Free-team profiles are valid for
// 7 days, so the default schedules a refresh 2 days before expiry, leaving
// room for retries.
const DefaultLead = 48 * time.Hour

// expiryGuardLead is the hard safety net: if a profile is within this window
// of expiry and has no active (non-dead) refresh job, a refresh is forced
// regardless of the normal schedule. This prevents a broken refresh chain
// from letting the profile expire entirely.
const expiryGuardLead = 24 * time.Hour

// refreshMaxAttempts is the number of retry attempts given to scheduler-
// created refresh jobs. Higher than the default (5) because a refresh that
// fails today may succeed tomorrow once the root cause (auth, quota, tunnel)
// is resolved.
const refreshMaxAttempts = 15

// terminalBackoff is the minimum interval between automatic retries of a
// refresh cycle that died with a terminal error (auth, certificate). Without
// it the scheduler recovers the dead cycle on every tick, re-hammering Apple
// (or a broken tunnel) every few minutes forever: thousands of dead jobs and
// Apple-side throttling. With it the deployment is flagged for attention and
// retried at most once per window; a manual refresh always bypasses the wait.
const terminalBackoff = 6 * time.Hour

type dueDeployment struct {
	ID               uuid.UUID
	DeviceID         uuid.UUID
	Udid             string
	DeviceName       string
	DueAt            time.Time
	ExpiryAt         time.Time
	SourceArtifactID string
	SignedArtifactID string
	AccountID        string
	AppleID          string
}

type Service struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	jobs   *jobs.Service
	audit  *audit.Client
	lead   time.Duration
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger, jobService *jobs.Service, auditClient *audit.Client, lead time.Duration) *Service {
	return &Service{
		pool:   pool,
		logger: logger,
		jobs:   jobService,
		audit:  auditClient,
		lead:   lead,
		now:    time.Now,
	}
}

// dueDeployments returns deployments whose refresh is due (profile expiry is
// within the lead window, or already past) and whose device has an agent.
// Source IPA, installed derivative and Apple account resolve authoritatively
// through the installation record: deployments that cannot be resolved are
// left out here and flagged by markUnrefreshable instead of guessed.
func (s *Service) dueDeployments(ctx context.Context) ([]dueDeployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dep.id, dep.device_id, d.udid, COALESCE(d.device_name, ''),
		       COALESCE(dep.next_refresh_due_at, ir.provisioning_expiry_at - $1::interval) AS due_at,
		       COALESCE(ir.provisioning_expiry_at, dep.next_refresh_due_at + $1::interval) AS expiry_at,
		       sa.source_artifact_id::text AS source_artifact_id,
		       COALESCE(ir.signed_artifact_id::text, '') AS signed_artifact_id,
		       acc.id::text AS account_id,
		       COALESCE(acc.label, '') AS apple_id
		FROM deployments dep
		JOIN installation_records ir ON ir.deployment_id = dep.id
		JOIN devices d ON d.id = dep.device_id
		JOIN signed_artifacts sa ON sa.id = ir.signed_artifact_id
		JOIN apple_accounts acc ON acc.id = sa.account_id
		WHERE d.agent_id IS NOT NULL
		  AND COALESCE(dep.next_refresh_due_at, ir.provisioning_expiry_at - $1::interval) <= now()
		  AND ir.provisioning_expiry_at IS NOT NULL
		ORDER BY due_at`, s.lead.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dueDeployment
	for rows.Next() {
		var d dueDeployment
		var expiryAt *time.Time
		if err := rows.Scan(&d.ID, &d.DeviceID, &d.Udid, &d.DeviceName, &d.DueAt, &expiryAt,
			&d.SourceArtifactID, &d.SignedArtifactID, &d.AccountID, &d.AppleID); err != nil {
			return nil, err
		}
		if expiryAt != nil {
			d.ExpiryAt = *expiryAt
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// markUnrefreshable flags due deployments that cannot be resolved to a
// source IPA and Apple account (missing installation record linkage). They
// are never scheduled blindly; the dashboard surfaces them as needing
// attention (manual reassociation) instead.
func (s *Service) markUnrefreshable(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT dep.id
		FROM deployments dep
		JOIN installation_records ir ON ir.deployment_id = dep.id
		JOIN devices d ON d.id = dep.device_id
		LEFT JOIN signed_artifacts sa ON sa.id = ir.signed_artifact_id
		LEFT JOIN apple_accounts acc ON acc.id = sa.account_id
		WHERE d.agent_id IS NOT NULL
		  AND COALESCE(dep.next_refresh_due_at, ir.provisioning_expiry_at - $1::interval) <= now()
		  AND ir.provisioning_expiry_at IS NOT NULL
		  AND (ir.signed_artifact_id IS NULL OR sa.id IS NULL OR acc.id IS NULL)`, s.lead.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	const reason = "needs attention: deployment is not linked to a signed artifact and Apple account (re-associate instead of guessing)"
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE deployments SET
				last_refresh_at = now(),
				last_refresh_result = 'attention',
				last_refresh_error = $2
			WHERE id = $1 AND COALESCE(last_refresh_error, '') <> $2`, id, reason); err != nil {
			return err
		}
	}
	return rows.Err()
}

// tick inspects due deployments and enqueues one refresh job per refresh
// cycle. The idempotency key is derived from the deployment, the due date
// and the profile expiry date, so retries and repeated ticks never duplicate
// work within a cycle, while a completed refresh (which advances the expiry
// and therefore the key) starts a new cycle even if the next due date falls
// on the same calendar day.
//
// A dead refresh job holds its cycle's idempotency key forever (the due and
// expiry dates never advance), which would silently starve the deployment of
// all future refreshes. When the key conflicts with a dead job, the stale
// row is removed so the cycle can be retried.
//
// An expiry guard runs after the normal schedule: if a profile is within
// expiryGuardLead of expiry and has no active (non-dead) refresh job, a
// refresh is forced. This is the last line of defence against a broken
// refresh chain letting a profile expire entirely.
func (s *Service) tick(ctx context.Context) (int, error) {
	// Deployments that are due but cannot be resolved to a source IPA and
	// Apple account are flagged for manual reassociation, never scheduled
	// blindly.
	if err := s.markUnrefreshable(ctx); err != nil {
		s.logger.Warn("attention marking failed", "error", err)
	}
	due, err := s.dueDeployments(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, d := range due {
		created += s.createRefreshJob(ctx, d)
	}

	// Expiry guard: force-refresh deployments within expiryGuardLead of
	// expiry that have no live (non-dead) refresh job in flight.
	guarded, err := s.expiryGuard(ctx)
	if err != nil {
		s.logger.Warn("expiry guard failed", "error", err)
	} else {
		created += guarded
	}

	return created, nil
}

// createRefreshJob creates a single refresh job for a due deployment,
// handling dead-cycle recovery. Returns 1 if a job was created, 0 otherwise.
// A dead cycle that died terminally (auth, certificate) is retried at most
// once per terminalBackoff window and otherwise left flagged for attention,
// so the scheduler cannot hammer a broken dependency every tick.
// The job carries the authoritative workflow context (source IPA, previous
// derivative, Apple account) so no later stage has to guess it.
func (s *Service) createRefreshJob(ctx context.Context, d dueDeployment) int {
	dueDay := d.DueAt.UTC().Format("2006-01-02")
	expiryDay := d.ExpiryAt.UTC().Format("2006-01-02")
	key := fmt.Sprintf("refresh:%s:%s:%s", d.ID, dueDay, expiryDay)
	params, err := json.Marshal(map[string]any{
		"deployment_id":              d.ID.String(),
		"source_artifact_id":         d.SourceArtifactID,
		"previous_signed_artifact_id": d.SignedArtifactID,
		"apple_account_id":           d.AccountID,
		"apple_id":                   d.AppleID,
		"udid":                       d.Udid,
		"device_name":                d.DeviceName,
		"expiry_at":                  d.ExpiryAt.UTC().Format(time.RFC3339),
		"artifact_id":                d.SourceArtifactID,
	})
	if err != nil {
		s.logger.Warn("refresh params marshal failed", "deployment", d.ID, "error", err)
		return 0
	}
	job, err := s.jobs.CreateRefresh(ctx, "scheduler", &d.DeviceID, params, key, refreshMaxAttempts, "refresh")
	if err != nil {
		s.logger.Warn("refresh job creation failed", "deployment", d.ID, "error", err)
		return 0
	}
	if job != nil && job.State == "dead" {
		if terminal, summary := s.deadCycleTerminal(ctx, key); terminal {
			if s.attentionThrottled(ctx, d.ID) {
				// Still inside the backoff window: leave the dead row
				// holding the key so no new work is created this tick.
				return 0
			}
			s.flagAttention(ctx, d.ID, summary)
			// Fall through to a single recovery attempt; if the fresh
			// job dies terminally again it re-arms the backoff window.
		}
		if err := s.releaseDeadCycle(ctx, key); err != nil {
			s.logger.Warn("clearing dead refresh cycle failed",
				"deployment", d.ID, "key", key, "error", err)
			return 0
		}
		job, err = s.jobs.CreateRefresh(ctx, "scheduler", &d.DeviceID, params, key, refreshMaxAttempts, "refresh")
		if err != nil {
			s.logger.Warn("refresh job re-creation failed", "deployment", d.ID, "error", err)
			return 0
		}
		if job != nil && job.State == "dead" {
			return 0
		}
		s.logger.Info("refresh cycle recovered from dead job",
			"job", job.ID, "deployment", d.ID)
	}
	if job != nil {
		s.logger.Info("refresh scheduled",
			"job", job.ID, "deployment", d.ID, "udid", d.Udid,
			"expiry_at", d.ExpiryAt.Format(time.RFC3339), "due_at", d.DueAt.Format(time.RFC3339))
		return 1
	}
	return 0
}

// expiryGuard checks for deployments whose profile is within expiryGuardLead
// of expiry and has no live (non-dead, non-completed) refresh job. These
// deployments are at risk of expiring because the normal schedule didn't
// trigger early enough or all refresh attempts failed. The guard forces a
// new refresh job with a unique key so it doesn't collide with the normal
// schedule's idempotency.
func (s *Service) expiryGuard(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dep.id, dep.device_id, d.udid, COALESCE(d.device_name, ''),
		       ir.provisioning_expiry_at AS expiry_at,
		       sa.source_artifact_id::text AS source_artifact_id,
		       COALESCE(ir.signed_artifact_id::text, '') AS signed_artifact_id,
		       acc.id::text AS account_id,
		       COALESCE(acc.label, '') AS apple_id
		FROM deployments dep
		JOIN installation_records ir ON ir.deployment_id = dep.id
		JOIN devices d ON d.id = dep.device_id
		JOIN signed_artifacts sa ON sa.id = ir.signed_artifact_id
		JOIN apple_accounts acc ON acc.id = sa.account_id
		WHERE d.agent_id IS NOT NULL
		  AND ir.provisioning_expiry_at IS NOT NULL
		  AND ir.provisioning_expiry_at <= now() + $1::interval
		  AND NOT (
			dep.last_refresh_result = 'attention'
			AND dep.last_refresh_at > now() - $2::interval
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM jobs j2
			WHERE j2.device_id = dep.device_id
			  AND j2.job_type = 'refresh'
			  AND j2.state IN ('pending', 'claimed', 'in_progress')
		  )`, expiryGuardLead.String(), terminalBackoff.String())
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	created := 0
	for rows.Next() {
		var d dueDeployment
		if err := rows.Scan(&d.ID, &d.DeviceID, &d.Udid, &d.DeviceName, &d.ExpiryAt,
			&d.SourceArtifactID, &d.SignedArtifactID, &d.AccountID, &d.AppleID); err != nil {
			s.logger.Warn("expiry guard scan failed", "error", err)
			continue
		}
		// Use a key that differs from the normal schedule's key to avoid
		// collisions, but includes the day so it doesn't duplicate within
		// the same day.
		expiryDay := d.ExpiryAt.UTC().Format("2006-01-02")
		key := fmt.Sprintf("refresh-guard:%s:%s", d.ID, expiryDay)
		params, err := json.Marshal(map[string]any{
			"deployment_id":              d.ID.String(),
			"source_artifact_id":         d.SourceArtifactID,
			"previous_signed_artifact_id": d.SignedArtifactID,
			"apple_account_id":           d.AccountID,
			"apple_id":                   d.AppleID,
			"udid":                       d.Udid,
			"device_name":                d.DeviceName,
			"expiry_at":                  d.ExpiryAt.UTC().Format(time.RFC3339),
			"artifact_id":                d.SourceArtifactID,
		})
		if err != nil {
			continue
		}
		job, err := s.jobs.CreateRefresh(ctx, "scheduler", &d.DeviceID, params, key, refreshMaxAttempts, "refresh")
		if err != nil {
			s.logger.Warn("expiry guard job creation failed", "deployment", d.ID, "error", err)
			continue
		}
		if job != nil {
			created++
			s.logger.Warn("expiry guard: forced refresh",
				"job", job.ID, "deployment", d.ID,
				"expiry_at", d.ExpiryAt.Format(time.RFC3339),
				"remaining", time.Until(d.ExpiryAt).Round(time.Minute))
		}
	}
	return created, rows.Err()
}

// releaseDeadCycle deletes a refresh job that holds the given idempotency key
// but can never succeed. Deleting the row is safe: dead jobs are no longer
// claimed, and the installation record / deployment state it described is
// still authoritative for the next cycle.
func (s *Service) releaseDeadCycle(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE idempotency_key = $1 AND state = 'dead'`, key)

	return err
}

// deadCycleTerminal reports whether the dead job holding key died with a
// terminal error (one that blind retrying cannot fix: auth, certificate).
// The summary is a short human-readable description for attention flagging.
func (s *Service) deadCycleTerminal(ctx context.Context, key string) (bool, string) {
	var deadReason *string
	var errorDetails *string
	if err := s.pool.QueryRow(ctx, `
		SELECT dead_reason, error_details FROM jobs
		WHERE idempotency_key = $1 AND state = 'dead'`, key).Scan(&deadReason, &errorDetails); err != nil {
		return false, ""
	}
	if deadReason == nil || !strings.HasPrefix(*deadReason, "terminal_error:") {
		return false, ""
	}
	summary := *deadReason
	if errorDetails != nil && *errorDetails != "" {
		if detail := firstLine(*errorDetails); detail != "" {
			summary += ": " + detail
		}
		if len(summary) > 500 {
			summary = summary[:500]
		}
	}
	return true, summary
}

// attentionThrottled reports whether the deployment was recently flagged for
// attention, meaning an automatic terminal retry is still inside its backoff
// window and must wait. Manual refreshes bypass this entirely.
func (s *Service) attentionThrottled(ctx context.Context, deploymentID uuid.UUID) bool {
	var throttled bool
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(last_refresh_result = 'attention'
		                AND last_refresh_at > now() - $2::interval, false)
		FROM deployments WHERE id = $1`, deploymentID, terminalBackoff.String()).Scan(&throttled); err != nil {
		return false
	}
	return throttled
}

// flagAttention marks the deployment as needing operator attention with the
// terminal failure summary, starting (or restarting) the backoff window. A
// later successful refresh resets the result to 'ok'.
func (s *Service) flagAttention(ctx context.Context, deploymentID uuid.UUID, reason string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployments SET
			last_refresh_at = now(),
			last_refresh_result = 'attention',
			last_refresh_error = $2
		WHERE id = $1`, deploymentID, reason); err != nil {
		s.logger.Warn("attention flagging failed", "deployment", deploymentID, "error", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Run performs one scheduling pass and reports how many jobs were created.
// It is safe to call periodically.
func (s *Service) Run(ctx context.Context) (int, error) {
	return s.tick(ctx)
}
