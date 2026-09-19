---
status: current
last-verified: 2026-02-14
verified-by: web research — 2 angles (local codebase survey + Strava official docs/web research), date 2026-02-14
---

# Strava Activity Posting — Feasibility & Effort Research

**Question:** How hard would it be to add Strava support to tugbot so that when a known user finishes a ride/run, the bot automatically posts the activity link to a specified Discord thread?

## Executive Summary

**Moderate, well-trodden, low-risk: roughly 1–2 days of work including tests.** The Go side (~550–850 new lines) has a direct in-repo precedent for every integration point; the non-obvious cost is Strava-side setup (app registration, one-time per-user OAuth, refresh-token rotation), not code. A 15-minute polling loop fits inside Strava's default per-app rate limits with headroom for up to 10 users and requires **no public endpoint**, so it slots into the bot's existing `errgroup` background-loop machinery exactly like `gulag`'s loops. Webhooks remain a viable later latency upgrade.

## Findings

### 1. Local codebase — cost of adding a 14th handler

All integration points have an exact precedent in the repo:

| # | File | Change | ~Lines |
|---|------|--------|--------|
| 1 | `migrations/000006_strava.up.sql` | NEW — `strava_athletes` table + `features` seed row (`ON CONFLICT DO NOTHING`, shape of `000002`) | 40–60 |
| 2 | `internal/handlers/strava/strava.go` | NEW — `Strava` type, `New(*app.App)`, `FeatureKey`, `RunStravaPoll(ctx)` ticker loop (copy `gulag/loops.go:53–86`), `pollIteration` gated on `features.IsEnabled` | 120–180 |
| 3 | `internal/handlers/strava/client.go` | NEW — minimal Strava API client (list activities, token refresh) | 120–220 |
| 4 | `internal/handlers/strava/*_test.go` | NEW — `TestFeatureKey` pin + pure-logic units + PG-skip integration (gulag_test.go pattern) | 140–330 |
| 5 | `internal/config/config.go` | EDIT — optional Strava env vars (mirror `TUGBOT_MCP_PORT` handling) | 15–35 |
| 6 | `cmd/tugbot/main.go` | EDIT — `strava` field in `handlers` struct (`:111`), `strava: strava.New(a)` in `newHandlers` (`:153–171`), one `eg.Go(h.strava.RunStravaPoll(egCtx))` (`:508–533`), reword "thirteen"→"fourteen" (`:154`, `:241`, `:314`) | 5–10 |
| 7 | `AGENTS.md` | EDIT — reword selftest expectation "all thirteen handlers" (`:28`) | 1 |
| 8 | `docs/parity/checklist.md` | EDIT — add `## strava` section | 10–15 |

**Key facts (file:line citations):**

- **Registration:** `newHandlers(a *app.App) *handlers` at `cmd/tugbot/main.go:153–171`; the selftest (`main.go:239–315`) calls it and logs "all thirteen handlers… constructed" at `main.go:314` but **never asserts a count** — no numeric check to update, only 4 rewords ("thirteen"→"fourteen" in `main.go:154/241/314` + `AGENTS.md:28`).
- **Background loop:** `gulag/loops.go:55–86` is the canonical shape — ticker loop with `select` on `ctx.Done()`; iteration errors log-and-continue, `ctx.Err()` returned so the errgroup decides. Wired via `eg.Go(...)` in `main.go:508–533` on the `egCtx` derived from the `signal.NotifyContext` ctx (`main.go:352`); SIGTERM → ctx cancel → loop exits. Clean-shutdown is handled by `isContextErr` (`main.go:687`). One `eg.Go` call is all the plumbing needed.
- **Feature gate:** `features.IsEnabled(ctx, pool, FeatureKey)` (`internal/features/features.go:86–94`) is the documented background-task flavor — swallows DB errors and returns false, so a flapping DB cannot crash the errgroup. Seed the `features` row in the migration (pattern in `migrations/000002_derpies_gimmicks.up.sql:50`).
- **Config:** `config.LoadConfig` (`internal/config/config.go:75–152`) hard-requires only `DISCORD_TOKEN`, `APPLICATION_ID`, `DATABASE_URL`; all other vars are optional. Strava vars must stay **optional** (feature-flag-gated no-op) or `runSelftest` breaks when Strava env is absent.
- **DB:** `dbmigrate.Run` globs `migrations/*.up.sql` and applies each in its own transaction — a new table is one new file + `make migrate`; nothing in `dbmigrate` changes. House convention for new handler tables is raw `pool.Exec/Query` SQL with positional `$n` (whole of `gulag/loops.go`; `serversStore` in `main.go:172–207`) — no sqlc regeneration required.
- **Discord posting:** `ChannelMessageSend` is the right call for a thread — in bwmarrin/discordgo a thread is just a channel; no separate thread endpoint or extra intents needed. Only requirement: bot has read/send on that channel.
- **Selftest gotcha:** the selftest deliberately constructs handlers with no gateway, no background loops, no network (`main.go:239–251`). Keep `strava.New(app)` network-free; do all Strava I/O in the poll loop.
- **Test taint gotcha:** keep the integration test's table names strava-specific to avoid cross-tainting the shared compose DB (per `AGENTS.md` state-residue warning; repo runs DB tests with `-p 1`).

