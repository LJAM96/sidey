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
	// StateDead marks a job that can never succeed (revoked certificate,
	// authentication failure, corrupt IPA, ...) or that exhausted its retry
	// budget. Dead jobs are never re-queued; they surface on the dashboard
	// as requiring attention, so an operator decides.
	StateDead = "dead"
)

// terminalErrorCategories describe failures that retrying cannot fix. Reaping
// classifies a failed job with one of these as dead instead of re-queuing it,
// so a permanently broken sign/refresh cannot hammer the queue (and Apple's
// services) forever. Network and timeout failures remain retryable.
var terminalErrorCategories = map[string]bool{
	"auth":                 true,
	"certificate":          true,
	"provisioning":         true,
	"entitlement":          true,
	"unsupported_job_type": true,
}

// defaultMaxAttempts bounds how many times a job can be claimed before it is
// retired to the dead state. Individual jobs may override via max_attempts.
const defaultMaxAttempts = 5

// Standard JobTypes recognized across Sidey.
const (
	JobTypeInstall   = "install"
	JobTypeVerify    = "verify"
	JobTypeRefresh   = "refresh"
	JobTypeSign      = "sign"
	JobTypeUninstall = "uninstall"
	JobTypeInventory = "inventory"
	// JobTypeExportP12 requests the signing worker export the account's
	// development certificate + private key as a PKCS#12 archive. It carries
	// no device and is served back to the control plane in the job result.
	JobTypeExportP12 = "export_p12"

	// JobTypeAppIDs asks the signing worker to list or delete the account's
	// registered App IDs on the Apple developer portal (GUI-managed quota).
	JobTypeAppIDs = "appids"
	// JobTypeLiveContainerPush requests the device service push a guest IPA
	// (or certificate p12) file directly into the LiveContainer app container
	// on a device, typically over the wireless RSD tunnel. It is a device-
	// scoped job executed by the install-side agent.
	JobTypeLiveContainerPush = "livecontainer_push"
	// JobTypeInstalledApps requests the device service inventory the device:
	// system apps via the installation proxy, plus the guest apps inside the
	// LiveContainer container. The inventory is reported in the job result.
	JobTypeInstalledApps = "installed_apps"
)

// refreshProfileValidity is the assumed validity of a freshly issued free-team
// provisioning profile (7 days), used when a completed refresh job does not
// report the real profile expiry (e.g. legacy agents). The refresh agent
// normally reports the exact new expiry, which takes precedence.
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
	// ParentJobID links a child job to its parent workflow (e.g. a sign or
	// install child points at its refresh parent). NULL for standalone jobs.
	ParentJobID *uuid.UUID `json:"parent_job_id,omitempty"`
	// Purpose names the workflow that created the job: deploy, refresh,
	// manual_refresh, update. NULL for standalone jobs.
	Purpose        *string        `json:"purpose,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	StartedAt      *time.Time     `json:"started_at"`
	CompletedAt    *time.Time     `json:"completed_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// CreateRequest describes a new job.
type CreateRequest struct {
	JobType        string          `json:"job_type"`
	DeviceID       *uuid.UUID      `json:"device_id"`
	ApplicationID  *uuid.UUID      `json:"application_id"`
	Parameters     json.RawMessage `json:"parameters"`
	IdempotencyKey string          `json:"idempotency_key"`
	RetryAt        *time.Time      `json:"retry_at"`
	ParentJobID    *uuid.UUID      `json:"parent_job_id,omitempty"`
	Purpose        *string         `json:"purpose,omitempty"`
}

