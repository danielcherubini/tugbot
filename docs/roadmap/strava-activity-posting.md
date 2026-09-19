---
status: committed
done-when: a finished Run/Cycling-family Strava activity of any enabled athlete lands in its per-athlete thread (or the shared fallback thread) within ~15 minutes as "{label} finished {noun}" + activity link, with no double posts across restarts, and the features flag 'strava' toggles it live
---

# Strava Activity Posting — Plan

**Goal:** A single 15-minute background poll loop (no public endpoint) detects finished run/cycling activities for a small set of authorized Strava athletes and posts a two-line message (short line + `https://www.strava.com/activities/<id>`) to each athlete's Discord thread, with crash-safe no-double-post guarantees.

**Architecture:** A new `internal/handlers/strava` package following the house handler pattern (`New(*app.App)`, `FeatureKey`, feature-table gate). `RunPoll` is one `eg.Go` in `cmd/tugbot/main.go` using the exact `gulag/loops.go` ticker shape. Each iteration, per athlete: token refresh (persisting the ROTATING refresh token), `GET /athlete/activities?after=<cursor>` (cursor holds at the oldest un-settled activity's start — "pending-hold" semantics), per-activity disposition (`pending`/`posted`/`skipped` rows in `strava_seen_activities`), ONE DB transaction (seen rows + retries + cursor advance), then posts via `ChannelMessageSend`. Schema via one new migration; the `strava` row of the `features` table gates the loop.

**Tech stack:** Go (repo's go.mod), `bwmarrin/discordgo`, `jackc/pgx/v5/pgxpool`, `golang.org/x/sync/errgroup`, Postgres (compose PG via `make db-up`).

**Ground rules for every task (the repo's gate, from AGENTS.md):**

```bash
go build ./...
go vet ./...
gofmt -l .            # must print NOTHING
make lint             # golangci-lint (CI-pinned) + go vet
go test ./...         # without PG: DB-touching tests self-skip cleanly
```

DB-touching gate (where a task says "DB gate"):

```bash
make db-up
TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...
```

`-p 1` + `-count=1` are mandatory for DB packages (shared compose DB; cache would mask). Selftest (task 6 only; needs `make db-up`):

```bash
go run ./cmd/tugbot --selftest
# must log: "selftest: Discord session and all fourteen handlers and the MCP server constructed"
```

House warnings (AGENTS.md): DB tests can leave the shared compose DB with dropped/recreated tables and an unsettled migration tracker — remedy before a later `make migrate`: `docker compose down -v`.

---

### Task 1: DB migration (000006)

**Context:**
The first DB surface change; every later task depends on these tables existing. The house migration runner (`internal/dbmigrate`) globs `migrations/*.up.sql`, sorts lexicographically, and applies each in its own transaction, recording `version = basename` — a new table is exactly one new file, nothing else changes. Applied via `make migrate` against the compose PG. **Deliberate, declared deviation** (do not "fix" it): both tables use `timestamptz` while the rest of the DB is naive-timestamp — the strava values come straight from the Strava epoch-seconds API and are compared against `now()`, so tz-aware columns avoid local-time ambiguity.

**Files:**
- Create: `migrations/000006_strava.up.sql`

**What to implement:**
Exactly this content (the header comment is part of it — it records the declared deviation for future readers):

```sql
-- 000006_strava — schema for the strava feature (Go-origin).
-- DECLARED DEVIATION: the first timestamptz columns in a naive-timestamp DB
-- (only dbmigrate's own schema_migrations.applied_at predates this). Rationale:
-- these values come straight from the Strava epoch-seconds API and are
-- compared against now(), so tz-aware columns avoid local-time ambiguity.
-- All other tables are untouched.
CREATE TABLE public.strava_athletes (
    id               serial PRIMARY KEY,
    label            text NOT NULL,            -- display name in the post: "Matt"
    strava_athlete_id bigint NOT NULL UNIQUE,  -- Strava's id for the user
    access_token     text NOT NULL,
    refresh_token    text NOT NULL,            -- ROTATES on every refresh — must be re-persisted
    token_expires_at timestamptz NOT NULL,
    last_polled_at   timestamptz,             -- the `after` cursor; NULL = first poll = 24h lookback
    target_thread_id bigint,                 -- nullable: per-athlete thread, else shared fallback
    needs_reauth     boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.strava_seen_activities (
    id                 serial PRIMARY KEY,
    strava_athletes_id int NOT NULL REFERENCES public.strava_athletes(id) ON DELETE CASCADE,
    strava_activity_id bigint NOT NULL,
    start_date         timestamptz NOT NULL,  -- the activity's start time; watermark math from here
    status             text NOT NULL CHECK (status IN ('pending', 'posted', 'skipped')),
    retries            int NOT NULL DEFAULT 0,  -- full cycles the 'pending' row has been re-fetched (drop at 5; 0 on its discovery pass)
    dispositioned_at   timestamptz NOT NULL DEFAULT now(),  -- when the row was first created (any status)
    UNIQUE (strava_athletes_id, strava_activity_id)  -- crash-safe dedupe key
);

INSERT INTO public.features (name, enabled) VALUES ('strava', false) ON CONFLICT (name) DO NOTHING;
```

Do NOT create a `.down.sql` — the repo has none (dbmigrate is up-only by file convention). Do NOT edit `internal/dbmigrate` or `internal/db` (no sqlc regeneration for this feature — the handler uses raw SQL, house convention).

**Steps:**
- [ ] Create `migrations/000006_strava.up.sql` with exactly the content above.
- [ ] Run `make db-up` (compose PG; credentials postgres:postgres, database `tugbot`).
- [ ] Run `DATABASE_URL=postgres://postgres:postgres@localhost:5432/tugbot make migrate` — did it complete without error? (`cmd/migrate` reads the env var directly — no godotenv — the var is MANDATORY; `make db-up` alone is not enough.)
- [ ] Verify: `docker compose exec postgres psql -U postgres -d tugbot -c "\dt strava_*"` lists `strava_athletes` and `strava_seen_activities`.
- [ ] Verify: `docker compose exec postgres psql -U postgres -d tugbot -c "SELECT enabled FROM features WHERE name = 'strava'"` prints `f`.
- [ ] Run `DATABASE_URL=postgres://postgres:postgres@localhost:5432/tugbot make migrate` a second time — did it complete as a no-op (the applied version is skipped; no duplicate errors)?
- [ ] Run the gate block (nothing Go changed, but the migration must pass the dbmigrate tests: `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/dbmigrate/`).
- [ ] Commit with message: "db: add strava_athletes + strava_seen_activities (migration 000006)"

**Acceptance criteria:**
- [ ] `make migrate` applies 000006 and is idempotent on re-run.
- [ ] Both tables exist in the compose PG with the declared `timestamptz` columns; `features` has a `('strava', false)` row.
- [ ] `dbmigrate` tests green under the DB gate.

---

### Task 2: Config vars

**Context:**
The handler needs four optional `.env` vars. "Optional" is load-bearing: `--selftest` (and production startup) must succeed with NONE of them set (the loop then simply no-ops). Only one var fails loud: a SET-but-non-numeric `STRAVA_POLL_MINUTES` is a `LoadError` — the `parseMCPPort` precedent (`config.go:97–103` call site + `config.go:147–160` helper), by design a malformed value fails the WHOLE config load (documented blast radius; the strava feature being disabled does not soften this). Below-15 values are floored to 15 with a warning instead of failing (a human typo like `5` is a usable intent: "faster" = the floor).

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**What to implement:**

Add to the `Config` struct (after the `DerpiesUserIDs` block, mirroring the comment style):

```go
	// StravaClientID / StravaClientSecret are the registered Strava app's
	// credentials (STRAVA_CLIENT_ID / STRAVA_CLIENT_SECRET). Optional: the
	// strava handler no-ops when absent (feature-flag gated too).
	StravaClientID string
	StravaClientSecret string

	// StravaSharedThreadID (STRAVA_SHARED_THREAD_ID) is the fallback post
	// target: 0 = unset (per-athlete threads only). Malformed → 0 (the
	// single-ID parseID convention, silently).
	StravaSharedThreadID int64

	// StravaPollMinutes (STRAVA_POLL_MINUTES): the poll cadence in minutes.
	// Default 15. Below 15 → floored to 15 (slog.Warn). A set-but-non-numeric
	// value fails LOUD (LoadError — the parseMCPPort precedent; a malformed
	// value fails the whole config load, by design).
	StravaPollMinutes int
```

Add a helper + a call site in `LoadConfig` (mirror the `parseMCPPort` helper at `config.go:147–160` and its call site at `config.go:97–103`, which does `return nil, perr` immediately — do NOT append to `errs` in the parse region: the `len(errs) > 0` early return at `config.go:90–92` (the `errs` block spans `:80–92`) happens before the call site, so an append there would be dead code and the malformed value would sail through):

```go
// parseStravaPoll — like parseMCPPort: unset → 15; below 15 → 15 (slog.Warn);
// set-but-non-numeric → LoadError (fail loud: a malformed value fails the WHOLE
// config load, by design — documented blast radius).
func parseStravaPoll(v string) (int, error) {
	if v == "" {
		return 15, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, &LoadError{problems: []string{"STRAVA_POLL_MINUTES is not a valid minute count: " + v}}
	}
	if n < 15 {
		slog.Warn("STRAVA_POLL_MINUTES below 15 — using 15", "module", "config")
		return 15, nil
	}
	return n, nil
}
```
called from `LoadConfig` exactly the way the `parseMCPPort` result is consumed:

```go
stravaPoll, perr := parseStravaPoll(os.Getenv("STRAVA_POLL_MINUTES"))
if perr != nil {
	return nil, perr
}
```

(plus `StravaClientID: os.Getenv("STRAVA_CLIENT_ID")`, `StravaClientSecret: os.Getenv("STRAVA_CLIENT_SECRET")`, `StravaSharedThreadID: parseID(os.Getenv("STRAVA_SHARED_THREAD_ID"))`, `StravaPollMinutes: stravaPoll` in the returned `Config` literal — `parseID` already exists and returns `int64`, 0 on absent/malformed; add `log/slog` to imports if not present.)

Tests — add to `internal/config/config_test.go` using the existing `t.Setenv` pattern (the file's helper at ~line 14 sets the required vars `DISCORD_TOKEN`/`APPLICATION_ID`/`DATABASE_URL`; call it in each new test):

1. `TestLoadConfigStravaDefaults` — required vars set, all four strava vars `""` → `cfg.StravaClientID == ""`, `cfg.StravaClientSecret == ""`, `cfg.StravaSharedThreadID == 0`, `cfg.StravaPollMinutes == 15`.
2. `TestLoadConfigStravaSet` — `STRAVA_CLIENT_ID=abc`, `STRAVA_CLIENT_SECRET=xyz`, `STRAVA_SHARED_THREAD_ID=100`, `STRAVA_POLL_MINUTES=30` → all parsed.
3. `TestLoadConfigMalformedStravaPoll` — `STRAVA_POLL_MINUTES=abc` → `LoadConfig` returns an error (assert `errors.As` `*LoadError` and its `Error()` contains `STRAVA_POLL_MINUTES`).
4. `TestLoadConfigStravaPollBelowFloor` — `STRAVA_POLL_MINUTES=5` → `StravaPollMinutes == 15`.

**Steps:**
- [ ] Add the four STRAVA keys (`STRAVA_CLIENT_ID`, `STRAVA_CLIENT_SECRET`, `STRAVA_SHARED_THREAD_ID`, `STRAVA_POLL_MINUTES`) to the `validEnv()` map in `config_test.go` so ambient shell/`.env` values cannot taint the tests.
- [ ] Write the four tests in `internal/config/config_test.go`.
- [ ] Run `go test ./internal/config/ -count=1` — did it fail (the fields don't exist yet)?
- [ ] Implement the struct fields + `LoadConfig` block + the `Config` literal additions.
- [ ] Run `go test ./internal/config/ -count=1` — did all tests pass?
- [ ] Run the gate block.
- [ ] Commit with message: "config: optional strava vars (client credentials, shared thread, poll minutes floor 15)"

**Acceptance criteria:**
- [ ] All four new config tests pass; `go test ./internal/config/ -count=1` fully green.
- [ ] A config with no strava vars loads cleanly (`--selftest`-safe: no required-status change).
- [ ] Gate block green.

---

### Task 3: Strava API client

**Context:**
The minimal HTTP surface the loop needs, behind an interface so the handler's tests stub it (the repo's `fu`/seam convention). Three error classes drive the loop's mechanics and MUST be typed: **401** (either an expired-access/invalid-grant or a user deauthorization → `needs_reauth`), **429** (cursor holds; the 15-min ticker is the backoff), and **transient** (5xx/network → the id stays `pending` and is retried). Base URL is swappable for tests (`httptest`).

**Files:**
- Create: `internal/handlers/strava/client.go`
- Test: `internal/handlers/strava/client_test.go`

**What to implement:**

```go
// client.go — the Strava v3 API surface for the strava feature.
package strava

// Summary is what the list endpoint returns per activity (a subset
// model — enough to gate the detail fetch on sport_type).
type Summary struct {
	ID        int64
	SportType string    // "" when absent (unparsed device upload → pending, per spec)
	StartDate time.Time
}

// Activity is the full detail model.
// InProgressSet mirrors the SOURCE JSON: whether the "in_progress" key was
// present at all (the documented fallback: field absent → ready unless
// resource_state == -1, the official API reference's processing indicator).
type Activity struct {
	ID            int64
	SportType     string
	Title         string // "" when unset
	Distance      float64 // meters
	StartDate     time.Time
	InProgress    bool
	InProgressSet bool
	ResourceState int // as returned; -1 == 'processing'
}

// Error classes (each with a distinct Error() text).
type ErrUnauthorized struct{ Why string }        // 401, or refresh 400 invalid_grant
type ErrRateLimited struct{ RetryAfter string }  // 429 + X-RateLimit headers
type ErrTransient struct{ Cause error }          // 5xx / network / unexpected 4xx

// StravaAPI is the seam the handler resolves through (tests stub it).
type StravaAPI interface {
	// ListActivities pages internally: loop while a full 100-row page returns.
	ListActivities(ctx context.Context, token string, after time.Time) ([]Summary, error)
	GetActivity(ctx context.Context, token string, id int64) (Activity, error)
	// RefreshToken performs POST /oauth/token (grant_type=refresh_token form:
	// client_id, client_secret, refresh_token). The ROTATED refresh_token is
	// part of the return — the caller MUST persist it (spec).
	RefreshToken(ctx context.Context, clientID, clientSecret, refreshToken string) (accessToken string, newRefreshToken string, expiresAt time.Time, err error)
}

type stravaClient struct{ base string }

// NewStravaAPI is the production constructor (base = https://www.strava.com);
// tests construct stravaClient directly with an httptest URL.
func NewStravaAPI() StravaAPI
```

Mapping rules (exact): **any 401** (list, detail, or refresh), or a refresh-route 400 whose body contains `invalid_grant` → `ErrUnauthorized`; any 429 → `ErrRateLimited` (populate `RetryAfter` from `X-RateLimit-Retry-After` / the `X-RateLimit-*` headers when present, else `"`); other 4xx, any 5xx, dial/TLS/timeout errors → `ErrTransient{Cause}`. JWT parsing is NOT needed — the client never decodes tokens (the server returns `expires_at` epoch seconds; the refresh form is the only token work).

**Steps:**
- [ ] Write `client_test.go` first (httptest.Server per test; the file's struct needs only the unexported `stravaClient` + `NewStravaAPI`-shaped constructor injection — construct `stravaClient{base: ts.URL}` directly, the test lives in package `strava` so unexported access is fine):
  1. `TestListActivitiesPagination` — handler returns 100 summaries then 6; client returns all 106 in one call; a 429 response (with `X-RateLimit-Retry-After: 123`) surfaces as `ErrRateLimited{RetryAfter: "123"}`; a 401 surfaces as `ErrUnauthorized`; a 503 surfaces as `ErrTransient`; assert the merged 106 `Summary` values carry parsed `ID`/`SportType`/`StartDate` (the list `StartDate` drives the cursor math).
  2. `TestGetActivitySignals` — response with `"in_progress": true` → `InProgress true, InProgressSet true`; response with the key absent and `"resource_state": -1` → `InProgressSet false, ResourceState -1`; response with NEITHER signal → `InProgressSet false, ResourceState != -1` (ready class); `sport_type`/`title`/`distance`/`start_date` parsed; 400 body `{"error":"invalid_grant"}` on the refresh route → `ErrUnauthorized`.
  3. `TestRefreshTokenRotation` — success returns both tokens + `expires_at` (epoch seconds → `time.Time`); verify the request carried `grant_type=refresh_token` + the three form values (assert in the handler).
- [ ] Run `go test ./internal/handlers/strava/ -count=1` — did it fail (no `client.go`)?
- [ ] Implement `client.go`.
- [ ] Run `go test ./internal/handlers/strava/ -count=1` — did all tests pass?
- [ ] Run the gate block.
- [ ] Commit with message: "strava: API client (list/detail/refresh; typed 401/429/transient errors)"

**Acceptance criteria:**
- [ ] All three client test groups pass; error mapping is exact per the rules above.
- [ ] `NewStravaAPI()` performs no network I/O (selftest-safe; nothing in this task opens connections at construction).
- [ ] Gate block green.

---

### Task 4: Handler core + pure-logic units

**Context:**
The loop itself plus its pure decision functions, unit-tested without PG (DB mechanics land in task 5 through a stubbed `StravaAPI` + the real test PG). Everything in spec §3/§4 mechanics is here; this task's discipline: `New` is network-free (the selftest constructs every handler offline), and every decision function is a pure exported-or-unexported helper so a context-free regression test can pin it.

**Files:**
- Create: `internal/handlers/strava/strava.go`
- Test: `internal/handlers/strava/strava_test.go`

**What to implement:**

```go
const FeatureKey = "strava"

// The fixed, non-configured run/cycling family (spec §1 — 9 values; unit
// test pins EVERY member + known exclusions).
var familySet = map[string]struct{}{
	"Run": {}, "TrailRun": {}, "VirtualRun": {}, "Ride": {}, "VirtualRide": {},
	"GravelRide": {}, "MountainBikeRide": {}, "EBikeRide": {}, "EMountainBikeRide": {},
}

type Strava struct {
	app   *app.App
	api   StravaAPI      // seam: production = NewStravaAPI(); nil until first use
	poll  time.Duration  // poll interval (from Config, task 2; default 15m)
	// sendFn seam: func(threadID string, msg string) error → production posts
	// via h.app.D.ChannelMessageSend (the single-thread discipline: threads are
	// plain channels in discordgo).
	sendFn func(threadID string, msg string) error
}

func New(app *app.App) *Strava // NO network I/O here (selftest discipline).
                             // discordgo's ChannelMessageSend is
                             // (channelID string, content string, ...RequestOption) (*Message, error)
                             // — wire the seam via a CLOSURE (a direct method-value
                             // does not compile to the seam's signature):
                             // s.sendFn = func(threadID, msg string) error {
                             //     _, err := s.app.D.ChannelMessageSend(threadID, msg)
                             //     return err
                             // }
func (s *Strava) RunPoll(ctx context.Context) error // EXACT gulag/loops.go:53–86 shape:
	// ticker := time.NewTicker(s.poll); defer ticker.Stop(); for { select {
	// case <-ctx.Done(): return ctx.Err()
	// case <-ticker.C: if err := s.iteration(ctx); err != nil { slog.Error(...); } } }

func (s *Strava) iteration(ctx context.Context) error
// spec §3 sequence, verbatim:
// 1. config preflight: features.IsEnabled(ctx, s.app.Pool, FeatureKey) false →
//    return nil; cfg.StravaClientID/Secret "" → return nil (log-once optional);
//    load strava_athletes rows (raw SQL, house style); skip needs_reauth=true
//    (call nothing for them — their cursor is implicitly preserved); none → return nil.
// 2. per athlete, in order (an error here: log + continue to the next athlete —
//    EXCEPT the 401 class below):
//    a. Token: if token_expires_at < now+1h → api.RefreshToken; on success
//       persist (access_token, refresh_token=ROTATED, token_expires_at) in ONE
//       transaction. ErrUnauthorized → UPDATE needs_reauth=true, log (re-auth
//       pointer), continue (pass ABORTS for this athlete — spec: 401 aborts the
//       remaining pass, the per-athlete transaction is discarded, cursor preserved; the `needs_reauth = true` UPDATE commits immediately as its OWN statement — only the seen-rows/cursor transaction is discarded (test 7's dual assertion depends on this)).
//    b. window_start = last_polled_at ?? (now − 24h); api.ListActivities(after=window_start).
//       ErrRateLimited → log (with RetryAfter), NO cursor change for this athlete,
//       continue. ErrUnauthorized → abort this athlete's pass (as a.).
//    c. per listed ID (a seen row of any status → no-op except `pending` via d):
//       - summary SportType == "" (unparsed) → INSERT seen 'pending' (start_date) —
//         NOT 'skipped'.
//       - summary family miss → INSERT seen 'skipped' — NO detail fetch.
//       - family hit → api.GetActivity:
//           - isProcessing(a) → INSERT seen 'pending'.
//           - else → INSERT seen 'posted' BEFORE posting (posted invariant),
//             then post (below).
//       - ErrTransient/ErrRateLimited on the detail → INSERT seen 'pending'
//         (the existing hold rule keeps the id in the window; d re-fetches
//         next cycles; the 5-retry drop covers a stuck fetch).
//    d. Pending retry → for each CARRIED pending row — SNAPSHOT the pending-row set at pass start (before step c's inserts); a row created by step c this pass is `pending` with `retries = 0` too, so the snapshot is the only discriminator;
//       they first re-fetch next cycle, so `retries` counts full cycles from 0):
//       re-fetch the detail — UNIFORMLY, including rows created via the unparsed
//       summary path (SportType == ""): the detail yields the activity's final
//       sport_type + processing state. ready & family → UPDATE 'posted' + post;
//       detail now outside the family → UPDATE 'skipped'. still processing, or the
//       fetch failed again → increment `retries`; if the NEW value >= 5, set
//       status 'skipped' in the SAME UPDATE (drop — that workout never gets a
//       post); otherwise stays 'pending'.
//    e. Cursor (nextCursor below): pending remain → hold at min(pending.start_date);
//       else (nothing pending, all-time dispositioned) → max(dispositioned) per the `nextCursor` doc;
//       else unchanged.
// 3. Atomicity: ONE transaction per athlete-per-pass applies the new seen rows +
//    retries increments + the new last_polled_at. THEN the posts.
// 4. 429s elsewhere: log (headers) — the 15-min ticker is the backoff (no sleeps).
// Post (only when a row was dispositioned 'posted' this pass):
//    The target is resolved at DISPOSITION time — inside the transaction, before
//    the INSERT (never post-commit): target = athlete.target_thread_id loaded via
//    SQL COALESCE so a NULL column reads as 0 (no pgx NULL scan); the fallback is then
//    cfg.StravaSharedThreadID; RESOLUTION APPLIES TO BOTH the step-c INSERT and the step-d UPDATE-to-`'posted'`; still 0 → the family activity is dispositioned
//    'skipped' instead (one counted line in the per-cycle log — spec failure
//    table). Build the message via buildPost and call sendFn (strconv.FormatInt
//    the int64 thread id to the seam's string); a send error: log, no retry (the
//    activity is 'posted' — not re-posted later; a missed post is a log artifact).

// ---- pure decision functions (the unit-test surface) ----
func family(sportType string) bool                 // familySet membership
func isProcessing(a Activity) bool                // a.InProgressSet && a.InProgress
                                               // || !a.InProgressSet && a.ResourceState == -1
func nextCursor(pending, dispositioned []time.Time) (next time.Time, advanced bool)
 // The CALLER loads `dispositioned` = start dates of EVERY seen row with
 // status != 'pending' — ALL-TIME, not this-pass-only (step-d resolutions
 // count once their status is updated to posted/skipped).
 // pending non-empty → (min(pending), false)          [hold the window open]
 // else dispositioned non-empty → (max(dispositioned), true)
 // else → (zero time, false)                          [unchanged]
func formatNoun(a Activity) string
 // noun = <distance> <SportType>; distance (meters → km): >= 100 km → round
 // HALF UP to an integer via int64(m/1000 + 0.5) ("142 km"; 142500 m → "143 km");
 // < 100 km → one decimal via `%.1f` ("42.3 km").
func buildPost(label, noun, activityID string) string
 // two lines: label + " finished " + noun + "\n" + "https://www.strava.com/activities/" + id
 // (title branch composed by the caller: noun = `"%s" (%s)` title, distanceNoun)
```

(`poll` is set from `app.Cfg.StravaPollMinutes` at first use/iteration — not in `New`, keeping `New` trivial. Task 4's tests need only `FeatureKey` + the five pure helpers — NO constructor seams here; the sole test constructor, `newTestStrava`, lands with task 5's `strava_integration_test.go`).

`strava_test.go` — house-unit pattern (mirror `gokupoll_test.go`'s `TestFeatureKey` pin + `instagram_test.go` style):

- `TestFeatureKey` — `FeatureKey == "strava"`.
- `TestFamilyPin` — table test: all 9 family values → true; `Swim`, `Walking`, `Badminton`, `""` → false.
- `TestIsProcessing` — table: `InProgressSet true, InProgress true` → true; `InProgressSet true, InProgress false` → false; `InProgressSet false, ResourceState -1` → true; `InProgressSet false, ResourceState 2` → false; `InProgressSet true, InProgress false, ResourceState -1` → false (a present false field WINS over the -1 fallback).
- `TestNextCursor` — hold-priority (pending + dispositioned → min(pending), false); advance (dispositioned only → max, true); empty (→ zero, false).
- `TestFormatNoun` — 42300 m → `"42.3 km Run"`; 142000 m → `"142 km MountainBikeRide"`; 142500 m → `"143 km ..."` (the ≥ 100 km branch, round half up via `int64(m/1000+0.5)`); 99800 m → `"99.8 km Ride"` (the < 100 km branch, `%.1f`); boundary: exactly 100000 m → `"100 km Ride"` (the branch flips at exactly 100.0 — no decimals).
- `TestBuildPost` — `Matt` + `42.3 km Run` + id `123` → exactly `Matt finished 42.3 km Run\nhttps://www.strava.com/activities/123`; title branch: `Matt finished "Tuesday tempo" (42.3 km Run)\n...`.

**Steps:**
- [ ] Write `strava_test.go` (all six test groups above).
- [ ] Run `go test ./internal/handlers/strava/ -run "TestFeatureKey|TestFamilyPin|TestIsProcessing|TestNextCursor|TestFormatNoun|TestBuildPost" -count=1` — did it fail (no `strava.go`)?
- [ ] Implement `strava.go` (struct, `New`, `RunPoll`, `iteration`, the five pure helpers + `FeatureKey`).
- [ ] Run the same test command — did all tests pass?
- [ ] Run the gate block.
- [ ] Commit with message: "strava: handler core (FeatureKey, RunPoll, iteration, cursor/token/dedupe mechanics)"

**Acceptance criteria:**
- [ ] All six unit test groups pass; `go test ./internal/handlers/strava/ -count=1` fully green (client tests from task 3 still included).
- [ ] `New(app)` compiles into the selftest surface with zero network I/O.
- [ ] The family set is exactly the 9 spec values — not configurable anywhere.
- [ ] Gate block green.

---

### Task 5: DB integration tests

**Context:**
The DB-touching mechanics (cursor + seen atomicity, reauth pause/resume, 429 hold, pending lifecycle, first-enable lookback) run against a real Postgres with a stubbed `StravaAPI` and a capturing `sendFn` — the repo's exact skip pattern (`gulag_test.go:23–60`: `skipIfShort`, `TUGBOT_TEST_DATABASE_URL` defaulting to `postgres://tugbot:tugbot@127.0.0.1:5432/tugbot_test`, pool with 10s timeout, `t.Skipf` when unreachable). Because this test resets tables IN the shared compose DB, the DB gate runs `-p 1 -count=1` (AGENTS.md state-residue warning; `docker compose down -v` is the remedy before a later `make migrate`).

**Files:**
- Test: `internal/handlers/strava/strava_integration_test.go`

**What to implement:**

`setupStravaTestDB(t)` — mirror `setupGulagTestDB` line for line, but reset the strava surface:

```go
func setupStravaTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	skipIfShort(t)
	// pool with 10s timeout against testDBURL(); t.Skipf on error (same message style
	// as gulag: "cannot create pool: %v (is the compose PG running?)")
	// ONE script:
	//   DROP TABLE IF EXISTS strava_seen_activities;
	//   DROP TABLE IF EXISTS strava_athletes;
	//   <exact CREATE TABLE DDL from migrations/000006, both tables>
	//   CREATE TABLE IF NOT EXISTS features (id serial PRIMARY KEY,
	//       name character varying(255) UNIQUE NOT NULL,
	//       enabled boolean DEFAULT false NOT NULL);  -- the derpies precedent
	//   DELETE FROM features;                        -- the shared compose features table
	//   INSERT INTO features (name, enabled) VALUES ('strava', true);
	// t.Cleanup(pool.Close) INSIDE setupStravaTestDB (gulag_test.go:99 — the
	// house pattern); callers do NOT close.
}
```

A `newTestStrava(t, pool, stubAPI, sendCapture)` helper wiring the seams (`api`, `sendFn`, the pool, a `Config` with client id/secret set, `StravaSharedThreadID` = a chosen id, `StravaPollMinutes` = 15) via the unexported test constructor. The stub `StravaAPI` is a small struct with scriptable fields: `listFn func(after time.Time) ([]Summary, error)`, `detailFn func(id int64) (Activity, error)`, `refreshFn func() (string, string, time.Time, error)` + call counters.

Tests (each: `setupStravaTestDB`, seed the athlete row with raw SQL — `INSERT INTO strava_athletes (label, strava_athlete_id, access_token, refresh_token, token_expires_at, last_polled_at, target_thread_id) VALUES (...)` — call `s.iteration(ctx)` directly (or `RunPoll` with a `time.Millisecond*50` `poll` seam for the one loop test), assert via raw SQL + the captured posts):

1. `TestStravaPassPostsNewFamily` — athlete `target_thread_id=9001`, token valid (expiry now+6h), `last_polled_at = now-2h`; stub: list → [A (Run, 42300m, start in window), B (Swim, start in window)]; detail A ready. One iteration → exactly one captured post on `9001` = `Label finished 42.3 km Run\nhttps://www.strava.com/activities/A`; SQL: A row `posted`, B row `skipped`; `last_polled_at` = max(A, B start) (B's skip counts as dispositioned; no pending → advance).
2. `TestStravaNoRepost` — pre-seed A `posted`; same stub → iteration → ZERO posts; A row unchanged.
3. `TestStravaPendingHoldAndRetry` — pass 1: A detail processing → A row `pending`, `retries 0`, cursor = A.start (hold), no post. Mutate the stub; pass 2: A ready → A row `posted`, one post, `retries` still 0 (per the carried-row rule `retries` counts full re-fetch cycles from 0 — A resolved on its first re-fetch, so it stays 0).
4. `TestStravaPendingDropsAt5` — always processing: 6 iterations → A row `skipped`, `retries 5`, ZERO posts.
5. `TestStravaDetail429CreatePending` — seed with `A.start < B.start`. Pass 1: A detail → `ErrRateLimited`; B (family, ready) posted in the same pass. Assert: A row `pending`, B row `posted`, ONE post (only B), `last_polled_at` = A.start (HOLD — never advanced past the failed id). Pass 2: A detail ready → A posted, second post, `last_polled_at` = B.start (a genuine advance — the all-time seen semantics; A is settled, nothing pending).
6. `TestStravaNeedsReauthPauseResume` — stub refresh → `ErrUnauthorized`; pass 1 → athlete row `needs_reauth = true`, cursor frozen, API call counters show no list calls for that athlete in passes 2–3. Re-auth: UPDATE the row (flag false, fresh token, stub refresh now succeeds) → pass 4 → A listed, posted exactly once, cursor resumes from the frozen value (the stub's `listFn` called with `after =` the frozen cursor; assert).
7. `TestStrava401ListAbortsPreservesCursor` — pre-seed A as `pending` (retries 0). Pass with list → `ErrUnauthorized` → row `needs_reauth = true`; the per-athlete transaction is discarded: assert cursor unchanged, A row's `retries` unchanged (0), no post.
8. `TestStravaNoTargetSkips` — athlete `target_thread_id NULL` + `StravaSharedThreadID = 0`: family activity → row `skipped`, ZERO captured posts (the counted one-line log: assert the row + no post; worry about log text separately).
9. `TestStravaFirstEnable24hLookback` — `last_polled_at IS NULL`: `listFn` asserts the `after ≈ now-24h` window and returns A (start now-2h) → posted.
10. `TestRunPollLoopExits` — `poll` seam `50ms`, `run` iterates once, `ctx` canceled → `RunPoll` returns `context.Canceled` (the loop's ctx discipline, the `gulag` shape).
11. `TestStravaList429Hold` — list → `ErrRateLimited` → assert: cursor unchanged, zero `seen` rows, zero posts; next pass with a normal list (A ready) → A posted (the hold released cleanly, nothing double-posted).

**Steps:**
- [ ] Write `strava_integration_test.go` (setup + helper + the eleven tests).
- [ ] Run `go test ./internal/handlers/strava/ -run "TestStrava|TestRunPollLoopExits" -count=1` WITHOUT `TUGBOT_TEST_DATABASE_URL` — did they self-skip cleanly (PG unreachable / the default URL)?
- [ ] Run `make db-up`, then `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/` — did ALL eleven execute and pass (no skips under the override — a skip there is a missing override, not a legitimate result)?
- [ ] Run the gate block + the full DB gate.
- [ ] Commit with message: "strava: DB integration tests (post/seen/atomicity/reauth/429-pending/lookback/loop-exit)"

**Acceptance criteria:**
- [ ] No-env var: the eleven tests skip cleanly (no failures).
- [ ] With the override + `-p 1 -count=1`: all eleven execute and pass.
- [ ] The full DB gate (`./...` with the override) stays green.

---

### Task 6: Wire into main + selftest + docs

**Context:**
The last assembly seam: the `strava` handler joins the `handlers` struct + `newHandlers` + the errgroup (the exact pattern of the two `gulag` `eg.Go` blocks), "thirteen" → "fourteen" is 3 strings in `main.go` + 1 in `AGENTS.md` (there is NO numeric assertion — the selftest only constructs; the reword is all), and the house doc surface (`docs/features/strava.md` — a Go-origin feature, so NOT the Rust-parity checklist, the `derpies` precedent).

**Files:**
- Modify: `cmd/tugbot/main.go`
- Modify: `AGENTS.md`
- Modify: `.env.example`
- Create: `docs/features/strava.md`

**What to implement:**

`cmd/tugbot/main.go` (five edit groups):
1. Import `github.com/danielcherubini/tugbot/internal/handlers/strava`.
2. `handlers` struct — add after the `derpies` field (~line 127): `strava *strava.Strava`.
3. `newHandlers` — add after `derpies: derpies.New(a),`: `strava: strava.New(a),`.
4. After the `gulag` VoteCheck `eg.Go` block (~line 521, before the MCP `eg.Go`), add EXACTLY:
   ```go
   eg.Go(func() error {
   	if err := h.strava.RunPoll(egCtx); err != nil && !isContextErr(err) {
   		slog.Error("strava poll loop terminated", "module", "main", "error", err)
   	}
   	return nil
   })
   ```
5. The three "thirteen" rewords (`main.go:154` comment, `:241` flag help, `:314` selftest log string): "thirteen" → "fourteen" — all others.

`AGENTS.md:28` — repair the quote fidelity while rewording (the current line quotes a shorter string than `:314` logs):
```
go run ./cmd/tugbot --selftest   # must log "selftest: Discord session and all fourteen handlers and the MCP server constructed", exit 0
```
Also in AGENTS.md: add `internal/handlers/strava` to the DB-touching test package enumeration (the `:20–22` list) — one word, keeping the doc honest.

`.env.example` — append (following the file's existing comment style):
```
# --- strava (optional; the feature is flag-gated in the features table too) ---
STRAVA_CLIENT_ID=
STRAVA_CLIENT_SECRET=
STRAVA_SHARED_THREAD_ID=
STRAVA_POLL_MINUTES=
```

`docs/features/strava.md` — front-matter mirroring `docs/features/derpies.md` (`status: live`, `last-verified: <today>`, `verified-by: <the exact gate run of this task>`), sections: **Mechanics** (the 15-min loop; per-athlete: token refresh + rotated persistence, `after` cursor with pending-hold semantics, the `pending`/`posted`/`skipped` disposition, one transaction then post; first enable = 24h lookback; 5-cycle processing drop); **Setup** (register the app at strava.com/settings/api → `STRAVA_CLIENT_ID`/`STRAVA_CLIENT_SECRET`; >1 athlete: self-serve dashboard upgrade to 10, no review; per-athlete: one-time OAuth consent over a localhost redirect — the consent MUST be requested with scope `activity:read_all` (lesser scopes filter 'Only You'/private activities out of the list endpoint; the authorize URL must carry the `scope` parameter — show it in the doc) → ONE SQL insert, show the exact statement); **Re-auth** (401/invalid-grant → `needs_reauth = true`, cursor preserved; re-run the consent → show the exact UPDATE statement + flag clear); **Rate budget** (at ≤5 athletes the default 1,000 read req/day cap suffices; approaching 10 requires the self-serve upgrade to the 2,000/day cap; token refresh is EXPECTED not to count against the read budget — verify against Strava's current rate-limits doc at enable time); **Late-sync trade-off** (the declared miss case); **Removing an athlete** (ONE DELETE FROM strava_athletes → FK cascade); **Webhooks** (deferred, additive — the seen table absorbs duplicate events; the poll stays source of truth; revisit if sub-minute freshness is wanted).

**Steps:**
- [ ] Make the `main.go` edits (five groups) + the two `AGENTS.md` edits + the `.env.example` edits; write `docs/features/strava.md`.
- [ ] Run `go build ./...` + `go vet ./...` + `gofmt -l .` (silent) + `make lint`.
- [ ] Run `go test ./... -count=1` (no PG — the selftest is still built+constructed; the DB tests self-skip).
- [ ] Run `make db-up`, then the full DB gate: `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` — did ALL packages pass?
- [ ] Run `go run ./cmd/tugbot --selftest` — did it exit 0 and log EXACTLY `selftest: Discord session and all fourteen handlers and the MCP server constructed`?
- [ ] Update `docs/features/strava.md`'s `verified-by` with this task's actual gate results (before committing).
- [ ] Commit with message: "strava: wire the 14th handler (poll loop, selftest 'fourteen handlers', .env.example, feature doc)"

**Acceptance criteria:**
- [ ] `go run ./cmd/tugbot --selftest` logs the full "fourteen handlers and the MCP server" line, exit 0.
- [ ] Full DB gate green (`-p 1 -count=1` with the override).
- [ ] `gofmt -l .` silent; `make lint` green.
- [ ] `AGENTS.md`'s selftest line quotes the same full string the binary logs.
- [ ] `.env.example` documents all four optional vars.
