# tugbot

A Discord bot for a crappy discord — the Go port of tugbot-rs. Shares the Rust bot's Postgres DB, bot token, Application ID, and guild roles; it replaces the Rust binary on the same host, not alongside it.

## Language

**App**:
The single struct holding `*discordgo.Discordgo`, `*pgxpool.Pool`, `*PiRpc`, and `Config`. Replaces Serenity's three TypeMap keys (`DbPoolKey`, `ConfigKey`, `PiRpcKey`) — every handler is a constructor that takes `*App`.
_Avoid_: TypeMap, context plumbing, DI container

**Re-scoped port**:
The Go rewrite ports the 11 **live** handler modules/packages of the Rust bot (including the gulag package; the command shapes: 7 slash + 2 message-context-menu + the goku message-update + the reaction voting) and drops dead code: `tiktok` and `elkmen` (already disabled in Rust) and `elon` and `derpies` (defined in Rust but never dispatched from `mod.rs`, and no feature-flag rows in any migration). It is not a 1:1 port; behavior parity is matched to live behavior only. The Go bot additionally introduces the derpies filter as its own original feature: the Rust `derpies` module was a never-dispatched stub, so the Go handler is a NEW feature, not a port (see **Derpies filter**).
_Avoid_: 1:1 port, literal port, full rewrite

**Gimmick word**
An obfuscated token (e.g. `sw1ft`) that Derpie uses to evade the derpies filter's word matching. Persisted in the `derpies_gimmicks` table with `source` distinguishing `seed` (migration-seeded), `llm` (learnt at runtime via the pi RPC verdict), and `manual` (curated at runtime via the `/gimmick` slash command). Matched by lowercase, punctuation-trimmed exact token match — never substring.
_Avoid_: blocklist entry, banned word, filter entry

**Gimmick score**
The LLM's 0–100 judgement of how much of a gimmick a derpies message is — the combo judgement (a harmless emoji alone vs. a soliciting combination). Returned as the `SCORE:` line of the slow-path verdict; the code thresholds it (learn floor, delete threshold). Persisted in the decision record.
_Avoid_: confidence, verdict score, risk score

**Delete threshold**
The operator's live-tunable dial for derpies deletion (T): a message scoring ≥ T is deleted. Stored in `derpies_config.delete_threshold` (single row, default 50), fetched per-message like the prompt and the word list — tunable via SQL/MCP with no deploy.
_Avoid_: cutoff, score limit, delete floor

**Learn floor**
The code-constant confidence floor (L=40) for learning a derpies word: a message scoring ≥ L is a "real trace" whose anchor word is learned (subject to the anchor gate); below L nothing is learned or deleted.
_Avoid_: learn threshold, word floor

**Word-like token**
A derpies token that could be a verdict word: a valid non-digit word (`wordValid`, not `allDigitVerdict`) or a valid non-ASCII shape (`unicodeVerdictShape`). A mention snowflake, a custom-emoji ref, or a pure-emoji token is NOT word-like — a message with no word-like tokens is judged like an image-only post (the anchor gate is skipped).
_Avoid_: valid token, matchable token

**Decision record**
A row in `derpies_decisions`: one per judged message/edit (append-only — an edit of the same `message_id` is a new row). Carries the posted content, the path (fast/slow), the gimmick score, the delete threshold applied, the anchor word, and the `learned`/`deleted`/`reject_reason` outcomes. The input to the improve-over-time loop (curate phrases/words, tune the threshold from observed scores).
_Avoid_: verdict log, audit row, judgement row

**Derpies filter**
The Go bot's own original feature: silently deletes messages from author IDs in `TUGBOT_DERPIES_USER_IDS` via a fast-path token match against `derpies_gimmicks`, falling back on a fast-path miss to a pi RPC `GIMMICK:<word>` / `CLEAN` verdict that persists valid words back into the table. It also re-judges `GuildMessageUpdate` by a gated author — the full flow on the updated content (at most one ask per edit). The slow-path prompt carries the FULL known gimmick list (already in memory from the fast-path fetch) so the LLM pattern-matches respellings against the known family — the list lives in the DB, not in a static skills/ file. The only action is `DeleteMessage` — no bot response, no reaction, no gulag involvement on any message- or edit- path (the nickname flow's action is the member-reset PATCH — see **Nickname reset**). Repeated images are re-judged, never fast-deleted: the 2026-09-08 repeat-image fast delete (flow 4.6 — seen-content cache, zero-ask re-post delete) was REMOVED on 2026-09-10 (operator decision 2026-09-10) — a post carrying an already-judged image (or a re-post of one) goes into a fresh pi ask bounded by the per-ask image guard. The image leg (4.5: attachment + embed download, URL-deduped) is kept; no image post is ever fast-deleted on account of its content having been previously judged (the separate word fast path, which runs before any image handling, is unchanged).
_Avoid_: mod action, anti-spam handler, derpies handler, repeat-image fast delete, seen-image cache

**Nickname reset**
The derivative of the Derpies filter on `GuildMemberUpdate`: when a gated derpies user changes their per-server nickname, the new nickname is judged the same way as a message (fast-path token match against `derpies_gimmicks`, then the slow pi-RPC verdict); a GIMMICK verdict resets the nickname to the fixed neutral name `Derpies` (`PATCH .../members/{member} {"nick":"Derpies"}` — a null-clear is not used: it would expose the global display name, which the bot cannot modify); the reset action fires on any parseable GIMMICK verdict (the learning gate — wordValid + folded-token-in-nick — now gates learning only). Resets are per-member coalesced: at most one reset attempt per 60-second window (success or failure marks the window), and the bot's own reset is never re-judged (a successful reset records the reset value in the cache BEFORE the gateway echo arrives). The `derpies` feature flag gates both the message flow and this one.
_Avoid_: nickname ban, display name reset, member rename

