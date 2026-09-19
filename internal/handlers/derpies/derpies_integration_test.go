package derpies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/dbmigrate"
	"github.com/danielcherubini/tugbot/internal/mcp"
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

// TestMigration000005AppliesAndSeeds — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run against the test PG and asserts
// the seeded state: the derpies_config row (id=1, delete_threshold=50),
// the derpies_gimmick_phrases shape (the UNIQUE phrase constraint), and
// derpies_prompt.body == defaultPromptTemplate (which also pins the
// 000005 UPDATE text to the code constant — the forever sync guard for
// the prompt flip, the same way TestMigration000003AppliesAndSeeds pins
// the seed). The precondition drops the three new tables (so the test
// is rerunnable) and recreates derpies_prompt with a STALE row so the
// UPDATE has a row to apply to.
func TestMigration000005AppliesAndSeeds(t *testing.T) {
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
	src, err := os.ReadFile("../../../migrations/000005_derpies_score.up.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000005_derpies_score.up.sql"), src, 0o644); err != nil {
		t.Fatalf("write migration to temp dir: %v", err)
	}

	// 2. Precondition (the shared test DB may already have any of these;
	//    never TRUNCATE features — other packages own it). The DROP ... CASCADE
	//    first makes the test rerunnable on the same test DB. derpies_prompt
	//    is recreated (mirroring 000003's DDL) with a STALE row so the
	//    migration's UPDATE has a row to apply to — a missing row would make
	//    the UPDATE a no-op and the body assert fail. Never touch the
	//    features rows or derpies_gimmicks.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_config CASCADE;
		DROP TABLE IF EXISTS derpies_decisions CASCADE;
		DROP TABLE IF EXISTS derpies_gimmick_phrases CASCADE;
		DROP TABLE IF EXISTS derpies_prompt CASCADE;
		CREATE TABLE public.derpies_prompt (
		    id integer NOT NULL,
		    body text NOT NULL,
		    updated_at timestamp without time zone DEFAULT now() NOT NULL
		);
		CREATE SEQUENCE public.derpies_prompt_id_seq
		    AS integer
		    START WITH 1
		    INCREMENT BY 1
		    NO MINVALUE
		    NO MAXVALUE
		    CACHE 1;
		ALTER SEQUENCE public.derpies_prompt_id_seq OWNED BY public.derpies_prompt.id;
		ALTER TABLE ONLY public.derpies_prompt ALTER COLUMN id SET DEFAULT nextval('public.derpies_prompt_id_seq'::regclass);
		ALTER TABLE ONLY public.derpies_prompt
		    ADD CONSTRAINT derpies_prompt_pkey PRIMARY KEY (id);
		INSERT INTO public.derpies_prompt (body) VALUES ('stale pre-score prompt');
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version text PRIMARY KEY,
		    applied_at timestamptz DEFAULT now()
		);
		DELETE FROM schema_migrations WHERE version = '000005_derpies_score';
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// 3. The real migration file runs clean.
	if err := dbmigrate.Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 4. Clean up (leave derpies_prompt and schema_migrations' other rows
	//    intact — this test only owns its three new tables + version row).
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			DROP TABLE IF EXISTS derpies_config CASCADE;
			DROP TABLE IF EXISTS derpies_decisions CASCADE;
			DROP TABLE IF EXISTS derpies_gimmick_phrases CASCADE;
			DELETE FROM schema_migrations WHERE version = '000005_derpies_score';
		`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// 5. Asserts.
	// 5a. derpies_config: the seeded row (id=1, delete_threshold=50 — the
	//     code default) and the CHECK constraint.
	var id int
	var threshold int
	if err := pool.QueryRow(ctx, `SELECT id, delete_threshold FROM derpies_config`).Scan(&id, &threshold); err != nil {
		t.Fatalf("derpies_config row: %v", err)
	}
	if id != 1 || threshold != 50 {
		t.Errorf("derpies_config row = (id=%d, delete_threshold=%d), want (1, 50)", id, threshold)
	}
	var contype string
	if err := pool.QueryRow(ctx,
		`SELECT contype::text FROM pg_constraint WHERE conname = 'derpies_config_threshold_check'`).Scan(&contype); err != nil {
		t.Errorf("derpies_config_threshold_check: %v (missing?)", err)
		return
	}
	if contype != "c" {
		t.Errorf("derpies_config_threshold_check contype = %q, want c", contype)
	}

	// 5a-bis. The singleton CHECK (id = 1): the constraint exists AND a
	//         second row is actually rejected (the finding's concern — a
	//         stray operator row must not be insertable).
	var singletonContype string
	if err := pool.QueryRow(ctx,
		`SELECT contype::text FROM pg_constraint WHERE conname = 'derpies_config_singleton_check'`).Scan(&singletonContype); err != nil {
		t.Errorf("derpies_config_singleton_check: %v (missing?)", err)
		return
	}
	if singletonContype != "c" {
		t.Errorf("derpies_config_singleton_check contype = %q, want c", singletonContype)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO derpies_config (id, delete_threshold) VALUES (2, 60)`); err == nil {
		t.Errorf("inserting a second derpies_config row (id=2) succeeded, want the singleton CHECK to reject it")
		// Clean up the stray row (if it slipped through) so the test is rerunnable.
		if _, err := pool.Exec(context.Background(), `DELETE FROM derpies_config WHERE id = 2`); err != nil {
			t.Errorf("cleanup stray derpies_config row: %v", err)
		}
	}

	// 5b. derpies_gimmick_phrases: the UNIQUE (phrase) constraint.
	var phraseContype string
	if err := pool.QueryRow(ctx,
		`SELECT contype::text FROM pg_constraint WHERE conname = 'derpies_gimmick_phrases_phrase_key'`).Scan(&phraseContype); err != nil {
		t.Errorf("derpies_gimmick_phrases_phrase_key: %v (missing?)", err)
		return
	}
	if phraseContype != "u" {
		t.Errorf("derpies_gimmick_phrases_phrase_key contype = %q, want u", phraseContype)
	}

	// 5c. The prompt flip: the live row's body is the code default
	//     template BYTE-FOR-BYTES (which also pins the 000005 UPDATE text
	//     to the constant — the forever sync guard for the flip).
	var body string
	if err := pool.QueryRow(ctx, `SELECT body FROM derpies_prompt`).Scan(&body); err != nil {
		t.Fatalf("derpies_prompt body: %v", err)
	}
	if body != defaultPromptTemplate {
		t.Errorf("derpies_prompt.body != defaultPromptTemplate constant (the 000005 UPDATE text is out of sync with the code default)")
	}
}

