---
status: approved
done-when: A new guild member runs /strava in a thread, clicks the posted link, and Accepts activity:read_all — the "Done" page renders, the athlete row exists (valid token, thread target, correct label), the confirm message is in the thread, and the next poll tick posts their first finished activity; verified end-to-end on the production box with a second real athlete (the Strava app is already upgraded to 10 athletes).
---

# Strava self-serve onboarding (`/strava` + the in-process callback)

## Background

The Strava feature (shipped 2026-09-19, `docs/features/strava.md`) is live: the 15-min poll loop posts finished family activities to per-athlete threads. Adding an **athlete** (CONTEXT.md) is today a manual operator flow: the person consents in a browser, an operator exchanges the code, and an SQL insert lands the row. The Strava app has been upgraded (self-serve dashboard) to the 10-athlete cap, so the growth path is open — this spec makes onboarding a 30-second self-serve flow and removes the operator from the loop.

The one-time consent redirect is the only person-facing surface in the Strava design. Today it lands on an ad-hoc standalone `strava-callback` systemd unit on `:8080` (caddy-proxied at `tugbot.wizards.town`), which renders the `code` for a human to copy. This spec replaces that with a completed flow: the callback **does** the exchange, insert, and confirm — no human in the loop.

## Goals

- Any guild member can onboard their own Strava account by running `/strava` in the thread they want posts to — no permission gate, no operator.
- The consent redirect completes the flow automatically: exchange the code, verify the scope, insert/upsert the athlete row, confirm in the thread.
- Re-consent (re-auth) by an existing athlete works through the same path (the upsert refreshes the token, clears `needs_reauth`, moves the thread, preserves the label).
- The manual runbook flow stays valid as a fallback (operator without a thread; re-auth without re-running the command).

## Non-goals

- No per-athlete removal command (removal stays a manual `DELETE` — the runbook).
- No multi-guild scoping (single-guild bot; the command is ungated by design decision).
- No webhook-based onboarding or polling changes — the poll loop is untouched (it picks up new athlete rows on the next tick).
- No new env vars, no new systemd unit, no new handler (the command attaches to the existing strava handler; the selftest handler count stays fourteen).

## Design

### 1. The flow & actors

1. **Athlete** runs `/strava` (optionally `label:Sam`) in the thread they want posts to.
2. **Bot (strava handler)**: generates a `state` (16 `crypto/rand` bytes → 32 hex), inserts a pending row in the new `strava_onboardings` table (`state`, `thread_id`, `label` nullable, `expires_at = now + 1h`, `status = 'pending'`), and replies **in the thread** (visible, mention-suppressed): a one-liner with the authorize link — `https://www.strava.com/oauth/authorize?client_id=<id>&redirect_uri=https%3A%2F%2Ftugbot.wizards.town%2Fstrava%2Fcallback&response_type=code&scope=activity:read_all&state=<state>` — plus "this link expires in an hour". If the `strava` feature flag is currently off, one appended clause: "(the strava feature is disabled — you'll be tracked once it's enabled)". Onboarding is **not** gated on the flag.
3. **Athlete** clicks → logs into *their* Strava → Accepts `activity:read_all` → Strava redirects the browser to `https://tugbot.wizards.town/strava/callback?code=…&state=…`.
4. **Bot (in-process callback, `:8643`)**: state lookup → exchange → scope check → athlete fetch → label resolve → upsert → claim → confirm (see §3).
5. **Poll loop (unchanged)**: the new athlete row is picked up on the next 15-min tick — 24h first-enable lookback, first post within 15 min of the athlete's next finished activity.

**Shared-thread semantics** (confirmed): the post target is per-athlete (`strava_athletes.target_thread_id`) — any number of athletes can point at the same thread; each one's posts land there. No exclusivity, no dedupe.

### 2. The `/strava` command (added to the existing strava handler)

