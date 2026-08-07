// Package jobs implements the job queue backed by PostgreSQL.
//
// Lifecycle: pending -> claimed (lease granted) -> in_progress -> completed
// or failed. A job whose lease expires is reset to pending with a retry
// delay, so restarting the API or a worker never loses an in progress job.
// Claims are serialised per device with SELECT ... FOR UPDATE on the device
// row, so two agents can never execute conflicting jobs for the same device.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
	"sidey/internal/audit"
)

const (
	StatePending    = "pending"
	StateClaimed    = "claimed"
	StateInProgress = "in_progress"
	StateCompleted  = "completed"
	StateFailed     = "failed"
)

// JobTypeRefresh identifies refresh jobs created by the scheduler (Phase I).
// The refresh agent claims them, re-signs the app and reports back; the
// completion hook then reschedules the next refresh from the new expiry.
const JobTypeRefresh = "refresh"

// refreshProfileValidity is the assumed validity of a freshly issued free-team
// provisioning profile (7 days). The refresh agent does not report the exact
// new expiry today, so the completion hook schedules the next refresh from
// now + this window.
const refreshProfileValidity = 7 * 24 * time.Hour

// ErrNotClaimed is returned when an agent updates a job it does not hold.
var ErrNotClaimed = errors.New("job is not claimed by this agent")

// ErrInvalidTransition is returned for invalid state transitions.
var ErrInvalidTransition = errors.New("invalid job state transition")

