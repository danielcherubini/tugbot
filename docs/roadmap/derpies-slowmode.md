---
status: approved
done-when: A bot restart with the deployed binary deletes the 3rd+ single-token post by a gated author per channel within any rolling 30 s, before the fast path, with zero pi asks, and logs a `derpies_decisions` row with `path = 'slowmode'` (NULL score/threshold/word, `deleted = true`, `learned = false`). Posts with ≥2 tokens, 0-token posts, edits, non-gated authors, and non-guild messages run the full flow exactly as before. The derpies feature flag off disables the gate. Full gate green: `go build ./... && go vet ./... && gofmt -l . (silent) && make lint && go test ./... -count=1`, the DB-gated run under `make db-up` with `TUGBOT_TEST_DATABASE_URL`, and `go run ./cmd/tugbot --selftest`.
---

# Derpies single-slowmode gate

## Background

A gated derpies author (`TUGBOT_DERPIES_USER_IDS`) is fast-signalling channels with one-word posts (observed live on 2026-09-19: 114 → 209 decision rows in ~30 min, 2–5 s apart, all scored 2–25/50). The flow deliberately has no per-author rate limiting — "Burst amplification: there is no per-author coalescing or cooldown … rate limiting is out of scope per the spec" — so every burst word costs a full pi ask (300 s deadline flow) plus image leg work. Decision 0011 and the `Slowmode gate` CONTEXT.md entry record this feature's ruling.

**The rule (as one law):**

> A gated author's **single-token post** (raw whitespace split, exactly 1 token), **per (author, channel)**: the **3rd+ within any rolling 30 s window is deleted immediately** — zero pi asks, zero fast-path SELECT, zero image handling — and every pass-through single-token post is **counted regardless of flow outcome**. Posts with ≥2 tokens are uninvolved (no counting, no gating). Edits are out of scope (v1). The constants (2 / 30 s / 1 token) are v1 code constants.

## Scope

**In scope:**
- `internal/handlers/derpies` — the message-flow gate + counter + gate-delete + gate record.
- `migrations/0011_derpies_slowmode_path.{up,down}.sql` — the only DDL: extend `derpies_decisions.path` from `CHECK (path IN ('fast','slow'))` to `CHECK (path IN ('fast','slow','slowmode'))`; down restores the 2-value CHECK.
- `docs/` — ADR 0011 (done), CONTEXT.md `Slowmode gate` entry (done), this spec.

**Out of scope (YAGNI / deferred):**
- Edits (GuildMessageUpdate re-judgement) — judged normally; document the gap (the edit-bypass is a documented, accepted limitation for v1).
- Dials / config table / slash command — none; the feature rides the existing `derpies` feature flag only.
- Warning responses, reactions, mention/gulag involvement — the bot's silence axiom holds; the only action is still `DeleteMessage`.
- DB-backed counter — rejected (decision 0011 / approach B); in-memory state that resets on restart is an accepted v1 trade (a burst is a ≤30 s event; bot restarts are rare).

## Design (approved sections)

### S1 — Gate and delete

1. **Position:** inside the message flow, after the feature gate / guild guard / author-ID checks, **before** the fast path. Nothing else moves; edit and nickname flows untouched.
2. **Gateable:** `len(strings.Fields(m.Content)) == 1` — a post of exactly one raw whitespace-split token.
3. **Check-then-judge:** prune the (author, channel) queue to the last 30 s.
   - **≥2 recent** → over: `DeleteMessage` immediately (zero pi asks, zero list SELECT, zero image handling), write the decision row, and **return** — no fast path, no slow path.
   - **≤1 recent** → append its timestamp and fall through to the full normal flow (fast path → image → pi ask → learn/delete).
4. **Delete failure** (429/403/already deleted): best-effort, `slog` logged (module derpies), the row is still written with `deleted = true` — the same error discipline as the existing fast- and slow-path deletes.
5. **No bot response, no reaction, no warning** — the only action is `DeleteMessage`.
6. **Constants:** 2 posts / 30 s window / 1 token — code constants, no dials, no config.

### S2 — Counter state (in-memory)