// UpdateRequest describes a status update by the claiming agent.
type UpdateRequest struct {
	State         string          `json:"state"`
	Progress      *int            `json:"progress"`
	ErrorCategory *string         `json:"error_category"`
	ErrorDetails  *string         `json:"error_details"`
	Result        json.RawMessage `json:"result"`
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
		maxBackoff:   5 * time.Minute,
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
	retry_at, result, parent_job_id, purpose,
	created_at, started_at, completed_at, updated_at`

func scanJob(row pgx.Row) (*Job, error) {
	j := &Job{}
	err := row.Scan(
		&j.ID, &j.JobType, &j.DeviceID, &j.ApplicationID, &j.State,
		&j.Attempt, &j.Progress, &j.Parameters, &j.ClaimedBy,
		&j.LeaseExpiresAt, &j.ErrorCategory, &j.ErrorDetails, &j.RetryAt,
		&j.Result, &j.ParentJobID, &j.Purpose,
		&j.CreatedAt, &j.StartedAt, &j.CompletedAt, &j.UpdatedAt,
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
			idempotency_key, retry_at, parent_job_id, purpose)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+jobColumns,
		req.JobType, req.DeviceID, req.ApplicationID, parameters,
		req.IdempotencyKey, req.RetryAt, req.ParentJobID, req.Purpose)
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
		&job.Result, &job.ParentJobID, &job.Purpose,
		&job.CreatedAt, &job.StartedAt, &job.CompletedAt,
		&job.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// CreateRefresh creates a refresh job with a custom max_attempts value and
// workflow purpose. This is used by the scheduler to give refresh jobs more
// retry headroom than the default (5), since a refresh that fails today may
// succeed once the root cause (auth, quota, tunnel) is resolved.
func (s *Service) CreateRefresh(ctx context.Context, actor string, deviceID *uuid.UUID, params json.RawMessage, idempotencyKey string, maxAttempts int, purpose string) (*Job, error) {
	if deviceID == nil {
		return nil, errors.New("device_id is required for refresh jobs")
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	if purpose == "" {
		purpose = "refresh"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (job_type, device_id, parameters, idempotency_key, max_attempts, purpose)
		VALUES ('refresh', $1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+jobColumns,
		deviceID, params, idempotencyKey, maxAttempts, purpose)
	job, err := scanJob(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		job = &Job{}
	}
	if job.ID != uuid.Nil {
		s.audit.Record(ctx, actor, "job.created", audit.WithDevice(job.DeviceID))
		return job, nil
	}
	err = s.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`,
		idempotencyKey).Scan(
		&job.ID, &job.JobType, &job.DeviceID, &job.ApplicationID, &job.State,
		&job.Attempt, &job.Progress, &job.Parameters, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.ErrorCategory, &job.ErrorDetails, &job.RetryAt,
		&job.Result, &job.ParentJobID, &job.Purpose,
		&job.CreatedAt, &job.StartedAt, &job.CompletedAt,
		&job.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// Get returns a single job by id. Used by the device-service refresh
// orchestrator to follow a sign job it requested.
func (s *Service) Get(ctx context.Context, jobID uuid.UUID) (*Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, jobID)
	return scanJob(row)
}

