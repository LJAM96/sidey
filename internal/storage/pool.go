// Package storage provides the PostgreSQL connection used by the control plane.
package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a connection pool. If databaseURL carries no password and
// DB_PASSWORD_FILE points at a file, the password is injected into the URL.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if passwordFile := os.Getenv("DB_PASSWORD_FILE"); passwordFile != "" {
		u, err := url.Parse(databaseURL)
		if err == nil && u.User != nil {
			if _, has := u.User.Password(); !has {
				password, err := os.ReadFile(passwordFile)
				if err != nil {
					return nil, fmt.Errorf("read DB_PASSWORD_FILE: %w", err)
				}
				u.User = url.UserPassword(u.User.Username(), strings.TrimSpace(string(password)))
				databaseURL = u.String()
			}
		}
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
