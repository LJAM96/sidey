// Package api implements the control plane REST API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
	webassets "sidey"
	"sidey/internal/artifacts"
	"sidey/internal/audit"
	"sidey/internal/auth"
	"sidey/internal/jobs"
)

type Server struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	audit     *audit.Client
	jobs      *jobs.Service
	artifacts *artifacts.Store
	adminKey  string
	// maxArtifactBytes caps the ingest body size (original IPA uploads and
	// signed derivative multipart uploads). Requests larger than this are
	// rejected before the body can exhaust the artifact volume.
	maxArtifactBytes int64
	now              func() time.Time
}

func NewServer(pool *pgxpool.Pool, logger *slog.Logger, auditClient *audit.Client, jobService *jobs.Service, artifactStore *artifacts.Store, adminKey string) *Server {
	return NewServerWithLimits(pool, logger, auditClient, jobService, artifactStore, adminKey, defaultMaxArtifactBytes)
}

// NewServerWithLimits constructs the server with an explicit maximum ingest
// body size for artifact uploads.
func NewServerWithLimits(pool *pgxpool.Pool, logger *slog.Logger, auditClient *audit.Client, jobService *jobs.Service, artifactStore *artifacts.Store, adminKey string, maxArtifactBytes int64) *Server {
	if maxArtifactBytes <= 0 {
		maxArtifactBytes = defaultMaxArtifactBytes
	}
	return &Server{
		pool:             pool,
		logger:           logger,
		audit:            auditClient,
		jobs:             jobService,
		artifacts:        artifactStore,
		adminKey:         adminKey,
		maxArtifactBytes: maxArtifactBytes,
		now:              time.Now,
	}
}

// defaultMaxArtifactBytes allows IPAs up to 4 GiB. Configure a smaller value
// via SIDEY_MAX_ARTIFACT_BYTES when the deployment imposes a tighter limit.
const defaultMaxArtifactBytes int64 = 4 << 30

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	mux.Handle("GET /", http.FileServerFS(webassets.Sub))

	mux.Handle("POST /api/v1/admin/enrolment-tokens", s.admin(s.handleCreateEnrolmentToken))

	mux.Handle("POST /api/v1/admin/apple-accounts/credentials", s.admin(s.handleUpdateAppleCredentials))
	mux.Handle("POST /api/v1/admin/deploy", s.admin(s.handleAdminDeploy))

	mux.Handle("POST /api/v1/artifacts", s.admin(s.handleUploadArtifact))
	mux.Handle("PATCH /api/v1/artifacts/{id}", s.admin(s.handleSetArtifactState))
	mux.Handle("GET /api/v1/artifacts/{id}/download", s.admin(s.handleDownloadArtifact))
	mux.Handle("POST /api/v1/sign-jobs", s.admin(s.handleCreateSignJob))
	mux.Handle("POST /api/v1/signed-artifacts", s.agent(s.handleUploadSignedArtifact))
	mux.Handle("GET /api/v1/agents/artifacts/{id}/download", s.agent(s.handleAgentDownloadArtifact))

	mux.Handle("POST /api/v1/certificates/{id}/revoke", s.admin(s.handleRevokeCertificate))

	mux.Handle("POST /api/v1/agents/enrol", s.enrolmentToken(s.handleEnrolAgent))
	mux.Handle("POST /api/v1/agents/me/heartbeat", s.agent(s.handleHeartbeat))
	mux.Handle("POST /api/v1/agents/me/devices", s.agent(s.handleReportDevices))

	mux.Handle("POST /api/v1/jobs", s.admin(s.handleCreateJob))
	mux.Handle("POST /api/v1/jobs/claim", s.agent(s.handleClaimJobs))
	mux.Handle("POST /api/v1/jobs/{id}/status", s.agent(s.handleUpdateJob))

	mux.Handle("GET /api/v1/dashboard/agents", s.admin(s.handleListAgents))
	mux.Handle("GET /api/v1/dashboard/devices", s.admin(s.handleListDevices))
	mux.Handle("GET /api/v1/dashboard/jobs", s.admin(s.handleListJobs))
	mux.Handle("GET /api/v1/dashboard/applications", s.admin(s.handleListApplications))
	mux.Handle("GET /api/v1/dashboard/artifacts", s.admin(s.handleListArtifacts))
	mux.Handle("GET /api/v1/dashboard/signed-artifacts", s.admin(s.handleListSignedArtifacts))
	mux.Handle("GET /api/v1/dashboard/accounts", s.admin(s.handleListAccounts))
	mux.Handle("GET /api/v1/dashboard/certificates", s.admin(s.handleListCertificates))
	mux.Handle("GET /api/v1/dashboard/deployments", s.admin(s.handleListDeployments))
	mux.Handle("GET /api/v1/dashboard/refresh", s.admin(s.handleListRefresh))

	return s.recoverMiddleware(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ping(r.Context()); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// recoverMiddleware converts panics into 500s so one bad handler cannot take
// the whole control plane down.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey == "" {
			s.writeError(w, http.StatusServiceUnavailable, "admin key not configured")
			return
		}
		key, ok := bearerToken(r)
		if !ok || !auth.ConstantTimeEqual(key, s.adminKey) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid admin key"})
			return
		}
		next(w, r)
	})
}

func (s *Server) agent(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer token"})
			return
		}
		var id uuid.UUID
		// Fast path: agents enrolled after the key_id column exists resolve
		// in a single indexed query.
		err := s.pool.QueryRow(r.Context(),
			`SELECT id FROM agents WHERE api_key_id = $1`, auth.KeyID(key)).Scan(&id)
		if err == nil {
			next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, id)))
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeError(w, http.StatusInternalServerError, "agent lookup failed")
			return
		}
		// Legacy path: agents predating key ids are verified against their
		// stored bcrypt hash. Only rows without a key id are scanned.
		rows, err := s.pool.Query(r.Context(),
			`SELECT id, api_key_hash FROM agents
			 WHERE api_key_id IS NULL AND api_key_hash IS NOT NULL`)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "agent lookup failed")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var aID uuid.UUID
			var hash string
			if err := rows.Scan(&aID, &hash); err != nil {
				s.writeError(w, http.StatusInternalServerError, "agent lookup failed")
				return
			}
			if auth.VerifySecret(key, hash) {
				next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, aID)))
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent key"})
	})
}

func (s *Server) enrolmentToken(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer token"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), enrolmentKey{}, key)))
	})
}

type agentKey struct{}

type enrolmentKey struct{}

func agentID(ctx context.Context) uuid.UUID {
	return ctx.Value(agentKey{}).(uuid.UUID)
}

func enrolmentSecret(ctx context.Context) string {
	return ctx.Value(enrolmentKey{}).(string)
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || header[:7] != "Bearer " {
		return "", false
	}
	return header[7:], true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	writeJSON(w, http.StatusOK, map[string]any{
		"admin_key":  s.adminKey,
		"configured": s.adminKey != "",
	})
}

