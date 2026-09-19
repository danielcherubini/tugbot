// strava_integration_test.go — task 5: the DB-touching mechanics (cursor + seen
// atomicity, reauth pause/resume, 429 hold, pending lifecycle, first-enable
// lookback) against a real Postgres with a stubbed StravaAPI and a capturing
// sendFn. Skip convention mirrors internal/handlers/gulag/gulag_test.go
// (skipIfShort, TUGBOT_TEST_DATABASE_URL, 10s pool timeout, t.Skipf when
// unreachable). Because this test resets tables IN the shared compose DB, the
// DB gate runs -p 1 -count=1 (AGENTS.md state-residue warning; docker compose
// down -v is the remedy before a later make migrate).
package strava

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
)

// ---------------------------------------------------------------------------
// Test DB setup (repo convention: compose PG, skip when unreachable)
// ---------------------------------------------------------------------------

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
}

func testURLEnv() string { return os.Getenv("TUGBOT_TEST_DATABASE_URL") }

func testDBURL() string {
	const defaultURL = "postgres://tugbot:tugbot@127.0.0.1:5432/tugbot_test"
	url := testURLEnv()
	if url == "" {
		return defaultURL
	}
	return url
}

// setupStravaTestDB returns a pool to a reset strava test schema, or marks
// the test skipped when PG is unreachable.
func setupStravaTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	skipIfShort(t)
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
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS strava_seen_activities;
		DROP TABLE IF EXISTS strava_athletes;
		CREATE TABLE public.strava_athletes (
		    id               serial PRIMARY KEY,
		    label            text NOT NULL,
		    strava_athlete_id bigint NOT NULL UNIQUE,
		    access_token     text NOT NULL,
		    refresh_token    text NOT NULL,
		    token_expires_at timestamptz NOT NULL,
		    last_polled_at   timestamptz,
		    target_thread_id bigint,
		    needs_reauth     boolean NOT NULL DEFAULT false,
		    created_at       timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE public.strava_seen_activities (
		    id                 serial PRIMARY KEY,
		    strava_athletes_id int NOT NULL REFERENCES public.strava_athletes(id) ON DELETE CASCADE,
		    strava_activity_id bigint NOT NULL,
		    start_date         timestamptz NOT NULL,
		    status             text NOT NULL CHECK (status IN ('pending', 'posted', 'skipped')),
		    retries            int NOT NULL DEFAULT 0,
		    dispositioned_at   timestamptz NOT NULL DEFAULT now(),
		    UNIQUE (strava_athletes_id, strava_activity_id)
		);
		CREATE TABLE IF NOT EXISTS features (
			id serial PRIMARY KEY,
			name character varying(255) UNIQUE NOT NULL,
			enabled boolean DEFAULT false NOT NULL
		);
		DELETE FROM features;
		INSERT INTO features (name, enabled) VALUES ('strava', true);
	`); err != nil {
		pool.Close()
		t.Skipf("cannot set up tables: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---------------------------------------------------------------------------
// Seams: the stub StravaAPI + the capturing sendFn + the test constructor
// ---------------------------------------------------------------------------

// stubStrava is the scriptable StravaAPI stand-in (list/detail/refresh +
// call counters + the `after` values observed by the list seam — the cursor
// assertions).
type stubStrava struct {
	mu           sync.Mutex
	listFn       func(after time.Time) ([]Summary, error)
	detailFn     func(id int64) (Activity, error)
	refreshFn    func() error
	listCalls    int
	detailCalls  int
	refreshCalls int
	listAfters   []time.Time
}

func (s *stubStrava) ListActivities(_ context.Context, _ string, after time.Time) ([]Summary, error) {
	s.mu.Lock()
	s.listCalls++
	s.listAfters = append(s.listAfters, after)
	fn := s.listFn
	s.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(after)
}

func (s *stubStrava) GetActivity(_ context.Context, _ string, id int64) (Activity, error) {
	s.mu.Lock()
	s.detailCalls++
	fn := s.detailFn
	s.mu.Unlock()
	if fn == nil {
		return Activity{}, nil
	}
	return fn(id)
}

func (s *stubStrava) RefreshToken(_ context.Context, _, _, _ string) (string, string, time.Time, error) {
	s.mu.Lock()
	s.refreshCalls++
	fn := s.refreshFn
	s.mu.Unlock()
	if fn == nil {
		return "tok", "rtok", time.Now().UTC().Add(6 * time.Hour), nil
	}
	if err := fn(); err != nil {
		return "", "", time.Time{}, err
	}
	return "tok2", "rtok2", time.Now().UTC().Add(6 * time.Hour), nil
}

// capturedPost is one recorded call to the sendFn seam.
type capturedPost struct {
	threadID string
	msg      string
}

// capture opts into the sendFn seam and records every post.
type capture struct {
	posts []capturedPost
}

func (c *capture) sendFn(threadID, msg string) error {
	c.posts = append(c.posts, capturedPost{threadID, msg})
	return nil
}

// byThread records only the posts that went to one thread id (string form).
func (c *capture) byThread(threadID string) []capturedPost {
	var out []capturedPost
	for _, p := range c.posts {
		if p.threadID == threadID {
			out = append(out, p)
		}
	}
	return out
}

// newTestStrava wires the seams: the (stubbed) api, the capturing sendFn,
// the pool, a Config with client id/secret set, the shared thread id, and a
// 15-minute poll cadence — via the unexported handler constructor shape
// (same-package tests; no exported test seam on production code).
func newTestStrava(t *testing.T, pool *pgxpool.Pool, api StravaAPI, sharedThreadID int64) (*Strava, *capture) {
	t.Helper()
	caps := &capture{}
	s := &Strava{
		app: &app.App{
			Pool: pool,
			Cfg: &config.Config{
				StravaClientID:       "test-client-id",
				StravaClientSecret:   "test-client-secret",
				StravaSharedThreadID: sharedThreadID,
				StravaPollMinutes:    15,
			},
		},
		api:  api,
		poll: 15 * time.Minute,
	}
	s.sendFn = caps.sendFn
	return s, caps
}

// ---------------------------------------------------------------------------
// Seed + read helpers (raw SQL, house style)
// ---------------------------------------------------------------------------

// muTime truncates to microsecond precision — or the timestamptz round-trip
// would lose the trailing nanos and exact cursor equality would be flaky.
func muTime(v time.Time) time.Time { return v.Truncate(time.Microsecond) }

// seedAthlete inserts one athlete row: a valid token (tokenExpiresIn from
// now — 6h = no refresh path; 30m = inside the 1h refresh lead),
// lastPolledAt zero = NULL (the first-enable 24h lookback),
// targetThreadID zero = NULL; returns the row id.
func seedAthlete(t *testing.T, pool *pgxpool.Pool, targetThreadID int64, lastPolledAt time.Time, tokenExpiresIn time.Duration) int32 {
	return seedAthlete2(t, pool, "Matt", 1, targetThreadID, lastPolledAt, tokenExpiresIn)
}

// seedAthlete2 is seedAthlete with an explicit label and strava_athlete_id
// (for seeding multiple athletes in one test table reset).
func seedAthlete2(t *testing.T, pool *pgxpool.Pool, label string, stravaAthleteID int64, targetThreadID int64, lastPolledAt time.Time, tokenExpiresIn time.Duration) int32 {
	t.Helper()
	now := time.Now().UTC()
	var lastPolled any // zero time.Time encodes as an invalid timestamp; NULL for the first-enable lookback
	if !lastPolledAt.IsZero() {
		lastPolled = lastPolledAt
	}
	var target any // zero = NULL column (resolved via the shared-thread fallback at disposition time)
	if targetThreadID != 0 {
		target = targetThreadID
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO strava_athletes (label, strava_athlete_id, access_token, refresh_token, token_expires_at, last_polled_at, target_thread_id)
		 VALUES ($1, $2, 'tok', 'rtok', $3, $4, $5)`,
		label, stravaAthleteID, now.Add(tokenExpiresIn), lastPolled, target); err != nil {
		t.Fatalf("seed athlete: %v", err)
	}
	var id int32
	if err := pool.QueryRow(context.Background(), `SELECT id FROM strava_athletes WHERE strava_athlete_id = $1`, stravaAthleteID).Scan(&id); err != nil {
		t.Fatalf("read athlete id: %v", err)
	}
	return id
}

