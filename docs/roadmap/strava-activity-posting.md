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
- **Trigger**: an activity whose `sport_type` is in the **fixed, non-configured** run/cycling family constant (extension is a one-line change; unit tests pin the set member-by-member):
  `Run, TrailRun, VirtualRun, Ride, VirtualRide, GravelRide, MountainBikeRide, EBikeRide, EMountainBikeRide`
  "e-bike" means **both** e-bike variants (`EBikeRide` + `EMountainBikeRide`), per operator choice.
- **Post target**: the athlete's `target_thread_id` (nullable column) with fallback to the configured **shared thread**.
- **Gating**: house feature flag `strava` in the `features` table; the loop no-ops when the flag is disabled, when the app credentials are absent, or when no **usable** athlete exists ("usable" = not `needs_reauth` and has a resolvable target).
- **Post shape** (exactly two lines; `sport_type` is the **raw** enum value — no prettification):

  ```
  {label} finished <noun>
  https://www.strava.com/activities/<id>
  ```

  `noun` = `{distance} {sport_type}` (e.g. `42.3 km Run`, `98.3 km MountainBikeRide`); when the activity has a title: `"{title}" ({distance} {sport_type})` (e.g. `Matt finished "Tuesday tempo" (42.3 km Run)`). Distance: no decimals ≥ 100 km (`142 km`), one decimal < 100 km (`42.3 km`).

- **Setup flow (operator, one-time):**
  1. Register the Strava app (`STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` in `.env`).
  2. >1 athlete: self-serve dashboard upgrade to 10 (no review).
  3. Per athlete: one-time OAuth consent over a **localhost redirect** (no public endpoint); the consent output (label, athlete id, fresh tokens) lands in `strava_athletes` via the house SQL/MCP adjustment pattern — **adding an athlete is a single SQL insert, no deploy**.
  4. Enable the `strava` feature flag in SQL.
- **Explicitly out of scope (YAGNI):** webhooks (deferred; the `seen` table makes them additive later — rename/privacy update events on seen ids are no-ops by design), writing to Strava, editing/deleting old posts, per-activity-type threads, non-Run/Cycling sports.

## 2. Data model

`migrations/000006_strava.up.sql` (one new file, `make migrate`; `dbmigrate` untouched). The feature-seed INSERT matches `000002_derpies_gimmicks.up.sql:50` verbatim in shape. **Declared deviation (both tables):** `timestamptz` instead of the house naive-timestamp frame — an intentional first: the strava values come straight from the Strava epoch-seconds API and are compared against `now()`, so tz-aware columns avoid local-time ambiguity. All other tables are untouched; the DDL uses plain `CREATE TABLE` (not the pg_dump-style explicit-sequence idioms of 000002).

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

**Status semantics** (every discovered activity gets exactly one row, in one of three states — the dedupe key is the non-negotiable):
- `pending` — **not yet dispositioned: still processing, or the detail fetch failed transiently (429/5xx/timeout)**; re-fetched each cycle by step d, `retries` incremented, dropped to `skipped` at 5.
- `posted` — posted (or will be posted in the same pass); **never re-posted, by invariant**.
- `skipped` — deterministically not posting: outside the family, or a `pending` row that gave up at 5. Never re-fetched.

**Config (`.env`, all optional** — the loop no-ops when absent; `--selftest` never breaks):

