package dbmigrate

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// skipIfNoPG + setup return a pool to a reset state, or skip the test when PG
// is unreachable.
func setupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := os.Getenv("TUGBOT_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://tugbot:tugbot@127.0.0.1:5432/tugbot_test"
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
	return pool
}

// resetMigrateState wipes all objects the test migrations AND the real
// migrations/ chain (000001-000008) create, so both kinds of test start
// deterministic on the shared test DB. The list is verified against the
// actual migrations/*.up.sql files (the 000001 baseline creates the
// job_status TYPE — not a table — the two diesel functions, its tables and
// the later files' tables; 000004 only INSERTs). Tables drop first (their
// indexes, triggers, owned sequences go with them), then the functions,
// then the type (a message_votes column references it).
func resetMigrateState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS schema_migrations;
		DROP TABLE IF EXISTS servers;
		DROP TABLE IF EXISTS mig_test_baseline_guard;
		DROP TABLE IF EXISTS extra_mig_test;
		DROP TABLE IF EXISTS __diesel_schema_migrations, ai_slop_usage, features,
			goku_poll_usage, gulag_users, gulag_votes, is_this_real_usage,
			message_votes, reversal_of_fortunes, user_activity,
			derpies_gimmicks, derpies_prompt, derpies_config, derpies_decisions,
			derpies_gimmick_phrases, strava_athletes, strava_seen_activities,
			strava_onboardings;
		DROP FUNCTION IF EXISTS public.diesel_manage_updated_at(regclass) CASCADE;
		DROP FUNCTION IF EXISTS public.diesel_set_updated_at() CASCADE;
		DROP TYPE IF EXISTS public.job_status CASCADE;
	`); err != nil {
		t.Fatalf("reset state: %v", err)
	}
}

// existingServers simulates a database provisioned by the OLD diesel history:
// the `servers` table exists but schema_migrations has no (yet) rows.
func existingServers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE servers (
			id serial PRIMARY KEY,
			guild_id bigint NOT NULL,
			gulag_id bigint NOT NULL
		);
		INSERT INTO servers (guild_id, gulag_id) VALUES (1, 2);
	`); err != nil {
		t.Fatalf("existingServers: %v", err)
	}
}

// writeTestMigrations renders the two test migration files into dir. The
// baseline DDL is a plain CREATE (no IF NOT EXISTS) so re-execution would
// collide — the trap that proves the sentinel skipped it.
func writeTestMigrations(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		// Note: this test migration deliberately does NOT touch the
		// `features` table — the features package tests own that table on
		// the same shared test database (the full gate runs the DB-touching
		// packages with -p 1; the features tests self-heal it with
		// CREATE TABLE IF NOT EXISTS after resetMigrateState drops it).
		"000001_baseline.up.sql": `
			CREATE TABLE servers (
				id serial PRIMARY KEY,
				guild_id bigint NOT NULL,
				gulag_id bigint NOT NULL
			);
			CREATE TABLE mig_test_baseline_guard (
				id serial PRIMARY KEY
			);
		`,
		"000002_extra.up.sql": "CREATE TABLE extra_mig_test (id serial PRIMARY KEY);",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func versionRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("versionRows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("versionRows scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

// TestSequelDatabaseSchemaMigrationsOwnership: the runner owns the tracker
// table — it is created by Run, not by the caller.
func TestRunCreatesSchemaMigrations(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)

	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("schema_migrations already exists before Run — bad reset?")
	}

	dir := t.TempDir()
	writeTestMigrations(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("schema_migrations was not created by Run")
	}
}