**Slowmode gate**
The derpies filter's anti-burst extension: the message flow gates the gated author's single-token posts (raw whitespace split, exactly 1 token) per (author, channel) against a rolling 30 s window; the 3rd+ inner post is deleted **before the fast path** — zero pi ask, zero fast-path SELECT, zero image handling, action = `DeleteMessage`. The counter is in-memory (mutex + map on the handler; lazy pruning on access; resets to empty on bot restart — v1-accepted since a burst is a ≤30 s event); edits (re-judgement flow) are out of gate scope; the constants (2 posts / 30 s / 1 token) are v1 code constants, not dials. Every gate-delete writes a decision row with `path = 'slowmode'` (score/threshold/word and reject_reason NULL — the delete is not score-driven; the path column is the reason). See decision 0011
_Avoid_: rate limiter, anti-spam, cooldown, throttle, post-rate gate

**Baseline migration**:
The single migration file representing the entire pre-cutover schema (diesel history treated as settled fact). Go-owned migration history begins with it; the Go runner stamps it applied on first run over a live DB without executing the DDL.
_Avoid_: schema snapshot import, diesel history

**Cutover**:
The production switchover: stop the Rust systemd unit, point the unit at the Go binary, start. The Rust binary and old unit file are kept ~2 weeks for one-click rollback. There is no shadow/gray period — the token allows a single gateway connection.
_Avoid_: migration, gray rollout, shadow bot, blue-green

**MCP layer**:
The in-process `internal/mcp` package: an always-on, zero-config Streamable-HTTP MCP server embedded in the tugbot process that shares the bot's single `*discordgo.Session` (the embedded-provider pattern — no second session, since the token allows one gateway connection). Agents (pi, Claude, …) connect to `:8642/mcp` and act as the bot.
_Avoid_: MCP sidecar, MCP daemon, MCP plugin

**Bridge tools**:
The five v1 raw-Discord MCP tools: `list_guilds`, `list_channels`, `read_messages`, `post_message`, `react` — thin wrappers over session methods, no Postgres, no Pi.
_Avoid_: MCP tools (unqualified), Discord API tools

**Feature tools**:
Follow-on MCP tools wrapping the bot's feature handlers (feature toggles, gimmick, gulag, ask-pi) — distinct from bridge tools; each needs a small `Invoke`-style public surface on the event-callback-shaped handlers.
_Avoid_: feature MCP, command tools

**Instagram rewrite**:
The Instagram handler's mechanic — the 1:1 port of `instagram.rs` (feature-gated, suppresses the matched message's embeds, posts a new message with only the matched URL, `www.` prefix preserved) **except the target domain**: the Go bot rewrites it to `oginstagram.com`, deliberately diverging from Rust's `kkinstagram.com` (the port's first deliberate Go-vs-Rust divergence).
_Avoid_: kkinstagram rewrite, rewriter

**Athlete**:
A monitored Strava user who authorized the tugbot Strava app through the one-time OAuth consent (the redirect is served at `tugbot.wizards.town/strava/callback` — an in-process listener caddy proxies; the `localhost` flow stays valid via Strava's independent localhost whitelist; see **Onboarding**). Lives as a row in `strava_athletes` (label, Strava id, rotating tokens, `last_polled_at` cursor, nullable per-athlete thread, `needs_reauth`). Growing the set is purely operational (the app is on the 10-athlete tier via the self-serve dashboard upgrade + one onboarding per person — self-serve via **Onboarding**, or the manual runbook SQL) — never a code change.
_Avoid_: user (when about the Strava side), player, authorized user

**Onboarding**:
The one-time consent + insert flow that turns a Strava account into an athlete row. Self-serve: any member runs `/strava` (optional `label` argument) in the thread they want posts to; the bot (strava handler) issues a 128-bit `state` and replies in the thread with the authorize link (scope `activity:read_all`, redirect `https://tugbot.wizards.town/strava/callback`); the athlete consents; Strava redirects their browser to the callback, where the in-process handler (strava package, `:8643` — decision 0012) exchanges the code, verifies the scope, fetches the athlete (id + first name), resolves the label (command arg, else first name), upserts the athlete row (token pair + `token_expires_at` + `needs_reauth = FALSE` + `target_thread_id` = the command's thread — an existing athlete gets the token refreshed, the thread moved to the newest, and the label preserved unless an explicit label was given), and posts a confirm in the thread. Pending states live in `strava_onboardings` (1h TTL, `pending|done|failed`, atomic claim, per-tick expired-row cleanup). Multiple athletes can share one thread (the target is per-athlete). The manual runbook flow (consent + SQL insert) remains the valid fallback.
_Avoid_: signup, registration, athlete setup, OAuth flow (when about the whole flow)

**Push subscription**
Strava's app-level webhook registration: one per app, created with `client_id`/`client_secret` (no athlete token), covering every athlete who has authorized the app. The bot's `:8643` answers its one-time `hub.challenge` verification GET and its event POSTs (`/strava/webhook`); the subscription id is an ops note in the runbook, not bot state. Webhook events are an early trigger only — the 15-min poll remains the backstop (no delivery guarantees: ≤3 attempts, duplicates and losses both documented; the seen table absorbs duplicates).
_Avoid_: webhook (unqualified), subscription (when about the app-level registration)
