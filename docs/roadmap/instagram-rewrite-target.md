---
status: committed
done-when: With the `instagram` feature flag enabled, a message containing an `https://(www\.)?instagram.com/...` URL triggers the bot to suppress the original's embeds and post a NEW message containing only the matched URL with the domain group rewritten to `oginstagram.com` (the `www.` prefix preserved); `go build ./...`, `go vet ./...`, and `go test ./...` are all green; `rg kkinstagram internal/` returns only the three comment lines in the Instagram package that document the deliberate divergence (none in Go string literals or test expectations); `docs/parity/checklist.md` records the divergence from Rust's `kkinstagram.com`
---

# Instagram Rewrite Target: kkinstagram → oginstagram

**Goal:** The Instagram handler keeps its exact mechanic but rewrites the matched URL's domain to `oginstagram.com` instead of `kkinstagram.com` — the port's first deliberate Go-vs-Rust divergence (Rust's `instagram.rs` stays on `kkinstagram.com`; this repo documents the divergence rather than matching it).

**Architecture:** One behavior constant inside the `internal/handlers/instagram` package changes; the trigger regex, feature-flag gating, embed suppression, new-message posting, `www.`-prefix preservation, logging, and wiring in `cmd/tugbot/main.go` are all unchanged. The Go test suite is the spec — its expectations are updated first (failing on purpose) and then the constant is swapped. `docs/parity/checklist.md` is reworded so the checklist stays a truthful port-verification document.

**Tech Stack:** Go (toolchain per `go.mod`), `github.com/bwmarrin/discordgo`, pgx pool for the feature-flag check — that path and the whole handler surface are untouched; `testing` (stdlib).

**Context for the executing agent (no conversation memory assumed):**
- This bot is the Go port of the Rust bot `tugbot-rs`. `docs/parity/checklist.md` is the port-verification checklist; each item cites the Rust source (`src: instagram.rs:<lines>`) it was ported from.
- The Instagram handler (`internal/handlers/instagram/instagram.go`): on every message create, if the `instagram` feature flag is enabled (`features.IsEnabled` against the features table, `FeatureKey = "instagram"`) and the message content matches `reInstagram = https://(www\.)?(instagram\.com)/.+` (group 1 = optional `www.` prefix, left untouched; group 2 = the `instagram.com` domain), it (1) sets the `SUPPRESS_EMBEDS` flag on the ORIGINAL message via a flags-only edit, then (2) posts a NEW message in the same channel containing ONLY the matched URL substring, with every occurrence of the group-2 string `instagram.com` replaced by the target domain (`strings.ReplaceAll`, mirroring Rust's `String::replace`).
- The operator-approved change: target domain `kkinstagram.com` → `oginstagram.com`. Nothing else about the mechanic changes.
- `docs/parity/checklist.md` items 44 and 46 (under the `## instagram` section, ~lines 41–48) currently claim 1:1 parity including the `kkinstagram.com` domain. After this change they must record the deliberate divergence so future readers don't "fix" the code to match Rust.

---

### Task 1: Swap the rewrite domain to `oginstagram.com` (code + tests)

**Context:**
All behavior lives in one function (`rewrite`) and its tests. This task keeps code and tests in the same commit so no intermediate repo state is red. TDD: update the test expectations first and watch them fail (proves the tests are the spec), then flip the constant. The two doc comments that claim "parity" are reworded to state the deliberate divergence — a reader who only reads Go must not conclude the Go bot matches Rust on the domain.

**Files:**
- Modify: `internal/handlers/instagram/instagram.go`
- Modify: `internal/handlers/instagram/instagram_test.go`

**What to implement:**
- `internal/handlers/instagram/instagram.go`:
  - `rewrite()`: the final return becomes
    `return strings.ReplaceAll(m[0], m[2], "oginstagram.com")` (was `"kkinstagram.com"`).
  - Package doc comment (top of file): replace the ENTIRE 8-line block below, verbatim
    ```
    // Mechanic parity (port 1:1, see docs/parity/checklist.md — instagram
    // section): when the "instagram" feature flag is enabled and the message
    // content matches the instagram-URL regex, the bot (1) edits the ORIGINAL
    // message to suppress its embeds, then (2) posts a NEW message containing
    // only the matched URL with its domain rewritten to kkinstagram.com while
    // the optional "www." prefix is preserved. This is NOT an in-place edit —
    // Rust does the same: `edit(...suppress_embeds(true))` followed by
    // `channel.say(fixed)`.
    ```
    with the ENTIRE 9-line block below
    ```
    // Port of the Rust bot's mechanic (see docs/parity/checklist.md — instagram
    // section): when the "instagram" feature flag is enabled and the message
    // content matches the instagram-URL regex, the bot (1) edits the ORIGINAL
    // message to suppress its embeds, then (2) posts a NEW message containing
    // only the matched URL with its domain rewritten to oginstagram.com while
    // the optional "www." prefix is preserved — a deliberate divergence from
    // Rust, which rewrites to kkinstagram.com. This is NOT an in-place edit —
    // Rust does the same: `edit(...suppress_embeds(true))` followed by
    // `channel.say(fixed)`.
    ```
  - `rewrite` doc comment: replace
    `// rewrite mirrors Rust's fx_rewriter (instagram.rs:26-40): find the first`
    `// match and replace EVERY occurrence of the domain group ("instagram.com",`
    `// group 2 — group 1 "www." is untouched) inside the matched substring with`
    `// "kkinstagram.com" (Rust's String::replace replaces all occurrences).`
    with
    `// rewrite mirrors Rust's fx_rewriter (instagram.rs:26-40) with ONE`
    `// deliberate divergence: find the first match and replace EVERY occurrence`
    `// of the domain group ("instagram.com", group 2 — group 1 "www." is`
    `// untouched) inside the matched substring with "oginstagram.com" (Rust`
    `// rewrites to "kkinstagram.com"; Rust's String::replace also replaces all`
    `// occurrences).`
- `internal/handlers/instagram/instagram_test.go`:
  - File-header comment: replace
    `` // Port of fx_rewriter's regex `https://(www\.)?(instagram\.com)/.+` with the
    // SECOND capture group ("instagram.com") replaced by "kkinstagram.com" while
    // the optional "www." prefix is preserved (src/handlers/instagram.rs:28-40). ``
    with
    `` // Follows the regex `https://(www\.)?(instagram\.com)/.+` from the Rust
    // fx_rewriter (src/handlers/instagram.rs:28-40), with the SECOND capture
    // group ("instagram.com") replaced by "oginstagram.com" while the optional
    // "www." prefix is preserved — a deliberate divergence: Rust rewrites to
    // "kkinstagram.com". ``
  - Test expectations (only the five below change; `TestInstagramNoMatch` and `TestInstagramEmptyString` are untouched; the per-test `// Mirrors instagram.rs test ...` comments stay as-is — the tests still mirror the Rust test shapes, with the domain diverged, which the header now says):
    - `TestInstagramRewrite`: `want := "https://www.kkinstagram.com/reel/DCkUQSry42v/?igsh=MXNrMDFwbTEzZnFvMg=="` → `want := "https://www.oginstagram.com/reel/DCkUQSry42v/?igsh=MXNrMDFwbTEzZnFvMg=="`
    - `TestInstagramRewriteWithoutWWW`: `want := "https://kkinstagram.com/p/ABC123/"` → `want := "https://oginstagram.com/p/ABC123/"`
    - `TestInstagramPostURL`: `want := "https://www.kkinstagram.com/p/ABC123/"` → `want := "https://www.oginstagram.com/p/ABC123/"`
    - `TestInstagramStoryURL`: `if !strings.Contains(got, "kkinstagram.com") {` and `t.Errorf("rewrite(story url) = %q, want it to contain \"kkinstagram.com\"", got)` → same with `"oginstagram.com"` in both places.
- What NOT to change: `FeatureKey`, `reInstagram`, `MessageCreate`, `suppressEmbeds`, the four log lines (`Error supressing embeds` / `Suppressed Embed` / `Error posting Instagram message` / `Posted Instagram` — note the existing "supressing" typo in the first is deliberate Rust-ported logging parity, do NOT fix it), imports, `cmd/tugbot/main.go`, migrations, `docs/runbook/cutover.md` (no domain references there).

**Steps:**
- [ ] Edit `internal/handlers/instagram/instagram_test.go` per "What to implement" (header + the five expectations).
- [ ] Run `go test ./internal/handlers/instagram/`
  - Did it FAIL with 3–5 errors, each showing `got "https://...kkinstagram.com/..."` vs `want "https://...oginstagram.com/..."`? If it passed unexpectedly, stop and investigate why.
- [ ] Edit `internal/handlers/instagram/instagram.go` per "What to implement" (`rewrite()` + the two doc comments).
- [ ] Run `go test ./internal/handlers/instagram/`
  - Did all tests pass? If not, fix the failures and re-run before continuing.
- [ ] Run `go vet ./internal/handlers/instagram/` — did it succeed? If not, fix and re-run.
- [ ] Run `gofmt -l internal/handlers/instagram/` — did it print nothing? If not, run `gofmt -w internal/handlers/instagram/` and re-run both gofmt and the package tests.
- [ ] Run `go build ./...` — did it succeed? If not, fix and re-run.
- [ ] Run the full repo gate (per `AGENTS.md`): `go vet ./...`, `go test ./...` (DB-touching tests self-skip when `TUGBOT_TEST_DATABASE_URL` is unset — a skip is green, not a miss), `gofmt -l .`, `make lint` — did all succeed (gofmt prints nothing, lint passes, all tests pass)? If not, fix and re-run.
- [ ] Run `rg -n kkinstagram internal/` — did it return EXACTLY three lines, all `//` comment lines: one in `instagram.go`'s package doc (`// Rust, which rewrites to kkinstagram.com. This is NOT an in-place edit —`), one in `instagram.go`'s `rewrite` doc (`// rewrites to "kkinstagram.com"; Rust's String::replace also replaces all` — the rest of that line), and one in `instagram_test.go`'s header (`// "kkinstagram.com".`)? Any OTHER match — a Go string literal, an identifier, or a test expectation — means the swap missed a spot: stop and fix before committing.
- [ ] Stage ONLY this task's two files and commit — the working tree has a pre-existing `CONTEXT.md` modification and an untracked `docs/roadmap/` belonging to this plan; do NOT use `git commit -a` or `git add -A` (they would fold those in):
  `git add internal/handlers/instagram/instagram.go internal/handlers/instagram/instagram_test.go && git commit -m "instagram: change rewrite target from kkinstagram.com to oginstagram.com"`

**Acceptance criteria:**
- [ ] `go test ./internal/handlers/instagram/` passes with all six existing test functions (same names — no new or removed tests).
- [ ] The only domain `rewrite()` emits contains `oginstagram.com` for both `www`-prefixed and non-`www` URLs; `www.` preservation and no-match/empty behavior are unchanged (the existing six tests assert exactly this).
- [ ] `rg kkinstagram internal/` matches only the three divergence-documenting comment lines — no `kkinstagram` remains in any Go string literal or test expectation.
- [ ] Both Go doc comments explicitly state the divergence from Rust's `kkinstagram.com`.
- [ ] The commit contains exactly the two files in "Files" (no `CONTEXT.md`, no `docs/roadmap/`).

---

### Task 2: Record the divergence in the parity checklist

**Context:**
Task 1 changed behavior; this task keeps the port-verification checklist (`docs/parity/checklist.md`) truthful: items 44 and 46 under the `## instagram` section currently assert `kkinstagram.com` as part of 1:1 parity. After Task 1 they would be false. Reword them so the checklist states: all other mechanics remain 1:1, the target domain intentionally diverges, and the Go test anchors are the `oginstagram` ones. Without this, a future reader (or a diff-vs-Rust check) will "fix" the code back to `kkinstagram.com`.

**Files:**
- Modify: `docs/parity/checklist.md`
- Commit verbatim (content already in target state — created earlier, do NOT edit): `CONTEXT.md`, `docs/roadmap/instagram-rewrite-target.md`

**What to implement:**
- Item 44 — replace the line
  `- [ ] on match: rewrites by replacing only the domain group (group 2) with `kkinstagram.com`, preserving a leading `www.` when present, and posts the matched URL substring as a NEW message (src: instagram.rs:16,25,43-44)`
  with
  `- [ ] on match: rewrites by replacing only the domain group (group 2) with `oginstagram.com`, preserving a leading `www.` when present, and posts the matched URL substring as a NEW message — **deliberate divergence from Rust**, which rewrites to `kkinstagram.com`; all other mechanics 1:1 (src: instagram.rs:16,25,43-44)`
- Item 46 — replace the line
  `- [ ] parity test anchors (ported 1:1): `www.instagram.com` URL → `https://www.kkinstagram.com/...`; `instagram.com` without www → `https://kkinstagram.com/...`; empty / non-instagram → no match (src: instagram.rs:48-99)`
  with
  `- [ ] test anchors (Go, deliberately diverged on the target domain from the Rust anchors of `kkinstagram.com`): `www.instagram.com` URL → `https://www.oginstagram.com/...`; `instagram.com` without www → `https://oginstagram.com/...`; empty / non-instagram → no match (src: instagram.rs:48-99)`
- What NOT to change: the other five checkbox items in the section (lines 42, 43, 45, 47, 48: gating, trigger regex, pre-post embed suppression, logging, wiring — all remain 1:1) and the rest of the file (line 41 is the `## instagram` header, untouched).

**Steps:**
- [ ] Apply the two line replacements in `docs/parity/checklist.md`.
- [ ] Run `rg -n 'kkinstagram|oginstagram' docs/parity/checklist.md`
  - Does `kkinstagram.com` appear ONLY inside the two changed items (44's divergence note and 46's "Rust anchors" note) — i.e. at most the two lines above — and does `oginstagram.com` appear in both changed items? If anything else in the file mentions either domain, stop and review.
- [ ] Run `gofmt -l .` — did it print nothing? (No Go files changed in this task; this is just a gate check.)
- [ ] Stage ONLY the three files below and commit — do NOT use `git commit -a` or `git add -A`:
  `git add docs/parity/checklist.md CONTEXT.md docs/roadmap/instagram-rewrite-target.md && git commit -m "docs: record the oginstagram.com divergence (checklist, glossary, plan)"`
  Context for the pre-existing `CONTEXT.md` modification: its "Instagram rewrite" glossary entry was written in the same session that approved this design and is ALREADY in the target state (it describes `oginstagram.com` as the Go bot's domain). It needs no content edits — committing it here closes the loop so the tree ends clean.

**Acceptance criteria:**
- [ ] `docs/parity/checklist.md` items 44 and 46 read as specified above; the other five checklist items (gating, trigger regex, pre-post suppression, logging, wiring) still say 1:1.
- [ ] A reader of the checklist alone can tell that `kkinstagram.com` is RUST's domain and `oginstagram.com` is the Go bot's deliberate divergence.
- [ ] The commit contains exactly the three files in the commit step; no file outside this task was edited.