- A new field on the existing `Derpies` struct: `rateMu sync.Mutex` + `rateMap map[[authorID, channelID]][]time.Time — exactly the existing `dials` in-memory map pattern (single lock, no goroutine, no timers).
- **Append rule:** every pass-through single-token post has its timestamp appended — counted **regardless of flow outcome** (the window counts *posts*, not *deletes*).
- **Prune:** lazy on each access — drop entries older than 30 s, delete empty keys. No background ticker. A rejected (gated) post is **not** appended.
- **Growth bound:** gated authors come from an explicit operator env list (≤3); per-channel queues ≤2 entries. The map stays at a dozen or so entries. Trivial.
- **Restart loss:** the window starts empty; a burst straddling a restart sees a fresh 2. Accepted for v1 — see decision 0011.

### S3 — Audit trail

- **Every gate-delete is logged.** On over-limit the flow writes its own `derpies_decisions` row before returning:
  - `path = 'slowmode'` (new path value)
  - `score = NULL, threshold = NULL, word = NULL, learned = false, deleted = true, reject_reason = NULL` — the delete is *not* score-driven; the path column carries the reason.
- **DDL:** migration 0011 extends the `path` CHECK to `IN ('fast','slow','slowmode')`. This is the feature's only schema change. Down restores `IN ('fast','slow')`.
- Fast/slow rows are unaffected. The MCP layer (`read_derpies_decisions`) passes the path value through unchanged — a new value in the payload is not an error (regression test only).

### S4 — Boundaries

- **Not gateable (full flow, as today):** 0-token (attachment-only/empty) posts, posts with ≥2 tokens, non-gated authors, non-guild posts, `derpies` feature flag off.
- **Count is posts, not outcome:** a single-token post that was fast-path deleted (e.g. a lone `sw1ft`), one that was judged and slow-path deleted, and one that was left — all, if pass-through, are counted. The 3rd single-token post is gated regardless of the fate of the prior ones.
- **Per-(author, channel):** two channels get independent windows.
- **Delete failure:** best-effort log + row recorded `deleted = true` (S1.4).
- **Restart:** window starts empty (approved, decision 0011).
- **Concurrency:** the gate is strictly local to the flow and reads `m.Content` before the fast path; no cross-feature locking.

### S5 — Constants and toggle

- Constants in the handler package: `maxPosts = 2`, `window = 30 * time.Second`, `maxTokens = 1` — test-settable, hardcoded in production. **No dials, no config table, no slash command.** The entire feature rides the existing `derpies` feature flag; the only "off" is that flag (or removing the gated-author env var).
- No change to the handler constructor signature or `cmd/tugbot` wiring — state lives on the existing `Derpies` struct and is keyed by *content* (a trivial, bounded map).
- The `MessageCreate` comment declaring rate limiting "out of scope per the spec" is updated to point to this gate + decision 0011.

### S6 — Test plan (TDD, one RED per case)

1. **Gate core (fake store + scripted clock):** 2 single-token posts inside 30 s → 3rd is gate-deleted (no fast-path SELECT call, no pi asks, row `path='slowmode'`, NULL score/threshold/word, `deleted=true`, `learned=false`).
2. **Sliding-edge correctness:** posts at t=0, t=29 s; 3rd at t=30.5 s → **not** deleted (t=0 pruned), appended as #1.
3. **Outcome-independent counting:** 1 single-token post that is fast-path deleted (seed word), then 2 more single-token posts → 3rd is still gated; also 5 posts of ≥2 tokens in 10 s → **zero** gating, all fully judged.
4. **Exclusions:** 0-token attachment posts, non-gated author, feature off, non-guild → gate does not fire; single-token post by a non-gated author is fully judged.
5. **Delete failure:** fake DELETE returns an error → no panic, row recorded `deleted=true`, later gate-hits still work.
6. **Per-(author, channel) isolation:** 2 posts to channel A + 2 to channel B → 3rd to A is gated, 3rd to B is **not**.
7. **Migration 0011:** up/down replay (the dbmigrate runner test + migration-file tests); before the migration, a `path='slowmode'` insert fails the CHECK; after, it passes.
8. **MCP pass-through (regression, no code change):** a `slowmode` path row passes through `read_derpies_decisions` unchanged.
9. **Restart semantics:** new handler ⇒ empty window (one assertion, test 1's context).

1–9 are unit-level with fake store/seams; where the DB-gated integration pattern applies (the derpies integration tests under `TUGBOT_TEST_DATABASE_URL`, `-p 1`), the migration-replay check rides it. `go run ./cmd/tugbot --selftest` must stay green (the handler is constructed as before; the 14-handler line holds).

## Files (expected)

- `internal/handlers/derpies/derpies.go` — struct field (mutex + map), gate helper, flow hook (S1.1 position), constants, comment update (S5).
- `internal/handlers/derpies/derpies_test.go` (or the package's existing test layout) — cases S6.1–6, S6.9.
- `migrations/0011_derpies_slowmode_path.up.sql` / `.down.sql` — the CHECK extension (S6.7, DB-gated: `make db-up` + `TUGBOT_TEST_DATABASE_URL`).
- `internal/dbmigrate` — its migration-file test picks up 0011 (pattern: existing numbered migrations; verify the runner test's file list/fixture pattern).
- `docs/decisions/0011-derpies-single-slowmode.md` (done), `CONTEXT.md` (done), this spec.

## Definition of done

See front matter.