### 2. Strava API — detecting "a user just finished a ride/run"

Sources: official Strava developer docs (authentication, webhooks, rate-limits, API reference) — URLs in Evidence below.

**When an activity "appears":** an activity first surfaces (as a `/athlete/activities` list entry or an `activity.create` webhook) when the device/app uploads it after the workout — that is the "finished" trigger, plus device-sync lag. In-progress GPS sessions do not surface on the list endpoint until saved/uploaded (reasonable inference from the documented upload/processing workflow; the docs give no explicit statement).

**OAuth model** (official authentication docs):
- Standard OAuth 2.0 three-legged: authorize → code → `POST /oauth/token`. Access tokens last **exactly 6 hours** (`expires_in: 21600`).
- **Refresh tokens rotate on every refresh** ("a refresh token is issued back after all successful requests… the older refresh token is invalidated immediately"). Persisting the new refresh token on every refresh is the classic integration failure mode.
- Current official docs state no refresh-token TTL (historically ~42 days/inactivity-based; the 42-hour rule is not in current docs). Refreshing each polling cycle (≪6h apart) keeps the chain alive indefinitely; a long bot outage can force re-authorization — treat a 401 as "needs re-auth," not "retry."
- `localhost`/`127.0.0.1` are whitelisted redirect URIs → **per-user one-time OAuth can complete over a local redirect; no public endpoint needed for the auth flow.**
- Scopes: use **`activity:read_all`** — "Only You" (private) activities are filtered out of list results and arrive as synthetic deletes for `activity:read`-only apps.

**Rate limits (per-app, not per-athlete — current docs):**
- Read endpoints (incl. `GET /athlete/activities`): **100 req/15 min and 1,000 req/day** default; doubled (200/2,000) via self-serve dashboard upgrade.
- **Athlete capacity:** new apps start at **1 athlete**; self-serve upgrade to **10 athletes, no review**; >10 requires app review.
- Fit check: **10 users × 15-min cadence = 960 read req/day (≤1,000) and 10/15-min window (≤100) — inside the default limits.** 5-min cadence (2,880/day) is not. Strava's own rate-limits doc names activity polling as the known daily-limit culprit and steers volume to webhooks.
- 429s come with `X-RateLimit-*` headers; windows reset at :00/:15/:30/:45, daily at midnight UTC.

**Webhooks (current status: live, actively encouraged):**
- Managed at `https://www.strava.com/api/v3/push_subscriptions` (old host deprecated per changelog). **One subscription per app covers all athletes who authorized that app** — no per-user subscription ceilings in current docs.
- Events: `activity.create/update/delete`, `athlete` deauthorization; updates fire on Title/Type/Privacy changes (privacy updates require `activity:read_all`). No in-progress event exists.
- Requirements: public HTTPS (or tunnel) callback; `hub.challenge` handshake answered within 2s; events acked within 2s (3 retry attempts); payload is thin (`object_id`, `owner_id`, …) so you must **fetch and verify the activity yourself** (extra authenticated call).
- **Signature verification (`X-Strava-Signature`) is flaky** — community threads record Strava's API team saying it is "currently not supported" and removal of the docs, alongside `invalid_signature` reports. Do not hard-depend on it; use `verify_token` + owner-id checks + fetch-then-act.

**Activity data & dedupe:**
- `type` is **deprecated in favor of `sport_type`** (adds `MountainBikeRide`, `GravelRide`, `TrailRun`, `VirtualRide`, …). Match the run/cycling families on `sport_type` with `type` fallback; optionally exclude `VirtualRide`/`VirtualRun`.
- **Build the link from `id`**: `https://www.strava.com/activities/<id>`. `url`/`pr_url` are absent from current official model specs (they exist in real-world responses) — don't depend on them.
- Multiple webhook events can follow a single save (async attribute updates) → **dedupe on activity `id` in Postgres**, verify `sport_type` and non-processing state on first sight, treat update events on already-posted ids as no-ops.

