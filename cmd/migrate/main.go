// Command migrate runs the in-tree PostgreSQL migration runner (ADR 0002) and
// exits. It reads DATABASE_URL (required) and MIGRATIONS_DIR (default
// "migrations").
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielcherubini/tugbot/internal/config"
	"github.com/danielcherubini/tugbot/internal/db"
	"github.com/danielcherubini/tugbot/internal/dbmigrate"
)

func main() {
	// RUST_LOG maps to the slog level (the env var name is kept on purpose —
	// existing journalctl workflows rely on it).
	level := config.LogLevelFromEnv()

	base := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(base)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL must be set", "module", "migrate")
		os.Exit(1)
	}
	migDir := os.Getenv("MIGRATIONS_DIR")
	if migDir == "" {
		migDir = "migrations"
	}

	// 30s deadline mirroring r2d2's 30s acquire timeout (pgxpool has no
	// connection_timeout equivalent — see internal/db).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		// Allow Ctrl-C to abort the upgrade early.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()

	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		slog.Error("failed to create database pool", "module", "migrate", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := dbmigrate.Run(ctx, pool, migDir); err != nil {
		slog.Error("migration run failed", "module", "migrate", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations complete", "module", "migrate", "dir", migDir)
}
