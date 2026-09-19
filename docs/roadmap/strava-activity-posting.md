---
status: approved
done-when: a finished Run/Cycling-family Strava activity of any enabled athlete lands in its per-athlete thread (or the shared fallback thread) within ~15 minutes as "{label} finished {noun}" + activity link, with no double posts across restarts, and the features flag 'strava' toggles it live
---

# Strava activity posting

A Go-origin feature (no Rust counterpart — excluded from `docs/parity/checklist.md`; lives in `docs/features/` per the derpies precedent). When a monitored Strava athlete finishes a run or ride, the bot posts the activity to Discord.

## Report it was researched against

`docs/research/strava-activity-posting.md` (rate-limit math, OAuth/refresh-token rotation, webhook status, codebase integration points — file:line citations there hold).

## 1. Scope & behavior

- **Athletes**: a per-athlete data model from day one. Start with one (the owner); grow to 10 via Strava's self-serve dashboard upgrade (no app review) — purely operational, no code change.
- **Cadence**: a single background poll loop, default **15 minutes** (no public endpoint; webhooks deferred, additive later).
- **Trigger**: an activity whose `sport_type` is in the **run/cycling family — including virtual and e-bike** (per operator choice), that is no longer processing.
- **Post target**: the athlete's `target_thread_id` (nullable column) with fallback to the configured **shared thread**.
- **Gating**: house feature flag `strava` in the `features` table; the loop no-ops when disabled or when no usable athletes exist.
- **Post shape** (exactly two lines):

  ```
  {label} finished <noun>
  https://www.strava.com/activities/<id>
  ```

  `noun` = `{distance} {sport_type}` (e.g. `42.3 km Running`, `98 km MountainBikeRide`); when the activity has a title: `<title> ({distance} {sport_type})` (e.g. `Matt finished "Tuesday tempo" (42.3 km Running)`). Distance: no decimals ≥ 100 km (`142 km`), one decimal < 100 km (`42.3 km`).

- **Setup flow (operator, one-time):**
  1. Register the Strava app (`STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` in `.env`).
  2. >1 athlete: self-serve dashboard upgrade to 10 (no review).
  3. Per athlete: one-time OAuth consent over a **localhost redirect** (no public endpoint); the consent output (label, athlete id, fresh tokens) lands in `strava_athletes` via the house SQL/MCP adjustment pattern — **adding an athlete is a single SQL insert, no deploy**.
  4. Enable the `strava` feature flag in SQL.
- **Explicitly out of scope (YAGNI):** webhooks (deferred; the `seen` table makes them additive later — rename/privacy update events on seen ids are no-ops by design), writing to Strava, editing/deleting old posts, per-activity-type threads, non-Run/Cycling sports.

## 2. Data model

`migrations/000006_strava.up.sql` (one new file, `make migrate`; `dbmigrate` untouched; table + feature-seed shape of `000002`):

```sql
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
    strava_athletes_id int NOT NULL REFERENCES public.strava_athletes(id),
    strava_activity_id bigint NOT NULL,
    posted_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (strava_athletes_id, strava_activity_id)  -- crash-safe dedupe key
);

INSERT INTO public.features (name, enabled) VALUES ('strava', false) ON CONFLICT (name) DO NOTHING;
```

**Config (`.env`, all optional** — the loop no-ops when absent; `--selftest` never breaks):

| Var | Purpose |
|---|---|
| `STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` | registered app credentials |
| `STRAVA_SHARED_THREAD_ID` | the fallback thread (per-athlete column takes precedence when set) |
| `STRAVA_POLL_MINUTES` | default 15; values below 15 are floor-enforced with a log warning |

Access: raw `pool.Exec/Query` with positional `$n` inside the strava package (house convention — `gulag/loops.go`, `serversStore`); **no sqlc regeneration**.

## 3. Loop mechanics

One `eg.Go` in `cmd/tugbot/main.go`'s errgroup block (the exact `gulag/loops.go:53–86` shape: ticker + `select` on `ctx.Done()`, iteration errors log-and-continue, `ctx.Err()` on shutdown). One 15-minute rhythm.

**Per-iteration sequence** (each step's failure: log + `continue` — one athlete's failure never blocks the others, never crashes the errgroup):

1. `features.IsEnabled(ctx, pool, "strava")` (the silently-false flavor) → false, return.
2. Load `strava_athletes`; skip `needs_reauth = true`; none, return.
3. **Per athlete, in order:**
   - **a. Token** — if `token_expires_at < now + 1h`: `POST /oauth/token` (`grant_type=refresh_token`) → **persist new access token + rotated refresh token + new expiry in one transaction**. 401/invalid-grant → `needs_reauth = true`, log, continue.
   - **b. List** — `GET /athlete/activities?after=<last_polled_at>`. 429 → log (with `X-RateLimit-*`), **no cursor update**, next cycle retries the window.
   - **c. New activities** — for each returned `id` NOT in `strava_seen_activities`: fetch `GET /athlete/activities/{id}`, then:
     - `sport_type` **not** in the family → `seen` row (no post, no re-fetch).
     - still **processing** → retry next cycles, **max 5**, then drop to `seen` (skipped; that workout never gets a post).
     - ready → **insert `seen` row first, then post** (crash ⇒ at worst a *missed* post, never a *double* post).
