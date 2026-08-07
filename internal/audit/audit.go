// Package audit records every state changing action in audit_events.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
)

// Client writes audit events.
type Client struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Client {
	return &Client{pool: pool, logger: logger}
}

type Option func(*event)

type event struct {
	actor       string
	action      string
	deviceID    *uuid.UUID
	application *uuid.UUID
	artifact    string
	previous    any
	current     any
	result      string
}

// WithDevice attaches a device id.
func WithDevice(id *uuid.UUID) Option {
	return func(e *event) { e.deviceID = id }
}

// WithApplication attaches an application id.
func WithApplication(id *uuid.UUID) Option {
	return func(e *event) { e.application = id }
}

// WithArtifact attaches an artifact sha256.
func WithArtifact(sha string) Option {
	return func(e *event) { e.artifact = sha }
}

// WithState attaches previous and new state snapshots.
func WithState(previous, current any) Option {
	return func(e *event) { e.previous, e.current = previous, current }
}

// WithData attaches a snapshot without a previous state.
func WithData(current any) Option {
	return func(e *event) { e.current = current }
}

// WithResult records a failure result (default "ok").
func WithResult(result string) Option {
	return func(e *event) { e.result = result }
}

// Record inserts one audit event. It is best effort: failures are logged and
// never fail the underlying operation.
func (c *Client) Record(ctx context.Context, actor, action string, opts ...Option) {
	ev := &event{actor: actor, action: action, result: "ok"}
	for _, o := range opts {
		o(ev)
	}
	var previous, current []byte
	var err error
	if ev.previous != nil {
		previous, err = json.Marshal(ev.previous)
		if err != nil {
			previous = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		}
	}
	if ev.current != nil {
		current, err = json.Marshal(ev.current)
		if err != nil {
			current = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		}
	}
	_, err = c.pool.Exec(ctx, `
		INSERT INTO audit_events
			(actor, action, device_id, application_id, artifact_sha256,
			 previous_state, new_state, occurred_at, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), $8)`,
		ev.actor, ev.action, ev.deviceID, ev.application, ev.artifact,
		previous, current, ev.result)
	if err != nil {
		c.logger.Error("audit write failed", "action", action, "error", err)
	}
}
