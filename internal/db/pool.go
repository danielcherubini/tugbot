// Package db is the PostgreSQL data layer: the pgx connection pool
// (mirroring Rust's r2d2 pool with max_size=15) and the sqlc-generated
// queries for all 10 tables.
package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxPoolSize mirrors Rust's r2d2 pool `max_size(15)`.
const MaxPoolSize = 15

// NewPool creates a pgxpool with MaxPoolSize (15) connections and pings it,
// so a bad DATABASE_URL surfaces at startup (Rust parity: establish_pool()
// expects the pool to work right away).
//
// Timeouts (do not fake the knob): pgxpool has no direct connection_timeout
// equivalent to r2d2's 30s acquire timeout. Every long-lived Acquire call
// site (background loop startup, cmd/migrate) MUST wrap its context with a
// 30s context.WithTimeout to mirror that behavior. NewPool itself returns
// without blocking (pgxpool connects lazily); a startup call with a bounded
// context bounds the initial Ping check.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", databaseURL, err)
	}
	cfg.MaxConns = MaxPoolSize
	slog.Debug("creating DB pool", "module", "db", "max_conns", MaxPoolSize)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping pool: %w", err)
	}
	return pool, nil
}