// Job mirrors the jobs table.
type Job struct {
	ID             uuid.UUID       `json:"id"`
	JobType        string          `json:"job_type"`
	DeviceID       *uuid.UUID      `json:"device_id"`
	ApplicationID  *uuid.UUID      `json:"application_id"`
	State          string          `json:"state"`
	Attempt        int             `json:"attempt"`
	Progress       int             `json:"progress"`
	Parameters     json.RawMessage `json:"parameters"`
	ClaimedBy      *uuid.UUID      `json:"claimed_by"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at"`
	ErrorCategory  *string         `json:"error_category"`
	ErrorDetails   *string         `json:"error_details"`
	RetryAt        *time.Time      `json:"retry_at"`
	Result         json.RawMessage `json:"result"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at"`
	CompletedAt    *time.Time      `json:"completed_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// CreateRequest describes a new job.
type CreateRequest struct {
	JobType       string          `json:"job_type"`
	DeviceID      *uuid.UUID      `json:"device_id"`
	ApplicationID *uuid.UUID      `json:"application_id"`
	Parameters    json.RawMessage `json:"parameters"`
	IdempotencyKey string         `json:"idempotency_key"`
	RetryAt       *time.Time      `json:"retry_at"`
}

// UpdateRequest describes a status update by the claiming agent.
type UpdateRequest struct {
	State          string          `json:"state"`
	Progress       *int            `json:"progress"`
	ErrorCategory  *string         `json:"error_category"`
	ErrorDetails   *string         `json:"error_details"`
	Result         json.RawMessage `json:"result"`
}

type Service struct {
	pool         *pgxpool.Pool
	audit        *audit.Client
	lease        time.Duration
	maxBackoff   time.Duration
	offlineAfter time.Duration
	refreshLead  time.Duration
}

// Option configures the service.
type Option func(*Service)

// WithRefreshLead sets the lead window used to schedule the next refresh
// after a completed refresh job (default: 48h before profile expiry).
func WithRefreshLead(lead time.Duration) Option {
	return func(s *Service) {
		s.refreshLead = lead
	}
}

func NewService(pool *pgxpool.Pool, auditClient *audit.Client, lease time.Duration, opts ...Option) *Service {
	s := &Service{
		pool:         pool,
		audit:        auditClient,
		lease:        lease,
		maxBackoff:   30 * time.Minute,
		offlineAfter: 2 * time.Minute,
		refreshLead:  48 * time.Hour,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

const jobColumns = `
	id, job_type, device_id, application_id, state, attempt, progress,
	parameters, claimed_by, lease_expires_at, error_category, error_details,
	retry_at, result, created_at, started_at, completed_at, updated_at`

func scanJob(row pgx.Row) (*Job, error) {
	j := &Job{}
	err := row.Scan(
		&j.ID, &j.JobType, &j.DeviceID, &j.ApplicationID, &j.State,
		&j.Attempt, &j.Progress, &j.Parameters, &j.ClaimedBy,
		&j.LeaseExpiresAt, &j.ErrorCategory, &j.ErrorDetails, &j.RetryAt,
		&j.Result, &j.CreatedAt, &j.StartedAt, &j.CompletedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// Create inserts a job idempotently: a repeated idempotency_key returns the
// existing job instead of inserting a duplicate.
func (s *Service) Create(ctx context.Context, actor string, req CreateRequest) (*Job, error) {
	if req.JobType == "" {
		return nil, errors.New("job_type is required")
	}
	if req.IdempotencyKey == "" {
		return nil, errors.New("idempotency_key is required")
	}
	parameters := req.Parameters
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (job_type, device_id, application_id, parameters,
			idempotency_key, retry_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+jobColumns,
		req.JobType, req.DeviceID, req.ApplicationID, parameters,
		req.IdempotencyKey, req.RetryAt)
	job, err := scanJob(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return nil, fmt.Errorf("referenced device or application does not exist")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		// Idempotency key already exists: RETURNING produced no row.
		job = &Job{}
	}
	if job.ID != uuid.Nil {
		s.audit.Record(ctx, actor, "job.created", audit.WithDevice(job.DeviceID),
			audit.WithApplication(job.ApplicationID))
		return job, nil
	}
	err = s.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`,
		req.IdempotencyKey).Scan(
		&job.ID, &job.JobType, &job.DeviceID, &job.ApplicationID, &job.State,
		&job.Attempt, &job.Progress, &job.Parameters, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.ErrorCategory, &job.ErrorDetails, &job.RetryAt,
		&job.Result, &job.CreatedAt, &job.StartedAt, &job.CompletedAt,
		&job.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

func collectUUIDs(rows pgx.Rows) ([]uuid.UUID, error) {
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Claim atomically claims up to limit pending jobs for the caller's devices.
// Device rows are locked FOR UPDATE, serialising concurrent claims per device.
func (s *Service) Claim(ctx context.Context, agentID uuid.UUID, deviceIDs []uuid.UUID, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var devices []uuid.UUID
	if len(deviceIDs) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT id FROM devices WHERE id = ANY($1) AND agent_id = $2 ORDER BY id FOR UPDATE`,
			deviceIDs, agentID)
		if err != nil {
			return nil, err
		}
		devices, err = collectUUIDs(rows)
		if err != nil {
			return nil, err
		}
	} else {
		rows, err := tx.Query(ctx,
			`SELECT id FROM devices WHERE agent_id = $1 ORDER BY id FOR UPDATE`, agentID)
		if err != nil {
			return nil, err
		}
		devices, err = collectUUIDs(rows)
		if err != nil {
			return nil, err
		}
	}

	claimed := make([]*Job, 0, limit)
	for _, deviceID := range devices {
		if len(claimed) >= limit {
			break
		}
		row := tx.QueryRow(ctx, `
			SELECT `+jobColumns+`
			FROM jobs
			WHERE device_id = $1 AND state = $2
			  AND (retry_at IS NULL OR retry_at <= now())
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED`, deviceID, StatePending)
		job, err := scanJob(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		err = tx.QueryRow(ctx, `
			UPDATE jobs
			SET state = $1, claimed_by = $2, attempt = attempt + 1,
			    started_at = COALESCE(started_at, now()),
			    lease_expires_at = now() + $3::interval,
			    retry_at = NULL, updated_at = now()
			WHERE id = $4
			RETURNING `+jobColumns,
			StateClaimed, agentID, s.leaseSeconds()+" seconds", job.ID).Scan(
			&job.ID, &job.JobType, &job.DeviceID, &job.ApplicationID, &job.State,
			&job.Attempt, &job.Progress, &job.Parameters, &job.ClaimedBy,
			&job.LeaseExpiresAt, &job.ErrorCategory, &job.ErrorDetails, &job.RetryAt,
			&job.Result, &job.CreatedAt, &job.StartedAt, &job.CompletedAt,
			&job.UpdatedAt)
		claimed = append(claimed, job)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, job := range claimed {
		s.audit.Record(ctx, "agent:"+agentID.String(), "job.claimed",
			audit.WithDevice(job.DeviceID))
	}
	return claimed, nil
}

// Update applies a status change to a job held by the agent.
func (s *Service) Update(ctx context.Context, agentID, jobID uuid.UUID, req UpdateRequest) (*Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var current Job
	err = tx.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(
		&current.ID, &current.JobType, &current.DeviceID, &current.ApplicationID,
		&current.State, &current.Attempt, &current.Progress, &current.Parameters,
		&current.ClaimedBy, &current.LeaseExpiresAt, &current.ErrorCategory,
		&current.ErrorDetails, &current.RetryAt, &current.Result,
		&current.CreatedAt, &current.StartedAt, &current.CompletedAt,
		&current.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if current.ClaimedBy == nil || *current.ClaimedBy != agentID {
		return nil, ErrNotClaimed
	}
	if !validTransition(current.State, req.State) {
		return nil, ErrInvalidTransition
	}

	var updated Job
	var stateSQL string
	args := []any{req.State}
	if req.State == StateInProgress {
		stateSQL = `state = $1, lease_expires_at = now() + $2::interval, updated_at = now()`
		args = append(args, s.leaseSeconds()+" seconds")
	} else {
		stateSQL = `state = $1, lease_expires_at = NULL, updated_at = now()`
	}
	var extraSQL string
	var extraArgs []any
	if req.Progress != nil {
		extraSQL = `, progress = $` + fmt.Sprint(len(args)+1)
		extraArgs = append(extraArgs, *req.Progress)
	}
	if req.ErrorCategory != nil {
		extraSQL += `, error_category = $` + fmt.Sprint(len(args)+len(extraArgs)+1)
		extraArgs = append(extraArgs, *req.ErrorCategory)
	}
	if req.ErrorDetails != nil {
		extraSQL += `, error_details = $` + fmt.Sprint(len(args)+len(extraArgs)+1)
		extraArgs = append(extraArgs, *req.ErrorDetails)
	}
	if len(req.Result) > 0 {
		extraSQL += `, result = $` + fmt.Sprint(len(args)+len(extraArgs)+1)
		extraArgs = append(extraArgs, req.Result)
	}
	if req.State == StateCompleted {
		extraSQL += `, completed_at = now(), error_category = NULL, error_details = NULL`
	}
	allArgs := append(args, extraArgs...)
	allArgs = append(allArgs, jobID)
	row := tx.QueryRow(ctx,
		`UPDATE jobs SET `+stateSQL+extraSQL+` WHERE id = $`+
			fmt.Sprint(len(allArgs))+` RETURNING `+jobColumns, allArgs...)
	err = row.Scan(
		&updated.ID, &updated.JobType, &updated.DeviceID, &updated.ApplicationID,
		&updated.State, &updated.Attempt, &updated.Progress, &updated.Parameters,
		&updated.ClaimedBy, &updated.LeaseExpiresAt, &updated.ErrorCategory,
		&updated.ErrorDetails, &updated.RetryAt, &updated.Result,
		&updated.CreatedAt, &updated.StartedAt, &updated.CompletedAt,
		&updated.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := s.applyRefreshOutcome(ctx, tx, &updated, req); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.audit.Record(ctx, "agent:"+agentID.String(), "job."+req.State,
		audit.WithDevice(updated.DeviceID),
		audit.WithState(map[string]any{"state": current.State}, map[string]any{"state": updated.State}))
	return &updated, nil
}

// applyRefreshOutcome records the outcome of a refresh job on the deployment:
// a completed refresh moves the next due date forward (new expiry minus the
// lead window); a failure is recorded for the dashboard. Runs inside the job
// update transaction so the bookkeeping can never be lost.
func (s *Service) applyRefreshOutcome(ctx context.Context, tx pgx.Tx, job *Job, req UpdateRequest) error {
	if job.JobType != JobTypeRefresh || (job.State != StateCompleted && job.State != StateFailed) {
		return nil
	}
	var params struct {
		DeploymentID *uuid.UUID `json:"deployment_id"`
	}
	if err := json.Unmarshal(job.Parameters, &params); err != nil || params.DeploymentID == nil {
		return fmt.Errorf("refresh job %s: missing deployment_id in parameters", job.ID)
	}
	if job.State == StateCompleted {
		newExpiry := time.Now().Add(refreshProfileValidity)
		nextDue := newExpiry.Add(-s.refreshLead)
		if _, err := tx.Exec(ctx, `
			UPDATE deployments SET
				next_refresh_due_at = $2,
				last_refresh_at = now(),
				last_refresh_result = 'ok',
				last_refresh_error = NULL
			WHERE id = $1`, params.DeploymentID, nextDue); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE installation_records SET
				provisioning_expiry_at = $2,
				verified_at = now()
			WHERE deployment_id = $1`, params.DeploymentID, newExpiry); err != nil {
			return err
		}
		return nil
	}
	errorDetails := ""
	if req.ErrorDetails != nil {
		errorDetails = *req.ErrorDetails
	}
	if len(errorDetails) > 2000 {
		errorDetails = errorDetails[:2000]
	}
	_, err := tx.Exec(ctx, `
		UPDATE deployments SET
			last_refresh_at = now(),
			last_refresh_result = 'failed',
			last_refresh_error = $2
		WHERE id = $1`, params.DeploymentID, errorDetails)
	return err
}

