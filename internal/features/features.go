// Package features mirrors src/features/mod.rs of the Rust bot.
//
// Table shape: features(id serial PK, name varchar(255) unique not null,
// enabled boolean default false not null).
//
// Semantics (parities with Rust):
//   - CheckEnabled propagates a DB failure as an error, but a missing row
//     returns (false, nil) — Rust's `.optional().unwrap_or(false)` — which is
//     what makes an unregistered feature "disabled" on a fresh DB rather than
//     a DB error.
//   - IsEnabled is the background-task flavor: every failure (including a
//     missing table) is logged and reported as false.
//   - Update errors when 0 rows were affected (Rust's
//     `bail!("Feature '{}' not found in database")`).
package features

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Feature is one row of the features table.
type Feature struct {
	ID      int32
	Name    string
	Enabled bool
}

// ErrNotFound is returned by Update when the feature has no row.
var ErrNotFound = errors.New("Feature not found in database")

// notFoundError carries the feature name while keeping identifiable via
// errors.Is(err, ErrNotFound); its message is Rust's bail text
// (mod.rs:63-64: `bail!("Feature '{}' not found in database", name)`).
func newNotFound(name string) error { return &notFoundError{name: name} }

type notFoundError struct{ name string }

func (e *notFoundError) Error() string {
	return fmt.Sprintf("Feature %q not found in database", e.name)
}
func (e *notFoundError) Unwrap() error { return ErrNotFound }

// All lists every feature (Rust `Features::all`).
func All(ctx context.Context, pool *pgxpool.Pool) ([]Feature, error) {
	rows, err := pool.Query(ctx, `SELECT id, name, enabled FROM features`)
	if err != nil {
		return nil, fmt.Errorf("failed to get features: %w", err)
	}
	defer rows.Close()
	out := make([]Feature, 0)
	for rows.Next() {
		var f Feature
		if err := rows.Scan(&f.ID, &f.Name, &f.Enabled); err != nil {
			return nil, fmt.Errorf("failed to scan feature row: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CheckEnabled reports whether the feature is enabled, returning an error when
// the database is unreachable. A missing row is not an error: an unregistered
// feature is "disabled" on a fresh DB (Rust's `.optional().unwrap_or(false)`).
func CheckEnabled(ctx context.Context, pool *pgxpool.Pool, key string) (bool, error) {
	var enabled bool
	err := pool.QueryRow(ctx, `SELECT enabled FROM features WHERE name = $1`, key).Scan(&enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("failed to query feature %q: %w", key, err)
	}
	return enabled, nil
}

// IsEnabled reports whether the feature is enabled, silently returning false on
// any error — for background tasks that must not crash on DB errors.
func IsEnabled(ctx context.Context, pool *pgxpool.Pool, key string) bool {
	on, err := CheckEnabled(ctx, pool, key)
	if err != nil {
		slog.Error("error checking feature", "module", "features", "feature", key, "error", err)
		return false
	}
	return on
}

// Update sets the enabled flag for an existing feature, erroring when the
// feature has no row (mirrors Rust's rows_affected == 0 bail).
func Update(ctx context.Context, pool *pgxpool.Pool, key string, enabled bool) error {
	tag, err := pool.Exec(ctx, `UPDATE features SET enabled = $1 WHERE name = $2`, enabled, key)
	if err != nil {
		// Rust mod.rs:52-56: pool connection failure bails with a fixed text.
		return errors.New("Failed to get database connection from pool")
	}
	if tag.RowsAffected() == 0 {
		// Rust mod.rs:63-64: bail!("Feature '{}' not found in database", name)
		return newNotFound(key)
	}
	return nil
}
