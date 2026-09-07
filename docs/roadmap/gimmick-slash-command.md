---
status: committed
done-when: `/gimmick list`, `/gimmick add word`, and `/gimmick delete word` work as a registered Discord slash command: a member with the Highly Regarded or admin role can manage the `derpies_gimmicks` table without any database console, and `go build ./...`, `go test ./...` (with the compose PG for DB-touching tests), and `go vet ./...` are all green; `tugbot --selftest` (with `make db-up`) logs "Discord session and all thirteen handlers constructed".
---

# /gimmick slash command — implementation plan

**Goal:** Add a `/gimmick` slash command (subcommands `add` / `list` / `delete`) that views, adds, and deletes rows in `derpies_gimmicks` (the derpies filter's gimmick word list) from Discord, with no database console.

**Architecture:** Three pieces: (1) `internal/wordmatch` — the filter's fold + charset/length contract extracted from `internal/handlers/derpies/derpies.go` into a shared package so the command and the filter can never lose the "a word must be a pure `[a-z0-9]{2,32}` token" invariant; (2) `internal/handlers/gimmick` — a new command handler following the codebase's one-package-per-handler pattern (pool store seam + fake in unit tests, exactly like the derpies package); (3) `cmd/tugbot/main.go` — wiring (registration, dispatch) plus a multi-chunk follow-up delivery extension, since `list` may exceed Discord's 2000-char message limit and every command response is ephemeral (`FollowupMessageCreate` is the only follow-up endpoint that may carry `MessageFlagsEphemeral`).

**Tech Stack:** Go (the module directive in `go.mod` is `go 1.22`; it builds under the installed go1.26.x toolchain — do NOT touch `go.mod`), discordgo v0.29.0 (slash commands via `ApplicationCommandOptionSubCommand`; ephemeral `FollowupMessageCreate` for overflow), pgx v5 (`pgxpool`), `golang.org/x/text/unicode/norm` (already a derpies dependency).

**Ground rules for every task:** format check = `gofmt -l .` (must print nothing); build = `go build ./...`; vet = `go vet ./...`. DB-touching tests in `cmd/tugbot` and `internal/handlers/derpies` self-skip when the test PG is absent — run `make db-up` first when you need them to actually run. Commit only after that task's acceptance criteria all pass.

---

### Task 1: Extract `internal/wordmatch` (shared word contract; derpies rewires to it)

**Context:** `internal/handlers/derpies/derpies.go` owns two pure helpers: `foldToASCII(s string) string` (NFD-normalize, drop Unicode combining marks `Mn`, then `strings.ToLower`) and `wordValid(w string) bool` (regex `^[a-z0-9]{2,32}$`, precompiled as `wordRe`). The derpies filter's fast path matches FOLDED, punctuation-TRIMMED message tokens against lowercased stored words; the trim happens in `tokensForMatch` (match-time ONLY — never on stored words), so the stored-word list is a pure `[a-z0-9]{2,32}` token space. The new `/gimmick` command (Task 3) must normalize user input with the IDENTICAL fold and validate with the IDENTICAL regex, or it can store words that can never match. Meanwhile `foldToASCII` performs NO punctuation trim (this is load-bearing: `Swift.` folds to `swift.`, which fails `wordValid` and must be REJECTED, not trimmed to `swift`). The contract is extracted verbatim into `internal/wordmatch` so the filter and the command share one source of truth. Existing derpies tests (`TestFoldToASCII`, `TestWordValid` in `internal/handlers/derpies/derpies_test.go`) pin the behavior and MOVE with the code — zero derpies behavior change is mandatory: every remaining derpies test must pass unmodified after this task.

**Files:**
- Create: `internal/wordmatch/wordmatch.go`
- Create: `internal/wordmatch/wordmatch_test.go`
- Modify: `internal/handlers/derpies/derpies.go`
- Modify: `internal/handlers/derpies/derpies_test.go`
- Test: `internal/wordmatch/wordmatch_test.go`

**What to implement:**

`internal/wordmatch/wordmatch.go` — package `wordmatch`, containing:
- `const Module = "wordmatch"` is NOT needed; keep the package minimal.
- `internal/wordmatch/wordmatch.go` imports `strings`, `regexp`, `golang.org/x/text/unicode/norm`, and `unicode`.
- `func FoldToASCII(s string) string` — the BODY of derpies.go's `foldToASCII` moved byte-for-byte (NFD via `norm.NFD.String(s)`, drop runs with `unicode.Is(unicode.Mn, r)`, `strings.ToLower` on the result). Do NOT add any punctuation trim.
- `func WordValid(w string) bool` — derpies's `wordValid` with its precompiled `var wordRe = regexp.MustCompile(` + "`^[a-z0-9]{2,32}$`" + `)` (keep the unexported `wordRe` name or rename to exported `wordRe` — either is fine, it stays private to the package).
- Remove nothing else; the package exports exactly `FoldToASCII` and `WordValid`.

`internal/handlers/derpies/derpies.go` changes (and ONLY these):
- Delete `func foldToASCII` and `var wordRe` + `func wordValid`.
- `tokensForMatch` (unchanged otherwise): the call `out[strings.Trim(foldToASCII(tok), punctTrim)] = true` becomes `out[strings.Trim(wordmatch.FoldToASCII(tok), punctTrim)] = true`.
- Flow step 9: `fw := foldToASCII(word)` becomes `fw := wordmatch.FoldToASCII(word)`; `if !wordValid(fw)` becomes `if !wordmatch.WordValid(fw)`.
- Remove now-unused imports (`golang.org/x/text/unicode/norm`, `unicode`, AND `regexp` — `wordRe` is the file's only `regexp` use, so it becomes unused along with the two helpers). Add `import "github.com/danielcherubini/tugbot/internal/wordmatch"`.
- Update the package doc comment only if it names `foldToASCII`/`wordValid` and would now be wrong — the existing package comment describes the flow in prose ("checked conversion (the house discipline)") and does NOT name the helpers, so leave it.
- DO NOT touch `tokensForMatch`'s punctuation-trim consts, `sortedKeys`, `gimmickPrompt`, the prompt template, image-leg code, or any SQL in `poolStore.listGimmicks`/`addGimmick`/`promptText`.

**Steps:**
- [ ] Create `internal/wordmatch/wordmatch_test.go` moving `TestFoldToASCII` and `TestWordValid` VERBATIM from `derpies_test.go` (all table entries unchanged: `świft→swift`, `žwift→zwift`, `źwift→zwift`, `SWIFT→swift`, `swift`, `zzz`, `""`; `sw1ft`, `zswiftf`, 32×`a` true; 33×`a`, `a`, `s-w1ft`, `swïft`, `sw1ft!` false), but calling the `wordmatch.` qualified names.
- [ ] Run `go test ./internal/wordmatch/ -v`
  - It FAILS TO COMPILE (`use of package wordmatch not intended for goimport` / `undefined package`) — the red state of the extraction.
- [ ] Create `internal/wordmatch/wordmatch.go` with the two functions moved verbatim from `derpies.go`.
- [ ] Run `go test ./internal/wordmatch/ -v`
  - All pass? If any FAILs, the move altered behavior — fix the port, not the test (the cases are the pinned truth), before continuing.
- [ ] Re-wire `derpies.go` per "What to implement" (delete old helpers, switch call sites, fix imports).
- [ ] Run `go test ./internal/handlers/derpies/ -v`
  - ALL tests pass unmodified (this is the zero-behavior-change pin). If `TestTokensForMatch` or any flow test fails, STOP — `foldToASCII` semantics drifted; diff the move.
- [ ] Run `gofmt -l .` → no output; `go build ./...` → succeeds; `go vet ./...` → clean.
- [ ] Commit with message: `wordmatch: extract fold/validate contract from derpies (shared with /gimmick)`

**Acceptance criteria:**
- [ ] `go test ./internal/wordmatch/ ./internal/handlers/derpies/` is fully green with no derpies test modified (only the two moved functions removed).
- [ ] `grep -rn "func foldToASCII\|func wordValid" internal/handlers/derpies/` returns nothing; `grep -n "wordmatch\." internal/handlers/derpies/derpies.go` shows exactly the `tokensForMatch` call site, the flow-step-9 fold call site, and the flow-step-9 validate call site.
- [ ] No derpies prompt/output bytes changed (`TestGimmickPromptDefault` passes untouched).

---

### Task 2: Multi-chunk follow-up delivery in `cmd/tugbot/main.go`

**Context:** Every tugbot slash response is at most ONE message, delivered by `deliverInteraction`'s two branches (fast path: one `InteractionRespond`; slow path: watchdog defer-ACK then ONE `FollowupMessageCreate`). The `/gimmick list` response can exceed Discord's 2000-char message limit (gimmick words are small — max ≈44 chars/line — but the LLM-learnt list grows unbounded), so it must deliver as multiple messages. Channel posts can't be ephemeral (all command responses are ephemeral per the spec), so every overflow chunk travels as an ephemeral `FollowupMessageCreate` after an acknowledged interaction. Design (reviewer-verified semantics): a `reply` gains `chunks []string`; the **delivery set** = `chunks` when non-empty, else `[content]`; **fast path** delivers content via `InteractionRespond` and the REMAINDER via follow-ups; if the fast-path respond FAILS, fall back to the slow path (single defer-ACK, then ALL of the delivery set as follow-ups — never mix a sent initial response with follow-ups for the same reply); **slow path** = defer-ACK (fatal-skip on ACK failure, existing "Fatal work" semantics) + the whole delivery set as follow-ups. Single-message replies (every non-list response in the whole bot) behave EXACTLY as today in the success case, and gain the fallback in the failure case. `handlers.respond` currently swallows its error (log-only), so it gains an `error` return, and a new `respondFu func(reply) error` seam replaces it when set — this lets `TestDeliverInteractionACKRace` (whose bad-token session makes the REAL respond always 401) pin success, failure, and fallback without a live gateway. The `followupFu` seam changes from `func(content string, ephemeral bool) error` to `func(chunks []string, ephemeral bool) error` (stub at `main_test.go:173` records `([]string, bool)`; production runs one `FollowupMessageCreate` per chunk with `MessageFlagsEphemeral` when ephemeral).

**Files:**
- Modify: `cmd/tugbot/main.go` (handlers struct ~line 130-145, `deliverInteraction` ~497-558, `respond` ~748-788, `reply` struct ~565-580)
- Modify: `cmd/tugbot/main_test.go` (`TestDeliverInteractionACKRace`)

**What to implement:**

`cmd/tugbot/main.go`:
1. `reply` struct: add field `chunks []string` (keep `content string`, `ephemeral`, `deferResp`). Add unexported helper on `reply`:
   ```go
   // deliverySet is the message set a follow-up branch must deliver:
   // the chunks when present, else the single content message.
   func (r reply) deliverySet() []string {
       if len(r.chunks) > 0 {
           return r.chunks
       }
       return []string{r.content}
   }
   ```
2. `handlers` struct: change `followupFu func(content string, ephemeral bool) error` → `followupFu func(chunks []string, ephemeral bool) error` (update its doc comment to "substitutes for the post-ACK follow-up message(s)"); add `respondFu func(reply) error` with doc "substitutes for the fast-path respond (see deliverInteraction)."
3. `respond(i *discordgo.Interaction, r reply) error`: same body; on `d.InteractionRespond` failure keep the existing `slog.Error("Cannot respond to slash command", ...)` log AND `return err`. All other behavior (defer arm, ephemeral arm, no components) unchanged.
4. `deliverInteraction` — replace the fast branch and the slow branch's follow-up block:
   - Fast defer arm: `if err := h.callRespond(i, r); err != nil { return }` where `callRespond` is the seam-or-real selector (see 5) — the error is already logged inside `respond`; NO follow-up, NO fallback (cull's defer arm is its own ACK; the existing fatal-skip semantics are preserved).
   - Fast plain arm (inside `claimed.CompareAndSwap(false, true)`):
     ```go
     if err := h.callRespond(i, r); err != nil {
         // Fast-path failure → fallback (never mix a sent initial
         // response with follow-ups for the same reply): single
         // defer-ACK, then deliver the whole delivery set as
         // follow-ups; skip the follow-ups if the ACK itself fails
         // (Fatal-work semantics, same as the slow branch).
         if ackErr := h.watchdogACK(); ackErr != nil {
             return
         }
         h.deliverFollowUps(i, r.deliverySet(), r.ephemeral)
         return
     }
     if len(r.chunks) > 1 {
         h.deliverFollowUps(i, r.chunks[1:], r.ephemeral)
     }
     ```
     Extract into the two existing small helpers so the slow branch shares them: `watchdogACK()` = the slow branch's defer-ACK block (stub-or-real, on error log "Cannot defer slash command (ACK)" and return the error); `deliverFollowUps(i, set []string, ephemeral bool)` = if `h.followupFu != nil` → `h.followupFu(set, ephemeral)`, else a loop: one `fm := &discordgo.WebhookParams{Content: c}` per chunk, `fm.Flags = discordgo.MessageFlagsEphemeral` when ephemeral, `h.app.D.FollowupMessageCreate(i, false, fm)`, each failure logged "Cannot follow up slash command" (per-chunk logging — not once per batch); return an error if ANY follow-up failed (the caller logs/absorbs per the existing style).
   - Slow branch: unchanged structure (urgent-log, `watchdogACK()` with the SAME log-on-failure behavior, `r := <-done`), then `h.deliverFollowUps(i, r.deliverySet(), r.ephemeral)` instead of the single-follow-up block.
   - `callRespond(i, r) error` helper: `if h.respondFu != nil { return h.respondFu(r) }; return h.respond(i, r)`.
   - Update `deliverInteraction`'s doc comment (the `/gulag-release 10062` history text stays): add one line documenting chunk delivery — "multi-chunk replies: fast path responds with chunk 0 and follow-ups the remainder; fast-path failure falls back to the slow path (defer-ACK + the full delivery set as follow-ups); slow path delivers the delivery set (chunks, else content) as follow-ups."
5. Do NOT change: `interactionAckBudget`, the `claimed` race, `dispatchCommand`, `guildSetupFu`/`applyShapeFu`/`checkGuildFu`, registration code, or any handler code.

`cmd/tugbot/main_test.go` — `TestDeliverInteractionACKRace` (keep the existing structure, bad-token session, `interactionAckBudget = 30ms` shrink): RESET the accumulated counters at the top of every arm (`ackCalls, followupContents, followupEphemeral = nil, nil, nil` before each `deliverInteraction` call — the per-arm expectations below are only consistent with fresh counters).
- Update the follow-up stub: `h.followupFu = func(chunks []string, ephemeral bool) error { followupContents = append(followupContents, chunks); followupEphemeral = append(followupEphemeral, ephemeral); return nil }` (record the chunk slice + flag).
- Replace the four existing arms with (same `t.Fatalf` assertion style):
  1. **Fast plain, single message:** `h.respondFu = func(reply) error { return nil }`; `h.deliverInteraction(i, func() reply { return reply{content: "fresh"} })` → want `ackCalls == 0`, ZERO follow-ups (guard: the real 401 path is arm 3's job).
  2. **Fast defer response (cull's contract):** `h.respondFu = func(reply) error { return nil }`; `reply{content: "cull", deferResp: true, ephemeral: true}` → want `ackCalls == 0`, zero follow-ups (unchanged semantics).
  3. **Fast plain, respond FAILURE (new — exercises the fallback; the real session's 401 makes this arm runnable without a stub):** `h.respondFu = nil` (falls back to the real `respond` → 401, logged), empty chunks: `reply{content: "fresh"}` → want `ackCalls == 1`, follow-ups == `[[fresh]]`, ephemeral flag `false`.
  4. **Fast plain, multi-chunk (success arm of the fan-out):** `h.respondFu = func(reply) error { return nil }`; `reply{content: "c0", chunks: []string{"c0", "c1", "c2"}, ephemeral: true}` → want `ackCalls == 0`, follow-ups == `[[c1 c2]]`, flag `true` (chunk 0 rides the interaction; the remainder follow up).
  5. **Fast plain multi-chunk, respond failure (full-set fallback):** `h.respondFu = nil`; `reply{content: "c0", chunks: []string{"c0", "c1"}}` → want `ackCalls == 1`, follow-ups == `[[c0 c1]]` (no mix: nothing was delivered initially).
  6. **Slow single message (single-message delivery-set arm — the pass-4 gap regression guard):** `reply{content: "solo"}` (sleeps 150ms) → want `ackCalls == 1`, follow-ups == `[[solo]]`, flag `false`.
  7. **Slow multi-chunk:** sleep 150ms, `reply{content: "s0", chunks: []string{"s0", "s1", "s2"}}` → want `ackCalls == 1`, follow-ups == `[[s0 s1 s2]]`, flag `false` (replace the existing "slow" + "slow ephemeral" arms; keep one ephemeral variant: same reply with `ephemeral: true` → flag `true`).

**Steps:**
- [ ] Write the updated `TestDeliverInteractionACKRace` in `cmd/tugbot/main_test.go` per the arm list above (referencing the new `respondFu` field and the `func(chunks []string, ephemeral bool) error` `followupFu` signature).
- [ ] Run `go test ./cmd/tugbot/ -run TestDeliverInteractionACKRace -v`
  - It FAILS TO COMPILE (undefined `respondFu`, signature mismatch) — the red state.
- [ ] Implement the `main.go` changes per "What to implement" (reply.chunks + deliverySet, seam changes, `respond` error return, `deliverInteraction` branch surgery, `callRespond`/`watchdogACK`/`deliverFollowUps` helpers, comment updates).
- [ ] Run `go test ./cmd/tugbot/ -run TestDeliverInteractionACKRace -v`
  - All seven arms pass? If any `ackCalls`/follow-up count is off, the branch surgery broke a delivery path — debug `deliverInteraction` directly, do not relax an assertion.
- [ ] Run `go test ./cmd/tugbot/ -v` (DB-touching tests self-skip without PG; the ACK-race test must run and pass without PG), then `gofmt -l .` → nothing, `go build ./...` → succeeds, `go vet ./...` → clean.
- [ ] Commit with message: `main: multi-chunk follow-up delivery for overlong list responses (delivery set + fast-path fallback)`

**Acceptance criteria:**
- [ ] `go test ./cmd/tugbot/` green (non-DB tests; DB tests skip cleanly without PG).
- [ ] No existing non-list command's behavior changed: a `reply` with empty `chunks` takes exactly the same code path as before on success (arm 1/6 pin this).
- [ ] The file `cmd/tugbot/main.go` compiles with `respond` returning `error` and no other caller of `respond` changed (grep `h.respond(` → only `deliverInteraction` via `callRespond`).

---

### Task 3: The `internal/handlers/gimmick` command package

**Context:** The command itself, following the codebase's one-package-per-handler pattern (mirroring `cull`'s `New(app *app.App)` with its own `gulag.New(app)` member, and `derpies.go`'s `store`-seam + fake test pattern). Behavior (reviewer-verified spec): guild guard → role gate (Highly Regarded + admin via `gulag.MemberHasAnyRole`) → subcommand dispatch; NO feature-flag check (deliberate — `docs/decisions/0003-gimmick-command-ungated.md`: the list is pre-seedable while the derpies feature is off; the role gate covers the write cost, and the log line with the invoker is the trace); all responses ephemeral. Words are normalized with `wordmatch.FoldToASCII` (NO punctuation trim — see Task 1), validated with `wordmatch.WordValid`, inserted with `source = 'manual'` (a new value the `varchar(8)` column fits; distinct from `seed` = migration-seeded and `llm` = runtime-learnt). The write path is idempotent (`ON CONFLICT (word) DO NOTHING`; an existing `seed`/`llm` row's `source` is never modified). Deletion is an exact `WHERE word = $1` on the normalized word (stored words always satisfy the contract, so an invalid token can never match); there is no cascade (no FK on the table) and it takes effect on the next message (the filter re-reads the list per event). The `list` response is pre-sliced into `Response.Chunks` (Task 2's delivery set carries them: first chunk `Gimmick words (N): ` with N = total line count, continuation chunks `Gimmick words (continued): `; greedy whole-line packing, ≤2000 chars including the header — all content is ASCII so rune length == byte length).

**Files:**
- Create: `internal/handlers/gimmick/gimmick.go`
- Create: `internal/handlers/gimmick/gimmick_test.go`

**What to implement:**

`internal/handlers/gimmick/gimmick.go`:
- `const Module = "gimmick"` (slog tag), `const SourceManual = "manual"`.
- `type gimmickRow struct{ Word, Source string }` (the `created_at` column is not part of the command surface).
- `type Response struct{ Content string; Ephemeral bool; Chunks []string }` (contract: when `Chunks` non-empty, `Content == Chunks[0]`).
- Type:
  ```go
  type store interface {
      listGimmicks(ctx context.Context) ([]gimmickRow, error)
      addGimmick(ctx context.Context, word, source string) (int64, error)
      deleteGimmick(ctx context.Context, word string) (int64, error)
  }
  ```
- Type:
  ```go
  type gateOps interface {
      guildMember(guildID, userID string) (*discordgo.Member, error)
      memberHasAnyRole(ctx context.Context, guildID string, m *discordgo.Member) bool
  }
  type realGateOps struct {
      session *discordgo.Session
      g       *gulag.Gulag
  }
  ```
  `realGateOps.memberHasAnyRole` delegates to `g.MemberHasAnyRole(ctx, guildID, m, "Highly Regarded", "admin")` (Role order pinned matching `cull.whitelistRoles()`); `guildMember` delegates to `session.GuildMember`.
- `type Gimmick struct{ app *app.App; store store; gate gateOps }` and `func New(a *app.App) *Gimmick { return &Gimmick{app: a, store: &poolStore{pool: a.Pool}, gate: &realGateOps{session: a.D, g: gulag.New(a)}} }` (cull mirror: its own `gulag.New(a)`).
- `poolStore` (raw SQL over `a.Pool`, mirroring `derpies.poolStore`):
  ```go
  func (p *poolStore) listGimmicks(ctx context.Context) ([]gimmickRow, error) {
      rows, err := p.pool.Query(ctx, `SELECT word, source FROM derpies_gimmicks ORDER BY word`)
      // scan into gimmickRow, standard rows.Next/rows.Err pattern
  }
  func (p *poolStore) addGimmick(ctx context.Context, word, source string) (int64, error) {
      res, err := p.pool.Exec(ctx,
          `INSERT INTO derpies_gimmicks (word, source) VALUES ($1, $2) ON CONFLICT (word) DO NOTHING`,
          word, source)
      if err != nil { return 0, err }
      return res.RowsAffected(), nil
  }
  func (p *poolStore) deleteGimmick(ctx context.Context, word string) (int64, error) {
      res, err := p.pool.Exec(ctx, `DELETE FROM derpies_gimmicks WHERE word = $1`, word)
      if err != nil { return 0, err }
      return res.RowsAffected(), nil
  }
  ```
- `func (h *Gimmick) SetupCommand() *discordgo.ApplicationCommand` — the shape (pin test):
  ```go
  &discordgo.ApplicationCommand{
      Type:        discordgo.ChatApplicationCommand,
      Name:        "gimmick",
      Description: "Manage the gimmick word list (derpies filter)",
      Options: []*discordgo.ApplicationCommandOption{
          {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Add a gimmick word",
           Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "word", Description: "The respelling to add (e.g. sw1ft)", Required: true}}},
          {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List all gimmick words"},
          {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "delete", Description: "Delete a gimmick word",
           Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "word", Description: "The word to delete", Required: true}}},
      },
  }
  ```
- `func (h *Gimmick) HandleInteraction(i *discordgo.Interaction) Response` — gate order (all returns ephemeral via a private `ephemeral(content string) Response` helper):
  1. `i.GuildID == ""` → `Response{Content: "This command can only be used in a guild", Ephemeral: true}`
  2. Invoker ID: `i.Member != nil && i.Member.User != nil` → `i.Member.User.ID`, else `i.User != nil` → `i.User.ID`, else `""`; fetch via `h.gate.guildMember(i.GuildID, invokerID)`; fetch error → `Error: Could not verify your permissions`; `!h.gate.memberHasAnyRole(ctx, i.GuildID, member)` → `Error: You need Highly Regarded or admin role to use this command`.
  3. Subcommand: assert `i.Data.(discordgo.ApplicationCommandInteractionData)`; `data.Options[0]` (the subcommand option; missing/non-option → `Unknown subcommand`); arm by `sub.Name`.
  4. **add** — `word` option from `sub.Options` by name (missing → `Missing required option: word`); `raw, _ := opt.Value.(string)`; `w := wordmatch.FoldToASCII(raw)`; `if !wordmatch.WordValid(w)` → ``Invalid word "<w>": 2-32 lowercase letters or digits only``; `n, err := h.store.addGimmick(ctx, w, SourceManual)`; `err` → `slog.Error("gimmick add failed", "module", Module, "word", w, "user", invokerID, "guild", i.GuildID, "error", err)` + `Failed to add ` + w; `n == 1` → `slog.Info("gimmick add", "module", Module, "word", w, "source", SourceManual, "user", invokerID, "guild", i.GuildID)` + `Added ` + w + ` to the gimmick list`; else → `slog.Info("gimmick add skipped (already exists)", ...same fields...)` + w + ` is already in the list` (idempotent; do NOT re-insert or touch the existing row's `source`).
  5. **list** — `rows, err := h.store.listGimmicks(ctx)`; `err` → `Failed to query gimmick words. Please try again later.` (single message, no chunks); `len(rows) == 0` → `No gimmick words yet.`; else build lines `fmt.Sprintf("%s (%s)", r.Word, r.Source)` and `chunks := chunkGimmickList(len(rows), lines)` (the package-level pure function; note: `total` always equals `len(lines)` and is used only in the header); return `Response{Content: chunks[0], Ephemeral: true, Chunks: chunks}`.
  6. **delete** — same option/normalization arms as add; `n, err := h.store.deleteGimmick(ctx, w)`; `err` → `slog.Error("gimmick delete failed", ...)` + `Failed to delete ` + w; `n == 1` → `slog.Info("gimmick delete", "module", Module, "word", w, "user", invokerID, "guild", i.GuildID)` + `Deleted ` + w + ` from the gimmick list`; else → w + ` is not in the list`.
- Chunking helper (pure, directly testable — pin the boundary):
  ```go
  // chunkGimmickList packs whole lines under the 2000-char limit
  // (content is pure ASCII, so rune len == byte len). First chunk:
  // "Gimmick words (N): " + lines; continuation chunks:
  // "Gimmick words (continued): " + lines. Splits at line boundaries
  // only; a fresh chunk always fits (max line ≈ 44 chars).
  func chunkGimmickList(total int, lines []string) []string {
      // greedy: start a chunk with the appropriate header; append the
      // next line while len(header + "\n" + joined lines) <= 2000;
      // flush and open a new chunk (headerC) when the next line would
      // exceed; flush the remainder at the end. Each chunk's final
      // message text — including its header — is <= 2000 chars.
  }
  ```
  (Implementation detail: `join = header + "\n" + strings.Join(lineSoFar, "\n")` when lines exist, `header` alone when none — the empty-list case never reaches this function.)

`internal/handlers/gimmick/gimmick_test.go` (fake store + fake gateOps; construct `Gimmick` by assigning the struct fields directly, mirroring `derpies_test.go`):
- `TestSetupCommandShape`: pins `SetupCommand()` — name `gimmick`, `ChatApplicationCommand` type, exactly three subcommands `add`/`list`/`delete`, the `word` string option (`Required: true`) on `add` and `delete`, no options on `list`, correct descriptions.
- Interaction builders: a helper `cmdInteraction(guildID string, subName string, wordOpt interface{}) *discordgo.Interaction` building an `ApplicationCommandInteractionData` with one subcommand option (whose `Options` holds the `word` option when `wordOpt != nil`); plus a `fakeGate` controlling member-fetch success/failure and role allow/deny, and a `fakeStore` implementing the three store methods with scripted results and call recording (mirror `derpies_test.go`'s fakeStore shape — record `added []string` as "word|source" and `deleted []string`).
- Arms (assert EXACT `Response.Content`, and `Ephemeral == true` on every arm):
  - Guild guard: empty `GuildID` → `This command can only be used in a guild` (gate not consulted — fake records zero calls).
  - Role gate: denied → `Error: You need Highly Regarded or admin role to use this command`; member-fetch failure → `Error: Could not verify your permissions`; allowed → proceeds (store consulted).
  - Unknown subcommand (after an allowed gate): → `Unknown subcommand`.
  - Missing `word` option, `add`/`delete`: → `Missing required option: word`.
  - No-trim normalization arms (exact input → expected): `"Swift."` → folds to `swift.` → `Invalid word "swift.": 2-32 lowercase letters or digits only`; `"ŚW1FT"` → `sw1ft` → `Added sw1ft to the gimmick list` (and `fakeStore` records `added == []string{"sw1ft|manual"}`); `"sw 1ft"` (space survives fold) → `Invalid word "sw 1ft": 2-32 lowercase letters or digits only`.
  - `add` existing: `addGimmick` returns 0 → `sw1ft is already in the list`, store called once.
  - `add` DB error: store returns error → `Failed to add sw1ft`.
  - `delete` found: → `Deleted sw1ft from the gimmick list`; absent: → `sw1ft is not in the list`; DB error: → `Failed to delete sw1ft` (assert the fake received the FOLDED word, e.g. input `"SW1FT!"`… — use `"SW1FT"` → `sw1ft`).
  - `list`: 3-word result → `Content == "Gimmick words (3): \nswift (seed)\nsw1ft (manual)\nzswift (llm)"` (sorted by the fake's ORDERED rows, single chunk: `len(Chunks) == 1`, `Chunks[0] == Content` — the `Content == Chunks[0]` contract arm); empty → `No gimmick words yet.` with `Chunks == nil`; DB error → `Failed to query gimmick words. Please try again later.` with `Chunks == nil`.
  - `list` chunking: a `fakeStore` with 60 scripted rows — each word = `strings.Repeat("a", 30) + fmt.Sprintf("%02d", i)` so every line is a 38-char `aaaa…NN (llm)` (total ≈ 2386 chars: 60×38 lines + 59 newlines + both headers > 2000, so a split is guaranteed) → assert: `len(Chunks) == 2` (deterministic boundary: chunk 1 = 20-char first header + 50 lines × 39 = 1970 ≤ 2000; the 51st line would reach 2009 → flush; chunk 2 = 27-char continuation header + 10 lines × 39 = 417); `Content == Chunks[0]`; every `len(chunk) <= 2000`; `Chunks[0]` starts with `Gimmick words (60): `; `Chunks[1]` starts with `Gimmick words (continued): `; every line whole (check by rejoining: the concatenation of the line bodies across chunks has no line names mangled, i.e. each chunk's post-header text splits on `\n` into complete 38-char `aaaa…NN (llm)` tokens); `total line count across chunks == 60`.
  - No feature-flag anywhere: a test asserting the package has no reference to the `features` package (grep-level: `grep -c "features" internal/handlers/gimmick/*.go` → 0) — reviewer-verified design constraint from ADR 0003.
  - No `slog`-dependent assertions (logs are `slog.Info`/`slog.Error` with `module` = `gimmick`; no test asserts log output).

**Steps:**
- [ ] Write `internal/handlers/gimmick/gimmick_test.go` (full arm list above) so the package does NOT yet exist.
- [ ] Run `go test ./internal/handlers/gimmick/ -v`
  - It FAILS TO COMPILE (`no required module provides package …/gimmick` / undefined `Gimmick`) — the red state.
- [ ] Implement `internal/handlers/gimmick/gimmick.go` per "What to implement".
- [ ] Run `go test ./internal/handlers/gimmick/ -v`
  - All arms pass? The chunking arm must produce `len(Chunks) == 2` at the deterministic boundary above — a result of 1 means the arithmetic/data is wrong (recompute the char counts), a chunk over 2000 means the greedy check is off-by-one — fix the loop, not the data.
- [ ] Run `gofmt -l .` → nothing; `go build ./...` → succeeds; `go vet ./...` → clean.
- [ ] Commit with message: `gimmick: the /gimmick add|list|delete command (manual source, no flag gate, ephemeral, role-gated)`

**Acceptance criteria:**
- [ ] `go test ./internal/handlers/gimmick/` fully green, all arms from the arm list (including the 60-row chunk split with the `<= 2000` + header-order assertions).
- [ ] `grep -rn "features\." internal/handlers/gimmick/` → no matches (the ADR 0003 constraint).
- [ ] `wordmatch.FoldToASCII` / `wordmatch.WordValid` are the ONLY normalization/validation call sites (no hand-rolled `strings.ToLower` in the handler).
- [ ] The `poolStore` SQL is exactly the three statements pinned above.

---

### Task 4: Wire `/gimmick` into `cmd/tugbot/main.go`

**Context:** Registration, dispatch, and the bookkeeping that the 13th handler's presence affects. Registration follows the existing per-guild pattern in `registerCommands`: the five non-gulag shapes (AI Slop, horny, phony, feature, cull) registered one-`applyShape`-per-guild from the `servers` table slice — `gimmick` joins that list after `h.cull.SetupCommand()` (it is a Go-native command, so it appends to the end, not inserted into the Rust parity vector). `dispatchCommand` gets a `case "gimmick"` mapping `Response{Content, Ephemeral, Chunks}` → `reply{content, ephemeral, chunks}` (Task 2's delivery set does the rest — no further `main.go` change is needed). Three "all twelve handlers" strings in `main.go` (the `newHandlers` doc at ~147, the `--selftest` flag help at ~233, and the selftest log line at ~289) are bumped to "thirteen", as are the "nine commands" / "five non-gulag shapes" comments (`main.go` ~142 `applyShapeFu` doc, ~569 `reply` doc, ~709 `registerCommands` doc — it straddles a line break; treat as one logical comment — and ~751 `respond` doc), the `OnReady` comment "command registration (9 shapes)" at ~441 → "(10 shapes)", and `main_test.go`'s `TestRegisterCommandsUsesReadySliceRustOrder` (its `want` slice gains `999/gimmick` and the "five non-gulag shapes" comment becomes six).

**Files:**
- Modify: `cmd/tugbot/main.go`
- Modify: `cmd/tugbot/main_test.go`

**What to implement:**

`cmd/tugbot/main.go`:
1. Import `"github.com/danielcherubini/tugbot/internal/handlers/gimmick"`.
2. `handlers` struct: add field `gimmick *gimmick.Gimmick` (place it after `cull`).
3. `newHandlers`: add `gimmick: gimmick.New(a)` (after `cull: cull.New(a)`); update its comment "constructs all twelve handlers" → "constructs all thirteen handlers".
4. `registerCommands`:
   - Add `h.gimmick.SetupCommand()` after `h.cull.SetupCommand()` in the `shapes` slice.
   - Append `"gimmick"` to the final log line's name array (`slog.Info("I now have the following guild slash commands: ...")` — now 10 names).
   - Update its doc comment: "EXACTLY the nine command shapes (7 slash + 2 message-kind...)" → "EXACTLY the ten command shapes (8 slash + 2 message-kind...)" and add `gimmick` to the prose enumeration.
5. `dispatchCommand`: add
   ```go
   case "gimmick":
       r := h.gimmick.HandleInteraction(i)
       return reply{content: r.Content, ephemeral: r.Ephemeral, chunks: r.Chunks}
   ```
   (before the `default` arm); update its doc comment's arm list to include `gimmick`.
6. `--selftest` flag help text: "…construct the discordgo session and all twelve handlers" → "all thirteen handlers"; the selftest log line (at ~289) `"selftest: Discord session and all twelve handlers constructed"` → `"selftest: Discord session and all thirteen handlers constructed"`.
7. Other comment bumps: `applyShapeFu` doc "the five non-gulag shape" → "the six non-gulag shape"; `OnReady` comment "command registration (9 shapes)" (~441) → "command registration (10 shapes)"; `reply` struct doc "none of the nine commands become components" → "…the ten commands…"; `respond` doc "…the nine commands…" → "…the ten commands…".
8. DO NOT change: `readyThreeWay`, the `gulag`-shape registration block, the `deferResp`/ACK logic, or any task-2 code.

`cmd/tugbot/main_test.go` — `TestRegisterCommandsUsesReadySliceRustOrder`:
- `want` slice gains `"999/gimmick"` after `"999/cull"`.
- Update the test doc comment: "…on the five non-gulag shapes (M11: AI Slop, horny, phony, feature, cull)" → "…on the six non-gulag shapes (M11: AI Slop, horny, phony, feature, cull, gimmick)".
- Keep the assertion-style error message format (the `t.Errorf` inline vector-order message gains `gimmick`).

**Steps:**
- [ ] Apply the `main_test.go` changes first (the red test).
- [ ] Run `make db-up` (if the compose PG isn't already up), then `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./cmd/tugbot/ -run TestRegisterCommandsUsesReadySliceRustOrder -v`
  - Empirically verified by the plan reviewer: the compose PG creates ONLY the `tugbot` database, so every DB-touching test (which defaults to a `tugbot_test` URL) self-SKIPS under plain `go test` even with `make db-up` — the `TUGBOT_TEST_DATABASE_URL` override (exactly what `make test-db` does) is what makes this test actually run. Under that override the red state: FAIL — `shape registrations: got [...five...], want [...six...]`. (If the test SKIPs instead of failing, the URL override is not applied — check the env prefix of the command; a skip is NOT the red state.)
- [ ] Apply the `main.go` changes per "What to implement".
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./cmd/tugbot/ -run TestRegisterCommandsUsesReadySliceRustOrder -v` → passes with all six shapes registered (MUST run and not SKIP — the URL override guarantees it).
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./cmd/tugbot/ -v` (all green / clean-skip — DB-touching tests run against the compose PG via the URL override), `gofmt -l .` → nothing, `go build -o /tmp/tugbot-build ./cmd/tugbot` → succeeds, `go vet ./...` → clean.
- [ ] Commit with message: `main: wire /gimmick (registration, dispatch, 13-handler bookkeeping)`

**Acceptance criteria:**
- [ ] `TestRegisterCommandsUsesReadySliceRustOrder` (run under the `TUGBOT_TEST_DATABASE_URL` override; it MUST run, not skip) passes with `999/gimmick` last in the order.
- [ ] `grep -n "thirteen handlers" cmd/tugbot/main.go` → exactly 3 hits; `grep -cn "twelve handlers" cmd/tugbot/main.go` → 0.
- [ ] `grep -cw nine cmd/tugbot/main.go` → 0; `grep -cw nine cmd/tugbot/main_test.go` → 0; `grep -c "five non-gulag" cmd/tugbot/main.go` → 0; `grep -c "five non-gulag" cmd/tugbot/main_test.go` → 0 (whole-word `nine` catches every bumped comment — including the line-straddling `registerCommands` doc and the ~441 one — because no legitimate "nine" remains after the bumps).
- [ ] `go build ./...` succeeds and the built binary's `--selftest` (run with `make db-up` and the compose `DATABASE_URL`) logs `selftest: Discord session and all thirteen handlers constructed` and exits 0.

---

### Task 5: Full-suite verification pass (build, test, vet, selftest)

**Context:** The final gate before done-when: prove the whole feature (all four commits) is green at the suite level, that the DB-touching tests actually RUN (not skip) against the compose PG, and that the selftest — the CI surface proxy for "can the bot start?" — constructs all 13 handlers and exits 0. The DB-touching tests MUST be run with `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot` (compose PG only has `tugbot` — the tests' `tugbot_test` defaults would otherwise self-skip; this is `make test-db`'s mechanism). Operational note (per the Makefile's own warning): running DB tests against the shared compose DB leaves state residue (the derpies integration test drops/recreates `derpies_gimmicks` and can leave the migration tracker unsettled); the documented remedy is `docker compose down -v` before a later `make migrate`. This task is a verification pass with no planned code changes; if a failure surfaces, fix it at the smallest scope and re-run the failing suite before committing the fix (commit message: `fix: …` describing the specific break). Do not "fix" a test assertion to make a failing test pass without first confirming the test's expected value is the pinned truth.

**Files:**
- Modify: (none expected — only if a fix surfaces.)

**Steps:**
- [ ] Run `make db-up` (start the compose PG; `docker compose up -d postgres` if the alias is stale).
- [ ] Run `go build ./...`
  - Succeeds? If not, fix and repeat.
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./... -v` (the FULL suite against the live PG; derpies/cull/main DB-touching tests this time actually run via the URL override — `make test-db` is NOT used, the PG is already up and plain `go test ./...` runs them).
  - All pass / only cleanly documented skips? If a DB-touching test SKIPs here, the URL override is missing — that is NOT a legitimate skip; re-run with the env prefix. If any FAILs, identify the package and run just that package with `-v` and the URL override first; fix; re-run the full suite.
- [ ] Run `go vet ./...`
  - Clean? Fix if not.
- [ ] Run `gofmt -l .`
  - No output? If any file is listed, `gofmt -w` it and re-check diffs for accidental reformatting.
- [ ] Run `make lint` (golangci-lint + vet)
  - Clean? Fix findings; re-run.
- [ ] Run `go run ./cmd/tugbot --selftest`
  - It must print config-loaded, pool-connected, session+thirteen-handlers constructed, and exit 0 (this is the selftest gate of Task 4, re-verified at the suite level).
- [ ] If any fix was committed: re-run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./... -v` once more after the fix commit to confirm green.
- [ ] Commit with message: `verify: /gimmick feature gate — full suite + selftest green` (ONLY if Task 5 required a fix commit; otherwise no commit — the feature is done over Tasks 1-4).

**Acceptance criteria (done-when, all of these are observable):**
- [ ] `go build ./...` succeeds; `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./...` (with the compose PG) is fully green; `go vet ./...` clean; `gofmt -l .` silent; `make lint` clean.
- [ ] `go run ./cmd/tugbot --selftest` (with `make db-up`) logs `selftest: Discord session and all thirteen handlers constructed` and exits 0.
- [ ] In Discord (post-deploy, per cutover runbook): the `gimmick` slash command appears in shared/guild-picker with the three subcommands; `/gimmick list` returns an ephemeral `Gimmick words (N): …` reply; a Highly Regarded/admin member's `/gimmick add sw1ft` inserts a `source='manual'` row (visible via `/gimmick list` as `sw1ft (manual)`); a non-privileged member's attempt gets `Error: You need Highly Regarded or admin role to use this command`; `/gimmick delete sw1ft` removes the row; and a derpies-filtered user's next `sw1ft` message is deleted (filter re-reads the list per event — no restart).