func validTransition(from, to string) bool {
	switch to {
	case StateInProgress:
		return from == StateClaimed || from == StateInProgress
	case StateCompleted, StateFailed:
		return from == StateClaimed || from == StateInProgress
	}
	return false
}

func (s *Service) leaseSeconds() string {
	return fmt.Sprintf("%d", int(s.lease.Seconds()))
}

// Reap resets jobs whose lease expired back to pending with a retry delay,
// re-queues failed refresh jobs (the app must be kept alive, so a refresh
// retries until it succeeds), and marks agents offline when their heartbeat
// is stale. Called periodically by the scheduler.
func (s *Service) Reap(ctx context.Context) (reaped int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, device_id, attempt
		FROM jobs
		WHERE (state IN ($1, $2) AND lease_expires_at < now())
		   OR (state = $3 AND job_type = $4)
		ORDER BY COALESCE(lease_expires_at, now())
		FOR UPDATE SKIP LOCKED`, StateClaimed, StateInProgress, StateFailed, JobTypeRefresh)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id       uuid.UUID
		deviceID *uuid.UUID
		attempt  int
	}
	var expiredJobs []expired
	for rows.Next() {
		var j expired
		if err := rows.Scan(&j.id, &j.deviceID, &j.attempt); err != nil {
			rows.Close()
			return 0, err
		}
		expiredJobs = append(expiredJobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	backoff := s.backoffSeconds()
	for _, j := range expiredJobs {
		retryAfter := backoff
		if j.attempt > 1 {
			retryAfter = s.minBackoff(j.attempt)
		}
	_, err = tx.Exec(ctx, `
			UPDATE jobs
			SET state = $1, claimed_by = NULL, lease_expires_at = NULL,
			    retry_at = now() + $2::interval, updated_at = now()
			WHERE id = $3`,
			StatePending, fmt.Sprintf("%d", retryAfter), j.id)
		if err != nil {
			return 0, err
		}
		s.audit.Record(ctx, "scheduler", "job.reclaimed",
			audit.WithDevice(j.deviceID),
			audit.WithResult(fmt.Sprintf("re-queued, retry in %ds", retryAfter)))
	}

	_, err = tx.Exec(ctx, `
		UPDATE agents
		SET connection_state = 'offline', updated_at = now()
		WHERE connection_state = 'online'
		  AND last_heartbeat_at < now() - $1::interval`,
		fmt.Sprintf("%d", int(s.offlineAfter.Seconds())))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(expiredJobs), nil
}

func (s *Service) backoffSeconds() int {
	return 30
}

func (s *Service) minBackoff(attempt int) int {
	backoff := 30 << (attempt - 1)
	if backoff > int(s.maxBackoff.Seconds()) {
		backoff = int(s.maxBackoff.Seconds())
	}
	return backoff
}
