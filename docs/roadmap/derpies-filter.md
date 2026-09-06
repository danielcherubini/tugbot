---
status: committed
done-when: With `TUGBOT_DERPIES_USER_IDS=163055057254875136` in .env and the `derpies` feature flag enabled in prod: (1) a message by that user containing a seeded gimmick word (e.g. `sw1ft`) is deleted within seconds with NO bot message, reaction, or gulag involvement; (2) a message by him containing a NEW respelling (e.g. `zswiftf`) that no list entry matches is sent to the pi RPC, and on a `GIMMICK:<word>` verdict the word is persisted to `derpies_gimmicks` with `source='llm'` and the message is deleted; (3) any later post containing `zswiftf` is deleted by the fast path with zero pi RPC asks.
---

# Derpies Filter Plan

**Goal:** Silently delete Derpie's gimmick posts, learning his new respellings via the pi RPC.
**Architecture:** A new passive handler (`internal/handlers/derpies`) gated by an author-ID list. Fast path: exact token match against a Postgres-backed gimmick word list. On a miss, one pi RPC verdict; a valid `GIMMICK:<word>` verdict upserts the word into the table and deletes the message. The flow's only outgoing REST call is `DeleteMessage` — no bot response, no reaction, no gulag involvement on any path.
**Tech Stack:** Go, bwmarrin/discordgo, jackc/pgx (pool + raw SQL), the existing pi RPC subprocess, this repo's in-tree `dbmigrate` runner.

