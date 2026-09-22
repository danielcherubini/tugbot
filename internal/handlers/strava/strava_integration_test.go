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
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
		DROP TABLE IF EXISTS strava_onboardings;
		CREATE TABLE strava_onboardings (
		    state      text PRIMARY KEY,
		    thread_id  bigint NOT NULL,
		    label      text,
		    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
		    created_at timestamptz NOT NULL DEFAULT now(),
		    expires_at timestamptz NOT NULL
		);
		CREATE INDEX strava_onboardings_expires_at_idx ON strava_onboardings (expires_at);
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
	mu            sync.Mutex
	listFn        func(after time.Time) ([]Summary, error)
	detailFn      func(id int64) (Activity, error)
	refreshFn     func() (string, string, time.Time, error)
	listCalls     int
	detailCalls   int
	refreshCalls  int
	listAfters    []time.Time
	athleteFn     func() (Athlete, error)
	athleteCalls  int
	exchangeFn    func() (string, string, time.Time, string, error)
	exchangeCalls int
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
	// The scripted pair (the 401 case returns ErrUnauthorized{}; the success
	// case scripts the ROTATED pair so the persistence is assertable).
	return fn()
}

func (s *stubStrava) GetAthlete(_ context.Context, _ string) (Athlete, error) {
	s.mu.Lock()
	s.athleteCalls++
	fn := s.athleteFn
	s.mu.Unlock()
	if fn == nil {
		return Athlete{}, nil
	}
	return fn()
}

func (s *stubStrava) ExchangeCode(_ context.Context, _, _, _ string) (string, string, time.Time, string, error) {
	s.mu.Lock()
	s.exchangeCalls++
	fn := s.exchangeFn
	s.mu.Unlock()
	if fn == nil {
		return "canned-access", "canned-refresh", time.Now().UTC().Add(6 * time.Hour), "read activity:read_all", nil
	}
	return fn()
}

// capturedPost is one recorded call to the sendFn seam.
type capturedPost struct {
	threadID string
	msg      string
}

// capture opts into the sendFn seam and records every post.
// mutex-protected: the concurrency test fires two callbacks that both
// reach the seam (pre-fix), so the append must not race.
type capture struct {
	mu sync.Mutex

	posts []capturedPost
}

func (c *capture) sendFn(threadID, msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts = append(c.posts, capturedPost{threadID, msg})
	return nil
}

// byThread records only the posts that went to one thread id (string form).
func (c *capture) byThread(threadID string) []capturedPost {
	c.mu.Lock()
	defer c.mu.Unlock()
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
		refreshFn: func() (string, string, time.Time, error) { return "", "", time.Time{}, ErrUnauthorized{Why: "test"} },
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
	stub.refreshFn = func() (string, string, time.Time, error) {
		return "tok", "rtok", time.Now().UTC().Add(6 * time.Hour), nil
	}
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
		listFn: func(_ time.Time) ([]Summary, error) { return nil, ErrRateLimited{RetryAfter: "900"} },
		refreshFn: func() (string, string, time.Time, error) {
			return "tok", "rtok", time.Now().UTC().Add(6 * time.Hour), nil
		},
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

// ---------------------------------------------------------------------------
// 14. refresh SUCCESS persists the ROTATED pair (the single most
//     credential-fatal line: persisting the OLD refresh token / a swapped
//     column / a wrong row passes every other test and surfaces two rotations
//     later as invalid_grant → needs_reauth with no operator-visible cause)
// ---------------------------------------------------------------------------

// TestStravaRefreshRotatedPersistence pins the refresh-success path: a token
// expiring inside the 1h refresh lead (now+30m) makes the pass refresh,
// and the success must persist (new access_token, the ROTATED refresh_token,
// new token_expires_at) in the athlete row. Assertions hit the EXACT columns
// (a swapped-column or old-token persistence must not survive). The second
// pass — the token now ≈ now+6h, safely outside the 1h lead — must make NO
// spurious branch refresh and must not double-post.
func TestStravaRefreshRotatedPersistence(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 30*time.Minute) // inside the 1h refresh lead — refresh MUST fire
	// the rotation-seed pair (the stub's success is scripted to (tok2, rtok2, now+6h)).
	if _, err := pool.Exec(context.Background(),
		`UPDATE strava_athletes SET access_token='tok1', refresh_token='rtok1', token_expires_at=$1 WHERE id=$2`,
		now.Add(30*time.Minute), aid); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}
	stub := &stubStrava{
		refreshFn: func() (string, string, time.Time, error) {
			return "tok2", "rtok2", time.Now().UTC().Add(6 * time.Hour), nil
		}, // success → the ROTATED pair (tok2, rtok2, now+6h)
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return readyAct(1, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	// ---- pass 1: the refresh fires and its success persists the ROTATED pair ----
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if stub.refreshCalls != 1 {
		t.Fatalf("pass 1: refresh calls = %d, want exactly 1 (a now+30m expiry is inside the 1h refresh lead)", stub.refreshCalls)
	}
	var access, refresh string
	var expiresAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT access_token, refresh_token, token_expires_at FROM strava_athletes WHERE id=$1`, aid).Scan(&access, &refresh, &expiresAt); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if access != "tok2" {
		t.Errorf("pass 1: access_token = %q, want 'tok2' (the NEW access token — the exact column)", access)
	}
	if refresh != "rtok2" {
		t.Errorf("pass 1: refresh_token = %q, want the ROTATED 'rtok2' exactly ('rtok1' = old-token persistence; 'tok2' = a swapped column)", refresh)
	}
	want := time.Now().UTC().Add(6 * time.Hour) // the stub's returned expiry
	d := expiresAt.Sub(want)
	if d < 0 {
		d = -d
	}
	if d > 5*time.Minute {
		t.Errorf("pass 1: token_expires_at = %v, want ≈ now+6h (%v)", expiresAt, want)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("pass 1: captured %d posts, want exactly 1 (the pass completes end-to-end)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startA) {
		t.Errorf("pass 1: cursor = %v, want advanced ONCE to %v", got, startA)
	}

	// ---- pass 2: the token is now fresh (≈ now+6h > now+1h): no spurious re-refresh, no double-post ----
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if stub.refreshCalls != 1 {
		t.Errorf("pass 2: refresh calls = %d, want still 1 (no spurious re-refresh while the token is fresh)", stub.refreshCalls)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("pass 2: captured %d total posts, want still 1 (A is deduped — never double-posted)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startA) {
		t.Errorf("pass 2: cursor = %v, want %v (advanced exactly once in pass 1; the pass-2 re-list is free)", got, startA)
	}
}

// ---------------------------------------------------------------------------
// 15. a 404 on a listed activity (gone: deleted / access revoked) →
//     dispositioned 'skipped' on first sight — deterministic, never re-fetched
// ---------------------------------------------------------------------------

// TestStravaDetailGoneSkipsDeterministic pins the gone-404 disposition: pass 1
// — a family activity G whose detail fetch 404s is dispositioned 'skipped'
// IMMEDIATELY (not pending), ZERO posts, detail called EXACTLY ONCE (no 5-cycle
// re-fetch budget burned), and the cursor advanced as a normal disposition.
// Pass 2 — G re-listed (the 1h-overlapped window), detailFn still ErrGone: the
// row stays 'skipped' and detail is NOT re-invoked (detailCalls stays 1), the
// cursor is untouched. A 404 for an id the list just returned is a DETERMINISTIC
// outcome — the same category as off-family.
func TestStravaDetailGoneSkipsDeterministic(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startG := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete(t, pool, 9001, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 1, SportType: "Run", StartDate: startG}}, nil
		},
		detailFn: func(_ int64) (Activity, error) { return Activity{}, ErrGone{} },
	}
	s, caps := newTestStrava(t, pool, stub, 9001)
	ctx := context.Background()

	// ---- pass 1: the detail 404 disposes 'skipped' on first sight ----
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if status, retries := seenRow(t, pool, aid, 1); status != statusSkipped || retries != 0 {
		t.Fatalf("pass 1: G row = %q retries %d, want skipped/0 (disposed on first sight — never a pending row)", status, retries)
	}
	if n := len(caps.posts); n != 0 {
		t.Fatalf("pass 1: captured %d posts, want zero", n)
	}
	if n := stub.detailCalls; n != 1 {
		t.Fatalf("pass 1: detail calls = %d, want exactly 1 (no re-fetch cycle for a deterministic 404)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startG) {
		t.Errorf("pass 1: cursor = %v, want advanced to %v as a normal disposition", got, startG)
	}

	// ---- pass 2: G is re-listed (the −1h overlap window); detailFn still ERRs
	// GONE: the row stays skipped, detail is NOT re-invoked, cursor untouched ----
	if err := s.iteration(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if status, _ := seenRow(t, pool, aid, 1); status != statusSkipped {
		t.Errorf("pass 2: G row = %q, want still skipped", status)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("pass 2: captured %d posts, want zero", n)
	}
	if n := stub.detailCalls; n != 1 {
		t.Errorf("pass 2: detail calls = %d, want still 1 (a seen 'skipped' row is never re-fetched)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(startG) {
		t.Errorf("pass 2: cursor = %v, want unchanged (%v) — nothing newly dispositioned", got, startG)
	}
}

// ---------------------------------------------------------------------------
// The /strava command (onboarding): the command path needs no StravaAPI
// calls — a real test-DB pool is all it touches.
// ---------------------------------------------------------------------------

// commandInteraction builds a /strava slash-command interaction (the
// interaction-construction style: a *discordgo.Interaction carrying the
// command data + channel id directly).
func commandInteraction(channelID string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return &discordgo.Interaction{
		ChannelID: channelID,
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    "strava",
			Options: opts,
		},
	}
}

// onboardingRowCount is the strava_onboardings row count (the setup resets
// the table per test, so the count is isolated).
func onboardingRowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_onboardings`).Scan(&n); err != nil {
		t.Fatalf("count strava_onboardings: %v", err)
	}
	return n
}

