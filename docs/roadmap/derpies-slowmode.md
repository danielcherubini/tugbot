---
status: committed
done-when: A bot restart with the deployed binary deletes the 3rd+ single-token post by a gated author per channel within any rolling 30 s, before the fast path, with zero pi asks, and logs a `derpies_decisions` row with `path = 'slowmode'` (NULL score/threshold/word, `deleted = true`, `learned = false`). Posts with ≥2 tokens, 0-token posts, edits, non-gated authors, and non-guild messages run the full flow exactly as before — **edits are explicitly out of gate scope** (the edit flow uses a gate-excluded variant of the flow). The derpies feature flag off disables the gate. Full gate green: `go build ./... && go vet ./... && gofmt -l . (silent) && make lint && go test ./... -count=1`, the DB-gated run under `make db-up` with `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...`, and `go run ./cmd/tugbot --selftest`.
---

# Derpies single-slowmode gate — Plan

**Goal:** An in-handler sliding-window gate deletes a gated author's 3rd+ single-token **create** per channel inside any rolling 30 s — before the fast path, zero pi asks — and logs the delete with `path = 'slowmode'`.
**Architecture:** One mutex-guarded `map[string][]time.Time`, lazily initialized on the existing `Derpies` struct (key `authorID + "|" + channelID`), checked in the flow between the author-ID gate and the decision-record defer; a gate hit deletes via the existing `discordOps.deleteMessage` seam, writes its own `decisionRecord` (the deferred one never fires for gate hits), and returns. The edit flow (`edits.go`) switches to a gate-excluded variant of the same flow. One new migration (000007) extends the `path` CHECK constraint.
**Tech Stack:** Go, discordgo events, pgx (only the migration test touches PG), the package's existing fake seams.

**Spec source of truth:** this plan replaced the approved spec content (the spec sections S1–S6 are preserved below the tasks). **Naming correction from the spec text:** the spec called the DDL migration "0011" — the migrations directory ends at `000006`, so the task uses **`000007_derpies_slowmode_path.{up,down}.sql`**. Branch: create from `main` (gitflow-branching), PR back to `main`, squash merge. **Repo convention note:** no other migration has a `.down.sql` file — `dbmigrate` globs `*.up.sql` only; the 000007 down file is the repo's first down file and is a manual-rollback artifact (no tooling consumes it); the DB-gated test exercises it by direct execution (Task 1).

---

### Task 1: Migration 000007 — extend the `path` CHECK constraint