func insertSeen(t *testing.T, pool *pgxpool.Pool, athleteID int32, activityID int64, start time.Time, status string, retries int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO strava_seen_activities (strava_athletes_id, strava_activity_id, start_date, status, retries)
		 VALUES ($1, $2, $3, $4, $5)`,
		athleteID, activityID, start, status, retries); err != nil {
		t.Fatalf("insert seen row: %v", err)
	}
}

func seenRow(t *testing.T, pool *pgxpool.Pool, athleteID int32, activityID int64) (status string, retries int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, retries FROM strava_seen_activities WHERE strava_athletes_id=$1 AND strava_activity_id=$2`,
		athleteID, activityID).Scan(&status, &retries); err != nil {
		t.Fatalf("read seen row: %v", err)
	}
	return status, retries
}

func cursorAt(t *testing.T, pool *pgxpool.Pool, athleteID int32) time.Time {
	t.Helper()
	var v time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_polled_at FROM strava_athletes WHERE id=$1`, athleteID).Scan(&v); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return v
}

func needsReauthAt(t *testing.T, pool *pgxpool.Pool, athleteID int32) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(),
		`SELECT needs_reauth FROM strava_athletes WHERE id=$1`, athleteID).Scan(&v); err != nil {
		t.Fatalf("read needs_reauth: %v", err)
	}
	return v
}

func seenCount(t *testing.T, pool *pgxpool.Pool, athleteID int32) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM strava_seen_activities WHERE strava_athletes_id=$1`, athleteID).Scan(&n); err != nil {
		t.Fatalf("count seen rows: %v", err)
	}
	return n
}