**Setup & ToS:**
- Register the app at `strava.com/settings/api` (client_id/secret); set the authorization callback domain (or use localhost for one-time per-user authorize).
- Self-serve upgrade 1→10 athletes from the API dashboard (no review) — covers the expected user count.
- Official FAQ: "Strava subscription is a prerequisite" for building an app with the API — the bot's users would need Strava subscriptions.
- API agreement: token revocation is reserved for uses that replicate Strava's site/services or enable virtual races/competitions. A personal bot posting activity links to a Discord thread is fine; don't scrape/redisplay, honor privacy (a flip to `private=true` should be treated as delete semantics).

### 3. Recommended architecture & effort verdict

**Baseline build (no public endpoint required): Strava-side polling at a 15-minute cadence.**

1. One-time per-user OAuth (`activity:read_all`) over a local redirect; store per user in Postgres: athlete id, access token + expiry, refresh token, scope.
2. Refresh tokens proactively each cycle (refresh anything with <~1h remaining; persist the rotated refresh token); 401/invalid-grant → mark user for re-authorization.
3. Poll loop (the `gulag` `errgroup` pattern): every 15 min per user, `GET /athlete/activities?after=<last epoch>`; for each new `id` not in the `seen` table — fetch, verify `sport_type` ∈ {Run, Ride family} and not processing, upsert `seen` (BEFORE posting, so a crash can't double-post), then `ChannelMessageSend(threadID, "https://www.strava.com/activities/<id>")`.
4. 429 handler: back off to the next 15-min boundary on `X-RateLimit-*`; a lost poll is fine — the next cycle catches up via `after`.
5. Optional latency upgrade: one app-level webhook subscription (public HTTPS or tunnel) as a *hint* that triggers an immediate fetch+verify, with the 15-min poll as the correctness backstop.

**Effort verdict: moderate, low-risk, ~1–2 days (code + tests + local setup).** Hard part is Strava-side setup (app registration, self-serve athlete upgrade, per-user OAuth consent rounds), not code; the only genuinely tricky code is the rotating refresh-token lifecycle.

## Evidence

| Source | Type | Credibility |
|--------|------|-------------|
| https://developers.strava.com/docs/authentication/ | Official docs (OAuth, token refresh, rotation, scopes, revoke) | 1 |
| https://developers.strava.com/docs/webhooks/ | Official docs (subscription model, handshake, payload, signature caveat) | 1 |
| https://developers.strava.com/docs/webhookexample/ | Official example (signature scheme) | 1 |
| https://developers.strava.com/docs/rate-limits/ | Official docs (per-app limits, athlete capacity, upgrade tiers, 429 headers) | 1 |
| https://developers.strava.com/docs/reference/ + http://strava.github.io/api/v3/activities/ | Official API reference (activity model, `type`/`sport_type`, list params, "Only Me" filtering) | 1 |
| https://communityhub.strava.com/developers-api-7/how-long-does-a-refresh-token-last-3038 | Community (historical 42-day refresh-token expiry) | 4 |
| https://communityhub.strava.com/developers-api-7/signature-verification-shared-signing-secret-13220 / …/webhooks-invlid-signature-13568 | Community (signature verification flakiness) | 4 |
| https://communityhub.strava.com/developers-knowledge-base-14/strava-api-faq-12906 | Official FAQ (subscription prerequisite) | 2 |
| https://www.strava.com/legal/api | API agreement | 1 |
| Local code survey (`cmd/tugbot/main.go`, `internal/handlers/gulag`, `internal/{config,features,dbmigrate}`, `migrations/000002`, `AGENTS.md`) | Direct code reading, file:line cited in Findings | 1 |

## Unresolved Contradictions

- **Webhook signature verification:** officially documented in the example page, but community threads report Strava's API team not supporting it and its removal from docs. Resolution adopted: don't depend on it — `verify_token` + fetch-then-act covers it.

## Gaps / What Remains Unknown

- Refresh-token TTL is unlisted in current official docs (historical ~42-day/inactivity behavior from community). The per-cycle refresh loop neutralizes it; a long outage may require user re-auth.
- `in_progress`, `url`, `pr_url` are absent from current official docs (documentation lag vs. real API responses). The design deliberately avoids depending on them.
- Intermittent Strava 503/HTML responses reported in community — wrap API calls in retry-with-backoff.