4. **Atomicity (per athlete, per pass):** one DB transaction inserts all new `seen` rows **and** advances `last_polled_at` to the new watermark; then the posts. Crash anywhere ⇒ no double posts; a re-poll re-walks the window and seen rows make it a no-op.
5. 429s elsewhere: log + the natural 15-min ticker is the backoff (no extra sleeps).

**First enable:** `last_polled_at NULL` → **24h lookback**; everything in the window lands in `seen` (posted if it matches). Enabling mid-week may post the last day's rides; anything older is never posted.

**Rate budget:** ≤10 athletes = 1 list/cycle + a few detail fetches ≈ **≤1,000 read req/day** — comfortable at ≤5 athletes on default limits; 10 athletes is at the default edge and covered by the 2,000/day cap the 10-athlete upgrade grants. (Recorded in `docs/features/strava.md`.)

## 4. Failure behavior

| Failure | Behavior |
|---|---|
| 401 / invalid-grant on refresh | `needs_reauth = true`; athlete skipped on all later cycles; others unaffected; log with re-auth pointer. Cursor **pauses** while the flag is set (no gap-flood on re-auth; resumption continues from the stored cursor). |
| Re-auth | re-run the local OAuth consent → SQL update of tokens + flag cleared (no deploy) |
| 429 anywhere | log (with `X-RateLimit-*`); no cursor update for that call; 15-min cadence is the backoff |
| Discord send fails | log; the activity is `seen` — **not** re-posted later (a missed post is a log artifact) |
| Still processing after 5 cycles | drops to `seen` (skipped) |
| Long bot outage (weeks) | refresh tokens may be expired (TTL undocumented) → 401 → `needs_reauth`; operator re-auths; no data loss (cursor + seen survive) |
| DB flap during an iteration | `IsEnabled` false path or per-athlete error → logged, cycle skipped, loop alive |
| Title rename / privacy flip on a seen id | **no-op** (privacy flips to Only-You honored by not re-posting) |

## 5. Wiring, tests & docs

| # | File | Kind |
|---|------|------|
| 1 | `migrations/000006_strava.up.sql` | new (tables + feature-flag seed) |
| 2 | `internal/handlers/strava/strava.go` | new — `New(*app.App)` (network-free, selftest-safe), `FeatureKey`, `RunPoll`, iteration; seams: a `StravaAPI` interface + the house `fu`-style Discord-send seam |
| 3 | `internal/handlers/strava/client.go` | new — Strava API client (list / detail / token refresh) |
| 4 | `internal/handlers/strava/strava_test.go` | new — `TestFeatureKey` pin + pure units (distance/noun formatting incl. title branch, sport-family matching incl. virtual & e-bike vs. e.g. Swimming, cursor/lookback helpers) |
| 5 | `internal/handlers/strava/strava_integration_test.go` | new — house DB-skip pattern, strava-specific table names, one iteration against a stubbed `StravaAPI` (post triggered / no-op on seen / cursor+seen atomicity) |
| 6 | `internal/config/config.go` | edit — the 4 optional vars (floor 15 enforced in config parsing) |
| 7 | `cmd/tugbot/main.go` | edit — struct field, `newHandlers` line, one `eg.Go`, reword "thirteen"→"fourteen" (3 strings) |
| 8 | `AGENTS.md` | edit — selftest expectation reworded |
| 9 | `docs/features/strava.md` | new — **not** the parity checklist: mechanism, setup (registration + self-upgrade), onboarding procedure (localhost consent + SQL insert), rate budget, re-auth flow, "webhooks — deferred, additive" note |

**Test gate:** `go build ./...`, `go vet`, `gofmt -l .`, `make lint`, `go test ./...` (DB tests self-skip without PG); full green gate with `TUGBOT_TEST_DATABASE_URL` + `-p 1 -count=1`; `go run ./cmd/tugbot --selftest` must log "Discord session and all **fourteen** handlers constructed".

**Rollout:** register app → `.env` vars → `make migrate` → one-time local OAuth consent + SQL insert (owner first) → enable the `strava` flag in SQL.

## Deferred (additive, no rework planned)

Webhook receiver (embedded in the existing in-process HTTP layer, per the MCP precedent on :8642) with the 15-min poll as the correctness backstop. The `seen` table absorbs duplicate events; the poll remains the source of truth. Trigger for revisiting: operator wants sub-minute freshness.
