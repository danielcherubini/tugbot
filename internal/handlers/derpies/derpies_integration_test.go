package derpies

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/dbmigrate"
)

// TestMigration000002AppliesAndSeeds — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run against the test PG and asserts
// the seeded state.
func TestMigration000002AppliesAndSeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := os.Getenv("TUGBOT_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@127.0.0.1:5432/tugbot_test"
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("cannot create pool: %v (is the compose PG running?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v (is the compose PG running?)", err)
	}
	t.Cleanup(pool.Close)

	// 1. Run the real migration file (not an inline copy) from temp dir.
	dir := t.TempDir()
	src, err := os.ReadFile("../../../migrations/000002_derpies_gimmicks.up.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000002_derpies_gimmicks.up.sql"), src, 0o644); err != nil {
		t.Fatalf("write migration to temp dir: %v", err)
	}

	// 2. Precondition (the shared test DB may already have any of these;
	//    never TRUNCATE features — other packages own it). The DROP ...
	//    CASCADE first makes the test rerunnable on the same test DB. The
	//    features table (with the UNIQUE (name) constraint) must exist
	//    before the run: the migration's ON CONFLICT (name) requires it,
	//    and 000001_baseline is not in this test dir. Never touch
	//    features rows.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_gimmicks CASCADE;
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS features (
			id serial PRIMARY KEY,
			name character varying(255) UNIQUE NOT NULL,
			enabled boolean DEFAULT false NOT NULL
		);
		DELETE FROM schema_migrations WHERE version = '000002_derpies_gimmicks';
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// 3. The real migration file runs clean.
	if err := dbmigrate.Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 5. Clean up (leave features intact — its derpies row is idempotent
	//    ON CONFLICT DO NOTHING state).
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			DELETE FROM derpies_gimmicks;
			DELETE FROM schema_migrations WHERE version = '000002_derpies_gimmicks';
		`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// 4. Asserts.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM derpies_gimmicks`).Scan(&n); err != nil {
		t.Fatalf("row count: %v", err)
	}
	if n != 5 {
		t.Errorf("derpies_gimmicks rows = %d, want 5", n)
	}

	rows, err := pool.Query(ctx, `SELECT word FROM derpies_gimmicks`)
	if err != nil {
		t.Fatalf("word query: %v", err)
	}
	defer rows.Close()
	var words []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			t.Fatalf("word scan: %v", err)
		}
		words = append(words, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("words iteration: %v", err)
	}
	sort.Strings(words)
	wantWords := []string{"bike", "buy", "give", "swift", "zswift"}
	if len(words) != len(wantWords) {
		t.Fatalf("words = %v, want %v", words, wantWords)
	}
	for i := range wantWords {
		if words[i] != wantWords[i] {
			t.Errorf("words = %v, want %v", words, wantWords)
		}
	}

	var nonSeed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM derpies_gimmicks WHERE source <> 'seed'`).Scan(&nonSeed); err != nil {
		t.Fatalf("non-seed count: %v", err)
	}
	if nonSeed != 0 {
		t.Errorf("rows with source != 'seed' = %d, want 0", nonSeed)
	}

	checkConstraint := func(name string, wantType string) {
		t.Helper()
		var got string
		err := pool.QueryRow(ctx,
			`SELECT contype::text FROM pg_constraint WHERE conname = $1`, name).Scan(&got)
		if err != nil {
			t.Errorf("constraint %s: %v (missing?)", name, err)
			return
		}
		if got != wantType {
			t.Errorf("constraint %s contype = %q, want %q", name, got, wantType)
		}
	}
	checkConstraint("derpies_gimmicks_word_key", "u")
	checkConstraint("derpies_gimmicks_pkey", "p")
}