Conventions this plan follows (read these files first if context is missing):
- Handler shape / seams: `internal/handlers/mention/mention.go` + `mention_test.go` (unexported fields `store`/`ops`, in-package tests construct the struct directly with fakes; a `fakePi` implements `app.PiBackend`).
- Feature flag: `internal/features` (`IsEnabled` = the silent flavor used in message flows).
- Checked ID conversion at DB boundaries: `core.DiscordID(name, raw)` in `internal/handlers/gulag/gulag_core.go`.
- Migration style: `migrations/000001_baseline.up.sql` (`character varying`, `DEFAULT x NOT NULL` column order, `public.` qualification, `ON CONFLICT DO NOTHING` seed inserts into `features`).
- Integration-test skip pattern (PG required): `internal/dbmigrate/migrate_test.go` (`testing.Short()` skip; `TUGBOT_TEST_DATABASE_URL` override; `t.Skipf` when the pool can't be created/pinged).

**Do NOT change:** the Rust repo, the gulag package, any existing handler, command registration in `main.go` (`registerCommands`), or `internal/db` (no sqlc regeneration — this feature uses raw SQL seams, the committed-handler pattern).

---

### Task 1: Migration 000002 (gimmick table + seed + flag row) and `TUGBOT_DERPIES_USER_IDS` config

**Context:** The filter needs (a) a durable, append-only gimmick word list that survives restarts and picks up LLM-learned words — Postgres is this bot's only persistent state; (b) the target author IDs — the house pattern for user-ID gating is a comma-separated env var parsed to `map[int64]struct{}` (`SLOW_USER_IDS` in `internal/config/config.go`). This task creates both, independently committable: the migration file and the config field are consumed by Task 2 but neither needs the other to build.

**Files:**
- Create: `migrations/000002_derpies_gimmicks.up.sql`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Test: `internal/config/config_test.go` (new test function)

**What to implement:**

1. `migrations/000002_derpies_gimmicks.up.sql` — EXACT content, EXACT statement order, and NOTHING else (no trailing statements):

```sql
-- 000002_derpies_gimmicks — the derpies filter's gimmick word list + flag row.
--
-- derpies_gimmicks: words that signal Derpie's recurring respelling scheme.
-- `source` distinguishes 'seed' (this file) from 'llm' (learnt at runtime
-- by the handler's pi RPC verdict). The UNIQUE word constraint makes the
-- runtime insert an idempotent upsert.
--
-- The `features` insert is ON CONFLICT DO NOTHING: the live production DB
-- already carries a 'derpies' flag row (the baseline migration's comment);
-- on a fresh DB this seeds the feature OFF.
CREATE TABLE public.derpies_gimmicks (
    id integer NOT NULL,
    word character varying(64) NOT NULL,
    source character varying(8) DEFAULT 'seed' NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
--
--
CREATE SEQUENCE public.derpies_gimmicks_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.derpies_gimmicks_id_seq OWNED BY public.derpies_gimmicks.id;
--
--
ALTER TABLE ONLY public.derpies_gimmicks ALTER COLUMN id SET DEFAULT nextval('public.derpies_gimmicks_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.derpies_gimmicks
    ADD CONSTRAINT derpies_gimmicks_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.derpies_gimmicks
    ADD CONSTRAINT derpies_gimmicks_word_key UNIQUE (word);
--
--
INSERT INTO public.derpies_gimmicks (word, source) VALUES
    ('swift', 'seed'),
    ('zswift', 'seed'),
    ('bike', 'seed'),
    ('give', 'seed'),
    ('buy', 'seed');
--
--
INSERT INTO public.features (name, enabled) VALUES ('derpies', false)
ON CONFLICT (name) DO NOTHING;
```

File contents = the header comment + exactly these eight DDL/DML statements, in exactly this order. (The per-table grouping is the correct layout for a single-table file: the baseline defers ALL `SET DEFAULT`s before ALL constraints because it has many tables; here sequence → OWNED BY → SET DEFAULT → pkey → unique is the same relative order.) Verify after writing: `grep -c "^CREATE\|^ALTER\|^INSERT" migrations/000002_derpies_gimmicks.up.sql` must print `8` and the word `placeholder` must NOT appear in the file.

2. `internal/config/config.go`:
   - Add to the `Config` struct, immediately after the `SlowUserIDs` field:

```go
	// DerpiesUserIDs are the Discord user IDs whose messages the derpies
	// filter watches (fast-path word match + pi RPC fallback). Kept as a
	// map like the other lists. Default: empty.
	DerpiesUserIDs map[int64]struct{}
```

   - In `LoadConfig`, immediately after `slow := parseIDList(os.Getenv("SLOW_USER_IDS"))` add:

```go
	// TUGBOT_DERPIES_USER_IDS — comma-separated Discord user IDs; same
	// parsing as SLOW_USER_IDS (malformed parts skipped).
	derpies := parseIDList(os.Getenv("TUGBOT_DERPIES_USER_IDS"))
```

   - In the returned `&Config{...}` literal, add `DerpiesUserIDs: derpies,` right after the `SlowUserIDs: slow,` entry.

   Do NOT change any existing field, parse function, or the godotenv behavior.

3. `internal/config/config_test.go`:
   - In `validEnv()`, add `"TUGBOT_DERPIES_USER_IDS": ""` to the returned map (keeps every existing test hermetic — the parser sees the var present-and-empty rather than never set).
   - Add the test (exact; table style matches the file):

```go
func TestLoadConfigDerpiesUserIDs(t *testing.T) {
	t.Run("comma list parses", func(t *testing.T) {
		vars := validEnv()
		vars["TUGBOT_DERPIES_USER_IDS"] = "163055057254875136,999"
		setEnv(t, vars)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig() error = %v, want nil", err)
		}
		if _, ok := cfg.DerpiesUserIDs[163055057254875136]; !ok {
			t.Error("DerpiesUserIDs missing 163055057254875136")
		}
		if _, ok := cfg.DerpiesUserIDs[999]; !ok {
			t.Error("DerpiesUserIDs missing 999")
		}
		if len(cfg.DerpiesUserIDs) != 2 {
			t.Errorf("DerpiesUserIDs len = %d, want 2", len(cfg.DerpiesUserIDs))
		}
	})

	t.Run("malformed parts skipped, default empty", func(t *testing.T) {
		vars := validEnv()
		vars["TUGBOT_DERPIES_USER_IDS"] = "163055057254875136,abc,"
		setEnv(t, vars)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig() error = %v, want nil", err)
		}
		if _, ok := cfg.DerpiesUserIDs[163055057254875136]; !ok {
			t.Error("DerpiesUserIDs missing the valid entry")
		}
		if len(cfg.DerpiesUserIDs) != 1 {
			t.Errorf("DerpiesUserIDs len = %d, want 1 (malformed parts must be skipped)", len(cfg.DerpiesUserIDs))
		}

		vars2 := validEnv()
		setEnv(t, vars2)
		cfg2, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig() error = %v, want nil", err)
		}
		if len(cfg2.DerpiesUserIDs) != 0 {
			t.Errorf("unset var: DerpiesUserIDs len = %d, want 0", len(cfg2.DerpiesUserIDs))
		}
	})
}
```

**Steps:**
- [ ] Write the failing test `TestLoadConfigDerpiesUserIDs` in `internal/config/config_test.go` (plus the `validEnv()` entry first)
- [ ] Run `go test ./internal/config/ -count=1 -run TestLoadConfigDerpiesUserIDs`
  - Did it fail with a compile error (`cfg.DerpiesUserIDs undefined`)? If it passed unexpectedly, stop and investigate why.
- [ ] Create `migrations/000002_derpies_gimmicks.up.sql` with the exact content above; verify with the two grep checks in the numbered list
- [ ] Implement the three `config.go` edits above
- [ ] Run `go test ./internal/config/ -count=1`
  - Did all tests pass? If not, fix the failures and re-run before continuing.
- [ ] Run `go build ./...`
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Run `go vet ./...`
- [ ] Commit with message: "config + migration 000002: TUGBOT_DERPIES_USER_IDS, derpies_gimmicks table, derpies flag row"

**Acceptance criteria:**
- [ ] `go test ./internal/config/ -count=1` green (all existing + new tests)
- [ ] `migrations/000002_derpies_gimmicks.up.sql` contains exactly: header, CREATE TABLE, sequence, OWNED BY, SET DEFAULT, pkey, word_key, the 5-word seed INSERT, the features INSERT — nothing else
- [ ] `Config.DerpiesUserIDs` exists, defaults to an empty map, and parses `TUGBOT_DERPIES_USER_IDS` with the same skip-malformed semantics as `SLOW_USER_IDS`
- [ ] `git status` shows only the three intended files changed

---

### Task 2: The derpies handler package — fast path, pi RPC verdict, learning

**Context:** The behavior itself, per the approved spec. Package `internal/handlers/derpies` follows the `mention` package exactly: a `Derpies` struct with unexported `store`/`ops` fields (production: raw-SQL pool store + `*discordgo.Session` ops; unit tests: in-package fakes), `New(*app.App)` for production, in-package tests that construct the struct directly with fake `store`, fake `ops`, and a `fakePi` implementing `app.PiBackend` (pattern: `mention_test.go`). The flow's gate order is fixed: feature flag → guild → author-ID; then fast path; then the pi path. The flow's only outgoing REST call is `DeleteMessage`. Every failure path is silent: slog line + return. No bot message is ever sent.

**Files:**
- Create: `internal/handlers/derpies/derpies.go`
- Create: `internal/handlers/derpies/derpies_test.go`
- Create: `internal/handlers/derpies/derpies_integration_test.go`

**What to implement:**

`internal/handlers/derpies/derpies.go` — exact API surface (imports: `context`, `log/slog`, `regexp`, `strings`, `github.com/bwmarrin/discordgo`, `github.com/jackc/pgx/v5/pgxpool`, plus the repo packages `internal/app`, `internal/features`, `core "internal/handlers/gulag"`):

```go
package derpies

// Pinned constants.
const (
	FeatureKey = "derpies"
	SourceSeed = "seed"
	SourceLLM  = "llm"

	module = "derpies" // slog module tag
)
```

Struct + constructor:

```go
type Derpies struct {
	app *app.App
	// New() wires the production store/ops. Tests (same package) assign
	// the fakes directly, mirroring mention_test.go.
	store store
	ops   discordOps
}

func New(a *app.App) *Derpies {
	return &Derpies{app: a, store: &poolStore{pool: a.Pool}, ops: &realOps{d: a.D}}
}
```

Seam interfaces (production: pool + session; tests: fakes):

```go
type store interface {
	// featureEnabled — the silent flavor (false on any error, including a
	// missing row; features.IsEnabled).
	featureEnabled(ctx context.Context, key string) bool
	// listGimmicks — every word, lowercased, as map keys. A DB error is
	// propagated so the flow degrades (log + skip), never acts on a
	// half-loaded list.
	listGimmicks(ctx context.Context) (map[string]bool, error)
	// addGimmick — idempotent upsert: INSERT ... ON CONFLICT (word)
	// DO NOTHING.
	addGimmick(ctx context.Context, word, source string) error
}

type discordOps interface {
	// deleteMessage — the flow's ONLY outgoing REST call.
	deleteMessage(channelID, messageID string) error
}
```

Production implementations:

```go
type poolStore struct{ pool *pgxpool.Pool }

func (p *poolStore) featureEnabled(ctx context.Context, key string) bool {
	return features.IsEnabled(ctx, p.pool, key)
}

func (p *poolStore) listGimmicks(ctx context.Context) (map[string]bool, error) {
	rows, err := p.pool.Query(ctx, `SELECT word FROM derpies_gimmicks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out[strings.ToLower(w)] = true
	}
	return out, rows.Err()
}

