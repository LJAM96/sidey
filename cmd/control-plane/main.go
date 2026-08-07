// Control plane entry point (Phase D): REST API, job queue and dashboard.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sidey/internal/api"
	"sidey/internal/audit"
	"sidey/internal/jobs"
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
	jobService := jobs.NewService(pool, auditClient, lease)

	server := api.NewServer(pool, logger, auditClient, jobService, os.Getenv("SIDEY_ADMIN_API_KEY"))

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

	port := envOr("PORT", "8080")
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("control plane listening", "port", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
