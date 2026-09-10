---
status: committed
done-when: All of `go build ./...`, `go vet ./...`, `gofmt -l .` (must print nothing), `make lint`, `go test ./... -count=1` pass; `grep -rn "seenImage\|repeatImage\|isEdit\|imageHash" internal/handlers/derpies/` prints nothing; an image re-post is re-judged by the LLM (one fresh pi ask per post, bounded by the pirpc per-ask image guard) and the `derpies delete (repeat image)` log line no longer exists in the codebase; docs/features/derpies.md, CHANGE.md, ADR 0006 and CONTEXT.md are committed.
---

# Remove the derpies repeat-image fast delete (flow 4.6) Plan

**Goal:** Derpies never blind-deletes on image content again — a re-posted image is re-judged by a fresh pi ask; the 4.5 image leg (download + send to the ask) and the pirpc per-ask image guard stay.
**Architecture:** Hard removal of flow 4.6 from the derpies message flow (no feature gate, no keep-cache variant): the `seenImages` in-memory cache (24h TTL, 2048-entry bound), the seen-skip logic, the pure-repeat delete arm (create path) and its edit no-op arm, and the `isEdit` origin flag on `flow()` are all gone, so an edit is origin-agnostic and costs at most one list SELECT + one pi ask like a create. The 4.5 leg (image plan, URL-dedup, per-URL download) and the pirpc per-ask guard (byte-dedupe, resize, 12MB cap — shared with the mention handler) are untouched. No schema, migration, config, or `derpies_prompt` changes.
**Tech Stack:** Go (discordgo, pgx, pi RPC), house test/fmt/lint gate per AGENTS.md.

Background for any executor: flow 4.6 shipped 2026-09-08 (CHANGE.md / `git log` commit `bae6467`): after any completed LLM ask, image content hashes (sha256 of downloaded bytes) were memorized in an in-process map; a message whose content is image-only and whose images were all "seen" was a repeat and deleted with ZERO asks. On 2026-09-10 a filtered user re-posted one 1,346,869-byte PNG twice in 14s (the original LLM-judged CLEAN); both re-posts were deleted in under a second without an ask. The operator decision: **images must never be blindly deleted — a re-posted image is re-judged.** Approved design: approach A (hard removal), with ADR capture and a CONTEXT.md glossary note.

---

### Task 1: Strip the 4.6 mechanism from the handler code

