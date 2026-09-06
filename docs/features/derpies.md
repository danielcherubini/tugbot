---
status: live
last-verified: 2026-09-06
verified-by: `make selftest` prints "selftest: Discord session and all twelve handlers constructed" (exit 0); pinned-URL rehearsal `DATABASE_URL=postgres://postgres:postgres@localhost:5432/tugbot make migrate` applied `000002_derpies_gimmicks` (idempotent on re-run); `psql` shows the five seed words (all source='seed') and the `derpies` flag row
---

# Derpies filter

The derpies filter is a passive handler (the twelfth, `internal/handlers/derpies`) that silently deletes Derpie's gimmick posts. It never sends a bot message, reaction, or `#the-gulag` notification — `DeleteMessage` is the flow's only outgoing REST call, and every failure path is log-only.

## Gates (checked in this order)

1. Feature flag `derpies` (`features.IsEnabled` silent flavor: off on any error, including a missing row).
2. Guild guard (DMs are ignored).
3. Author-ID gate — the checked `core.DiscordID` conversion of the author ID must hit `Config.DerpiesUserIDs` (parsed from `TUGBOT_DERPIES_USER_IDS`, comma-separated, malformed parts skipped — same semantics as `SLOW_USER_IDS`).

## Fast path

Per message, the handler fetches `derpies_gimmicks` (a plain list SELECT — the table has no index, a seq scan is fine at this size), tokenizes the content (lowercase, `strings.Fields`, edge-punctuation trim), and deletes on any exact token hit — no pi RPC ask.

## Slow path (learning)

A miss sends ONE pi RPC ask (the 300 s deadline lives in the pi RPC package; the event thread is never held — each message spawns its own flow goroutine). The prompt fences the raw message in `<<<UNTRUSTED MESSAGE` blocks, injects the known-word list (sorted, byte-stable), and the pi RPC always appends the anti-injection system fallback. A valid verdict is the first non-empty line: `GIMMICK:<word>` (case-insensitive, colon required) or `CLEAN`; anything else is `unknown` (log, no action).

Before a verdict learns, two sanity gates must both pass: `wordValid` (`^[a-z0-9]{2,32}$`) AND the word appeared as a token of the submitted message. A hallucinated or coerced word can never enter the list — at most the filtered user can teach words he typed himself. Learned words are `INSERT ... ON CONFLICT (word) DO NOTHING` (idempotent upsert over the `UNIQUE (word)` constraint, `source='llm'`), then the message is deleted. A delete failure after a successful add leaves the word learned (the next occurrence is a fast hit).

## Data

`derpies_gimmicks` (migration `000002_derpies_gimmicks`): `id`, `word varchar(64) UNIQUE`, `source varchar(8) DEFAULT 'seed'`, `created_at`. Seeded words: `swift`, `zswift`, `bike`, `give`, `buy` (all `source='seed'`). The `features` table gains a `derpies` row via `ON CONFLICT (name) DO NOTHING` (lives already in prod; fresh DBs seed it OFF).

## Known limitations (intentional, per spec — delete-only, no rate limiting)

- **Unicode respellings evade both paths by design**: `wordValid` rejects non-`[a-z0-9]` and tokenization lowercases ASCII-only, so a pure-unicode respelling (e.g. `swïft`) never fast-matches and any verdict naming it is rejected by the validity gate (logged, not learned). The next ASCII respelling catches it.
- **Burst amplification**: no per-author coalescing or cooldown — N near-identical novel posts from a filtered user yield N serialized pi asks (the single pi RPC request channel is shared with the mention handler) plus N list SELECTs. A learned word's repetition is fast-path only; coalescing/rate limiting is out of scope per the spec.
- `make test-db` invalidates the migration tracker on the shared compose DB (see the `NOTE` on the Makefile target) — recreate with `docker compose down -v` or `DROP` the test-left objects before the next `make migrate`/`make setup`.

## Production acceptance (done-when)

With `TUGBOT_DERPIES_USER_IDS=163055057254875136` in `.env` and the `derpies` flag enabled in prod: (1) a message by that user containing a seeded gimmick word (e.g. `sw1ft`) is deleted within seconds with NO bot message, reaction, or gulag involvement; (2) a message containing a NEW respelling (e.g. `zswiftf`) that no list entry matches is sent to the pi RPC, and on a `GIMMICK:<word>` verdict the word is persisted to `derpies_gimmicks` with `source='llm'` and the message is deleted; (3) any later post containing `zswiftf` is deleted by the fast path with zero pi RPC asks.

## Out of scope (by spec)

- The Rust `derpies` module's reaction-removal behavior (dead; superseded by this feature's delete-only behavior).
- Any notification to `#the-gulag`, any bot message, any reaction.
- Per-user offense counters, rate limiting, or `SlowUserIDs`-style auto-gulag for derpies.
