---
status: committed
done-when: a per-server nickname change by a gated derpies user that contains a known gimmick word (e.g. "Purchase me a zwift for 9/11") is answered within the same event by `derpies nickname cleared (fast|llm)` in the bot log and the member's nickname back to the global display name (cleared to null); a re-set within 60s is coalesced (at most one write per window per member); the bot's own edits are never re-judged; the full suite green with DB tests genuinely running
---

# Derpies nickname reset Plan

**Goal:** Close the nickname vector — the derpies filter watches `GuildMemberUpdate` for the gated user(s) and clears (resets to default) a nickname that judged as a gimmick word, using the same fast-path + LLM-slow-path architecture as the message filter.
**Architecture:** Extend the existing `derpies` package (one handler now watches two message events — house rule "one package per handler" preserved: no new handler entry, no bookkeeping change). All 90% plumbing is reused: feature gate, guild guard, author-ID gate, `store` seam, prompt seam, verdict parse/sanity, degradation discipline. New surface is one seam method (`clearNickname`), one `AddHandler` closure, and per-member state (last-nick cache, 60s edit window, busy guard).
**Tech Stack:** Go 1.22 module directive (build under the repo's Go toolchain), discordgo v0.29.0, log/slog (module `derpies`), pgx (no new DB surface — `poolStore` is reused verbatim).

**Why this design (context for the executing agent — read everything under a task's "Context" before implementing):**
- Discord `GUILD_MEMBER_UPDATE` events fire for ANY member change (role, mute, etc.), not just nicknames. discordgo v0.29.0's `Member` struct has **neither `OldNick` nor `ActorID` fields** (verified in `~/go/pkg/mod/github.com/bwmarrin/discordgo@v0.29.0/structs.go`) — so "did the nick change?" and "did the bot just write this?" cannot be answered from the event payload. Both run on an in-process **last-nick cache** instead: an event whose `Nick` equals the cache is skipped (no nick change — includes the bot's own echo, which is updated in the cache BEFORE the echo arrives), and a successful clear writes `""` into the cache. No recursion, no wasted RPC for echoes.
- "Reset to default" is **clearing the nickname**, not overwriting: a live probe (2026-09-07, on the real member PATCH via the bot token) verified `PATCH /guilds/{gid}/members/{mid} {"nick":null}` clears the nickname (display falls back to the global name) and `{"nick":""}` does the same. We use `null`. discordgo's `GuildMemberEdit` CANNOT do this (`GuildMemberParams.Nick` is `string json:"nick,omitempty"` — empty string is omitted from the JSON, so Discord never sees it). The real implementation uses the exported `Session.Request(method, url string, data interface{}, options ...RequestOption) ([]byte, error)` with `map[string]any{"nick": nil}`.
- The `derpies` feature flag gates both flows (message + nickname); the gated users come from `config.Config.DerpiesUserIDs` via the same checked conversion (`core.DiscordID("user", id)`).
- Degradation discipline (mirroring the message flow, `derpies.go` package doc): every failure arm logs and stops; a clear failure after a successful learn keeps the word learned; a list-fetch error never acts on a half-loaded list.

---

### Task 1: Nickname flow in the derpies package (tests + implementation)

**Context:**
This task adds the entire nickname judgement flow to the existing `internal/handlers/derpies` package in one commit: the `MemberUpdate` entry point, the `nickFlow` core, the per-member state (last-nick cache / 60s edit window / busy guard), the new `clearNickname` seam, and the full test suite for it. It is a new capability with zero change to the message flow's behavior — do NOT modify any statement in `derpies.go`'s `flow()` function, and do NOT touch `tokensForMatch`, `parseVerdict`, `gimmickPrompt`, `wordmatch`, or `poolStore`. The message flow's 47 tests must pass unmodified. Errors are read with `slog` and module tag "derpies" (the existing `module` const in `derpies.go`). See "Tech Stack" above for the two API constraints (no `OldNick`/`ActorID` in v0.29.0 → cache design; `omitempty` nick param → raw `Request` PATCH).

**Files:**
- Create: `internal/handlers/derpies/nicknames.go`
- Modify: `internal/handlers/derpies/derpies.go` (ONLY: 5 new fields on `Derpies`, 1 rewired `New()` body, 1 new method on `realOps`, 1 new method on the `discordOps` interface, plus the `sync` import (for `nickMu`) — nothing else)
- Test: Create: `internal/handlers/derpies/nicknames_test.go`
- Test: Modify: `internal/handlers/derpies/derpies_test.go` (ONLY: add 3 fields to `fakeOps`)

**What to implement:**

1. `Derpies` struct — add exactly these 5 fields (place after `ops`):
   - `clock func() time.Time` — injectable clock.
   - `lastNick map[string]string` — per-member last-known nickname; `""` means "no nickname" (display is the global name).
   - `lastEdit map[string]time.Time` — per-member last reset attempt (success OR clear-failure mark the window — 429 discipline: a failed attempt must not be retried within the window).
   - `busy map[string]bool` — per-member in-flight guard.
   - `nickMu sync.Mutex` — guards the four maps.
   Keys are `guildID + "|" + memberID` (helper `nickKey(guildID, memberID string) string` in `nicknames.go`).
   The current `New()` sets only `app`/`store`/`ops` — REWRITE it to initialize ALL FIVE, or the first live event assigns to a nil map and crashes the process (selftest constructs the handler but never dispatches events, so no gate would catch this):
   ```go
   func New(a *app.App) *Derpies {
   	return &Derpies{app: a, store: &poolStore{pool: a.Pool}, ops: &realOps{d: a.D},
    		clock: time.Now, lastNick: map[string]string{},
    		lastEdit: map[string]time.Time{}, busy: map[string]bool{}}
   }
   ```

2. `discordOps` interface — add one method (keep the two existing ones untouched):
   - `clearNickname(guildID, memberID string) error`
   Real implementation (add to `realOps`, do NOT change `New()`'s `ops: &realOps{d: a.D}` line):
   ```go
   // clearNickname — "reset to default": Discord's member PATCH accepts
   // "nick": null, which clears the per-server nickname (display falls back
   // to the global display name; verified live 2026-09-07). GuildMemberEdit
   // cannot do this — GuildMemberParams.Nick is an omitempty string, so an
   // empty value is omitted from the JSON and Discord never sees it.
   func (o *realOps) clearNickname(guildID, memberID string) error {
   	_, err := o.d.Request("PATCH", "/guilds/"+guildID+"/members/"+memberID, map[string]any{"nick": nil})
   	return err
   }
   ```

3. `nicknames.go` — the flow. `MemberUpdate(evt *discordgo.GuildMemberUpdate) { go h.nickFlow(evt) }` (mirrors `MessageCreate`). `func (h *Derpies) nickFlow(evt *discordgo.GuildMemberUpdate)`:
   - `ctx := context.Background()`, then gate in this EXACT order (each failing gate returns without touching state):
     1. `if !h.store.featureEnabled(ctx, FeatureKey) { return }` (silent flavor — no log, same as the message flow).
     2. `if evt == nil || evt.Member == nil || evt.Member.User == nil { return }` (silent — the `main.go` closure already nil-guards the payload; this makes the flow call-safe on its own).
     3. `if evt.GuildID == "" { return }`.
     4. Author gate: `uid, err := core.DiscordID("user", evt.User.ID); if err != nil { return }` then `if _, ok := h.app.Cfg.DerpiesUserIDs[uid]; !ok { return }` (same checked-conversion discipline as the message flow's step 3).
     5. `slog.Info("derpies nickname event", "module", module, "user", evt.User.ID, "guild", evt.GuildID, "nick", evt.Nick)`.
   - STATE HELPERS (each locks `h.nickMu`, sets the one map, then unlocks): `h.saveNick(key, value string)` sets `lastNick[key] = value`; `h.markEdit(key)` sets `lastEdit[key] = h.clock()`. (Used by the empty-nick arm, the keep-the-nick verdict arms, and `clearNow`.) Also `const nicknameResetCooldown = 60 * time.Second` (const in `nicknames.go`, NOT config — YAGNI).
   - Define the key ONCE here, after gate 5, before anything that uses it: `key := nickKey(evt.GuildID, evt.User.ID)`.
   - Empty-nick arm: `if evt.Nick == "" { h.saveNick(key, ""); return }` (a clear/absence event: the display is already the global name — record and return; NO list fetch).
   - Serialization + cooldown + change detection (all under `h.nickMu`):
     ```go
     h.nickMu.Lock()
     if h.busy[key] { h.nickMu.Unlock(); slog.Info("derpies nickname busy — skipping", "module", module, "guild", evt.GuildID, "member", evt.User.ID); return }
     if last, ok := h.lastEdit[key]; ok && h.clock().Sub(last) < nicknameResetCooldown {
         h.nickMu.Unlock(); slog.Info("derpies nickname cooldown — skipping", "module", module, "guild", evt.GuildID, "member", evt.User.ID); return
     }
     if cur, ok := h.lastNick[key]; ok && cur == evt.Nick { h.nickMu.Unlock(); return } // same nick (incl. bot echo) — no change, no log
     h.busy[key] = true
     h.nickMu.Unlock()
     defer func() { h.nickMu.Lock(); h.busy[key] = false; h.nickMu.Unlock() }()
     ```
   - FAST PATH (identical shape to the message flow's step 4, minus the reference leg):
     ```go
     list, err := h.store.listGimmicks(ctx)
     if err != nil { slog.Error("derpies nickname list fetch failed", "module", module, "error", err); return }
     toks := tokensForMatch(evt.Nick)
     for tok := range toks {
         if list[tok] { h.clearNow(ctx, key, evt, "fast", tok); return }
     }
     ```
   - SLOW PATH (mirrors message flow steps 5–10, minus the image leg — a nickname has no images, so `h.app.Pi.Ask` is ALWAYS used, never `AskWithImages`; the `Pi == nil` arm is `slog.Info("derpies pi RPC not available, skipping (nickname)")` + return, no cache write):
     - Prompt: `tmpl, err := h.store.promptText(ctx)`; on `err != nil || !validTemplate(tmpl)` → `tmpl = defaultPromptTemplate` (+ the same warn log as the message flow, with `"(nickname)"` appended to the log message). `content := "New nickname set by the user: " + evt.Nick`. `prompt := gimmickPrompt(tmpl, content, sortedKeys(list), 0, "")`.
     - `text, askErr := h.app.Pi.Ask(ctx, prompt)`; on error → `slog.Error("derpies nickname ask failed", "module", module, "error", askErr)` + return (no cache write).
     - `kind, word := parseVerdict(text)`; arm semantics (each log line mirrors the message flow's with the "(nickname)" suffix where a message-flow analogue exists; the cache write decision is EXPLICIT — the executing agent must not guess):
       - `"clean"` → `slog.Info("derpies nickname verdict clean", "module", module, "guild", evt.GuildID, "member", evt.User.ID, "nick", evt.Nick)`; `h.saveNick(key, evt.Nick)` (terminal for this nick — the same nick must not be re-judged on a subsequent role-only update); return.
       - `"unknown"` → `slog.Warn("derpies nickname unrecognized verdict — doing nothing", "verdict", strings.TrimSpace(text))`; `h.saveNick(key, evt.Nick)`; return.
       - `"gimmick"` → sanity (EXACT port of the message flow's step 9, token scope narrowed to the nickname; the message flow's `hasTextTokens` check is replaced by a single check because a non-empty `evt.Nick` has tokens after `tokensForMatch` by construction):
         ```go
         fw := wordmatch.FoldToASCII(word)
         if !wordmatch.WordValid(fw) { slog.Warn("derpies nickname invalid verdict word — doing nothing", "module", module, "word", word, "nick", evt.Nick); h.saveNick(key, evt.Nick); return }
         if !toks[fw] { slog.Warn("derpies nickname verdict word not in the nickname — doing nothing", "word", word, "nick", evt.Nick); h.saveNick(key, evt.Nick); return }
         ```
         Then learn-then-act (message-flow discipline, verbatim shape):
         ```go
         if err := h.store.addGimmick(ctx, fw, SourceLLM); err != nil {
             slog.Error("derpies nickname add gimmick failed", "module", module, "word", fw, "error", err)
         }
         h.clearNow(ctx, key, evt, "llm", fw)
         ```
   - `clearNow(ctx, key, evt, path string, word string)` — the single action arm:
     ```go
     if err := h.ops.clearNickname(evt.GuildID, evt.User.ID); err != nil {
         slog.Error("derpies nickname clear ("+path+") failed", "module", module, "word", word, "guild", evt.GuildID, "member", evt.User.ID, "from", evt.Nick, "error", err)
         h.markEdit(key) // 429 discipline: a failed attempt marks the window (no retry inside the window)
         return
     }
     h.markEdit(key)
     h.saveNick(key, "") // BEFORE the gateway echo arrives — the echo event then sees nick == cache and skips (no self-re-judge, no recursion); empty = "no nickname"
     slog.Info("derpies nickname cleared ("+path+")", "module", module, "word", word, "guild", evt.GuildID, "member", evt.User.ID, "from", evt.Nick)
     ```
   Do NOT add any new store methods, any DB query, any config field, or any migration.

4. `fakeOps` in `derpies_test.go` — add exactly 3 fields (keep the existing 2 methods (`deleteMessage`, `channelMessageRetrieve`) byte-identical):
   ```go
   clearErr  error
   clears    int
   clearArgs []string
   ```
   implement `clearNickname` on `fakeOps` to INCREMENT `clears` AND APPEND `guildID + "|" + memberID` TO `clearArgs` **UNCONDITIONALLY — before the error return — then return `f.clearErr`**. This deliberately DIFFERS from `deleteMessage` (whose real fake early-returns on `o.delErr` WITHOUT recording): the nickname tests must count a FAILED attempt as an attempt (tests 17/19 require `clears == 1` when `clearErr` is set). Naming mirrors the file's existing fakes (`delErr`/`deleted` are the real delete-fake field names); the store assertions use the EXISTING `fakeStore.added` field. The field list above is the complete `fakeOps` addition — do NOT change `deleteMessage` or `channelMessageRetrieve`, and do not add any other fake field in `derpies_test.go`. `assertNoEdits(t, ops)` helper (in `nicknames_test.go`): fails if `ops.clears != 0`.

**Steps:**
- [ ] Write ALL of the following failing tests in `internal/handlers/derpies/nicknames_test.go` first (TDD red). Common helpers to write alongside the tests: `newTestNickDerpies(store, ops, pi, fixed *time.Time) *Derpies` = `newTestDerpies(store, ops, pi)` followed by zeroing the new state (`h.lastNick = map[string]string{}`, `h.lastEdit = map[string]time.Time{}`, `h.busy = map[string]bool{}`) and setting `h.clock = func() time.Time { return *fixed }`. EVERY test declares its OWN `var fixed time.Time` inside the test body and passes `&fixed`; any test that fires a SECOND event after a reset bumps `fixed` first (e.g. `fixed = fixed.Add(59 * time.Second)`) — `markEdit` snapshots `h.clock()` at event time, so a relative bump satisfies the window math (the clock is only dereferenced by the cooldown check when a prior edit exists, so single-event tests run fine at the zero time).; `nickEvent(g, u, nick string) *discordgo.GuildMemberUpdate` = `&discordgo.GuildMemberUpdate{Member: &discordgo.Member{GuildID: g, Nick: nick, User: &discordgo.User{ID: u}}}`; `const filteredNickUser = "163055057254875136"` (the EXACT gated-user ID constant the `derpMsg`/`newTestDerpies` message tests use — `newTestDerpies` hard-codes `Cfg.DerpiesUserIDs: map[int64]struct{}{163055057254875136: {}}`, so only THIS ID passes the author gate).
- [ ] Failing tests (each: declare `var fixed time.Time`, then `h := newTestNickDerpies(store, ops, pi, &fixed)`; then `h.nickFlow(...)`; assert):
  1. `TestNickFlowFeatureFlagOff` — `enabled: {FeatureKey: false}`; nick event with a list-hit nick; assert no clear, `store.listCalls == 0`, `pi.asks == 0`.
  2. `TestNickFlowNoGuild` — enabled; `nickEvent("", user, "sw1ft")`; assert no clear, `listCalls == 0`.
  3. `TestNickFlowNilMemberGuard` — enabled; `h.nickFlow(&discordgo.GuildMemberUpdate{})` (nil `Member`); assert no panic, no clear, `listCalls == 0`.
  4. `TestNickFlowAuthorNotFiltered` — enabled; a NON-gated member ID that still converts (a valid int64 NOT in `Cfg.DerpiesUserIDs`, e.g. `"123456789012345678"`); assert no clear, `listCalls == 0`.
  5. `TestNickFlowEmptyNickSkipsAndCaches` — enabled; event with `Nick: ""`; assert `listCalls == 0`; then a SECOND event with `Nick: ""` → still 0 (the empty arm is pre-list-fetch — assert both returns and `ops.clears == 0`).
  6. `TestNickFlowFastHitClearsNoLearn` — `list: {"sw1ft": true}`; event nick `"Who's giving me a sw1ft."`; assert `ops.clears == 1`, `len(store.added) == 0`, `pi.asks == 0`; then a SAME-nick event again → still `clears == 1` — the successful clear MARKED THE WINDOW, so the repeat is skipped by the COOLDOWN check (which precedes list fetch → assert `store.listCalls` did not grow beyond 1).
  7. `TestNickFlowFastHitUnicode` — `list: {"zwift": true}`; event nick `"give me a žwift"`; assert `clears == 1`, `pi.asks == 0` (the fold makes it a fast hit, message-flow parity).
  8. `TestNickFlowFastMissListErrorSkips` — `store.listErr` set; event nick `"sw1ft"`; assert no clear, no ask, no panic.
  9. `TestNickFlowFastMissNilPiSilent` — `pi` nil shape per the message tests (`newTestDerpies` with a nil Pi — check how `TestFlowNilPiSilentReturn` constructs it and mirror it); event with a list-miss nick; assert no clear, `listCalls == 1`.
  10. `TestNickFlowVerdictClean` — list-miss nick; `pi` answers `"CLEAN"`; assert `clears == 0`, `len(store.added) == 0`; a SAME-nick event again → `listCalls` stays 1 (the clean verdict caches the nick; NO edit was performed so NO cooldown was marked — the repeat is blocked by the CACHED-EQUAL skip, not the cooldown).
  11. `TestNickFlowVerdictUnknown` — `pi` answers `"gimmick without colon"`; assert no clear, no learn; same-nick repeat does not re-fetch (`listCalls` stays 1).
  12. `TestNickFlowVerdictInvalidWord` — `pi` answers `"GIMMICK:x"` (1 char, fails `WordValid`); assert no clear, `len(store.added) == 0`.
  13. `TestNickFlowVerdictNotInNickname` — `pi` answers `"GIMMICK:zwift"`; event nick `"hello world"` (no `zwift` token); assert no clear, `len(store.added) == 0` (hallucination guard — mirrors `TestFlowVerdictHallucinatedWordAbsentFromMessage`); a same-nick repeat does not re-fetch (`listCalls` stays 1 — the not-in-nickname arm caches the nick; no edit → no cooldown; blocked by the CACHED-EQUAL skip).
  14. `TestNickFlowVerdictLearnsAndClears` — `pi` answers `"GIMMICK:zwift"`; event nick `"Purchase me a zwift for 9/11"` (the real-world trigger — its tokens include `zwift`); assert `len(store.added) == 1` and `store.added[0] == "zwift|llm"`, `ops.clears == 1`, `pi.asks == 1`; a same-nick repeat → no second edit (the successful clear MARKS THE WINDOW — the repeat is blocked by the COOLDOWN check, which precedes the cached-equal check; `lastNick` is `""` after a clear).
  15. `TestNickFlowCooldownBlocksWithinWindow` — `list: {"sw1ft": true}`; event A nick `"sw1ft"` → `clears == 1`; BUMP `fixed` by 59s; event B nick `"Sw1ft again!"` (a different nick, so cache-skip does NOT apply) → `clears` stays 1 (cooldown skip), `listCalls` stays 1 (the cooldown check precedes the list fetch).
  16. `TestNickFlowCooldownExpires` — same setup; BUMP `fixed` to +61s instead → event B → `clears == 2`.
  17. `TestNickFlowClearFailureMarksWindow` — `list: {"sw1ft": true}`, `ops.clearErr` set; event A → `clears == 1` (the attempt happens, fails); BUMP 59s; event B (different nick) → no second attempt (`clears` stays 1); BUMP to +61s; event C → `clears == 2` succeeds (error flag cleared first — reset `ops.clearErr = nil`).
  18. `TestNickFlowBusySkips` — pre-set `h.busy[nickKey] = true` (via the test helper access; state is set BEFORE calling `nickFlow`); event with a list-hit nick → no clear, `listCalls == 0` (the busy check precedes the list fetch).
  19. `TestNickFlowLearnSurvivesClearFailure` — `ops.clearErr` set; `pi` answers `"GIMMICK:zwift"`; event with a matching token; assert `len(store.added) == 1` and `ops.clears == 1` (the attempt) — the word stays learned on the clear failure (message-flow parity: a delete failure after a successful learn keeps the word learned).
- [ ] Run `go test ./internal/handlers/derpies/ -run 'TestNick' -count=1`
  - Did it fail (undefined symbols / missing methods)? It MUST fail (red). If it compiled or passed, stop and investigate why.
- [ ] Implement: the 5 struct fields + the `New()` body per item 1 (clock + the three map initializers — `nickMu` takes its zero value), the `clearNickname` seam (interface + `realOps`), the `sync` import in `derpies.go`, the full `nicknames.go` flow per section 3 above.
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` (without PG the 3 migration tests in `derpies_integration_test.go` self-skip — EXPECTED, not a stop condition; the true 0-FAIL/0-SKIP gate is Task 3's)
  - All non-skip tests pass — the 47 message-flow tests UNMODIFIED AND the 19 new `TestNick*`. If any message-flow test changed or fails, stop and fix before continuing.
- [ ] Run `gofmt -l .` → must print nothing; `go build ./...` → succeeds.
- [ ] Commit with message: `"derpies: watch nickname changes, clear gimmick nicks (fast + llm path)"`

**Acceptance criteria:**
- [ ] `internal/handlers/derpies/nicknames.go` exists; `derpies.go` diff is exactly 5 struct fields + 1 rewired `New()` body + 1 `realOps` method + 1 interface method + the `sync` import (for `nickMu`).
- [ ] `go test ./internal/handlers/derpies/ -count=1` → 19 new `TestNick*` pass AND the pre-existing message-flow suite passes unmodified.
- [ ] A fast-path hit clears the nickname without a pi ask or a learn; a slow-path GIMMICK verdict learns (source `llm`) and then clears; `CLEAN`/`unknown`/invalid/out-of-nickname verdicts produce zero writes.
- [ ] Cooldown: at most one clear attempt per member per 60s window (success AND failure mark the window); the 60s value is a const, not config.
- [ ] A successful clear records `lastNick = ""` so the gateway echo of our own write is skipped (no self-re-judge); a same-nick (or role-only) event does not re-fetch the list.
- [ ] `gofmt -l .` silent; `go build ./...` clean.

---

### Task 2: Wire the event in command

**Context:**
Task 1 built the flow; this task makes the live bot receive the event. The house pattern (see `cmd/tugbot/main.go` around the existing closures) is a `d.AddHandler(func(...) { go h.<handler>.... })` closure per event kind, with a leading nil payload guard where the event has one, registered in the same block as the other event closures (main.go, before `d.Open()` — the command registration itself happens in the `OnReady` handler). The `GUILD_MEMBERS` intent is ALREADY pinned in `botIntents()` (verified at main.go lines 84–90), so NO intent change is needed and NO command-registration count changes (this is an event handler, not a command; the 13-handler selftest bookkeeping and `TestRegisterCommandsUsesReadySliceRustOrder` must pass UNMODIFIED).

**Files:**
- Modify: `cmd/tugbot/main.go` (ONE new closure, placed immediately after the existing `GuildMemberAdd` closure which looks like `d.AddHandler(func(_ *discordgo.Session, evt *discordgo.GuildMemberAdd) { ... h.gulag.JoinRejoin(evt.Member) })`)
- Test: `cmd/tugbot/main_test.go` — read it to confirm no test asserts a fixed number of `AddHandler` registrations (it should not — registrations are unobservable; the 13-handler count is about the constructor, which is untouched). If such an assertion exists, do NOT change it — report the contradiction.

**What to implement:**
Add exactly this closure in `main.go` immediately AFTER the `GuildMemberAdd` closure:
```go
	// OnGuildMemberUpdate → the derpies nickname reset (feature-gated
	// inside the handler; the derpies package now watches two events).
	// NON-BLOCKING: the flow runs in its own goroutine.
	d.AddHandler(func(_ *discordgo.Session, evt *discordgo.GuildMemberUpdate) {
		if evt == nil || evt.Member == nil || evt.Member.User == nil {
			return
		}
		h.derpies.MemberUpdate(evt)
	})
```
(Match the sibling closures' comment style — the short capitalized-lead `// On<Event> → …` comment; verify against the actual file when implementing.) Do NOT touch `botIntents()`, `registerCommands`, `dispatchCommand`, or the handler construction list.

**Steps:**
- [ ] Read `cmd/tugbot/main_test.go` and confirm no test pins an AddHandler count (document the check in the commit? no — just do not break anything).
- [ ] Add the closure per above (you may apply the edit; this is a one-hunk change, but FIRST the test step below does not exist pre-implementation — the "failing test" for this task is the BUILD: without Task 1's `MemberUpdate` the closure would not compile; it exists, so the red state is absent by design — the acceptance is the modified build + suite. If you are executing this task WITHOUT Task 1 landed, `go build` fails on `h.derpies.MemberUpdate` — that is the red state; do not proceed otherwise).
- [ ] Run `go build ./...` → succeeds.
- [ ] Run `go vet ./...` → clean; `gofmt -l .` → silent.
- [ ] Run `go test ./cmd/tugbot/ -count=1` (no PG needed — the DB-touching tests self-skip cleanly per `AGENTS.md`)
  - All non-skip tests pass, including `TestRegisterCommandsUsesReadySliceRustOrder` (if it runs without a PG skip). If it skips, that is correct per AGENTS.md (run Task 3's DB gate for the real check).
- [ ] Run `go run ./cmd/tugbot --selftest` (requires the DB up per `AGENTS.md`: `make db-up` + URL override NOT needed — selftest uses the default config env; if it skips DB check locally use `make db-up` first; the expected log line is `Discord session and all thirteen handlers constructed`)
  - Did it print the thirteen-handlers line and exit 0? If the handler count changed, STOP — nothing in this task must touch construction.
- [ ] Commit with message: `"main: route GuildMemberUpdate to the derpies nickname flow"`

**Acceptance criteria:**
- [ ] The new `AddHandler` closure is the ONLY line in this task's diff (plus the comment), placed after the `GuildMemberAdd` closure.
- [ ] `go build ./...`, `go vet ./...` clean; `gofmt -l .` silent.
- [ ] `go test ./cmd/tugbot/ -count=1` green (self-skips clean, no failures).
- [ ] Selftest still logs "thirteen handlers" and exits 0.

---

### Task 3: Full gate, verification, and documentation

**Context:**
Final passing of the running feature: prove the full suite green WITH the DB tests actually run (the DB-touching packages exercise `poolStore`-adjacent code paths — the store seam the nickname flow reuses is the same `poolStore` already exercised by the message-flow integration tests, so no new DB test is required — but the full gate proves no cross-package interference, per `AGENTS.md`'s documented discipline: compose PG, `-p 1` (the DB packages DROP/recreate shared tables in one shared compose DB), `-count=1` (cache masking)).

**Files:**
- Modify: `CONTEXT.md` (append ONE glossary entry near the **Derpies filter** entry)
- Create: none (no new document — the ADR `docs/decisions/0004-derpies-nickname-reset.md` already exists; it and this plan are committed together with the first commit of this branch, per normal gitflow-branching practice)

**What to implement:**
1. `CONTEXT.md` — append this entry immediately after the **Derpies filter** entry's `_Avoid_` line (present-tense, matching the file's style):
   ```
   **Nickname reset**
   The derivative of the Derpies filter on `GuildMemberUpdate`: when a gated derpies user changes their per-server nickname, the new nickname is judged the same way as a message (fast-path token match against `derpies_gimmicks`, then the slow pi-RPC verdict); a GIMMICK verdict clears the nickname (`PATCH .../members/{member} {"nick":null}` — display falls back to the global display name). Resets are per-member coalesced: at most one clear attempt per 60-second window (success or failure marks the window), and the bot's own writes are never re-judged because a successful clear records the cache BEFORE the gateway echo arrives. The `derpies` feature flag gates both the message flow and this one.
   _Avoid_: nickname ban, display name reset, member rename
   ```
2. Run the full gate (exact commands, in order; see `AGENTS.md`):
   - `make db-up`
   - `go build ./...`
   - `go vet ./...`
   - `gofmt -l .` → must print nothing
   - `make lint`
   - `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...`
     - ALL packages `ok`, 0 FAIL, 0 SKIP (a skip under this exact command line is a missing override, NOT a legitimate result — per AGENTS.md).
   - `go run ./cmd/tugbot --selftest` → the thirteen-handlers line + exit 0.
3. State-residue caveat (AGENTS.md): the DB suite can leave the shared compose DB with reset tables; if a later `make migrate` misbehaves, remedy with `docker compose down -v` first.

**Steps:**
- [ ] Run the full gate items in order above.
  - Did everything pass? If the suite reports a SKIP, stop and re-run with the URL override (a skip = missing override, not a legitimate result).
- [ ] Append the `CONTEXT.md` entry per above.
- [ ] `git add CONTEXT.md && git commit -m "docs: nickname reset glossary entry"` (only `CONTEXT.md` — verify with `git status` that no other files drifted into this commit).

**Acceptance criteria:**
- [ ] The full gate output above: 0 FAIL, 0 SKIP, all packages `ok`, selftest clean.
- [ ] `CONTEXT.md` carries the **Nickname reset** entry (present tense, with the `_Avoid_` line).
- [ ] Post-deploy verification procedure (for the operator after `update-tugbot` on `root@tugbot`): the gated user's gimmicky nickname (e.g. the observed `Purchase me a zwift for 9/11`) is cleared to the global display name within the event: the FIRST event logs `derpies nickname cleared (fast)` when its token is already in `derpies_gimmicks` (in the production DB `zwift` IS seed-listed — verified 2026-09-07), otherwise `derpies nickname cleared (llm)` — the token is learned and THEN cleared, after which a re-set AFTER the 60s window logs `(fast)`; a re-set WITHIN 60s logs `derpies nickname cooldown — skipping`; the bot's own clear does not re-log a judgement; the message filter's behaviour is byte-identical (no `derpies delete (fast)` path change).