**Context:**
This task removes the 4.6 machinery from `internal/handlers/derpies/derpies.go` and `edits.go`, and mechanically updates every surviving `h.flow(...)` test call site (the `isEdit bool` parameter is dropped, so `h.flow(x, false)` becomes `h.flow(x)`). Six 4.6-specific tests (4 in `derpies_repeat_image_test.go`, 2 in `derpies_edits_test.go`) are intentionally left in place by this task — FIVE of them fail after this change (the sixth, `TestUnjudgedImageRepostIsJudged`, passes in both worlds — see the NOTE in the step below) — and **do not "fix" the failures by re-adding any behavior** (Task 2 deletes them). The `clock` and `nickMu` fields stay: they still serve the nickname flow (`markEdit`'s 60s-window math in `nicknames.go`, which calls `h.clock()`; `nickMu` guards the nickname maps). What NOT to touch: `imageURLPlan`, `isSafeURL`, `mimeForURL`, `downloadPlan`, `downloadImages`, the 3.5 referenced-fetch, the fast path (step 4), the ask/verdict/learn/delete steps (5–10), the 4.5 download comment, the package doc comment at the top of `derpies.go` (it does not mention 4.6), `New()` (it does not initialize `seenImages` — the map was lazily created in `markImagesSeen`), and every file outside `internal/handlers/derpies/`.

**Files:**
- Modify: `internal/handlers/derpies/derpies.go`
- Modify: `internal/handlers/derpies/edits.go`
- Modify (call-site edits only): `internal/handlers/derpies/derpies_test.go` (24 call sites), `internal/handlers/derpies/derpies_images_test.go` (11 call sites), `internal/handlers/derpies/derpies_edits_test.go` (2 call sites, at the `h.flow(imgMsg("m1", srv.URL+"/a.png"), false)` lines — both inside the two tests Task 2 deletes, but update them now so the package compiles)
- Verify (must be unchanged, 0 call sites): `internal/handlers/derpies/derpies_integration_test.go`, `internal/handlers/derpies/nicknames_test.go`

**What to implement:**

1. `derpies.go` — deletion list (exact symbols):
   - `Derpies` struct: delete the field `seenImages map[string]time.Time` and its doc comment. Rewrite the `nickMu` comment from `// guards the four maps (lastNick, lastEdit, busy, seenImages)` to `// guards the three maps (lastNick, lastEdit, busy)`.
   - Delete the two consts `repeatImageTTL = 24 * time.Hour` and `repeatImageMax = 2048` (the const block is removed entirely when empty).
   - Delete the three functions `imageHash(im app.PiImage) string`, `seenImage(hash string) bool`, `markImagesSeen(hashes []string)`.
   - `flow(m *discordgo.Message)`: drop the `isEdit bool` parameter. Rewrite its doc comment's FIRST line from `// flow — the full message flow (gates → fast path → images/repeat → slow path → learn/delete).` to `// flow — the full message flow (gates → fast path → images → slow path → learn/delete).` THEN rewrite the tail of that doc comment — currently "`isEdit` distinguishes the origin: the create path passes false, the edit path (edits.go) passes true. The ONLY behavioral difference is flow 4.6's pure-repeat delete arm, which an EDIT must never trigger: … Everything else is origin-agnostic — an edit costs at most one list SELECT + one pi ask, same as a create." — to: "The create and edit paths run the identical flow — origin-agnostic; each post or edit costs at most one list SELECT + one pi ask." AND in `MessageCreate` (same file, the edits.go twin is covered by item 2): `go h.flow(m, false)` → `go h.flow(m)`.
   - Inside `flow()`: delete the entire 4.6 block — the `var freshImages []app.PiImage` / `seenImgCount` loop, the pure-repeat delete arm INCLUDING its `if isEdit { ... "derpies edit all-seen image-only — no-op" ... return }` branch, `slog.Info("derpies skipping seen image(s) from the ask", ...)`, and `images = freshImages`. After 4.5's download the flow goes directly to the step-5 `pi == nil` check.
   - Delete the post-ask marking block — the comment `// Judged (a completed ask, every verdict leg ...)` and the `for _, im := range images { h.markImagesSeen([]string{imageHash(im)}) }` loop (the ask itself and steps 6, 7, 8 stay).
   - Imports: delete `"crypto/sha256"` and `"encoding/hex"` — FIRST run `grep -n "sha256\|hex\." internal/handlers/derpies/derpies.go` to confirm they are used ONLY by `imageHash` (they are; if the grep shows any other use, stop and investigate).
   - Keep: `clock`, `nickMu`, `lastNick`, `lastEdit`, `busy`, and everything in steps 1–4 and 5–10.

2. `edits.go` — doc/comment rewrites (no code change to `editFlow`'s logic beyond the call-site arg):
   - Package comment, last sentence of the first paragraph: delete "The single origin difference: flow 4.6's pure-repeat delete arm is disabled for edits (an all-seen image-only EDIT never deletes — see flow's isEdit docs)." and replace the earlier phrase "with the isEdit origin flag — fast path, images/repeat-image, one-hop reference, one slow-path ask, learning, and deletion all carry over unchanged (an edit costs at most one list SELECT + one pi ask, same as a create)" with "— fast path, images, one-hop reference, one slow-path ask, learning, and deletion all carry over unchanged (an edit is origin-agnostic: at most one list SELECT + one pi ask, same as a create)".
   - `MessageUpdate` handler comment: "fast path, images/repeat-image, one-hop reference, one slow ask, learn, delete" → "fast path, images, one-hop reference, one slow ask, learn, delete".
   - `h.flow(m, true)` → `h.flow(m)`.
   - Step-5 comment in `editFlow`: "The full create flow, with the `isEdit` origin flag (the only difference: flow 4.6's pure-repeat delete arm is disabled — an all-seen image-only EDIT never deletes). The flow's own gates re-run on m — idempotent and cheap; m already passed the event-level gates." → "The full create flow (origin-agnostic; an edit costs at most one list SELECT + one pi ask). The flow's own gates re-run on m — idempotent and cheap; m already passed the event-level gates."

3. Test call-site edits (mechanical, no behavioral rewrites): in `derpies_test.go` (24), `derpies_images_test.go` (11), and `derpies_edits_test.go` (2), change every `h.flow(<args>, false)` → `h.flow(<args>)`. Locate with `grep -n "flow(.*false)" internal/handlers/derpies/*_test.go` and verify no `h.flow(…, false)` call sites remain after the edit — the ONLY remaining grep match at this point is the section-header COMMENT in `derpies_edits_test.go` (`// is primed through the CREATE path (flow(m, false)), then the edit is`), inside the block Task 2 deletes.

**Steps:**
- [ ] Baseline: run `go test ./internal/handlers/derpies/ -count=1` — all tests must pass BEFORE any edit. If any fail before the change, stop and investigate (do not carry existing failures forward).
- [ ] Apply the `derpies.go` deletions (item 1), including the import cleanup.
- [ ] Apply the `edits.go` rewrites (item 2).
- [ ] Apply the test call-site edits (item 3).
- [ ] Run `go build ./...` — must succeed; fix anything the compiler flags (missed call sites, unused imports).
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` — EXPECTED: exactly these FIVE tests fail (they assert the removed behavior): `TestRepeatImageFastDelete`, `TestRepeatImageWithNewTextGetsTextOnlyAsk`, `TestMixedSeenAndFreshImages` (in `derpies_repeat_image_test.go`) and `TestEditFlowSeenImageRepeatDoesNotDelete`, `TestEditFlowFreshTextWithSeenImageGetsTextOnlyAsk` (in `derpies_edits_test.go`). NOTE: the sixth 4.6-file test, `TestUnjudgedImageRepostIsJudged`, still PASSES in both code worlds — it pins the invariant "an un-judged image is always judged and never fast-deleted blind", and a fresh re-post is asked in old and new code alike — so do NOT be surprised when it is green; it still gets deleted with the file in Task 2. Every other test must still pass. If any other test fails, stop and investigate before continuing.
- [ ] Run `go vet ./...` (must pass) and `gofmt -l internal/handlers/derpies` (must print nothing).
- [ ] Commit with message: `derpies: strip the repeat-image fast delete (flow 4.6) — re-posted images are re-judged in fresh asks`
- [ ] Do NOT fix the six stale-test failures; they are deleted in Task 2.

**Acceptance criteria:**
- [ ] `grep -rn "seenImage\|repeatImage\|isEdit\|imageHash" internal/handlers/derpies/derpies.go internal/handlers/derpies/edits.go` prints nothing.
- [ ] `go build ./...` and `go vet ./...` pass; `gofmt -l internal/handlers/derpies` prints nothing.
- [ ] The only failing tests in the package are the five listed above (`TestUnjudgedImageRepostIsJudged` passes by design); `TestEditFlowFastPath`, `TestEditFlowSlowPathLearnsDeletes`, `TestEditFlowEmptyPayloadFetches`, and all `derpies_test.go`/`derpies_images_test.go`/`nicknames_test.go` tests pass.
- [ ] `grep -rn "crypto/sha256\|encoding/hex" internal/handlers/derpies/derpies.go` prints nothing.

---

### Task 2: Delete the stale 4.6 tests and rescue the orphaned helper

**Context:**
After Task 1, six tests are 4.6-specific (FIVE of them fail — see Task 1; `TestUnjudgedImageRepostIsJudged` passes in both worlds) and three test helpers defined in `derpies_repeat_image_test.go` plus its `requestAlias` type alias have mixed lifetimes: `newRepeatTest` and `imgMsg` are used ONLY by tests deleted in this task (they die with the file — no action), but `bodyServer` is used by the SURVIVING test `TestEditFlowEmptyTextWithAttachmentsJudgedInPlace` in `derpies_edits_test.go` (the `srv := bodyServer(t, ...)` line), so it — AND its `requestAlias` companion, which dies with the file — MUST be relocated before the file is deleted or the surviving test breaks. The `net/http` + `net/http/httptest` imports ride with the relocation (added to `derpies_edits_test.go`).

**Files:**
- Delete: `internal/handlers/derpies/derpies_repeat_image_test.go` (contains 4 tests: `TestRepeatImageFastDelete`, `TestRepeatImageWithNewTextGetsTextOnlyAsk`, `TestUnjudgedImageRepostIsJudged`, `TestMixedSeenAndFreshImages`; helpers `bodyServer`, `newRepeatTest`, `imgMsg`)
- Modify: `internal/handlers/derpies/derpies_edits_test.go`

**What to implement:**
1. Relocate BOTH `func bodyServer(t *testing.T, body []byte) *httptest.Server { ... }` (copy verbatim, with its leading comment if it has one) AND the companion declaration `type requestAlias = http.Request` (used by bodyServer's handler signature; it is a SEPARATE top-level declaration, NOT part of the function — deleting the file without moving it leaves `undefined: requestAlias`) into `derpies_edits_test.go`: place `bodyServer` directly above `TestEditFlowEmptyTextWithAttachmentsJudgedInPlace` (its only remaining user) with the alias adjacent. `derpies_edits_test.go` currently imports neither `net/http` nor `net/http/httptest` — ADD both imports after the move (the alias needs `net/http`; bodyServer needs `net/http/httptest`; remove no other imports).
2. Delete `derpies_repeat_image_test.go` in full.
3. In `derpies_edits_test.go`: delete the section-header comment block for the removed repeat-on-edit tests (the `// ---` banner and the paragraph starting "Repeat-image on the EDIT path — an all-seen image-only edit must NEVER trigger the pure-repeat delete (flow 4.6 with isEdit)…"), and delete the two test functions `TestEditFlowSeenImageRepeatDoesNotDelete` and `TestEditFlowFreshTextWithSeenImageGetsTextOnlyAsk` in full.

**Steps:**
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` — confirm the FIVE Task-1 failures are visible (and nothing else fails).
- [ ] Relocate `bodyServer` (item 1), delete the file (item 2), delete the two tests + comment block (item 3).
- [ ] Run `grep -rn "newRepeatTest\|imgMsg(" internal/` — must print nothing (both die with the file; if the grep finds a SURVIVING reference, stop and relocate the needed helper before deleting the file).
- [ ] Run `grep -rn "bodyServer\|requestAlias" internal/handlers/derpies/` — expect exactly: the relocated `bodyServer` declaration, the one `requestAlias` reference inside it, the relocated `type requestAlias` declaration, and `bodyServer` usage lines in the surviving `derpies_edits_test.go` tests only. No other file may reference either symbol.
- [ ] Run `go vet ./...` right after this grep, BEFORE the test run, so a missing `net/http` import or any other orphan surfaces early.
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` — all green.
- [ ] Run `go test ./... -count=1` (DB-touching tests self-skip without `TUGBOT_TEST_DATABASE_URL` — skips are expected, failures are not), `go vet ./...`, and `gofmt -l .` (must print nothing).
- [ ] Commit with message: `derpies: delete the 4.6 repeat-image tests and relocate the orphaned bodyServer helper`

**Acceptance criteria:**
- [ ] `internal/handlers/derpies/derpies_repeat_image_test.go` no longer exists.
- [ ] `go test ./... -count=1` is fully pass/skip (zero failures); `grep -rn "repeat-image\|repeatImage\|seenImage" internal/` prints nothing.

---

### Task 3: Docs — feature doc, CHANGE, ADR 0006, CONTEXT.md

**Context:**
`docs/features/derpies.md` ("Message edits" paragraph and the "Repeat-image fast path" bullet under "Known limitations") and the 2026-09-08 CHANGE.md entry describe 4.6 as live behavior — both must match the removal. The decision (no blind image-deletion ever; re-posted images re-judged in fresh asks; the re-post burst-amplification cost is accepted and bounded by the pirpc per-ask image guard) is hard to reverse in effect, surprising without context, and a real trade-off → house convention (see `docs/decisions/0005-*` header) records it as ADR 0006; the prose decision lives in the "Derpies filter" glossary entry in `CONTEXT.md`. No other doc is touched.

**Files:**
- Modify: `docs/features/derpies.md`
- Modify: `CHANGE.md`
- Create: `docs/decisions/0006-derpies-repeat-image-fast-delete-removed.md`
- Modify: `CONTEXT.md`

**What to implement:**
1. `docs/features/derpies.md`:
   - "## Message edits" paragraph: delete the clause "the seen-image cache works across edits"; in the same sentence's parenthetical change "judged as-is (fast path, images/repeat-image, one-hop reference, …)" to "judged as-is (fast path, images, one-hop reference, …)"; replace the sentence "The edit path carries ONE origin difference (the `isEdit` flag): the pure-repeat delete is a CREATE-path rule — an all-seen image-only EDIT never deletes (an edit that lands in that state REMOVED content from a previously-judged message; it is a clean no-op — no delete, no ask)." with "An edit is origin-agnostic — there is no repeat-delete arm: a re-posted image is re-judged in a fresh ask like any other content (see the repeat-image note under Known limitations)."; rewrite the fetch-deviation parenthetical "(fewer REST calls — the image leg is identical either way, and the seen-image cache makes the fetch redundant)" to "(fewer REST calls — the image leg is identical either way; a judged-in-place payload re-downloads its images through the same seam)".
   - "## Known limitations" bullet: replace the whole "Repeat-image fast path" bullet with: "**Repeat-image re-judgment (the 4.6 fast delete was removed)**: an image whose CONTENT was previously LLM-judged is NOT a fast delete — its post goes into a FRESH pi ask (at most one list SELECT + one ask per post or edit), bounded by the pirpc per-ask image guard (byte-identical dedupe, >1MB / >2048px resize to 2048/JPEG q80, 12MB raw cap). There is no zero-ask deletion on any image state: N re-posts = N bounded asks in the shared pi queue."
2. `CHANGE.md`: prepend at the top (matching the existing `## <date>` / `### <module>: <title>` style) an entry dated the day this change lands: title `### derpies: repeat-image fast delete (4.6) removed — re-posts are re-judged, never blind-deleted`, with bullets: operator decision driven by the 2026-09-10 journal incident (a filtered user's same-bytes PNG re-posts, the original LLM-judged clean, were deleted in under a second with zero asks); what was removed (the `seenImages` in-process cache with 24h TTL / 2048 bound, the seen-skip logic, the pure-repeat delete + edit no-op arms, the `isEdit` origin flag, the 4.6 tests); what was kept (the 4.5 image leg, the pirpc per-ask image guard, the nickname flow); the cost trade (re-post burst amplification: N re-posts = N bounded asks; no zero-ask deletion on any image post); the code line (`internal/handlers/derpies/derpies.go`, `edits.go` + test files) and docs line (`docs/features/derpies.md`, `docs/decisions/0006-derpies-repeat-image-fast-delete-removed.md`, `CONTEXT.md`).
3. Create `docs/decisions/0006-derpies-repeat-image-fast-delete-removed.md` following the house header (copy the front-matter shape of `docs/decisions/0005-derpies-edit-rejudge-nickreset.md`: `status: accepted` / `date: <landing day>` / `superseded-by:`). Body: (a) context — what 4.6 was (shipped 2026-09-08, commit `bae6467`), the Sep 10 incident, the operator decision that images must never be blindly deleted; (b) the decision — a re-posted image is re-judged in a fresh pi ask; the seen-content cache, seen-skip, and `isEdit` origin are gone; edits are origin-agnostic; the re-post burst-amplification cost is accepted and bounded by the per-ask image guard; (c) rejected alternatives — keep 4.6 (the incident shows the operator does not accept blind image deletions), feature-gate-4.6-off (dead machinery + a 2048-entry in-memory LRU kept for a path nobody wants on), keep-cache-drop-only-the-delete-arm (re-post stays re-judged cheaper by skipping already-seen images, but keeps the cache machinery the decision discarded and entangles seen-content semantics with no-delete).
4. `CONTEXT.md`: extend the existing **Derpies filter** glossary paragraph (the one starting "The Go bot's own original feature:") with a boundary sentence: "Repeated images are re-judged, never fast-deleted: the 2026-09-08 repeat-image fast delete (flow 4.6 — seen-content cache, zero-ask re-post delete) was REMOVED on <landing day> (operator decision 2026-09-10) — a post carrying an already-judged image (or a re-post of one) goes into a fresh pi ask bounded by the per-ask image guard; no image post is ever deleted with zero asks. The image leg (4.5: attachment + embed download, URL-deduped) is kept; no image post is ever fast-deleted on account of its content having been previously judged (the separate word fast path, which runs before any image handling, is unchanged)." Then APPEND `repeat-image fast delete, seen-image cache` to the entry's EXISTING `_Avoid_:` line (currently `_Avoid_: mod action, anti-spam handler, derpies handler`) — do not add a second Avoid line.

**Steps:**
- [ ] Edit `docs/features/derpies.md` (both sections, item 1).
- [ ] Prepend the `CHANGE.md` entry (item 2).
- [ ] Create the ADR (item 3).
- [ ] Edit `CONTEXT.md` (item 4).
- [ ] Front-matter policy for `docs/features/derpies.md` (the front-matter block is lines 1–5: `status`, `last-verified`, `verified-by`, closing `---`): keep the `verified-by` line's review narrative AS WRITTEN — the 2026-09-09 review date/gate recital, the "isEdit flag" reference, the selftest recital, the 19-package record are a history record; but WITHIN that same line, update ONLY the derpies test-count annotation — `91 tests (88 pass, 3 DB integration self-skip …)` → `85 tests (82 pass, 3 DB integration self-skip …)` and the reproducible `= 91` → `= 85` (six tests were removed from the package); bump `last-verified` to the landing day; leave `status` unchanged.
- [ ] Consistency check (BODY ONLY — the front-matter block, lines 1–5, is excluded by starting at line 6): run `sed -n '6,$p' docs/features/derpies.md | grep -n "seen-image\|fast delete\|isEdit"` — every remaining hit must be in removal context (i.e., the new "…was removed" wording); fix any stale body prose it surfaces.
- [ ] Commit with message: `docs: derpies repeat-image fast delete removed (feature doc, CHANGE, ADR 0006, glossary)`

**Acceptance criteria:**
- [ ] No remaining live-behavior description of 4.6 in `docs/features/derpies.md` (grep per the step above shows only removal-context wording).
- [ ] All four files (two modified, one created, one modified) are committed; the ADR front-matter matches the house shape; `docs/decisions/` still has no gap: 0001…0006 all present (note: 0004 has two files in the repo — that pre-existing duplication is NOT a defect to fix).

---

## Final acceptance gate (run after Task 3's commit; not a separate commit)

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `gofmt -l .` — must print nothing
- [ ] `make lint`
- [ ] `go test ./... -count=1` — all pass/skip
- [ ] If a local Postgres is reachable (or after `make db-up`): `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` and `go run ./cmd/tugbot --selftest` — the selftest must log `selftest: Discord session and all thirteen handlers and the MCP server constructed` and exit 0 (this change adds/removes no handlers, so the count is unchanged). If neither `make db-up` nor the DB is reachable, record the skip — do not fake the gate.