// ClaimSpecific claims one known job for an agent. Pending jobs are claimed
// directly; failed or dead ones are requeued first so install retries reuse
// the existing signed artifact instead of signing again. Completed jobs are
// returned as-is; active ones are an error. This is how the refresh
// orchestrator takes ownership of its install child while the normal poll
// loop keeps claiming everything else.
func (s *Service) ClaimSpecific(ctx context.Context, agentID, jobID uuid.UUID) (*Job, error) {
	var cur string
	var claimedBy *uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT state, claimed_by FROM jobs WHERE id = $1`, jobID).Scan(&cur, &claimedBy); err != nil {
		return nil, fmt.Errorf("job not found")
	}
	switch cur {
	case StateCompleted:
		return s.Get(ctx, jobID)
	case StateClaimed, StateInProgress:
		// Our own in-flight claim (e.g. orchestrator resumed after a
		// restart inside the lease window) resumes instead of conflicting.
		if claimedBy != nil && *claimedBy == agentID {
			return s.Get(ctx, jobID)
		}
		return nil, fmt.Errorf("job is already active")
	case StateFailed, StateDead:
		if _, err := s.pool.Exec(ctx, `
			UPDATE jobs
			SET state = $1, claimed_by = NULL, lease_expires_at = NULL,
			    retry_at = NULL, updated_at = now()
			WHERE id = $2`, StatePending, jobID); err != nil {
			return nil, fmt.Errorf("job requeue failed")
		}
	case StatePending:
		// Proceed to claim below.
	default:
		return nil, fmt.Errorf("job cannot be claimed from state %s", cur)
	}
	claimed, err := scanJob(s.pool.QueryRow(ctx, `
		UPDATE jobs
		SET state = $1, claimed_by = $2, attempt = attempt + 1,
		    started_at = COALESCE(started_at, now()),
		    lease_expires_at = now() + $3::interval,
		    retry_at = NULL, updated_at = now()
		WHERE id = $4 AND state = $5
		RETURNING `+jobColumns,
		StateClaimed, agentID, s.leaseSeconds()+" seconds", jobID, StatePending))
	if err != nil {
		return nil, fmt.Errorf("job could not be claimed")
	}
	s.audit.Record(ctx, "agent:"+agentID.String(), "job.claimed",
		audit.WithDevice(claimed.DeviceID))
	return claimed, nil
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

// nullableJobTypes returns nil for an empty slice so callers can pass it as a
// PostgreSQL text[] that matches everything when NULL.
func nullableJobTypes(types []string) any {
	if len(types) == 0 {
		return nil
	}
	return types
}

// globalTypes reports whether every requested job type is claimable by any
// agent regardless of device ownership. Currently sign jobs and the
// device-less certificate export job are global.
func globalTypes(types []string) bool {
	for _, t := range types {
		if t != JobTypeSign && t != JobTypeExportP12 && t != JobTypeAppIDs {
			return false
		}
	}
	return true
}

// Claim atomically claims up to limit pending jobs for the caller's devices.
// Device rows are locked FOR UPDATE, serialising concurrent claims per device.
// When jobTypes is non-empty, only jobs of those types are claimed; sign jobs
// are claimable by any agent (the signing worker does not own the target
// device), while other types remain restricted to the agent's own devices. A
// claim without jobTypes never delivers sign jobs: they belong to the signing
// worker, and generic agents would only fail them. An explicit device_ids
// list narrows the claim set in every case.
func (s *Service) Claim(ctx context.Context, agentID uuid.UUID, deviceIDs []uuid.UUID, jobTypes []string, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var isGlobalAgent bool
	if agentID == uuid.Nil {
		isGlobalAgent = true
	} else {
		var role string
		_ = tx.QueryRow(ctx, `SELECT role FROM agents WHERE id = $1`, agentID).Scan(&role)
		if role == "device_service" || role == "signing_worker" {
			isGlobalAgent = true
		}
	}

	claimed := make([]*Job, 0, limit)

	// Device-less jobs (e.g. certificate p12 export) have no target device
	// row to lock, so they bypass the per-device serialisation below. They are
	// only claimable by a global agent (signing worker / device service) that
	// explicitly requests the type.
	if len(jobTypes) > 0 && len(deviceIDs) == 0 && (isGlobalAgent || globalTypes(jobTypes)) {
		row := tx.QueryRow(ctx,
			`SELECT `+jobColumns+`
			 FROM jobs
			 WHERE device_id IS NULL AND state = $1
			   AND (retry_at IS NULL OR retry_at <= now())
			   AND job_type = ANY($2)
			 ORDER BY created_at, id
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED`, StatePending, jobTypes)
		job, err := scanJob(row)
		if err == nil {
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
				&job.Result, &job.ParentJobID, &job.Purpose,
				&job.CreatedAt, &job.StartedAt, &job.CompletedAt,
				&job.UpdatedAt)
			if err == nil {
				claimed = append(claimed, job)
			}
		}
	}

	var devices []uuid.UUID
	if len(jobTypes) > 0 {
		// Candidate devices with pending jobs of the requested types.
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT j.device_id
			 FROM jobs j
			 WHERE j.job_type = ANY($1) AND j.state = $2
			   AND (j.retry_at IS NULL OR j.retry_at <= now())
			   AND j.device_id IS NOT NULL`,
			jobTypes, StatePending)
		if err != nil {
			return nil, err
		}
		candidate, err := collectUUIDs(rows)
		if err != nil {
			return nil, err
		}
		// Narrow candidates: non-global types may only target the caller's
		// own devices (a job_types filter must not become a claim scoping
		// escape hatch), and an explicit device_ids list further restricts.
		if len(candidate) > 0 {
			var rows pgx.Rows
			switch {
			case (isGlobalAgent || globalTypes(jobTypes)) && len(deviceIDs) == 0:
				rows, err = tx.Query(ctx,
					`SELECT id FROM devices WHERE id = ANY($1) ORDER BY id FOR UPDATE`, candidate)
			case isGlobalAgent || globalTypes(jobTypes):
				rows, err = tx.Query(ctx,
					`SELECT id FROM devices WHERE id = ANY($1) AND id = ANY($2) ORDER BY id FOR UPDATE`,
					candidate, deviceIDs)
			case len(deviceIDs) == 0:
				rows, err = tx.Query(ctx,
					`SELECT id FROM devices WHERE id = ANY($1) AND agent_id = $2 ORDER BY id FOR UPDATE`,
					candidate, agentID)
			default:
				rows, err = tx.Query(ctx,
					`SELECT id FROM devices WHERE id = ANY($1) AND agent_id = $2 AND id = ANY($3) ORDER BY id FOR UPDATE`,
					candidate, agentID, deviceIDs)
			}
			if err != nil {
				return nil, err
			}
			devices, err = collectUUIDs(rows)
			if err != nil {
				return nil, err
			}
		}
	} else if len(deviceIDs) > 0 {
		var rows pgx.Rows
		if isGlobalAgent {
			rows, err = tx.Query(ctx,
				`SELECT id FROM devices WHERE id = ANY($1) ORDER BY id FOR UPDATE`, deviceIDs)
		} else {
			rows, err = tx.Query(ctx,
				`SELECT id FROM devices WHERE id = ANY($1) AND agent_id = $2 ORDER BY id FOR UPDATE`,
				deviceIDs, agentID)
		}
		if err != nil {
			return nil, err
		}
		devices, err = collectUUIDs(rows)
		if err != nil {
			return nil, err
		}
	} else {
		var rows pgx.Rows
		if isGlobalAgent {
			rows, err = tx.Query(ctx,
				`SELECT id FROM devices ORDER BY id FOR UPDATE`)
		} else {
			rows, err = tx.Query(ctx,
				`SELECT id FROM devices WHERE agent_id = $1 ORDER BY id FOR UPDATE`, agentID)
		}
		if err != nil {
			return nil, err
		}
		devices, err = collectUUIDs(rows)
		if err != nil {
			return nil, err
		}
	}
	for _, deviceID := range devices {
		if len(claimed) >= limit {
			break
		}
		row := tx.QueryRow(ctx, `
			SELECT `+jobColumns+`
			FROM jobs
			WHERE device_id = $1 AND state = $2
			  AND (retry_at IS NULL OR retry_at <= now())
			  AND (job_type <> $3 OR $4::text[] IS NOT NULL)
			  AND ($4::text[] IS NULL OR job_type = ANY($4))
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED`, deviceID, StatePending, JobTypeSign, nullableJobTypes(jobTypes))
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
			&job.Result, &job.ParentJobID, &job.Purpose,
			&job.CreatedAt, &job.StartedAt, &job.CompletedAt,
			&job.UpdatedAt)
		if err != nil {
			return nil, err
		}
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
		&current.ParentJobID, &current.Purpose,
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
		&updated.ParentJobID, &updated.Purpose,
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
		// Prefer the real expiry reported by the refresh orchestrator (the
		// installer prints the embedded provisioning profile's expiry after
		// a successful install). Fall back to an assumed validity window
		// when the agent does not report one.
		newExpiry := time.Now().Add(refreshProfileValidity)
		var signedID, installID *uuid.UUID
		if len(req.Result) > 0 {
			var result struct {
				ProfileExpiryAt  *time.Time `json:"profile_expiry_at"`
				SignedArtifactID *uuid.UUID `json:"signed_artifact_id"`
				InstallJobID     *uuid.UUID `json:"install_job_id"`
			}
			if err := json.Unmarshal(req.Result, &result); err != nil {
				return fmt.Errorf("refresh job %s: invalid result JSON", job.ID)
			}
			if result.ProfileExpiryAt != nil {
				newExpiry = *result.ProfileExpiryAt
			}
			signedID, installID = result.SignedArtifactID, result.InstallJobID
		}
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
		// The completed refresh names the exact signed artifact installed
		// and the install job that installed it: this row becomes the
		// authoritative install state the next refresh resolves through.
		// Legacy results without the ids keep the previous record linkage
		// untouched (only expiry advances).
		if signedID != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE installation_records SET
					provisioning_expiry_at = $2,
					verified_at = now(),
					signed_artifact_id = $3,
					install_job_id = $4
				WHERE deployment_id = $1`, params.DeploymentID, newExpiry, signedID, installID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
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
// re-queues failed refresh and sign jobs (an app must be kept signed, and a
// sign request may fail transiently), and marks agents offline when their
// heartbeat is stale. Called periodically by the scheduler.
//
// Retry is finite: a failed job whose error category is terminal (auth,
// certificate, provisioning, entitlement, unsupported type) or whose attempt
// count reached max_attempts is moved to the dead state with
// requires_attention instead of being re-queued, so permanently broken work
// never retries forever (or keeps hitting external services).
func (s *Service) Reap(ctx context.Context) (reaped int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, device_id, attempt, error_category, max_attempts, state
		FROM jobs
		WHERE (state IN ($1, $2) AND lease_expires_at < now())
		   OR (state = $3 AND job_type IN ($4, $5))
		ORDER BY COALESCE(lease_expires_at, now())
		FOR UPDATE SKIP LOCKED`, StateClaimed, StateInProgress, StateFailed, JobTypeRefresh, JobTypeSign)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id            uuid.UUID
		deviceID      *uuid.UUID
		attempt       int
		errorCategory *string
		maxAttempts   int
		state         string
	}
	var expiredJobs []expired
	for rows.Next() {
		var j expired
		if err := rows.Scan(&j.id, &j.deviceID, &j.attempt, &j.errorCategory, &j.maxAttempts, &j.state); err != nil {
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
		category := ""
		if j.errorCategory != nil {
			category = *j.errorCategory
		}
		maxAttempts := j.maxAttempts
		if maxAttempts <= 0 {
			maxAttempts = defaultMaxAttempts
		}
		terminal := j.state == StateFailed && terminalErrorCategories[category]
		exhausted := j.attempt >= maxAttempts
		if terminal {
			reason := category
			_, err = tx.Exec(ctx, `
				UPDATE jobs
				SET state = $1, claimed_by = NULL, lease_expires_at = NULL,
				    retry_at = NULL, requires_attention = true,
				    last_failure_class = $2, dead_reason = $3, updated_at = now()
				WHERE id = $4`,
				StateDead, category, "terminal_error:"+reason, j.id)
			if err != nil {
				return 0, err
			}
			s.audit.Record(ctx, "scheduler", "job.dead",
				audit.WithDevice(j.deviceID),
				audit.WithResult("terminal error "+reason+"; not retried"))
			reaped++
			continue
		}
		if exhausted {
			reason := "max_attempts"
			_, err = tx.Exec(ctx, `
				UPDATE jobs
				SET state = $1, claimed_by = NULL, lease_expires_at = NULL,
				    retry_at = NULL, requires_attention = true,
				    last_failure_class = $2, dead_reason = $3, updated_at = now()
				WHERE id = $4`,
				StateDead, category, reason, j.id)
			if err != nil {
				return 0, err
			}
			s.audit.Record(ctx, "scheduler", "job.dead",
				audit.WithDevice(j.deviceID),
				audit.WithResult(reason+"; not retried"))
			reaped++
			continue
		}
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
		reaped++
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
