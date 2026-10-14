// Package api implements the control plane REST API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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
	// enrollCIDRs gates POST /api/v1/devices/enroll by source network
	// (Phase 6 one-tap pairing). enrollLimit rate-limits it per IP.
	enrollCIDRs []*net.IPNet
	enrollLimit *ipLimiter
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
	cidrs, err := parseEnrollCIDRs("")
	if err != nil {
		cidrs = nil
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
		enrollCIDRs:      cidrs,
		enrollLimit:      newIPLimiter(10, time.Minute),
	}
}

// defaultMaxArtifactBytes allows IPAs up to 4 GiB. Configure a smaller value
// via SIDEY_MAX_ARTIFACT_BYTES when the deployment imposes a tighter limit.
const defaultMaxArtifactBytes int64 = 4 << 30

// SetEnrollCIDRs overrides the default enrollment source allow-list
// (SIDEY_ENROLL_CIDRS, comma-separated). An invalid list is rejected and
// the previous list kept, so a typo cannot silently open enrollment.
func (s *Server) SetEnrollCIDRs(csv string) error {
	cidrs, err := parseEnrollCIDRs(csv)
	if err != nil {
		return err
	}
	s.enrollCIDRs = cidrs
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	mux.HandleFunc("GET /api/v1/server-identity", s.handleServerIdentity)
	mux.HandleFunc("POST /api/v1/devices/enroll", s.handleEnrollDevice)
	fileServer := http.FileServerFS(webassets.Sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     "sidey_session",
				Value:    s.adminKey,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		fileServer.ServeHTTP(w, r)
	})

	mux.Handle("POST /api/v1/admin/enrolment-tokens", s.admin(s.handleCreateEnrolmentToken))

	mux.Handle("POST /api/v1/admin/apple-accounts/credentials", s.admin(s.handleUpdateAppleCredentials))
	mux.Handle("POST /api/v1/admin/apple-accounts/2fa-code", s.adminOrToken("control", s.handleSubmit2FACode))
	mux.Handle("POST /api/v1/admin/apple-app-ids", s.admin(s.handleManageAppIDs))
	mux.Handle("DELETE /api/v1/admin/apple-accounts/{id}", s.admin(s.handleDeleteAppleAccount))
	mux.Handle("GET /api/v1/admin/apple-accounts/{id}/certificate/download", s.admin(s.handleAppleCertificateDownload))
	mux.Handle("POST /api/v1/admin/deploy", s.admin(s.handleAdminDeploy))
	mux.Handle("POST /api/v1/admin/refresh", s.adminOrToken("control", s.handleManualRefresh))
	mux.Handle("POST /api/v1/admin/livecontainer/install", s.adminOrToken("control", s.handleInstallLiveContainer))
	mux.Handle("POST /api/v1/admin/livecontainer/push", s.adminOrToken("control", s.handleLiveContainerPush))
	mux.Handle("POST /api/v1/admin/livecontainer/push-p12", s.adminOrToken("control", s.handleLiveContainerP12Push))
	mux.Handle("POST /api/v1/admin/livecontainer/installed-apps", s.adminOrToken("control", s.handleInstalledApps))
	mux.Handle("POST /api/v1/admin/devices", s.admin(s.handleAdminCreateDevice))
	mux.Handle("DELETE /api/v1/admin/devices/{id}", s.admin(s.handleAdminDeleteDevice))

	mux.Handle("GET /api/v1/admin/store/sources", s.adminOrToken("read", s.handleListStoreSources))
	mux.Handle("POST /api/v1/admin/store/sources", s.admin(s.handleAddStoreSource))
	mux.Handle("DELETE /api/v1/admin/store/sources/{id}", s.admin(s.handleDeleteStoreSource))
	mux.Handle("GET /api/v1/admin/store/apps", s.adminOrToken("read", s.handleListStoreApps))
	mux.Handle("POST /api/v1/admin/store/install", s.adminOrToken("control", s.handleStoreInstall))

	mux.HandleFunc("GET /source.json", s.handleStoreSourceJSON)
	mux.HandleFunc("GET /api/v1/store/source.json", s.handleStoreSourceJSON)
	mux.HandleFunc("GET /api/v1/store/artifacts/{id}/install.ipa", s.handleStoreArtifactInstall)

	mux.Handle("POST /api/v1/artifacts", s.adminOrToken("control", s.handleUploadArtifact))
	mux.Handle("PATCH /api/v1/artifacts/{id}", s.adminOrToken("control", s.handleSetArtifactState))
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

	mux.Handle("GET /api/v1/dashboard/agents", s.adminOrToken("read", s.handleListAgents))
	mux.Handle("GET /api/v1/dashboard/devices", s.adminOrToken("read", s.handleListDevices))
	mux.Handle("GET /api/v1/dashboard/jobs", s.adminOrToken("read", s.handleListJobs))
	mux.Handle("GET /api/v1/dashboard/applications", s.adminOrToken("read", s.handleListApplications))
	mux.Handle("GET /api/v1/dashboard/artifacts", s.adminOrToken("read", s.handleListArtifacts))
	mux.Handle("GET /api/v1/dashboard/signed-artifacts", s.adminOrToken("read", s.handleListSignedArtifacts))
	mux.Handle("GET /api/v1/dashboard/accounts", s.adminOrToken("read", s.handleListAccounts))
	mux.Handle("GET /api/v1/dashboard/certificates", s.adminOrToken("read", s.handleListCertificates))
	mux.Handle("GET /api/v1/dashboard/deployments", s.adminOrToken("read", s.handleListDeployments))
	mux.Handle("GET /api/v1/dashboard/refresh", s.adminOrToken("read", s.handleListRefresh))
	mux.Handle("GET /api/v1/dashboard/updates", s.adminOrToken("read", s.handleListUpdates))
	mux.Handle("GET /api/v1/admin/store/subscriptions", s.adminOrToken("read", s.handleListSubscriptions))
	mux.Handle("POST /api/v1/admin/store/subscriptions", s.adminOrToken("control", s.handleAddSubscription))
	mux.Handle("DELETE /api/v1/admin/store/subscriptions/{bundle}", s.adminOrToken("control", s.handleDeleteSubscription))
	mux.Handle("GET /api/v1/admin/device-tokens", s.admin(s.handleListDeviceTokens))
	mux.Handle("POST /api/v1/admin/device-tokens", s.admin(s.handleCreateDeviceToken))
	mux.Handle("DELETE /api/v1/admin/device-tokens/{id}", s.admin(s.handleRevokeDeviceToken))

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
		if !s.checkAdmin(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid admin key"})
			return
		}
		next(w, r)
	})
}

// checkAdmin reports whether the request carries the admin key, via bearer
// token or the dashboard session cookie. Client-supplied headers are never
// sufficient on their own (they are trivially forgeable by non-browser
// clients).
func (s *Server) checkAdmin(r *http.Request) bool {
	if key, ok := bearerToken(r); ok && auth.ConstantTimeEqual(key, s.adminKey) {
		return true
	}
	if cookie, err := r.Cookie("sidey_session"); err == nil && auth.ConstantTimeEqual(cookie.Value, s.adminKey) {
		return true
	}
	return false
}

// adminOrToken grants access to the admin key or to a device token carrying
// the required scope ("read" or "control"; control implies read). This is
// the mobile-companion surface: narrow, revocable, auditable.
func (s *Server) adminOrToken(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey != "" && s.checkAdmin(r) {
			next(w, r)
			return
		}
		if key, ok := bearerToken(r); ok {
			if _, ok := s.checkDeviceToken(r.Context(), key, scope); ok {
				next(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials or scope"})
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
	// Never expose the admin key: it was previously returned here in
	// plaintext to any caller. Clients learn nothing except whether auth
	// is configured.
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": s.adminKey != "",
	})
}

