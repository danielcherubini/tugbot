---
status: committed
done-when: A gated user's emoji-combo post (e.g. `<@…> 💸 🚵`) is deleted by the slow path (score ≥ T) with a `derpies_decisions` row written; a stored phrase (e.g. `buy me a bike`) is fast-deleted with zero asks; the `read_derpies_decisions` MCP tool returns rows; and the selftest + the DB-touching test gate are green.
---

# Derpies — score verdict, decision log, and phrase fast-match — Plan

**Goal:** Make the derpies filter score a message's gimmick-ness 0–100 (LLM `SCORE` + optional `WORD`) instead of a binary `GIMMICK`/`CLEAN`, threshold it with a live dial (delete threshold `T`, learn floor 40), fix the emoji-only dead end (the anchor gate only applies when the post has *word-like* tokens), log every judged message to `derpies_decisions` (read back via a new MCP tool), and fast-match stored multi-word phrases.

**Architecture:** The slow-path verdict becomes `SCORE:<0-100>` + optional `WORD:<anchor>`; the code thresholds it against a two-band matrix (learn at ≥40, delete at ≥T) with the existing two-arm word gate. The anchor requirement changes from "the message has text tokens" to "the post has word-like tokens" (an emoji-only post skips the anchor, so a semantic word passes). A `derpies_decisions` append-only row is written per judged message/edit via a single `defer` after the author-ID gate. Three tables (`derpies_config`, `derpies_decisions`, `derpies_gimmick_phrases`) + the `derpies_prompt` UPDATE land in one migration (000005) so the prompt flip, the DDL, and the code deploy are one atomic step. The `wordmatch` package is unchanged.

**Tech Stack:** Go, `jackc/pgx/v5/pgxpool`, `bwmarrin/discordgo`, the pi RPC (`app.PiBackend`), the `modelcontextprotocol/go-sdk` MCP server, `log/slog`.

**Constants (derpies.go, new):** `learnFloor = 40` (code constant), `defaultThreshold = 50` (code default for the DB dial; `T` is clamped to 41–100).

**The new `defaultPromptTemplate`** (used in Task 2 — set the Go constant, the `000003` seed byte-for-byte modulo `''`, and the `000005` `UPDATE` all to this exact text). The mandatory `{content}` / `{known}` markers and the optional `{{IMAGES}}` / `{{REF}}` / `{{EMBED}}` / `{{GIFS}}` markers are preserved. The `gimmickPrompt` `{known}` block gains a phrases sub-block (Task 2).

> **Retrieving the full prompt text:** `git show HEAD:docs/roadmap/derpies-score-model.md` — the prior spec (the last committed version of this file) contains the new SCORE prompt in its `## The prompt` section (the `defaultPromptTemplate` block, from `A Discord message was just posted by a user…` through `…the innocent reading is obvious.`). Task 2 copies that block **verbatim** into the three in-sync locations. After copying, **re-grep the new text for apostrophes** (`grep -o "'" | wc -l` on the block) and `''`-double every one in the SQL literal (a missed `''` breaks both the migration file and the byte-for-byte sync guard). It is the prior prompt with: (1) the binary `GIMMICK`/`CLEAN` reply protocol replaced by the `SCORE:<0-100>` + optional `WORD:<anchor>` protocol + a 4-band scoring scale; (2) a new `EMOJI-ENCODING` technique (a combo of emojis whose combined meaning is a roster solicitation; a single emoji is harmless alone); (3) the anchor rule relaxed to "the word must be a token of the message text AS IT APPEARS — EXCEPT when the gimmick lives ONLY in the emojis (the message has no other text words): then answer the most distinctive word of what the emojis MEAN"; and (4) "score toward the HIGHER side" replacing "GIMMICK" in the torn-between bands. Everything else (the roster framing, the technique list, the known-words block, the judgement rules) is unchanged.

---

### Task 1: Foundation — pure helpers + `store` interface + `poolStore` + test fakes

**Context:** Add the new seams and pure helpers the rest of the plan builds on, without changing any flow behavior yet. `parseVerdict` (the old binary parser) stays in place for this task (the message + nickname flows switch to `parseVerdictScore` in Tasks 2–3). Extending the `store` interface requires every implementation to satisfy it, so the single shared `fakeStore` (in `derpies_test.go`, reused by all derpies test files) is updated in the same task or the package won't compile. The `mcp.DecisionFilter` / `mcp.DecisionRow` types are also defined here (in `internal/mcp/mcp.go`) so the `queryDecisions` signature compiles at this boundary — Task 4 builds the `DecisionSource` seam + tool on top of them.

**Files:**
- Modify: `internal/handlers/derpies/derpies.go`
- Modify: `internal/handlers/derpies/derpies_test.go`
- Modify: `internal/mcp/mcp.go`

**What to implement:**