// readyAct is a finished (not processing) family activity.
func readyAct(id int64, start time.Time, distance float64) Activity {
	return Activity{ID: id, SportType: "Run", StartDate: start, Distance: distance, ResourceState: 2}
}

// processingAct is a still-processing activity (resource_state == -1).
func processingAct(id int64, start time.Time) Activity {
	return Activity{ID: id, SportType: "Run", StartDate: start, ResourceState: -1}
}

// ---------------------------------------------------------------------------
// 1. a posts the new family activity; the non-family sibling is dispositioned
// ---------------------------------------------------------------------------

// TestStravaPassPostsNewFamily pins the happy path: exactly one post on the
// athlete's thread (the 42.3 km Run, the exact two-line text), the Run
// dispositioned 'posted', the Swim 'skipped', and the cursor advanced to
// max(A, B start) — the skip counts as dispositioned, nothing pending.
func TestStravaPassPostsNewFamily(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))
	startB := muTime(now.Add(-30 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{
				{ID: 1, SportType: "Run", StartDate: startA},
				{ID: 2, SportType: "Swim", StartDate: startB},
			}, nil
		},
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	sent := caps.byThread("9001")
	if len(caps.posts) != 1 || len(sent) != 1 {
		t.Fatalf("captured %d posts, want exactly 1 on 9001", len(caps.posts))
	}
	const want = "Matt finished 42.3 km Run\nhttps://www.strava.com/activities/1"
	if sent[0].msg != want {
		t.Errorf("post = %q, want %q", sent[0].msg, want)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("A row = %q, want %q", status, statusPosted)
	}
	if status, _ := seenRow(t, pool, aid, 2); status != statusSkipped {
		t.Errorf("B row = %q, want %q", status, statusSkipped)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startB) {
		t.Errorf("cursor = %v, want max(A, B start) = %v", got, startB)
	}
}

// ---------------------------------------------------------------------------
// 2. re-posts never happen (the ON CONFLICT DO NOTHING dedupe)
// ---------------------------------------------------------------------------