**Context:** `derpies_decisions.path` is `CHECK (path IN ('fast', 'slow'))` (created **unnamed** by `migrations/000005_derpies_score.up.sql`, so PostgreSQL's auto-name is `derpies_decisions_path_check`). Gate-deletes need a third value, `slowmode`. The up file must be **re-run-idempotent** on the shared DB-gated test database: it drops the constraint under its known auto-name and re-adds it under the *same* name with the 3-value check (a drop+re-add with an identical name is content-stable on a second run). The down file does the reverse. No other table is touched.

**Files:**
- Create: `migrations/000007_derpies_slowmode_path.up.sql`
- Create: `migrations/000007_derpies_slowmode_path.down.sql`
- Test: `internal/handlers/derpies/derpies_integration_test.go` (append; the existing `TestMigration000002AppliesAndSeeds` is the pattern to mirror — note it exercises **up only**; there is no established down convention, so the down check executes the down file's statements directly via `pool.Exec`, which is a faithful check of the file's content)

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

Test — `TestMigration000007AppliesAndRollsBack` in `internal/handlers/derpies/derpies_integration_test.go`, mirroring `TestMigration000002AppliesAndSeeds` mechanically (skip pattern: `testing.Short()` skip, `TUGBOT_TEST_DATABASE_URL` env → default `postgres://postgres:postgres@127.0.0.1:5432/tugbot_test`, `pgxpool.New`, ping-or-skip, `t.Cleanup(pool.Close)`, copy the REAL file into a temp dir, `dbmigrate.Run`), with this definitive procedure:

1. **Precondition** (the shared test DB may be in any state, including residue from a crashed prior run — the `TRUNCATE` goes FIRST so a stale `path='slowmode'` row cannot block the 2-value constraint re-add):
```sql
CREATE TABLE IF NOT EXISTS derpies_decisions (
    id bigserial PRIMARY KEY,
    message_id text NOT NULL,
    channel_id text NOT NULL,
    author_id text NOT NULL,
    content text NOT NULL DEFAULT '',
    path text,
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
    applied_at timestamp with time zone DEFAULT now()
);
TRUNCATE derpies_decisions;
ALTER TABLE derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
DELETE FROM schema_migrations WHERE version = '000007_derpies_slowmode_path';
```
(The `path text` in the CreateTable has NO check first — the constraint is added explicitly, per line: a crashed prior run could otherwise leave either variant and the re-add would fail.)
2. **UP** via `dbmigrate.Run` on a temp dir containing only the copied `000007_derpies_slowmode_path.up.sql`.
3. **Assert up:** `INSERT ... path='slowmode'` succeeds; `path='fast'` succeeds; `path='nope'` **fails**. Delete the inserted rows.
4. **DOWN** via direct execution: read the real `migrations/000007_derpies_slowmode_path.down.sql` with `os.ReadFile` and execute its content with `pool.Exec(ctx, content)` (the two ALTER statements may run as one string). Do NOT use `dbmigrate.Run` for down — there is no down convention in the runner (it globs `*.up.sql` only, and the tracker row from step 2 would make a re-run skip the file).
5. **Assert down:** `path='slowmode'` now **fails**; `path='fast'` succeeds. Delete the inserted rows.
6. **Restore the UP state** by direct execution of the up file's two statements (`os.ReadFile` + `pool.Exec` — NOT `dbmigrate.Run`: the tracker row `000007_derpies_slowmode_path` still exists from step 2, so `dbmigrate.Run` would be a no-op and the DB would stay in the 2-value state, breaking later DB-gated runs in this package). Assert `path='slowmode'` succeeds again. Delete the rows.

**Steps:**
- [ ] Write `TestMigration000007AppliesAndRollsBack` first (RED without the files: the test FATALs on reading the missing migration files — the expected failure)
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./internal/handlers/derpies/ -run TestMigration000007 -v` (after `make db-up`) — expect the missing-file fatal
- [ ] Create the two migration files
- [ ] Re-run the DB-gated test — expect pass
- [ ] `gofmt -l internal/handlers/derpies/` → silent
- [ ] Commit: `derpies: migration 000007 adds the slowmode decision path (DB-gated test)`

**Acceptance criteria:**
- [ ] Up file: `path IN ('fast','slow','slowmode')`, named constraint `derpies_decisions_path_check`, re-run-idempotent
- [ ] Down file: restores `IN ('fast','slow')`, re-run-idempotent
- [ ] The DB-gated test passes (and skips cleanly without PG / under `testing.Short()`), leaving the shared DB in the UP state

---

### Task 2: The gate — state, check, delete, record (+ edit exclusion, core S1–S3 behavior)

**Context:** The core: an in-memory per-(author, channel) sliding window that, for a gated author's **single-token create**, deletes the 3rd+ post inside a rolling 30 s before the fast path (zero pi asks, zero `listGimmicks`, zero image work), counts pass-through single-token posts regardless of flow outcome, and writes a `path='slowmode'` decision row (NULL score/threshold/word, `learned=false`, `deleted=true`, NULL reject_reason). **Edits are out of gate scope** (approved rule: creates only) — but `edits.go`'s `editFlow` currently calls the same `h.flow(m)`, and the flow is origin-agnostic, so the gate would silently catch edits too. Resolution: split the entry points — `flowGated(m, slowmodeOn bool)` carries the gate; `flow(m)` (which every existing test calls) becomes a thin wrapper `flowGated(m, true)`; `editFlow` calls `flowGated(m, false)`. Zero churn to existing tests. The state is lazily initialized (nil map → create, under `rateMu`), so `New()` need no changes — BUT `newTestDerpies` must gain `clock: time.Now` (house pattern: construction wires the clock, tests override it with a pinned closure; without it, the gate's `h.clock()` panics the entire existing suite, because at least six existing tests post single-token gated messages).

**Files:**
- Modify: `internal/handlers/derpies/derpies.go` (constants, struct fields, `gateSlowmode`, `flowGated` + `flow` wrapper, `MessageCreate`, comment, `newTestDerpies` is next)
- Modify: `internal/handlers/derpies/edits.go` (`editFlow` call site + its stale "origin-agnostic" comment)
- Modify: `internal/handlers/derpies/derpies_test.go` (`newTestDerpies`: add `clock: time.Now` — **and the `time` import**: the file currently imports context/errors/strings/testing + discordgo/app/config/mcp, no `time`)
- Test: `internal/handlers/derpies/derpies_slowmode_test.go` (new file)

**What to implement (in `derpies.go` unless noted):**

1. Constants (top of file, with the other consts):
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
	// pattern). Lazily initialized (nil map → create, under rateMu); resets
	// to empty on bot restart (accepted, decision 0011). The overflow arm
	// writes the pruned window back (no stale entries accumulate); an empty
	// key cannot persist (a pass-through always appends `now`).
	rateWindow map[string][]time.Time
	rateMu     sync.Mutex
```

3. `gateSlowmode` (place immediately before `func (h *Derpies) flow(`). REST + DB writes happen AFTER the lock (the delete can block on a Discord 429; do not hold `rateMu` across it):
```go
// gateSlowmode — the single-slowmode check (S1): called after the
// author-ID gate and BEFORE the decision-record defer. Returns true when
// it deleted the post (the caller must return — no fast path, no slow
// path, no pi ask). Ineligible (0-token or ≥2-token) posts fall through.
// Overflow (≥ slowmodeMaxPosts recent, window = now.Sub(ts) <=
// slowmodeWindow, inclusive) deletes via the ops seam (best-effort on
// failure — logged; the row is still written) and records its OWN
// path='slowmode' row. A pass-through appends its timestamp.
func (h *Derpies) gateSlowmode(ctx context.Context, m *discordgo.Message) bool {
	if len(strings.Fields(m.Content)) != 1 {
		return false
	}
	now := h.clock()
	gated := false
	h.rateMu.Lock()
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
		h.rateWindow[key] = kept // pruned write-back (the overflow doesn't append)
		gated = true
	} else {
		h.rateWindow[key] = append(kept, now)
	}
	h.rateMu.Unlock()
	if gated {
		if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
			slog.Error("derpies gate delete failed", "module", module, "message", m.ID, "error", err)
		}
		h.recordDecision(ctx, m, &decisionRecord{
			MessageID: m.ID, ChannelID: m.ChannelID, AuthorID: m.Author.ID,
			Content: m.Content, Path: strPtr("slowmode"),
			Learned: false, Deleted: true,
		})
	}
	return gated
}
```
(`strings` — check the existing imports in `derpies.go`; add only if missing. `sync` is already imported for `nickMu`. `strPtr` already exists.)

4. Flow entry-point split (the edit-exclusion plumbing):
   - Rename the existing `func (h *Derpies) flow(m *discordgo.Message) {` to `func (h *Derpies) flowGated(m *discordgo.Message, slowmodeOn bool) {` (body unchanged EXCEPT the hook from step 5).
   - New wrapper (directly above `flowGated`):
```go
// flow — the MessageCreate entry: the full flow WITH the single-slowmode
// gate. Existing tests call this; production MessageCreate (below) routes
// through flowGated directly for the gate. The edit flow (edits.go) calls
// flowGated directly with the gate OFF (approved rule: edits are out of
// gate scope — the gate watches MessageCreate only).
func (h *Derpies) flow(m *discordgo.Message) { h.flowGated(m, true) }
```
   - `MessageCreate`:
```go
func (h *Derpies) MessageCreate(m *discordgo.Message) { go h.flowGated(m, true) }
```
   - In `edits.go`, the `editFlow` call site (currently the 4-line commented block ending `h.flow(m)`): replace `h.flow(m)` with `h.flowGated(m, false)` and amend the comment block to:
```go
	// 5. The full create flow with the single-slowmode gate EXCLUDED
	//    (approved v1 rule: the gate watches MessageCreate only — edits
	//    are judged normally, at most one ask per edit). The flow's own
	//    gates re-run on m — idempotent and cheap; m already passed the
	//    event-level gates.
	h.flowGated(m, false)
```

5. The gate hook — inside `flowGated`, immediately after the line
```go
	slog.Info("derpies message from filtered user", "module", module, "user", m.Author.ID, "guild", m.GuildID)
```
insert (numbered 3.4 — the flow already has a "3.5 Referenced message" block, and the `dec := &decisionRecord{...}; defer h.recordDecision(...)` "C" block that follows must NOT run for a gate hit, so the hook precedes it):
```go
	// 3.4 Single-slowmode gate (decision 0011): the 3rd+ single-token post
	// within 30 s is deleted here — before the fast path (zero pi asks,
	// zero list fetch, zero image leg). A gate hit writes its own
	// path='slowmode' row; the deferred "C" record below never fires.
	if slowmodeOn && h.gateSlowmode(ctx, m) {
		return
	}
```

6. `MessageCreate` comment — the existing block is:
```go
// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
// Burst amplification: there is no per-author coalescing or cooldown — N novel
// posts from a filtered user yield N serialized pi asks (the pi RPC queue is shared with the mention handler); rate limiting is out of scope per the spec.
```
Replaced by (every line carries the `//` prefix):
```go
// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
// Burst amplification note (superseded for single-token posts, decision 0011):
// the single-slowmode gate (step 3.4) deletes a gated author's 3rd+
// single-token post per channel inside 30 s before the fast path (zero pi
// asks). LONG posts (≥2 tokens) are still unthrottled — N novel long posts
// still yield N serialized pi asks (the pi RPC queue is shared with the mention handler).
```

7. `newTestDerpies` (in `derpies_test.go` — **add the `time` import to this file** — it does not currently import `time`): the returned literal gains one field (alongside `store`/`ops` in the `Derpies` struct):
```go
	return &Derpies{
		app: &app.App{ ... },
		store: store,
		ops:   ops,
		clock: time.Now, // tests override with a pinned closure (the gate reads h.clock())
	}
```
Gate tests still pin `h.clock` themselves (the override wins); this line makes the existing suite safe.

8. **Stale-comment fixes (text-only, no behavior)** — the `flow` doc comment (currently the 4 lines immediately above `func (h *Derpies) flow(`):
```
// flow — the full message flow (gates → fast path → images → slow path →
// learn/delete). The create and edit paths run the identical flow —
// origin-agnostic; each post or edit costs at most one list SELECT + one
// pi ask.
```
— moves with the rename to `flowGated` and is replaced by:
```
// flowGated — the full message flow (gates → [single-slowmode gate,
// when slowmodeOn] → fast path → images → slow path → learn/delete).
// Each post costs at most one list SELECT + one pi ask. The
// MessageCreate entry (flow below) runs it with slowmodeOn=true; the
// edit flow (edits.go) runs it with slowmodeOn=false — the gate is
// the only flow element that differs between the two entries (approved
// v1 rule: edits are out of gate scope).
```
And in `edits.go`, the package-level doc's last 2 lines of that 8-line block:
```
// deletion all carry over unchanged (an edit is origin-agnostic: at most
// one list SELECT + one pi ask, same as a create).
```
→
```
// deletion all carry over unchanged (at most one list SELECT + one pi
// ask, same as a create) — with the single-slowmode gate EXCLUDED
// (approved v1 rule: the gate watches MessageCreate only; an edit is
// judged normally and is not counted).
```

**Tests (in `derpies_slowmode_test.go`, package `derpies`)** — construction pattern:
```go
store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}, prompt: defaultPromptTemplate}
ops := &fakeOps{}
pi := &fakePi{resp: "SCORE:3"} // one ask, score 3 < the learn floor 40 < the default threshold 50: path 'slow', no learn, no delete
h := newTestDerpies(store, ops, pi)
h.clock = func() time.Time { return tt } // tt moves per step
```
('fakePi' fields (verified in 'derpies_test.go'): 'resp string, askErr error, asks int, prompts []string, …' — no 'verdict/verdict/threshold' fields; the existing slow-path tests use 'resp' values in the 'SCORE:95\nWORD:zswiftf' style. With 'SCORE:3' the slow path records 'dec' with 'Path = "slow"' — matching the rows asserted below. 'fakeOps' has 'deleted [][]string' (each '{channelID, messageID}') and 'delErr error'; 'fakeStore.decisions' holds '[]*decisionRecord'.)

Helper (the 'derpMsg' factory hard-codes 'ID: "msg1"' — delete-ID assertions need distinct IDs):
```go
func gateMsg(id, ch, content string) *discordgo.Message {
	m := derpMsg(content)
	m.ID, m.ChannelID = id, ch
	return m
}
```

- `TestGateDeletesThirdSingleToken` (S6.1): pinned clock `tt = base`; post `gateMsg("a1","c1","cat")`, then `"has"`, then `"hit"` (IDs a1/a2/a3) via `h.flow(...)`. Assert after the 3rd: `len(ops.deleted) == 1` with `ops.deleted[0] == []string{"c1","a3"}`; `store.listCalls == 2` (A and B passed the gate and ran the fast-path list fetch; C never reached it); `pi.asks == 2`; `store.decisions` — the C row: `Path != nil && *Path == "slowmode"`, `Score == nil`, `Threshold == nil`, `Word == nil`, `Learned == false`, `Deleted == true`, `RejectReason == nil`; the A and B rows: `*Path == "slow"`.
- `TestGateSlidingEdge` (S6.2): posts at t=0, t=+29 s, t=+30.5 s ("a", "b", "c") → C passes: `len(ops.deleted) == 0`, `pi.asks == 3`.
- `TestGateCountsFastPathDeletes` (S6.3a): `store.words = {"sw1ft": true}` (already in the pattern); posts (fast-deleted single token): `"sw1ft"`, `"cat"`, `"has"` → the 3rd is gate-deleted: `len(ops.deleted) == 2`, `ops.deleted[1]` = the 3rd post's ID.
- `TestGateIgnoresLongPosts` (S6.3b): five 2-token posts in 10 s (`pi.asks == 5`, `len(ops.deleted) == 0` — `fakePi{resp: "SCORE:3"}`).
- `TestGateIgnoresEdits` (S6.4-add, the edit-scope pin, per the done-when): pinned clock; 2 single-token creates ("a1", "a2") via `h.flow(...)`; then a single-token edit via `editEvent` (the helper from `derpies_edits_test.go`, `editUser`-gated, channel c1, content "has") through `h.editFlow(evt)` → the edit is judged normally (`pi.asks` +1) and NOT gate-deleted (`len(ops.deleted) == 0` — the edit's fast/slow path runs; the edit does not count); then a 3rd single-token **create** ("a3") via `h.flow(...)` → gate-deleted (`len(ops.deleted) == 1`, `ops.deleted[0]` = a3).

**Steps:**
- [ ] Write the `newTestDerpies` clock fix + `TestGateDeletesThirdSingleToken` (RED: the test **compiles** and **fails at the `len(ops.deleted)` assertion** — no gate exists yet, so post 3 runs the full flow and nothing is deleted; that assertion failure is the expected RED; it is NOT a compile failure — if you get a compile error instead, the `time` import (item 7) is missing)
- [ ] Under `make db-up` + env, run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/derpies/ -run 'TestGate'` → expect the compile failure
- [ ] Implement constants + struct fields + `gateSlowmode` + the `flowGated`/`flow` split + the `edits.go` call site + the flow hook + the `MessageCreate` comment
- [ ] Re-run → `TestGateDeletesThirdSingleToken` passes
- [ ] Write `TestGateSlidingEdge`, `TestGateCountsFastPathDeletes`, `TestGateIgnoresLongPosts`, `TestGateIgnoresEdits` (each RED→GREEN; the sliding one should now pass — if it fails, the window math is wrong (check the `<= slowmodeWindow` comparison))
- [ ] **Run the ENTIRE package** (not a `-run` filter — the clock panic class hides in tests other than the gate ones): `TUGBOT_TEST_DATABASE_URL=... go test -p 1 -count=1 ./internal/handlers/derpies/` → all green (existing tests included: the `newTestDerpies` clock wiring covers them)
- [ ] `go build ./... && go vet ./... && gofmt -l .` → build/vet OK, silent
- [ ] Commit: `derpies: single-slowmode gate (2/30s sliding, per author+channel, creates only) + core tests`

**Acceptance criteria:**
- [ ] 2 single-token creates within 30 s → 3rd deleted at gate: no `listGimmicks` call, no pi ask, `ops.deleteMessage` delete, row `path='slowmode'` (NULL score/threshold/word, `deleted=true`, `learned=false`)
- [ ] Sliding edge: a post at t=0 is pruned at t=30.5 s (the 3rd passes)
- [ ] A fast-path-deleted single token counts for the window
- [ ] ≥2-token posts are never gated; 0-token posts fall through (full flow)
- [ ] **Edits (editFlow) run the full flow, are NOT counted, and cannot be gate-deleted**
- [ ] `MessageCreate`/`newTestDerpies` wiring complete; the entire derpies package green (build/vet/fmt verified)

---

### Task 3: Exclusions, failure, isolation, restart + MCP pass-through + docs + full gate (S6.4–S6.9)

**Context:** The remaining behavior is regression-recording: the gate must be inert for everything the flow already short-circuits (feature off, non-gated author, no guild, 0-token / attachment-only); tolerate delete failure (best-effort — the row is recorded; the fake's `delErr` makes EVERY `deleteMessage` fail, so the "4th post still gate-deleted" check requires clearing it first); keep per-(author, channel) windows independent; start empty on a "restart". The `read_derpies_decisions` MCP tool passes a `slowmode` path value through unchanged (no tool code changes — regression test only, in `internal/mcp/mcp_test.go` — the renderer is unexported in package `mcp`). Docs ship with the feature (the house pattern: CHANGE.md + `docs/features/derpies.md` + the path-valued comment/description strings that now read as "fast"/"slow"-only are wrong). Finally the full house gate and `--selftest`.

**Files:**
- Test: `internal/handlers/derpies/derpies_slowmode_test.go` (extend with 4 groups)
- Test: `internal/mcp/mcp_test.go` (extend — mirror `TestReadDecisionsToolTextRendersRows`: `fakeDecisionSource`/`NewServer`/`connectInProcess`/`callTool`, the `pStr` helper)
- Modify: `CHANGE.md`, `docs/features/derpies.md`
- Modify (comment/string fixes only, no behavior): `internal/handlers/derpies/derpies.go` (`decisionRecord.Path` doc), `internal/mcp/mcp.go` (`DecisionRow.Path`, `DecisionFilter.Path` docs), `internal/mcp/tools_decisions.go` (filter arg doc + tool Description)

**What to implement:**

1. `TestGateExclusions` (S6.4) — four sub-cases, each asserting `len(ops.deleted) == 0` and NO `slowmode` row in `store.decisions`:
   - **Feature off**: `store.enabled[FeatureKey] = false`, post `gateMsg("a1","c1","cat")` → 0 asks (the feature gate returns before the gate).
   - **Non-gated author**: `otherMsg` style (author ID "222"), single token → 0 asks (author gate).
   - **No guild**: `m := gateMsg("a1","c1","cat"); m.GuildID = ""` → 0 asks (guild guard).
   - **0-token post**: `gateMsg("a1","c1","")` (empty content — the attachment-only shape) with `words: {}` and `fakePi{resp: "SCORE:3"}` → the flow continues: exactly 1 ask, no gate row.
2. `TestGateDeleteFailure` (S6.5): `ops.delErr = errors.New("429")`; 3 single-token posts; the 3rd's delete fails → no panic, `len(ops.deleted) == 0` (the fake records only successes), **but** `store.decisions` still has a `path='slowmode'` row with `Deleted == true` (best-effort recorded). Then **`ops.delErr = nil`** (the blanket fake otherwise keeps failing — the "4th post gate-deleted" is only meaningful after clearing it), post #4 → `len(ops.deleted) == 1`. If a test in this group fails, fix the plan's test construction (fakes/clock sequence) first; suspect the implementation only then.
3. `TestGatePerChannelIsolation` (S6.6): 2 posts in c1 + 2 posts in c2 (`gateMsg`'s ch parameter) → the 3rd in c1 is gate-deleted; the 3rd in c2 passes (`len(ops.deleted) == 1`, the sole delete's ID is c1's 3rd).
4. `TestGateEmptyOnRestart` (S6.9): 2 single-token posts on `h`; a fresh `h1 := newTestDerpies(<fresh fakes>)` (new `fakeStore`/`fakeOps`/`fakePi` + pinned clock) receives 2 identical posts → no delete (fresh window is empty).
5. MCP pass-through (S6.8) in `internal/mcp/mcp_test.go`: **the test is named `TestReadDecisionsToolRendersSlowmodeRow`** (it must match the run filter in the verification step; a different name would make the filtered run vacuously green). Mirror `TestReadDecisionsToolTextRendersRows` mechanically; a single `DecisionRow` with `Path: pStr("slowmode")`, `Score`/`Threshold`/`Word` left nil (a `pInt` helper exists; nil pointers = the gate row's shape), `Learned: false`, `Deleted: true` → assert the text line renders `slowmode` in the path slot with `-/-` score/threshold and `-` word (the renderer's NULL optionals), and the structured payload carries `"path": "slowmode"`. No production changes.
6. Docs (house pattern — every prior derpies feature shipped with these):
   - `CHANGE.md`: new `## 2026-09-19` section, `### derpies: single-slowmode gate — 3rd+ single-token post per channel in 30 s deleted pre-judgement (ADR 0011)`, with the house bullets (Observed: the one-word post-burst, 114→209 decision rows in ~30 min on 2026-09-19, each word = a full pi ask; Changed: gate = 2 single-token creates per 30 s per author+channel, 3rd+ deleted before the fast path (zero asks), audit row `path='slowmode'` (NULL score/threshold/word), long posts/edits/unfiltered behavior unchanged, in-memory counter that resets on restart by design; Code: `internal/handlers/derpies` (gate + the `flowGated` entry split) + the `000007` migration; Docs: the `0011` decision + the `CONTEXT.md` entry + this file's limitations rewording + the path-valued string fix).
   - `docs/features/derpies.md`: (a) insert a new numbered item 4 into the `## Gates (checked in this order)` list (which currently ends at item 3, the author-ID gate): "4. **Single-slowmode gate (v1, creates only — decision 0011)**: a gated author's 3rd+ **single-token** post per channel inside a rolling 30 s is deleted here (pre-fast-path, zero pi asks, silent); pass-through single-token posts are counted regardless of flow outcome; ≥2-token posts, 0-token posts, and **edits** are not gated."; (b) line 90 heading ("…no rate limiting") + line 98 burst cell → mark the single-token case as gated (decision 0011; the gate covers single-token posts only — long-post burst amplification is unchanged); line 109 line (rate limiting out-of-scope) → single slowmode (decision 0011) implemented, long-post coalescing remains out of scope; (c) **refresh the front-matter `last-verified`/`verified-by` block AFTER the full gate runs** (house pattern — same inline form as the 2026-09-18 line; date = the ship date, the line naming the full gate evidence + `selftest` + "all fourteen handlers" + the decision 0011 entry).
   - Path strings (now wrong if left "fast"/"slow"-only): `derpies.go` `decisionRecord.Path` field doc → `"fast" | "slow" | "slowmode" | NULL`; **the `decisionRecord` struct-level doc (its line 3 quotes `"path text CHECK (path IN ('fast','slow'))"`) → the 3-value form after 000007** (quote the CHECK as `CHECK (path IN ('fast','slow','slowmode'))`); **the `Derpies` struct doc (the line `// Derpies handles the derpies flow (feature gate → guild guard →` / `// author-ID gate → fast-path token match → pi RPC verdict). It also`) → `// author-ID gate → [single-slowmode gate, creates only — decision 0011,` / `// the 3rd+ single-token post per channel within 30 s] → fast-path` / `// token match → pi RPC verdict). It also`** (add the bracketed gate to the chain, keep the remainder); `internal/mcp/mcp.go` `DecisionRow.Path` → `"fast" | "slow" | "slowmode" | NULL`, `DecisionFilter.Path` → `"fast" | "slow" | "slowmode" | ""`; `tools_decisions.go` filter arg doc likewise + tool Description `path ("fast"/"slow")` → `path ("fast"/"slow"/"slowmode")`.
7. **Full gate run (house discipline, AGENTS.md order)** — if the compose DB has dirty state from an earlier run, clear it first: `docker compose down -v && make db-up`.
   - [ ] `go build ./... && go vet ./... && gofmt -l .` (silent) && `make lint` (0 issues)
   - [ ] `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` — all green (includes Task 1's migration test + this task's MCP regression — the env-var run)
   - [ ] `go test ./... -count=1` (WITHOUT `TUGBOT_TEST_DATABASE_URL` — the CI-equivalent: DB tests self-skip cleanly)
   - [ ] `go run ./cmd/tugbot --selftest` → logs the fourteen-handler line, exit 0

**Steps:**
- [ ] Write the 4 derpies test groups + the MCP regression test (they should PASS immediately against Task 2's implementation — if one fails: fix the plan's test construction first (fakes/clock sequence), suspect the implementation only then; record any such fix)
- [ ] Run `go test ./internal/handlers/derpies/ -run 'TestGate' -v` + `go test ./internal/mcp/ -run 'TestReadDecisions|TestReadDerpiesDecisions' -v` → all pass
- [ ] Apply the docs (CHANGE.md, `docs/features/derpies.md`, the path-valued strings — no behavior changes; `go build ./...` + `go vet ./...` afterwards)
- [ ] Run the full gate sequence (step 7 above), including both `go test` variants and the `--selftest`
- [ ] `gofmt -l .` → silent; `make lint` → 0 issues
- [ ] Commit: `derpies: slowmode gate exclusions/failure/isolation tests + mcp path regression + docs (CHANGE, features doc, path strings)`

**Acceptance criteria:**
- [ ] S6.4–S6.9 all hold as stated (including the edit exclusion pin from Task 2)
- [ ] MCP tool renders `slowmode` rows (text slot + payload path) with no tool code change
- [ ] CHANGE.md + `docs/features/derpies.md` + the four path strings updated (no contradiction with the shipped behavior)
- [ ] The full house gate is green — see `done-when`

---

## Retained spec (approved design, compressed; full text in git history — commit `13bb42f`)

### S1 — Gate and delete — same as the approved spec section (S1: position after feature/guild/author gates, before fast path; gate-eligible = `len(strings.Fields(m.Content)) == 1`; ≥2 recent → delete + record + return; ≤1 recent → append + full flow; delete failure = best-effort logged, row recorded with `deleted=true`; the only action is `DeleteMessage` — no bot response, no reaction, no warning).

### S2 — Counter state — `rateMu sync.Mutex` + `rateWindow map[string][]time.Time` on `Derpies`; pass-through append rule (counts posts, not outcomes); lazy prune on each access (write-back the pruned window on both arms; an empty key cannot persist — a non-empty pass-through always appends `now`); no ticker; growth bounded (≤3 gated authors × channels, ≤2 entries per queue); loss on restart accepted.

### S3 — Audit trail — every gate-delete writes its own `derpies_decisions` row: `path = 'slowmode'`, `score/threshold/word = NULL`, `learned = false`, `deleted = true`, `reject_reason = NULL`; the CHECK extension by migration is the only DDL; MCP pass-through unchanged (regression test only).

### S4 — Boundaries — exclusions (0-token, ≥2-token, non-gated, non-guild, feature off; *edits — v1 scope*); `count = posts` regardless of outcome (fast-path-deleted ones count too); per-(author, channel); delete failure best-effort; restart = empty window; no cross-feature locks.

### S5 — Constants and toggle — code constants (2 / 30 s / 1 token); no dials, no config table, no slash command; the feature follows the existing `derpies` feature flag only; `flow`/`flowGated` entry split + the `newTestDerpies` clock hookup (no *production* constructor change — `New()` already wires `clock: time.Now`); `MessageCreate` comment updated.

### S6 — Test plan — cases 1–9 as executed in Tasks 2 and 3 (case 4 += the edit-scope pin).
