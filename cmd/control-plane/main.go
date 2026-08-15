// Control plane entry point (Phase D): REST API, job queue and dashboard.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"sidey/internal/api"
	"sidey/internal/artifacts"
	"sidey/internal/audit"
	"sidey/internal/jobs"
	"sidey/internal/retention"
	"sidey/internal/scheduler"
	"sidey/internal/storage"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	pool, err := storage.Open(ctx, databaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	auditClient := audit.New(pool, logger)

	lease := time.Duration(envInt("JOB_LEASE_SECONDS", 120)) * time.Second
	refreshLead := time.Duration(envInt("REFRESH_LEAD_DAYS", 2)*24) * time.Hour
	jobService := jobs.NewService(pool, auditClient, lease, jobs.WithRefreshLead(refreshLead))

	// Immutable IPA repository (Phase E).
	artifactDir := envStr("ARTIFACT_DIR", "/var/lib/sidey/artifacts")
	artifactStore := artifacts.NewStore(artifactDir)
	if err := artifactStore.EnsureDir(); err != nil {
		logger.Warn("artifact dir not writable", "dir", artifactDir, "error", err)
	}

	server := api.NewServerWithLimits(pool, logger, auditClient, jobService, artifactStore,
		os.Getenv("SIDEY_ADMIN_API_KEY"), int64(envInt("SIDEY_MAX_ARTIFACT_BYTES", 4<<30)))

	// Refresh scheduler: enqueues refresh jobs for deployments whose profile
	// is within the lead window of expiry.
	sched := scheduler.NewService(pool, logger, jobService, auditClient, refreshLead)
	schedInterval := time.Duration(envInt("SCHEDULER_INTERVAL_SECONDS", 300)) * time.Second
	go func() {
		runTick := func() {
			created, err := sched.Run(ctx)
			if err != nil {
				logger.Warn("refresh scheduling failed", "error", err)
				return
			}
			if created > 0 {
				logger.Info("scheduled refresh jobs", "count", created)
			}
		}
		runTick() // one pass immediately at startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(schedInterval):
				runTick()
			}
		}
	}()

	// Lease reaper: resets expired job leases and marks stale agents offline.
	reapInterval := time.Duration(envInt("REAP_INTERVAL_SECONDS", 15)) * time.Second
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reapInterval):
				reaped, err := jobService.Reap(ctx)
				if err != nil {
					logger.Warn("reap failed", "error", err)
					continue
				}
				if reaped > 0 {
					logger.Info("reaped expired job leases", "count", reaped)
				}
			}
		}
	}()

	// Signed-derivative retention: original IPAs are kept forever, but a
	// signed derivative is only useful while its embedded profile is valid.
	// Rows whose profile expired beyond the retention window are pruned once
	// a day, together with the files nothing else references.
	retentionDays := envInt("SIGNED_ARTIFACT_RETENTION_DAYS", 30)
	pruneInterval := time.Duration(envInt("PRUNE_INTERVAL_SECONDS", 86400)) * time.Second
	go func() {
		prune := func() {
			pruned, err := retention.PruneSignedArtifacts(ctx, pool, auditClient, artifactStore,
				time.Duration(retentionDays)*24*time.Hour)
			if err != nil {
				logger.Warn("signed-artifact prune failed", "error", err)
				return
			}
			if pruned > 0 {
				logger.Info("pruned expired signed artifacts", "rows", pruned, "retention_days", retentionDays)
			}
		}
		prune() // one pass at startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pruneInterval):
				prune()
			}
		}
	}()

	port := envOr("PORT", "8080")
	// Timeouts are part of the hardening contract: ReadTimeout caps slow
	// request bodies, WriteTimeout prevents a stuck upstream from holding a
	// connection open indefinitely, and IdleTimeout recycles idle keep-alive
	// connections so slowloris-style resource exhaustion is bounded.
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		logger.Info("control plane listening", "port", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Same-host device service channel (ADR-0008). When SIDEY_DEVICE_SOCKET
	// is set, the control plane exposes the device job/control endpoints on a
	// Unix socket whose parent directory is restricted to this process's
	// owner (0700). The local device service is trusted without a bearer key;
	// the remote-node path (agent keys) remains unchanged for optional
	// multi-site deployments.
	deviceSocket := envOr("SIDEY_DEVICE_SOCKET", "/run/sidey/device.sock")
	deviceServer, err := listenDeviceSocket(deviceSocket, server)
	if err != nil {
		logger.Warn("device socket not started", "path", deviceSocket, "error", err)
	} else if deviceServer != nil {
		logger.Info("device socket listening", "path", deviceSocket)
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if deviceServer != nil {
		_ = deviceServer.Shutdown(shutdownCtx)
		_ = os.Remove(deviceSocket)
	}
}

// listenDeviceSocket prepares the Unix socket directory (0700), removes any
// stale socket file from a previous run, and serves the device handler. The
// socket file itself is created by the OS with the process umask; the
// directory mode is what excludes other local users.
func listenDeviceSocket(socketPath string, server *api.Server) (*http.Server, error) {
	if socketPath == "" {
		return nil, nil
	}
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	deviceServer := &http.Server{
		Handler:           server.DeviceHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		if err := deviceServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("device socket server failed", "error", err)
		}
	}()
	return deviceServer, nil
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envStr(name string, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
