---
status: accepted
date: 2026-09-21
superseded-by:
---

# Strava webhooks are an early trigger only — the 15-minute poll remains the backstop

Strava webhooks (the push subscription, decision-adjacent to **Push subscription** in `CONTEXT.md`) narrow first-sight latency from ~15 minutes to ~1 minute, but the 15-minute poll loop is **not** removed. The webhook path is additive: it fetches the activity on event, runs the same decision function, and persists through the same seen table — and the poll loop keeps running unchanged as the backstop that catches whatever the webhook path missed, dropped, or was late for.

## Why

Strava documents **no delivery guarantee** for webhooks. The documented semantics are only: the callback must return 200 within 2 seconds, and the push is retried **up to three attempts total** — nothing after that. Community evidence (marked anecdotal, but consistent in direction): events observed **three times** for one activity (duplicates — handlers must be idempotent), events **randomly lost** with no Strava-side resolution, and delivery delays of **15+ minutes**. Strava's own docs recommend webhooks *instead of* polling — never "webhook + poll" — so the combination is a deliberate design choice, not a documented pattern.

With no guarantee, a webhook-only design would silently drop activities whenever an event is lost, the queue is full, the fetch 404s during Strava's async-attribute window, or the subscription silently dies (community-reported; no documented expiry, no documented re-registration cadence). The poll loop is the only mechanism that closes all of those gaps — and it already exists, is already proven in production, and costs ~10–20 read calls per 15-minute window (≈10% of the read cap at full load on the 10-athlete tier).

## Considered options

- **Webhook-only (rejected):** remove the poll loop once webhooks are live. Saves the ~10–20 read calls per window, but converts every documented failure mode (loss, late delivery, subscription death, the 2-second-ack drop) into a silent miss with no recovery path. The seen table's dedupe would then be the *only* line of defense, and it has nothing to defend against a miss.
- **Webhook + backstop poll (chosen):** the poll cadence stays 15 minutes (the backstop interval, not a latency target); the webhook merely narrows the common-case latency. Correctness is identical to today's poll-only behavior — the webhook can only make a post *earlier*, never later, never twice (the seen table's `UNIQUE (strava_athletes_id, strava_activity_id)` + `ON CONFLICT DO NOTHING` absorbs duplicates from both sources).
- **Webhook + faster poll (deferred):** a 5/10-minute backstop is an orthogonal cadence change (the `STRAVA_POLL_MINUTES` floor + the time-based pending-drop) — kept separate per the feature-boundary decision.

## Consequences

- The seen table's idempotency invariants are now load-bearing for **two** sources (poll + webhook), not one: a duplicate `create` event, a `create` after a `posted` row, and a webhook-created row meeting the next poll tick all resolve to no-ops. The cursor stays purely poll-owned — a webhook never advances it.
- The 15-minute poll's role in the runbook changes from "the mechanism" to "the backstop"; the "Webhooks deferred" section is replaced by a live "Webhooks" section recording the registration ops step, the security model (unauthenticated POST; the `X-Strava-Signature` signing secret is undocumented, so verification is optional — the trust model is throttle + bounded work + seen-table dedupe), and the "verify empirically" gap list (subscription survival across re-consent, event latency, silent death — the backstop is the standing remedy for all three).
- If Strava ever documents a real delivery guarantee (at-least-once + replay), the backstop can be reconsidered — the dedupe design means removing it is a config/loop change, not a data-migration.