// TestCleanDBExecutesBaselineAndLaterMigrations: empty tracker + no servers
// table (clean/dev DB) -> every migration runs, and all versions recorded.
func TestCleanDBExecutesBaselineAndLaterMigrations(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)

	dir := t.TempDir()
	writeTestMigrations(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("schema_migrations rows = %d, want 2", n)
	}
	versions := versionRows(t, pool)
	sort.Strings(versions)
	if versions[0] != "000001_baseline" || versions[1] != "000002_extra" {
		t.Errorf("versions = %v, want [000001_baseline 000002_extra]", versions)
	}

	var serversCols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='servers' AND column_name='guild_id'
	`).Scan(&serversCols); err != nil {
		t.Fatalf("query cols: %v", err)
	}
	if serversCols != 1 {
		t.Error("baseline DDL was not executed: servers.guild_id column missing")
	}
}

// TestSentinelExistingTableSkipsBaselineDDL: empty tracker + servers table
// already present (diesel-provisioned DB) -> the baseline file's DDL is NOT
// executed (the plain CREATE would collide), its row is stamped applied, and
// later migrations still run.
func TestSentinelExistingTableSkipsBaselineDDL(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)
	existingServers(t, pool)

	dir := t.TempDir()
	writeTestMigrations(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v (a baseline DDL collision here proves the sentinel failed)", err)
	}

	// The pre-seeded row proves the DDL never ran the CREATE (it would have
	// failed on the existing table) and the table survived untouched.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM servers`).Scan(&rows); err != nil {
		t.Fatalf("servers count: %v", err)
	}
	if rows != 1 {
		t.Errorf("servers rows = %d, want 1 (seeded row must survive)", rows)
	}

	var stamped int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = '000001_baseline'`).Scan(&stamped); err != nil {
		t.Fatalf("stamped: %v", err)
	}
	if stamped != 1 {
		t.Error("baseline row was not stamped in schema_migrations")
	}

	// The later migration still ran.
	var extraCols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema='public' AND table_name='extra_mig_test'
	`).Scan(&extraCols); err != nil {
		t.Fatalf("extra cols: %v", err)
	}
	if extraCols != 1 {
		t.Error("000002_extra was not applied after a sentinel-stamped baseline")
	}
}

// TestIdempotentRerun: running the same migration set twice more is a no-op —
// no errors, no new rows, applied_at values unchanged.
func TestIdempotentRerun(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)
	existingServers(t, pool) // any state is fine; use sentinel shape

	dir := t.TempDir()
	writeTestMigrations(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Run(ctx, pool, dir); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	var firstApplied int64
	if err := pool.QueryRow(ctx, `
		SELECT extract(epoch from min(applied_at))::bigint FROM schema_migrations
	`).Scan(&firstApplied); err != nil {
		t.Fatalf("applied_at: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // ensure a re-apply would bump the timestamp

	for i := 0; i < 2; i++ {
		if err := Run(ctx, pool, dir); err != nil {
			t.Fatalf("rerun %d: Run() error = %v (must be a no-op)", i+1, err)
		}
	}
	var n2 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n2); err != nil {
		t.Fatalf("count: %v", err)
	}
	var secondApplied int64
	if err := pool.QueryRow(ctx, `
		SELECT extract(epoch from min(applied_at))::bigint FROM schema_migrations
	`).Scan(&secondApplied); err != nil {
		t.Fatalf("applied_at: %v", err)
	}
	if n != n2 {
		t.Errorf("rerun changed schema_migrations rows: %d -> %d", n, n2)
	}
	if n2 != 2 {
		t.Errorf("schema_migrations rows = %d, want 2", n2)
	}
	if firstApplied != secondApplied {
		t.Errorf("rerun re-stamped applied_at: %d -> %d", firstApplied, secondApplied)
	}
}

// TestNoMigrationsErrors: an empty migrations directory is an error, not a
// silent success.
func TestNoMigrationsErrors(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, pool, dir); err == nil {
		t.Error("Run() on empty dir: error = nil, want error")
	}
}

