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

// resetMigrateState wipes all objects the test migrations create so each case
// starts deterministic.
func resetMigrateState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS schema_migrations;
		DROP TABLE IF EXISTS servers;
		DROP TABLE IF EXISTS mig_test_baseline_guard;
		DROP TABLE IF EXISTS extra_mig_test;
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
		// the same shared test database (packages run in parallel).
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
