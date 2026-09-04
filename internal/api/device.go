package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Device service node (ADR-0008).
//
// The default deployment runs the device service on the same host as the
// control plane and gives it a private, filesystem-permission-protected
// Unix socket (default /run/sidey/device.sock) instead of a bearer API key.
//
// The node is represented in `agents` by a row keyed by the non-secret
// sentinel api_key_id below, with role 'device_service'. The sentinel cannot
// authenticate over the public HTTP API: agent() resolves keys via
// auth.KeyID(), which is always a 64-char sha256 hex string, so the sentinel
// never matches a bearer key. The socket mux bypasses agent() entirely and
// trusts its (local, filesystem-gated) caller. No bearer token, enrolment
// token, capability report or heartbeat is required for the same-host node;
// remote-node device services still enrol with a normal token and this same
// role.

const (
	// localDeviceServiceSentinel is the api_key_id of the same-host device
	// service row. It is a fixed, public marker -- never a secret -- and is
	// unreachable through the HTTP agent auth path by construction.
	localDeviceServiceSentinel = "socket:device-service"
	// deviceServiceNodeName is the agents-visible name of the same-host node.
	deviceServiceNodeName = "device-service"
	// deviceServiceRole is the server-controlled role of the node. Remote
	// device services enrol with the same role via an enrolment token.
	deviceServiceRole = "device_service"
)

// DeviceHandler exposes the job/control endpoints the same-host device
// service drives over its Unix socket. It trusts its caller without bearer
// credentials; reachability is gated by the socket path's filesystem mode
// (set in cmd/control-plane), which excludes every user except the control
// plane's owner.
func (s *Server) DeviceHandler() http.Handler {
	mux := http.NewServeMux()
	d := s.deviceService
	mux.Handle("GET /api/v1/device/health", d(s.handleDeviceHealth))
	mux.Handle("GET /api/v1/device/me", d(s.handleDeviceMe))
	mux.Handle("POST /api/v1/device/me/heartbeat", d(s.handleHeartbeat))
	mux.Handle("POST /api/v1/device/me/devices", d(s.handleReportDevices))
	mux.Handle("POST /api/v1/device/jobs/claim", d(s.handleClaimJobs))
	mux.Handle("POST /api/v1/device/jobs/{id}/status", d(s.handleUpdateJob))
	mux.Handle("GET /api/v1/device/jobs/{id}", d(s.handleDeviceGetJob))
	mux.Handle("POST /api/v1/device/refresh/{id}/sign", d(s.handleDeviceRefreshSign))
	mux.Handle("GET /api/v1/device/artifacts/{id}/download", d(s.handleAgentDownloadArtifact))
	return s.recoverMiddleware(mux)
}

// deviceService authenticates a same-host caller. The local node has no API
// key: the request is accepted because it can only arrive over the Unix
// socket whose parent directory is restricted to the control plane owner.
// The node's agent id is resolved (and the row created on first use) from
// the fixed sentinel.
func (s *Server) deviceService(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.localDeviceServiceID(r.Context())
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "device service node unavailable")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, id)))
	})
}

// localDeviceServiceID resolves the same-host device service node, creating
// the row the first time it is needed. It is idempotent: the ON CONFLICT
// DO NOTHING makes repeated calls (and restarts) a no-op, and the row is
// keyed by the api_key_id sentinel (covered by the partial unique index on
// non-null api_key_id), not by name.
func (s *Server) localDeviceServiceID(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE api_key_id = $1`, localDeviceServiceSentinel).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO agents (name, role, api_key_id, connection_state, last_heartbeat_at)
		VALUES ($1, $2, $3, 'online', now())
		ON CONFLICT (api_key_id) WHERE api_key_id IS NOT NULL DO NOTHING`,
		deviceServiceNodeName, deviceServiceRole, localDeviceServiceSentinel)
	if err != nil {
		return uuid.Nil, err
	}
	err = s.pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE api_key_id = $1`, localDeviceServiceSentinel).Scan(&id)
	return id, err
}

// handleDeviceMe reports the node identity without the capability report the
// remote-agent heartbeat carries.
func (s *Server) handleDeviceMe(w http.ResponseWriter, r *http.Request) {
	agent := agentID(r.Context())
	var name, state string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT name, connection_state FROM agents WHERE id = $1`, agent).Scan(&name, &state); err != nil {
		s.writeError(w, http.StatusInternalServerError, "node lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": agent, "name": name, "role": deviceServiceRole,
		"connection_state": state,
	})
}

// handleDeviceHealth is the socket-local health probe (the public healthz is
// mapped to the HTTP listener only).
func (s *Server) handleDeviceHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ping(r.Context()); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "transport": "unix-socket", "node": localDeviceServiceSentinel,
	})
}