// TestStravaNoRepost pins the no-repost invariant: a previously 'posted'
// activity is a no-op on re-list, the new Swim is dispositioned 'skipped',
// zero posts, and A's row is byte-unchanged.
func TestStravaNoRepost(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))
	startB := muTime(now.Add(-30 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	insertSeen(t, pool, aid, 1, startA, statusPosted, 0)

	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{
				{ID: 1, SportType: "Run", StartDate: startA},
				{ID: 2, SportType: "Swim", StartDate: startB},
			}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if len(caps.posts) != 0 {
		t.Errorf("captured %d posts, want zero (no re-post)", len(caps.posts))
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusPosted || retries != 0 {
		t.Errorf("A row = %q retries %d, want %q/0 (unchanged)", status, retries, statusPosted)
	}
}

// ---------------------------------------------------------------------------
// 3. pending hold + retry (per the carried-row rule, retries = full re-fetch
//    cycles from 0)
// ---------------------------------------------------------------------------

// TestStravaPendingHoldAndRetry pins the two-cycle lifecycle: pass 1 holds
// the cursor at A.start (no post); pass 2 resolves the carried-pending,
// posts once, and keeps retries at 0 (A resolved on its first re-fetch).
func TestStravaPendingHoldAndRetry(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(_ int64) (Activity, error) {
			return processingAct(1, startA), nil
		},
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusPending || retries != 0 {
		t.Fatalf("pass 1: A row = %q retries %d, want pending/0", status, retries)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startA) {
		t.Errorf("pass 1: cursor = %v, want held at A.start (%v)", got, startA)
	}
	if len(caps.posts) != 0 {
		t.Fatalf("pass 1: captured %d posts, want none", len(caps.posts))
	}

	stub.detailFn = func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil }
	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusPosted || retries != 0 {
		t.Errorf("pass 2: A row = %q retries %d, want posted/0 (resolved on its first re-fetch)", status, retries)
	}
	if len(caps.posts) != 1 {
		t.Errorf("pass 2: captured %d posts, want exactly 1", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// 4. a stuck pending is dropped at 5 full cycles (skipped, no post ever)
// ---------------------------------------------------------------------------

// TestStravaPendingDropsAt5 pins the dropAfterRetries branch: six iterations
// against a forever-processing activity land on skipped/retries-5 with zero
// posts.
func TestStravaPendingDropsAt5(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return processingAct(1, startA), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		if err := s.iteration(ctx); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusSkipped || retries != 5 {
		t.Errorf("A row = %q retries %d, want skipped/5 (dropped at the new value >= 5)", status, retries)
	}
	if len(caps.posts) != 0 {
		t.Errorf("captured %d posts, want zero", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// 5. detail 429 → pending (hold at the failed id) then a genuine advance
// ---------------------------------------------------------------------------

// TestStravaDetail429CreatePending pins the detail-429 hold: pass 1 leaves
// A pending, posts B in the same pass, and never advances past the failed id
// (cursor held at A.start < B.start = B never posted-then-overwritten).
// Pass 2 is a genuine advance to the max of the all-time seen windows (B.start).
func TestStravaDetail429CreatePending(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-3 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute)) // A.start < B.start
	startB := muTime(now.Add(-30 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{
				{ID: 1, SportType: "Run", StartDate: startA},
				{ID: 2, SportType: "Run", StartDate: startB},
			}, nil
		},
		detailFn: func(id int64) (Activity, error) {
			if id == 1 {
				return Activity{}, ErrRateLimited{RetryAfter: "60"}
			}
			return readyAct(2, startB, 42300), nil
		},
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPending {
		t.Errorf("pass 1: A row = %q, want pending", status)
	}
	if status, _ := seenRow(t, pool, aid, 2); status != statusPosted {
		t.Errorf("pass 1: B row = %q, want posted", status)
	}
	if len(caps.posts) != 1 {
		t.Fatalf("pass 1: captured %d posts, want exactly 1 (only B)", len(caps.posts))
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startA) {
		t.Errorf("pass 1: cursor = %v, want held at A.start (%v) — never advanced past the failed id", got, startA)
	}

	stub.detailFn = func(id int64) (Activity, error) {
		if id == 1 {
			return readyAct(1, startA, 42300), nil
		}
		return readyAct(2, startB, 42300), nil
	}
	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("pass 2: A row = %q, want posted", status)
	}
	if len(caps.posts) != 2 {
		t.Fatalf("pass 2: captured %d posts, want 2 (B in pass 1, A in pass 2)", len(caps.posts))
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startB) {
		t.Errorf("pass 2: cursor = %v, want B.start (%v) — a genuine advance, all-time seen semantics", got, startB)
	}
}

// ---------------------------------------------------------------------------
// 6. refresh 401 → needs_reauth pause; re-auth → resume from the frozen cursor
// ---------------------------------------------------------------------------

// TestStravaNeedsReauthPauseResume pins the full reauth lifecycle: pass 1's
// failed refresh sets needs_reauth (cursor frozen), passes 2–3 make zero API
// calls (loadAthletes filters the flagged row), and after the re-auth UPDATE
// pass 4 lists with `after =` the 1h-overlapped frozen cursor and posts exactly once.
func TestStravaNeedsReauthPauseResume(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 30*time.Minute) // inside the 1h refresh lead
	stub := &stubStrava{
		refreshFn: func() error { return ErrUnauthorized{Why: "test"} },
		listFn:    nil,
	}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if !needsReauthAt(t, pool, aid) {
		t.Fatal("pass 1: needs_reauth = false, want true (invalid/deauthorized token)")
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("pass 1: cursor = %v, want frozen at %v", got, WINDOW)
	}
	listsBefore := stub.listCalls
	for _, pass := range []int{2, 3} {
		if err := s.iteration(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if stub.listCalls != listsBefore {
			t.Fatalf("pass %d: list calls = %d, want %d (a needs_reauth athlete is filtered and makes no API calls)", pass, stub.listCalls, listsBefore)
		}
	}
	if stub.refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want 1 (only pass 1's failed attempt)", stub.refreshCalls)
	}

	// re-auth: clear the flag, restore a fresh valid token, and let refresh succeed.
	if _, err := pool.Exec(ctx,
		`UPDATE strava_athletes SET needs_reauth=false, access_token='tok', refresh_token='rtok', token_expires_at=$1 WHERE id=$2`,
		now.Add(6*time.Hour), aid); err != nil {
		t.Fatalf("re-auth update: %v", err)
	}
	stub.refreshFn = func() error { return nil }
	stub.listFn = func(after time.Time) ([]Summary, error) {
		return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
	}
	stub.detailFn = func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil }

	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	if got := caps.byThread("9001"); len(got) != 1 {
		t.Fatalf("pass 4: captured %d posts, want exactly 1", len(caps.byThread("9001")))
	}
	if len(stub.listAfters) != 1 || !stub.listAfters[0].Equal(WINDOW.Add(-time.Hour)) {
		t.Errorf("pass 4: list afters = %v, want exactly one call with after = the 1h-overlapped frozen cursor %v",
			stub.listAfters, WINDOW.Add(-time.Hour))
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("pass 4: A row = %q, want posted", status)
	}
}

// ---------------------------------------------------------------------------
// 7. list 401 → abort: the per-athlete transaction is discarded
// ---------------------------------------------------------------------------

// TestStrava401ListAbortsPreservesCursor pins the list-401 abort: the
// needs_reauth UPDATE commits (autocommit) but the seen/cursor transaction is
// never run — cursor and the pending row's retries are untouched, no post.
func TestStrava401ListAbortsPreservesCursor(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	insertSeen(t, pool, aid, 1, startA, statusPending, 0)

	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) { return nil, ErrUnauthorized{Why: "test"} },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if !needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = false, want true (list 401 aborts to reauth)")
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusPending || retries != 0 {
		t.Errorf("A row = %q retries %d, want pending/0 (the per-athlete transaction was discarded)", status, retries)
	}
	if len(caps.posts) != 0 {
		t.Errorf("captured %d posts, want zero", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// 8. no target (per-athlete NULL + shared 0) → family activity skipped, no post
// ---------------------------------------------------------------------------

// TestStravaNoTargetSkips pins the no-target resolution: a family-Ready
// activity with no per-athlete thread and no shared thread is dispositioned
// 'skipped' with zero posts (the counted one-line log is pinned separately).
func TestStravaNoTargetSkips(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 0, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil },
	}
	// sharedThreadID = 0: no shared fallback.
	s, caps := newTestStrava(t, pool, stub, 0)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusSkipped {
		t.Errorf("A row = %q, want skipped (no post target)", status)
	}
	if len(caps.posts) != 0 {
		t.Errorf("captured %d posts, want zero (no target → no post)", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// 9. first enable (NULL cursor) → the 24h lookback window
// ---------------------------------------------------------------------------

// TestStravaFirstEnable24hLookback pins the NULL-cursor first-enable path:
// the list seam is called with after ≈ now-24h, and a family activity within
// the window is posted.
func TestStravaFirstEnable24hLookback(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	startA := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete(t, pool, 9001, time.Time{}, 6*time.Hour) // last_polled_at NULL
	stub := &stubStrava{
		listFn: func(after time.Time) ([]Summary, error) {
			want := now.Add(-firstPollLookback)
			d := after.Sub(want)
			if d < 0 {
				d = -d
			}
			if d > 5*time.Minute {
				t.Errorf("list after = %v, want ≈ now-24h (%v)", after, want)
			}
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if len(caps.posts) != 1 {
		t.Fatalf("captured %d posts, want exactly 1", len(caps.posts))
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("A row = %q, want posted", status)
	}
}

// ---------------------------------------------------------------------------
// 10. the ticker loop's ctx discipline (the gulag shape)
// ---------------------------------------------------------------------------

// TestRunPollLoopExits pins RunPoll's ctx discipline with the 50ms poll seam:
// no athlete rows are seeded (the preflight no-op path still lets the ticker
// fire), and a canceled ctx returns context.Canceled — within a bounded
// deadline, so a hang fails the test.
func TestRunPollLoopExits(t *testing.T) {
	pool := setupStravaTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stub := &stubStrava{}
	s, _ := newTestStrava(t, pool, stub, 9001)
	s.poll = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- s.RunPoll(ctx) }()

	select {
	case <-time.After(1500 * time.Millisecond):
		cancel()
	case err := <-done:
		t.Fatalf("RunPoll returned %v before the cancel", err)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunPoll returned %v, want context.Canceled on ctx cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPoll did not return within 5s of ctx cancel (deadline)")
	}
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// 11. list 429 → cursor hold; the next clean pass releases it, no double-post
// ---------------------------------------------------------------------------

// TestStravaList429Hold pins the list-429 hold: pass 1 changes nothing
// (cursor unchanged, zero seen rows, zero posts); pass 2 with a normal list
// posts A exactly once — the hold released cleanly, nothing double-posted.
func TestStravaList429Hold(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn:   func(_ time.Time) ([]Summary, error) { return nil, ErrRateLimited{RetryAfter: "60"} },
		detailFn: func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1 (429): %v", err)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("pass 1: cursor = %v, want unchanged (%v)", got, WINDOW)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("pass 1: %d seen rows, want zero (the 429 path never commits a tx)", n)
	}
	if len(caps.posts) != 0 {
		t.Errorf("pass 1: captured %d posts, want zero", len(caps.posts))
	}

	stub.listFn = func(_ time.Time) ([]Summary, error) {
		return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
	}
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 2 (clean): %v", err)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("pass 2: A row = %q, want posted", status)
	}
	if len(caps.posts) != 1 {
		t.Errorf("pass 2: captured %d posts, want exactly 1 (the hold released cleanly, no double-post)", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// 12. list 429 -> abort the WHOLE iteration (the Strava rate window is
//     app-wide, not per-athlete; the 15-min ticker IS the app-wide backoff)
// ---------------------------------------------------------------------------

// TestStravaList429AbortsIteration pins the app-wide 429 abort: A (first,
// valid token, cursor W_A) hits a list-endpoint 429 and B (second, token
// inside the 1h refresh lead — so a visit would have called the refresh seam —
// cursor W_B) is NEVER visited: B's refreshFn and a second listFn call never
// fire, and NEITHER cursor is touched. In pass 2 (clean list) both windows
// proceed: each posts its activity exactly once, and the follow-up pass is
// deduped (no double-post). The detail-endpoint 429->pending behavior is a
// distinct per-activity path and is NOT affected by this abort.
func TestStravaList429AbortsIteration(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	W_A := muTime(now.Add(-2 * time.Hour))
	W_B := muTime(now.Add(-3 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute)) // inside A's window (after W_A)
	startB := muTime(now.Add(-60 * time.Minute)) // inside B's window (after W_B)

	aidA := seedAthlete2(t, pool, "AG", 1, 9001, W_A, 6*time.Hour)    // valid token: no refresh path
	aidB := seedAthlete2(t, pool, "BG", 2, 9001, W_B, 30*time.Minute) // inside the 1h refresh lead
	stub := &stubStrava{
		listFn:    func(_ time.Time) ([]Summary, error) { return nil, ErrRateLimited{RetryAfter: "900"} },
		refreshFn: func() error { return nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	// ---- pass 1: A's list 429 aborts the whole iteration ----
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1 (429): %v (iteration must return nil; the errgroup never sees a 429 as fatal)", err)
	}
	if stub.listCalls != 1 {
		t.Fatalf("pass 1: list calls = %d, want exactly 1 (a list 429 aborts the pass; B is never visited)", stub.listCalls)
	}
	if stub.refreshCalls != 0 {
		t.Errorf("pass 1: refresh calls = %d, want 0 (visiting B's near-expiry token would call refresh; B was never visited)", stub.refreshCalls)
	}
	if got := cursorAt(t, pool, aidA); !got.Equal(W_A) {
		t.Errorf("pass 1: A cursor = %v, want unchanged (%v)", got, W_A)
	}
	if got := cursorAt(t, pool, aidB); !got.Equal(W_B) {
		t.Errorf("pass 1: B cursor = %v, want UNTOUCHED (%v) — B was never visited this pass", got, W_B)
	}
	if n := seenCount(t, pool, aidA) + seenCount(t, pool, aidB); n != 0 {
		t.Errorf("pass 1: %d seen rows, want zero (a 429 pass commits no transaction for any athlete)", n)
	}
	if len(caps.posts) != 0 {
		t.Errorf("pass 1: captured %d posts, want zero", len(caps.posts))
	}

	// ---- pass 2: clean list; both windows proceed, each posts exactly once ----
	stub.listFn = func(after time.Time) ([]Summary, error) {
		// a non-NULL cursor backdates the list window by 1h (windowOverlap).
		if after.Equal(W_A.Add(-time.Hour)) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		}
		if after.Equal(W_B.Add(-time.Hour)) {
			return []Summary{{ID: 2, SportType: "Ride", StartDate: startB}}, nil
		}
		t.Errorf("pass 2: unexpected list after = %v (want W_A−1h or W_B−1h)", after)
		return nil, nil
	}
	stub.detailFn = func(id int64) (Activity, error) {
		switch id {
		case 1:
			return readyAct(1, startA, 42300), nil
		case 2:
			return readyAct(2, startB, 100050), nil
		}
		t.Errorf("pass 2: unexpected detail id = %d", id)
		return Activity{}, nil
	}

	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 2 (clean): %v", err)
	}
	posts := caps.byThread("9001")
	if len(posts) != 2 {
		t.Fatalf("pass 2: captured %d posts on 9001, want exactly 2 (one per athlete)", len(posts))
	}
	var gotA, gotB bool
	for _, p := range posts {
		switch {
		case strings.Contains(p.msg, "activities/1"):
			gotA = true
		case strings.Contains(p.msg, "activities/2"):
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Errorf("pass 2: posts = %v, want one containing activities/1 and one containing activities/2 (each posted exactly once)", posts)
	}
	if got := cursorAt(t, pool, aidA); !got.Equal(startA) {
		t.Errorf("pass 2: A cursor = %v, want advanced to %v", got, startA)
	}
	if got := cursorAt(t, pool, aidB); !got.Equal(startB) {
		t.Errorf("pass 2: B cursor = %v, want advanced to %v", got, startB)
	}
	if status, _ := seenRow(t, pool, aidA, 1); status != statusPosted {
		t.Errorf("pass 2: A row = %q, want posted", status)
	}
	if status, _ := seenRow(t, pool, aidB, 2); status != statusPosted {
		t.Errorf("pass 2: B row = %q, want posted", status)
	}

	// ---- pass 3: the held windows close over already-seen ids: deduped, no
	// double-post (the 1h-overlapped window re-lists, the seen table absorbs)
	// ----
	stub.listFn = func(after time.Time) ([]Summary, error) {
		if after.Equal(startA.Add(-time.Hour)) || after.Equal(startB.Add(-time.Hour)) {
			return []Summary{}, nil // everything in the window is already seen
		}
		t.Errorf("pass 3: unexpected list after = %v (want startA−1h or startB−1h)", after)
		return nil, nil
	}
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 3 (dedupe): %v", err)
	}
	if n := len(caps.posts); n != 2 {
		t.Errorf("pass 3: captured %d total posts, want still 2 (deduped — no double-post)", n)
	}
	if n := seenCount(t, pool, aidA) + seenCount(t, pool, aidB); n != 2 {
		t.Errorf("pass 3: %d seen rows, want 2 (one per athlete, no new rows)", n)
	}
}

// ---------------------------------------------------------------------------
// 13. a backdated late activity surfaces via the 1h list-window overlap;
//     the seen dedupe makes the re-listing free
// ---------------------------------------------------------------------------

// TestStravaBackdatedOverlap pins the 1h overlap: the seeded cursor is T and
// a late-surfacing activity X has start_date T-30m (BELOW the cursor, inside
// the 1h overlap). Pass 1 lists with after = T-1h (the overlap, not T) and
// posts X exactly once with the cursor advancing to max(dispositioned
// all-time) (= X.start) as before. Pass 2 re-lists X: ZERO posts (the seen
// dedupe), cursor unchanged (nothing new dispositioned).
func TestStravaBackdatedOverlap(t *testing.T) {
	pool := setupStravaTestDB(t)
	cursor := muTime(time.Now().UTC().Add(-2 * time.Hour))
	startX := muTime(cursor.Add(-30 * time.Minute)) // below the cursor, inside the 1h overlap

	aid := seedAthlete(t, pool, 9001, cursor, 6*time.Hour)
	stub := &stubStrava{}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	// ---- pass 1: the list window starts 1h BELOW the seeded cursor ----
	stub.listFn = func(after time.Time) ([]Summary, error) {
		want := cursor.Add(-time.Hour)
		if !after.Equal(want) {
			t.Errorf("pass 1: list after = %v, want the 1h-overlapped %v (not the cursor %v)", after, want, cursor)
		}
		return []Summary{{ID: 1, SportType: "Run", StartDate: startX}}, nil
	}
	stub.detailFn = func(_ int64) (Activity, error) { return readyAct(1, startX, 42300), nil }

	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(caps.posts) != 1 || len(caps.byThread("9001")) != 1 {
		t.Fatalf("pass 1: captured %d posts, want exactly 1 (the backdated X, inside the overlap)", len(caps.posts))
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusPosted {
		t.Errorf("pass 1: X row = %q, want %q", status, statusPosted)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startX) {
		t.Errorf("pass 1: cursor = %v, want max(dispositioned all-time) = %v (the overlap only widens the LIST window, not the watermark)", got, startX)
	}

	// ---- pass 2: the same X is re-listed: deduped, ZERO posts, cursor held ----
	stub.listFn = func(after time.Time) ([]Summary, error) {
		want := startX.Add(-time.Hour) // (advanced) cursor − 1h
		if !after.Equal(want) {
			t.Errorf("pass 2: list after = %v, want the 1h-overlapped %v", after, want)
		}
		return []Summary{{ID: 1, SportType: "Run", StartDate: startX}}, nil
	}
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("pass 2: captured %d total posts, want still 1 (a re-listed 'posted' row is deduped — never re-posted)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startX) {
		t.Errorf("pass 2: cursor = %v, want unchanged (%v) — nothing new dispositioned", got, startX)
	}
}
