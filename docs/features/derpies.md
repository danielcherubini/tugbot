---
status: live
last-verified: 2026-09-06
verified-by: `make selftest` prints "selftest: Discord session and all twelve handlers constructed" (exit 0); pinned-URL rehearsal `DATABASE_URL=postgres://postgres:postgres@localhost:5432/tugbot make migrate` applied `000002_derpies_gimmicks`, `000003_derpies_prompt` and `000004_derpies_cog_words` (idempotent on re-run); `000003` applied on the compose DB, `derpies_prompt` seeded; `psql` shows the fourteen seed words (all source='seed') and the `derpies` flag row; `go test ./internal/handlers/derpies/ -count=1 -short` green — 46 tests (30 pre-existing unchanged + 16 image/referenced)
---

# Derpies filter

The derpies filter is a passive handler (the twelfth, `internal/handlers/derpies`) that silently deletes Derpie's gimmick posts. It never sends a bot message, reaction, or `#the-gulag` notification — `DeleteMessage` is the flow's only outgoing REST call that takes an action (a reply/quote-reply message additionally makes one data GET to fetch the referenced message), and every failure path is log-only.

## Gates (checked in this order)

1. Feature flag `derpies` (`features.IsEnabled` silent flavor: off on any error, including a missing row).
2. Guild guard (DMs are ignored).
3. Author-ID gate — the checked `core.DiscordID` conversion of the author ID must hit `Config.DerpiesUserIDs` (parsed from `TUGBOT_DERPIES_USER_IDS`, comma-separated, malformed parts skipped — same semantics as `SLOW_USER_IDS`).

## Fast path

Per message, the handler fetches `derpies_gimmicks` (a plain list SELECT — the table has no index, a seq scan is fine at this size), tokenizes the content into a FOLDED ASCII token space (each token NFD-decomposed, combining marks stripped, lowercased, `strings.Fields`, edge-punctuation trim), and deletes on any exact token hit — no pi RPC ask. The token set is the union of the posted message and, when fetchable, the one-hop referenced message's content — a reply re-quoting a seeded word is a fast-path hit without retyping it (the referenced fetch is one REST GET via the `discordOps` seam). A unicode respelling of a seed word (e.g. `świft`, which folds to `swift`) is a fast-path hit.

## Referenced messages

A reply/quote-reply message (`MessageReference` with a message ID) fetches one hop of the referenced message — a single REST GET via the `discordOps` seam (mention parity; chains are not followed). On fetch failure (already deleted, rate-limited) the flow logs and continues WITHOUT a reference — never aborting the flow. When fetched: the reference's text joins the fast-path token union, its images join the download union, and its content is quoted in the pi ask between the `<<<REFERENCED MESSAGE` / `REFERENCED MESSAGE>>>` markers (the shipped prompt's `{{REF}}` marker substitutes that block) — so a gimmick living only in the quote is both fast-path catchable and LLM-visible.

## Slow path (learning)

A miss sends ONE pi RPC ask (the 300 s deadline lives in the pi RPC package; the event thread is never held — each message spawns its own flow goroutine) — via `AskWithImages` when the message's downloaded images (attachments + embed images, mention-mirror leg, per-URL failure logged + skipped, degrades to a text-only ask when nothing downloads) exist — the shipped prompt's `{{IMAGES}}` marker substitutes that images line — plus the fetched referenced content, when a reply/quote was resolved. The prompt fences the raw message in `<<<UNTRUSTED MESSAGE` blocks, injects the known-word list (sorted, byte-stable), BANS base-word answers (the verdict word must be the as-appears token, not the base/known word), and names the evasion techniques as an instance class (respelling / unicode lookalikes / punctuation-wedged / split / hidden / squeezed / asking-phrasing / quoting / images); the pi RPC always appends the anti-injection system fallback. A valid verdict is the first non-empty line: `GIMMICK:<word>` (case-insensitive, colon required) or `CLEAN`; anything else is `unknown` (log, no action).