| Var | Purpose |
|---|---|
| `STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` | registered app credentials |
| `STRAVA_SHARED_THREAD_ID` | the fallback thread (per-athlete column takes precedence when set) |
| `STRAVA_POLL_MINUTES` | default 15; **non-numeric → `LoadError` (fail loud, `parseMCPPort` precedent — a malformed value fails the whole bot's config load, and `--selftest`, by design, even with the flag disabled)**; values below 15 are floor-enforced to 15 with a log warning |

Access: raw `pool.Exec/Query` with positional `$n` inside the strava package (house convention — `gulag/loops.go`, `serversStore`); **no sqlc regeneration**.

**Removing an athlete** is a single `DELETE FROM strava_athletes` — the FK `ON DELETE CASCADE` removes the seen rows. No code path needed.

## 3. Loop mechanics

One `eg.Go` in `cmd/tugbot/main.go`'s errgroup block (the exact `gulag/loops.go:53–86` shape: ticker + `select` on `ctx.Done()`, iteration errors log-and-continue, `ctx.Err()` on shutdown). One 15-minute rhythm.

**Cursor semantics (defined, per review blocker).** Strava's `after` parameter filters on **activity start time** (epoch seconds), so `last_polled_at` tracks *start times*. Rule: the cursor **holds at the oldest `pending` row's `start_date` while any `pending` rows exist**; otherwise it advances to the newest confirmed `start_date` — and a `pending` row exists for **every** still-undispositioned id (processing or transient detail-failure, per 2.c), so the cursor can never advance past an undispositioned family activity. The held cursor makes the next cycle re-walk the open window, which is what **catches late-synced activities** whose start is still ≥ the window start but hadn't been listed before (seen rows make re-walked ids no-ops); `pending` retries themselves are by-id detail fetches (step d), independent of the list window. **`after` is treated as exclusive** (verify inclusivity once against the live API during implementation; a same-second off-by-one is bounded by seen dedupe + the 5-retry drop).

**Per-iteration sequence** (each step's failure: log + `continue` — one athlete's failure never blocks the others, never crashes the errgroup; **exception: a 401 on any call aborts the athlete's remaining pass** — the per-athlete transaction is discarded, so the cursor is preserved and the post re-auth re-walk re-covers the window, per the failure table):

1. **Config preflight** — `features.IsEnabled(ctx, pool, "strava")` (the silently-false flavor) → false, return. `STRAVA_CLIENT_ID/SECRET` unset → return. Load `strava_athletes`; skip `needs_reauth = true` (their cursor is implicitly preserved — no calls are made for them, so nothing advances); none usable, return.
2. **Per athlete, in order:**
   - **a. Token** — if `token_expires_at < now + 1h`: `POST /oauth/token` (`grant_type=refresh_token`) → **persist new access token + rotated refresh token + new expiry in one transaction**. 401/invalid-grant → `needs_reauth = true`, log, continue.
   - **b. Window & list** — `window_start = last_polled_at ?? (now − 24h)`; `GET /athlete/activities?after=<window_start>` (paginate while a full 100-row page returns). 429 → log (with `X-RateLimit-*`), **no cursor change**, next cycle retries the window.
   - **c. Per listed id** (a `seen` row of any status in the list → no-op, handled by `d` if `pending`):
     - summary `sport_type` **missing/empty** (device upload not yet parsed by Strava) → `pending` row (step d fetches the detail next cycle) — **not** `skipped` (an unverified-absence must not become a permanent silent miss).
     - summary `sport_type` **not** in the family → `seen` row `skipped` (with `start_date`), **no detail fetch** — the summary carries `sport_type`; don't burn a read on a non-family activity.
     - in family → fetch `GET /athlete/activities/{id}`:
       - still **processing** — `in_progress == true` when the field is present, or (when `in_progress` is absent) `resource_state == -1`, the official API reference's documented processing indicator (**when neither signal is present → treat as ready**, the documented fallback) → `pending` row (with `start_date`).
       - ready → `seen` row `posted` (inserted **before** the post — a crash ⇒ at worst a *missed* post, never a *double* post), then post.
     - detail fetch fails transiently (429/5xx/timeout) → `pending` row (with `start_date`): the existing hold rule keeps the id inside the window and step d re-fetches it next cycles — a 429'd id can therefore never be advanced past. **A 401 on the detail (or list) call aborts the pass** (see the sequence preamble).
   - **d. Pending retry** — for each of the athlete's **carried** `pending` rows (from earlier passes; a row newly created in step c first re-fetches on the *next* cycle, so `retries` counts full re-fetch cycles starting at 0 on the discovery pass): re-fetch the detail by id. ready now → `posted` + post; still processing, or the fetch failed transiently again → `retries += 1`, stays `pending`; `retries ≥ 5` → `skipped` (drop — that workout never gets a post). The retry count is **persisted in the row**, so a restart does not reset the 5-cycle budget (and a restart cannot loop a stuck activity forever).
   - **e. Cursor** — if `pending` rows remain (processing or transient-failed, per 2.c): `last_polled_at = min(pending.start_date)` (hold the window open — this is the hold that covers the 429'd / failed ids from 2.c; there is no separate "undispositioned" hold condition because step c creates a `pending` row for every such id). Else if this pass dispositioned anything: `last_polled_at = max(this-pass start_dates)`. Else: unchanged.
3. **Atomicity (per athlete, per pass):** one DB transaction applies all new `seen` rows + `retries` increments + the new `last_polled_at`; **then** the posts. Crash anywhere ⇒ no double posts (the `posted` invariant); a re-poll re-walks the window and seen rows make it a no-op.
4. 429s elsewhere: log (with `X-RateLimit-*`) + the natural 15-min ticker is the backoff (no extra sleeps).

**First enable (operator-visible, one-time):** `last_polled_at NULL` → **24h lookback**; everything in the window gets a `seen` row (posted / skipped / pending as applicable). Enabling mid-week may post the last day's runs; anything older is never posted. While a lookback window still has `pending` rows the cursor holds at its low end, so the 24h window re-walks until everything settles — which also picks up late-synced activities inside it.

**Documented trade-off (late-sync, answered per review):** an activity whose start lands **below** the current window start (device sync lag spanning more than one cycle while the window was closed) is **missed** — the cursor does not backdate. Sync-lag ≥15 min is the residual case; the `pending`-hold machinery covers the common case, and a missed post is a log artifact, not data loss.

**Rate budget:** ≤10 athletes = 1–2 list calls/cycle (pagination) + 1 detail fetch per family candidate ≈ **≤1,000 read req/day at ≤5 athletes**, at the default cap's edge at 10 — covered by the 2,000/day cap the 10-athlete self-upgrade grants. Token refreshes are OAuth-endpoint calls, not read-budget traffic (even if counted: ≤ ~5/athlete/day). Recorded in `docs/features/strava.md`.

## 4. Failure behavior

| Failure | Behavior |
|---|---|
| 401 / invalid-grant on **any** athlete call (refresh, list, or detail — covers the user de-authorizing the app on the Strava side) | **the athlete's remaining pass aborts and the per-athlete transaction (incl. `last_polled_at`) is discarded** — the cursor is preserved, and the post re-auth re-walk re-covers every id in the window. `needs_reauth = true`; athlete skipped on all later cycles; others unaffected; log with re-auth pointer |
| Re-auth | re-run the local OAuth consent → SQL update of tokens + flag cleared (no deploy); polling resumes from the preserved cursor — no gap-flood |
| 429 anywhere | log (with `X-RateLimit-*`); cursor holds for that call; 15-min cadence is the backoff |
| Discord send fails | log; the activity is `posted` (dispositioned) — **not** re-posted later (a missed post is a log artifact) |
| Still processing after 5 cycles | drops to `skipped` (persisted `retries` — restart-safe) |
| Athlete with no resolvable target (no per-athlete id and no `STRAVA_SHARED_THREAD_ID`) | new family activities dispositioned `skipped`; one counted line in the per-cycle log (no per-activity spam) |
| Late-synced activity with start ≥ window start | caught by the `pending`-hold re-walk (see §3 trade-off note) |
| Long bot outage (weeks) | refresh tokens may be expired (TTL undocumented) → 401 → `needs_reauth`; operator re-auths; no data loss (cursor + seen survive) |
| DB flap during an iteration | `IsEnabled` false path or per-athlete error → logged, cycle skipped, loop alive |
| Title rename / privacy flip on a dispositionsed id | **no-op** (privacy flips to Only-You honored by not re-posting) |
| Removing an athlete | single SQL delete (FK cascade clears seen rows) |

## 5. Wiring, tests & docs

| # | File | Kind |
|---|------|------|
| 1 | `migrations/000006_strava.up.sql` | new (tables incl. the declared `timestamptz` deviation + feature-flag seed) |
| 2 | `internal/handlers/strava/strava.go` | new — `New(*app.App)` (network-free, selftest-safe), `FeatureKey`, `RunPoll`, iteration (config preflight, per-athlete error isolation, cursor hold); seams: a `StravaAPI` interface + the house `fu`-style Discord-send seam |
| 3 | `internal/handlers/strava/client.go` | new — Strava API client (list w/ pagination / detail / token refresh; 429 surfaced to the iteration) |
| 4 | `internal/handlers/strava/strava_test.go` | new — `TestFeatureKey` pin + pure units: distance/noun formatting incl. title branch, sport-family set pin (all 9 values, member-by-member, vs. e.g. `Swim`/`Walking`), cursor water-mark math (hold-at-oldest-pending, advance-on-all-settled), retry lifecycle (`pending` → `retries` → `skipped` at 5, incl. the carried-only step-d semantics), 429 detail-failure → `pending` retry path (incl. the cursor can-not-advance-past invariant), 429 list-failure → no-cursor-change, processing-detection: the `in_progress` path, the `resource_state == -1` path, and the neither-field-present ⇒ ready fallback, missing/empty summary `sport_type` → `pending` (not `skipped`) |
| 5 | `internal/handlers/strava/strava_integration_test.go` | new — house DB-skip pattern, strava-specific table names, one+ iteration(s) against a stubbed `StravaAPI`: post triggered / no-op on seen / cursor+seen atomicity in one transaction / `needs_reauth` pause → re-auth → resume from the preserved cursor (no gap-flood) |
| 6 | `internal/config/config.go` | edit — the 4 optional vars (non-numeric poll minutes → `LoadError`; <15 floored with a log warning) |
| 7 | `cmd/tugbot/main.go` | edit — struct field, `newHandlers` line, one `eg.Go`, reword "thirteen"→"fourteen" (3 strings) |
| 8 | `AGENTS.md` | edit — selftest expectation reworded to the FULL log string (the current :28 paraphrase quotes a shorter string than `main.go:314` logs — repair the quote fidelity while touching the line) |
| 9 | `docs/features/strava.md` | new — **not** the parity checklist: mechanism, setup (registration + self-upgrade), onboarding procedure (localhost consent + SQL insert), the rate budget (incl. the token-refresh note), the re-auth flow, the late-sync trade-off, and a "webhooks — deferred, additive" note |

**Test gate:** `go build ./...`, `go vet`, `gofmt -l .`, `make lint`, `go test ./...` (DB tests self-skip without PG); full green gate with `TUGBOT_TEST_DATABASE_URL` + `-p 1 -count=1`; `go run ./cmd/tugbot --selftest` must log `"selftest: Discord session and all fourteen handlers and the MCP server constructed"` (the full expected string, per `cmd/tugbot/main.go:314`).

**Rollout:** register app → `.env` vars → `make migrate` → one-time local OAuth consent + SQL insert (owner first) → enable the `strava` flag in SQL.

## Deferred (additive, no rework planned)

Webhook receiver (embedded in the existing in-process HTTP layer, per the MCP precedent on :8642) with the 15-min poll as the correctness backstop. The `seen` table absorbs duplicate events; the poll remains the source of truth. Trigger for revisiting: operator wants sub-minute freshness.
