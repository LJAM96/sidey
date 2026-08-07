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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
	"sidey/internal/artifacts"
	"sidey/internal/auth"
	"sidey/internal/audit"
	"sidey/internal/jobs"
	webassets "sidey"
)

type Server struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	audit     *audit.Client
	jobs      *jobs.Service
	artifacts *artifacts.Store
	adminKey  string
	now       func() time.Time
}

func NewServer(pool *pgxpool.Pool, logger *slog.Logger, auditClient *audit.Client, jobService *jobs.Service, artifactStore *artifacts.Store, adminKey string) *Server {
	return &Server{
		pool:      pool,
		logger:    logger,
		audit:     auditClient,
		jobs:      jobService,
		artifacts: artifactStore,
		adminKey:  adminKey,
		now:       time.Now,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /", http.FileServerFS(webassets.Sub))

	mux.Handle("POST /api/v1/admin/enrolment-tokens", s.admin(s.handleCreateEnrolmentToken))

	mux.Handle("POST /api/v1/artifacts", s.admin(s.handleUploadArtifact))
	mux.Handle("PATCH /api/v1/artifacts/{id}", s.admin(s.handleSetArtifactState))
	mux.Handle("GET /api/v1/artifacts/{id}/download", s.admin(s.handleDownloadArtifact))

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
		rows, err := s.pool.Query(r.Context(),
			`SELECT id, api_key_hash FROM agents WHERE api_key_hash IS NOT NULL`)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "agent lookup failed")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var hash string
			if err := rows.Scan(&id, &hash); err != nil {
				s.writeError(w, http.StatusInternalServerError, "agent lookup failed")
				return
			}
			if auth.VerifySecret(key, hash) {
				next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, id)))
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