// TestStravaCommandInsertsPendingOnboarding pins the happy path: the reply
// carries the authorize URL with a 32-hex state=, is EPHEMERAL (visible
// only to the invoker — the link is bound to them; a thread member cannot
// consume it), and the row is inserted (state, thread_id, label,
// status='pending', expires_at within 60–120 min of now()).
func TestStravaCommandInsertsPendingOnboarding(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, nil, 0)
	i := commandInteraction("999000111222", &discordgo.ApplicationCommandInteractionDataOption{
		Name:  "label",
		Type:  discordgo.ApplicationCommandOptionString,
		Value: "Matt",
	})
	r := s.HandleInteraction(i)
	if !r.Ephemeral {
		t.Errorf("Ephemeral = false, want true (the link is visible only to the invoker)")
	}

	wantPrefix := "https://www.strava.com/oauth/authorize?client_id=test-client-id&"
	if !strings.HasPrefix(r.Content, wantPrefix) {
		t.Fatalf("reply = %q, want it to start with %q", r.Content, wantPrefix)
	}
	stateSuffix := strings.SplitN(r.Content, "&state=", 2)
	if len(stateSuffix) != 2 {
		t.Fatalf("reply = %q, want a &state= token", r.Content)
	}
	if len(stateSuffix[1]) < 32 {
		t.Fatalf("state = %q, want at least 32 chars", stateSuffix[1])
	}
	state := stateSuffix[1][:32]
	for _, c := range state {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("state = %q, want all [0-9a-f]", state)
		}
	}

	var threadID int64
	var label, status string
	var expires time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT thread_id, label, status, expires_at FROM strava_onboardings WHERE state = $1`, state).
		Scan(&threadID, &label, &status, &expires)
	if err != nil {
		t.Fatalf("row lookup: %v", err)
	}
	if threadID != 999000111222 {
		t.Errorf("thread_id = %d, want 999000111222", threadID)
	}
	if label != "Matt" {
		t.Errorf("label = %q, want 'Matt'", label)
	}
	if status != "pending" {
		t.Errorf("status = %q, want 'pending'", status)
	}
	delta := time.Until(expires)
	// ±1 min: the DB's now() and Go's time.Now() skew by milliseconds
	// (the delta lands just under 60m in the DB-time-earlier case).
	if delta < 59*time.Minute || delta > 121*time.Minute {
		t.Errorf("expires_at = %v (%v from now), want within ~60–120 min (59–121)", expires, delta)
	}
}

// TestStravaCommandOmittedLabelIsNULL pins the omitted-label path: no
// options → the row's label IS NULL; the reply still carries a valid link.
func TestStravaCommandOmittedLabelIsNULL(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, nil, 0)
	i := commandInteraction("999000111222")
	r := s.HandleInteraction(i)
	if !strings.Contains(r.Content, "https://www.strava.com/oauth/authorize?client_id=test-client-id") {
		t.Fatalf("reply = %q, want a valid authorize link", r.Content)
	}
	state := strings.SplitN(r.Content, "&state=", 2)
	if len(state) != 2 || len(state[1]) < 32 {
		t.Fatalf("reply = %q, want a 32-hex &state= token", r.Content)
	}
	var label any
	if err := pool.QueryRow(context.Background(),
		`SELECT label FROM strava_onboardings WHERE state = $1`, state[1][:32]).Scan(&label); err != nil {
		t.Fatalf("row lookup: %v", err)
	}
	if label != nil {
		t.Errorf("label = %v, want SQL NULL", label)
	}
}

// TestStravaCommandUnconfiguredCreds pins the creds preflight: an empty
// StravaClientID → the "not configured" text, ZERO rows inserted (the link
// would be dead).
func TestStravaCommandUnconfiguredCreds(t *testing.T) {
	pool := setupStravaTestDB(t)
	s := &Strava{
		app: &app.App{
			Pool: pool,
			Cfg:  &config.Config{StravaPollMinutes: 15},
		},
		poll: 15 * time.Minute,
	}
	i := commandInteraction("999000111222", &discordgo.ApplicationCommandInteractionDataOption{
		Name:  "label",
		Type:  discordgo.ApplicationCommandOptionString,
		Value: "Matt",
	})
	r := s.HandleInteraction(i)
	want := "strava is not configured on this bot — the owner needs to set the client credentials first"
	if r.Content != want {
		t.Errorf("reply = %q, want %q", r.Content, want)
	}
	if n := onboardingRowCount(t, pool); n != 0 {
		t.Errorf("onboarding rows = %d, want zero", n)
	}
}

// TestStravaCommandFlagOffClause pins the disabled-clause: with the strava
// feature flag OFF the reply appends the clause (the command itself is NOT
// gated on the flag — the row is still inserted). The flag is restored in
// a t.Cleanup.
func TestStravaCommandFlagOffClause(t *testing.T) {
	pool := setupStravaTestDB(t)
	if _, err := pool.Exec(context.Background(), `UPDATE features SET enabled = false WHERE name = 'strava'`); err != nil {
		t.Fatalf("disable flag: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `UPDATE features SET enabled = true WHERE name = 'strava'`); err != nil {
			t.Errorf("restore flag: %v", err)
		}
	})
	s, _ := newTestStrava(t, pool, nil, 0)
	i := commandInteraction("999000111222")
	r := s.HandleInteraction(i)
	if !strings.Contains(r.Content, "(the strava feature is disabled — you'll be tracked once it's enabled)") {
		t.Errorf("reply = %q, want the disabled-clause", r.Content)
	}
	if !strings.Contains(r.Content, "https://www.strava.com/oauth/authorize") {
		t.Errorf("reply = %q, want the authorize link (the command is NOT gated on the flag)", r.Content)
	}
	if n := onboardingRowCount(t, pool); n != 1 {
		t.Errorf("onboarding rows = %d, want 1 (the row sits until the flag is enabled)", n)
	}
}

// TestStravaCommandUnparsableChannelID pins the empty ChannelID path: the
// "onboarding setup failed — try again" reply, ZERO rows inserted.
func TestStravaCommandUnparsableChannelID(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, nil, 0)
	i := commandInteraction("")
	r := s.HandleInteraction(i)
	want := "onboarding setup failed — try again"
	if r.Content != want {
		t.Errorf("reply = %q, want %q", r.Content, want)
	}
	if n := onboardingRowCount(t, pool); n != 0 {
		t.Errorf("onboarding rows = %d, want zero", n)
	}
}

// ---------------------------------------------------------------------------
// Onboarding callback (task 6): the :8643 in-process flow against a real PG
// with a stubbed StravaAPI and a capturing sendFn.
// ---------------------------------------------------------------------------

// seedOnboarding inserts one strava_onboardings row (label nil = NULL
// column; expiresAt controls the TTL branch).
func seedOnboarding(t *testing.T, pool *pgxpool.Pool, state string, threadID int64, label *string, status string, expiresAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO strava_onboardings (state, thread_id, label, status, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, now(), $5)`,
		state, threadID, label, status, expiresAt); err != nil {
		t.Fatalf("seed onboarding: %v", err)
	}
}

