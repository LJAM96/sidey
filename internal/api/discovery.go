package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"sidey/internal/audit"
	"sidey/internal/auth"
)

// Companion discovery and one-tap enrollment (Phase 6).
//
// Two public, unauthenticated endpoints let a companion app find this
// control plane and pair with one tap:
//
//	GET  /api/v1/server-identity   stable {id, name, version}
//	POST /api/v1/devices/enroll    mint a scoped device token (source-gated)
//
// mDNS advertisement of the same identity (_sidey._tcp) is handled by
// discovery_mdns.go. The tailnet has no mDNS, so companions verify
// tailnet candidates against server-identity instead.

// defaultEnrollCIDRs is the default source allow-list for automatic
// enrollment: loopback, RFC 1918, IPv6 loopback/link-local, and the
// Tailscale CGNAT range (100.64.0.0/10). Public sources are never
// eligible. Override with SIDEY_ENROLL_CIDRS (comma-separated CIDRs).
var defaultEnrollCIDRs = []string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"::1/128",
	"fe80::/10",
}

// parseEnrollCIDRs parses a comma-separated CIDR list. An empty string
// yields the defaults. Invalid entries are rejected with an error so a
// typo cannot silently widen enrollment to the world.
func parseEnrollCIDRs(csv string) ([]*net.IPNet, error) {
	if strings.TrimSpace(csv) == "" {
		csv = strings.Join(defaultEnrollCIDRs, ",")
	}
	var out []*net.IPNet
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		out = append(out, cidr)
	}
	if len(out) == 0 {
		return nil, errNoCIDRs
	}
	return out, nil
}

var errNoCIDRs = errorString("no enrollment CIDRs configured")

type errorString string

func (e errorString) Error() string { return string(e) }

// clientIP extracts the remote IP from the connection. X-Forwarded-For is
// deliberately ignored: it is client-controlled, and trusting it would let
// any public caller claim a private source address. Behind the gateway the
// peer is the gateway's RFC 1918 address, which is eligible anyway.
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	host = strings.Trim(host, "[]")
	// Zone identifiers (fe80::1%eth0) are irrelevant to CIDR matching
	// and rejected by ParseIP; strip them.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// ipLimiter is a fixed-window per-IP rate limiter for the public enroll
// endpoint: max events per window per source IP, lazily evicted.
type ipLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPLimiter(max int, window time.Duration) *ipLimiter {
	return &ipLimiter{max: max, window: window, hits: make(map[string][]time.Time)}
}

// allow reports whether ip may proceed, recording the attempt.
func (l *ipLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	// Opportunistic eviction so idle IPs do not accumulate.
	if len(l.hits) > 4096 {
		for k, v := range l.hits {
			if len(v) == 0 {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// serverIdentityRow reads the single stable identity row.
func (s *Server) serverIdentityRow(ctx context.Context) (uuid.UUID, string, error) {
	var (
		id   uuid.UUID
		name string
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT id, name FROM server_identity LIMIT 1`).Scan(&id, &name); err != nil {
		return uuid.Nil, "", err
	}
	return id, name, nil
}

// handleServerIdentity is public and unauthenticated: companions need the
// stable ID before they hold any credential (that is the point).
func (s *Server) handleServerIdentity(w http.ResponseWriter, r *http.Request) {
	id, name, err := s.serverIdentityRow(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "identity unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id.String(), "name": name, "version": 1,
	})
}

// mintDeviceTokenRow mints a scoped device token using the same scheme as
// dashboard issuance (bcrypt hash behind a sha256 key id). The plaintext
// secret is returned for single delivery to the caller and never logged.
func (s *Server) mintDeviceTokenRow(ctx context.Context, name string, scopes []string, deviceID *uuid.UUID, expires *time.Time) (id uuid.UUID, secret string, created string, err error) {
	secret, err = auth.GenerateSecret()
	if err != nil {
		return uuid.Nil, "", "", err
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		return uuid.Nil, "", "", err
	}
	var ts time.Time
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO device_api_tokens (name, key_id, token_hash, scopes, device_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		name, auth.KeyID(secret), hash, scopes, deviceID, expires).
		Scan(&id, &ts); err != nil {
		return uuid.Nil, "", "", err
	}
	return id, secret, ts.UTC().Format(time.RFC3339), nil
}

// handleEnrollDevice mints a scoped companion token for one tap on the
// phone. No credential is required; instead the source network must be
// eligible (loopback/RFC1918/tailnet by default) and the caller is rate
// limited. The token carries the control scope the companion needs for
// refresh/2FA/import/upload; read is implied.
func (s *Server) handleEnrollDevice(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	allowed := false
	if ip != nil {
		for _, cidr := range s.enrollCIDRs {
			if cidr.Contains(ip) {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		s.logger.Warn("enroll refused: ineligible source", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "automatic enrollment is not allowed from this network"})
		return
	}
	if !s.enrollLimit.allow(ip.String()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many enrollment attempts; try again later"})
		return
	}
	var req struct {
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
		AppVersion string `json:"app_version"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.DeviceName = strings.TrimSpace(req.DeviceName)
	req.Platform = strings.TrimSpace(req.Platform)
	if req.DeviceName == "" || len(req.DeviceName) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device_name is required (max 128 chars)"})
		return
	}
	if req.Platform == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "platform is required"})
		return
	}
	id, secret, created, err := s.mintDeviceTokenRow(r.Context(), req.DeviceName, []string{"control"}, nil, nil)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "enrollment failed")
		return
	}
	srvID, srvName, _ := s.serverIdentityRow(r.Context())
	s.audit.Record(r.Context(), "enroll", "device_token.enrolled", audit.WithData(map[string]any{
		"token_id": id.String(), "name": req.DeviceName,
		"platform": req.Platform, "app_version": strings.TrimSpace(req.AppVersion),
	}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": secret, "device_id": nil, "created_at": created,
		"server_id": srvID.String(), "server_name": srvName,
		"note": "token is shown once; the companion stores it in the iOS Keychain",
	})
}
