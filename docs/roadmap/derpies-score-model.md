---
status: approved
done-when: A gated user's emoji-combo post (e.g. `<@…> 💸 🚵`) is deleted by the slow path (score ≥ T) with a `derpies_decisions` row written; a stored phrase (e.g. `buy me a bike`) is fast-deleted with zero asks; the `read_derpies_decisions` MCP tool returns rows; and the selftest + the DB-touching test gate are green.
---

# Derpies — score verdict, decision log, and phrase fast-match

## Context

The 2026-09-18 prod observation (channel 1044752345583599626, message
1550528637319512095): the filtered user posted `<@…> 💸 <:derpies:…> 🚵` —
a mention + a money emoji + his custom "derpies" emoji (the person he
pretends to be) + a cyclist emoji. The meaning is a **combo** (*buy me a
bike* = `zwift`). The message survived, and the failure is structural at
**two layers at once** (full analysis in
`docs/decisions/0010-derpies-score-verdict.md`):

1. **Fast path miss.** `tokensForMatch` yields exactly two tokens — the
   mention snowflake and the custom-emoji ref. The emojis fold to nothing.
   Neither token is a stored word → no zero-ask delete.
2. **Verdict dead end.** The LLM judged `CLEAN` (it cannot decode the combo),
   but even a correct `GIMMICK:zwift` could never act: the step-9 gate
   requires the verdict word to be a token of the message, and the only
   tokens are the snowflake (all-digit → rejected) and the emoji-ref (not a
   word shape → rejected). The message could **never** be deleted.

The operator's insight: a single emoji is **harmless alone**; the **combo**
is the gimmick. A deterministic matcher sees tokens, not meaning — only a
whole-message semantic judgement can score a combo. And the binary
`GIMMICK`/`CLEAN` verdict has no middle ground, so there is no dial to tune
as the evasion evolves.

This spec delivers three coordinated changes (A + B + C) plus the unifying
**score model**:

- **A — the emoji fix:** the anchor gate only applies when the message has
  *word-like* tokens; the prompt teaches the LLM to score a combo and to
  answer a semantic word when the anchor lives only in the emojis.
- **The score model (ADR 0010):** the slow-path verdict becomes
  `SCORE:<0-100>` + optional `WORD:<anchor>`; the code thresholds it (learn
  floor, delete threshold).
- **C — the decision log:** a `derpies_decisions` row per judged
  message/edit (content, score, threshold, outcome) + a
  `read_derpies_decisions` MCP tool — the input to the improve-over-time
  loop.
- **B — phrases:** a `derpies_gimmick_phrases` table + a fast sequence match
  + prompt inclusion — the optimizer that converts the score's one-ask
  catches of known text phrasings into zero-ask fast hits.

## Schema — migration `000005_derpies_score.up.sql`