func (p *poolStore) addGimmick(ctx context.Context, word, source string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO derpies_gimmicks (word, source) VALUES ($1, $2)
		 ON CONFLICT (word) DO NOTHING`,
		word, source)
	return err
}

type realOps struct{ d *discordgo.Session }

func (o *realOps) deleteMessage(channelID, messageID string) error {
	return o.d.ChannelMessageDelete(channelID, messageID)
}
```

Pure helpers (unexported; tested in-package):

```go
// punctTrim is the edge-punctuation set trimmed from each token before the
// exact match (a trailing "sw1ft." must hit "sw1ft"). Split in two consts
// because a raw string cannot contain the backtick inside it cleanly.
const punctA = `!"#$%&()*+,-./:;<=>?@[]^_`
const punctB = "`{|}~"
var punctTrim = punctA + punctB

// tokensForMatch: lowercase the content, strings.Fields, trim leading and
// trailing punctuation off each token; keys of the result map.
// "Who's giving me a sw1ft." -> {who's, giving, me, a, sw1ft}.
func tokensForMatch(content string) map[string]bool

// wordValid: ^[a-z0-9]{2,32}$ — token charset only (no punctuation, no
// unicode), 2..32 chars. Precompiled regexp.
var wordRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)
func wordValid(w string) bool