// onboardingStatus reads the onboarding row's status (ok=false when the row
// is gone — the expired-delete assertion).
func onboardingStatus(t *testing.T, pool *pgxpool.Pool, state string) (string, bool) {
	t.Helper()
	var s string
	err := pool.QueryRow(context.Background(), `SELECT status FROM strava_onboardings WHERE state = $1`, state).Scan(&s)
	if err != nil {
		var n int
		if e := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_onboardings WHERE state = $1`, state).Scan(&n); e == nil && n == 0 {
			return "", false
		}
		t.Fatalf("read onboarding status: %v", err)
	}
	return s, true
}

// athleteRowState reads one strava_athletes row by strava_athlete_id
// (targetThread zero = NULL column; ok=false when the row is absent).
func athleteRowState(t *testing.T, pool *pgxpool.Pool, stravaAthleteID int64) (label, access, refresh string, targetThread int64, needsReauth bool, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT label, access_token, refresh_token, COALESCE(target_thread_id, 0), needs_reauth
		 FROM strava_athletes WHERE strava_athlete_id = $1`, stravaAthleteID).
		Scan(&label, &access, &refresh, &targetThread, &needsReauth)
	if err != nil {
		var n int
		if e := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes WHERE strava_athlete_id = $1`, stravaAthleteID).Scan(&n); e == nil && n == 0 {
			return "", "", "", 0, false, false
		}
		t.Fatalf("read athlete row: %v", err)
	}
	return label, access, refresh, targetThread, needsReauth, true
}

// doCallback drives one request through a fresh OnboardingHandler (fresh
// closure-local throttle state per call — no cross-request budgeting).
func doCallback(t *testing.T, s *Strava, method, url string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	rec := httptest.NewRecorder()
	s.OnboardingHandler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// seedExistingAthlete inserts a strava_athletes row with an explicit
// needs_reauth flag (the re-consent upsert setup).
func seedExistingAthlete(t *testing.T, pool *pgxpool.Pool, label string, stravaAthleteID int64, targetThreadID int64, needsReauth bool) {
	t.Helper()
	now := time.Now().UTC()
	var target any
	if targetThreadID != 0 {
		target = targetThreadID
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO strava_athletes (label, strava_athlete_id, access_token, refresh_token, token_expires_at, target_thread_id, needs_reauth)
		 VALUES ($1, $2, 'old-access', 'old-refresh', $3, $4, $5)`,
		label, stravaAthleteID, now.Add(6*time.Hour), target, needsReauth); err != nil {
		t.Fatalf("seed existing athlete: %v", err)
	}
}

// TestOnboardingHappyPath pins the happy path: 200 "Matt is set up", the
// athlete row (label resolved from the first name — the row label was NULL,
// the canned tokens, needs_reauth false, the onboarding thread), the
// onboarding row 'done', and the captured confirm on the athlete's thread.
func TestOnboardingHappyPath(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt", Username: "matt"}, nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	if !strings.Contains(body, "Matt is set up") {
		t.Errorf("body = %q, want it to contain \"Matt is set up\"", body)
	}
	label, access, refresh, target, needsReauth, ok := athleteRowState(t, pool, 31337)
	if !ok {
		t.Fatalf("no strava_athletes row")
	}
	if label != "Matt" {
		t.Errorf("label = %q, want \"Matt\" (resolved from the first name — the row label was NULL)", label)
	}
	if access != "canned-access" || refresh != "canned-refresh" {
		t.Errorf("tokens = (%q, %q), want the canned exchange values", access, refresh)
	}
	if needsReauth {
		t.Errorf("needs_reauth = true, want false")
	}
	if target != 999000111222 {
		t.Errorf("target_thread_id = %d, want 999000111222", target)
	}
	if status, _ := onboardingStatus(t, pool, state); status != "done" {
		t.Errorf("onboarding status = %q, want \"done\"", status)
	}
	sent := caps.byThread("999000111222")
	if len(sent) != 1 {
		t.Fatalf("captured %d posts on the thread, want exactly 1", len(sent))
	}
	const want = "✅ Matt is now tracked — finished runs and rides will post here."
	if sent[0].msg != want {
		t.Errorf("confirm = %q, want %q", sent[0].msg, want)
	}
}

// TestOnboardingConcurrentSameState pins the claim race: two concurrent
// callbacks for the SAME state, both with valid codes (the stub's
// ExchangeCode and GetAthlete always succeed — deliberately bypassing
// the real single-use-code reality: the DB must be the arbiter). The
// exchange sleep widens the window so both requests pass the pre-exchange
// fast path before either claims (deterministic RED on the old code:
// both 200, both confirm). Assertions: exactly one 200 and one 409,
// exactly one confirm, the onboarding row 'done', exactly one athlete
// row.
func TestOnboardingConcurrentSameState(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		exchangeFn: func() (string, string, time.Time, string, error) {
			time.Sleep(100 * time.Millisecond) // widen the race window
			return "canned-access", "canned-refresh", time.Now().UTC().Add(6 * time.Hour), "read activity:read_all", nil
		},
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt", Username: "matt"}, nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/strava/callback?code=c1&state="+state, nil)
			rec := httptest.NewRecorder()
			s.OnboardingHandler().ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	sort.Ints(codes)
	if codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
		t.Fatalf("codes = %v, want exactly one 200 and one 409", codes)
	}
	sent := caps.byThread("999000111222")
	if len(sent) != 1 {
		t.Fatalf("captured %d confirms, want exactly 1", len(sent))
	}
	if status, ok := onboardingStatus(t, pool, state); !ok || status != "done" {
		t.Fatalf("onboarding status = %q (ok=%v), want \"done\"", status, ok)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 1 {
		t.Errorf("athlete rows = %d, want exactly 1", n)
	}
}

// TestOnboardingExplicitLabel pins the explicit-command-label override: the
// onboarding row's label wins over the athlete's first name.
func TestOnboardingExplicitLabel(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6"
	label := "M"
	seedOnboarding(t, pool, state, 999000111222, &label, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt", Username: "matt"}, nil },
	}
	s, _ := newTestStrava(t, pool, stub, 0)

	code, _ := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if label, _, _, _, _, ok := athleteRowState(t, pool, 31337); !ok || label != "M" {
		t.Errorf("label = %q (ok=%v), want \"M\" (the command label wins over the first name)", label, ok)
	}
}

// TestOnboardingUnknownState pins the no-row 404 (and that nothing was
// upserted).
func TestOnboardingUnknownState(t *testing.T) {
	pool := setupStravaTestDB(t)
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt"}, nil },
	}
	s, _ := newTestStrava(t, pool, stub, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state=deadbeef")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if !strings.Contains(body, "unknown or expired link") {
		t.Errorf("body = %q, want it to contain \"unknown or expired link\"", body)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingDenialRedirect pins Strava's Decline redirect: 200 "consent
// was not granted", NO failed mark (the pending row is left as-is — a
// denial is not a flow failure; the row expires harmlessly).
func TestOnboardingDenialRedirect(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?error=access_denied")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "consent was not granted") {
		t.Errorf("body = %q, want it to contain \"consent was not granted\"", body)
	}
	if status, ok := onboardingStatus(t, pool, state); !ok || status != "pending" {
		t.Errorf("onboarding status = %q (ok=%v), want \"pending\" (NO failed mark)", status, ok)
	}
}