Three new objects **plus a `derpies_prompt` UPDATE**. The prompt flip moves
into this migration so the prompt, the config/decisions/phrases DDL, and the
code deploy are **one atomic step** (the deploy runs migrations then restarts
— see the Rollout section). The `derpies_gimmicks` table is unchanged in
shape. The `derpies_prompt` table is unchanged in shape; its *content* changes
in two places: the `000003` seed (for fresh DBs + the sync guard) and the
live row (via this migration's UPDATE).

### `derpies_config` (single row — the operator's live dials)

```sql
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
```

Single row (`id = 1`), mirroring `derpies_prompt`. `delete_threshold` is T
(default **50**). Fetched per-message like the prompt and the list; a fetch
error or a missing row → the code default (50) — the flow never acts on a
half-loaded config. Live-tunable via SQL/MCP with **no deploy**. The CHECK
bounds T to **41–100** (T must exceed the learn floor of 40 for the matrix
bands to be non-contradictory — a `T ≤ 40` value is rejected at write time),
and `configThreshold` clamps an out-of-range read to the default + a
`slog.Warn` (belt-and-suspenders).

### `derpies_decisions` (append-only — one row per judged message/edit)

```sql
CREATE TABLE public.derpies_decisions (
    id bigserial PRIMARY KEY,
    message_id text NOT NULL,
    channel_id text NOT NULL,
    author_id text NOT NULL,
    content text NOT NULL DEFAULT '',
    path text NOT NULL CHECK (path IN ('fast', 'slow')),
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
```

- `score` / `threshold` are NULL for `fast`-path rows, **and on every arm
  that never reached the matrix** (`list fetch failed`, `pi unavailable`,
  `ask failed`, `unrecognized verdict`) — a literal `score=0` is not written
  for a verdict that never parsed.
- `reject_reason` ∈ {`list fetch failed`, `pi unavailable`, `ask failed`,
  `unrecognized verdict`, `no valid word`, `verdict word not in message`,
  `all-digit word`, `invalid word`, `delete failed`, NULL}.
- `message_id` is **not** unique — an edit of the same message appends a new
  row (append-only history). `id` is the row identity.
- Retention: v1 keeps everything (no prune). ~7k rows/yr is trivial.

### `derpies_gimmick_phrases` (manual-only — the optimizer)

```sql
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
```

**Phrase contract:** 2–8 tokens, each `^[a-z0-9]{1,32}$` (ASCII for v1),
joined by single spaces. The per-token minimum is **1** (not the word
contract's 2) so real phrases like `buy me a bike` (whose `a` is 1 char) are
storable — a single 1-char token is never a valid *word* (min 2), but it is
fine as part of a multi-token phrase; the message side (`foldedTokenSequence`)
already yields 1-char tokens, so only the stored-phrase validation relaxes.
Max = 8×32+7 = 263 → varchar(300). `source` is **manual only** (no LLM learn
of phrases — the verdict is a single word, so there is no phrase to learn).

## Code changes

All in `internal/handlers/derpies` unless noted; the MCP tool work is in
`internal/mcp` + `cmd/tugbot` (section 4). The `wordmatch` package is
**unchanged** (the `WordValid` stored-space contract stays
`^[a-z0-9]{2,32}$`).

### 1. The score verdict (ADR 0010)

- **`parseVerdict` → `parseVerdictScore`** (derpies.go). Scans **all** lines
  (not just the first non-empty): each line is `TrimSpace`d first (the prompt
  displays the reply format **indented** — `␣␣SCORE:<0-100>` — and an LLM
  echoing the indentation must still parse), then the first line matching
  `^SCORE:\s*(\d+)$` (case-insensitive) whose value is 0–100 is the score (the
  `\s*` absorbs a space after the colon — `Score: 95` is a very common LLM
  reply shape); the first line matching `^WORD:\s*(.+)$` (case-insensitive)
  is the word — the **remainder of the line** (trimmed, lowercased), so a
  spaced SPLIT answer
  (`WORD:z w i f t`) is captured and handed to the unchanged
  `foldVerdictWord` whitespace-collapse in the gate (ADR 0009's belt-and-
  suspenders is preserved: both the spaced and the collapsed answer form
  learn the collapsed word). Returns `(score int, hasScore bool, word
  string)`. No valid SCORE line → `hasScore=false` → the flow logs
  `derpies unrecognized verdict — doing nothing` and returns (the existing
  degradation arm).
- **The decision matrix** (replaces the current `switch kind` block, step
  8–10). Constants: `learnFloor = 40` (code constant), `defaultThreshold =
  50` (code default for the DB dial).
  ```
  score < 40:            do nothing (no delete, no learn)
  40 ≤ score < T:        learn the word (if valid+anchored); no delete
  score ≥ T:             learn the word (if valid+anchored) + delete
  ```
  - `T` is read via a new `store.configThreshold(ctx) (int, error)` seam
    (`SELECT delete_threshold FROM derpies_config LIMIT 1`); a fetch error
    or missing row → `defaultThreshold` (50) + a `slog.Warn` (mirrors the
    prompt fallback). A read outside the 41–100 bounds (a pre-CHECK row) is
    clamped to `defaultThreshold` + a `slog.Warn` — the matrix is defined
    for `T > learnFloor` only; `T ≤ 40` is unrepresentable (the CHECK
    constraint rejects it at write time).
  - **Learn** = the existing step-9 gate (two-arm + all-digit precondition)
    on the folded verdict word, **gated on `score ≥ 40`** (below the floor,
    no learn). The gate is unchanged in shape; only the score precondition
    is added.
  - **Delete** = `score ≥ T` (independent of whether a word was learned — a
    high-score combo with no word still deletes).
- **The anchor gate (the emoji fix, A):** the anchor requirement
  (`hasTextTokens && !toks[fw]`) becomes **`hasWordLikeTokens &&
  !toks[fw]`**. New helper `wordLike(tok string) bool`:
  ```go
  func wordLike(tok string) bool {
      if tok == "" || allDigitVerdict(tok) {
          return false
      }
      return wordmatch.WordValid(tok) || unicodeVerdictShape(tok)
  }
  ```
  `hasWordLikeTokens` is computed from **`tokensForMatch(m.Content)` only**
  (the posted message — matching the prompt's "the message has no other text
  words" scope). The **anchor check** (`!toks[fw]`) still uses the **union**
  `toks` (posted + referenced + embed titles — the word may legitimately
  anchor to the quoted content, which the LLM judges). So: an emoji-only
  *post* (even when it *replies* to a texted message, or carries a link
  with a word-like embed title — the ADR 0007 Klipy vector) has no word-like
  tokens in `m.Content` → the anchor requirement is skipped → a semantic
  `WORD` passes; a post WITH word-like text of its own requires the word to
  be a token of the union. A mention snowflake (all-digit) / emoji-ref
  (colon) / pure-emoji token (folds to `""`) is **not** word-like, so a
  "mention + emoji" message is judged like an image-only post: a valid word
  passes, the score decides. The non-ASCII arm (b) text-anchor requirement
  is unchanged.

### 2. The fast-path phrase match (B)

- **New `foldedTokenSequence(content string) []string`** (derpies.go) — the
  ordered, edge-trimmed, folded token sequence: `strings.Fields`, each token
  `FoldToASCII` + `TrimFunc(edgePunct)`, **preserving order and duplicates**,
  dropping tokens that trim to `""`. (No dedup, no collapsed-run handling —
  that stays in `tokensForMatch`, which is unchanged.)
- **New `store.listPhrases(ctx) ([]string, error)`** seam
  (`SELECT phrase FROM derpies_gimmick_phrases ORDER BY phrase` — sorted,
  so the `{known}` phrases sub-block is byte-stable and prompt tests can
  assert on it, mirroring `sortedKeys` for the words). A DB error degrades
  the flow (log + skip the phrase match, continue to the slow path) — never
  act on a half-loaded phrase list.
- **The phrase match** (flow step 4.25, after the word fast path, before
  images): for each stored phrase, split on single spaces into its tokens;
  slide a window of that length over `foldedTokenSequence` of the **posted
  content and the referenced content SEPARATELY** (never concatenated — a
  phrase spanning the message/reply boundary must not match), mirroring how
  the word fast path unions the two. **Embed titles are NOT scanned for
  phrases** in v1 (a phrase living in an embed title is a follow-up; the
  word fast path's embed-title union is unchanged). An exact consecutive
  match → fast hit: delete (zero asks) + a `derpies_decisions` row
  (`path=fast`, `word=<phrase>`, `deleted=true`). The same delete-failed /
  success log shape as the word fast path.

### 3. The decision log (C)

- **New `store.recordDecision(ctx, d *decisionRecord) error`** seam
  (`INSERT INTO derpies_decisions …`). Best-effort: a write failure logs
  (`slog.Error`, module derpies) and does **not** abort — the delete/learn
  already happened.
- **One `defer`** placed right after the author-ID gate (step 3) captures
  every terminal arm: `defer h.recordDecision(ctx, m, dec)` where `dec` is a
  local `*decisionRecord` the flow populates as it progresses. Pre-gate arms
  (feature off, not a guild, author not gated) log **no** row. Terminal
  arms:
  - list fetch failed → `reject_reason=list fetch failed`
  - fast word/phrase hit → `path=fast`, `word`, `deleted` (or
    `reject_reason=delete failed`)
  - pi unavailable → `reject_reason=pi unavailable`
  - ask failed → `reject_reason=ask failed`
  - unrecognized verdict → `reject_reason=unrecognized verdict`
  - score < 40 → `score`, `threshold`, no delete/learn
  - 40 ≤ score < T → `score`, `threshold`, `learned` (if a valid word), no
    delete
  - score ≥ T → `score`, `threshold`, `learned` (if a valid word), `deleted`
  - score ≥ T, delete FAILED → `score`, `threshold`, `learned` (if a valid
    word), `deleted=false`, `reject_reason=delete failed` (the slow-path
    variant of the fast-path delete-failure arm — the word stays learned,
    the delete is logged as failed)
  - word rejected → the specific `reject_reason` (`no valid word` /
    `verdict word not in message` / `all-digit word` / `invalid word`);
    `deleted` still reflects `score ≥ T`
- **Scope boundary:** the **nickname flow** (nicknames.go) is NOT logged to
  `derpies_decisions` in v1 (a nickname is not a message) — its decisions
  stay in the slog log. It DOES use the score model (section 6).

### 6. The nickname flow (required — shares the verdict parse + prompt)

`nicknames.go` calls `parseVerdict` (which section 1 **replaces**) and shares
`defaultPromptTemplate`, the live `derpies_prompt` row, and `gimmickPrompt`.
Leaving it untouched would both break compilation and silently kill the
nickname-reset filter (once the live prompt is SCORE-format, every nickname
verdict parses as "unrecognized" → no resets ever). So the nickname flow is
updated in lockstep:

- **Parse:** `nickFlow` calls `parseVerdictScore` (not the removed
  `parseVerdict`).
- **T:** the same `store.configThreshold` seam + fallback (50).
- **The nickname matrix** (mirrors the message matrix; the action is the
  nick reset, not a delete):
  ```
  score < 40:            do nothing (no reset, no learn)
  40 ≤ score < T:        learn the word (if valid+anchored to the nick); no reset
  score ≥ T:             learn the word (if valid+anchored) + reset the nick
  ```
  - **Fast path** (word-only, unchanged): a known **word** in the nick →
    reset, zero asks. Phrase matching is **not** added to the nickname flow
    in v1 — a nickname is a short display name and the word fast path covers
    it (a phrase-in-nick is a follow-up). `nickFlow` passes `[]string{}`
    (empty phrases) to the new `gimmickPrompt` signature, so the nickname
    prompt's `{known}` block carries words only.
  - **Learn gate:** the existing ADR 0008 arm (`wordValid` +
    folded-token-in-nick), gated on `score ≥ 40`. The `wordLike` anchor fix
    (section 1) applies: a nick with **no word-like tokens** (an all-emoji
    nick) skips the anchor requirement → a valid word passes, the score
    decides.
  - **Reset** = `score ≥ T` (independent of whether a word was learned).
  - **Cache write (load-bearing — mirrors today's discipline):** the
    `lastNick` change-detection cache is written on every **judged** terminal
    arm, so a later `GUILD_MEMBER_UPDATE` for the same nick (a role change,
    a mute, etc.) is skipped by `cur == evt.Nick` and never re-judged. The
    `score < 40` and `40 ≤ score < T` arms call `saveNick(key, evt.Nick)`
    (the clean/unknown analog); `score ≥ T` goes through `resetNow`, which
    already caches both the success and the failure outcome. The **pre-matrix
    failure arms do NOT cache** (`list fetch failed`, `pi unavailable`,
    `ask failed`) — a transient failure must stay retryable, so the nick is
    re-judged on the next event. (The 60s `lastEdit` cooldown is separate —
    it is marked only by `resetNow`.)
- **Coalescing** (unchanged): at most one reset attempt per 60-second window
  (success or failure marks the window); the bot's own reset is never
  re-judged (the `cur == evt.Nick` echo skip).
- **Tests:** the existing `nicknames_test.go` fake-pi seam — add the matrix
  cases (score < 40 → no reset; 40 ≤ score < T → learn, no reset; score ≥ T
  → reset; an all-emoji nick + a semantic `WORD` → reset; an all-emoji nick
  + a `WORD` not anchored and score < 40 → no reset) **plus the cache case:
  two `GUILD_MEMBER_UPDATE` events with the same nick, the first scoring
  < 40 → the second (a role-only change) must produce zero pi asks** (the
  `saveNick` on the no-reset arm is what makes the second event skip).

### 4. The MCP tool (C)

`internal/mcp` today has exactly ONE seam (`DiscordAPI`, wrapping
discordgo session methods) and **no** DB/pool access — so this tool is new
architectural surface, specified concretely:

- **New seam interface in `internal/mcp`** (mcp.go):
  ```go
  type DecisionSource interface {
      ReadDecisions(ctx context.Context, f DecisionFilter) ([]DecisionRow, error)
  }
  ```
  `DecisionFilter` and `DecisionRow` are defined in `internal/mcp` (the
  tool's I/O contract). `DecisionFilter`: `AuthorID`, `ChannelID`, `Path`,
  `Deleted *bool`, `ScoreMin *int`, `ScoreMax *int`, `Since`, `Until
  *time.Time`, `Limit` (default 50, max 500 — clamped, not an error).
  `DecisionRow` mirrors the `derpies_decisions` columns. (The handler's
  `ReadDecisions` method — below — takes these `internal/mcp` types, so
  `internal/handlers/derpies` imports `internal/mcp` for the types; there is
  no cycle — `internal/mcp` does not import the handler.)
- **`NewServer` signature change**: `NewServer(d DiscordAPI, decisions
  DecisionSource, port int) *Server`. Existing callers (`cmd/tugbot`, the
  `mcp_test.go` fakes) are updated to pass the decision source — tests pass
  a fake `DecisionSource` (or a nil-tolerant no-op where the tool is not
  under test).
- **`Derpies.ReadDecisions(ctx context.Context, f mcp.DecisionFilter)
  ([]mcp.DecisionRow, error)`** — the handler's public surface (the
  `Invoke`-style seam the Feature-tools glossary describes), built on the
  `recordDecision` store seam's inverse: a parameterized `SELECT` with the
  filter clauses composed (all optional, AND-combined), `ORDER BY
  created_at DESC`, `LIMIT min(limit, 500)`. A DB error propagates (the MCP
  tool surfaces it as a tool error — a read tool failing is not a silent
  degradation).
- **Production wiring (`cmd/tugbot`)**: the `*Derpies` handler (which
  implements `ReadDecisions`) is passed to `NewServer` as the
  `DecisionSource`.
- **New MCP tool `read_derpies_decisions`** (registered in `tools_read.go`
  — or a new `tools_decisions.go`; `tools.go` is the shared-helpers file,
  not where tools register): parses the tool arguments into a
  `DecisionFilter`, calls `s.decisions.ReadDecisions`, returns the rows as
  JSON. The `registerReadTools(srv, d)` signature gains the `DecisionSource`
  (or a new `registerDecisionsTools`). Note: ADR 0004's LAN-trust posture
  applies — the tool is read-only and the embedded server is LAN-bound.
  **Call-site notes:** `cmd/tugbot` has **two** `NewServer` call sites —
  the production one (pass the `*Derpies` handler) and the selftest one
  (currently `_ = newHandlers(a)`, which discards the handlers — it must
  keep the handler, or pass a no-op `DecisionSource`, to compile); and the
  production construction's tool-list `slog` line (which hardcodes
  `"… read_messages, post_message, react"`) gains the new tool name.
- **Test fake**: a fake `DecisionSource` in `mcp_test.go` (mirrors the
  existing `DiscordAPI` fake pattern).

### 5. The store interface

The `store` interface (derpies.go) gains four methods: `configThreshold`,
`listPhrases`, `recordDecision` (the write), and `queryDecisions` (the
parameterized `SELECT` behind `ReadDecisions` — the filter clauses composed
from the `mcp.DecisionFilter`, `ORDER BY created_at DESC`, `LIMIT`). The
`poolStore` implements them; the test fakes implement them (mirroring the
existing seam pattern).

## The prompt (code default + migration seed + live row, in sync)

The `defaultPromptTemplate` constant (derpies.go) is replaced by the text
below. The `000003` migration seed is updated to match **byte-for-byte**
(modulo the `''` doubling) — `TestMigration000003AppliesAndSeeds` is the
forever sync guard. The live `derpies_prompt` row is `UPDATE`d to the same
text (the flow's actual source; the constant is the fallback and **must** be
the new format so a missing/invalid row never degrades to the old binary
prompt).

The mandatory `{content}` and `{known}` markers are preserved; the optional
`{{IMAGES}}`, `{{REF}}`, `{{EMBED}}`, `{{GIFS}}` markers are preserved. The
`{known}` block assembly (gimmickPrompt) gains a second sub-block — the
phrases — appended after the words:

```
-----< known gimmick words (sorted ascending) >-----
<words>
-----< known gimmick phrases (exact multi-word patterns) >-----
<phrases>
```

(Only the phrases sub-block is emitted when phrases exist; the words
sub-block is unchanged. The template's `{known}` marker is unchanged — the
code assembles the combined block. The words list is always seeded and
non-empty in practice, so the "words empty but phrases present" dangling-
header case is a documented non-case.)

The new `defaultPromptTemplate`:

```
A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of short, repetitive, annoying gimmicks — and of evading, over and over, the word filters built to catch them. He is notorious for this.

HE WILL TEST THIS FILTER. Every message you judge from him is a probe: he actively measures what gets through, and the respellings in his posts are his evasions, not typos to forgive. Your stance is adversarial, not polite: when a message carries ANY trace of the roster — respelled, bent, squeezed, split, quoted, or dressed up as a question — judge it a GIMMICK. Judge it innocent only when there is NO trace of the roster at all AND a plainly innocent reading is obvious. For this user a false negative (a gimmick getting through) is the worse error. When you are torn between two bands: score toward the HIGHER side. His messages are the filter's only queue, so err toward catching the roster, never toward letting it through.

{content}
{{EMBED}}
{{GIFS}}

Techniques he uses — in any combination; judge on ALL of them at once:
- RESPPELLING: letters swapped/added/dropped/reordered, or bent — including unicode lookalikes (a z or s with a diacritic, ß, ø, ς, and the like), all-caps, or letters spelled out. Examples: zwift, schwift, žwift, s1ft. A bent letter does NOT change the word: "žwift" IS the swift-thing.
- NON-ENGLISH LETTERS: a known word written in Cyrillic, Greek, or any other lookalike script (з = z, и = i, о = o, ο = o, ς = s, and the like) IS that known word. Judge by what it spells, not by which script it is wearing.
- WEIRD SPELLINGS OF EVERY KIND: any spelling of a known word that a reasonable reader can still see through — letter transpositions, doubled letters, "wrong" but recognizable spellings. If it is recognizably the known word, judge it.
- PUNCTUATION / DASHES EVERYWHERE: punctuation, dashes, dots, slashes, brackets, or symbols wedged INTO a known word (sw-ift, s.w.i.f.t, s/w/i/f/t, s(w)i(f)t), or between its letters — punctuation does not break the word.
- SPLIT: a known word spread over spaces or symbols between its letters (g i v e, s w i f t with dots/dashes between the letters).
- HIDDEN IN OTHER WORDS: a known word buried inside a longer word it is not a token of (a "swift"-like string stitched into another word, a known word straddling a word boundary, or known words jammed together into one token) — it still counts; the anchor is the token containing it, AS IT APPEARS.
- SQUEEZED/CONCATENATED: a known word fused into or onto another word without the space (a "swiftin…"-style blend), one or more known words jammed together, or extra letters sprinkled through a known word.
- ASK-PHRASING (the core of the roster): asking OTHER users to buy/give him something — a Zwift subscription, a free bicycle, a "gift" keyed to a known word — OR a fresh short repetitive solicitation in the same style (a FRESH gimmick in the roster style counts).
- QUOTING/REFERENCING: replying to or quoting one of his own earlier messages so the gimmick lives in the quote (quoted text counts as part of the message).
- IMAGES: the gimmick inside an attached/quoted screenshot or pasted image (images arrive with the message for you to read; a word visible in an image counts as if it were written).
- EMOJI-ENCODING: a sequence of emojis whose COMBINED meaning is a roster solicitation (💸 + 🚵 = "buy me a bike" = zwift; 💰 + a face + 🚲 = "buy me a bike"). Judge the COMBINATION, not the individual emojis — a single emoji (money, a bike, a face) is harmless alone; the combo is the gimmick. A combo that plainly means a roster solicitation scores 90-100.

{{IMAGES}}
{{REF}}

His gimmicks are short, repetitive solicitations he posts over and over. Example from the roster: trying to get other users to buy HIM a Zwift subscription, or to give him a free bicycle. The roster rotates — old gimmicks come back — so the known-word list below spans EVERY past gimmick, not just the current one.

Scoring scale (score the WHOLE message, all techniques at once):
- 90-100: an unambiguous roster solicitation — a known word as-is (any script), or a combo (emoji/image/text) that plainly means one.
- 60-89: a clear trace — a recognizable respelling / squeeze / split / foreign-script rendering of a known word, or a solicitation phrasing in the roster style.
- 40-59: a possible trace — a bent letter, a partial pattern, a combo that could go either way.
- 0-39: no meaningful trace — a plainly innocent reading.

Known gimmick words (each was the anchor word of a past gimmick; respellings of them are how he dodges the fast filter):
{known}

Judgement rules (these override politeness):
- A known word or any respelling of one — even when the surrounding text looks mildly innocent — is a GIMMICK (score it 60-100 by the scale).
- A known word hidden inside another word, written in non-English letters, or shot full of punctuation and dashes is a GIMMICK — dressing does not launder the word.
- A KNOWN GIMMICK IN ANY LANGUAGE IS STILL A GIMMICK: he now posts the same roster in OTHER LANGUAGES (observed: Arabic دراجة زويفت / زويفت, Mandarin 骑行/飞快/长城, Persian دوچرخه). The roster is the MEANING — a message that asks someone to buy/give him a bicycle, a Zwift subscription, or riding gear, in any script, language, or wording, is a GIMMICK. Translate the message in your head and judge what it MEANS, never let the script launder it.
- An anchor word embedded inside a squeeze/blend is a GIMMICK; the anchor word is the most distinctive token of the blend AS IT APPEARS.
- If you have to imagine an innocent reading to score it 0-39, you are probably wrong — he is very good at making solicitations look like questions.
- When you are torn between two bands: score toward the HIGHER side.

Reply with one or two lines — the SCORE line always, the WORD line only when
the message carries a real trace (score >= 40):
  SCORE:<0-100>
  WORD:<anchor>
where <word> is the anchor word: the as-appears respelled token for a known-gimmick trace, or the single most distinctive word of the fresh gimmick. The rules for <word>:
- It MUST be a token of the message text AS IT APPEARS (case and edge punctuation aside; ignore unicode bent — you SHOULD judge "žwift" to be "zwift") — EXCEPT when the gimmick lives ONLY in the emojis (the message has no other text words): then answer the most distinctive word of what the emojis MEAN (e.g. "zwift" for 💸🚵).
- When the anchor is in a NON-LATIN script, answer the message's OWN foreign-script token as it appears (e.g. زويفت, دراجة, 骑行, دوچرخه) — NEVER the English-known-word translation unless that English word literally appears in the message. "zwift" for a message containing only زويفت is the INVALID answer; "زويفت" is correct.
- For a respelling, answer the respelled token AS IT APPEARS. NEVER answer the base/known word unless that base token itself appears in the message text — for "zwift" the answer is "zwift"; "swift" for it is the INVALID answer. Never answer a known word that is not in the message. The same rule holds across scripts: a foreign-script rendering of a known word is answered by its OWN script token, never by the English base.
- For a SPLIT word (letters spread over spaces or symbols between its letters), answer the COLLAPSED form — the letters joined without the spacing: "z w i f t" -> "zwift", "g i v e" -> "give". Never the spaced form; the spaced form is not a valid answer.
- When the anchor word lives ONLY in an image, answer the most distinctive word of that image as if it were in the message.
- When the anchor word lives ONLY in the emojis (the message has no other text words), answer the most distinctive word of what the emojis MEAN (e.g. "zwift" for 💸🚵) — not a token of the message.
- Score 0-39 only when the message carries NO trace of the roster at all and the innocent reading is obvious.
```

## Test plan

Mirror the existing `derpies_test.go` / `derpies_integration_test.go` /
`derpies_gif_test.go` seams (the test fakes implement the new `store`
methods).

- **`parseVerdictScore`** (unit): `SCORE:95` + `WORD:zwift` → (95, true,
  "zwift"); `SCORE:95` alone → (95, true, ""); `CLEAN` / `GIMMICK:zwift`
  (old format) → (0, false, "") → "unrecognized"; `SCORE:150` (out of
  range) → (0, false, ""); `SCORE:55\nWORD:زويفت` → (55, true, "زويفت");
  **indented** `  SCORE:95\n  WORD:zwift` (the prompt's reply format is
  indented — `TrimSpace` per line) → (95, true, "zwift"); **space after the
  colon** `Score: 95` (a very common LLM shape — the `\s*` absorbs it) →
  (95, true, ""); **prose around
  the lines** (`Sure!\nSCORE:80\nWORD:swift\nHope that helps`) → (80, true,
  "swift") (the scan-all-lines behavior); **spaced SPLIT word**
  `SCORE:70\nWORD:z w i f t` → (70, true, "z w i f t") — the remainder is
  captured and the gate's `foldVerdictWord` collapses it to `zwift` (ADR
  0009 preserved).
- **`wordLike`** (unit): `zwift` → true; `272889785318768641` (all-digit) →
  false; `derpies:1021692390177775657` (colon) → false; `""` → false;
  `خفيف` (non-ASCII shape) → true; `a` (1 rune) → false.
- **The decision matrix** (unit, fake store + fake pi): score < 40 → no
  delete/learn; 40 ≤ score < T → learn, no delete; score ≥ T → learn +
  delete; score ≥ T with no word → delete, no learn; word rejected (all-digit
  / invalid / not anchored) → the specific `reject_reason`, delete still
  reflects `score ≥ T`; **`T ≤ 40` is unrepresentable** (the CHECK rejects
  it at write time; a pre-CHECK out-of-range read is clamped to the default
  + a `slog.Warn`).
- **The anchor gate (A)** (integration, the observed case): a "mention +
  emoji" message (`<@…> 💸 <:derpies:…> 🚵`) with a `SCORE:95 WORD:zwift`
  verdict → `hasWordLikeTokens=false` → the word passes → delete + learn.
  A message WITH word-like text tokens + a `WORD` not in the message →
  rejected (`verdict word not in message`).
- **The phrase fast match** (integration): a stored `buy me a bike` + a post
  `who wants to buy me a bike` → fast delete, zero pi asks, a
  `derpies_decisions` row (`path=fast`, `word=buy me a bike`). A post with a
  SPLIT word inside the phrase (`buy me a b i k e`) → NOT fast-matched (the
  known limitation) → falls to the slow path.
- **The decision log** (integration): each terminal arm writes the expected
  row (content, path, score, threshold, word, learned, deleted,
  reject_reason); a `recordDecision` insert failure logs and does not abort
  the delete.
- **`ReadDecisions`** (unit, fake store): the filter clauses (author, path,
  deleted, score range, since/until, limit) compose correctly; ordered
  `created_at DESC`.
- **The prompt sync** (the forever guard): `TestMigration000003AppliesAndSeeds`
  passes with the new `defaultPromptTemplate` == the `000003` seed
  (byte-for-byte modulo `''`).
- **The config fallback** (unit): a `configThreshold` error / missing row →
  the flow uses `defaultThreshold` (50) + a `slog.Warn`.

## Rollout

The prompt flip lives **inside migration 000005** (not a manual pre-deploy
UPDATE): the deploy runs migrations then restarts, so the prompt, the
config/decisions/phrases DDL, and the new code all flip in **one atomic
step**. (A manual pre-deploy `UPDATE derpies_prompt` would create a
guaranteed slow-path-dead window: old code + new SCORE-format prompt →
every verdict "unrecognized" → zero slow-path deletes/learns until the
deploy completes — and silent if the deploy stalls. The reverse order is
equally dead: new code + old binary prompt → `hasScore=false`.)

1. **Migration** `000005_derpies_score.up.sql` — the three objects +
   `UPDATE derpies_prompt SET body = <new score prompt>, updated_at =
   now();`. Applied by the deploy (or `make migrate`).
2. **`000003` seed update** — the `derpies_prompt` seed in
   `migrations/000003_derpies_prompt.up.sql` is set to the new score prompt
   (byte-for-byte with the Go constant, modulo the `''` doubling) — for
   fresh DBs + the `TestMigration000003AppliesAndSeeds` sync guard. (000003
   is already applied in production; editing it does not re-run there — the
   production flip comes from 000005's UPDATE. On a fresh DB, 000003 seeds
   the score prompt and 000005's UPDATE re-sets it to the same text — a
   no-op.)
3. **Code** (this spec) — the derpies package + the `internal/mcp` tool +
   the `cmd/tugbot` wiring.
4. **Deploy**: `ssh root@tugbot update-tugbot` (pulls main, runs migrations
   — including 000005's prompt UPDATE — builds, restarts). Confirm the head
   commit matches the pushed commit and the service is `active (running)`.
   After the restart the prompt is SCORE-format and the code parses SCORE —
   atomic.
5. **Verify** (below).

Residual risk: a deploy that fails *after* the migration applies but *before
the restart* leaves the prompt SCORE-format while the old code still runs —
the slow path is dead until a re-deploy. Manual rollback: `UPDATE
derpies_prompt SET body = <previous binary prompt>, updated_at = now();`
(the previous body is in git history / the pre-deploy DB).

## Verification

- **The observed case:** a subsequent `<@…> 💸 🚵`-style post by the gated
  user is deleted by the slow path (a `derpies delete (llm)` log line with a
  score) — or, once a phrase is curated, by the fast path.
- **The decision log:** `SELECT message_id, path, score, threshold, word,
  learned, deleted, reject_reason FROM derpies_decisions ORDER BY created_at
  DESC LIMIT 10;` returns rows; the `read_derpies_decisions` MCP tool returns
  the same.
- **The selftest:** `go run ./cmd/tugbot --selftest` logs "Discord session
  and all thirteen handlers and the MCP server constructed", exit 0.
- **The full gate** (per AGENTS.md): `go build ./... && go vet ./... &&
  gofmt -l . && make lint && go test ./...`, then the DB-touching gate
  (`make db-up` + `TUGBOT_TEST_DATABASE_URL=… go test -p 1 -count=1 ./...`).

## Out of scope (follow-ups)

- **Non-ASCII phrase tokens** (v1 phrases are ASCII-only).
- **`/gimmick` phrase subcommands** (v1 curation is SQL-only).
- **Logging the nickname flow** to `derpies_decisions` (v1: message flow
  only).
- **A `derpies_decisions` prune/retention tool** (v1: keep everything).
- **A SPLIT word inside a phrase** (v1: exact-consecutive match only).