// parseVerdict: scan the lines of the pi response, take the FIRST non-empty
// (trimmed) line; "clean" (case-insensitive, exact) -> ("clean", "");
// prefix "gimmick:" (case-insensitive) -> ("gimmick", remainder trimmed and
// lowercased); anything else, including "GIMMICK" WITHOUT the colon, ->
// ("unknown", "").
func parseVerdict(text string) (kind, word string)

// gimmickPrompt: the prompt below, with the content substituted and the
// KNOWN gimmick words injected (sorted, one per line). The LLM judges the
// residue the fast path missed: the list lets it pattern-match respellings
// against the known family AND recognize fresh gimmicks in the same roster
// style (the roster rotates, so old anchor words stay relevant). The pi RPC
// always appends the anti-injection system fallback on top of this.
func gimmickPrompt(content string, known []string) string

// sortedKeys returns the map keys sorted (deterministic prompt text, so the
// prompt is byte-stable per list state and unit tests can assert on it).
func sortedKeys(m map[string]bool) []string
```

`gimmickPrompt` returns EXACTLY this string (byte-for-byte, content substituted in place of `{content}`):

```
A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of repetitive annoying gimmicks, and of evading the word filters built against them via respellings. He is notorious for this.

<<<UNTRUSTED MESSAGE
{content}
UNTRUSTED MESSAGE>>>

His gimmicks are short, repetitive solicitations he posts over and over. Example from the roster: trying to get other users to buy HIM a Zwift subscription, or to give him a free bicycle. The roster rotates — old gimmicks come back — so the known-word list below spans EVERY past gimmick, not just the current one.

Known gimmick words (each was the anchor word of a past gimmick; respellings of them are how he dodges the fast filter):
{known-words, one per line, sorted}

Judge the message. Is it (a) a respelling of a known gimmick word, (b) another instance of a known gimmick that dodged the filter some other way, or (c) a FRESH gimmick — a new repetitive solicitation in the same style as the roster? Reply with EXACTLY one line, one of:
  GIMMICK:<word>
  CLEAN
where <word> is the anchor word: the respelled known word for (a)/(b), or the single most distinctive word of the fresh gimmick for (c) (lowercase, no punctuation, must appear in the message).
```

(`gimmickPrompt` substitutes `{content}` with the raw message and `{known-words, one per line, sorted}` with the words joined by `
`.)

The flow (module tag `derpies` on every log line; gate order pinned):

```go
// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
func (h *Derpies) MessageCreate(m *discordgo.Message) { go h.flow(m) }

