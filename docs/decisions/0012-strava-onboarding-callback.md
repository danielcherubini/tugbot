---
status: accepted
date: 2026-09-21
superseded-by:
---

# Strava onboarding callback lives in-process (deliberate deviation from the zero-public-surface constraint)

The Strava feature spec (2026-09-19) declared the bot's runtime to have **zero public surface** — polling is outbound (tugbot→Strava, tugbot→Discord) and the only person-facing surface is the one-time consent redirect. Self-serve onboarding (an athlete runs `/strava` in their thread, clicks the posted link, and the flow completes itself — no operator SQL) requires a callback endpoint, and we place it **inside the tugbot process**: an `:8643` listener beside the in-process MCP's `:8642`, caddy-proxied at `tugbot.wizards.town/strava/callback`. This deliberately relaxes the zero-public-surface constraint: the added surface is one unauthenticated GET scoped to a single path, guarded by a required 128-bit `state`, single-use authorization codes, an atomic claim, and a per-IP throttle — and it consolidates the consent surface (which already existed as an ad-hoc sidecar on `:8080`) into the process that owns the feature.

## Considered options

- **Standalone sidecar (rejected):** extend the existing ad-hoc `strava-callback` unit to consume the onboarding (exchange + upsert + confirm via the bot's MCP `post_message`). Kept the bot binary untouched (selftest string stable), but split the onboarding logic across two binaries, added a second long-lived unit holding DB credentials, and introduced a best-effort notify hop (bot down = athlete row lands but the confirm is lost — a half-completed state).
- **Sidecar with Discord REST notify (rejected):** the same split-brain cost plus a second holder of the bot token.
- **In-process (chosen):** the `/strava` command and the callback handler both live in the existing strava handler (it already holds the StravaAPI client, config, DB pool, and the bot session); the confirm rides the bot's own session (mention-suppression pattern). One liveness (bot down = 502, the athlete re-runs `/strava` — no half-completed state), one unit, one set of credentials, one test home, no new env vars. Cost: one unauthenticated port on the bot binary and one selftest gate-string clause.

## Consequences

- The selftest gate string gains a clause — `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback constructed` (the 4-string ripple: 3 in `cmd/tugbot/main.go` + `AGENTS.md` — the same mechanical change as the earlier thirteen→fourteen).
- The caddy `tugbot.wizards.town` block is scoped to `handle /strava/callback` (everything else 404s) and its target flips `:8080` → `:8643`; the ad-hoc `strava-callback` systemd unit is retired (its source was never in the repo).
- The "zero public surface" property no longer holds for the bot runtime. The entire surface is: one unauthenticated GET on a caddy-routed port, serving one path.
- `update-tugbot` is unchanged (it builds the bot binary, which now carries the listener); no new systemd unit, no new env vars.

## Post-ship amendment

The `/strava` reply is **ephemeral** (invoker-only visibility), so the posted authorize link is bound to the invoker (a thread member can no longer consume another member's link — the ownership check is impossible in a plain browser redirect, so visibility IS the binding). The in-thread confirm remains a separate, non-ephemeral message posted to the thread for everyone.
