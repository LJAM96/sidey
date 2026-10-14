package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"sidey/internal/auth"
)

// Pure unit tests (no database).

func TestParseEnrollCIDRsDefaults(t *testing.T) {
	cidrs, err := parseEnrollCIDRs("")
	if err != nil {
		t.Fatal(err)
	}
	allow := []string{"127.0.0.1", "10.1.2.3", "172.16.9.9", "192.168.31.212", "100.100.12.70", "::1"}
	for _, s := range allow {
		ip := net.ParseIP(s)
		ok := false
		for _, c := range cidrs {
			if c.Contains(ip) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("expected %s to be enrollable", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "150.136.227.21"} {
		ip := net.ParseIP(s)
		for _, c := range cidrs {
			if c.Contains(ip) {
				t.Errorf("expected %s to be refused", s)
			}
		}
	}
}

func TestParseEnrollCIDRsInvalid(t *testing.T) {
	if _, err := parseEnrollCIDRs("nope"); err == nil {
		t.Error("expected error for garbage CIDR")
	}
	if _, err := parseEnrollCIDRs("192.168.1.1"); err == nil {
		t.Error("expected error for bare IP without mask")
	}
	if _, err := parseEnrollCIDRs("10.0.0.0/8, 192.168.0.0/16"); err != nil {
		t.Error(err)
	}
}

func TestClientIP(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5:8080":  "192.168.1.5",
		"127.0.0.1:1234":    "127.0.0.1",
		"[::1]:8080":        "::1",
		"[fe80::1%eth0]:80": "fe80::1",
		"10.0.0.9":          "10.0.0.9",
		"not-an-ip:1234":    "",
	}
	for remote, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		got := clientIP(r)
		if want == "" {
			if got != nil {
				t.Errorf("%s: expected nil, got %v", remote, got)
			}
			continue
		}
		if got == nil || got.String() != net.ParseIP(want).String() {
			t.Errorf("%s: got %v want %s", remote, got, want)
		}
	}
}

func TestDiscoveryTXT(t *testing.T) {
	txt := discoveryTXT("srv-1", "Garage")
	want := map[string]bool{"id=srv-1": true, "v=1": true, "name=Garage": true}
	if len(txt) != len(want) {
		t.Fatalf("txt = %v", txt)
	}
	for _, e := range txt {
		if !want[e] {
			t.Errorf("unexpected entry %q", e)
		}
	}
}

func upFlags(extra ...string) net.Interface {
	var flags net.Flags = net.FlagUp | net.FlagMulticast
	for _, e := range extra {
		switch e {
		case "loopback":
			flags |= net.FlagLoopback
		case "nomcast":
			flags &^= net.FlagMulticast
		case "down":
			flags &^= net.FlagUp
		}
	}
	return net.Interface{Flags: flags}
}

func TestDiscoveryInterfaces(t *testing.T) {
	all := []net.Interface{
		{Name: "good", Flags: upFlags().Flags},
		{Name: "down", Flags: upFlags("down").Flags},
		{Name: "lo", Flags: upFlags("loopback").Flags},
		{Name: "nomcast", Flags: upFlags("nomcast").Flags},
	}
	got := discoveryInterfaces(all)
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("got %v", got)
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("192.168.1.5") {
			t.Fatalf("attempt %d blocked", i)
		}
	}
	if l.allow("192.168.1.5") {
		t.Error("4th attempt should be blocked")
	}
	if !l.allow("192.168.1.6") {
		t.Error("other IP should be unaffected")
	}
	fast := newIPLimiter(1, 20*time.Millisecond)
	if !fast.allow("10.0.0.1") || fast.allow("10.0.0.1") {
		t.Fatal("window accounting wrong")
	}
	time.Sleep(30 * time.Millisecond)
	if !fast.allow("10.0.0.1") {
		t.Error("window should have reset")
	}
}