// TestStravaOnboardingTable: the REAL migration chain (the migrations/ dir,
// 000001-000008) applies cleanly through Run — the shared test DB may
// already have the real tables (the reset above wipes them; the runner's
// skip semantics make any leftover state a no-op) — and 000008 creates
// strava_onboardings with the shape the onboarding flow requires: a state
// PK, thread_id bigint NOT NULL, a nullable label, a status CHECK on the
// three values with a 'pending' default, a created_at defaulting to now(),
// an expires_at timestamptz NOT NULL, and an index on expires_at.
func TestStravaOnboardingTable(t *testing.T) {
	pool := setupPool(t)
	resetMigrateState(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "000008_strava_onboarding.up.sql")); err != nil {
		t.Fatalf("000008_strava_onboarding.up.sql missing: %v", err)
	}
	if err := Run(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("Run() on the real migrations dir: %v", err)
	}

	// The whole real chain applied: one schema_migrations row per .up.sql
	// file (7 today — 000007 is reserved for the derpies-slowmode plan).
	migFiles, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != len(migFiles) {
		t.Errorf("schema_migrations rows = %d, want %d (one per .up.sql file)", n, len(migFiles))
	}

	// state is the PRIMARY KEY (structural + enforced).
	var pkCols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.key_column_usage k
		JOIN information_schema.table_constraints c USING (constraint_name)
		WHERE c.constraint_type = 'PRIMARY KEY'
		  AND k.table_schema = 'public' AND k.table_name = 'strava_onboardings'
		  AND k.column_name = 'state'
	`).Scan(&pkCols); err != nil {
		t.Fatalf("pk query: %v", err)
	}
	if pkCols != 1 {
		t.Error("strava_onboardings has no PRIMARY KEY on state")
	}

	// Column shapes: thread_id bigint NOT NULL, label text NULL,
	// status text NOT NULL DEFAULT 'pending', created_at timestamptz
	// defaulting to now(), expires_at timestamptz NOT NULL.
	col := func(name string) (dataType, nullable, def string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable, coalesce(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'strava_onboardings' AND column_name = $1
		`, name).Scan(&dataType, &nullable, &def); err != nil {
			t.Fatalf("column %s: %v (table missing?)", name, err)
		}
		return dataType, nullable, def
	}
	if dt, null, _ := col("thread_id"); dt != "bigint" || null != "NO" {
		t.Errorf("thread_id = %s nullable=%s, want bigint NOT NULL", dt, null)
	}
	if dt, null, def := col("label"); dt != "text" || null != "YES" || def != "" {
		t.Errorf("label = %s nullable=%s default=%q, want text NULL (no default)", dt, null, def)
	}
	if dt, null, def := col("status"); dt != "text" || null != "NO" || def != "'pending'::text" {
		t.Errorf("status = %s nullable=%s default=%q, want text NOT NULL DEFAULT 'pending'", dt, null, def)
	}
	if dt, null, def := col("created_at"); dt != "timestamp with time zone" || null != "NO" || def != "now()" {
		t.Errorf("created_at = %s nullable=%s default=%q, want timestamptz NOT NULL DEFAULT now()", dt, null, def)
	}
	if dt, null, def := col("expires_at"); dt != "timestamp with time zone" || null != "NO" || def != "" {
		t.Errorf("expires_at = %s nullable=%s default=%q, want timestamptz NOT NULL (no default)", dt, null, def)
	}

	// The defaults apply and the PK is enforced — in one transaction (the
	// duplicate insert aborts the tx, so it goes last; the rollback leaves
	// no residue on the shared DB).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO strava_onboardings (state, thread_id, expires_at) VALUES ('s1', 1, now())`); err != nil {
		t.Fatalf("insert (status omitted): %v", err)
	}
	var gotStatus string
	var gotCreatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT status, created_at FROM strava_onboardings WHERE state = 's1'`).Scan(&gotStatus, &gotCreatedAt); err != nil {
		t.Fatalf("read back s1: %v", err)
	}
	if gotStatus != "pending" {
		t.Errorf("status default = %q, want 'pending'", gotStatus)
	}
	if gotCreatedAt.IsZero() {
		t.Error("created_at = NULL on a status-omitting insert, want the now() default")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO strava_onboardings (state, thread_id, expires_at) VALUES ('s1', 2, now())`); err == nil {
		t.Error("duplicate state inserted — the state PRIMARY KEY is not enforced")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The CHECK rejects a fourth value and expires_at is enforced NOT NULL
	// — each in its own transaction (a failed statement aborts the tx).
	expectErr := func(query string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, query); err == nil {
			t.Errorf("statement unexpectedly succeeded: %s", query)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
	expectErr(`INSERT INTO strava_onboardings (state, thread_id, status, expires_at) VALUES ('s2', 1, 'bogus', now())`) // CHECK
	expectErr(`INSERT INTO strava_onboardings (state, thread_id) VALUES ('s3', 1)`)                                     // expires_at NOT NULL

	// The index on expires_at.
	var idx int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'strava_onboardings' AND indexdef ILIKE '%expires_at%'
	`).Scan(&idx); err != nil {
		t.Fatalf("index query: %v", err)
	}
	if idx != 1 {
		t.Errorf("index on expires_at: found %d, want 1", idx)
	}
}
