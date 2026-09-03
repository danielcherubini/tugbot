// Package dbmigrate is the in-tree PostgreSQL migration runner (ADR 0002 —
// no golang-migrate dependency).
//
// Run applies <migDir>/*.up.sql lexicographically, one transaction per file,
// recording version = file basename without ".up.sql" into the
// schema_migrations tracker it owns.
//
// First-run sentinel (critical): a database provisioned by the OLD diesel
// history already contains every table (including servers) with no rows in
// schema_migrations. Executing the baseline file's raw CREATEs there would
// collide, so when the tracker is empty AND the `servers` table exists, the
// 000001_baseline file's DDL is skipped and its row stamped applied instead;
// later files still run. On a clean DB (no servers table) the baseline
// executes normally in its own transaction.
package dbmigrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoMigrations is returned when the directory holds no *.up.sql files.
var ErrNoMigrations = errors.New("no .up.sql migration files found")

const ensureTracker = `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz DEFAULT now()
	)
`

// Run applies every unapplied migration in migDir (lexicographic order),
// one transaction per file (DDL + tracker row commit atomically). Callers
// that need a longer window should wrap ctx — e.g. 30s — with
// context.WithTimeout to mirror r2d2's 30s connection_timeout, which
// pgxpool has no direct equivalent for.
func Run(ctx context.Context, pool *pgxpool.Pool, migDir string) error {
	log := slog.Default().With("module", "dbmigrate")

	if _, err := pool.Exec(ctx, ensureTracker); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	if err != nil {
		return fmt.Errorf("glob %s: %w", migDir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("%s: %w", migDir, ErrNoMigrations)
	}
	sort.Strings(files)

	// First-run sentinel, decided exactly once before the loop.
	sentinelSkipBaseline := false
	var tracked int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&tracked); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if tracked == 0 {
		var serversExists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'servers'
			)
		`).Scan(&serversExists); err != nil {
			return fmt.Errorf("check servers table: %w", err)
		}
		if serversExists {
			sentinelSkipBaseline = true
			log.Warn("first run on a pre-provisioned database (table \"servers\" exists) — " +
				"000001_baseline DDL will be skipped and its row stamped applied")
		}
	}

	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".up.sql")

		var alreadyApplied bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if alreadyApplied {
			log.Debug("migration already applied", "version", version)
			continue
		}

		skipDDL := sentinelSkipBaseline && version == "000001_baseline"
		if err := withTx(ctx, pool, func(tx pgx.Tx) error {
			if !skipDDL {
				b, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("read %s: %w", file, err)
				}
				if _, err := tx.Exec(ctx, string(b)); err != nil {
					return fmt.Errorf("migrate %s: %w", version, err)
				}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO schema_migrations (version, applied_at)
				VALUES ($1, now())
			`, version); err != nil {
				return fmt.Errorf("record %s: %w", version, err)
			}
			return nil
		}); err != nil {
			return err
		}
		if skipDDL {
			log.Warn("skipped baseline DDL (pre-provisioned DB), stamped applied", "version", version)
		} else {
			log.Info("applied migration", "version", version)
		}
	}
	return nil
}

// withTx runs fn inside a single transaction, rolling back on error.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("%w; rollback also failed: %v", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}
