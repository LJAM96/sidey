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

type dueDeployment struct {
	ID           uuid.UUID
	DeviceID     uuid.UUID
	Udid         string
	DeviceName   string
	DueAt        time.Time
	ExpiryAt     time.Time
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
func (s *Service) dueDeployments(ctx context.Context) ([]dueDeployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dep.id, dep.device_id, d.udid, COALESCE(d.device_name, ''),
		       COALESCE(dep.next_refresh_due_at, ir.provisioning_expiry_at - $1::interval) AS due_at,
		       COALESCE(ir.provisioning_expiry_at, dep.next_refresh_due_at + $1::interval) AS expiry_at
		FROM deployments dep
		JOIN installation_records ir ON ir.deployment_id = dep.id
		JOIN devices d ON d.id = dep.device_id
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
		if err := rows.Scan(&d.ID, &d.DeviceID, &d.Udid, &d.DeviceName, &d.DueAt, &expiryAt); err != nil {
			return nil, err
		}
		if expiryAt != nil {
			d.ExpiryAt = *expiryAt
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// tick inspects due deployments and enqueues one refresh job per refresh
// cycle. The idempotency key is derived from the deployment, the due date
// and the profile expiry date, so retries and repeated ticks never duplicate
// work within a cycle, while a completed refresh (which advances the expiry
// and therefore the key) starts a new cycle even if the next due date falls
// on the same calendar day.
func (s *Service) tick(ctx context.Context) (int, error) {
	due, err := s.dueDeployments(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, d := range due {
		dueDay := d.DueAt.UTC().Format("2006-01-02")
		expiryDay := d.ExpiryAt.UTC().Format("2006-01-02")
		key := fmt.Sprintf("refresh:%s:%s:%s", d.ID, dueDay, expiryDay)
		params, err := json.Marshal(map[string]any{
			"deployment_id": d.ID.String(),
			"udid":          d.Udid,
			"device_name":   d.DeviceName,
			"expiry_at":     d.ExpiryAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return created, err
		}
		job, err := s.jobs.Create(ctx, "scheduler", jobs.CreateRequest{
			JobType:        jobs.JobTypeRefresh,
			DeviceID:       &d.DeviceID,
			Parameters:     params,
			IdempotencyKey: key,
		})
		if err != nil {
			s.logger.Warn("refresh job creation failed", "deployment", d.ID, "error", err)
			continue
		}
		if job != nil {
			created++
			s.logger.Info("refresh scheduled",
				"job", job.ID, "deployment", d.ID, "udid", d.Udid,
				"expiry_at", d.ExpiryAt.Format(time.RFC3339), "due_at", d.DueAt.Format(time.RFC3339))
		}
	}
	return created, nil
}

// Run performs one scheduling pass and reports how many jobs were created.
// It is safe to call periodically.
func (s *Service) Run(ctx context.Context) (int, error) {
	return s.tick(ctx)
}
