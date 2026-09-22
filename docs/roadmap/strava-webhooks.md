---
status: approved
done-when: the /strava/webhook route is live in production on :8643, the app-level push subscription is registered (runbook ops step, subscription id recorded in the runbook), and a real ride is posted via the webhook path before the next poll tick (the seen row's dispositioned_at + post timestamp prove it) — with the 15-min poll running unchanged as the backstop (decision 0013)
---

# Strava Webhooks — Spec

## Context

The Strava feature (live: 15-min poll, seen-table dedupe, per-athlete rows) posts a finished ride/run ~15 min late. Strava's push-subscription API (research: `docs/research/strava-webhooks.md`) is **app-level** — one subscription per app, registered with `client_id`/`client_secret` (no athlete token), covering every athlete who authorized the app. Its delivery has **no guarantees** (≤3 attempts, duplicates and losses both documented) → the poll stays as the backstop (decision 0013). Webhooks narrow the common-case latency to ~1 min.

## Design

### Architecture & runtime surface

- The `:8643` listener (decision 0012) switches from a single-route handler to a small mux: `GET /strava/callback` (onboarding, unchanged) + `/strava/webhook` (new — both methods). Everything else 404s. Caddy gains `handle /strava/webhook` → `:8643` (mirrors the existing scoping).
- **Synchronous (must finish < 2 s):** route guard → the webhook route's own closure-local throttle (onboarding's pattern: last-XFF-hop keying, window sweep, hard cap) → payload parse → `owner_id` → athlete-row lookup (one SELECT) → enqueue a job (athlete row + activity id) onto a bounded channel (cap ~32) → **200 immediately**. Queue full → log + 200 (safe: the backstop catches it). Unknown `owner_id` → log + 200 (no-op).
- **Asynchronous (2 workers):** detail fetch (the athlete's token) → the shared decision function → a per-event transaction (seen-row upsert `ON CONFLICT DO NOTHING`-then-`UPDATE` + the 2-line post via `sendFn` after commit) → done. Terminal seen rows (`posted`/`skipped`) are absorbed — no re-post, no re-evaluation, consistent with the poll path.
- Shutdown: bounded grace drain of the queue, then cancel — a dropped job is safe (backstop). No new persisted state; the webhook is stateless by design (its only state: the in-memory throttle map — self-hygienic — and the bounded queue). No per-tick cleanup needed.
- Selftest: the gate string extends by one clause — `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback and the strava webhook constructed` (a parallel `WebhookHandler() == nil` check; the handler count stays fourteen — the webhook is a route on the existing strava handler).

### Event scoping & the deauth bonus

- **The `hub.challenge` verification GET** (part of the one-time ops registration): `GET /strava/webhook?hub.mode=subscribe&hub.challenge=…&hub.verify_token=…` → `200` + `{"hub.challenge":"<echoed>"}` within 2 s, only if `hub.verify_token` matches `STRAVA_WEBHOOK_VERIFY_TOKEN` (optional config var; empty = the verification GET is refused — matters only at registration time). Event POSTs never carry `hub.*` params — the two behaviors are cleanly separated by the query.
- **Event scoping** (the synchronous part classifies and only enqueues actionable work):
  - `object_type=activity` + `aspect_type=create|update` → enqueue (the worker does the fetch + disposition).
  - `aspect_type=delete` → **no-op log** (the activity is gone; the seen row stays as-is — terminal rows stay terminal).
  - `object_type=athlete` + `updates.authorized="false"` → **deauth bonus**: `UPDATE strava_athletes SET needs_reauth=TRUE WHERE strava_athlete_id=<owner_id>` + log. The poll loop's next tick sees the dead token → 401 → `needs_reauth` (already true) → the athlete is paused with the cursor preserved; re-consent (the onboarding flow) self-heals. The flag is idempotent — a duplicate deauth event is a no-op.
  - All other `athlete`/`updates` shapes, unknown `object_type`, unparseable payload → log + 200 (no-op; the backstop is unaffected).
- The classification is synchronous: pure in-memory work (parse + one SELECT + channel send), comfortably inside the 2 s ack, keeping the workers on network + DB work only.

### The worker

Per job (athlete row + activity id), in order:

1. **Detail fetch** `GET /api/v3/activities/{id}` with the athlete's token (the existing `StravaAPI.GetActivity`):
   - **401** → `needs_reauth = TRUE` + log, job ends (no cursor impact — a single event; the poll loop's next tick self-heals the flag state and re-consent clears it). No in-worker retry (the next event or the next poll tick is the retry).
   - **404 / gone** → the seen row becomes `skipped` (first-sight, consistent with the poll path's gone-arm) — only if no row exists yet (a terminal row is never overwritten).
   - **429 / transient** → the seen row becomes `pending` (first-sight) — the next tick's carried-pending retry re-fetches it (the existing retry budget + drop rule apply, unchanged).
   - **success but `isProcessing`** → `pending` (first-sight) — the same carried-pending path; an `update` event that arrives later may resolve it early (the whole point of acting on update).
2. **Shared decision function** on the ready detail: family gate → target resolution → `posted` + the 2-line post item, or `skipped` (out-of-family / no target). Pure — returns the disposition + post item; does not persist.
3. **Per-event transaction:** seen-row upsert (`INSERT … ON CONFLICT (strava_athletes_id, strava_activity_id) DO NOTHING`-then-`UPDATE` — first-sight insert; an existing terminal row is left untouched; an existing `pending` row is updated to the new disposition — this is how a later `update` event resolves it) → commit → the post is sent via `sendFn` after commit (the existing order: persist before post, never the reverse). `dispositioned_at` is set at first creation (whichever source — webhook or poll — creates the row first).

**Idempotency invariants** (all held by the seen table, no new dedupe logic): a duplicate `create` event (Strava retry / async re-event) lands on the existing row → no re-post. A `create` after a `posted` row → absorbed. A `create` that the poll tick already saw → the poll tick's own `ON CONFLICT DO NOTHING` absorbs the webhook-created row (the poll treats it as seen: `pending` → carried-pending re-fetch, terminal → absorbed). **The cursor is never touched by the webhook path** — it stays purely poll-owned (a webhook can't advance a cursor it didn't list).

**Rate budget:** each actionable event = 1 read call (the detail fetch). A burst of N simultaneous finishes = N reads — trivially inside 200/15 min at 10 athletes.

### The extracted decision function (the one refactor)

The per-activity *decision* moves out of `passForAthlete`'s steps c/d ready-arms into a pure function:

```
decideActivity(athlete *athleteRow, fetchOutcome) (disposition, postItem)
```

It owns: the in-family `sport_type` gate (the 9-value set), the `isProcessing` check, `resolveTarget` (per-athlete thread → shared config → none), and `makePost`/`buildPost` (the 2-line post, newline-sanitized). It takes the fetch **outcome** (not just a ready detail) so the error arms (404/429/transient → the pending/skipped first-sight decisions) are shared too. It returns the disposition (`posted` + the post item, or `skipped` + the reason) and **never persists** — no transaction, no slices, no cursor.

Stays poll-side (in `passForAthlete`): the list walk (the client's fixed-`after`/advancing-`page` math), the carried-pending snapshot, the per-athlete batched transaction + cursor math (`loadColumnTimes` + `nextCursor`), and the retry-budget arms (`retries += 1`, drop-at-5). The poll path calls `decideActivity` per activity and persists the results in its existing batched transaction; the webhook worker calls it and persists in its own per-event transaction. The two callers persist differently (batched tx vs per-event tx) — exactly why persistence stays caller-side and the function stays pure.

The extraction is a pure re-slice of existing code, not a behavior change (the decision logic today only appends to deferred `inserts`/`updates`/`posts` slices and never touches the transaction directly).

**Guard rails:** the existing integration tests pin every arm of the sequence (`TestStravaPassPostsNewFamily`, `TestStravaNoRepost`, `TestStravaPendingHoldAndRetry`, the 404/429 arms, the no-target arm) — they run unchanged against the refactored poll path and are the regression net for the extraction. The new webhook tests then exercise the same function from the other caller.

### Ops: registration, caddy, runbook

**One-time registration (runbook Setup §, a new subsection next to the OAuth app registration):**

1. Choose a `verify_token` value (any random string) → set `STRAVA_WEBHOOK_VERIFY_TOKEN` in the box `.env` + restart (the bot needs it live to answer the verification GET).
2. `POST https://www.strava.com/api/v3/push_subscriptions` with `client_id`, `client_secret`, `callback_url=https://tugbot.wizards.town/strava/webhook`, `verify_token=<value>`.
3. Strava immediately GETs the callback with `hub.mode=subscribe` — the bot answers the challenge echo automatically; the POST then returns `{"id": <n>}`.
4. Record `<n>` (the subscription id) in the runbook next to the other app credentials. `GET /push_subscriptions` (client credentials) is the health check if it's ever needed; `DELETE /push_subscriptions/<n>` is the documented way to change the callback (delete + re-create).

The step doubles as the **empirical test** of the two open research gaps: (a) whether the API is still "select applications only" (a 200/201 = it's open to our app), and (b) the subscription's live behavior (the first real event is the proof). Re-consent by any athlete does **not** touch the subscription (app-level) — if events ever stop, the runbook remedy is delete + re-create + check the scope (`activity:read` is required for activity events; our tokens carry it).

**Caddy:** one line added to the scoped `tugbot.wizards.town` block: `handle /strava/webhook` → `10.0.0.44:8643` (mirrors the existing `handle /strava/callback`); everything else still 404s. Both the verification GET and the event POSTs ride the same route.

**Runbook (`docs/features/strava.md`):** the "Webhooks deferred" section is **replaced** by a live "Webhooks" section — the registration ops step + subscription-id note; the security model (unauthenticated POST; the `X-Strava-Signature` signing secret is undocumented — verification is optional; the trust model = throttle + bounded work + seen-table dedupe, same as onboarding); the event-scoping table (create/update acted on, delete no-op, deauth sets the flag); the rate-budget line (1 read per actionable event); the "verify empirically" gap list (subscription survival across re-consent, event latency, silent death — the poll backstop is the standing remedy); and the deauth bonus (early revocation detection; re-consent self-heals).

### Testing & verification

**Unit (no DB):** payload parse (all documented shapes + unparseable); the `hub.challenge` verification (echo on matching `verify_token`, refuse on mismatch/empty config); event classification (create/update → job; delete → no-op; deauth → flag; unknown → no-op); the webhook route's throttle (its own closure-local instance — same test shape as onboarding's); queue-full → log + 200; unknown `owner_id` → log + 200; and the **200-before-work** guarantee (the handler returns before the worker runs — a stalled worker can't hold the ack).

**Integration (PG, the `stubStrava`/`newTestStrava` pattern):** webhook receipt → seen row + post in the thread (the `capture`/`byThread` assertions); **dedupe** — the same event twice → one post; a `create` after a `posted` row → absorbed; an `update` resolving a `pending` row → posts once; an unknown `owner_id` → no row, no post; a deauth event → `needs_reauth=TRUE` on the right row; a 401 detail fetch → flag set, no post, no cursor change; and the **refactor guard** — the existing poll-path integration tests run unchanged (they pin every arm of the extracted function).

**Verification gate (the house gate, extended):** the full gate as always (build/vet/fmt/lint/test + the DB-touching packages + `--selftest`), with the selftest gate string extended per the architecture section (the 4-string ripple: `cmd/tugbot/main.go` + `AGENTS.md` + the runbook's `verified-by`). **Rollout:** deploy → caddy line → the one-time registration ops step → a real ride is the end-to-end proof (the seen row's `dispositioned_at` vs the post timestamp shows the webhook beat the next tick; the runbook's "verify empirically" list gets checked off as events accumulate).

### Glossary & decisions

- `Push subscription` — added to `CONTEXT.md` (the app-level webhook registration; the subscription id is an ops note in the runbook, not bot state).
- Decision 0013 — webhooks are an early trigger only; the 15-min poll remains the backstop (no delivery guarantees; the seen-table dedupe is load-bearing for two sources).

### Out of scope

- The 5/10-min cadence change (the `STRAVA_POLL_MINUTES` floor 15→5 + the time-based pending-drop) — a separate, orthogonal feature (it becomes *optional* once webhooks land — the poll is just the backstop interval).
- Signature verification (`X-Strava-Signature`) — the signing secret is undocumented/unobtainable; verification stays optional (runbook-noted).
- Persisting the subscription ID (a runbook note suffices; the `GET /push_subscriptions` health check exists if it's ever needed).
- Per-athlete registration — doesn't exist in the current API (app-level only).
