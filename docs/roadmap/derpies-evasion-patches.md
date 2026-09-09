---
status: shipped
done-when: A gated author's guild nick, when set to a gimmick name, is reset to the literal neutral name "Derpies" (not cleared to null); and any GUILD_MESSAGE_UPDATE by a gated author re-runs the full derpies create flow (fast path, images/repeat-image, referenced fetch, one slow-path ask, learn, delete) on the updated content. Full gate green: `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` + `go run ./cmd/tugbot --selftest` logs "Discord session and all thirteen handlers constructed" and exits 0.
---

# Derpies evasion patches: edit re-judgment + "Derpies" name reset

**Goal:** Close the two evasion vectors a gated author found: (1) he re-judges free to edit an already-posted message to carry a gimmick (the filter watched `MessageCreate` only), and (2) the nickname "reset" cleared the nick to null, which exposes his global display name (the bot cannot modify another user's global name) — so the reset now sets the guild nick to the fixed neutral name `Derpies`, and the name reset action no longer depends on the verdict word being learnable.
**Architecture:** Two additive changes inside the existing `internal/handlers/derpies` package — a reworked action arm in `nicknames.go` (seam rename `clearNickname` → `setNickname` in `derpies.go`) and a new `edits.go` entry point that funnels `MessageUpdate` into the existing `flow()`. No table, no migration, no new handler struct (the selftest's "thirteen handlers" is unchanged).
**Tech Stack:** Go, discordgo v0.29.0, the package's existing fake seams (`fakeStore`/`fakeOps`/`fakePi`) for tests.

## Decisions already made (do not re-litigate)

- **Name reset value is a literal const** `Derpies`. Rationale (user-confirmed): this filter gates exactly one user; the bot has no API to modify a user's global name, so the only name lever is the guild nick; setting it to a fixed neutral value MASKS the global name (a clear, by contrast, exposes it).
- **Name action is decoupled from the learning gate.** On the nickname flow, ANY parseable `GIMMICK` verdict resets the nick to `Derpies`. Learning a word into `derpies_gimmicks` still requires the existing two-arm gate (wordValid on the folded word AND folded word present in the nick's folded tokens); a non-learnable verdict resets the name and logs, but stores nothing. THIS DECOUPLING IS NICKNAME-ONLY: the message flow is UNCHANGED — an unlearnable word in a message still deletes nothing (the two-arm gate there gates BOTH learning and deletion; do not touch it).
- **Every edit by a gated author re-runs the full create flow** (no content-diffing cache). An edit is at most one pi ask + one list SELECT — the same cost profile as a create, and burst amplification is an accepted spec limitation.
- **Empty update payload is fetched** via the existing `channelOps.channelMessageRetrieve` seam (the gokupoll precedent, mod.rs:126-136). Fetch failure logs and skips (never aborts).
- No bot message, reaction, or gulag on any new path — logging only, flow discipline unchanged.

---

### Task 1: Nickname reset to "Derpies" (decoupled action arm)

**Context:**
`internal/handlers/derpies/nicknames.go` judges a gated member's guild nick (fast-path token match against `derpies_gimmicks`, else one pi ask) and, on a GIMMICK verdict, today calls `h.ops.clearNickname`, which PATCHes `{"nick": null}` — display then falls back to the member's GLOBAL name, which the bot cannot touch. Two problems this task fixes: (a) the clear exposes the global name; (b) the current step-10 gate makes the WHOLE arm (including the nickname change) a no-op whenever the verdict word is unlearnable (e.g. LLM answers the word but the as-appears nick folds to a token the LLM didn't name, or a dead-end nick) — so a recognizable gimmick name survives. After this task: a GIMMICK verdict ALWAYS resets the nick to the const `Derpies`; learning happens only when the existing gate passes; all 60s coalescing / busy / lastNick change-detection mechanics are UNCHANGED, except the success-path cache now records `Derpies` (so the gateway echo of our own set — `Nick == "Derpies"` — is skipped by the existing change-detection, exactly as the null-clear's echo `Nick == ""` was skipped by the empty-nick arm… note: the echo now arrives with `Nick != ""`, so it is skipped by the `cur == evt.Nick` check in step "change detection" (line ~86 of nicknames.go), NOT by the empty-nick arm).

**Files:**
- Modify: `internal/handlers/derpies/nicknames.go`
- Modify: `internal/handlers/derpies/derpies.go` (the `discordOps` interface + `realOps` impl + the `lastNick` doc comment on `Derpies`)
- Modify: `internal/handlers/derpies/derpies_test.go` (`fakeOps`)
- Test: `internal/handlers/derpies/nicknames_test.go`

**What to implement:**

1. `derpies.go`:
   - In `type discordOps interface`: replace the `clearNickname(guildID, memberID string) error` method (and its comment) with `setNickname(guildID, memberID, nick string) error` — comment: "set the guild nickname (the reset is a fixed neutral value, not a clear)".
   - In `realOps`: replace the `clearNickname` method (lines ~182-190, including its whole "reset to default" doc comment) with:
     ```go
     // setNickname — set the guild nickname (the reset value; Discord's
     // member PATCH accepts a nick string — no null trick needed now).
     func (o *realOps) setNickname(guildID, memberID, nick string) error {
         _, err := o.d.Request("PATCH", "/guilds/"+guildID+"/members/"+memberID, map[string]any{"nick": nick})
         return err
     }
     ```
   - Update the `lastNick` field's doc comment: after a successful reset the cache holds `derpiesNickReset` (the echo of our own set is skipped via the cur == evt.Nick check); `""` still means "no nick known / cleared state" (failure arms and the empty-nick arm write `""`).
2. `nicknames.go`:
   - Add a package-level const with a rationale comment (single gated user; the bot cannot modify a global name; a fixed neutral value masks the global display name):
     ```go
     // derpiesNickReset — the fixed value the reset SETS (not clears) the
     // guild nick to. This filter gates exactly one user; the bot has no
     // API to modify another user's GLOBAL name, so the neutral value
     // masks it (a null clear would expose it).
     const derpiesNickReset = "Derpies"
     ```
   - Rename `clearNow` → `resetNow` (same signature `(key string, evt *discordgo.GuildMemberUpdate, path, word string)`), change the call to `h.ops.setNickname(evt.GuildID, evt.User.ID, derpiesNickReset)`, and change the SUCCESS arm from `h.saveNick(key, "")` to `h.saveNick(key, derpiesNickReset)` (the echo arrives with `Nick == derpiesNickReset`; the change-detection `cur == evt.Nick` skips it BEFORE the empty-nick arm is ever relevant). The FAILURE arm keeps `h.markEdit(key)` + `h.saveNick(key, "")` exactly as today (the nick is unchanged on failure; `""` baseline keeps the post-window same-nick retry working). Update the log lines from "nickname cleared/...failed" to "derpies nickname reset (fast|llm)" / "derpies nickname reset (...) failed". Update the method's leading doc comment accordingly (the old comment's null-echo explanation is replaced by the `Derpies`-echo explanation; keep the 429-discipline explanation).
   - Restructure the GIMMICK arm (current steps 10-11, after the `clean`/`unknown` switch arms return):
     ```go
     // 10. Learn — the two-arm gate is UNCHANGED and now gates LEARNING
     //     ONLY (a recognized gimmick name is reset even when the word
     //     isn't learnable; what may enter the table is not loosened).
     fw := wordmatch.FoldToASCII(word)
     if wordmatch.WordValid(fw) && toks[fw] {
         if err := h.store.addGimmick(ctx, fw, SourceLLM); err != nil {
             slog.Error("derpies nickname add gimmick failed", "module", module, "word", fw, "error", err)
         }
     } else {
         slog.Warn("derpies nickname verdict word not learnable — name reset only", "module", module, "word", word, "nick", evt.Nick)
     }
     // 11. Reset (ALWAYS, on a GIMMICK verdict) — the name action no
     //     longer depends on the learning gate.
     h.resetNow(key, evt, "llm", fw)
     ```
     The `clean` arm (saveNick + return) and `unknown` arm (log + saveNick + return) are unchanged. The fast path (step 6) now calls `resetNow` (name only — a fast hit is already-known, no addGimmick, same as today's fast clear).
   - Comment touch-ups cover ALL old-clear wording, wording-only (no logic): in addition to the `markEdit`/`saveNick`/step-5/step-4 comments, also update the `nicknames.go` package-level doc comment (the "a successful clear writes `\"\"` into the cache" paragraph → the reset value is written into the cache BEFORE the echo arrives), the `derpies.go` package doc ("one clear attempt ..." / "clears their nicks" → reset phrasing), the `Derpies` struct doc, `New()`'s doc, and the `lastNick` field comment (item 1). The 429-discipline text stays. Do NOT change any gate, cooldown, or map structure.
3. `derpies_test.go` — `fakeOps`: replace the `clearNickname` method and its `clearErr`/`clears`/`clearArgs` fields with:
   ```go
   // setNickname records EVERY attempt (including a failed one) then
   // returns setErr — the nickname flow's window discipline is asserted
   // on attempt counts, not successes.
   setErr   error
   sets     int
   setArgs  []string // "guildID|memberID|nick"
   ```
   and `func (o *fakeOps) setNickname(guildID, memberID, nick string) error { o.sets++; o.setArgs = append(o.setArgs, guildID+"|"+memberID+"|"+nick); return o.setErr }` — same "record before return" discipline. Update every other file that touches these fake fields (search `.\clears\b|clearArgs|clearErr` across the derpies package).
4. `nicknames_test.go` — update the helpers (`assertNoEdits` now checks `ops.sets == 0` and logs `setArgs`) and adapt every existing test that asserts the old clear semantics (assert `setArgs` ends in `|Derpies`). **TWO EXISTING TESTS FLIP — named so an executor doesn't have to discover them from failures**: `TestNickFlowVerdictInvalidWord` (verdict `GIMMICK:x` — one char, fails `WordValid`) and `TestNickFlowVerdictNotInNickname` both today assert `assertNoEdits` + nothing-learned; under the ANY-parseable-GIMMICK-resets decision they flip to **reset WITHOUT learning**: assert `ops.sets == 1`, `setArgs[0]` ends `|Derpies`, AND keep the no-learn assertion. `TestNickFlowVerdictNotInNickname` is SUPERSEDED by the new `TestNickFlowDeadEndVerdictResetsName` (same scenario, better fixture) — DELETE it (its second-half cached-equal-repeat assertion no longer applies: after a reset the cache holds `Derpies`, not the nick). `TestNickFlowVerdictClean`/`Unknown` are UNCHANGED (their arms still saveNick + return, no edit). New tests (TDD — write first, watch fail, then make pass), same shape as the existing ones (`newTestNickDerpies`, `nickEvent`, injected clock):
   - `TestNickFlowResetThenEchoSkippedByChangeDetection` (replaces the old "echo of our own clear is skipped" test) — pinned so the right gate is exercised: perform the reset (fixed clock); then `fixed = fixed.Add(61 * time.Second)` — WITHOUT this the next event is blocked by the 60s cooldown (the check BEFORE change-detection), which would mask the exact regression this test must pin (a leftover `saveNick(key, "")` on the success arm only shows up post-window: cache `""` ≠ `"Derpies"` → a second set); then deliver `nickEvent(g, u, "Derpies")` and assert NO second set attempt (`ops.sets == 1`) and `store.listCalls` unchanged (the skip is the `cur == evt.Nick` change-detection, which only works because the success arm now caches the non-empty reset value).
   - `TestNickFlowDeadEndVerdictResetsName` (replaces `TestNickFlowVerdictNotInNickname`): pi returns `GIMMICK:swift` for a nick like `"swiftы"` — verify in the test setup first that `tokensForMatch` of the nick contains `"swiftы"` but NOT `"swift"` (ы is not in the confusable table and is NFD-inert, so the folded token stays non-ASCII and `toks["swift"]` is false): assert `len(store.added) == 0` (NOT learned) AND `ops.sets == 1` (RESET happened) AND `setArgs[0]` ends `|Derpies`.
   - `TestNickFlowValidVerdictLearnsAndResets`: pi returns the as-appears token (e.g. nick `"sw1ft"`, pi `"GIMMICK:sw1ft"`, not in store words): assert `len(store.added) == 1` AND `ops.sets == 1`.
   - `TestNickFlowResetFailedMarksWindow`: `ops.setErr = errors.New("429")`, GIMMICK verdict: assert `ops.sets == 1`, then an immediate re-set of the same nick (same fixed clock) does NOT attempt again (cooldown), bump the clock past 60s and the same-nick re-set attempts again. (Mirror the existing failure-window test.)
   - `TestNickFlowFastPathResetsToDerpies`: store contains the token; assert `setArgs[0]` ends `|Derpies` and NO pi ask, no addGimmick.

**Steps:**
- [ ] Write the changed nickname tests in `internal/handlers/derpies/nicknames_test.go` (adapt the helper + the EXISTING clear-semantics tests, and FLIP the two named no-action tests — `TestNickFlowVerdictInvalidWord` stays (now asserts reset-without-learn), `TestNickFlowVerdictNotInNickname` is deleted — so the package compiles; the NEW behavior tests must be written red)
- [ ] Run `go test ./internal/handlers/derpies/ -count=1 -run TestNick 2>&1 | tail -30` — did the new tests fail for the right reasons (setArgs field missing / reset not taken when unlearnable)? If they pass unexpectedly, stop and investigate
- [ ] Implement the `derpies.go` seam rename (`setNickname` interface + `realOps` + `lastNick` comment) and the `fakeOps` update
- [ ] Implement the `nicknames.go` rework (const, `resetNow`, decoupled GIMMICK arm, comment touch-ups ONLY)
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` — did all derpies tests pass? Fix failures, re-run
- [ ] Run `gofmt -l internal/` — must print nothing; fix if it does
- [ ] Run `go build ./...` and `go vet ./...` — must succeed
- [ ] Commit with message: `feat(derpies): reset gimmick nicks to the fixed name "Derpies" — mask the global name, decouple the name action from the learning gate`

**Acceptance criteria:**
- [ ] No code path still PATCHes `{"nick": null}` for derpies (grep `"""nick":\s*null` in the package returns nothing)
- [ ] A GIMMICK verdict whose word fails the two-arm gate logs "not learnable" and STILL issues one `setNickname` with `Derpies`
- [ ] The gateway echo of our own reset, delivered AFTER the 60s window (the test pins change-detection, not the cooldown), causes no second set attempt and no list re-fetch (`TestNickFlowResetThenEchoSkippedByChangeDetection`)
- [ ] Failure-path 60s window discipline is preserved (asserted)
- [ ] The MESSAGE flow is byte-identical in behavior: no diff in `derpies.go` outside the seam/impl/comments; no message-flow test changes

---

### Task 2: Re-judgment of message edits (GUILD_MESSAGE_UPDATE)

**Context:**
The filter's flow runs on `MessageCreate` only (`MessageCreate` at derpies.go:563; wired in cmd/tugbot/main.go:396). A gated author can post a message that the LLM judges CLEAN (a new respelling the LLM missed, or an innocent post), then EDIT it to carry a gimmick — `GUILD_MESSAGE_UPDATE` fires and only gokupoll sees it (main.go:400-403). This task adds the third event the derpies package watches: any update by a gated author re-runs the EXISTING `h.flow(m)` verbatim on the updated content — backend, image/repeat-image, one-hop reference, one slow-path ask, learning, and deletion all carry over unchanged; an edit costs at most one list SELECT + one pi ask (same as a create). BARE update payloads (the mod.rs:126-136 quirk — a payload with no usable content) are fetched through the EXISTING `channelMessageRetrieve` seam. **NOTE — the fetch trigger is a STRICT, DELIBERATE DEVIATION from the gokupoll precedent, and every doc bullet that mentions the precedent must say so (never "gokupoll precedent" as if identical)**: gokupoll's ported `messageNeedsFetch` fetches on `m == nil || m.Content == ""` (attachment-agnostic); this trigger is `m.Content == "" && len(m.Attachments) == 0 && len(m.Embeds) == 0` — an attachments- or embeds-present, text-absent payload is judged in place instead of fetched (fewer REST calls; the image leg — attachments and embed images — is identical either way, and the seen-image cache makes the fetch redundant). Fetch failure logs and skips (degrades, never aborts). There is no echo-recursion concern (the bot never edits messages) and no message dedup cache exists for messages, so a simple "run the flow on the updated message" is correct. `flow` re-runs its own gates (feature SELECT, guild, author) — that is intentional (idempotent, cheap) and required: the update event's gates must run on the EVENT payload (cheap, before any REST), while `flow`'s gates then run on the possibly-fetched message.

**Files:**
- Create: `internal/handlers/derpies/edits.go`
- Modify: `cmd/tugbot/main.go` (the existing `OnMessageUpdate` handler block, ~lines 400-404)
- Test: `internal/handlers/derpies/derpies_edits_test.go` (new file)

**What to implement:**

1. `edits.go` — new file, package `derpies`, leading comment block in the house style (precedent: nicknames.go's package comment) explaining: the filter is create-only today; an edit of an already-posted message is a second look-free window; every update by a gated author re-runs the FULL create flow; backend payload is fetched via the seam; fetch failure degrades (log + skip), never aborts; the bot never edits, so there is no echo recursion.
   ```go
   // MessageUpdate re-judges an edit by a gated author: the full create
   // flow (fast path, images/repeat-image, one-hop reference, one slow
   // ask, learn, delete) runs on the UPDATED content. The event thread is
   // never held (same shape as MessageCreate).
   func (h *Derpies) MessageUpdate(evt *discordgo.MessageUpdate) { go h.editFlow(evt) }

   func (h *Derpies) editFlow(evt *discordgo.MessageUpdate) {
       // 0. Payload guard (the event carries *Message — nil guards are the
       //    house discipline; Author must exist for the gate below).
       if evt == nil || evt.Message == nil || evt.Message.Author == nil {
           return
       }
       ctx := context.Background()
       // 1. Feature gate (silent flavour, first gate).
       if !h.store.featureEnabled(ctx, FeatureKey) {
           return
       }
       // 2. Guild guard.
       if evt.Message.GuildID == "" {
           return
       }
       // 3. Author-ID gate (checked conversion — the house discipline).
       uid, err := core.DiscordID("user", evt.Message.Author.ID)
       if err != nil {
           return
       }
       if _, ok := h.app.Cfg.DerpiesUserIDs[uid]; !ok {
           return
       }
       slog.Info("derpies edit from filtered user", "module", module, "user", evt.Message.Author.ID, "message", evt.Message.ID)
       // 4. Bare-payload guard — the mod.rs:126-136 fetch branch (the
       //    gokupoll port, with a STRICT trigger deviation — gokupoll
       //    fetches on empty content ALONE; here also require NO
       //    attachments AND NO embeds, so an image-capable payload (the
       //    flow's imageURLPlan reads attachments + embed image/thumbnail
       //    urls) is judged in place): a payload with no text AND no
       //    attachments AND no embeds is fetched by channel+id through
       //    the seam. Fetch failure degrades: log + skip, never abort.
       m := evt.Message
       if m.Content == "" && len(m.Attachments) == 0 && len(m.Embeds) == 0 {
           fetched, err := h.ops.channelMessageRetrieve(m.ChannelID, m.ID)
           if err != nil {
               slog.Error("derpies edit fetch failed", "module", module, "message", m.ID, "error", err)
               return
           }
           m = fetched
       }
       // 5. The full create flow, verbatim (its own gates re-run on m —
       //    idempotent and cheap; m already passed the event-level gates).
       h.flow(m)
   }
   ```
   Import `core "github.com/danielcherubini/tugbot/internal/handlers/gulag"` exactly as nicknames.go/derpies.go do.
2. `cmd/tugbot/main.go` — in the EXISTING `OnMessageUpdate` handler block (the one that does `go h.gokupoll.MessageUpdate(evt)`), add `h.derpies.MessageUpdate(evt)` immediately after the gokupoll line (do NOT wrap in `go`: `MessageUpdate` spawns its own goroutine, exactly like `MessageCreate` is wired). Update the block's comment to mention BOTH handlers. Do not touch any other wiring block. The selftest constructs no event handlers — the "thirteen handlers" log line is UNCHANGED.
3. `derpies_edits_test.go` — new tests in the EXACT style of the existing message-flow tests (reuse `newTestDerpies`, `fakeStore`/`fakeOps`/`fakePi` and the `filteredNickUser`-style gated-ID constant the message tests hard-code; construct the update payload with `&discordgo.MessageUpdate{Message: &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1", Author: &discordgo.User{ID: gatedID}, Content: ...}}`). **TESTS CALL `h.editFlow(evt)` DIRECTLY** (synchronous — the house discipline of every existing flow test, `h.flow(...)` / `h.nickFlow(...)`); `MessageUpdate` only spawns the goroutine and is NOT called in tests (it would race the assertions). Cases (seven):
   - `TestEditFlowNotGatedAuthor`: author ID not in `Cfg.DerpiesUserIDs`, content carries the seed word: assert `store.listCalls == 0`, `pi.asks == 0`, no delete, `ops.refCalls == 0`.
   - `TestEditFlowFastPath`: gated author, payload Content has the seeded token: assert delete happened (same delete-assert helper the message tests use / `ops.deleted`), `pi.asks == 0`, `ops.refCalls == 0` (NO fetch — the payload had content).
   - `TestEditFlowNoGuild`: gated author, `GuildID: ""`, seeded token in Content: assert `store.listCalls == 0`, `pi.asks == 0`, no delete, `ops.refCalls == 0` (the step-2 guild guard is a real code arm — test it).
   - `TestEditFlowEmptyPayloadFetches`: gated author, Content `""`, no attachments and no embeds. **The fetched `ref` MUST carry GuildID + Author** — `flow`'s gates re-run on the fetched message (a missing Author panics in `flow`'s author gate; a missing GuildID silently no-ops it): `ops.ref = &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1", Author: &discordgo.User{ID: gatedID}, Content: <seeded word>}`, with `c1`/`m1` on the event (the fake's `ref`/`refErr` fields double as the fetch result — it already is the `channelMessageRetrieve` stub): assert `ops.refCalls == 1` and a delete of `m1` in `c1`.
   - `TestEditFlowEmptyPayloadFetchFails`: as above but `ops.refErr = errors.New("boom")`: assert NO list fetch, NO ask, NO delete (degrade, not abort).
   - `TestEditFlowSlowPathLearnsDeletes`: gated author, novel content, `pi.resp = "GIMMICK:<as-appears token of the content>"`: assert one ask, `len(store.added) == 1`, delete happened.
   - `TestEditFlowFeatureOff`: `fakeStore{enabled: {FeatureKey: false}}`: assert nothing (list 0, ask 0, delete 0).
   TDD: write the test file first (it won't compile until `MessageUpdate`/`editFlow` exist — that's the expected red), then implement `edits.go` + main.go wiring.

**Steps:**
- [ ] Write `internal/handlers/derpies/derpies_edits_test.go` with all SEVEN cases (expected: compile error — `h.editFlow` undefined)
- [ ] Run `go test ./internal/handlers/derpies/ -count=1 2>&1 | tail -20` — did it fail for the RIGHT reason (undefined method)? If it compiles, stop and investigate
- [ ] Implement `internal/handlers/derpies/edits.go` and the one-line wiring in `cmd/tugbot/main.go`
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` — did all derpies tests pass? Fix failures, re-run
- [ ] Run `gofmt -l internal/ cmd/` — must print nothing
- [ ] Run `go build ./...` and `go vet ./...` — must succeed
- [ ] Commit with message: `feat(derpies): re-judge message edits — GUILD_MESSAGE_UPDATE by a gated author runs the full create flow (fetch on empty payload, gokupoll precedent)`

**Acceptance criteria:**
- [ ] An edit by a GATED author whose payload carries a seeded token deletes with zero pi asks and zero fetches
- [ ] An edit with an empty payload triggers exactly one `channelMessageRetrieve`; a fetch failure is log-only (no ask, no delete)
- [ ] Non-gated / no-guild / feature-off edits cause zero store/ops/pi activity
- [ ] `flow` itself is UNMODIFIED (no diff in `derpies.go`'s flow function)
- [ ] The selftest still logs "thirteen handlers" (no new handler struct)

---

### Task 3: Documentation (CHANGE, feature doc, CONTEXT, ADR, verification)

**Context:**
Repo convention (see git log: "docs: remove X roadmap — shipped", "feat: ... (squash ...)") — every shipped change gets (a) a `CHANGE.md` entry under a date heading, (b) `docs/features/derpies.md` updated (including its front-matter `last-verified`/`verified-by` after a verified run), (c) `CONTEXT.md` language blocks updated, (d) a decision document `docs/decisions/NNNN-<slug>.md` in the house ADR shape (front-matter `status`/`date`/`superseded-by`). This task ships the docs for Tasks 1-2 and closes the verification loop. 0004 (`0004-derpies-nickname-reset.md`) is NOT superseded — extended: the ADR notes it replaces the null-clear remedy with the fixed-value set (ADRs are immutable; note the extension in 0005's body, leave 0004's file alone).

**Files:**
- Modify: `CHANGE.md` (new entry under today's date heading — `## 2026-09-09` on the current execution date; if no `## <today>` heading exists, create it as the NEW top heading, above `## 2026-09-08`; match the existing `## YYYY-MM-DD` → `### title` style)
- Modify: `docs/features/derpies.md` (**note: the doc currently does NOT cover the shipped nickname flow at all — it predates it; the intro stale-calls derpies "the twelfth" handler when there are now thirteen, and its front-matter is stale. This task brings it current.**) 
- Modify: `CONTEXT.md`
- Create: `docs/decisions/0005-derpies-edit-rejudge-nickreset.md`
- Modify: `docs/roadmap/derpies-evasion-patches.md` (final step only: front-matter `status: committed` → `status: shipped` — the file is removed by the ship/finish workflow per the house convention)
- Modify: `internal/handlers/derpies/derpies_test.go` is NOT touched by this task (already done in Task 1)

**What to implement:**

1. `CHANGE.md` — entry (two bullets, house voice, facts only), title: `derpies: edit re-judgment + the name reset is "Derpies" (not a clear)`:
   - Edit bullet: a gated author's GUILD_MESSAGE_UPDATE now re-runs the full create flow on the updated content (fast path / images / repeat-image / one-hop ref / one slow ask / learn / delete carry over; at most one list SELECT + one pi ask per edit); bare payloads — no text AND no attachments AND no embeds — fetch via the `channelMessageRetrieve` seam, the same fetch pattern as the gokupoll port of mod.rs:126-136, **with the strict trigger deviation stated in the bullet (gokupoll fetches on empty content alone; here an attachments- or embeds-present payload is judged in place — fewer REST calls, identical image leg)**; fetch failure degrades (log + skip). The bot never edits, so there is no echo recursion.
   - Name bullet: the nickname reset now SETS the guild nick to the fixed neutral name `Derpies` (const `derpiesNickReset`) instead of clearing to null — a null clear exposes the member's global display name, which the bot cannot modify; the fixed value masks it. The name reset action runs on ANY parseable GIMMICK verdict (the "as-appears word not learnable" dead end no longer lets a recognizable gimmick name survive); the learning gate (wordValid + folded-token-in-nick) is UNCHANGED and now gates learning only. Message flow is untouched (an unlearnable word there still deletes nothing).
   - Code line: `internal/handlers/derpies/edits.go` (new), `nicknames.go` (rework), `derpies.go` (seam `clearNickname` → `setNickname`), `derpies_edits_test.go` (new), `nicknames_test.go`/`derpies_test.go` (fake shape), `cmd/tugbot/main.go` (one-line wiring).
2. `docs/features/derpies.md`:
   - Fix the stale intro: "the twelfth, `internal/handlers/derpies`" → "the thirteenth" (the selftest now constructs thirteen handlers; the verified-by front-matter update below absorbs the rest of the staleness).
   - **ADD a `Nickname flow` section** (the doc currently has none — it predates the shipped nickname flow; do not "rewrite" a section that doesn't exist): place it after the **Referenced messages** section, covering: watches `GuildMemberUpdate` for gated members (per-member 60s coalesced, one attempt per window, success-or-failure marks the window, own-writes never re-judged — the cache-state mechanism); the reset now SETS the guild nick to the fixed neutral name `Derpies` (const `derpiesNickReset`) as one of the flow's outgoing REST calls (`PATCH /guilds/{guild}/members/{member}` — a fixed-value write, replacing the old null-clear, which would expose the member's global display name, unmodifiable by the bot); the reset action fires on ANY parseable GIMMICK verdict; the two-arm learning gate (wordValid + folded-token-in-nick) is unchanged for messages and now gates learning only for nicks (an unlearnable word resets the name, stores nothing).
   - Add a **Message edits** section immediately after the Nickname flow section: one paragraph — every `GUILD_MESSAGE_UPDATE` by a gated author re-runs the full create flow on the updated content (the update payload's content/attachments are judged as-is; a bare payload — no text AND no attachments AND no embeds — is fetched channel+id via the seam, fetch failure degrades log+skip — with the strict trigger deviation from the gokupoll fetch stated, not implied; at most one list SELECT + one pi ask per edit; the seen-image cache works across edits; no echo recursion because the bot never edits; burst profile = creates).
   - EXTEND the **Known limitations** dead-end bullet (append the nickname arm; the message arm is unchanged): for MESSAGES a non-foldable-token dead end still passes; for NICKNAMES a dead-end-as-appearance name is now still reset to `Derpies` (the word simply isn't learned).
   - After the verification step, update front-matter `last-verified` to today and `verified-by` to the actual verification output (the selftest log line + the test summary), same style as the current front-matter.
3. `CONTEXT.md` — two language blocks, minimal rewording (keep the shapes/tone):
   - **Derpies filter**: add after the "falling back on a fast-path miss..." sentence: "It also re-judges `GuildMessageUpdate` by a gated author — the full flow on the updated content (at most one ask per edit)."
   - **Nickname reset**: replace "a GIMMICK verdict clears the nickname (`PATCH .../members/{member} {"nick":null}` — display falls back to the global display name)" with "a GIMMICK verdict resets the nickname to the fixed neutral name `Derpies` (`PATCH .../members/{member} {"nick":"Derpies"}` — a null-clear is not used: it would expose the global display name, which the bot cannot modify); the reset action fires on any parseable GIMMICK verdict (the learning gate — wordValid + folded-token-in-nick — now gates learning only)." The "the bot's own writes are never re-judged" sentence becomes "the bot's own reset is never re-judged (a successful reset records the reset value in the cache BEFORE the gateway echo arrives)".
4. `docs/decisions/0005-derpies-edit-rejudge-nickreset.md` — house ADR shape (mirror `0004-derpies-nickname-reset.md`'s front-matter: `status: accepted`, `date: 2026-09-08`, `superseded-by:` empty/absent):
   - Title: `Derpies re-judges message edits; the name reset is "Derpies" (not a clear)`
   - Body: observed evasion (edits of already-posted messages are a second look-free window — the filter was create-only; a null-clear reset exposes a global name the bot cannot touch, so a recognizable-dead-end name or a global-name swap survives), the two decisions, the strict fetch-trigger deviation from the gokupoll precedent (stated, not implied — gokupoll fetches on empty content alone; derpies also requires no attachments and no embeds, so an attachments- or embeds-present payload is judged in place), and the extension of 0004 (the null-clear remedy is replaced by the fixed-value set; the learning gate is introverted onto learning only — the table's discipline is not loosened, the message flow is unchanged). The front-matter `date: 2026-09-08` is the DECISION date (the first decisions were made 2026-09-08) — keep it; do not "fix" it to the execution date.
   - **Considered Options**: (a) cache last content+attachments per message to skip no-op updates — rejected: new state for little gain (edits by a gated author are exactly the signal we want re-judged, and a fast-path hit costs one list SELECT); (b) decouple the message flow's delete from its learning gate too (so an unlearnable-word message is still delete on "I recognize it") — rejected/out of scope this pass: the message gate is the table's AND the delete's discipline; loosening it changes the documented dead-end semantics (revisit separately); (c) judge/act on the global display name — rejected for this pass: the bot has no API to modify another user's global name at all; the guild nick (the fixed `Derpies` value) is the only lever, and it masks the global name.

**Steps:**
- [ ] Run the FULL verification gate and record the real output (needed for the front-matter): `make db-up` → `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./... 2>&1 | tail -25` → `go run ./cmd/tugbot --selftest 2>&1 | tail -5` — all green, selftest logged "Discord session and all thirteen handlers constructed" (exit 0)? If not, STOP and fix (systematic debugging skill) before writing any docs
- [ ] Write the `CHANGE.md` entry, the `docs/features/derpies.md` updates, the `CONTEXT.md` rewordings, and `docs/decisions/0005-derpies-edit-rejudge-nickreset.md`
- [ ] Run `gofmt -l .` — must print nothing
- [ ] Run `go build ./...` and `go vet ./...` — must succeed (docs-only, but the gate is the gate)
- [ ] Run `make lint` — must pass
- [ ] **Retire the roadmap per the house convention** (git log: "docs: remove X roadmap — shipped"): set `docs/roadmap/derpies-evasion-patches.md` front-matter `status: committed` → `status: shipped` in the docs commit (the file itself is removed by the ship/finish workflow on merge — the finish step, not this task, deletes it)
- [ ] Commit with message: `docs: derpies edit re-judgment + the "Derpies" name reset (CHANGE, feature doc, CONTEXT, ADR 0005)`

**Acceptance criteria:**
- [ ] `CHANGE.md` has the dated entry; `docs/features/derpies.md` has the Message edits section + updated reset semantics + updated dead-end bullet + updated front-matter; `CONTEXT.md` blocks reworded; ADR 0005 exists in house shape
- [ ] `make lint` + `go build ./...` pass
- [ ] No doc claims a behavior the code doesn't have (verify each bullet against `nicknames.go`/`edits.go` before committing)

---

## Execution notes (for the implementing agent)

- DB-touching tests (the derpies/gimmick integration suites) self-skip without `TUGBOT_TEST_DATABASE_URL`; the per-task gate (`go test ./internal/handlers/derpies/ -count=1`) is sufficient mid-plan — the FULL green gate runs in Task 3.
- `-p 1 -count=1` discipline applies to the full gate only (shared DB); package-level runs during Tasks 1-2 are fine.
- Do NOT touch: `gokupoll` (wiring only in main.go), the message flow in `derpies.go`, `wordmatch`, migrations, the selftest log line.
- If `fakeOps`'s renamed fields break a test in a file you don't recognize, search `grep -rn "clears\|clearArgs\|clearErr" internal/handlers/derpies/` and adapt the assertion (the discipline is the same; only the seal name changes).
- House commit style: conventional prefix, em-dash, no body — match the `git log` voice.