- **Shape:** one optional `label` string argument (max 32 runes). No permission checks, no components.
- **Behavior on invoke:**
  1. Sanitize the label: trim, newlines → spaces, cap 32 runes; empty/omitted → NULL (the callback defaults to the Strava first name).
  2. Generate `state` = 16 `crypto/rand` bytes, hex-encoded (128 bits — unguessable).
  3. `INSERT` the pending onboarding row (`state`, `thread_id` = the interaction's channel ID — thread **or** regular channel, both valid post targets, `label`, `expires_at = now + 1h`, `status = 'pending'`).
  4. Reply **in the thread** (visible, mention-suppressed `ChannelMessageSendComplex` with `parse: []` — the house pattern): the authorize link + "this link expires in an hour".
  5. If the `strava` feature flag is off, append the one clause from §1.
- **No dedupe:** a second `/strava` in the same thread issues a fresh state; the unused one expires harmlessly.
- **Cleanup:** the strava handler's tick `DELETE`s expired onboarding rows (one statement per 15-min tick) — the table never grows unboundedly.

**Edge cases, pinned:**
- **Existing athlete re-consents** (scope fix, or from a new thread): the upsert refreshes the token pair, clears `needs_reauth`, moves `target_thread_id` to the newest thread, **preserves the existing label** unless the new command supplied an explicit one.
- **Link clicked after the 1h expiry:** the state row is expired → 404 page ("expired — run /strava again"); the stale row is deleted on sight. No insert.
- **Bot down at consent time:** the redirect 502s (caddy → `:8643` dead); the athlete re-runs `/strava` and retries (Strava codes are short-lived; a fresh link is the recovery).
- **Thread deleted before consent completes:** the upsert still lands the athlete row with the dangling `thread_id`; later posts fail with the existing "post failed (no retry; activity is 'posted')" semantics — operator fixes `target_thread_id` (one UPDATE). Declared, accepted.
- **App at the 10-athlete cap:** Strava rejects the consent itself (the athlete sees Strava's error); nothing lands. Check the dashboard before onboarding the 10th.

### 3. The callback (in-process — decision 0012)

- The strava handler exposes `OnboardingHandler() http.Handler` — a mux serving **only** `GET /strava/callback` (any other path/method → 404). `main.go` wires it as a new errgroup goroutine running `http.Server{Addr: ":8643"}` (house loop shape: `ctx.Done()` → graceful `Shutdown`; the port is a code constant, mirroring the MCP's `:8642`).
- **Handler flow** for `?code=…&state=…`:
  1. **State lookup**: missing/unknown/expired → 404 page ("unknown or expired link — run /strava again"); expired rows deleted on sight.
  2. **Exchange** the code (`POST https://www.strava.com/oauth/token`, `grant_type=authorization_code`, client_id + secret from config). Failure → mark the row `failed`, page: "authorization failed — run /strava again". (Marking `failed` = no retry: the code is likely dead; a fresh `/strava` is the recovery.)
  3. **Scope check**: the exchanged token's `scope` must include `activity:read_all` — otherwise `failed` + page ("consent lacked activity:read_all — use the link /strava posts").
  4. **Athlete fetch**: `GET /api/v3/athlete` with the new access token → id + first name. Failure → `failed` + page.
  5. **Label resolve**: the command's label if non-NULL, else the first name.
  6. **Upsert** the athlete row: `INSERT … ON CONFLICT (strava_athlete_id) DO UPDATE SET access_token, refresh_token, token_expires_at, needs_reauth = FALSE, target_thread_id = EXCLUDED.target_thread_id, label = COALESCE(EXCLUDED.label, strava_athletes.label)` — the §2 semantics in one statement.
  7. **Claim** the onboarding row: `UPDATE … SET status = 'done' WHERE state = $1 AND status = 'pending'` — after the exchange succeeds. Replay-safe: a replayed code fails the exchange at Strava; a double-claim is a no-op; the upsert is idempotent.
  8. **Confirm** in the thread on the bot's own session (mention-suppressed `parse: []`): first-time → `✅ {label} is now tracked — finished runs and rides will post here.`; existing athlete → `🔄 {label}'s Strava authorization was refreshed.`
  9. **Render**: 200, "Done — {label} is set up. You can close this tab."
- **Failure semantics collapse to one liveness**: bot up ⇒ callback works and the confirm lands; bot down ⇒ 502, the athlete re-runs `/strava` and retries.
- **Security model** (the callback is unauthenticated and caddy-fronted — the whole threat surface):
  - **State**: 128-bit random, required — no state → 404. An attacker can't target a flow they didn't initiate.
  - **Code**: single-use at Strava + the atomic claim → replay (re-GET the redirect URL, or reuse the code) is a no-op at worst.
  - **Throttle**: in-memory per-IP counter (10/min → 429; resets on restart — anti-hammer, not a security boundary).
  - **Secrets**: never in the response (the code is echoed — single-use and useless without the server-side secret; tokens are never rendered). The query is `html.EscapeString`-escaped in the page.
  - **Logging**: `slog` lines for completed/failed onboardings (state, thread, label, reason) — no tokens, no codes.

### 4. Deployment, caddy, docs

- **No new unit, no new env vars, `update-tugbot` unchanged** — the listener lives in the existing `tugbot-go.service` (the bot binary carries it); client_id/secret are already in `.env`; the port and the public redirect_uri are code constants.
- **Caddy** (`tugbot.wizards.town` block) — scope the surface to exactly the one path, flip the target:
  ```
  tugbot.wizards.town {
      import security_headers
      handle /strava/callback {
          reverse_proxy tugbot.cherub.casa:8643 { import proxy_headers }
      }
      handle { abort }
  }
  ```
  (today the block proxies the whole domain to `:8080`; the new block serves one path and 404s everything else.)
- **Sidecar retirement:** `systemctl disable --now strava-callback` (its source was ad-hoc on the box, never in the repo — nothing to port).
- **Rollout order:** `update-tugbot` (pull → migrate `000007` → build → restart, listener up on `:8643`) → caddy switch (the only user-visible change; the URL is unchanged) → disable the sidecar.
- **Migration `000007`** — `strava_onboardings`:
  ```sql
  CREATE TABLE strava_onboardings (
      state      text PRIMARY KEY,
      thread_id  bigint NOT NULL,
      label      text,
      status     text NOT NULL DEFAULT 'pending',  -- pending | done | failed
      created_at timestamptz NOT NULL DEFAULT now(),
      expires_at timestamptz NOT NULL
  );
  ```
  `timestamptz` = the declared deviation (consistent with the strava tables; values are operator-box-local, not Strava epochs). No index (the table holds at most a handful of live rows; the per-tick `DELETE … WHERE expires_at < now()` is trivial).
- **Docs:**
  - `docs/features/strava.md`: Setup § becomes the self-serve flow (run `/strava` in a thread → click → Accept → done) with the manual SQL flow kept as the fallback; Re-auth § gains the re-run-`/strava` path (the upsert refreshes the token and clears `needs_reauth`); infra notes gain the `:8643` listener + the scoped caddy block.
  - `CONTEXT.md`: the **Athlete** entry's "localhost redirect, no public endpoint" clause is refreshed (redirect served at `tugbot.wizards.town/strava/callback`, in-process listener, caddy-proxied; the `localhost` flow stays valid via Strava's independent localhost whitelist); a new **Onboarding** term is added (the one-time consent + insert flow).
- **Decision record:** `docs/decisions/0012-strava-onboarding-callback.md` — the in-process callback is a deliberate deviation from the Strava spec's zero-public-surface constraint (chosen over the standalone sidecar for consolidated liveness + one unit).

## Test plan

- **Command (unit + DB integration, strava package):** label sanitize/default (newlines→spaces, 32-rune cap, empty→NULL); state generation (32 hex, `crypto/rand`); reply content (authorize URL shape with the `/strava/callback` redirect_uri + the flag-off clause); pending-row insert; not gated on the feature flag.
- **Callback (httptest, the `client_test` precedent):** the full matrix — valid code+state → upsert + claim + confirm; unknown/expired state → 404 + delete; exchange failure → `failed` + no retry; scope-missing → `failed`; the upsert conflict semantics (new athlete insert; existing → token refresh + thread move + label preserved; explicit label overrides).
- **Pure helpers (unit):** label resolve, scope check, page rendering (escaped query).
- **Selftest:** the new 4-string gate — `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback constructed` (3 strings in `cmd/tugbot/main.go` + `AGENTS.md` — the thirteen→fourteen precedent, verbatim shape); the handler is constructed network-free (build the mux, don't `Listen`).
- **Mention suppression:** pin the confirm's payload (`parse: []` non-nil empty slice — the `TestMentionSuppressionPayload` precedent).

## Out of scope (declared)

- Per-athlete removal (stays a manual `DELETE` — FK cascade, runbook).
- Any change to the poll loop, the seen-table mechanics, or the post format.
- Webhooks (still deferred; additive later).
