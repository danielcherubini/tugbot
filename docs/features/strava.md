---
status: live
last-verified: 2026-09-22
verified-by: re-verified 2026-09-22 at the strava-webhooks ship (Tasks 1–4 — `decideActivity` extracted and shared by poll + webhook, the `/strava/webhook` route + verification handshake + event classification + job queue + 2 workers on `:8643`, the worker body (fetch + 401 refresh-and-retry arm + `start_date` rule + rows-affected-gated per-event transaction + post-after-commit), the selftest webhook clause, and this live Webhooks section): `gofmt -l .` silent; `go build ./...` ok; `go vet ./...` ok; `make lint` 0 issues; `go test ./... -count=1` all packages ok (the DB-touching tests self-skip without PG); the DB-touching gate `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` green with 0 skips under the override; `go run ./cmd/tugbot --selftest` logs exactly `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback and the strava webhook constructed` (exit 0)
---

# Strava activity posting

The strava handler (the fourteenth, `internal/handlers/strava`) polls the Strava v3 API every 10 minutes per configured athlete (`STRAVA_POLL_MINUTES=10` on the box; the default is 15) and posts the family's new activities into the shared Discord thread. The posting is a background loop only — no message event handlers — but onboarding is self-serve: the athlete runs the `/strava` slash command in their thread, consents via the posted link, and the bot's in-process `:8643` callback completes the flow (decision 0012; see Runtime surface). Failures are log-only per athlete (a per-athlete pass failure — a 401 to the reauth pause, a detail 429/5xx to the per-activity `pending` arm — never skips the other athletes; the one exception is a list-endpoint 429, which defers the remaining athletes of that tick to the next one, since Strava's 15-minute rate window is app-wide — the 10-minute ticker is the backoff); the only persistent side effect besides posts is the per-athlete seen/cursor state, plus the expiry sweep of lapsed onboarding rows (the tick also deletes `strava_onboardings` rows past their 1 h TTL).

## Mechanics

- **The poll loop** (`Strava.RunPoll`): the exact gulag loop shape (ticker + select; ctx cancel → return `ctx.Err()`; a per-iteration error logs and skips the iteration). Cadence is `STRAVA_POLL_MINUTES` (default 15; a value below 5 is clamped to 5 at config load with a warning; **5–14 accepted as-is** — the box is set to **10**). Each iteration is a no-op when the `strava` feature flag is off or the client credentials are absent (a logged-once warn).
- **Per athlete, one pass per iteration, in order:**
  1. **Token refresh near expiry** — when `token_expires_at` is within 1 hour, `POST /oauth/token` refresh runs, and the **ROTATED** refresh token is persisted immediately (an UPDATE of `access_token` / `refresh_token` / `token_expires_at`, its own statement). A 401-class refresh failure (`invalid_grant`) goes to the re-auth abort (below); any other refresh error skips the athlete with the cursor preserved (no commit).
  2. **The list call** — the single authenticated-athlete endpoint: `GET https://www.strava.com/api/v3/athlete/activities?per_page=100&after=<epoch>&page=N` walked with a **fixed** `after` on every page (the `after` = the athlete's `last_polled_at` cursor anchor) and an **advancing** `page = 1, 2, …`, **id-deduplicated**, until a short page or a page of zero new ids; the returned rows are sorted by (`start_date`, id). A NULL cursor (first-ever poll) adds a **24 h lookback**; the stamp check resolves off a base derived per pass.
  3. **Per new activity** (deduped against that athlete's `strava_seen_activities`, which is also the crash-safe dedupe): a family sport (the `family()` gate on `sport_type` — Run / Ride / one of the documented family values) gets one detail fetch (`GET /activities/{id}`); a still-unprocessed activity (the detail's `in_progress`, or `resource_state == -1` with the key absent), a transient detail fetch, a rate-limited detail fetch, or an unparsed device upload (empty `sport_type`) dispositions the row `pending` (re-fetched on the carried-pending step, NOT `skipped`); an out-of-family sport, or a family activity with no post target, dispositions it `skipped` (deterministic drops are not re-fetched); and **a 404 on a listed activity (deleted/revoked) is dispositioned `skipped` on first sight (deterministic; not retried)**.
  4. **The carried-pending step** — every carried `pending` row is re-fetched uniformly: now out-of-family → `skipped`; now ready → posted (if in-family with a target); a **gone 404** (the activity was deleted or its access revoked mid-hold) → `skipped` in the same UPDATE **without incrementing `retries`** (a terminal state — the 5-cycle budget stays for genuinely stuck/transient fetches); still unprocessed → `retries += 1` and the row stays `pending`. A carried row at **5 full cycles** is dropped: dispositioned `skipped` (and never posted) — a permanently stuck `pending` can never wedge the loop.
  5. **One transaction, then post** — the new seen-row inserts + the carried `pending` `retries` increments + the new `last_polled_at` commit in **ONE** transaction per athlete, and the posts (one `MessageCreate` into the athlete's `target_thread_id` or the `STRAVA_SHARED_THREAD_ID` fallback; no target at all → `skipped`) go out **after** the commit, keyed `posted` BEFORE posting by the activity id (never re-posted on a send error — a failed send is logged and the row stays `posted`). Posts are sent with mention parsing disabled (`allowed_mentions` `parse: []` — zero-value suppression: a regular bot message without `allowed_mentions` has ALL mention types parsed by default), so athlete titles/labels cannot trigger pings.
- **The `after` cursor (pending-hold semantics)**: computed in-transaction from the same unseen rows — if any `pending` rows remain, the cursor **holds at the `min(pending)` start date** (the window stays open, the unprocessed activity is inside it and will be re-seen next pass); otherwise the cursor advances to the `max(dispositioned)` start date; otherwise it is left unchanged. The update keys on the activity id, not the row. The cursor is thus watermark-math on `start_date` only.

The three dispositions on `strava_seen_activities.status`: `pending` (unprocessed or temporarily unreachable; retried, then dropped at the 5-cycle cap), `posted` (ready, in-family with a target; committed before its send), `skipped` (deterministic out-of-family / no-target / gone-404 drops, and the 5-cycle drops of stuck pendings).

## Setup

### Primary: the self-serve flow (the `/strava` command)

1. **Register the app** at <https://www.strava.com/settings/api> → `STRAVA_CLIENT_ID` / `STRAVA_CLIENT_SECRET` go in `.env`.
2. **More than one athlete:** the free tier is 1 athlete — a self-serve dashboard upgrade raises the cap to **10**, no application review needed.
3. **The athlete runs `/strava`** in the thread they want posts to go to (optionally `label:<name>` — the post's display name; omitted, the post falls to their Strava first name). The bot replies **privately (visible only to you)** with the authorize link (the link expires in an hour; the reply is ephemeral, binding the link to the invoker — a thread member cannot consume it). The command is NOT gated on the feature flag — onboarding is valid while the feature is off (a disabled flag appends "you'll be tracked once it's enabled" to the reply, and the row just sits until it's enabled).
4. **The athlete clicks the posted link, logs into their Strava, and Accepts `activity:read_all`.** The redirect lands on `tugbot.wizards.town/strava/callback`, which renders "Done — {label} is set up": the bot has already exchanged the code, verified the `activity:read_all` scope (a lesser-scope consent renders an "Insufficient scope" page instead — the scope note in the Fallback below), upserted the athlete row (token pair + `target_thread_id` = the command's thread + `needs_reauth = false`), and confirmed in the thread.
5. **The next 10-minute tick picks them up** (NULL cursor → the 24 h first-enable lookback).
6. **Enable the flag:** `UPDATE features SET enabled = true WHERE name = 'strava';`

### Fallback: the manual flow

Kept for operators without a thread (or for re-auth without re-running the command).

1. **Per athlete, one-time OAuth consent** (in the operator's browser, `localhost` redirect — the redirect target no longer needs anything listening: Strava whitelists `localhost` regardless of the declared domain, so the browser shows a connection-refused page and the code is in the address bar):

   ```
   https://www.strava.com/oauth/authorize?client_id=<STRAVA_CLIENT_ID>&redirect_uri=http://localhost:8080/&response_type=code&scope=activity:read_all&approval_prompt=auto
   ```

   **The consent MUST be requested with scope `activity:read_all`** — a lesser scope (e.g. `activity:read`) filters the athlete's own 'Only You'/private activities out of the list endpoint, so a private run rides straight into the no-target `skipped` arm. Ensure the `scope` parameter is actually on the URL above (a consent with no `scope` defaults to the lowest scope).
2. **One SQL insert** per athlete (the consent's code → access/refresh tokens via the OAuth token request, or pre-extracted):

   ```sql
   INSERT INTO strava_athletes
       (label, strava_athlete_id, access_token, refresh_token, token_expires_at, target_thread_id)
   VALUES
       ('Matt', <STRAVA_ATHLETE_ID>, '<access_token>', '<refresh_token>',
        to_timestamp(<TOKEN_EXPIRES_EPOCH>), <DISCORD_CHANNEL_ID_OR_NULL>)
   RETURNING *;
   ```

   `last_polled_at` is intentionally left `NULL` — the first poll does the 24 h lookback. `target_thread_id` is nullable (NULL → the `STRAVA_SHARED_THREAD_ID` fallback; neither → activities are `skipped`).
3. **Enable the flag:** `UPDATE features SET enabled = true WHERE name = 'strava';`

## Re-auth

A 401 / `invalid_grant` on the token refresh aborts **this athlete's pass** (the others still run) and commits, as its own statement:

```sql
UPDATE strava_athletes SET needs_reauth = true WHERE id = <athlete_row_id>;
```

The **cursor is preserved** (the transaction that carries it is not committed), so no activity is lost to the stall. A `needs_reauth = true` row is excluded from every pass until the flag is cleared, so the abort is sticky but recoverable without a redeploy.

**To fix — the self-serve path (primary):** the athlete re-runs `/strava` in their (new) thread and re-consents via the posted link. The callback's upsert refreshes the token pair, clears `needs_reauth`, moves `target_thread_id` to the newest thread, and lands the resolved label — an explicit command label overrides (new or existing athlete); an omitted one keeps the stored label (a re-consent never silently relabels a custom label). The confirm in the thread reads "🔄 {label}'s Strava authorization was refreshed."

**To fix — the manual path (fallback):** re-run the same consent in the operator's browser (Setup Fallback step 1, same `scope=activity:read_all` URL), swap in the new tokens, and clear the flag:

```sql
UPDATE strava_athletes
SET needs_reauth = false, access_token = '<new_access>', refresh_token = '<new_refresh>',
    token_expires_at = to_timestamp(<NEW_TOKEN_EXPIRES_EPOCH>)
WHERE id = <athlete_row_id>;
```

## Rate budget

The app is on the **10-athlete tier** (the self-serve dashboard upgrade is done). Caps: **read — 200 requests/15 min, 2,000/day**; **overall — 400 requests/15 min, 4,000/day** (the read cap is the binding one for polling; the overall cap also covers the OAuth endpoints). Per athlete per pass the read side is **one** list-side request — the `GET https://www.strava.com/api/v3/athlete/activities` walk (1–2 pages at normal cadence) — one detail fetch per new in-family activity, and a token refresh only when near expiry. At the full 10 athletes and the **10-minute cadence** that is ~15–30 read calls per 15-minute window (≈15% of the read cap) and ~2,900/day (≈72% of the 2,000/day read cap — the 10-minute choice is the safe one at 10-athlete scale; 5 minutes would be ~144% of the daily cap). Token refreshes are expected NOT to count against the read budget; **verify against Strava's current rate-limits documentation at enable time** (the cap is the operator's thing; a **detail-fetch** 429 surfaces as a transient `pending`, which is the intended degraded arm, not an error state, while a **list-fetch** 429 instead aborts the rest of the tick — deferred to the next cycle, since Strava's rate window is app-wide).

## Late-sync trade-off

The declared miss case: an activity that Strava itself only *finishes processing* (stays `pending`) over long transient/rate-limit walls is at most **75 minutes** from first sight to a `skipped` drop — the drop is **time-based** (a `pending` row is dropped once it has been `pending` for ≥ 75 min, via the row's first-creation `dispositioned_at` timestamp — cadence-independent; the `retries` column is retained as an observability counter of re-fetch cycles, no longer the drop trigger) — i.e. a run's post can arrive well after its tail (or never, if it stays unprocessed past 75 minutes). That is the declared trade-off for watermark-only cursor progression (no out-of-band gap detection).

**The 1-hour list-window overlap (backdated late uploads surface).** For a non-NULL cursor, the list window's `after` starts 1 hour *behind* the cursor (`after = last_polled_at − 1h`; the NULL-cursor first-enable 24 h lookback is untouched — it is already wide). The recent boundary is therefore re-listed on every pass, so an activity whose `start_date` is at most ~1 hour behind the cursor but surfaces LATER (a backdated manual upload, a second device syncing late, an edited start time) is NOT permanently missed by the all-time-max cursor advance — it lands on the next pass. The re-listing is free: re-listed ids are absorbed by the seen table (a re-listed `posted` row never re-posts; a re-listed `skipped` row stays `skipped`; a re-listed `pending` row is only ever re-fetched via the carried-snapshot step). The overlap widens the LIST window only — the cursor advance math (pending hold / all-time max / unchanged) is unchanged. **Declared miss** (the remaining backdate class): a backdated activity whose `start_date` is MORE than ~1 hour behind the cursor at the time it first surfaces is still missed — the cursor is a watermark and does not backdate (e.g. a manual upload backdated hours/days before the pass that surfaces it, if the `start_date` lands more than 1 h inside the cursor).

## Removing an athlete

One statement:

```sql
DELETE FROM strava_athletes WHERE id = <athlete_row_id>;
```

The `strava_seen_activities` rows are removed by the FK (`REFERENCES strava_athletes(id) ON DELETE CASCADE`) — no manual seen cleanup.

## Runtime surface (the `:8643` callback)

The consent redirect is served at `tugbot.wizards.town/strava/callback` — an in-process `:8643` listener in the bot (decision 0012: a deliberate deviation from the zero-public-surface constraint; the surface is one unauthenticated GET, guarded by the 128-bit `state`, single-use codes, the atomic claim, and a 10/min per-IP throttle). The caddy block for `tugbot.wizards.town` is scoped to `handle /strava/callback` (everything else 404s) and proxies to the bot host's `:8643`. The earlier standalone `strava-callback` sidecar (box-local, `:8080`) is retired — its source was never in the repo, and the consent surface now lives in the process that owns the feature. The `localhost` consent flow stays valid independently (Strava whitelists `localhost` regardless of the declared domain — the Fallback's manual consent needs nothing listening). The `/strava` reply is **ephemeral**, binding the link to the invoker (a thread member cannot consume another member's link); the in-thread confirm is a separate, non-ephemeral message.

## Webhooks

**What it is:** the webhook is an **early trigger**, not a replacement for the poll. Strava POSTs `activity/create` / `activity/update` events to `tugbot.wizards.town/strava/webhook` (a route on the existing `:8643` mux, served by the strava handler — the handler count stays fourteen). Each event carries the athlete's Strava id (`owner_id`); the bot maps it to `strava_athletes` (an **unknown `owner_id` is a cheap no-op** — log + 200), classifies the event, and queues a job for 2 workers: one detail fetch (`GET /activities/{id}`) and the post through the **shared `decideActivity`** (the same decision the poll path makes — Task 1's extraction). The **10-minute poll is the mandatory backstop** (decision 0013): the webhook has **no delivery guarantee** (duplicates and losses are both possible; Strava retries a non-200 callback up to 3 times, and the callback must answer 200 within 2 s — the bot ACKs the edge fast and does the fetch in the worker). A lost webhook event is recovered by the next poll pass; a duplicate is absorbed by the rows-affected gate below.

**Registration (one-time operational step).** The Strava webhook API is **app-level** — one subscription per app, registered with `client_id` / `client_secret`, NOT per-athlete. Registration is **not idempotent** (re-registering requires a `DELETE` first — see the remedies).

1. **Set `STRAVA_WEBHOOK_VERIFY_TOKEN` in `.env` and restart the bot** — the verification handshake needs it: absent/empty, the verification GET 403s and the subscription never activates (a subscription can be registered WITHOUT a `verify_token` and Strava will still deliver events, but the bot cannot then distinguish the verification GET from an event — so set the token BEFORE registering).
2. **Register the subscription:**
   ```
   curl -X POST https://www.strava.com/api/v3/push_subscriptions \
     -d client_id=<STRAVA_CLIENT_ID> \
     -d client_secret=<STRAVA_CLIENT_SECRET> \
     -d callback_url=https://tugbot.wizards.town/strava/webhook \
     -d verify_token=<STRAVA_WEBHOOK_VERIFY_TOKEN>
   ```
   (`callback_url` is ≤255 chars.)
3. **Strava immediately GETs the callback** with `hub.mode=subscribe`, `hub.challenge`, and `hub.verify_token`; the bot echoes the challenge (200 + `{"hub.challenge":"<echoed>"}`) and the subscription is active. A wrong/absent `verify_token` 403s the handshake and the subscription stays inactive.
4. **Record the returned subscription id here:** `373524` (registered 2026-09-22, HTTP 201; the verification chain was verified end-to-end at rollout — POST no-op 200, handshake echo 200, wrong-token 403 no-echo, 404 catchall — and the subscription lists from Strava with the correct `callback_url`).

**Caddy.** The block for `tugbot.wizards.town` is scoped `handle /strava/callback` + `handle /strava/webhook` → `10.0.0.44:8643` (everything else 404s — the mux on `:8643` serves both paths; the webhook route was added at the 2026-09-22 rollout):

```
handle /strava/webhook { reverse_proxy 10.0.0.44:8643 }
```

**Standing remedies.**

- **Re-registration** (change `callback_url` / `verify_token`): registration is NOT idempotent — `DELETE https://www.strava.com/api/v3/push_subscriptions/{id}?client_id=<…>&client_secret=<…>` first, then re-POST the registration above.
- **Health check:** `GET https://www.strava.com/api/v3/push_subscriptions?client_id=<…>&client_secret=<…>` lists the subscription.
- **Deauthorization:** a webhook event (`athlete/update` with `updates.authorized: "false"`) — the bot sets `needs_reauth` on the athlete row (the re-consent path above self-heals it).
- **The 401 → refresh-and-retry arm** (worker side): a merely expired token refreshes and the detail fetch retries; a genuinely revoked one (`invalid_grant`) sets `needs_reauth` and the job no-ops the rest of the pass for that athlete.
- **Rate-limited detail fetch:** a 429 is a transient no-row arm (the row is left for the poll's `pending` mechanics — see Rate budget).

**The accepted residual race.** The poll path snapshots `loadSeenIDs` before its detail fetches and appends posts *before* executing its `INSERT`s, so it cannot cheaply adopt the rows-affected gate without a restructure (out of scope — it would change pinned poll semantics). If a webhook event and a poll pass race on the same activity within the pass's window, a **double post is possible** (narrow window; the row state is still consistent). The webhook-vs-webhook case (the likely case — Strava's documented duplicate deliveries arrive close together) is **fully closed** by the rows-affected gate: the second event's per-event transaction inserts 0 rows and no-ops the post.

**Rate budget.** Webhook-triggered detail fetches are **ordinary read API calls** (no special rate treatment) — a burst of N events = N read calls. The 10-athlete tier's 200/15-min read cap absorbs realistic bursts: at the 10-minute cadence the poll is ~15–30 read calls per 15-minute window, so a webhook burst of ~170 or fewer stays under the cap. A pathological burst is rate-limited by Strava's 429, which the worker treats as a transient no-row arm (the row is left for the poll's `pending` mechanics — the backstop still posts it).