1. **`parseVerdictScore(text string) (score int, hasScore bool, word string)`** (replaces the *role* of `parseVerdict`, but coexists with it this task). Scans **all** lines (not just the first non-empty): each line is `TrimSpace`d first (the prompt displays the reply format **indented** — `  SCORE:<0-100>` — and an LLM echoing the indentation must still parse), then:
   - the first line matching `^SCORE:\s*(\d+)$` (case-insensitive) whose captured value is `0..100` is the score (`\s*` absorbs a space after the colon — `Score: 95` is a very common LLM shape);
   - the first line matching `^WORD:\s*(.+)$` (case-insensitive) is the word — the **remainder of the line** (trimmed, lowercased), so a spaced SPLIT answer (`WORD:z w i f t`) is captured and handed to the unchanged `foldVerdictWord` whitespace-collapse in the gate (ADR 0009's belt-and-suspenders: both the spaced and the collapsed answer form learn the collapsed word).
   - No valid SCORE line → `hasScore=false` (the caller treats it as "unrecognized"). A SCORE line out of range (`SCORE:150`) is NOT a valid score line (treated as absent → `hasScore=false`).

2. **`wordLike(tok string) bool`** — the anchor-gate token classifier (the emoji fix, A):
   ```go
   func wordLike(tok string) bool {
       if tok == "" || allDigitVerdict(tok) {
           return false
       }
       return wordmatch.WordValid(tok) || unicodeVerdictShape(tok)
   }
   ```

3. **`foldedTokenSequence(content string) []string`** — the ordered, edge-trimmed, folded token sequence (for the phrase match): `strings.Fields`, each token `wordmatch.FoldToASCII` + `strings.TrimFunc(edgePunct)`, **preserving order and duplicates**, dropping tokens that trim to `""`. (No dedup, no collapsed-run handling — that stays in `tokensForMatch`, unchanged.)

4. **`decisionRecord`** — the in-memory decision row (mirrors the `derpies_decisions` columns; the nullable fields are pointers so a zero value writes SQL `NULL`):
   ```go
   type decisionRecord struct {
       MessageID    string
       ChannelID    string
       AuthorID     string
       Content      string
       Path         *string // "fast" | "slow" | NULL (NULL for every arm that never reached a path — e.g. list fetch failed, pi unavailable, ask failed, unrecognized verdict)
       Score        *int   // NULL for fast rows + every arm that never reached the matrix
       Threshold    *int   // NULL, same as Score
       Word         *string
       Learned      bool
       Deleted      bool
       RejectReason *string // NULL when no rejection
   }
   ```
   `Path` is a `*string` (nullable) to match the DDL's `path text CHECK (path IN ('fast','slow'))` — a `NULL` path passes the CHECK (PostgreSQL evaluates CHECK on NULL as satisfied), so the pre-path arms (which never assign `Path`) persist as `path IS NULL` instead of being rejected by a `NOT NULL`/empty-string CHECK.

5. **`clampThreshold(v int) int`** — pure: `if v < 41 || v > 100 { return defaultThreshold }; return v` (the matrix is defined for `T > learnFloor` only; a `T ≤ 40` value is unrepresentable — the CHECK rejects it at write time, this clamps a pre-CHECK read).

6. **Extend the `store` interface** with four methods (the `poolStore` implements them; the `fakeStore` implements them as no-ops / recorders):
   - `configThreshold(ctx context.Context) (int, error)` — `SELECT delete_threshold FROM derpies_config LIMIT 1`. A DB error → `(0, err)`. A missing row (no rows) → `(0, err)` (a sentinel, e.g. `errors.New("derpies_config row missing")`). A valid row → `(clampThreshold(value), nil)` (the clamp is applied here per the spec; the caller's error/missing fallback is Task 2).
   - `listPhrases(ctx context.Context) ([]string, error)` — `SELECT phrase FROM derpies_gimmick_phrases ORDER BY phrase` (sorted, so the `{known}` phrases sub-block is byte-stable, mirroring `sortedKeys` for the words). A DB error propagates.
   - `recordDecision(ctx context.Context, d *decisionRecord) error` — `INSERT INTO derpies_decisions (message_id, channel_id, author_id, content, path, score, threshold, word, learned, deleted, reject_reason) VALUES ($1..$11)` (the pointer fields pass through as `NULL` when nil).
   - `queryDecisions(ctx context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error)` — the parameterized `SELECT` behind `ReadDecisions` (Task 4): compose the optional filter clauses (all AND-combined), `ORDER BY created_at DESC`, `LIMIT min(limit, 500)`. This method's signature references `internal/mcp` types, so **add `internal/mcp` to derpies.go's imports** (no cycle — `internal/mcp` does not import the handler). For this task the body may be a minimal correct implementation; Task 4 exercises it. **The `mcp.DecisionFilter` and `mcp.DecisionRow` types are defined in Task 1 (item 7 below), NOT Task 4** — they must exist before this signature compiles.

7. **Define the `mcp.DecisionFilter` and `mcp.DecisionRow` types in `internal/mcp/mcp.go`** (pure data types, no handler import, no cycle — safe to add in Task 1 so the `queryDecisions` signature above compiles; Task 4 then only adds the `DecisionSource` interface, the `NewServer` change, and the tool):
   ```go
   type DecisionFilter struct {
       AuthorID  string
       ChannelID string
       Path      string
       Deleted   *bool
       ScoreMin  *int
       ScoreMax  *int
       Since     *time.Time
       Until     *time.Time
       Limit     int // default 50, max 500 — clamped, not an error
   }
   type DecisionRow struct {
       ID         int64
       MessageID  string
       ChannelID  string
       AuthorID   string
       Content    string
       Path       *string
       Score      *int
       Threshold  *int
       Word       *string
       Learned    bool
       Deleted    bool
       RejectReason *string
       CreatedAt  time.Time
   }
   ```
   (Add `"time"` to `internal/mcp/mcp.go`'s imports if not already present.)

8. **Update the `fakeStore`** (derpies_test.go) to implement the four new methods: `configThreshold` (a `threshold int` + `thresholdErr error` field; **return `(clampThreshold(s.threshold), s.thresholdErr)`** so the zero value yields the default 50 — mirroring the `poolStore` contract and preventing a zero-threshold fake from making every scored verdict delete), `listPhrases` (a `phrases []string` + `phrasesErr error` field), `recordDecision` (a `recordErr error` field — if set, return it; otherwise append a COPY of the record to a `decisions []*decisionRecord` slice so later mutations don't alias), `queryDecisions` (a `queried mcp.DecisionFilter` recorder + a `queryRows []mcp.DecisionRow` / `queryErr error` field).

**Steps:**
- [ ] Write a failing `TestParseVerdictScore` table (derpies_test.go): `SCORE:95`+`WORD:zwift` → (95,true,"zwift"); `SCORE:95` alone → (95,true,""); `CLEAN` / `GIMMICK:zwift` (old) → (0,false,""); `SCORE:150` → (0,false,""); `SCORE:55\nWORD:زويفت` → (55,true,"زويفت"); indented `  SCORE:95\n  WORD:zwift` → (95,true,"zwift"); `Score: 95` (space after colon) → (95,true,""); prose-around (`Sure!\nSCORE:80\nWORD:swift\nHope that helps`) → (80,true,"swift"); spaced SPLIT `SCORE:70\nWORD:z w i f t` → (70,true,"z w i f t"). Run `go test ./internal/handlers/derpies/ -run TestParseVerdictScore -count=1` → fails (function missing).
- [ ] Implement `parseVerdictScore`. Re-run → passes.
- [ ] Write a failing `TestWordLike` table: `zwift`→true; `272889785318768641`→false; `derpies:1021692390177775657`→false; `""`→false; `خفيف`→true; `a` (1 rune)→false. Implement `wordLike` → passes.
- [ ] Write a failing `TestFoldedTokenSequence`: `"who wants to buy me a bike."` → `[who wants to buy me a bike]`; `"buy me a b i k e"` → `[buy me a b i k e]` (no collapse — that's tokensForMatch's job); a pure-punct token is dropped. Implement `foldedTokenSequence` → passes.
- [ ] Write a failing `TestClampThreshold`: `30`→50; `41`→41; `50`→50; `100`→100; `101`→50. Implement `clampThreshold` → passes.
- [ ] Extend the `store` interface + `poolStore` + `fakeStore` (the four methods). Run `go build ./... && go vet ./...` → compiles (the flow doesn't call the new methods yet).
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` → the existing tests still pass (the new fake methods are inert defaults).

**Acceptance criteria:**
- [ ] `parseVerdictScore`, `wordLike`, `foldedTokenSequence`, `clampThreshold` have passing unit tests (the cases above).
- [ ] The `store` interface has the four new methods; `poolStore` + `fakeStore` implement them; `go build ./... && go vet ./... && gofmt -l .` are clean.
- [ ] `internal/mcp` defines `DecisionFilter` + `DecisionRow` (pure data types); `go build ./...` is clean (the `queryDecisions` signature resolves).
- [ ] The `fakeStore.configThreshold` returns `(clampThreshold(s.threshold), s.thresholdErr)` — the zero value yields 50 (a unit test asserts `configThreshold` on a zero-value fake returns 50, not 0).
- [ ] `parseVerdict` (the old binary parser) is still present and its existing tests still pass (it is removed in Task 3).
- [ ] `go test ./internal/handlers/derpies/ ./internal/mcp/ -count=1` is green (no existing test regressed).

---

### Task 2: The message flow (score model) + the prompt flip

**Context:** Wire the score model into the message flow and flip the prompt to SCORE-format. This is the core of the feature: the slow path scores, the matrix decides learn/delete, the anchor gate is the emoji fix, the decision log records every terminal arm, and the phrase fast path is the optimizer. The prompt flip (Go constant + `000003` seed + `000005` migration + `gimmickPrompt` phrases sub-block) lands here so the code is never in a "new prompt + old parser" dead state. `parseVerdict` is KEPT (the nickname flow still uses it — Task 3 switches + removes it). **Because `gimmickPrompt`'s signature changes (gains a `phrases []string` param) and the parser switches to `parseVerdictScore`, EVERY `gimmickPrompt` call site and EVERY `GIMMICK:`/`CLEAN` test fixture in the package must be updated in this task or the package won't compile / the tests won't pass** — the Files list below enumerates all of them (the `nicknames.go` call site is a one-line `[]string{}` mechanical change; its score matrix lands in Task 3).

**Files:**
- Modify: `internal/handlers/derpies/derpies.go`
- Modify: `internal/handlers/derpies/nicknames.go` (the single `gimmickPrompt` call site → pass `[]string{}`; the nickname score matrix is Task 3)
- Modify: `internal/handlers/derpies/derpies_test.go` (the `fakeStore`/`fakePi` fixtures + the `gimmickPrompt` call sites + the `GIMMICK:`/`CLEAN` fixtures; the new `TestFlow*` unit tests land here, NOT the PG-gated integration file)
- Modify: `internal/handlers/derpies/derpies_images_test.go` (5 `gimmickPrompt` call sites + 4 `GIMMICK:` fixtures)
- Modify: `internal/handlers/derpies/derpies_embeds_test.go` (3 `gimmickPrompt` call sites + 3 `GIMMICK:` fixtures)
- Modify: `internal/handlers/derpies/derpies_gif_test.go` (1 `gimmickPrompt` call site + 4 `CLEAN` fixtures)
- Modify: `internal/handlers/derpies/derpies_edits_test.go` (2 `GIMMICK:` fixtures — edits run the same `flow`, so the matrix + decision-log defer now apply)
- Modify: `internal/handlers/derpies/derpies_video_test.go` (`CLEAN` fixtures — smoke-check compiles + passes)
- Create: `migrations/000005_derpies_score.up.sql`
- Modify: `migrations/000003_derpies_prompt.up.sql`

**What to implement:**

1. **The decision matrix** (replaces the current `switch kind` block, steps 8–10). After `score, hasScore, word := parseVerdictScore(text)`:
   - `!hasScore` → `slog.Warn("derpies unrecognized verdict — doing nothing")`, `dec.RejectReason = "unrecognized verdict"`, return (the existing degradation arm).
   - Read `T`: `t, err := h.store.configThreshold(ctx)`; `if err != nil { t = defaultThreshold; slog.Warn("derpies config threshold unavailable — using default", …) }` (the error/missing fallback; `configThreshold` already clamps a valid-but-out-of-range read).
   - Set `dec.Path` to a `"slow"` pointer (`slowPath := "slow"; dec.Path = &slowPath` — `Path` is a `*string`), `dec.Score = &score`, `dec.Threshold = &t`.
   - **Learn** (independent of delete), gated on `score ≥ learnFloor` (40): run the existing two-arm gate on `fw := foldVerdictWord(word)` — the shared all-digit precondition, arm (a) `wordmatch.WordValid`, arm (b) `unicodeVerdictShape` — **with the anchor requirement changed to `hasWordLikeTokens && !toks[fw]`** (see 2). On a pass: `h.store.addGimmick(ctx, fw, SourceLLM)`, `dec.Word = &fw`, `dec.Learned = true`. On a rejection: set `dec.RejectReason` to the specific reason (`no valid word` for `fw == ""`, `all-digit word`, `invalid word`, `verdict word not in message` for the anchor / arm-b rejections) — and **do NOT block the delete** (see 3).
   - **Delete** (independent of learn), `score ≥ T`: `h.ops.deleteMessage(…)`; on success `dec.Deleted = true` + `slog.Info("derpies delete (llm) …")` (log the score); on failure `dec.Deleted = false`, `dec.RejectReason = "delete failed"` (overrides any word-rejection reason) + `slog.Error`. A word stays learned even when the delete fails (the existing discipline).
   - `score < 40` → no learn, no delete (the matrix's do-nothing band) — `dec` carries only `Score`/`Threshold`.

2. **The anchor gate (the emoji fix, A).** Replace the current `hasTextTokens` (union, non-empty) with `hasWordLikeTokens` computed from **`tokensForMatch(m.Content)` only** (the posted message — the prompt's "the message has no other text words" scope):
   ```go
   hasWordLikeTokens := false
   for tok := range tokensForMatch(m.Content) {
       if wordLike(tok) { hasWordLikeTokens = true; break }
   }
   ```
   The **anchor check** (`!toks[fw]`) still uses the **union** `toks` (posted + referenced + embed titles — the word may legitimately anchor to the quoted content). So: `if hasWordLikeTokens && !toks[fw] { reject "verdict word not in message" }`, and arm (b) `if !asc && !hasWordLikeTokens { reject "verdict word not in message" }` (a non-ASCII word needs a word-like anchor; an emoji-only post has none → arm b stays dead, arm a (ASCII) is bounded by `wordValid` alone and passes). A mention snowflake (all-digit) / emoji-ref (colon) / pure-emoji token (folds to `""`) is **not** word-like, so a "mention + emoji" post is judged like an image-only post: a valid ASCII word passes, the score decides.

3. **The decision log (C).** One `defer` placed **right after the author-ID gate (step 3)**: `defer h.recordDecision(ctx, m, dec)` where `dec` is a local `*decisionRecord` the flow populates as it progresses (the pointer is captured at defer-time, so mutations are seen). Pre-gate arms (feature off, not a guild, author not gated) return **before** the defer → no row. Terminal arms populate `dec` before returning:
   - list fetch failed → `dec.RejectReason = "list fetch failed"` (Score/Threshold/Path all NULL — the arm never reached a path).
   - fast word hit → `dec.Path = fastPtr` (a `"fast"` pointer), `dec.Word = &tok`, `dec.Deleted = true` (or `dec.RejectReason = "delete failed"`).
   - fast phrase hit (see 4) → `dec.Path = fastPtr`, `dec.Word = &phrase`, `dec.Deleted = true` (or `delete failed`).
   - pi unavailable → `dec.RejectReason = "pi unavailable"`.
   - ask failed → `dec.RejectReason = "ask failed"`.
   - unrecognized verdict / matrix arms → as in 1.
   `recordDecision` is best-effort: a write failure logs `slog.Error` (module derpies) and does **not** abort (the delete/learn already happened). `dec.Content = m.Content`, `dec.MessageID = m.ID`, `dec.ChannelID = m.ChannelID`, `dec.AuthorID = m.Author.ID` are set up front (after the author gate).

4. **The phrase fast match (B).** After the word fast path (step 4), before images (4.5): `phrases, err := h.store.listPhrases(ctx)`; on error → `slog.Error` + skip the phrase match (continue to the slow path — never act on a half-loaded phrase list). For each stored phrase: split with `strings.Fields` into its tokens, **fold each phrase token with `wordmatch.FoldToASCII` + the same `edgePunct` trim** (a curated `Buy me a bike` must match the folded `buy` — curation is SQL-only, so operator casing is a real case; `strings.Fields` also drops any empty tokens from double/leading spaces), then slide a window of that length over `foldedTokenSequence` of the **posted content and the referenced content SEPARATELY** (never concatenated — a phrase spanning the message/reply boundary must not match), mirroring how the word fast path unions the two. Skip a phrase with no non-empty tokens. **Embed titles are NOT scanned for phrases** in v1. An exact consecutive match → fast hit: delete (zero asks) + `dec` (`path=fast`, `word=<phrase>`, `deleted`) + the same delete-failed/success log shape as the word fast path. (A SPLIT word inside a phrase is NOT fast-matched — v1 is exact-consecutive only; it falls to the slow path.)

5. **`gimmickPrompt` — the `{known}` phrases sub-block.** The `{known}` block assembly gains a second sub-block appended after the words:
   ```
   -----< known gimmick words (sorted ascending) >-----
   <words>
   -----< known gimmick phrases (exact multi-word patterns) >-----
   <phrases>
   ```
   The `gimmickPrompt` signature gains a `phrases []string` parameter (the message flow passes the stored phrases; the nickname flow passes `[]string{}` — see Task 3). Only the phrases sub-block is emitted when phrases exist; the words sub-block is unchanged. The `{known}` marker in the template is unchanged (the code assembles the combined block).

6. **The prompt flip (three in-sync locations, all to the Task-1-noted new `defaultPromptTemplate` text):**
   - `defaultPromptTemplate` (derpies.go) — the new SCORE-format prompt.
   - `migrations/000003_derpies_prompt.up.sql` — the seed body set to the new prompt **byte-for-byte** (modulo the `''` doubling for the two apostrophes: `filter's` → `filter''s`, `message's` → `message''s`). `TestMigration000003AppliesAndSeeds` is the forever sync guard.
   - `migrations/000005_derpies_score.up.sql` — **create this migration** with the three objects + the prompt UPDATE (the DDL below). The `UPDATE derpies_prompt SET body = <new score prompt>, updated_at = now();` makes the live row SCORE-format in the same atomic deploy.

   **The `000005` DDL** (three objects + the prompt UPDATE; `derpies_gimmicks` and `derpies_prompt` shapes are unchanged):
   ```sql
   -- derpies_config (single row — the operator's live dials)
   CREATE TABLE public.derpies_config (
       id integer NOT NULL,
       delete_threshold integer DEFAULT 50 NOT NULL,
       updated_at timestamp without time zone DEFAULT now() NOT NULL
   );
   CREATE SEQUENCE public.derpies_config_id_seq AS integer START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
   ALTER SEQUENCE public.derpies_config_id_seq OWNED BY public.derpies_config.id;
   ALTER TABLE ONLY public.derpies_config ALTER COLUMN id SET DEFAULT nextval('public.derpies_config_id_seq'::regclass);
   ALTER TABLE ONLY public.derpies_config ADD CONSTRAINT derpies_config_pkey PRIMARY KEY (id);
   ALTER TABLE ONLY public.derpies_config ADD CONSTRAINT derpies_config_threshold_check CHECK (delete_threshold BETWEEN 41 AND 100);
   INSERT INTO public.derpies_config (id, delete_threshold) VALUES (1, 50);

   -- derpies_decisions (append-only — one row per judged message/edit)
   CREATE TABLE public.derpies_decisions (
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
   CREATE INDEX derpies_decisions_author_created_idx ON public.derpies_decisions (author_id, created_at DESC);
   CREATE INDEX derpies_decisions_created_idx ON public.derpies_decisions (created_at DESC);

   -- derpies_gimmick_phrases (manual-only — the optimizer)
   CREATE TABLE public.derpies_gimmick_phrases (
       id integer NOT NULL,
       phrase character varying(300) NOT NULL,
       source character varying(8) DEFAULT 'manual' NOT NULL,
       created_at timestamp without time zone DEFAULT now() NOT NULL
   );
   CREATE SEQUENCE public.derpies_gimmick_phrases_id_seq AS integer START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
   ALTER SEQUENCE public.derpies_gimmick_phrases_id_seq OWNED BY public.derpies_gimmick_phrases.id;
   ALTER TABLE ONLY public.derpies_gimmick_phrases ALTER COLUMN id SET DEFAULT nextval('public.derpies_gimmick_phrases_id_seq'::regclass);
   ALTER TABLE ONLY public.derpies_gimmick_phrases ADD CONSTRAINT derpies_gimmick_phrases_pkey PRIMARY KEY (id);
   ALTER TABLE ONLY public.derpies_gimmick_phrases ADD CONSTRAINT derpies_gimmick_phrases_phrase_key UNIQUE (phrase);

   -- The prompt flip (atomic with the DDL — see Rollout).
   UPDATE public.derpies_prompt SET body = <NEW SCORE PROMPT, ''-doubled>, updated_at = now();
   ```

**Steps:**
- [ ] Write a failing `TestFlowDecisionMatrix` (derpies_test.go, fake store + fake pi): score < 40 → no delete/learn; 40 ≤ score < T → learn, no delete; score ≥ T → learn + delete; score ≥ T with no word → delete, no learn; word rejected (all-digit / invalid / not anchored) → the specific `reject_reason`, delete still reflects `score ≥ T`. Set `fakePi.resp` to the SCORE-format string and `fakeStore.threshold` to a known T. Run `go test ./internal/handlers/derpies/ -run TestFlowDecisionMatrix -count=1` → fails.
- [ ] Implement the matrix + the anchor gate (2) + the decision-log defer (3). Re-run → passes.
- [ ] Write a failing `TestFlowAnchorGateEmoji` (derpies_test.go, fake store + fake pi — NOT the PG-gated integration file): a "mention + emoji" message (`<@272889785318768641> 💸 <:derpies:1021692390177775657> 🚵`) with a `SCORE:95 WORD:zwift` verdict → `hasWordLikeTokens=false` → the word passes → delete + learn. A message WITH word-like text tokens + a `WORD` not in the message → rejected (`verdict word not in message`). Run → fails, implement → passes.
- [ ] Write a failing `TestFlowPhraseFastMatch` (derpies_test.go): a stored `buy me a bike` (via `fakeStore.phrases`) + a post `who wants to buy me a bike` → fast delete, zero pi asks (`fakePi.asks == 0`), a `derpies_decisions` row (`path=fast`, `word=buy me a bike`). A post with a SPLIT word inside the phrase (`buy me a b i k e`) → NOT fast-matched → falls to the slow path. Run → fails, implement the phrase match (4) → passes.
- [ ] Write a failing `TestFlowDecisionLogArms` (derpies_test.go): each terminal arm (list fetch failed, fast hit, pi unavailable, ask failed, unrecognized, score<40, 40≤score<T, score≥T, score≥T delete-failed, word-rejected) writes the expected `dec` (content, path, score, threshold, word, learned, deleted, reject_reason); a `recordDecision` insert failure (a `fakeStore` with `recordErr` set) logs and does not abort the delete. Run → fails, implement (3) → passes.
- [ ] Write a failing `TestFlowConfigFallback` (derpies_test.go): a `configThreshold` error / missing row → the flow uses `defaultThreshold` (50) + a `slog.Warn` (assert the matrix used T=50). Run → fails, implement the fallback (1) → passes.
- [ ] Implement `gimmickPrompt`'s phrases sub-block (5) + the `phrases` parameter; update the message flow's `gimmickPrompt` call to pass the stored phrases.
- [ ] **Update every other `gimmickPrompt` call site + every `GIMMICK:`/`CLEAN` fixture in the package** (the compile/test break the signature + parser change causes): (a) `nicknames.go` (the single `gimmickPrompt` call site, ~line 147) → add the 8th `[]string{}` argument (one-line mechanical change; the nickname score matrix is Task 3); (b) `derpies_images_test.go` (5 call sites), `derpies_embeds_test.go` (3), `derpies_gif_test.go` (1) → add the 8th argument (the stored phrases, or `[]string{}` where the test doesn't need them); (c) every `GIMMICK:<w>` fixture **asserting a learn/delete** → `"SCORE:<n>\nWORD:<w>"` with `n ≥ 60` (the learn band) and a `fakeStore.threshold` set to a known T; every `CLEAN` fixture → `"SCORE:10"` (the do-nothing band) or left as-is only if it asserts "unrecognized → no action"; **(d) every rejection-arm fixture asserting NO-learn AND NO-delete on an invalid/unanchored word** (`TestFlowUnicodeVerdictWordNotInMessageStillRejected`, `TestFlowUnicodeVerdictWordPunctuationRejected`, `TestFlowUnicodeVerdictAllDigitsRejected`, `TestFlowVerdictBaseFormStillRejected`, `TestFlowVerdictHallucinatedWordAbsentFromMessage`, `TestFlowVerdictInvalidWord`, `TestFlowSplitVerdictNotAnchoredStillRejected`) → `"SCORE:<n>\nWORD:<w>"` with `n` in the **40..T-1 band** and a known T (e.g. T=60, `SCORE:45`) — learn is attempted, the word is rejected, no learn, no delete, preserving their discriminating intent (delete-despite-rejection stays pinned by `TestFlowDecisionMatrix`); **(e) the two template-content assertions** `TestGimmickPromptDefault` + `TestGimmickPromptMissingOptionalMarkers` (which assert the rendered prompt contains `"  GIMMICK:<word>"`) → update the asserted reply-format strings to the new template's reply lines (`"  SCORE:<0-100>"` / `"  WORD:<anchor>"`); their other asserted phrases ("HE WILL TEST THIS FILTER", "Techniques he uses", "Judgement rules") survive unchanged. Run `go build ./... && go test ./internal/handlers/derpies/ -count=1` → compiles + green.
- [ ] Write `TestMigration000005AppliesAndSeeds` (derpies_integration_test.go, PG-gated, mirroring the 000003 test's shape): precondition `DROP TABLE IF EXISTS derpies_config/derpies_decisions/derpies_gimmick_phrases CASCADE` + recreate `derpies_prompt` so the UPDATE applies; run the real `000005` file through `dbmigrate.Run`; assert the `derpies_config` row (id=1, delete_threshold=50), the `derpies_gimmick_phrases` shape (the UNIQUE phrase constraint), and `derpies_prompt.body == defaultPromptTemplate` (which also pins the 000005 UPDATE text to the constant). (It fails at this point because `000005` doesn't exist yet; it passes once the next step creates the file.)
- [ ] Flip the prompt (6): set `defaultPromptTemplate`, the `000003` seed (byte-for-byte modulo `''`), and create `000005` (the DDL + the prompt UPDATE). `TestMigration000005AppliesAndSeeds` now passes (with the compose PG).
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` → green. Run `go test ./internal/handlers/derpies/ -run 'TestMigration000003AppliesAndSeeds|TestMigration000005AppliesAndSeeds' -count=1` (with the compose PG) → both the sync guard (seed == constant) and the 000005 migration test pass.

**Acceptance criteria:**
- [ ] The message flow scores via `parseVerdictScore` and applies the matrix (the `TestFlowDecisionMatrix` cases pass).
- [ ] The emoji-combo dead end is fixed (the `TestFlowAnchorGateEmoji` observed case deletes + learns).
- [ ] The phrase fast path fast-deletes a stored phrase with zero asks (`TestFlowPhraseFastMatch`).
- [ ] Every terminal arm writes a `derpies_decisions` row (`TestFlowDecisionLogArms`); a write failure degrades (logs, no abort).
- [ ] `TestMigration000003AppliesAndSeeds` passes (the seed is byte-for-byte the new `defaultPromptTemplate`); `TestMigration000005AppliesAndSeeds` passes (the three tables + the prompt UPDATE apply clean, `derpies_prompt.body == defaultPromptTemplate`).
- [ ] The config fallback degrades to `defaultThreshold` (50) + a `slog.Warn` (`TestFlowConfigFallback`).
- [ ] Every `gimmickPrompt` call site (derpies.go + nicknames.go + the 5 test files) + every `GIMMICK:`/`CLEAN` fixture is updated (the 8th argument + the SCORE-format fixtures) — the package compiles + all tests pass.
- [ ] `go build ./... && go vet ./... && gofmt -l .` are clean; `go test ./internal/handlers/derpies/ -count=1` is green.

---

### Task 3: The nickname flow (score model) + remove `parseVerdict`

**Context:** The nickname flow (`nicknames.go`) shares `parseVerdict`, `defaultPromptTemplate`, the live `derpies_prompt` row, and `gimmickPrompt`. Once the live prompt is SCORE-format (Task 2), every nickname verdict parsed by the old `parseVerdict` is "unrecognized" → no resets ever. So the nickname flow moves to the score model in lockstep, and `parseVerdict` (now unused by either flow) is removed. The nickname flow is NOT logged to `derpies_decisions` in v1 (a nickname is not a message) — its decisions stay in the slog log.

**Files:**
- Modify: `internal/handlers/derpies/nicknames.go`
- Modify: `internal/handlers/derpies/derpies.go` (remove `parseVerdict`)
- Modify: `internal/handlers/derpies/derpies_test.go` (remove `TestParseVerdict` — it lives here, not in `nicknames_test.go`)
- Modify: `internal/handlers/derpies/nicknames_test.go`

**What to implement:**

1. **Parse:** `nickFlow` calls `parseVerdictScore` (not the removed `parseVerdict`). `!hasScore` → `slog.Warn("derpies nickname unrecognized verdict — doing nothing")` + `h.saveNick(key, evt.Nick)` (the existing unknown-arm cache write) + return.

2. **T:** the same `store.configThreshold` seam + the Task-2 fallback (50) + clamp.

3. **The nickname matrix** (mirrors the message matrix; the action is the nick **reset**, not a delete):
   ```
   score < 40:            do nothing (no reset, no learn)
   40 ≤ score < T:        learn the word (if valid+anchored to the nick); no reset
   score ≥ T:             learn the word (if valid+anchored) + reset the nick
   ```
   - **Fast path** (word-only, unchanged): a known **word** in the nick → `resetNow`, zero asks. Phrase matching is **not** added to the nickname flow in v1 (a nickname is a short display name; the word fast path covers it). `nickFlow` already passes `[]string{}` (empty phrases) to the `gimmickPrompt` signature (done in Task 2's mechanical call-site update), so the nickname prompt's `{known}` block carries words only.
   - **Learn gate:** the existing ADR 0008 arm (`wordValid` + folded-token-in-nick), gated on `score ≥ 40`. The `wordLike` anchor fix (Task 2) applies: a nick with **no word-like tokens** (an all-emoji nick) skips the anchor requirement → a valid word passes, the score decides.
   - **Reset** = `score ≥ T` (independent of whether a word was learned) via the existing `resetNow` (which already caches both the success and the failure outcome and marks the 60s window).

4. **Cache write (load-bearing — mirrors today's discipline):** the `lastNick` change-detection cache is written on every **judged** terminal arm so a later `GUILD_MEMBER_UPDATE` for the same nick (a role change, a mute, etc.) is skipped by `cur == evt.Nick` and never re-judged. The `score < 40` and `40 ≤ score < T` arms call `h.saveNick(key, evt.Nick)` (the clean/unknown analog); `score ≥ T` goes through `resetNow`. The **pre-matrix failure arms do NOT cache** (`list fetch failed`, `pi unavailable`, `ask failed`) — a transient failure must stay retryable, so the nick is re-judged on the next event. (The 60s `lastEdit` cooldown is separate — marked only by `resetNow`.)

5. **Remove `parseVerdict`** (derpies.go) — it is now unused by either flow. (Its existing unit tests are replaced by the `parseVerdictScore` tests from Task 1.)

**Steps:**
- [ ] Write a failing `TestNickFlowMatrix` (nicknames_test.go, the existing fake-pi seam — set `fakePi.resp` to the SCORE-format string): score < 40 → no reset (`fakeOps.sets == 0`); 40 ≤ score < T → learn, no reset; score ≥ T → reset (`fakeOps.sets == 1`); an all-emoji nick + a semantic `WORD` → reset; an all-emoji nick + a `WORD` not anchored and score < 40 → no reset. Run `go test ./internal/handlers/derpies/ -run TestNickFlowMatrix -count=1` → fails.
- [ ] Implement the nickname matrix (3) + the parse switch (1) + the T seam (2). Re-run → passes.
- [ ] Write a failing `TestNickFlowCacheNoResetArm` (nicknames_test.go, the cache case): two `GUILD_MEMBER_UPDATE` events with the same nick, the first scoring < 40 → the second (a role-only change, same `evt.Nick`) must produce **zero** pi asks (`fakePi.asks == 1` total — the second event is skipped by `cur == evt.Nick`). The `saveNick` on the no-reset arm is what makes the second event skip. Run → fails, implement the cache discipline (4) → passes.
- [ ] Remove `parseVerdict` + its now-obsolete unit tests. Run `go build ./... && go vet ./...` → compiles (no reference to `parseVerdict` remains).
- [ ] Run `go test ./internal/handlers/derpies/ -count=1` → green (the existing nickname tests, updated to the SCORE-format `fakePi.resp`, pass).

**Acceptance criteria:**
- [ ] The nickname flow scores via `parseVerdictScore` and applies the matrix (the `TestNickFlowMatrix` cases pass).
- [ ] An all-emoji nick + a semantic `WORD` resets; a non-anchored `WORD` at score < 40 does not.
- [ ] The cache case holds: a `score < 40` arm writes `lastNick`, so a same-nick follow-up event produces zero pi asks (`TestNickFlowCacheNoResetArm`).
- [ ] Pre-matrix failure arms do NOT write `lastNick` (the nick stays retryable).
- [ ] `parseVerdict` is removed; `go build ./... && go vet ./...` are clean; `go test ./internal/handlers/derpies/ -count=1` is green.

---

### Task 4: The MCP tool (`read_derpies_decisions`)

**Context:** `internal/mcp` today has exactly ONE seam (`DiscordAPI`, wrapping discordgo session methods) and **no** DB/pool access — so this tool is new architectural surface. A `DecisionSource` seam lets the handler (which has the pool) satisfy a read the MCP server exposes as a tool. `internal/mcp` defines the seam + the I/O contract types; `internal/handlers/derpies` imports `internal/mcp` for the types and implements the seam (no cycle — `internal/mcp` does not import the handler).

**Files:**
- Modify: `internal/mcp/mcp.go`
- Modify: `internal/mcp/tools_read.go` (or create `internal/mcp/tools_decisions.go`)
- Modify: `internal/mcp/mcp_test.go`
- Modify: `internal/handlers/derpies/derpies.go` (the `ReadDecisions` public method)
- Modify: `cmd/tugbot/main.go`

**What to implement:**

1. **New seam interface in `internal/mcp`** (mcp.go):
   ```go
   type DecisionSource interface {
       ReadDecisions(ctx context.Context, f DecisionFilter) ([]DecisionRow, error)
   }
   ```
   **The `DecisionFilter` and `DecisionRow` types are ALREADY defined in Task 1** (item 8, in `internal/mcp/mcp.go`) — Task 4 only adds this `DecisionSource` interface (which references them). `DecisionFilter`: `AuthorID string`, `ChannelID string`, `Path string`, `Deleted *bool`, `ScoreMin *int`, `ScoreMax *int`, `Since *time.Time`, `Until *time.Time`, `Limit int` (default 50, max 500 — clamped, not an error). `DecisionRow` mirrors the `derpies_decisions` columns (`ID int64`, `MessageID`, `ChannelID`, `AuthorID`, `Content`, `Path *string`, `Score *int`, `Threshold *int`, `Word *string`, `Learned bool`, `Deleted bool`, `RejectReason *string`, `CreatedAt time.Time`).

2. **`NewServer` signature change**: `NewServer(d DiscordAPI, decisions DecisionSource, port int) *Server`. Store `decisions` on the `Server` struct. Update the `registerReadTools` call (or add `registerDecisionsTools(srv, decisions)`) so the new tool gets the seam.

3. **`Derpies.ReadDecisions(ctx context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error)`** — the handler's public surface (the `Invoke`-style seam the Feature-tools glossary describes). It calls `h.store.queryDecisions(ctx, f)` (added in Task 1) — a parameterized `SELECT` with the filter clauses composed (all optional, AND-combined), `ORDER BY created_at DESC`, `LIMIT min(limit, 500)` (the `Limit` default/clamp applied here: `limit <= 0 → 50`, `limit > 500 → 500`). **Normalize `Since`/`Until` to UTC before binding** (the `created_at` column is `timestamp without time zone`; pgx encodes a `*time.Time` with its offset, so a non-UTC value would compare against the session's timezone interpretation — `Since = Since.UTC()`, `Until = Until.UTC()`). A DB error propagates (the MCP tool surfaces it as a tool error — a read tool failing is not a silent degradation).

4. **New MCP tool `read_derpies_decisions`** (registered in `tools_read.go` — or a new `tools_decisions.go`; `tools.go` is the shared-helpers file, not where tools register): parses the tool arguments (a `readDecisionsArgs` struct mirroring `DecisionFilter` with `json` tags) into a `DecisionFilter`, calls `s.decisions.ReadDecisions`, returns the rows as a JSON structured payload (top-level array under a `"decisions"` key — mirroring `read_messages`'s `"messages"` convention) + a short text summary (`N decision(s)`). A `ReadDecisions` error → a `toolErr("tugbot", …)` IsError result. ADR 0004's LAN-trust posture applies — the tool is read-only and the embedded server is LAN-bound.

5. **Production wiring (`cmd/tugbot`)** — there are **two** `NewServer` call sites:
   - The **selftest** one (in the `--selftest` path, currently `_ = newHandlers(a)` then `mcp.NewServer(mcp.NewRealDiscord(d), cfg.MCPPort)`): it discards the handlers, so it must **keep the handler** (assign `h := newHandlers(a)`) and pass `h.derpies` as the `DecisionSource` (or pass a no-op `DecisionSource`), to compile.
   - The **production** one (in `run()`, `h := newHandlers(a)` then `mcp.NewServer(mcp.NewRealDiscord(d), cfg.MCPPort)`): pass `h.derpies` (the `*Derpies` handler implements `ReadDecisions` → satisfies `DecisionSource`).
   - The production construction's tool-list `slog` line (which hardcodes `"… read_messages, post_message, react"`) gains `read_derpies_decisions`.

6. **Test fake** (`mcp_test.go`): a fake `DecisionSource` (mirrors the existing `DiscordAPI` fake pattern) — a `decisions []mcp.DecisionRow` + `filter mcp.DecisionFilter` recorder + `err error`. Update the existing `NewServer` call sites in `mcp_test.go` to pass the fake.

**Steps:**
- [ ] Write a failing `TestReadDecisionsFilter` (derpies_test.go or a new derpies_mcp_test.go, fake store): the `queryDecisions`/`ReadDecisions` filter clauses (author, channel, path, deleted, score range, since/until, limit) compose correctly (assert the `fakeStore.queried` `DecisionFilter`), ordered `created_at DESC`, `Limit` default (50) + clamp (500). Run `go test ./internal/handlers/derpies/ -run TestReadDecisionsFilter -count=1` → fails.
- [ ] Implement `Derpies.ReadDecisions` (3) + finalize `queryDecisions` (Task 1's stub) → passes.
- [ ] Write a failing `TestReadDerpiesDecisionsTool` (mcp_test.go, the fake `DecisionSource`): the tool parses the args into a `DecisionFilter`, calls `ReadDecisions`, returns the rows under a `"decisions"` key + a text summary; an error → a `toolErr` IsError result. Run `go test ./internal/mcp/ -run TestReadDerpiesDecisionsTool -count=1` → fails.
- [ ] Implement the `DecisionSource` seam (1) + the `NewServer` signature change (2) + the `read_derpies_decisions` tool (4). Update the `mcp_test.go` fakes (6). **Update the two "exactly 5" tool-count pins in `mcp_test.go` — `TestListToolsShowsExactlyFiveTools` and `TestWireEndToEnd` — to expect 6 tools and include `read_derpies_decisions` in their expected-name lists** (the 6th tool breaks both as written); rename `TestListToolsShowsExactlyFiveTools` → `TestListToolsShowsExactlySixTools` so the name isn't a lie. Re-run → passes.
- [ ] Write a failing PG-backed `TestQueryDecisionsSQL` (derpies_integration_test.go, compose PG): insert a few rows through `recordDecision` (exercising the NULL-pointer binds — a `path=NULL` pre-path row, a `score=NULL` fast row, a `reject_reason=NULL` row), then call `queryDecisions` with each filter clause and assert the WHERE-building, `ORDER BY created_at DESC`, and the `LIMIT` clamp for real (a typo in the composed SQL ships green otherwise). Run (with the compose PG) → fails, finalize `queryDecisions` → passes.
- [ ] Update the two `cmd/tugbot` `NewServer` call sites (5) + the tool-list `slog` line. Run `go build ./...` → compiles.
- [ ] Run `go test ./internal/mcp/ ./internal/handlers/derpies/ ./cmd/tugbot/ -count=1` → green.

**Acceptance criteria:**
- [ ] `internal/mcp` has the `DecisionSource` seam (the `DecisionFilter`/`DecisionRow` types were defined in Task 1); `NewServer` takes the seam.
- [ ] `Derpies.ReadDecisions` composes the filter clauses correctly (normalizes `Since`/`Until` to UTC) and propagates DB errors (`TestReadDecisionsFilter` + the PG-backed `TestQueryDecisionsSQL`).
- [ ] The `read_derpies_decisions` tool returns rows (JSON under `"decisions"` + a text summary) and surfaces errors as `toolErr` (`TestReadDerpiesDecisionsTool`).
- [ ] The two "exactly 5" tool-count pins in `mcp_test.go` are updated to expect 6 (including `read_derpies_decisions`).
- [ ] Both `cmd/tugbot` `NewServer` call sites compile (selftest keeps the handler / passes a source; production passes `h.derpies`); the tool-list `slog` line names `read_derpies_decisions`.
- [ ] `go build ./... && go vet ./... && gofmt -l .` are clean; `go test ./internal/mcp/ ./internal/handlers/derpies/ ./cmd/tugbot/ -count=1` is green.

---

## Cross-cutting — rollout + verification (applies across Tasks 1–4)

**Rollout (one atomic deploy):** the prompt flip lives **inside migration 000005** (not a manual pre-deploy UPDATE): the deploy runs migrations then restarts, so the prompt, the config/decisions/phrases DDL, and the new code all flip in **one atomic step**. (A manual pre-deploy `UPDATE derpies_prompt` would create a guaranteed slow-path-dead window: old code + new SCORE-format prompt → every verdict "unrecognized" → zero slow-path deletes/learns until the deploy completes. The reverse order is equally dead: new code + old binary prompt → `hasScore=false`.)

1. **Migration** `000005_derpies_score.up.sql` (Task 2) — the three objects + the prompt UPDATE. Applied by the deploy (or `make migrate`).
2. **`000003` seed update** (Task 2) — the `derpies_prompt` seed set to the new score prompt (byte-for-byte with the Go constant, modulo the `''` doubling) — for fresh DBs + the `TestMigration000003AppliesAndSeeds` sync guard. (000003 is already applied in production; editing it does not re-run there — the production flip comes from 000005's UPDATE. On a fresh DB, 000003 seeds the score prompt and 000005's UPDATE re-sets it to the same text — a no-op.)
3. **Code** (Tasks 1–4) — the derpies package + the `internal/mcp` tool + the `cmd/tugbot` wiring.
4. **Deploy**: `ssh root@tugbot update-tugbot` (pulls main, runs migrations — including 000005's prompt UPDATE — builds, restarts). Confirm the head commit matches the pushed commit and the service is `active (running)`.
5. **Verify** (below).

Residual risk: a deploy that fails *after* the migration applies but *before* the restart leaves the prompt SCORE-format while the old code still runs — the slow path is dead until a re-deploy. Manual rollback: `UPDATE derpies_prompt SET body = <previous binary prompt>, updated_at = now();` (the previous body is in git history / the pre-deploy DB).

**Verification (per AGENTS.md):**
- **The observed case:** a subsequent `<@…> 💸 🚵`-style post by the gated user is deleted by the slow path (a `derpies delete (llm)` log line with a score) — or, once a phrase is curated, by the fast path.
- **The decision log:** `SELECT message_id, path, score, threshold, word, learned, deleted, reject_reason FROM derpies_decisions ORDER BY created_at DESC LIMIT 10;` returns rows; the `read_derpies_decisions` MCP tool returns the same.
- **The selftest:** `go run ./cmd/tugbot --selftest` logs "Discord session and all thirteen handlers and the MCP server constructed", exit 0.
- **The full gate:** `go build ./... && go vet ./... && gofmt -l . && make lint && go test ./...`, then the DB-touching gate (`make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...`).

**Out of scope (follow-ups):** non-ASCII phrase tokens (v1 phrases are ASCII-only); `/gimmick` phrase subcommands (v1 curation is SQL-only); logging the nickname flow to `derpies_decisions` (v1: message flow only); a `derpies_decisions` prune/retention tool (v1: keep everything); a SPLIT word inside a phrase (v1: exact-consecutive match only).