// TestQueryDecisionsSQL — the parameterized SELECT behind ReadDecisions,
// for real: rows are inserted through recordDecision (exercising the
// NULL-pointer binds — a path=NULL pre-path row, a score=NULL fast row,
// a reject_reason=NULL row) + a bulk past row set so the LIMIT clamp is
// real, then queryDecisions is called with each filter clause and the
// WHERE-building, ORDER BY created_at DESC, and the LIMIT default (50)
// + clamp (500) are asserted. A typo in the composed SQL ships green
// otherwise.
func TestQueryDecisionsSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// Precondition: the derpies_decisions shape (the 000005 DDL) — the
	// DROP ... CASCADE first makes the test rerunnable on the same test
	// DB. Never touch the other derpies tables or features.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_decisions CASCADE;
		CREATE TABLE public.derpies_decisions (
		    id bigserial PRIMARY KEY,
		    message_id text NOT NULL,
		    channel_id text NOT NULL,
		    author_id text NOT NULL,
		    content text NOT NULL DEFAULT '',
		    path text CHECK (path IN ('fast', 'slow')),
		    score integer,
		    threshold integer,
		    word text,
		    learned boolean NOT NULL DEFAULT false,
		    deleted boolean NOT NULL DEFAULT false,
		    reject_reason text,
		    created_at timestamp without time zone DEFAULT now() NOT NULL
		);
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS derpies_decisions CASCADE`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	store := &poolStore{pool: pool}

	// 1. Three rows through recordDecision (the NULL-pointer binds):
	//    a path=NULL pre-path row (reject_reason set), a score=NULL fast
	//    row (reject_reason NULL), a reject_reason=NULL slow row.
	if err := store.recordDecision(ctx, &decisionRecord{
		MessageID: "dm1", ChannelID: "c1", AuthorID: "u1", Content: "hi",
		RejectReason: strPtr("list fetch failed"),
	}); err != nil {
		t.Fatalf("recordDecision (pre-path row): %v", err)
	}
	if err := store.recordDecision(ctx, &decisionRecord{
		MessageID: "dm2", ChannelID: "c1", AuthorID: "u1", Content: "sw1ft",
		Path: strPtr("fast"),
	}); err != nil {
		t.Fatalf("recordDecision (fast row): %v", err)
	}
	if err := store.recordDecision(ctx, &decisionRecord{
		MessageID: "dm3", ChannelID: "c2", AuthorID: "u2", Content: "c0g",
		Path: strPtr("slow"), Score: intPtr(80), Threshold: intPtr(50),
		Word: strPtr("c0g"), Learned: true, Deleted: true,
	}); err != nil {
		t.Fatalf("recordDecision (slow row): %v", err)
	}
	// Distinct, ordered created_at (the default now() would tie within
	// the test): newest first — dm3, dm2, dm1.
	if _, err := pool.Exec(ctx, `
		UPDATE derpies_decisions SET created_at = '2026-01-03 00:00:00' WHERE message_id = 'dm3';
		UPDATE derpies_decisions SET created_at = '2026-01-02 00:00:00' WHERE message_id = 'dm2';
		UPDATE derpies_decisions SET created_at = '2026-01-01 00:00:00' WHERE message_id = 'dm1';
	`); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
	// 2. A bulk past row set (505 rows, all slow / score 10 / author u3)
	//    so the LIMIT clamp is real (508 total > 500).
	if _, err := pool.Exec(ctx, `
		INSERT INTO derpies_decisions (message_id, channel_id, author_id, content, path, score, threshold, learned, deleted, created_at)
		SELECT 'bulk' || i, 'c3', 'u3', 'bulk', 'slow', 10, 50, false, false,
		       '2025-01-01 00:00:00'::timestamp + (i || ' seconds')::interval
		FROM generate_series(1, 505) AS i;
	`); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	// 3. Asserts.
	// (a) No filter: the LIMIT default 50 (508 rows exist) + ORDER BY
	//     created_at DESC (dm3 first) + the NULL-pointer binds round-
	//     trip (the pre-path row's path is NULL, the fast row's score is
	//     NULL, the slow row's reject_reason is NULL).
	rows, err := store.queryDecisions(ctx, mcp.DecisionFilter{})
	if err != nil {
		t.Fatalf("queryDecisions (no filter): %v", err)
	}
	if len(rows) != 50 {
		t.Errorf("rows = %d, want the default-limit 50", len(rows))
	}
	if rows[0].MessageID != "dm3" {
		t.Errorf("first row = %q, want dm3 (ORDER BY created_at DESC)", rows[0].MessageID)
	}
	for _, r := range rows {
		switch r.MessageID {
		case "dm1":
			if r.Path != nil || r.RejectReason == nil || *r.RejectReason != "list fetch failed" {
				t.Errorf("dm1 = %+v, want path NULL + reject_reason 'list fetch failed'", r)
			}
		case "dm2":
			if r.Path == nil || *r.Path != "fast" || r.Score != nil || r.RejectReason != nil {
				t.Errorf("dm2 = %+v, want path 'fast' + score NULL + reject_reason NULL", r)
			}
		case "dm3":
			if r.Path == nil || *r.Path != "slow" || r.Score == nil || *r.Score != 80 ||
				r.RejectReason != nil || !r.Learned || !r.Deleted {
				t.Errorf("dm3 = %+v, want path 'slow' + score 80 + reject_reason NULL + learned + deleted", r)
			}
		}
	}
	// (b) The LIMIT clamp: 1000 → 500 (508 rows exist).
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("queryDecisions (limit 1000): %v", err)
	}
	if len(rows) != 500 {
		t.Errorf("rows = %d, want the clamped 500", len(rows))
	}
	// (c) Each WHERE clause.
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{AuthorID: "u2"})
	if err != nil {
		t.Fatalf("queryDecisions (author): %v", err)
	}
	if len(rows) != 1 || rows[0].MessageID != "dm3" {
		t.Errorf("author filter rows = %v, want exactly dm3", rows)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{ChannelID: "c2"})
	if err != nil {
		t.Fatalf("queryDecisions (channel): %v", err)
	}
	if len(rows) != 1 || rows[0].MessageID != "dm3" {
		t.Errorf("channel filter rows = %v, want exactly dm3", rows)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{Path: "fast"})
	if err != nil {
		t.Fatalf("queryDecisions (path fast): %v", err)
	}
	if len(rows) != 1 || rows[0].MessageID != "dm2" {
		t.Errorf("path fast rows = %v, want exactly dm2 (the NULL-path pre-path row is excluded)", rows)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{Path: "slow", Limit: 1000})
	if err != nil {
		t.Fatalf("queryDecisions (path slow): %v", err)
	}
	if len(rows) != 500 {
		t.Errorf("path slow rows = %d, want the clamped 500 (1 slow record row + 505 bulk rows)", len(rows))
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{Deleted: boolPtr(true)})
	if err != nil {
		t.Fatalf("queryDecisions (deleted true): %v", err)
	}
	if len(rows) != 1 || rows[0].MessageID != "dm3" {
		t.Errorf("deleted=true rows = %v, want exactly dm3", rows)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{ScoreMin: intPtr(50), ScoreMax: intPtr(90)})
	if err != nil {
		t.Fatalf("queryDecisions (score range): %v", err)
	}
	if len(rows) != 1 || rows[0].MessageID != "dm3" {
		t.Errorf("score range rows = %v, want exactly dm3 (score 80; the NULL-score fast row is excluded)", rows)
	}
	// (d) Since/Until (bound UTC — created_at is timestamp without time
	//     zone): the window [01-02, 01-03] captures dm3 + dm2, newest
	//     first; an until strictly before all rows captures nothing (the
	//     until comparison is inclusive: <=).
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{
		Since: timePtr(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		Until: timePtr(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("queryDecisions (since/until): %v", err)
	}
	if len(rows) != 2 || rows[0].MessageID != "dm3" || rows[1].MessageID != "dm2" {
		t.Errorf("since/until rows = %v, want dm3 then dm2", rows)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{
		Until: timePtr(time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("queryDecisions (until before all): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("until-before-all rows = %v, want 0", rows)
	}
	// (e) The id tiebreaker: two rows with the SAME created_at order by id
	//     DESC (newest id first) — the ORDER BY created_at DESC, id DESC
	//     makes same-timestamp rows deterministic. The created_at is older
	//     than every prior row so it does not disturb the (a) no-filter
	//     assertion; the unique author isolates the two rows.
	if _, err := pool.Exec(ctx, `
		INSERT INTO derpies_decisions (message_id, channel_id, author_id, content, path, score, threshold, learned, deleted, created_at)
		VALUES ('tie_low', 'c9', 'tie', 'tie', 'slow', 10, 50, false, false, '2024-01-01 00:00:00'),
		       ('tie_high', 'c9', 'tie', 'tie', 'slow', 10, 50, false, false, '2024-01-01 00:00:00');
	`); err != nil {
		t.Fatalf("tiebreaker insert: %v", err)
	}
	rows, err = store.queryDecisions(ctx, mcp.DecisionFilter{AuthorID: "tie"})
	if err != nil {
		t.Fatalf("queryDecisions (tiebreaker): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("tiebreaker rows = %d, want 2", len(rows))
	}
	if rows[0].MessageID != "tie_high" || rows[1].MessageID != "tie_low" {
		t.Errorf("tiebreaker order = %q then %q, want tie_high (higher id) first (created_at DESC, id DESC)", rows[0].MessageID, rows[1].MessageID)
	}
}

// TestConfigThresholdMissingRowSentinel — the poolStore level: an empty
// derpies_config (no rows) yields the errConfigThresholdMissing sentinel
// (not a value); a seeded row yields the value, not the sentinel. (The
// flow treats both identically — TestFlowConfigFallback pins that; this
// pins the sentinel distinction at the store level.)
func TestConfigThresholdMissingRowSentinel(t *testing.T) {
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

	// Precondition: the derpies_config shape (the 000005 DDL) with NO
	// rows — the DROP ... CASCADE first makes the test rerunnable on
	// the same test DB. Never touch the other derpies tables or
	// features.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_config CASCADE;
		CREATE TABLE public.derpies_config (
		    id integer NOT NULL,
		    delete_threshold integer DEFAULT 50 NOT NULL,
		    updated_at timestamp without time zone DEFAULT now() NOT NULL,
		    CONSTRAINT derpies_config_pkey PRIMARY KEY (id),
		    CONSTRAINT derpies_config_threshold_check CHECK (delete_threshold BETWEEN 41 AND 100)
		);
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS derpies_config CASCADE`); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	store := &poolStore{pool: pool}

	// 1. An empty derpies_config (no rows): the sentinel error, not a
	//    value — the caller can distinguish "not seeded yet" from a
	//    real failure.
	if _, err := store.configThreshold(ctx); !errors.Is(err, errConfigThresholdMissing) {
		t.Errorf("configThreshold on an empty table = error %v, want the errConfigThresholdMissing sentinel", err)
	}

	// 2. A seeded row: the value, NOT the sentinel (the distinction is
	//    one-way — a present row yields the clamped value).
	if _, err := pool.Exec(ctx, `INSERT INTO derpies_config (id, delete_threshold) VALUES (1, 50)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	got, err := store.configThreshold(ctx)
	if err != nil {
		t.Errorf("configThreshold on a seeded row = error %v, want nil", err)
	}
	if got != 50 {
		t.Errorf("configThreshold = %d, want 50", got)
	}
}

// TestMigration000007AppliesAndRollsBack — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run, rolls back by executing the down
// file's statements DIRECTLY (the runner has no down convention: it globs
// *.up.sql only, and the tracker row from the UP run would make a re-run
// skip the file), then restores the UP state the same direct-execution way
// so the shared test DB ends in the 3-value state for later DB-gated runs
// in this package.
func TestMigration000007AppliesAndRollsBack(t *testing.T) {
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

	// 1. UP: run the real migration file from a temp dir.
	dir := t.TempDir()
	upSQL, err := os.ReadFile("../../../migrations/000007_derpies_slowmode_path.up.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000007_derpies_slowmode_path.up.sql"), upSQL, 0o644); err != nil {
		t.Fatalf("write migration to temp dir: %v", err)
	}
	downSQL, err := os.ReadFile("../../../migrations/000007_derpies_slowmode_path.down.sql")
	if err != nil {
		t.Fatalf("read down file: %v", err)
	}

	// 2. Precondition (the shared test DB may be in any state, including
	//    residue from a crashed prior run — the TRUNCATE goes FIRST so a
	//    stale path='slowmode' row cannot block the 2-value constraint
	//    re-add). The path column has NO check in the CREATE (a crashed
	//    prior run could otherwise leave either variant and the re-add
	//    would fail), so the 2-value check is added explicitly.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS derpies_decisions (
		    id bigserial PRIMARY KEY,
		    message_id text NOT NULL,
		    channel_id text NOT NULL,
		    author_id text NOT NULL,
		    content text NOT NULL DEFAULT '',
		    path text,
		    score integer,
		    threshold integer,
		    word text,
		    learned boolean NOT NULL DEFAULT false,
		    deleted boolean NOT NULL DEFAULT false,
		    reject_reason text,
		    created_at timestamp without time zone DEFAULT now() NOT NULL
		);
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version text PRIMARY KEY,
		    applied_at timestamp with time zone DEFAULT now()
		);
		TRUNCATE derpies_decisions;
		ALTER TABLE derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
		ALTER TABLE derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
		DELETE FROM schema_migrations WHERE version = '000007_derpies_slowmode_path';
	`); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// 3. The real migration file runs clean.
	if err := dbmigrate.Run(ctx, pool, dir); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	pathGate := func(id, path string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO derpies_decisions (message_id, channel_id, author_id, path) VALUES ($1, 'c', 'u', $2)`,
			id, path)
		return err
	}
	deleteRow := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM derpies_decisions WHERE message_id = $1`, id); err != nil {
			t.Errorf("cleanup row %s: %v", id, err)
		}
	}

	// 4. Assert up: slowmode and fast succeed, an unknown value fails.
	if err := pathGate("up_slowmode", "slowmode"); err != nil {
		t.Fatalf("post-up insert path='slowmode' = %v, want success", err)
	}
	if err := pathGate("up_fast", "fast"); err != nil {
		t.Fatalf("post-up insert path='fast' = %v, want success", err)
	}
	if err := pathGate("up_nope", "nope"); err == nil {
		t.Errorf("post-up insert path='nope' succeeded, want the 3-value CHECK to reject it")
	}
	deleteRow("up_slowmode")
	deleteRow("up_fast")

	// 5. DOWN: execute the down file's statements directly (NOT
	//    dbmigrate.Run — it globs *.up.sql only and the step-3 tracker row
	//    would make a re-run skip the file).
	if _, err := pool.Exec(ctx, string(downSQL)); err != nil {
		t.Fatalf("down file exec: %v", err)
	}
	// Assert down: slowmode now fails, fast succeeds.
	if err := pathGate("dn_slowmode", "slowmode"); err == nil {
		t.Errorf("post-down insert path='slowmode' succeeded, want the 2-value CHECK to reject it")
	}
	if err := pathGate("dn_fast", "fast"); err != nil {
		t.Fatalf("post-down insert path='fast' = %v, want success", err)
	}
	deleteRow("dn_fast")

	// 6. Restore the UP state: direct execution of the up file's two
	//    statements (NOT dbmigrate.Run — the tracker row from step 3 still
	//    exists, so dbmigrate.Run would be a no-op and the DB would stay in
	//    the 2-value state, breaking later DB-gated runs in this package).
	if _, err := pool.Exec(ctx, string(upSQL)); err != nil {
		t.Fatalf("restore up exec: %v", err)
	}
	if err := pathGate("restored_slowmode", "slowmode"); err != nil {
		t.Fatalf("restored insert path='slowmode' = %v, want the UP (3-value) state", err)
	}
	deleteRow("restored_slowmode")
}

// intPtr / boolPtr / timePtr — pointer helpers for the test's nullable
// filter fields (strPtr lives in the package proper).
func intPtr(i int) *int              { return &i }
func boolPtr(b bool) *bool           { return &b }
func timePtr(t time.Time) *time.Time { return &t }
