package features

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testURLEnv returns the integration test database URL override.
func testURLEnv() string { return os.Getenv("TUGBOT_TEST_DATABASE_URL") }

// testDBURL returns the integration test database URL: env override or the
// docker-compose PG default.
func testDBURL() string {
	if u := testURLEnv(); u != "" {
		return u
	}
	return "postgres://tugbot:tugbot@127.0.0.1:5432/tugbot_test"
}

// setupTestDB returns a pool to a reset features table, or nil (and marks the
// test skipped) when PG is unreachable.
func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Skipf("cannot create pool: %v (is the compose PG running?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v (is the compose PG running?)", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS features (
		id serial PRIMARY KEY,
		name varchar(255) UNIQUE NOT NULL,
		enabled boolean DEFAULT false NOT NULL
	); DELETE FROM features;`); err != nil {
		pool.Close()
		t.Skipf("cannot reset features table: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCheckEnabledMissingRowIsNotAnError(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	on, err := CheckEnabled(ctx, pool, "never_registered_feature")
	if err != nil {
		t.Fatalf("CheckEnabled on missing row: error = %v, want nil (unregistered feature is disabled, not a DB error)", err)
	}
	if on {
		t.Error("CheckEnabled on missing row = true, want false")
	}
}

// newClosedPool returns a pool that has been created successfully (so PG is
// known to be up) and then closed, so every query returns a clean error.
func newClosedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Skipf("cannot create pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v", err)
	}
	pool.Close()
	return pool
}

func TestCheckEnabledDBFailurePropagates(t *testing.T) {
	pool := newClosedPool(t) // closed pool: queries fail
	_, err := CheckEnabled(context.Background(), pool, "anything")
	if err == nil {
		t.Error("CheckEnabled on closed pool: error = nil, want a propagated DB error")
	}
}

func TestIsEnabledSilentOnDBFailure(t *testing.T) {
	pool := newClosedPool(t) // closed pool: queries fail
	if got := IsEnabled(context.Background(), pool, "anything"); got {
		t.Error("IsEnabled on closed pool = true, want false (silent)")
	}
}

func TestUpdateMissingFeatureErrors(t *testing.T) {
	pool := setupTestDB(t)
	err := Update(context.Background(), pool, "no_such_feature", true)
	if err == nil {
		t.Fatal("Update on missing feature: error = nil, want 'not found' error (mirrors Rust rows_affected == 0 bail)")
	}
}

// TestUpdateErrorTextsMatchRust pins the exact error texts of Update against
// Rust src/features/mod.rs:52-64: pool error ->
// "Failed to get database connection from pool"; no row affected ->
// bail!("Feature '{}' not found in database", name).
func TestUpdateErrorTextsMatchRust(t *testing.T) {
	// no-row path
	pool := setupTestDB(t)
	err := Update(context.Background(), pool, "no_such_feature", true)
	if err == nil {
		t.Fatal("Update on missing feature: error = nil, want 'not found' error")
	}
	want := `Feature "no_such_feature" not found in database`
	if err.Error() != want {
		t.Errorf("Update no-row text = %q, want %q (Rust mod.rs:63-64 bail)", err.Error(), want)
	}
	// pool/connection error path
	closed := newClosedPool(t)
	err = Update(context.Background(), closed, "any", true)
	if err == nil {
		t.Fatal("Update on closed pool: error = nil, want the Rust pool-error text")
	}
	const wantPool = "Failed to get database connection from pool"
	if err.Error() != wantPool {
		t.Errorf("Update pool-error text = %q, want %q (Rust mod.rs:52-56)", err.Error(), wantPool)
	}
}

func TestFeatureLifecycle(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO features (name, enabled) VALUES ('new_feature', false), ('already_on', true)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// CheckEnabled
	on, err := CheckEnabled(ctx, pool, "already_on")
	if err != nil || !on {
		t.Fatalf("CheckEnabled(already_on) = %v, %v; want true, nil", on, err)
	}
	on, err = CheckEnabled(ctx, pool, "new_feature")
	if err != nil || on {
		t.Fatalf("CheckEnabled(new_feature) = %v, %v; want false, nil", on, err)
	}

	// IsEnabled mirrors CheckEnabled on success.
	if got := IsEnabled(ctx, pool, "already_on"); !got {
		t.Error("IsEnabled(already_on) = false, want true")
	}

	// All
	all, err := All(ctx, pool)
	if err != nil {
		t.Fatalf("All() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("All() = %d features, want 2", len(all))
	}
	byName := map[string]Feature{}
	for _, f := range all {
		byName[f.Name] = f
	}
	if byName["new_feature"].Enabled {
		t.Error("new_feature enabled = true, want false")
	}

	// Update flips the flag.
	if err := Update(ctx, pool, "new_feature", true); err != nil {
		t.Fatalf("Update(new_feature, true) error = %v", err)
	}
	on, _ = CheckEnabled(ctx, pool, "new_feature")
	if !on {
		t.Error("after Update: CheckEnabled(new_feature) = false, want true")
	}

	// Update with 0 rows affected errors.
	if err := Update(ctx, pool, "missing", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(missing) error = %v, want ErrNotFound", err)
	}
}