func testDiscoveryServer() *Server {
	cidrs, _ := parseEnrollCIDRs("")
	return &Server{
		logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		enrollCIDRs: cidrs,
		enrollLimit: newIPLimiter(10, time.Minute),
	}
}

func TestEnrollSourceGate(t *testing.T) {
	s := testDiscoveryServer()
	for _, remote := range []string{"8.8.8.8:443", "150.136.227.21:8080", "[2001:db8::1]:80"} {
		req := httptest.NewRequest("POST", "/api/v1/devices/enroll",
			strings.NewReader(`{"device_name":"x","platform":"ios"}`))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.handleEnrollDevice(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", remote, rec.Code)
		}
	}
}

func TestEnrollValidation(t *testing.T) {
	s := testDiscoveryServer()
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty name", `{"device_name":"","platform":"ios"}`, http.StatusBadRequest},
		{"missing name", `{"platform":"ios"}`, http.StatusBadRequest},
		{"missing platform", `{"device_name":"x"}`, http.StatusBadRequest},
		{"bad json", `{"device_name":`, http.StatusBadRequest},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/v1/devices/enroll", strings.NewReader(c.body))
		req.RemoteAddr = "192.168.1.5:9999"
		rec := httptest.NewRecorder()
		s.handleEnrollDevice(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

// Integration tests below run against the shared test database via the
// standard harness (DATABASE_URL); the whole suite skips without it.

// enrollDevice enrolls through the public endpoint like the companion app.
func enrollDevice(t *testing.T, name string) (token, id string) {
	t.Helper()
	res, body := doJSON(t, "POST", "/api/v1/devices/enroll", "", map[string]any{
		"device_name": name, "platform": "ios", "app_version": "1.0",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("enroll: %d %v", res.StatusCode, body)
	}
	token, _ = body["token"].(string)
	if token == "" {
		t.Fatalf("enroll response missing token: %v", body)
	}
	// The row id is not returned (only the secret is delivered once);
	// resolve it via the key id for the revoke step.
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM device_api_tokens WHERE key_id = $1`,
		auth.KeyID(token)).Scan(&id); err != nil {
		t.Fatalf("enroll row lookup: %v", err)
	}
	if _, ok := body["server_id"].(string); !ok {
		t.Fatalf("enroll response missing server_id: %v", body)
	}
	return token, id
}

func TestServerIdentityStable(t *testing.T) {
	_, first := doJSON(t, "GET", "/api/v1/server-identity", "", nil)
	_, second := doJSON(t, "GET", "/api/v1/server-identity", "", nil)
	id1, _ := first["id"].(string)
	id2, _ := second["id"].(string)
	if id1 == "" || id1 != id2 {
		t.Fatalf("identity not stable: %q vs %q", id1, id2)
	}
	if first["version"] != float64(1) {
		t.Fatalf("version = %v", first["version"])
	}
	if first["name"] == "" {
		t.Fatal("empty server name")
	}
}

func TestEnrollRoundTrip(t *testing.T) {
	token, id := enrollDevice(t, "test-iphone")
	// The minted token authenticates like a dashboard token (control).
	res, _ := doJSON(t, "GET", "/api/v1/dashboard/devices", token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard with enrolled token: %d", res.StatusCode)
	}
	// Control scope accepted: missing artifact → 404, not 401, no writes.
	res, _ = doJSON(t, "PATCH", "/api/v1/artifacts/00000000-0000-0000-0000-000000000000", token,
		map[string]any{"state": "approved"})
	if res.StatusCode == http.StatusUnauthorized {
		t.Fatal("enrolled token denied control scope")
	}
	// Revoke kills it.
	res, _ = doJSON(t, "DELETE", "/api/v1/admin/device-tokens/"+id, adminKey, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d", res.StatusCode)
	}
	res, _ = doJSON(t, "GET", "/api/v1/dashboard/devices", token, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token still accepted: %d", res.StatusCode)
	}
}
