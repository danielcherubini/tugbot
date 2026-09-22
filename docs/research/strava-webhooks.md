---
status: current
last-verified: 2026-09-21
verified-by: web research — 9 sources (official Strava docs + developer community), 2026-09-21
---

# Strava Webhooks — Research

Research for the deferred Strava-webhook design (the `docs/features/strava.md` "Webhooks deferred" section: "treated as additive — the seen table already absorbs webhook duplicate-events; the poll loop stays the source of truth"). Question: what does the Strava webhook API actually require, and what does the codebase already provide?

## Executive Summary

Strava's current webhook API is **app-level, not per-athlete**: one subscription per app, registered with `client_id`/`client_secret` (no athlete access token), and the event payload carries **`owner_id`** (the athlete's Strava id — maps directly onto `strava_athletes.strava_athlete_id`). That removes the per-athlete registration, the callback-URL attribution trick, and re-registration on re-consent from the design. It adds two details: a **`hub.challenge` verification handshake** at registration, and a **2-second-ack requirement** (processing must be async).

Delivery has **no guarantees** — ≤3 attempts, duplicates possible, Strava-side losses documented — so the 15-min poll **must** stay as the backstop. Local readiness is high: the `:8643` server is route-agnostic, the seen table absorbs webhook duplicates for free, and the one non-trivial refactor (extracting `disposeActivity`) is bounded and fully test-pinned.

## Findings

### 1. Registration (official)

- **Endpoint:** `POST https://www.strava.com/api/v3/push_subscriptions` (changelog confirms the move from the deprecated `api.strava.com` host). Form-encoded with `client_id`, `client_secret`, `callback_url` (≤255 chars), `verify_token` (chosen by the app owner). **App credentials — no access token involved.**
- **Scope:** app-level. "Each application may only have one subscription" — it covers events for *all* athletes who have authorized the app. **Not idempotent**: re-registering while a subscription exists fails; the documented remedy is `DELETE` first.
- **Two-step handshake:** after the POST, Strava issues a **GET** to the callback with `hub.mode=subscribe`, `hub.challenge=<random>`, `hub.verify_token=<yours>`; the server must answer **within 2 seconds** with `200` and body `{"hub.challenge":"<echoed>"}` (application/json). Only then does the original POST return.
- **Response:** `{"id": 1}` (the subscription id).
- **Deletion:** `DELETE https://www.strava.com/api/v3/push_subscriptions/{id}?client_id=…&client_secret=…` → `204 No Content`. **View:** `GET /push_subscriptions?client_id=…&client_secret=…` → subscription details (the health-check primitive).
- **No `push_type` param** in the current API — the single subscription covers activity create/update/delete **plus athlete deauthorization** (`athlete/update` with `updates.authorized: "false"` — a free revocation detector).
- **Scope gate:** `activity:read` is required for activity webhooks (authentication docs). Our tokens carry `read` + `activity:read_all` — satisfied.
- **Access gate (uncertain):** the legacy reference page says the API "is only available to select applications" (contact developers@strava.com); the current docs page does not repeat the restriction. **The registration POST at rollout is the test.**

### 2. Payload shape (official example)

```json
{
    "aspect_type": "update",
    "event_time": 1516126040,
    "object_id": 1360128428,
    "object_type": "activity",
    "owner_id": 134815,
    "subscription_id": 120475,
    "updates": { "title": "Messy" }
}
```

