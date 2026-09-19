---
status: committed
done-when: A bot restart with the deployed binary deletes the 3rd+ single-token post by a gated author per channel within any rolling 30 s, before the fast path, with zero pi asks, and logs a `derpies_decisions` row with `path = 'slowmode'` (NULL score/threshold/word, `deleted = true`, `learned = false`). Posts with ≥2 tokens, 0-token posts, edits, non-gated authors, and non-guild messages run the full flow exactly as before. The derpies feature flag off disables the gate. Full gate green: `go build ./... && go vet ./... && gofmt -l . (silent) && make lint && go test ./... -count=1`, the DB-gated run under `make db-up` with `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...`, and `go run ./cmd/tugbot --selftest`.
---

# Derpies single-slowmode gate — Plan

**Goal:** An in-handler sliding-window gate deletes a gated author's 3rd+ single-token post per channel inside any rolling 30 s — before the fast path, zero pi asks — and logs the delete with `path = 'slowmode'`.
**Architecture:** One mutex-guarded `map[string][]time.Time` lazily initialized on the existing `Derpies` struct (key `authorID + "|" + channelID`), checked in `flow()` between the author-ID gate and the decision-record defer; a gate hit deletes via the existing `discordOps.deleteMessage` seam, writes its own `decisionRecord` (rejecting the deferred one's position), and returns. One new migration (000007) extends the `path` CHECK constraint.
**Tech Stack:** Go, discordgo events, pgx (only the migration test touches PG), the package's existing fake seams.

**Spec source of truth:** this plan replaced the approved spec content (the spec sections S1–S6 are preserved verbatim below the tasks). **Naming correction from the spec text:** the spec called the DDL migration "0011" — the real migrations directory ends at `000006`, so the task uses **`000007_derpies_slowmode_path.{up,down}.sql`**. Branch: create from `main` (gitflow-branching), PR back to `main`, squash merge.

---

### Task 1: Migration 000007 — extend the `path` CHECK constraint

**Context:** `derpies_decisions.path` is `CHECK (path IN ('fast', 'slow'))` (created unnamed by `migrations/000005_derpies_score.up.sql`, so PostgreSQL's auto-name is `derpies_decisions_path_check`). Gate-deletes need a third value, `slowmode`, and this is the feature's only DDL. The up file must be **re-run-idempotent** on the shared DB-gated test database: it drops the constraint under its known auto-name and re-adds it under the *same* name with the 3-value check (drop+re-add with an identical name is a no-op content-wise on a second run). The down file does the reverse (drops and re-adds the 2-value check under the same name). No other table is touched.

**Files:**
- Create: `migrations/000007_derpies_slowmode_path.up.sql`
- Create: `migrations/000007_derpies_slowmode_path.down.sql`
- Test: `internal/handlers/derpies/derpies_integration_test.go` (append; it already has `TestMigration000002AppliesAndSeeds` as the pattern to mirror)

**What to implement:**

`000007_derpies_slowmode_path.up.sql` — exactly:
```sql
-- Extend the derpies_decisions.path CHECK with 'slowmode' (the single-slowmode
-- gate's audit value; decision 0011). Named drop+re-add under the original
-- auto-name keeps the file re-run-idempotent on the shared test DB.
ALTER TABLE public.derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE public.derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow', 'slowmode'));
```

`000007_derpies_slowmode_path.down.sql` — exactly:
```sql
ALTER TABLE public.derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE public.derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
```

Test — `TestMigration000007AppliesAndRollsBack` in `internal/handlers/derpies/derpies_integration_test.go`, mirroring `TestMigration000002AppliesAndSeeds` (same skip pattern: `testing.Short()` skip, `TUGBOT_TEST_DATABASE_URL` env → default `postgres://postgres:postgres@127.0.0.1:5432/tugbot_test`, `pgxpool.New`, ping-or-skip, `t.Cleanup(pool.Close)`, copy the REAL file to a temp dir, `dbmigrate.Run`):
1. Normalize the precondition (the shared test DB may or may not have it; the table must exist — create it if it doesn't):
```sql
CREATE TABLE IF NOT EXISTS derpies_decisions (
    id bigserial PRIMARY KEY,
    message_id text NOT NULL,
    channel_id text NOT NULL,
    author_id text NOT NULL,
    content text NOT NULL DEFAULT '',
    path text CHECK (path IN ('fast', 'slow')),
    score integer,
    threshold integer,
    word text,
    learned boolean NOT NULL DEFAULT false,
    deleted boolean NOT NULL DEFAULT false,
    reject_reason text,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz DEFAULT now()
);
DELETE FROM schema_migrations WHERE version = '000007_derpies_slowmode_path';
ALTER TABLE derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
TRUNCATE derpies_decisions;
```
2. Run **up** via `dbmigrate.Run` on a temp dir containing only the copied 000007 up file.
3. Assert up effects: `INSERT ... path='slowmode'` succeeds; `path='fast'` succeeds; `path='nope'` **fails** (error). Clean up the inserted rows.
4. Run **down** on a second temp dir (the copied 000007 down file — re-add the tracker row pointing at down first? No: `dbmigrate.Run` applies the file as-is; for the DOWN check, replicate what the runner does for down: just execute the down file's SQL directly via `pool.Exec(ctx, downSQL)` is NOT how dbmigrate applies — use `dbmigrate.Run` on a temp dir holding the down file renamed to its up-name `000007_derpies_slowmode_path.up.sql` and a pre-inserted marker in `schema_migrations` is NOT how the 000002 test works either — inspect the 000002 test for the exact down convention if present; if the 000002 test only exercises up, exercise down by executing the down file's two statements directly with `pool.Exec` (that is a faithful check of the file's content) and clear nothing else.
5. Assert down effects: `path='slowmode'` now **fails**; `path='fast'` succeeds. Clean up.
6. **Leave the DB in the UP state** (re-run the up statements the same way, or re-run step 3's `dbmigrate.Run`) so later DB-gated runs in the same package see the 3-value CHECK.

**Steps:**
- [ ] Write `TestMigration000007AppliesAndRollsBack` first (RED without the files: the test FATALs on reading the missing files — that is the expected failure)
- [ ] Run `go test ./internal/handlers/derpies/ -run TestMigration000007 -v` (no PG or short) — expect skip; run under `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot` — expect the missing-file fatal
- [ ] Create the two migration files
- [ ] Re-run the DB-gated test — expect pass
- [ ] `gofmt -l internal/handlers/derpies/` → silent
- [ ] Commit: `derpies: migration 000007 adds the slowmode decision path (DB-gated test)`

**Acceptance criteria:**
- [ ] Up file: `path IN ('fast','slow','slowmode')`, named constraint `derpies_decisions_path_check`, re-run-idempotent
- [ ] Down file: restores `IN ('fast','slow')`, re-run-idempotent
- [ ] The DB-gated test passes (and skips cleanly without PG / under `testing.Short()`)

---

### Task 2: The gate — state, check, delete, record (core S1–S3 behavior)

**Context:** This is the feature's core: an in-memory per-(author, channel) sliding window that, for a gated author's single-token post, deletes the 3rd+ post inside a rolling 30 s before the fast path (zero pi asks, zero `listGimmicks`, zero image work), counts pass-through single-token posts regardless of flow outcome, and writes a `path='slowmode'` decision row (NULL score/threshold/word, `learned=false`, `deleted=true`, NULL reject_reason). The state is lazily initialized (nil map → create, under the mutex), so `New()` and `newTestDerpies` need NO changes. The gate sits between the `slog.Info("derpies message from filtered user", ...)` line (section 3, author gate) and the `dec := &decisionRecord{...}; defer h.recordDecision(...)` block — a gate hit must NOT fall through to the deferred record (it would write a second, wrong row); the gate writes its own row via the existing `recordDecision` helper.

**Files:**
- Modify: `internal/handlers/derpies/derpies.go`
- Test: `internal/handlers/derpies/derpies_slowmode_test.go` (new file — keeps the gate tests out of the 3k-line `derpies_test.go`)

**What to implement (in `derpies.go`):**

1. Constants near the other package constants (top of the file, with the other consts):
```go
// The single-slowmode gate (decision 0011): the 3rd+ single-token post by a
// gated author per channel within a rolling window is deleted before the fast
// path. V1 code constants — no dials, no config.
const (
	slowmodeMaxPosts = 2
	slowmodeWindow   = 30 * time.Second
)
```

2. Struct fields (append to the existing `Derpies` struct, after `nickMu`):
```go
	// rateWindow — the single-slowmode gate's per-(author, channel) rolling
	// window: timestamps of the gated author's pass-through single-token
	// posts (COUNTS POSTS, NOT OUTCOMES — a fast-path-deleted single token
	// still counts). Key: authorID + "|" + channelID (the lastNick key
	// pattern). Lazily initialized (nil map → create, under rateMu), so
	// New()/newTestDerpies need no changes; resets to empty on bot restart
	// (accepted, decision 0011).
	rateWindow map[string][]time.Time
	rateMu     sync.Mutex
```

3. The gate method (place immediately before `func (h *Derpies) flow(`):
```go
// gateSlowmode — the single-slowmode check (S1): called after the
// author-ID gate and BEFORE the decision-record defer. Returns true when
// it deleted the post (the flow must return — no fast path, no slow
// path, no pi ask). Ineligible: any post of a different shape (0-token
// or ≥2-token) just falls through. Within-window overflow (≥
// slowmodeMaxPosts recent) deletes via the ops seam (best-effort on
// failure — logged, row still written) and records its OWN
// path='slowmode' row. A pass-through appends its timestamp; an
// overflow DELETE does not (only pass-through posts count).
// Window membership: now.Sub(ts) <= slowmodeWindow (inclusive — a post
// exactly 30 s old is still in the window).
func (h *Derpies) gateSlowmode(ctx context.Context, m *discordgo.Message) bool {
	if len(strings.Fields(m.Content)) != 1 {
		return false
	}
	now := h.clock()
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	if h.rateWindow == nil {
		h.rateWindow = map[string][]time.Time{}
	}
	key := m.Author.ID + "|" + m.ChannelID
	var kept []time.Time
	for _, ts := range h.rateWindow[key] {
		if now.Sub(ts) <= slowmodeWindow {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= slowmodeMaxPosts {
		if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
			slog.Error("derpies gate delete failed", "module", module, "message", m.ID, "error", err)
		}
		h.recordDecision(ctx, m, &decisionRecord{
			MessageID: m.ID, ChannelID: m.ChannelID, AuthorID: m.Author.ID,
			Content: m.Content, Path: strPtr("slowmode"),
			Learned: false, Deleted: true,
		})
		return true
	}
	h.rateWindow[key] = append(kept, now)
	return false
}
```
(Note: `strings` import — check whether `derpies.go` already imports `strings`; if not, add it. `now := h.clock()` — in tests `h.clock` is pinned by the gate tests; production `New()` already wires `clock: time.Now`. `strPtr` already exists in `derpies.go`.)

4. The flow hook — in `flow()`, immediately after the line
```go
	slog.Info("derpies message from filtered user", "module", module, "user", m.Author.ID, "guild", m.GuildID)
```
insert:
```go
	// 3.5 Single-slowmode gate (decision 0011): the 3rd+ single-token
	// post within 30 s is deleted here — before the fast path, zero pi
	// asks, zero list fetch, zero image leg. No deferred record for a
	// gate hit (the gate writes its own path='slowmode' row).
	if h.gateSlowmode(ctx, m) {
		return
	}
```
**DO NOT** move, delete, or renumber any other section. The edit flow (`edits.go`) and nickname flow (`nicknames.go`) are untouched.

5. Comment update (S5): the `MessageCreate` doc comment's line
"The message flow (gates → fast path → images → slow path → learn/delete). The create and edit paths run the identical flow" stays, but the line "Burst amplification: there is no per-author coalescing or cooldown — N novel posts from a filtered user yield N serialized pi asks (the pi RPC queue is shared with the mention handler); rate limiting is out of scope per the spec." is replaced by:
```go
// Burst amplification note (superseded, 0011): the single-slowmode gate
// (above, decision 0011) deletes a gated author's 3rd+ single-token post
// per channel inside 30 s before the fast path (zero pi asks). LONG posts
// (≥2 tokens) are still unthrottled — N novel long posts still yield N
// serialized pi asks (the pi RPC queue is shared with the mention handler).
```

**Tests (in `derpies_slowmode_test.go`, package `derpies`)** — construction pattern from the existing tests:
```go
store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}, prompt: defaultPromptTemplate}
ops := &fakeOps{}
pi := &fakePi{verdict: "CLEAN", score: 3, threshold: 50}
h := newTestDerpies(store, ops, pi)
h.clock = func() time.Time { return tt }   // tt is a package-level var moved per step
```
(Check `fakePi`'s actual fields in `derpies_test.go` — it has `asks int` and a verdict/score shape used by the existing slow-path tests; mirror one of them.)

- `TestGateDeletesThirdSingleToken` (S6.1): pinned clock `base`, three fresh `derpMsg`-style messages (distinct `ID` fields, content "cat"/"has"/"hit"). Post them in order via `h.flow(m)`. Assert on the 3rd: `len(ops.deleted) == 1` with `ops.deleted[0]` = the 3rd post's `{c1, id}`; `store.listCalls == 2` (A and B both passed the gate and ran the fast path's list fetch; C never reached it); `pi.asks == 2`; `store.decisions` contains the C row with `Path != nil` and `*Path == "slowmode"`, `Score == nil`, `Threshold == nil`, `Word == nil`, `Learned == false`, `Deleted == true`, `RejectReason == nil`, and the A/B rows with `*Path == "slow"`.
- `TestGateSlidingEdge` (S6.2): t=0 `"a"`, t=+29 s `"b"`, t=+30.5 s `"c"` → C passes (no 3rd delete): `len(ops.deleted) == 0`, `pi.asks == 3`.
- `TestGateCountsFastPathDeletes` (S6.3a): `store.words = {"sw1ft": true}`; posts: `"sw1ft"` (single token, fast-deleted), `"cat"`, `"has"` → 3rd is gate-deleted. Assert `len(ops.deleted) == 2`, the second delete's ID = the 3rd post.
- `TestGateIgnoresLongPosts` (S6.3b): five 2-token posts in 10 s (clock +0..+8 s, content like `"very nice"`, `store.words` empty, `fakePi` verdict `CLEAN`/score 3) → `len(ops.deleted) == 0`, `pi.asks == 5`.

**Steps:**
- [ ] Write `TestGateDeletesThirdSingleToken` (RED: no `gateSlowmode` field/method — test references a nonexistent method → compile error. Expected.)
- [ ] Run `go test ./internal/handlers/derpies/ -run TestGate -v` → expect the compile failure
- [ ] Implement constants + struct fields + `gateSlowmode` + flow hook + comment update
- [ ] Re-run → `TestGateDeletesThirdSingleToken` passes
- [ ] Write `TestGateSlidingEdge`, `TestGateCountsFastPathDeletes`, `TestGateIgnoresLongPosts` (each RED→GREEN in turn; the sliding one should now pass — if it fails, the window math is wrong (check the `<= slowmodeWindow` comparison))
- [ ] `gofmt -l internal/handlers/derpies/` → silent
- [ ] `go build ./... && go vet ./...`
- [ ] Commit: `derpies: single-slowmode gate (2/30s sliding, per author+channel) + core tests`

**Acceptance criteria:**
- [ ] 2 single-token posts within 30 s → 3rd deleted at the gate: no `listGimmicks` call, no pi ask, delete via `ops.deleteMessage`, row `path='slowmode'` with NULL score/threshold/word and `deleted=true, learned=false`
- [ ] Sliding edge: a post at t=0 is pruned at t=30.5 s (3rd passes)
- [ ] Fast-path-deleted single tokens pass through for counting
- [ ] ≥2-token posts are never gated and run the full flow
- [ ] No changes to edit flow, nickname flow, `New()`, or any signature

---

### Task 3: Exclusions, failure, isolation, restart + full gate (S6.4–S6.9)

**Context:** The remaining behavior is regression-recording: the gate must be inert for everything the flow already short-circuits (feature off, non-gated author, no guild, 0-token / attachment-only), tolerate delete failure (best-effort, row still recorded), keep per-(author, channel) windows independent, and start empty on a "restart" (fresh handler). Plus the `read_derpies_decisions` MCP tool passes a `slowmode` path value through unchanged (no tool code changes — regression test only). Finally the full project gate and the `--selftest`.

**Files:**
- Test: `internal/handlers/derpies/derpies_slowmode_test.go` (extend)
- Test: `internal/handlers/derpies/derpies_mcp_test.go` (extend)

**What to implement (tests only — no production code):**

1. `TestGateExclusions` (S6.4): four sub-cases, each asserting `len(ops.deleted) == 0` and "the gate did nothing" (no `slowmode` row in `store.decisions`):
   - **feature off**: `store.enabled[FeatureKey] = false`, post `"cat"` → flow returns at the feature gate (zero asks, zero list).
   - **non-gated author**: `otherMsg("cat")` → 0 asks (author gate).
   - **no guild**: `m := derpMsg("cat"); m.GuildID = ""` → 0 asks (guild guard).
   - **0-token post**: `derpMsg("")` (empty content — the attachment-only shape) → the flow continues (with a `words`-empty `fakePi` CLEAN verdict: exactly 1 ask) — no gate row.
2. `TestGateDeleteFailure` (S6.5): `ops.delErr = errors.New("429")`; 3 single-token posts; the 3rd's delete fails → no panic, `len(ops.deleted) == 0` (the fake records only successes), **but** `store.decisions` still has a `path='slowmode'` row with `Deleted == true` (best-effort-recorded); the 4th post is still gate-deleted (as a success).
3. `TestGatePerChannelIsolation` (S6.6): 2 posts to channel `c1` + 2 to a `c2` variant (`derpMsg` variant with `m.ChannelID = "c2"` → make a local `derpMsgCh(ch, content)` helper or set the field) → the 3rd to `c1` is gate-deleted; the 3rd to `c2` passes (`len(ops.deleted) == 1`, the sole delete is c1's).
4. `TestGateEmptyOnRestart` (S6.9): 2 single-token posts on `h`; a fresh `h2 := newTestDerpies(same-store-semantics...)` (a new `fakeStore`/`fakeOps`) receives 2 identical posts → no delete (fresh window is empty).
5. MCP pass-through (S6.8) in `derpies_mcp_test.go`: mirror the existing `read_derpies_decisions` fake-source test, one row with `Path` set to `"slowmode"` (see the `mcp.DecisionRow` field in `internal/mcp/mcp.go`) → the tool's text line shows `slowmode` in the path slot and the structured JSON has `"path": "slowmode"`. (If `DecisionRow.Path` is `*string`, use a helper; check the struct first.)
6. **Full gate run (DB-gated, house discipline — the Makefile/AGENTS.md order):**
   - [ ] `make db-up`
   - [ ] `go build ./... && go vet ./... && gofmt -l . (silent) && make lint`
   - [ ] `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` — all green (the DB-gated run includes `Task 1`'s migration test + `this task`'s MCP regression; if this compose DB has dirty state, clean before the run per AGENTS.md: `docker compose down -v && make db-up`)
   - [ ] `go run ./cmd/tugbot --selftest` → logs the 14-handler line, exit 0
   - [ ] `TUGBOT_TEST_DATABASE_URL=... go test ./... -count=1` (plain CI-equivalent: DB tests self-skip) — green

**Steps:**
- [ ] Write all five test groups (they should PASS immediately against Task 2's implementation — if one fails, fix the implementation, not the test, and record why)
- [ ] Run each under `go test ./internal/handlers/derpies/ -run 'TestGate|TestReadDecisions' -v` → all pass
- [ ] `gofmt -l .` → silent; `make lint` → 0 issues
- [ ] Run the full DB-gated sequence above (step 6)
- [ ] Commit: `derpies: slowmode gate exclusions/failure/isolation tests + mcp path regression + DB-gated gate green`

**Acceptance criteria:**
- [ ] All S6.4–S6.9 behaviors hold as written above
- [ ] Full house gate green (build/vet/fmt/lint/test + DB-gated run + selftest) — see the `done-when` above

---

## Retained spec (the approved design, compressed; full text in git history — commit `13bb42f`)

### S1 — Gate and delete — same as the approved spec sections (S1: position after feature/guild/author gates, before fast path; gateable = `len(strings.Fields(m.Content)) == 1`; ≥2 recent → delete + record + return; ≤1 recent → append + full flow; delete failure = best-effort logged, row recorded `deleted=true`; the only action is `DeleteMessage` — no bot reply, no reaction, no warning).

### S2 — Counter state — `rateMu sync.Mutex` + `rateWindow map[string][]time.Time` on `Derpies`; pass-through append rule (count posts, not outcomes); lazy pruning on each access (drop >30 s entries, delete empty keys); no ticker; growth bounded (≤3 gated authors × channels, ≤2 entries per queue); loss on restart accepted.

### S3 — Audit trail — every gate-delete writes its own `derpies_decisions` row: `path = 'slowmode'`, `score/threshold/word = NULL`, `learned = false`, `deleted = true`, `reject_reason = NULL`; the migration's CHECK extension is the only DDL; MCP pass-through unchanged (regression test only).

### S4 — Boundaries — exclusions (0-token, ≥2-token, non-gated, non-guild, flag off); count = posts regardless of outcome (fast-path-deleted counts too); per-(author, channel); delete failure best-effort; restart = empty watermark; no cross-feature locks.

### S5 — Constants and toggles — code constants (2 / 30 s / 1 token); no dials, no config table, no slash commands; feature follows the existing `derpies` feature flag only; no constructor/wiring change; `MessageCreate` comment updated.

### S6 — Test plan — cases 1–9 as executed in Tasks 2 and 3.
