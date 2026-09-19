---
status: live
last-verified: 2026-09-19
verified-by: verified 2026-09-19 at the handler-wiring ship (Task 6 — the `strava` handler joins the `handlers` struct + `newHandlers` + the errgroup, "thirteen" → "fourteen" reword, `.env.example` doc, this feature doc): `gofmt -l .` silent; `go build ./...` ok; `go vet ./...` ok; `make lint` 0 issues; `go test ./... -count=1` all 21 packages ok (the DB-touching tests self-skip without PG); the DB-touching gate `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` green — all 21 packages ok (incl. `internal/handlers/strava`'s 11 DB integration tests, `internal/dbmigrate`, `internal/features`, `cmd/tugbot`) with 0 skips under the override; `go run ./cmd/tugbot --selftest` logs exactly `selftest: Discord session and all fourteen handlers and the MCP server constructed` (exit 0)
---

# Strava activity posting

The strava handler (the fourteenth, `internal/handlers/strava`) polls the Strava v3 API every 15 minutes per configured athlete and posts the family's new activities into the shared Discord thread. It is a background loop only — no message event handlers, no slash command. Failures are log-only per athlete (one athlete's pass failure never skips the others); the only persistent side effect besides posts is the per-athlete seen/cursor state.

## Mechanics

- **The 15-minute loop** (`Strava.RunPoll`): the exact gulag loop shape (ticker + select; ctx cancel → return `ctx.Err()`; a per-iteration error logs and skips the iteration). Cadence is `STRAVA_POLL_MINUTES` (default 15; a value below 15 is clamped to 15 at config load). Each iteration is a no-op when the `strava` feature flag is off or the client credentials are absent (a logged-once warn).
- **Per athlete, one pass per iteration, in order:**
  1. **Token refresh near expiry** — when `token_expires_at` is within 1 hour, `POST /oauth/token` refresh runs, and the **ROTATED** refresh token is persisted immediately (an UPDATE of `access_token` / `refresh_token` / `token_expires_at`, its own statement). A 401-class refresh failure (`invalid_grant`) goes to the re-auth abort (below); any other refresh error skips the athlete with the cursor preserved (no commit).
  2. **The list call** — `GET /v3/athlete/activities?page&per_page=100`, paginated in full, with `after` = the athlete's `last_polled_at` cursor. A NULL cursor (first-ever poll) adds a **24 h lookback**; the stamp check resolves off a base derived per pass.
  3. **Per new activity** (deduped against that athlete's `strava_seen_activities`, which is also the crash-safe dedupe): a family sport (the `family()` gate on `sport_type` — Run / Ride / one of the documented family values) gets one detail fetch (`GET /activities/{id}`); a still-unprocessed activity (the detail's `in_progress`, or `resource_state == -1` with the key absent), a transient detail fetch, a rate-limited detail fetch, or an unparsed device upload (empty `sport_type`) dispositions the row `pending` (re-fetched on the carried-pending step, NOT `skipped`); an out-of-family sport, or a family activity with no post target, dispositions it `skipped` (deterministic drops are not re-fetched).
  4. **The carried-pending step** — every carried `pending` row is re-fetched uniformly: now out-of-family → `skipped`; now ready → posted (if in-family with a target); still unprocessed → `retries += 1` and the row stays `pending`. A carried row at **5 full cycles** is dropped: dispositioned `skipped` (and never posted) — a permanently stuck `pending` can never wedge the loop.
  5. **One transaction, then post** — the new seen-row inserts + the carried `pending` `retries` increments + the new `last_polled_at` commit in **ONE** transaction per athlete, and the posts (one `MessageCreate` into the athlete's `target_thread_id` or the `STRAVA_SHARED_THREAD_ID` fallback; no target at all → `skipped`) go out **after** the commit, keyed `posted` BEFORE posting by the activity id (never re-posted on a send error — a failed send is logged and the row stays `posted`).
- **The `after` cursor (pending-hold semantics)**: computed in-transaction from the same unseen rows — if any `pending` rows remain, the cursor **holds at the `min(pending)` start date** (the window stays open, the unprocessed activity is inside it and will be re-seen next pass); otherwise the cursor advances to the `max(dispositioned)` start date; otherwise it is left unchanged. The update keys on the activity id, not the row. The cursor is thus watermark-math on `start_date` only.

The three dispositions on `strava_seen_activities.status`: `pending` (unprocessed or temporarily unreachable; retried, then dropped at the 5-cycle cap), `posted` (ready, in-family with a target; committed before its send), `skipped` (deterministic out-of-family / no-target drops, and the 5-cycle drops of stuck pendings).

## Setup

1. **Register the app** at <https://www.strava.com/settings/api> → `STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` go in `.env`.
2. **More than one athlete:** the free tier is 1 athlete — a self-serve dashboard upgrade raises the cap to **10**, no application review needed.
3. **Per athlete, one-time OAuth consent** (in the operator's browser, `localhost` redirect):

   ```
   https://www.strava.com/oauth/authorize?client_id=<STRAVA_CLIENT_ID>&redirect_uri=http://localhost:8080/&response_type=code&scope=activity:read_all&approval_prompt=auto
   ```

   **The consent MUST be requested with scope `activity:read_all`** — a lesser scope (e.g. `activity:read`) filters the athlete's own 'Only You'/private activities out of the list endpoint, so a private run rides straight into the no-target `skipped` arm. Ensure the `scope` parameter is actually on the URL above (a consent with no `scope` defaults to the lowest scope).
4. **One SQL insert** per athlete (the consent's code → access/refresh tokens via the OAuth token request, or pre-extracted):

   ```sql
   INSERT INTO strava_athletes
       (label, strava_athlete_id, access_token, refresh_token, token_expires_at, target_thread_id)
   VALUES
       ('Matt', <STRAVA_ATHLETE_ID>, '<access_token>', '<refresh_token>',
        to_timestamp(<TOKEN_EXPIRES_EPOCH>), <DISCORD_CHANNEL_ID_OR_NULL>)
   RETURNING *;
   ```

   `last_polled_at` is intentionally left `NULL` — the first poll does the 24 h lookback. `target_thread_id` is nullable (NULL → the `STRAVA_SHARED_THREAD_ID` fallback; neither → activities are `skipped`).
5. **Enable the flag:** `UPDATE features SET enabled = true WHERE name = 'strava';`

## Re-auth

A 401 / `invalid_grant` on the token refresh aborts **this athlete's pass** (the others still run) and commits, as its own statement:

```sql
UPDATE strava_athletes SET needs_reauth = true WHERE id = <athlete_row_id>;
```

The **cursor is preserved** (the transaction that carries it is not committed), so no activity is lost to the stall. To fix: re-run the same consent in the operator's browser (step Setup 3, same `scope=activity:read_all` URL), swap in the new tokens, and clear the flag:

```sql
UPDATE strava_athletes
SET needs_reauth = false, access_token = '<new_access>', refresh_token = '<new_refresh>',
    token_expires_at = to_timestamp(<NEW_TOKEN_EXPIRES_EPOCH>)
WHERE id = <athlete_row_id>;
```

A `needs_reauth = true` row is excluded from every pass until the flag is cleared, so the abort is sticky but recoverable without a redeploy.

## Rate budget

At ≤ 5 athletes the default **1,000 read requests/day** API cap is comfortable: per athlete per 15-minute pass that is one list call (1–2 pages at normal cadence), one detail fetch per new in-family activity, and a token refresh only when near expiry. Approaching **10 athletes** (i.e. more total passes) requires the same self-serve dashboard upgrade that raises the athlete cap — it also raises the read cap to **2,000/day**. Token refreshes are expected NOT to count against the read budget; **verify against Strava's current rate-limits documentation at enable time** (the cap is the operator's thing; a 429 surfaces as a transient `pending`, which is the intended degraded arm, not an error state).

## Late-sync trade-off

The declared miss case: an activity that Strava itself only *finishes processing* (stays `pending`) beyond the 5-cycle cap, or a `pending` row whose `retries` runs out over long transient/rate-limit walls, is at most **5-cycle away** (≈ 75 minutes: 5 × 15-minute passes) from first sight to a `skipped` drop — i.e. a run's post can arrive well after its tail (or never, if it stays unprocessed past the 5 cycles). That is the declared trade-off for watermark-only cursor progression (no out-of-band gap detection).

**The 1-hour list-window overlap (backdated late uploads surface).** For a non-NULL cursor, the list window's `after` starts 1 hour *behind* the cursor (`after = last_polled_at − 1h`; the NULL-cursor first-enable 24 h lookback is untouched — it is already wide). The recent boundary is therefore re-listed on every pass, so an activity whose `start_date` is at most ~1 hour behind the cursor but surfaces LATER (a backdated manual upload, a second device syncing late, an edited start time) is NOT permanently missed by the all-time-max cursor advance — it lands on the next pass. The re-listing is free: re-listed ids are absorbed by the seen table (a re-listed `posted` row never re-posts; a re-listed `skipped` row stays `skipped`; a re-listed `pending` row is only ever re-fetched via the carried-snapshot step). The overlap widens the LIST window only — the cursor advance math (pending hold / all-time max / unchanged) is unchanged. **Declared miss** (the remaining backdate class): a backdated activity whose `start_date` is MORE than ~1 hour behind the cursor at the time it first surfaces is still missed — the cursor is a watermark and does not backdate (e.g. a manual upload backdated hours/days before the pass that surfaces it, if the `start_date` lands more than 1 h inside the cursor).

## Removing an athlete

One statement:

```sql
DELETE FROM strava_athletes WHERE id = <athlete_row_id>;
```

The `strava_seen_activities` rows are removed by the FK (`REFERENCES strava_athletes(id) ON DELETE CASCADE`) — no manual seen cleanup.

## Webhooks

Deferred, and treated as additive if ever added: `strava_seen_activities` (`UNIQUE (strava_athletes_id, strava_activity_id)`) already absorbs webhook duplicate-event (the seen table is the dedupe of either source), and the poll loop stays the source of truth (a webhook would merely narrow the first-sight latency, not replace the cursor). Revisit only if sub-minute freshness is wanted.