Before a verdict learns, the verdict word is FOLDED through the same token fold as the message tokens, and the two-arm sanity gate must pass on the FOLDED word (`GIMMICK:žwift` evaluates as `zwift`): `wordValid` (`^[a-z0-9]{2,32}$`) ALWAYS, AND — when the (union of posted + referenced) text has tokens — the folded word must have appeared as a folded token of that text. A message with NO text tokens at all (image-only) is bounded by `wordValid` alone (decided trade-off: the verdict word may come from image text, and a wrong word can only delete the gated user's own future messages containing that word). A hallucinated or coerced word can never enter the list — at most the filtered user can teach a word he posted himself (text, image, or quote). The stored/learned word is the FOLDED (ASCII) word, so `derpies_gimmicks` stays a pure-ASCII token space; it is `INSERT ... ON CONFLICT (word) DO NOTHING` (idempotent upsert over the `UNIQUE (word)` constraint, `source='llm'`), then the message is deleted. A delete failure after a successful add leaves the word learned (the next occurrence of the respelling is a fast hit).

## Data

`derpies_gimmicks` (migrations `000002_derpies_gimmicks` and `000004_derpies_cog_words`): `id`, `word varchar(64) UNIQUE`, `source varchar(8) DEFAULT 'seed'`, `created_at`. Seeded words: `swift`, `zswift`, `bike`, `give`, `buy` (all `source='seed'`) plus the nine "cog" respellings `cog`, `cogs`, `coggs`, `c0g`, `c0gs`, `coq`, `coqs`, `kog`, `kogs` (all `source='seed'`) — fourteen seed rows total. The `features` table gains a `derpies` row via `ON CONFLICT (name) DO NOTHING` (lives already in prod; fresh DBs seed it OFF).

## Live prompt (DB)

The prompt template lives in the single-row `derpies_prompt` table (migration `000003_derpies_prompt`, `body` + `updated_at`), seeded byte-for-byte with the code default; the flow fetches it per message (one row SELECT — the same per-message-fetch shape as the feature flag and the gimmick list).

Template contract: the literal `{content}` (code wraps the raw message in the `<<<UNTRUSTED MESSAGE` fence) and `{known}` (the sorted known-word list) are MANDATORY — a template missing either is INVALID and never used. `{{IMAGES}}` and `{{REF}}` are OPTIONAL — an absent optional marker is fine, the element is simply omitted. A missing or invalid row falls back to the code-pinned default template — the filter never runs with a broken prompt (a fetch error warns; an invalid-but-present row falls back silently).

Edit path: `psql … UPDATE derpies_prompt SET body = '…';` — the next message picks it up (no deploy, no restart). Rollback = restoring the previous body text.

Blast radius: a prompt edit changes WHAT gets caught, never what may be learned/deleted without a verdict word — the learning gate (`wordValid` on the folded word, + folded-token-in-(posted-+referenced-text) whenever that text has tokens) stays the code-enforced backstop.

## Known limitations (intentional, per spec — delete-only, no rate limiting)

- **Unicode**: Latin-extended unicode respellings now fold (NFD) into ASCII and are caught (fast-path seed hits / LLM-learnable); letters with NO Latin decomposition (e.g. Cyrillic lookalikes) still evade the fast path and are catchable only when the LLM names an ASCII form.
- Image downloads are best-effort — a per-URL failure skips that URL (logged); when nothing downloads the ask degrades to text-only. No image-size/count cap beyond Discord's own message limits (10 attachments / 25 boosted); a 10-image message burns one heavier (base64) pi ask.
- Referenced-message fetch is one hop (mention parity) — chains are not followed; a fetch failure (deleted/rate-limited) degrades to judging the posted message only, so a gimmick that lives ONLY in a deleted quote passes through.
- **Burst amplification**: no per-author coalescing or cooldown — N near-identical novel posts from a filtered user yield N serialized pi asks (the single pi RPC request channel is shared with the mention handler) plus N list SELECTs. A learned word's repetition is fast-path only; coalescing/rate limiting is out of scope per the spec.
- `make test-db` invalidates the migration tracker on the shared compose DB (see the `NOTE` on the Makefile target) — recreate with `docker compose down -v` or `DROP` the test-left objects before the next `make migrate`/`make setup`.

## Production acceptance (done-when)

With `TUGBOT_DERPIES_USER_IDS=163055057254875136` in `.env` and the `derpies` flag enabled in prod: (1) a message by that user containing a seeded gimmick word (e.g. `sw1ft`) is deleted within seconds with NO bot message, reaction, or gulag involvement; (2) a message containing a NEW respelling (e.g. `zswiftf`) that no list entry matches is sent to the pi RPC, and on a `GIMMICK:<word>` verdict the word is persisted to `derpies_gimmicks` with `source='llm'` and the message is deleted; (3) any later post containing `zswiftf` is deleted by the fast path with zero pi RPC asks; (4) an image attachment is downloaded (`Downloading image` lines) and joins the pi ask via `AskWithImages` — a `GIMMICK:<word>` on an image-only message deletes it and LEARNS the word (`wordValid` gate only), while a word valid but absent from a text+image message's text AND its (quoted) reference is neither deleted nor learned (the verdict gate rejects — doing nothing, logged); (5) a reply/quote re-quoting a seeded word deletes the CURRENT message on the fast path without retyping it, and a `GIMMICK:<word>` where the word appears only in the quoted message learns the word; (6) a referenced-message fetch failure (deleted/rate-limited) degrades to judging the posted message only — logged, no flow abort.

## Out of scope (by spec)

- The Rust `derpies` module's reaction-removal behavior (dead; superseded by this feature's delete-only behavior).
- Any notification to `#the-gulag`, any bot message, any reaction.
- Per-user offense counters, rate limiting, or `SlowUserIDs`-style auto-gulag for derpies.