- `object_type`: `"activity"` or `"athlete"`; `object_id`: the activity/athlete id; `aspect_type`: `"create" | "update" | "delete"`; `owner_id`: **the athlete's Strava id** (→ `strava_athletes.strava_athlete_id`); `updates`: for activity updates only `title` / `type` / `private` ("true"/"false"); for deauthorization `{"authorized": "false"}`.
- A thin pointer — no activity data beyond `updates`; the docs: "an application must decide how or if it wants to fetch the most up-to-date data." The detail fetch (`GET /api/v3/activities/{id}` with the athlete's token) is mandatory.
- **Signing (sample-code-only):** the official dummy-server walkthrough verifies an `X-Strava-Signature` header — HMAC-SHA256 over `<timestamp>.<raw-body>`, format `t=<unix ts>,v1=<hex sig>`, 300 s timestamp window. The main API page does **not** document the header or explain how to obtain the "signing secret" (the sample hardcodes `YOUR_SIGNING_SECRET`) → signature verification is **optional**; the unauthenticated-POST trust model (throttle + bounded work + seen-table dedupe) is the same as the onboarding callback.

### 3. Callback URL requirements

- Event pushes: return **200 within 2 seconds** or the push is **retried (up to 3 attempts total)** → heavy processing must be **async** (return 200 immediately; a slow 200 just triggers idempotent retries, which the seen table absorbs).
- No documented localhost exception (the official walkthrough uses ngrok to expose a local server — the callback must be publicly reachable; our `tugbot.wizards.town` caddy route fits). No explicit HTTPS requirement (docs examples use plain `http://`).
- `callback_url` max length 255 chars.

### 4. Reliability — the load-bearing finding

- **Docs:** the only delivery semantics are the 2 s ack and the ≤3-attempt retry. No at-least-once/at-most-once statement, no backoff interval, nothing after 3 failures.
- **Community (mark: anecdotal):**
  - **Duplicates:** events observed 3× for one activity (~7 min apart, despite a fast 200) — "make your handlers idempotent" (SDK docs echo). Our seen table's `UNIQUE (strava_athletes_id, strava_activity_id)` + `ON CONFLICT DO NOTHING` already is.
  - **Losses:** "Randomly Not Receiving Webhook Events" — confirmed not the app's side, **no resolution visible** (an open Strava-side gap). After 3 failed attempts, "you will not receive a webhook event for that specific activity again."
  - **Latency:** "shortly after" (docs, no SLA); third-party integrators report **15+ minute** delays and add defensive waits.
  - **Async attributes:** "some activity attributes are updated asynchronously, so one 'save' action by the athlete can result in multiple webhook events" — a `create` event can fire before all activity data is finalized; `GET /activities/{id}` may be incomplete right after (photos case; Strava's answer: "feature request backlog"). **No official statement that the GET can 404 — verify empirically; the pending-hold + poll backstop absorbs it either way.**
- **Token lifecycle:** the subscription is app-level (client credentials), keyed to "athletes that have authorized the application" — a refresh_token grant (new token pair, same authorization) **should not** affect it. Docs never say this explicitly; one community thread's missing events were attributed to **scope** (not token expiry); another was "fixed" by re-auth (anecdotal). **Treat as "probably independent — verify at rollout."**
- **Deauthorization is itself an event** (`athlete/update`, `updates.authorized: "false"`) — the bot can auto-detect revocation.
- **No documented expiry** on subscriptions — but community reports of silently dying subscriptions → the poll backstop (optionally + a periodic `GET /push_subscriptions` health log) is the remedy.

### 5. Rate budget interaction

Rate limits are per-app on all API requests (10-athlete tier: read 200/15 min + 2,000/day; overall 400/15 min + 4,000/day). Inbound webhook POSTs are not API requests (never mentioned in the rate-limit context); the **outbound detail fetches are ordinary read calls — no special treatment is documented**. A burst of N simultaneous finishes = N reads — trivially inside budget at 10 athletes.

### 6. Local codebase readiness

**Free (already there):**
- The `:8643` server is **route-agnostic** — `Start(ctx)` (`strava.go:1212`) wraps any `http.Handler`; a second route is a wiring change (small mux), not a listener change. The errgroup arm (`main.go:567-573`) and the selftest clause (`main.go:321-325`) extend by a parallel check.
- **Caddy scoping precedent** — the runbook's Runtime-surface section (`docs/features/strava.md:103`) + decision 0012: a second `handle /strava/webhook` mirrors the existing `handle /strava/callback` (everything else still 404s).
- **Duplicate absorption is free** — the seen table's `ON CONFLICT DO NOTHING` (`strava.go:438`) dedupes either source; the runbook's deferred-webhooks section already prescribes "the poll loop stays the source of truth (a webhook would merely narrow the first-sight latency, not replace the cursor)."
- **Client plumbing** — a 6th `StravaAPI` entry (e.g. `CreatePushSubscription` / `DeletePushSubscription` / `ListPushSubscriptions`) reuses `getJSON`, the form-POST pattern (`RefreshToken`), `classifyStatus`, the 30 s `stravaHTTP` client; `NewStravaAPI` stays network-free (selftest-safe).
- **All three test layers extend by copy-paste** — `statusServer` httptest (`client_test.go:18-28`), `stubStrava` scriptable fakes + call counters (`strava_integration_test.go:124-200`), `newTestStrava`/`doCallback` request driving (`:237-261`, `:1453-1459`).
- **Webhook state needs no per-tick hygiene** — the webhook is stateless by design (dedupe = the seen table; the only new state is the in-memory throttle map, which is self-hygenic: window sweep + 10 000-entry hard cap).

**New (net-new surface):**
- The `POST /strava/webhook` route + its own closure-local throttle instance (the documented bare-`&Strava{}` constraint at `strava.go:903-904` forces per-factory throttle state unless a lazy-init mechanism is added).
- The **`hub.challenge` verification GET** on the callback URL (registration handshake — distinguish from onboarding's `GET /strava/callback` by URL or `hub.mode`).
- The **async-processing worker** (2 s ack: return 200 immediately; the detail fetch + disposition happens in a bounded worker; a slow 200 just triggers idempotent retries).
- The selftest extension + the caddy runbook line.

**The one non-trivial refactor:** extract the per-activity disposition (the ready arms of `passForAthlete`'s steps c/d, `strava.go:314-420`) into a shared `disposeActivity(athlete, detail, target) (status, postItem)` that both the poll path and the webhook path call. The entanglement is bounded: the decision logic only appends to deferred `inserts`/`updates`/`posts` slices and never touches the transaction; the poll-specific parts (the list walk in `client.go:125-162`, the carried-pending snapshot, the per-athlete transaction commit, the cursor math at `strava.go:456-470`) stay in `passForAthlete` — a webhook never advances the cursor. The extraction is a pure re-slice of existing code, and the existing integration tests pin every arm of the sequence.

## Unresolved Contradictions

- **Delivery semantics:** Hookdeck characterizes Strava webhooks as "at-least-once"; the community thread says after 3 failed attempts the event is gone for good, and duplicates are observed. Best reading: **≤3 attempts, duplicates possible, losses possible** — the backstop is mandatory under any reading.
- **Re-auth effect on the subscription:** the app-level mechanism (docs) vs. a community thread where re-auth "fixed" missing events. Treat as "probably independent — verify empirically at rollout."

## Gaps (docs silent → verify empirically at rollout)

1. The `GET /activities/{id}` 404/incompleteness window right after a `create` event, and typical event latency (15+ min reported). The pending-hold + poll backstop absorbs it: a webhook-received `pending`/404 lands in the seen table and the next poll re-fetches.
2. The **signing secret** — `X-Strava-Signature` appears only in the official sample code with a hardcoded `YOUR_SIGNING_SECRET`; no doc explains how to obtain it. Signature verification is optional; the unauthenticated-POST trust model matches the onboarding callback.
3. Whether the webhook API is still "select applications only" (legacy-page note; the current docs don't repeat it). The registration POST at rollout is the test.
4. Subscription silent-death cadence — covered by the poll backstop; an optional periodic `GET /push_subscriptions` health log would surface it.

## Design consequences (for the discuss/specify phase)

- **One-time operational registration** (a runbook Setup step, like the OAuth app registration): `POST /push_subscriptions` + answer the `hub.challenge` GET. Not per-athlete; no re-registration on re-consent.
- **Webhook = early trigger, 15-min poll = backstop** — correctness identical to today; the common case posts in ~1 min instead of ~15.
- **`owner_id` → `strava_athletes` row** — no attribution trick needed; an event for an unknown `owner_id` is a cheap no-op (log + 200).
- **Bonus:** `athlete/update` deauth events let the bot auto-detect revocation (set `needs_reauth` / log).
- **The 5/10-min cadence question becomes optional** — with webhooks, the poll cadence is just the backstop interval.

## Evidence

| Source | Type | Credibility |
|---|---|---|
| https://developers.strava.com/docs/webhooks/ — "Webhooks Overview" | Official docs | 1 |
| https://strava.github.io/api/v3/events/ — "Strava Webhook Events V3 API" (legacy reference, still published by Strava) | Official (legacy) | 1 (superseded endpoint; cited as such) |
| https://developers.strava.com/docs/webhookexample/ — "Strava Webhooks Example" (ngrok dummy-server walkthrough; the `X-Strava-Signature` detail) | Official example | 1 (the signing flow is documented only here) |
| https://developers.strava.com/docs/changelog/ — endpoint move `api.strava.com` → `www.strava.com` | Official | 1 |
| https://developers.strava.com/docs/authentication/ — scope requirements, token lifetimes | Official docs | 1 |
| https://developers.strava.com/docs/rate-limits/ — per-app limits, 429 guidance | Official docs | 1 |
| communityhub.strava.com developer threads (8646, 1924, 1712, 1978, 1659, 3016, 12209) | Community | 4 (marked anecdotal) |
| https://pipedream.com/docs/v1/apps/strava — third-party latency reports | Third-party | 4 |
| https://github.com/james-langridge/strava-sdk/blob/main/docs/webhooks.md — idempotency guidance | SDK docs | 3 |
| Local code map: `internal/handlers/strava/{strava,client}.go`, `cmd/tugbot/main.go`, `strava_integration_test.go`, `docs/features/strava.md`, `docs/decisions/0012` | Local code | verified in-tree |