// TestOnboardingExpiredState pins the expired-row delete: 404 "expired", the
// onboarding row DELETED, no athlete row.
func TestOnboardingExpiredState(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(-time.Hour))
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if !strings.Contains(body, "expired") {
		t.Errorf("body = %q, want it to contain \"expired\"", body)
	}
	if _, ok := onboardingStatus(t, pool, state); ok {
		t.Errorf("onboarding row still present, want it deleted")
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingAlreadyUsedState pins the non-pending 409 (and that nothing
// was upserted).
func TestOnboardingAlreadyUsedState(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6"
	seedOnboarding(t, pool, state, 999000111222, nil, "done", time.Now().UTC().Add(time.Hour))
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
	if !strings.Contains(body, "already used") {
		t.Errorf("body = %q, want it to contain \"already used\"", body)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingExchangeFailed pins the exchange-failure branch: 400
// "authorization failed", the row 'failed', no athlete row.
func TestOnboardingExchangeFailed(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		exchangeFn: func() (string, string, time.Time, string, error) { return "", "", time.Time{}, "", ErrInvalidCode },
	}
	s, _ := newTestStrava(t, pool, stub, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body, "authorization failed") {
		t.Errorf("body = %q, want it to contain \"authorization failed\"", body)
	}
	if status, _ := onboardingStatus(t, pool, state); status != "failed" {
		t.Errorf("onboarding status = %q, want \"failed\"", status)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingScopeMissing pins the scope-check branch: the exchange
// succeeds with scope "read" (no activity:read_all) → 400 "activity:read_all",
// the row 'failed', NO athlete row, and GetAthlete is NEVER reached.
func TestOnboardingScopeMissing(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		exchangeFn: func() (string, string, time.Time, string, error) {
			return "a", "r", time.Now().UTC().Add(6 * time.Hour), "read", nil
		},
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt"}, nil },
	}
	s, _ := newTestStrava(t, pool, stub, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body, "activity:read_all") {
		t.Errorf("body = %q, want it to contain \"activity:read_all\"", body)
	}
	if stub.athleteCalls != 0 {
		t.Errorf("GetAthlete calls = %d, want 0 (the scope check must precede the fetch)", stub.athleteCalls)
	}
	if status, _ := onboardingStatus(t, pool, state); status != "failed" {
		t.Errorf("onboarding status = %q, want \"failed\"", status)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingAthleteFetchFailed pins the athlete-fetch-failure branch:
// 400 "could not load the athlete profile", the row 'failed', no athlete row.
func TestOnboardingAthleteFetchFailed(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8"
	seedOnboarding(t, pool, state, 999000111222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{}, errors.New("fetch failed") },
	}
	s, _ := newTestStrava(t, pool, stub, 0)

	code, body := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body, "could not load the athlete profile") {
		t.Errorf("body = %q, want it to contain \"could not load the athlete profile\"", body)
	}
	if status, _ := onboardingStatus(t, pool, state); status != "failed" {
		t.Errorf("onboarding status = %q, want \"failed\"", status)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes`).Scan(&n); err != nil {
		t.Fatalf("count strava_athletes: %v", err)
	}
	if n != 0 {
		t.Errorf("athlete rows = %d, want zero", n)
	}
}

// TestOnboardingExistingAthleteUpsert pins the re-consent edge case: an
// EXISTING athlete with an omitted (NULL) onboarding label → the stored
// label is PRESERVED, the thread moves to the newest, needs_reauth is
// cleared, the tokens are replaced, and the confirm is the refresh variant.
func TestOnboardingExistingAthleteUpsert(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9"
	seedExistingAthlete(t, pool, "OldName", 31337, 111, true)
	seedOnboarding(t, pool, state, 222, nil, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt"}, nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)

	code, _ := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	label, access, refresh, target, needsReauth, ok := athleteRowState(t, pool, 31337)
	if !ok {
		t.Fatalf("no strava_athletes row")
	}
	if label != "OldName" {
		t.Errorf("label = %q, want \"OldName\" (PRESERVED — the onboarding label was NULL: the spec's edge case)", label)
	}
	if target != 222 {
		t.Errorf("target_thread_id = %d, want 222 (moved to the newest)", target)
	}
	if needsReauth {
		t.Errorf("needs_reauth = true, want false")
	}
	if access != "canned-access" || refresh != "canned-refresh" {
		t.Errorf("tokens = (%q, %q), want the new canned exchange values", access, refresh)
	}
	sent := caps.byThread("222")
	if len(sent) != 1 {
		t.Fatalf("captured %d posts on 222, want exactly 1", len(sent))
	}
	const want = "🔄 OldName's Strava authorization was refreshed."
	if sent[0].msg != want {
		t.Errorf("confirm = %q, want %q", sent[0].msg, want)
	}
}

// TestOnboardingExistingAthleteExplicitLabelOverrides pins the explicit
// command label overriding the stored label on a re-consent.
func TestOnboardingExistingAthleteExplicitLabelOverrides(t *testing.T) {
	pool := setupStravaTestDB(t)
	const state = "d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0"
	seedExistingAthlete(t, pool, "OldName", 31337, 111, true)
	label := "New"
	seedOnboarding(t, pool, state, 222, &label, "pending", time.Now().UTC().Add(time.Hour))
	stub := &stubStrava{
		athleteFn: func() (Athlete, error) { return Athlete{ID: 31337, Firstname: "Matt"}, nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)

	code, _ := doCallback(t, s, http.MethodGet, "/strava/callback?code=c1&state="+state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if label, _, _, _, _, ok := athleteRowState(t, pool, 31337); !ok || label != "New" {
		t.Errorf("label = %q (ok=%v), want \"New\" (the explicit command label overrides the stored one)", label, ok)
	}
	sent := caps.byThread("222")
	if len(sent) != 1 {
		t.Fatalf("captured %d posts on 222, want exactly 1", len(sent))
	}
	const want = "🔄 New's Strava authorization was refreshed."
	if sent[0].msg != want {
		t.Errorf("confirm = %q, want %q", sent[0].msg, want)
	}
}

// TestOnboardingWrongRoute pins the route guard: GET / and POST
// /strava/callback both 404.
func TestOnboardingWrongRoute(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, nil, 0)

	if code, _ := doCallback(t, s, http.MethodGet, "/"); code != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404", code)
	}
	if code, _ := doCallback(t, s, http.MethodPost, "/strava/callback"); code != http.StatusNotFound {
		t.Errorf("POST /strava/callback status = %d, want 404", code)
	}
}

// TestOnboardingTickCleanup pins the per-tick hygiene: the expired
// onboarding row is deleted by iteration (BEFORE the flag check — the flag
// state doesn't matter; it's left as-is here), the live row remains.
func TestOnboardingTickCleanup(t *testing.T) {
	pool := setupStravaTestDB(t)
	const (
		expiredState = "e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1"
		liveState    = "f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2"
	)
	seedOnboarding(t, pool, expiredState, 111, nil, "pending", time.Now().UTC().Add(-time.Hour))
	seedOnboarding(t, pool, liveState, 222, nil, "pending", time.Now().UTC().Add(time.Hour))
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)

	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if _, ok := onboardingStatus(t, pool, expiredState); ok {
		t.Errorf("expired onboarding row still present, want it deleted")
	}
	if _, ok := onboardingStatus(t, pool, liveState); !ok {
		t.Errorf("live onboarding row deleted, want it to remain")
	}
}

// ---------------------------------------------------------------------------
// Webhook (task 2: the synchronous surface — the worker body is the stub;
// Task 3 replaces it. The job is captured at the worker seam (s.workerFn)
// — NEVER the raw s.jobs channel, which the two live workers actively
// consume and would race; the seam capture also survives Task 3's real
// body unchanged).
// ---------------------------------------------------------------------------

// doWebhook drives one POST /strava/webhook through a FRESH WebhookHandler
// (fresh closure-local throttle map per call — the throttle tests call the
// factory once and reuse the handler).
func doWebhook(t *testing.T, s *Strava, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/strava/webhook", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.WebhookHandler().ServeHTTP(rec, req)
	return rec.Code
}

// TestWebhookEnqueuesJob pins the full enqueue path (a real athlete row —
// the unit tests can't get past the nil-pool DB-failure arm): a create
// event for a known athlete enqueues a job (captured at the worker seam)
// with the athlete row + activity id, and 200s.
func TestWebhookEnqueuesJob(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	captured := make(chan webhookJob)
	s.workerFn = func(_ context.Context, j webhookJob) { captured <- j }
	if code := doWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	select {
	case j := <-captured:
		if j.actID != 555 {
			t.Errorf("actID = %d, want 555 (the payload's object_id)", j.actID)
		}
		if j.ath == nil || j.ath.label != "Web" {
			t.Errorf("ath = %+v, want the seeded row (label Web)", j.ath)
		}
		if j.aspect != "create" {
			t.Errorf("aspect = %q, want create (the log-only diagnostic)", j.aspect)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no job captured within 5s (the worker seam should observe the enqueued job)")
	}
}

// TestWebhookNumericPayloadEndToEnd pins the DOCUMENTED wire shape end-to-end
// (JSON NUMBERS — the shape Strava actually sends, per
// docs/research/strava-webhooks.md — NOT strings): a numeric create event
// for a known athlete enqueues a job with the right activity id + athlete
// row. (The string-shaped unit test pins the lenient acceptance; this is
// the shape the wire carries — a string-only decode would silently 200-no-op
// every real delivery, masked by the poll backstop.)
func TestWebhookNumericPayloadEndToEnd(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	captured := make(chan webhookJob)
	s.workerFn = func(_ context.Context, j webhookJob) { captured <- j }
	if code := doWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`); code != http.StatusOK {
		t.Fatalf("numeric create event = %d, want 200", code)
	}
	select {
	case j := <-captured:
		if j.actID != 555 {
			t.Errorf("actID = %d, want 555 (the payload's numeric object_id)", j.actID)
		}
		if j.ath == nil || j.ath.label != "Web" {
			t.Errorf("ath = %+v, want the seeded row (label Web)", j.ath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no job captured within 5s (the documented numeric payload must reach the worker — a string-only decode would silently no-op it on the wire)")
	}
}

// TestWebhookQueueFull pins the non-blocking enqueue: a SATURATED 32-cap
// queue (the 2 workers each PARKED on the first job they consume, the
// remaining sends filling the queue) DROPS the event with a log — 200, no
// panic, and the job is NOT added (len stays at cap — the default: arm
// fired; a blocking-send regression would HANG the POST, caught by the
// timeout). The poll backstop covers the drop.
func TestWebhookQueueFull(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	// Park the worker body BEFORE the first startWorkers() (the factory
	// runs on the WebhookHandler call below): the worker loop consumes
	// ONE job and THEN calls the body, so each parked worker HOLDS one
	// job — cap+2 sends land deterministically (2 held by the parked
	// workers, the remaining cap filling the queue; the last send can
	// only complete once a worker has taken one, and a parked worker
	// takes at most one until the ctx is done).
	ctx, cancel := context.WithCancel(context.Background())
	s.serverCtx = ctx // test-scoped: the parked workers exit on cancel (no goroutine leak)
	t.Cleanup(cancel)
	s.workerFn = func(ctx context.Context, _ webhookJob) { <-ctx.Done() }
	h := s.WebhookHandler()
	for i := 0; i < cap(s.jobs)+2; i++ {
		s.jobs <- webhookJob{}
	}
	if n := len(s.jobs); n != cap(s.jobs) {
		t.Fatalf("queue not saturated: len = %d, want %d (cap) — the 2 parked workers must hold exactly 2 jobs", n, cap(s.jobs))
	}
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/strava/webhook", strings.NewReader(`{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req) // a full queue must DROP (the non-blocking send) — no panic, 200
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("full-queue event = %d, want 200 (the enqueue is non-blocking — a full queue drops with a log)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event HUNG with a full queue — the enqueue must be non-blocking (a blocking send would block the 200)")
	}
	if n := len(s.jobs); n != cap(s.jobs) {
		t.Errorf("len = %d after the event, want %d (cap) — the job was NOT added (the default: drop arm fired)", n, cap(s.jobs))
	}
}

// TestWebhook200BeforeWork pins the ACK contract: with the worker body
// BLOCKED before the first startWorkers(), the POST still 200s — the 200
// goes out BEFORE any worker work (Strava expects 200 within 2 s; the work
// is async). If the handler waited for the worker, the request would hang.
func TestWebhook200BeforeWork(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	// Block the worker body BEFORE the first startWorkers() (the factory
	// runs on the WebhookHandler call below): the worker consumes the job
	// and blocks in the body, so the response can only arrive if the 200
	// is sent before any worker work.
	ctx, cancel := context.WithCancel(context.Background())
	s.serverCtx = ctx // test-scoped: the parked workers exit on cancel (no goroutine leak)
	t.Cleanup(cancel)
	s.workerFn = func(ctx context.Context, _ webhookJob) { <-ctx.Done() }
	h := s.WebhookHandler()
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/strava/webhook", strings.NewReader(`{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("event = %d, want 200 (the ACK is the 2 s contract — sent before any worker work)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the response did not arrive while the worker is blocked — the 200 must go out BEFORE any worker work")
	}
}

// TestWebhookDeauthSetsFlag pins the deauth arm: an athlete update carrying
// authorized: "false" (the exact string) sets needs_reauth=TRUE (one small
// write, inside the 2 s budget); a duplicate is idempotent (still TRUE, no
// error). An AMBIGUOUS shape (a boolean, not the string "false") is a 200
// no-op — the poll's 401 arm handles it; we do NOT guess.
func TestWebhookDeauthSetsFlag(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, _ := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	seedAthlete2(t, pool, "Amb", 778, 43, time.Time{}, 6*time.Hour)
	if _, _, _, _, needsReauth, ok := athleteRowState(t, pool, 777); !ok || needsReauth {
		t.Fatalf("seed state: needsReauth = %v (ok %v), want the fresh false flag", needsReauth, ok)
	}
	deauth := `{"aspect_type":"update","object_type":"athlete","owner_id":777,"updates":{"authorized":"false"}}`
	if code := doWebhook(t, s, deauth); code != http.StatusOK {
		t.Fatalf("deauth event = %d, want 200", code)
	}
	if _, _, _, _, needsReauth, ok := athleteRowState(t, pool, 777); !ok || !needsReauth {
		t.Fatalf("needs_reauth = %v (ok %v), want TRUE after the deauth event", needsReauth, ok)
	}
	// Duplicate: still TRUE, no error (idempotent).
	if code := doWebhook(t, s, deauth); code != http.StatusOK {
		t.Fatalf("duplicate deauth event = %d, want 200 (idempotent)", code)
	}
	if _, _, _, _, needsReauth, _ := athleteRowState(t, pool, 777); !needsReauth {
		t.Fatal("needs_reauth = false after the duplicate, want still TRUE")
	}
	// Ambiguous shape: a BOOLEAN authorized (not the string "false") is a
	// 200 no-op — the flag is untouched (we do NOT guess the shape).
	if code := doWebhook(t, s, `{"aspect_type":"update","object_type":"athlete","owner_id":778,"updates":{"authorized":false}}`); code != http.StatusOK {
		t.Fatalf("ambiguous deauth event = %d, want 200", code)
	}
	if _, _, _, _, needsReauth, _ := athleteRowState(t, pool, 778); needsReauth {
		t.Fatal("needs_reauth = true on an ambiguous shape, want the flag untouched (no guessing)")
	}
}

// TestWebhookUnknownOwnerNoop pins the unknown-owner arm: a create event for
// an owner_id with NO strava_athletes row is a 200 no-op — no row is
// created, no job is enqueued, no post goes out (the poll backstop covers
// the gap; we never invent athletes from a webhook).
func TestWebhookUnknownOwnerNoop(t *testing.T) {
	pool := setupStravaTestDB(t)
	s, caps := newTestStrava(t, pool, &stubStrava{}, 0)
	seedAthlete2(t, pool, "Web", 777, 42, time.Time{}, 6*time.Hour)
	captured := make(chan webhookJob, 1)
	s.workerFn = func(_ context.Context, j webhookJob) { captured <- j }
	if code := doWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":999999}`); code != http.StatusOK {
		t.Fatalf("unknown-owner event = %d, want 200 (a no-op, never 5xx)", code)
	}
	select {
	case <-captured:
		t.Fatal("a job was captured, want none (unknown owner → no row → no job)")
	case <-time.After(200 * time.Millisecond):
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM strava_athletes WHERE strava_athlete_id = 999999`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("strava_athletes rows for the unknown owner = %d, want 0 (no row is created)", n)
	}
	if len(caps.posts) != 0 {
		t.Fatalf("posts = %d, want 0 (no post for an unknown owner)", len(caps.posts))
	}
}

// ---------------------------------------------------------------------------
// The webhook worker body (task 3): the fetch + the start_date rule + the
// per-event transaction (the rows-affected gate) + the post, against a real
// PG. The workerFn seam (task 2) is the capture mechanism — the test sets
// s.workerFn to a fn that runs the REAL body (s.workerBody) inline in the
// worker goroutine and signals completion; the body is the production code
// path (the seam is the completion signal, not a test double).
// ---------------------------------------------------------------------------

// installRealWorker sets s.workerFn to a fn that runs the real worker body
// inline (in the worker goroutine) and signals each completion on the
// returned channel. Call it BEFORE the first WebhookHandler() (the factory
// starts the workers; the worker reads s.workerFn at consumption, so the
// set happens-before the first job).
func installRealWorker(s *Strava) chan struct{} {
	done := make(chan struct{}, 8)
	s.workerFn = func(ctx context.Context, j webhookJob) {
		s.workerBody(ctx, j)
		done <- struct{}{}
	}
	return done
}

// waitForJob waits for one completed worker job (bounded deadline — a hang
// fails the test).
func waitForJob(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker job did not complete within 5s")
	}
}

// postWebhook drives one POST /strava/webhook (a fresh handler) and waits
// for the (real-body) worker job to complete.
func postWebhook(t *testing.T, s *Strava, body string, done chan struct{}) int {
	t.Helper()
	code := doWebhook(t, s, body)
	waitForJob(t, done)
	return code
}

// seenStartDate reads a seen row's start_date (zero when the row is absent).
func seenStartDate(t *testing.T, pool *pgxpool.Pool, athleteID int32, activityID int64) time.Time {
	t.Helper()
	var v time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT start_date FROM strava_seen_activities WHERE strava_athletes_id=$1 AND strava_activity_id=$2`,
		athleteID, activityID).Scan(&v)
	if err != nil {
		return time.Time{}
	}
	return v
}

// ---------------------------------------------------------------------------
// 1. a create event for a known athlete → the activity is posted (the 2-line
//    post) and a 'posted' seen row exists with start_date = the detail's
//    StartDate; the cursor is untouched (the webhook never writes it).
// ---------------------------------------------------------------------------

func TestWebhookPostsNewFamily(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	sent := caps.byThread("42")
	if len(caps.posts) != 1 || len(sent) != 1 {
		t.Fatalf("captured %d posts, want exactly 1 on 42", len(caps.posts))
	}
	const want = "Web finished 42.3 km Run\nhttps://www.strava.com/activities/555"
	if sent[0].msg != want {
		t.Errorf("post = %q, want %q", sent[0].msg, want)
	}
	if status, _ := seenRow(t, pool, aid, 555); status != statusPosted {
		t.Errorf("row = %q, want %q", status, statusPosted)
	}
	if got := seenStartDate(t, pool, aid, 555); !got.Equal(startA) {
		t.Errorf("row start_date = %v, want the detail's StartDate %v", got, startA)
	}
	// The webhook NEVER writes the cursor (the poll's watermark).
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v) — the webhook path never writes last_polled_at", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 2. a create event TWICE (two POSTs, same activity id) → exactly ONE post
//    (the second worker's INSERT is a DO NOTHING → 0 rows → no post — the
//    rows-affected gate, the concurrency-safe dedupe).
// ---------------------------------------------------------------------------

func TestWebhookDedupe(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)
	body := `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`

	if code := postWebhook(t, s, body, done); code != http.StatusOK {
		t.Fatalf("first event = %d, want 200", code)
	}
	if n := len(caps.posts); n != 1 {
		t.Fatalf("after the first event: %d posts, want exactly 1", n)
	}
	// The duplicate delivery (Strava's documented duplicates arrive close
	// together): the second worker's INSERT is a DO NOTHING → 0 rows → no
	// post (the rows-affected gate — the unique constraint does the work,
	// the rows-affected check is the gate).
	if code := postWebhook(t, s, body, done); code != http.StatusOK {
		t.Fatalf("duplicate event = %d, want 200", code)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("after the duplicate = %d posts, want still 1 (the second worker's INSERT is a DO NOTHING → 0 rows → no post)", n)
	}
	if n := seenCount(t, pool, aid); n != 1 {
		t.Errorf("seen rows = %d, want exactly 1", n)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPosted || retries != 0 {
		t.Errorf("row = %q retries %d, want posted/0 (the terminal row is never overwritten)", status, retries)
	}
	if n := stub.detailCalls; n != 2 {
		t.Errorf("detail calls = %d, want 2 (both workers fetch; the dedupe is at the WRITE, not the fetch)", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 3. a create event, then an update event for the same id AFTER the first
//    posted → the second is ABSORBED (the terminal row → no re-post, no
//    re-evaluation — the no-re-post / no-re-evaluation invariant).
// ---------------------------------------------------------------------------

func TestWebhookCreateAfterPostedAbsorbed(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	if n := len(caps.posts); n != 1 {
		t.Fatalf("after the create: %d posts, want exactly 1", n)
	}
	if status, _ := seenRow(t, pool, aid, 555); status != statusPosted {
		t.Fatalf("row = %q, want posted (the create event posted it)", status)
	}
	// Then the update event: ABSORBED (the terminal row → no re-post, no
	// re-evaluation — the row is never overwritten).
	if code := postWebhook(t, s, `{"aspect_type":"update","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("update event = %d, want 200", code)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("after the update = %d posts, want still 1 (the terminal row absorbs — no re-post)", n)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPosted || retries != 0 {
		t.Errorf("row = %q retries %d, want posted/0 (the terminal row is never overwritten)", status, retries)
	}
	if n := stub.detailCalls; n != 2 {
		t.Errorf("detail calls = %d, want 2 (the update is fetched; the ABSORPTION is at the write)", n)
	}
}

// ---------------------------------------------------------------------------
// 4. a create event → pending (stub isProcessing), then an update event →
//    the pending row RESOLVES to posted (the pending→terminal UPDATE path),
//    one post; start_date stays the first-creation value.
// ---------------------------------------------------------------------------

func TestWebhookUpdateResolvesPending(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return processingAct(id, startA), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	// The create event → a pending row, no post.
	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPending || retries != 0 {
		t.Fatalf("row = %q retries %d, want pending/0 (still processing)", status, retries)
	}
	if got := seenStartDate(t, pool, aid, 555); got.IsZero() || !got.Equal(startA) {
		t.Fatalf("row start_date = %v, want the detail's StartDate %v (never the zero time — the start_date rule)", got, startA)
	}
	if n := len(caps.posts); n != 0 {
		t.Fatalf("after the create: %d posts, want zero (pending)", n)
	}
	// The update event → the pending row resolves to posted (the
	// pending→terminal UPDATE path), one post.
	stub.detailFn = func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil }
	if code := postWebhook(t, s, `{"aspect_type":"update","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("update event = %d, want 200", code)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPosted || retries != 0 {
		t.Errorf("row = %q retries %d, want posted/0 (resolved on its first re-fetch)", status, retries)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("after the update = %d posts, want exactly 1 (the pending→terminal resolution posts)", n)
	}
	if got := seenStartDate(t, pool, aid, 555); !got.Equal(startA) {
		t.Errorf("row start_date = %v, want UNCHANGED %v (the UPDATE does not touch it — set at first creation)", got, startA)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 5. a 401 AND a 401 refresh (genuinely revoked — the REFRESH failure is
//    what sets the flag, per the refresh-and-retry arm) → needs_reauth=TRUE,
//    no seen row, no post, the cursor unchanged. No further retries inside
//    the worker (the next event or the next poll tick is the retry).
// ---------------------------------------------------------------------------

func TestWebhook401RefreshFailsSetsFlag(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) { return Activity{}, ErrUnauthorized{Why: "revoked"} },
		refreshFn: func() (string, string, time.Time, error) {
			return "", "", time.Time{}, ErrUnauthorized{Why: "invalid_grant"}
		},
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	if !needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = false, want TRUE (the refresh 401s — genuinely revoked — the refresh failure is what sets the flag)")
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (no seen row on the 401 arm)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v) — the 401 arm never writes last_polled_at", got, WINDOW)
	}
	if n := stub.detailCalls; n != 1 {
		t.Errorf("detail calls = %d, want exactly 1 (no further retries inside the worker — the next event or the next poll tick is the retry)", n)
	}
	if n := stub.refreshCalls; n != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// 6. a 401 with a SUCCEEDING refresh (an EXPIRED token — the bot was down
//    >= 6h; Strava tokens last 6h — NOT a revoked one: a bare flag would
//    conflate the two and permanently disable the athlete until manual
//    re-consent) → refresh + retry ONCE → the fetch succeeds, the post
//    happens, needs_reauth stays FALSE, and the ROTATED refresh token is
//    persisted in the athlete row (or the token chain dies).
// ---------------------------------------------------------------------------

func TestWebhook401RefreshSucceeds(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	var fetches int
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) {
			fetches++
			if fetches == 1 {
				return Activity{}, ErrUnauthorized{Why: "expired"} // the 401 → refresh + retry
			}
			return readyAct(id, startA, 42300), nil // the retry succeeds
		},
		refreshFn: func() (string, string, time.Time, error) {
			return "waccess", "wrefresh", time.Now().UTC().Add(6 * time.Hour), nil
		},
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	if n := fetches; n != 2 {
		t.Fatalf("detail fetches = %d, want exactly 2 (the 401 + the ONE retry — the loop guard)", n)
	}
	if n := stub.refreshCalls; n != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", n)
	}
	if needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = true, want FALSE (an expired token refreshes — it is not revoked)")
	}
	// The ROTATED pair persisted (the new refresh token MUST be
	// re-persisted or the token chain dies).
	_, access, refresh, _, _, ok := athleteRowState(t, pool, 777)
	if !ok {
		t.Fatal("no athlete row")
	}
	if access != "waccess" {
		t.Errorf("access_token = %q, want 'waccess' (the NEW access token — the exact column)", access)
	}
	if refresh != "wrefresh" {
		t.Errorf("refresh_token = %q, want the ROTATED 'wrefresh' (the chain dies without it)", refresh)
	}
	var expiresAt time.Time
	if err := pool.QueryRow(context.Background(), `SELECT token_expires_at FROM strava_athletes WHERE id=$1`, aid).Scan(&expiresAt); err != nil {
		t.Fatalf("read token_expires_at: %v", err)
	}
	want := time.Now().UTC().Add(6 * time.Hour) // the stub's returned expiry
	d := expiresAt.Sub(want)
	if d < 0 {
		d = -d
	}
	if d > 5*time.Minute {
		t.Errorf("token_expires_at = %v, want ≈ now+6h (%v)", expiresAt, want)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("captured %d posts, want exactly 1 (the retry fetch succeeded → the post happens)", n)
	}
	if status, _ := seenRow(t, pool, aid, 555); status != statusPosted {
		t.Errorf("row = %q, want posted", status)
	}
	got := cursorAt(t, pool, aid)
	if !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 6b. a 401 where a CONCURRENT rotation (the poll's step-a, another worker
//     — the near-expiry window) rotates the stored refresh token between the
//     worker's snapshot and the worker's refresh → the refresh 401s
//     (invalid_grant — Strava answers 401 for the rotated-away token) but
//     the athlete is HEALTHY → NO needs_reauth flag (a spurious flag would
//     disable a healthy athlete until manual re-consent: loadAthletes filters
//     flagged rows out). The invariant: a failed refresh whose refresh_token
//     no longer matches the stored one → NO flag.
// ---------------------------------------------------------------------------

func TestWebhook401ConcurrentRotationNoFlag(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	// Pre-set the stored refresh token to X (the "before" value — the seed's
	// 'rtok' would work too; X is explicit about the rotation X → Y).
	if _, err := pool.Exec(context.Background(),
		`UPDATE strava_athletes SET refresh_token = 'X' WHERE id = $1`, aid); err != nil {
		t.Fatalf("pre-set refresh token: %v", err)
	}
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) { return Activity{}, ErrUnauthorized{Why: "expired"} },
		refreshFn: func() (string, string, time.Time, error) {
			// The "other source" (the poll's step-a / another worker) rotates
			// the stored pair X → Y between the worker's snapshot and the
			// worker's refresh — the deterministic simulation of the race.
			if _, err := pool.Exec(context.Background(),
				`UPDATE strava_athletes SET refresh_token = 'Y' WHERE id = $1`, aid); err != nil {
				return "", "", time.Time{}, err
			}
			// The worker's refresh with the rotated-away X then fails
			// (invalid_grant — Strava answers 401 for a rotated-away token).
			return "", "", time.Time{}, ErrUnauthorized{Why: "invalid_grant"}
		},
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	// The stored refresh token (Y) no longer equals the value the failed
	// attempt used (X) → someone else already rotated it → NO flag (the
	// athlete is healthy — a spurious flag would disable it until manual
	// re-consent).
	if needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = true, want FALSE (the stored token rotated concurrently — the 401 was a stale-snapshot artifact, not a revocation)")
	}
	// The stored token is the CONCURRENT source's Y (the worker's FAILED
	// refresh persisted nothing — only a SUCCESS refresh persists the pair).
	_, _, refresh, _, _, ok := athleteRowState(t, pool, 777)
	if !ok {
		t.Fatal("no athlete row")
	}
	if refresh != "Y" {
		t.Errorf("refresh_token = %q, want 'Y' (the concurrent source's rotation — the failed refresh persisted nothing)", refresh)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (no seen row on the 401 arm)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v) — the 401 arm never writes last_polled_at", got, WINDOW)
	}
	if n := stub.detailCalls; n != 1 {
		t.Errorf("detail calls = %d, want exactly 1 (no further retries inside the worker)", n)
	}
	if n := stub.refreshCalls; n != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// 6c. a 401 where the refresh SUCCEEDS (an EXPIRED token — refresh + retry)
//     but the RETRY fetch 401s (the explicit loop guard — the refresh did not
//     cure it) → the same arm as a 401 refresh: lastRefresh = newRefresh (the
//     value the worker JUST persisted — the stored token still equals it when
//     no concurrent rotation landed after the persist) → the atomic UPDATE …
//     WHERE refresh_token = newRefresh matches → needs_reauth=TRUE (the token
//     is genuinely revoked — the refresh did not cure the 401). The ROTATED
//     refresh token is persisted (or the token chain dies).
// ---------------------------------------------------------------------------

func TestWebhook401RetryFailsSetsFlag(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	// Pre-set the stored refresh token to X (the seed's 'rtok' would work too;
	// X is explicit about the rotation X → newRefresh the worker persists).
	if _, err := pool.Exec(context.Background(),
		`UPDATE strava_athletes SET refresh_token = 'X' WHERE id = $1`, aid); err != nil {
		t.Fatalf("pre-set refresh token: %v", err)
	}
	var fetches int
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) {
			fetches++
			return Activity{}, ErrUnauthorized{Why: "revoked"} // 401 on BOTH the first + the retry
		},
		refreshFn: func() (string, string, time.Time, error) {
			return "waccess", "wrefresh", time.Now().UTC().Add(6 * time.Hour), nil
		},
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	// The worker reloaded ath (refresh_token = X), the refresh SUCCEEDED → the
	// worker persisted the ROTATED pair (refresh_token = newRefresh = 'wrefresh'),
	// then the RETRY fetch 401s → reauthWebhook(ctx, ath, newRefresh): the stored
	// token IS newRefresh (the worker just persisted it) → the atomic UPDATE …
	// WHERE refresh_token = newRefresh matches (RowsAffected == 1) → flag set.
	if !needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = false, want TRUE (the refresh did not cure the 401 — the retry also 401s → genuinely revoked)")
	}
	// The ROTATED refresh token is persisted (the chain dies without it).
	_, access, refresh, _, _, ok := athleteRowState(t, pool, 777)
	if !ok {
		t.Fatal("no athlete row")
	}
	if refresh != "wrefresh" {
		t.Errorf("refresh_token = %q, want 'wrefresh' (the ROTATED newRefresh the worker persisted — the chain dies without it)", refresh)
	}
	if access != "waccess" {
		t.Errorf("access_token = %q, want 'waccess' (the NEW access token)", access)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (no seen row on the 401 arm)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v) — the 401 arm never writes last_polled_at", got, WINDOW)
	}
	if n := fetches; n != 2 {
		t.Errorf("detail calls = %d, want exactly 2 (the 401 + the ONE retry — the loop guard)", n)
	}
	if n := stub.refreshCalls; n != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// 6d. a 401 where the refresh SUCCEEDS and the RETRY fetch 401s, but a
//     CONCURRENT rotation (the poll's step-a, another worker) rotates the
//     stored refresh token to a THIRD value Z AFTER the worker persisted
//     newRefresh → the atomic UPDATE … WHERE refresh_token = newRefresh
//     matches 0 rows (the stored token is now Z, not newRefresh) → NO
//     needs_reauth flag (the athlete is HEALTHY — a spurious flag would disable
//     it until manual re-consent: loadAthletes filters flagged rows out).
// ---------------------------------------------------------------------------

func TestWebhook401RetryFailsConcurrentRotationNoFlag(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	// Pre-set the stored refresh token to X (the seed's 'rtok' would work too;
	// X is explicit about the rotation X → newRefresh → Z).
	if _, err := pool.Exec(context.Background(),
		`UPDATE strava_athletes SET refresh_token = 'X' WHERE id = $1`, aid); err != nil {
		t.Fatalf("pre-set refresh token: %v", err)
	}
	var fetches int
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) {
			fetches++
			if fetches == 1 {
				return Activity{}, ErrUnauthorized{Why: "expired"} // the 401 → refresh + retry
			}
			// The RETRY (fetch 2) lands AFTER the worker persisted newRefresh:
			// a concurrent source (the poll's step-a / another worker) rotates
			// the stored pair to a THIRD value Z — the deterministic simulation
			// of the race — then the retry 401s.
			if _, err := pool.Exec(context.Background(),
				`UPDATE strava_athletes SET refresh_token = 'Z' WHERE id = $1`, aid); err != nil {
				return Activity{}, err
			}
			return Activity{}, ErrUnauthorized{Why: "revoked"}
		},
		refreshFn: func() (string, string, time.Time, error) {
			return "waccess", "wrefresh", time.Now().UTC().Add(6 * time.Hour), nil
		},
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	// The worker persisted newRefresh ('wrefresh'), then the concurrent source
	// rotated the stored token to Z (the retry call), then the retry 401s →
	// reauthWebhook(ctx, ath, newRefresh): the stored token is now Z, NOT
	// newRefresh → the atomic UPDATE … WHERE refresh_token = newRefresh matches
	// 0 rows → NO flag (the athlete is healthy — a spurious flag would disable
	// it until manual re-consent).
	if needsReauthAt(t, pool, aid) {
		t.Error("needs_reauth = true, want FALSE (the stored token rotated concurrently to Z after the persist — the 401 was a stale-snapshot artifact, not a revocation)")
	}
	// The stored token is the CONCURRENT source's Z (it landed AFTER the
	// worker persisted newRefresh — the worker's persist is the loser's write).
	_, _, refresh, _, _, ok := athleteRowState(t, pool, 777)
	if !ok {
		t.Fatal("no athlete row")
	}
	if refresh != "Z" {
		t.Errorf("refresh_token = %q, want 'Z' (the concurrent source's rotation — it landed after the worker persisted newRefresh)", refresh)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (no seen row on the 401 arm)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v) — the 401 arm never writes last_polled_at", got, WINDOW)
	}
	if n := fetches; n != 2 {
		t.Errorf("detail calls = %d, want exactly 2 (the 401 + the ONE retry — the loop guard)", n)
	}
	if n := stub.refreshCalls; n != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// 7. a 404 (gone) → NO seen row (the start_date rule: a gone outcome has no
//    valid detail — a zero start_date on a pending row would poison the poll
//    cursor's min(pending) hold), no post, 200. The poll's gone arm creates
//    the skipped row on its next tick (the backstop, decision 0013).
// ---------------------------------------------------------------------------

func TestWebhookGoneNoRow(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) { return Activity{}, ErrGone{} },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200 (a gone outcome is a 200 — the poll backstop covers it)", code)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (a gone outcome has no valid detail — the start_date rule)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 8. a 429 (transient) → NO seen row (same rule; the poll's 429 arm creates
//    the pending row with the real summary start_date), no post, 200.
// ---------------------------------------------------------------------------

func TestWebhookTransientNoRow(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(_ int64) (Activity, error) { return Activity{}, ErrRateLimited{RetryAfter: "60"} },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200 (a transient outcome is a 200 — the poll backstop covers it)", code)
	}
	if n := seenCount(t, pool, aid); n != 0 {
		t.Errorf("seen rows = %d, want zero (a transient outcome has no valid detail — the start_date rule)", n)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero", n)
	}
	if got := cursorAt(t, pool, aid); !got.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", got, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 9. isProcessing → a pending row, no post, and start_date = the detail's
//    StartDate (asserted NOT the zero time — the start_date rule pin).
// ---------------------------------------------------------------------------

func TestWebhookProcessingHolds(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return processingAct(id, startA), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)

	if code := postWebhook(t, s, `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("create event = %d, want 200", code)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPending || retries != 0 {
		t.Fatalf("row = %q retries %d, want pending/0 (still processing)", status, retries)
	}
	got := seenStartDate(t, pool, aid, 555)
	if got.IsZero() {
		t.Fatal("row start_date is the ZERO time — the start_date rule pin: a pending row's start_date is the detail's StartDate, never the zero time")
	}
	if !got.Equal(startA) {
		t.Errorf("row start_date = %v, want the detail's StartDate %v", got, startA)
	}
	if n := len(caps.posts); n != 0 {
		t.Errorf("captured %d posts, want zero (pending)", n)
	}
	if gotC := cursorAt(t, pool, aid); !gotC.Equal(WINDOW) {
		t.Errorf("cursor = %v, want unchanged (%v)", gotC, WINDOW)
	}
}

// ---------------------------------------------------------------------------
// 10. the poll path posts an activity (the existing poll test machinery),
//     then a webhook update event for the same id → ABSORBED (no re-post —
//     cross-source dedupe via the terminal row).
// ---------------------------------------------------------------------------

func TestWebhookNoRepostAcrossSources(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		listFn: func(_ time.Time) ([]Summary, error) {
			return []Summary{{ID: 555, SportType: "Run", StartDate: startA}}, nil
		},
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)

	// The poll path posts the activity (the existing poll test machinery).
	if err := s.iteration(context.Background()); err != nil {
		t.Fatalf("poll pass: %v", err)
	}
	if n := len(caps.posts); n != 1 {
		t.Fatalf("poll pass: %d posts, want exactly 1", n)
	}
	if status, _ := seenRow(t, pool, aid, 555); status != statusPosted {
		t.Fatalf("row = %q, want posted (the poll posted it)", status)
	}
	// Then the webhook update event for the same id → ABSORBED (cross-source
	// dedupe via the terminal row — no re-post, no re-evaluation).
	done := installRealWorker(s)
	if code := postWebhook(t, s, `{"aspect_type":"update","object_type":"activity","object_id":555,"owner_id":777}`, done); code != http.StatusOK {
		t.Fatalf("update event = %d, want 200", code)
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("total posts = %d, want still 1 (the webhook's terminal row absorbs — no re-post)", n)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPosted || retries != 0 {
		t.Errorf("row = %q retries %d, want posted/0 (the terminal row is never overwritten)", status, retries)
	}
	if n := stub.detailCalls; n != 2 {
		t.Errorf("detail calls = %d, want 2 (the poll fetch + the webhook fetch — the ABSORPTION is at the write)", n)
	}
}

// ---------------------------------------------------------------------------
// 11. two goroutines POST the same create event SIMULTANEOUSLY (both workers
//     fetch the detail, both attempt the INSERT) → exactly ONE post (the
//     rows-affected gate — the concurrency-safe dedupe under READ COMMITTED:
//     the second worker's INSERT is a DO NOTHING → 0 rows → no double post).
// ---------------------------------------------------------------------------

func TestWebhookConcurrentDuplicates(t *testing.T) {
	pool := setupStravaTestDB(t)
	now := time.Now().UTC()
	WINDOW := muTime(now.Add(-2 * time.Hour))
	startA := muTime(now.Add(-90 * time.Minute))

	aid := seedAthlete2(t, pool, "Web", 777, 42, WINDOW, 6*time.Hour)
	stub := &stubStrava{
		detailFn: func(id int64) (Activity, error) { return readyAct(id, startA, 42300), nil },
	}
	s, caps := newTestStrava(t, pool, stub, 0)
	done := installRealWorker(s)
	body := `{"aspect_type":"create","object_type":"activity","object_id":555,"owner_id":777}`

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doWebhook(t, s, body)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		waitForJob(t, done) // both jobs completed (one posts, one is deduped)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("event %d = %d, want 200", i, c)
		}
	}
	if n := len(caps.posts); n != 1 {
		t.Errorf("captured %d posts, want exactly 1 (the rows-affected gate — the concurrency-safe dedupe under READ COMMITTED: the second worker's INSERT is a DO NOTHING → 0 rows → no double post)", n)
	}
	if n := seenCount(t, pool, aid); n != 1 {
		t.Errorf("seen rows = %d, want exactly 1", n)
	}
	if status, retries := seenRow(t, pool, aid, 555); status != statusPosted || retries != 0 {
		t.Errorf("row = %q retries %d, want posted/0", status, retries)
	}
	if n := stub.detailCalls; n != 2 {
		t.Errorf("detail calls = %d, want 2 (both workers fetch; the dedupe is at the write)", n)
	}
}
