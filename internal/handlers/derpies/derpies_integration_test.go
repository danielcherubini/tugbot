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

// TestMigration000003AppliesAndSeeds — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run against the test PG and asserts
// the seeded state: exactly ONE row in derpies_prompt whose body is
// the code-pinned default template BYTE-FOR-BYTES (the forever sync
// guard between the migration file's SQL literal and the Go constant —
// the file escapes the single apostrophe, the row carries the raw
// form).
func TestMigration000003AppliesAndSeeds(t *testing.T) {
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
	src, err := os.ReadFile("../../../migrations/000003_derpies_prompt.up.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000003_derpies_prompt.up.sql"), src, 0o644); err != nil {
		t.Fatalf("write migration to temp dir: %v", err)
	}

	// 2. Precondition (the shared test DB may already have any of these;
	//    never TRUNCATE features — other packages own it). The DROP ...
	//    CASCADE first makes the test rerunnable on the same test DB.
	//    Never touch the features rows or derpies_gimmicks.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_prompt CASCADE;
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz DEFAULT now()
		);
		DELETE FROM schema_migrations WHERE version = '000003_derpies_prompt';
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// 3. The real migration file runs clean.
	if err := dbmigrate.Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 4. Clean up (leave schema_migrations' other rows and the 000002
	//    tables intact — this test only owns its own version row + table).
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			DELETE FROM derpies_prompt;
			DELETE FROM schema_migrations WHERE version = '000003_derpies_prompt';
		`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// 5. Asserts.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM derpies_prompt`).Scan(&n); err != nil {
		t.Fatalf("row count: %v", err)
	}
	if n != 1 {
		t.Fatalf("derpies_prompt rows = %d, want 1", n)
	}

	var body string
	var bodyLen int
	if err := pool.QueryRow(ctx, `SELECT body, length(body) FROM derpies_prompt LIMIT 1`).Scan(&body, &bodyLen); err != nil {
		t.Fatalf("body query: %v", err)
	}
	// The forever sync guard: the seeded body is the Go constant,
	// byte-for-byte.
	if body != defaultPromptTemplate {
		t.Errorf("seed body != defaultPromptTemplate constant (first differing run: %d bytes of seed, %d bytes of constant)", len(body), len(defaultPromptTemplate))
	}
	if bodyLen < 1000 {
		t.Errorf("length(body) = %d, want > 1000", bodyLen)
	}
}

// TestMigration000004AppliesAndSeeds — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run against the test PG and asserts
// the seeded state: all nine "cog"-family respellings present in
// derpies_gimmicks with source='seed' and nothing else (000004 inserts
// rows ONLY — the table shape comes from 000002's migration, so the
// precondition recreates it with the UNIQUE (word) constraint the
// INSERT relies on).
func TestMigration000004AppliesAndSeeds(t *testing.T) {
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
	src, err := os.ReadFile("../../../migrations/000004_derpies_cog_words.up.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000004_derpies_cog_words.up.sql"), src, 0o644); err != nil {
		t.Fatalf("write migration to temp dir: %v", err)
	}

	// 2. Precondition (the shared test DB may already have any of these;
	//    never TRUNCATE features — other packages own it). 000004 inserts
	//    rows ONLY, so derpies_gimmicks (normally created by 000002's
	//    migration, which this test dir does not include) must exist with
	//    the UNIQUE (word) constraint the INSERT depends on — CREATE IF
	//    NOT EXISTS keeps this rerunnable beside 000002's run on the same
	//    DB (which leaves the table intact and empty after its cleanup).
	//    The serial id mirrors the sequence default 000002's migration
	//    grants, so the fresh-table case can INSERT (word, source) too.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS derpies_gimmicks (
			id serial,
			word character varying(64) NOT NULL,
			source character varying(8) DEFAULT 'seed' NOT NULL,
			created_at timestamp without time zone DEFAULT now() NOT NULL,
			CONSTRAINT derpies_gimmicks_word_key UNIQUE (word)
		);
		DELETE FROM schema_migrations WHERE version = '000004_derpies_cog_words';
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// 3. The real migration file runs clean.
	if err := dbmigrate.Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 4. Clean up (leave the table and features alone — this test only
	//    owns its version row and its seeded words).
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			DELETE FROM derpies_gimmicks;
			DELETE FROM schema_migrations WHERE version = '000004_derpies_cog_words';
		`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// 5. Asserts.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM derpies_gimmicks`).Scan(&n); err != nil {
		t.Fatalf("row count: %v", err)
	}
	if n != 9 {
		t.Errorf("derpies_gimmicks rows = %d, want 9", n)
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
	wantWords := []string{"c0g", "c0gs", "cog", "coggs", "cogs", "coq", "coqs", "kog", "kogs"}
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
}