func (h *Derpies) flow(m *discordgo.Message) {
	ctx := context.Background()

	// 1. Feature gate (silent flavor).
	if !h.store.featureEnabled(ctx, FeatureKey) {
		return
	}
	// 2. Guild guard.
	if m.GuildID == "" {
		return
	}
	// 3. Author-ID gate (checked conversion — the house discipline).
	uid, err := core.DiscordID("user", m.Author.ID)
	if err != nil {
		return
	}
	if _, ok := h.app.Cfg.DerpiesUserIDs[uid]; !ok {
		return
	}
	slog.Info("derpies message from filtered user", "module", module, "user", m.Author.ID, "guild", m.GuildID)

	// 4. Fast path: one indexed SELECT; exact token match.
	list, err := h.store.listGimmicks(ctx)
	if err != nil {
		slog.Error("derpies gimmick list fetch failed", "module", module, "error", err)
		return
	}
	toks := tokensForMatch(m.Content)
	for tok := range toks {
		if list[tok] {
			if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
				slog.Error("derpies delete (fast) failed", "module", module, "word", tok, "channel", m.ChannelID, "message", m.ID, "error", err)
			} else {
				slog.Info("derpies delete (fast)", "module", module, "word", tok, "channel", m.ChannelID, "message", m.ID)
			}
			return
		}
	}

	// 5. Slow path: pi unavailable -> silent return (the mention feature's
	//    degradation path, same shape).
	if h.app.Pi == nil {
		slog.Info("derpies pi RPC not available, skipping", "module", module)
		return
	}

	// 6-7. One ask (the 300s deadline lives in the pi package).
	text, err := h.app.Pi.Ask(ctx, gimmickPrompt(m.Content, sortedKeys(list)))
	if err != nil {
		slog.Error("derpies pi ask failed", "module", module, "error", err)
		return
	}

	// 8. Parse the verdict.
	kind, word := parseVerdict(text)
	switch kind {
	case "clean":
		slog.Info("derpies verdict clean", "module", module, "message", m.ID)
		return
	case "unknown":
		slog.Warn("derpies unrecognized verdict — doing nothing", "module", module, "verdict", strings.TrimSpace(text))
		return
	}

	// 9. SANITY before learning: charset/length AND the word must have
	//    appeared as a token in the submitted message (same tokenization as
	//    the fast path). A hallucinated word can never enter the list.
	if !wordValid(word) {
		slog.Warn("derpies invalid verdict word — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}
	if !toks[word] {
		slog.Warn("derpies verdict word not in the message — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}

	// 10. Learn, then delete. A delete failure is LOG ONLY — the word was
	//     actually used and stays learned (the next occurrence is a fast hit).
	if err := h.store.addGimmick(ctx, word, SourceLLM); err != nil {
		slog.Error("derpies add gimmick failed", "module", module, "word", word, "error", err)
	}
	if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
		slog.Error("derpies delete (llm) failed", "module", module, "word", word, "channel", m.ChannelID, "message", m.ID, "error", err)
	} else {
		slog.Info("derpies delete (llm) learned", "module", module, "word", word, "channel", m.ChannelID, "message", m.ID)
	}
}
```

`internal/handlers/derpies/derpies_test.go` — in-package; fakes mirror `mention_test.go`. Provide: a `fakeStore` with `enabled map[string]bool`, `listErr error`, `words map[string]bool`, `added []string` (each entry `word|source`), `listCalls int`; a `fakeOps` with `deleted [][]string` (each `{channelID, messageID}`) and `delErr error`; and a `fakePi` with fields `resp string`, `askErr error`, `asks int`, `prompts []string` that implements ALL THREE `app.PiBackend` methods (`Ask` records `asks` + `prompts` and returns `resp`/`askErr`; `AskWithImages` and `Stop` are no-ops — a fake with only `Ask` will NOT compile as `App.Pi`; the real `fakePi` in `mention_test.go` shows the same three-method shape). A `newTestDerpies(store *fakeStore, ops *fakeOps, pi app.PiBackend) *Derpies` helper builds `&Derpies{app: &app.App{Cfg: &config.Config{DerpiesUserIDs: map[int64]struct{}{163055057254875136: {}}}, Pi: pi}, store: store, ops: ops}` — `pi` is the INTERFACE type so a passed untyped `nil` yields a TRULY-nil `App.Pi` (a typed-nil `*fakePi` pointer would make the flow's `h.app.Pi == nil` guard false and panic `TestFlowNilPiSilentReturn`). A `derpMsg(content string)` helper returns `&discordgo.Message{ID: "msg1", GuildID: "g1", ChannelID: "c1", Author: &discordgo.User{ID: "163055057254875136"}}` and an `otherMsg()` variant with `Author.ID = "222"` for the not-filtered case. Tests to write (names exact):

- `TestTokensForMatch` — `"Who's giving me a sw1ft."` → keys `{who's, giving, me, a, sw1ft}`; mixed case `"SWIFT A"` → `{swift, a}`.
- `TestWordValid` — `sw1ft`→true; `zswiftf`→true; a 32-char token→true; a 33-char token→false; `a`→false (too short); `s-w1ft`→false (hyphen); `swïft`→false (unicode); `sw1ft!`→false.
- `TestParseVerdict` — `"GIMMICK:sw1ft"`→(`gimmick`,`sw1ft`); `"gimmick:SW1FT"`→(`gimmick`,`sw1ft`); `"clean"`→(`clean`,`"`"); leading blank line then `  CLEAN  `→(`clean`,`"`"); `"GIMMICK sw1ft"` (no colon)→(`unknown`); `"MAYBE"`→(`unknown`); `""`→(`unknown`); `"GIMMICK: zswiftf"` (space after the colon; the remainder is trimmed)→(`gimmick`,`zswiftf`); `"GIMMICK:"` (empty word)→(`gimmick`,`"`") — the validity gate then rejects it.
- `TestGimmickPrompt` — returned string contains `<<<UNTRUSTED MESSAGE`, the content, `UNTRUSTED MESSAGE>>>`, the protocol line `  GIMMICK:<word>` (byte check per the spec text above), AND the known-word block: with `known = []string{"sw1ft", "bike"}` the prompt contains both words in sorted order (`bike` before `sw1ft`), and `sortedKeys` of a 3-key map returns the sorted keys.
- `TestFlowFeatureFlagGate` — `enabled{"derpies": false}` → no deletes, `pi.asks == 0`, `store.listCalls == 0`.
- `TestFlowNoGuild` — `GuildID: ""` → no-op (assert `listCalls == 0`, no deletes).
- `TestFlowAuthorNotFiltered` — `otherMsg()` with the flag on → `listCalls == 0`, no deletes, `pi.asks == 0`.
- `TestFlowFastPathDeletesWithoutPi` — `words = {"sw1ft": true}`, content `"who's giving me a sw1ft."` → exactly one delete `["c1","msg1"]`, `pi.asks == 0`, `added` empty.
- `TestFastPathExactTokenBothWords` — `words = {"swift": true, "bike": true}`, content `"I'll sell you a bike that is swift."` → one fast delete, `pi.asks == 0`.
- `TestFastPathNearTokenFallsThrough` — `words = {"swift": true}`, content `"swiftly"` → NO fast delete; falls through to the pi path: `pi.asks == 1`; with `pi.resp = "CLEAN"` nothing is added or deleted.
- `TestFlowFastPathListErrorSkips` — `listErr` set → no delete, `pi.asks == 0`.
- `TestFlowNilPiSilentReturn` — no fast hit, `Pi == nil` → no delete, `added` empty.
- `TestFlowPiAskError` — `pi.askErr` set → no delete, `added` empty.
- `TestFlowVerdictClean` — `pi.resp = "CLEAN"`, no fast hit → no delete, `added` empty.
- `TestFlowVerdictUnknownGibberish` — `pi.resp = "I think maybe..."` → nothing.
- `TestFlowVerdictHallucinatedWordAbsentFromMessage` — `pi.resp = "GIMMICK:xyzzy"`, content `"completely clean text"` → `added` empty, no delete.
- `TestFlowVerdictInvalidWord` — `pi.resp = "GIMMICK:a"` with the word present is impossible, so use two subcases: `pi.resp = "GIMMICK:s-w1ft"` with content `"s-w1ft here"` (the charset gate rejects `s-w1ft`); and `pi.resp = "GIMMICK:sw1ft"` with content `"sw-1ft here"` (the token gate rejects: `sw1ft` is not a token of `"sw-1ft"`). In BOTH: `added` empty, no delete.
- `TestFlowVerdictLearnsAndDeletes` — `pi.resp = "GIMMICK:zswiftf"`, content `"holler at zswiftf now"` → `added == ["zswiftf|llm"]` and one delete `["c1","msg1"]`; also `pi.prompts[0]` equals `gimmickPrompt(content, sortedKeys(fakeStore.words))`.
- `TestFlowDeleteFailsAfterSuccessfulAdd` — `pi.resp = "GIMMICK:zswiftf"` (word in content), `ops.delErr` set → `added == ["zswiftf|llm"]` (the word stays added), no entry in `deleted`.

`internal/handlers/derpies/derpies_integration_test.go` — the 000002 apply-verification, skip SHAPE of `dbmigrate/migrate_test.go`'s `setupPool` (`testing.Short()` skip; `TUGBOT_TEST_DATABASE_URL` override; DEFAULT URL `postgres://postgres:postgres@127.0.0.1:5432/tugbot_test` — the COMPOSE credentials, aligned with `cmd/tugbot/main_test.go`'s default (modulo its `?timezone=UTC` session param; this test makes no timestamp assertions). NOTE `migrate_test.go`'s own default is `tugbot:tugbot`, so follow its skip SHAPE, not its default credentials; `t.Skipf` when the pool can't be created/pinged).

```go
// TestMigration000002AppliesAndSeeds — runs the REAL migration file (not
// an inline copy) through dbmigrate.Run against the test PG and asserts
// the seeded state.
```

Body, in order:
1. Read `../../../migrations/000002_derpies_gimmicks.up.sql` (relative to `internal/handlers/derpies/`) into a `t.TempDir()` under the SAME filename.
2. Precondition SQL (the shared test DB may already have any of these; never TRUNCATE `features` — other packages own it):
   `DROP TABLE IF EXISTS derpies_gimmicks CASCADE;`
   `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz DEFAULT now());`
   `CREATE TABLE IF NOT EXISTS features (id serial PRIMARY KEY, name character varying(255) UNIQUE NOT NULL, enabled boolean DEFAULT false NOT NULL);`
   `DELETE FROM schema_migrations WHERE version = '000002_derpies_gimmicks';`
   (the `DROP TABLE ... CASCADE` first is what makes the test rerunnable on the same test DB. The `features` TABLE must exist — with the UNIQUE (name) constraint — before the run: the migration's `ON CONFLICT (name)` requires the unique constraint, and `000001_baseline` is NOT in this test dir. Never touch `features` rows.)
3. `dbmigrate.Run(ctx, pool, dir)` must return nil.
4. Assert: row count of `derpies_gimmicks` == 5; the word set is exactly `{swift, zswift, bike, give, buy}`; every row has `source = 'seed'`; `pg_constraint` contains `derpies_gimmicks_word_key` (unique) and `derpies_gimmicks_pkey` (primary).
5. `t.Cleanup`: `DELETE FROM derpies_gimmicks;` and `DELETE FROM schema_migrations WHERE version = '000002_derpies_gimmicks';` (leave `features` intact — its `derpies` row is idempotent `ON CONFLICT DO NOTHING` state).

**Steps:**
- [ ] Write ALL the test functions above first (they will not compile until the handler exists — that IS the failing state)
- [ ] Run `go test ./internal/handlers/derpies/ -count=1`
  - Did it fail with a compile error (undefined `Derpies`, `New`)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement `derpies.go` per the exact API surface above
- [ ] Run `go test ./internal/handlers/derpies/ -count=1`
  - Did all tests pass? If not, fix the failures and re-run before continuing.
- [ ] Run `go vet ./...`
- [ ] Commit with message: "derpies handler: fast-path gimmick match, pi RPC verdict, runtime learning"

**Acceptance criteria:**
- [ ] `go test ./internal/handlers/derpies/ -count=1` green (unit tests green; the integration test green or cleanly skipped without PG)
- [ ] The flow never calls anything other than `store.*`, `ops.deleteMessage`, `Pi.Ask`, and slog
- [ ] A `GIMMICK:<word>` verdict adds the word ONLY if `wordValid(word)` AND `tokensForMatch(content)[word]`
- [ ] A delete failure after a successful add leaves the word added (`TestFlowDeleteFailsAfterSuccessfulAdd`)
- [ ] `go vet ./...` clean

---

### Task 3: Wire into main.go (twelfth handler) + full-suite green

**Context:** The handler is passive — one `MessageCreate` call in the existing event chain, no command registration. This task makes the production path reach it and leaves the whole repo green (unit suite, vet, lint, the CI selftest's construction surface, and a migration rehearsal against the COMPOSE database — never the live one).

**Files:**
- Modify: `cmd/tugbot/main.go`

**What to implement (exact edits to `cmd/tugbot/main.go` — and nothing else in the file):**

1. Import block: add `"github.com/danielcherubini/tugbot/internal/handlers/derpies"` (alphabetical position: after `cull`, before `feat`).
2. `type handlers struct`: add the field `derpies *derpies.Derpies` immediately AFTER the existing `cull *cull.Cull` line (i.e., as the last handler field).
3. `newHandlers`: add `derpies: derpies.New(a),` as the last entry in the returned literal.
4. The `OnMessageCreate` handler's inner goroutine: add `h.derpies.MessageCreate(m)` as the LAST call in the chain, after `h.mention.MessageCreate(m)`.
5. Text updates — find and replace exactly these three (only "eleven" → "twelve" in each; nothing else):
   - the `--selftest` flag description: `...construct the discordgo session and all eleven handlers` → `...all twelve handlers`
   - the `newHandlers` doc comment: `// newHandlers constructs all eleven handlers (the selftest's "handler` → `// newHandlers constructs all twelve handlers (the selftest's "handler`
   - the selftest log line: `slog.Info("selftest: Discord session and all eleven handlers constructed", "module", "main")` → `...twelve...`

   Do NOT touch `registerCommands`, the `dispatchCommand` switch, `readyThreeWay`, or any other handler's registration.

**Steps:**
- [ ] Run `go build ./...`
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Make the compose PG available: `make db-up` (or: `docker compose up -d postgres`)
- [ ] Run `go test ./cmd/tugbot/ -count=1`
  - Did all tests pass? NOTE: `TestSelftestBoundedPoolStart` does NOT skip when PG is unavailable — it black-holes the selftest URL via `TUGBOT_TEST_SELFTEST_DATABASE_URL` and expects the bounded 30 s pool deadline; expect it to always run (about 30 s) and pass. If a test failed, fix the failures and re-run before continuing.
- [ ] Run `make selftest`
  - Did it print `selftest: Discord session and all twelve handlers constructed` and exit 0? If not, fix and re-run before continuing. NOTE: `make selftest` builds and runs `tugbot --selftest`, which connects to `postgres://postgres:postgres@localhost:5432/tugbot` — the compose DB.
- [ ] Run the migration rehearsal against the COMPOSE DB — PIN the URL explicitly so a developer's `.env` pointing `DATABASE_URL` at the live DB cannot redirect this step (`cmd/migrate` reads `DATABASE_URL` from the environment and refuses to run when it is unset; a bare `make migrate` is FORBIDDEN here): `DATABASE_URL=postgres://postgres:postgres@localhost:5432/tugbot make migrate`
  - Did the output contain a line mentioning the version `000002_derpies_gimmicks` (or, on the second run, be a no-op with no error)? Run it TWICE to verify idempotency.
- [ ] Verify the seed state on the compose DB with a one-line query: `docker compose exec postgres psql -U postgres -d tugbot -c "SELECT word, source FROM derpies_gimmicks ORDER BY id; SELECT name, enabled FROM features WHERE name='derpies';"`
  - Did it print exactly the five seed words (all `seed`) and the `derpies` flag row? If the table is missing, re-run `make migrate` and investigate.
- [ ] Run the DB-touching suites SERIALLY against compose PG via `make test-db` — the target must cover the derpies package: extend it in the Makefile from `go test ./internal/dbmigrate ./internal/features -count=1` to `go test -p 1 ./internal/dbmigrate ./internal/features ./internal/handlers/derpies -count=1` (this is the ONE authoritative path — no parallel ad-hoc runs; the `-p 1` serialization matters because the `dbmigrate` tests' `resetMigrateState` `DROP`s `schema_migrations` and the `features` tests truncate their own rows, both of which would race the new integration test's preconditions/Run under parallel package execution; the given order also makes the features package create `features` with the UNIQUE constraint before derpies needs it)
  - Did all tests pass? NOTE: `make test-db` stops the compose PG at the end (`docker compose down`); if the compose setup has no `tugbot_test` database, create it first with `docker compose up -d postgres && docker compose exec postgres psql -U postgres -c "CREATE DATABASE tugbot_test;"` and re-run.
- [ ] Run the full unit suite (PG-dependent tests skip themselves): `go test ./... -count=1`
  - Did all tests pass or skip? If not, fix the failures and re-run before continuing.
- [ ] Run `golangci-lint run`
  - Does it report no issues? If not, fix and re-run before continuing.
- [ ] Run `go vet ./...`
- [ ] Commit with message: "wire the derpies filter into the message event chain (twelfth handler)"

**Acceptance criteria:**
- [ ] `go build ./...`, `go vet ./...`, `golangci-lint run` all clean
- [ ] `go test ./... -count=1` green (PG-dependent tests skip cleanly without compose PG)
- [ ] `make selftest` prints the TWELVE-handlers line and exits 0
- [ ] `make migrate` is idempotent and the compose DB holds the five seeded words + the `derpies` flag row
- [ ] The `cmd/tugbot/main.go` diff touches ONLY: the import, the struct field, `newHandlers`, the `OnMessageCreate` chain, and the three "eleven"→"twelve" text lines

---

## Out of scope (do not implement)

- The Rust `derpies` module's reaction-removal behavior (dead; superseded by this feature's delete-only behavior).
- Any notification to `#the-gulag`, any comprehensive bot message, any reaction from the bot.
- Per-user offense counters, rate limiting, or `SlowUserIDs`-style auto-gulag for derpies — the spec is delete-only.
- Editing `internal/db` (no sqlc regeneration) and touching the Rust repo.
