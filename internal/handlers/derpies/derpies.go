// Package derpies is the other-side filter for the user(s) in
// config.Config.DerpiesUserIDs: every message they post is first checked
// against the derpies_gimmicks word list (fast path — exact token match);
// a miss falls through to a pi RPC verdict (the slow path) that scores
// respellings / fresh gimmicks against the decision matrix (learn at
// score >= 40, delete at score >= T) and learners new words into the list
// at runtime.
//
// Degradation discipline (mirroring the mention feature): every failure
// arm logs and stops — the flow never acts on a half-loaded list, and a
// delete failure after a successful learn keeps the word learned.
//
// It also watches GUILD_MEMBER_UPDATE for the same gated users: a
// nickname judged a gimmick is reset to the fixed neutral name (a SET
// of a constant, not a null clear — nil is a dead end: the display
// would fall back to a global name the bot cannot touch) via the same
// fast path + pi RPC verdict, per member coalesced to one reset attempt
// per 60-second window (nicknames.go).
package derpies

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
	"github.com/danielcherubini/tugbot/internal/mcp"
	"github.com/danielcherubini/tugbot/internal/wordmatch"
)

const (
	FeatureKey = "derpies"
	SourceSeed = "seed"
	SourceLLM  = "llm"

	// learnFloor — the decision-matrix floor: the matrix is defined for
	// T > learnFloor only (learn at score >= 40, delete at score >= T).
	learnFloor = 40
	// defaultThreshold — the T a clamp (or a missing/errored config row)
	// falls back to; the derpies_config seed row's value.
	defaultThreshold = 50

	// The single-slowmode gate (decision 0011): the 3rd+ single-token post by a
	// gated author per channel within a rolling window is deleted before the fast
	// path. V1 code constants — no dials, no config.
	slowmodeMaxPosts = 2
	slowmodeWindow   = 30 * time.Second

	module = "derpies" // slog module tag
)

// Derpies handles the derpies flow (feature gate → guild guard →
// author-ID gate → [single-slowmode gate, creates only — decision 0011,
// the 3rd+ single-token post per channel within 30 s] → fast-path
// token match → pi RPC verdict). It also
// watches GUILD_MEMBER_UPDATE events for the same gated users and resets
// their nicks to a fixed neutral name when the flow judges them
// gimmicks (nicknames.go).
type Derpies struct {
	app *app.App
	// New() wires the production store/ops. Tests (same package) assign
	// the fakes directly, mirroring mention_test.go.
	store store
	ops   discordOps
	// clock is the injectable clock (tests pin it; markEdit snapshots
	// clock() at event time for the 60s window math).
	clock func() time.Time
	// videoFrameDecoder decodes a gifv embed video's (mp4/webm) bytes into
	// <=8 seek-sampled JPEG frames. New() wires the real govidVideoFrames
	// (pure Go, no cgo); tests wire a fake or leave it nil. A nil decoder
	// or ANY decoder error means "no frames for that video" (slog.Warn,
	// module derpies, degrade to thumbnail-only, never abort the flow).
	videoFrameDecoder func(data []byte) ([]app.PiImage, error)
	// lastNick is the per-member last-known nickname: after a successful
	// reset the cache holds derpiesNickReset (the echo of our own set is
	// skipped via the cur == evt.Nick check — the echo arrives with
	// Nick == derpiesNickReset, non-empty); "" still means "no nick
	// known / cleared state" (failure arms and the empty-nick arm write
	// ""). Events whose Nick equals the cache were not nick changes and
	// are skipped.
	lastNick map[string]string // key: guildID + "|" + memberID
	lastEdit map[string]time.Time
	busy     map[string]bool
	nickMu   sync.Mutex // guards the three maps (lastNick, lastEdit, busy)
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
}

// New builds the handler from the shared *app.App (mirrors the mention
// package's constructor). Initializes ALL nickname-flow state: the maps
// are keyed by guild+member and the first live event assigns into them,
// so a nil map here would panic on the first live event (the selftest
// constructs the handler but never dispatches events, so no gate would
// catch it).
func New(a *app.App) *Derpies {
	return &Derpies{app: a, store: &poolStore{pool: a.Pool}, ops: &realOps{d: a.D},
		clock: time.Now, videoFrameDecoder: govidVideoFrames,
		lastNick: map[string]string{},
		lastEdit: map[string]time.Time{}, busy: map[string]bool{}}
}

// ---------------------------------------------------------------------------
// Dependency seams (production: pgx pool + features + discordgo; tests:
// fakes)
// ---------------------------------------------------------------------------

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
	// promptText — the live prompt template (derpies_prompt, one row;
	// per-message fetch like the flag and the list). Row absent / any
	// error -> error — the flow's fallback engages (code default), so
	// the filter never runs with a broken prompt.
	promptText(ctx context.Context) (string, error)
	// configThreshold — the live delete threshold T (derpies_config,
	// one row). A DB error OR a missing row -> (0, err) (the caller's
	// fallback is Task 2); a valid row -> (clampThreshold(value), nil)
	// — the clamp is applied here, so a pre-CHECK read of T ≤ 40 or
	// > 100 yields the default.
	configThreshold(ctx context.Context) (int, error)
	// listPhrases — the stored multi-word phrases (derpies_gimmick_phrases),
	// sorted ascending (the {known} phrases sub-block is byte-stable,
	// mirroring sortedKeys for the words). A DB error propagates.
	listPhrases(ctx context.Context) ([]string, error)
	// recordDecision — append one decision row (derpies_decisions,
	// append-only); the pointer fields pass through as NULL when nil.
	recordDecision(ctx context.Context, d *decisionRecord) error
	// queryDecisions — the parameterized SELECT behind ReadDecisions
	// (Task 4): optional filter clauses (all AND-combined), ORDER BY
	// created_at DESC, LIMIT min(limit, 500).
	queryDecisions(ctx context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error)
}

type discordOps interface {
	// deleteMessage — the MESSAGE flow's only outgoing REST call that
	// takes an action (the referenced fetch is a data GET, not a bot
	// action; the nickname flow's action arm is setNickname, a PATCH).
	deleteMessage(channelID, messageID string) error
	// channelMessageRetrieve — the one-hop referenced-message fetch
	// (mention parity): a REST GET of the earlier message behind a
	// reply/quote-reply. A failure (already deleted, rate limit) is
	// handled by the CALLER as "no reference" — log and continue, never
	// abort the flow.
	channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error)
	// setNickname — set the guild nickname (the reset is a fixed neutral
	// value, not a clear).
	setNickname(guildID, memberID, nick string) error
}

// poolStore is the production store (raw SQL over the shared pool).
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

func (p *poolStore) promptText(ctx context.Context) (string, error) {
	var body string
	err := p.pool.QueryRow(ctx, `SELECT body FROM derpies_prompt LIMIT 1`).Scan(&body)
	return body, err
}

// errConfigThresholdMissing — the sentinel for a derpies_config with no
// row (a DB error is returned as-is; a missing row gets this sentinel so
// the caller can distinguish "not seeded yet" from a real failure).
var errConfigThresholdMissing = errors.New("derpies_config row missing")

func (p *poolStore) configThreshold(ctx context.Context) (int, error) {
	var v int
	// Target the singleton row explicitly (id = 1, enforced by the DDL's
	// CHECK) rather than LIMIT 1 — a stray second row can never be picked.
	err := p.pool.QueryRow(ctx, `SELECT delete_threshold FROM derpies_config WHERE id = 1`).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errConfigThresholdMissing
		}
		return 0, err
	}
	return clampThreshold(v), nil
}

func (p *poolStore) listPhrases(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT phrase FROM derpies_gimmick_phrases ORDER BY phrase`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ph string
		if err := rows.Scan(&ph); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

func (p *poolStore) recordDecision(ctx context.Context, d *decisionRecord) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO derpies_decisions (message_id, channel_id, author_id, content, path, score, threshold, word, learned, deleted, reject_reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		d.MessageID, d.ChannelID, d.AuthorID, d.Content, d.Path, d.Score, d.Threshold, d.Word, d.Learned, d.Deleted, d.RejectReason)
	return err
}

func (p *poolStore) queryDecisions(ctx context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50 // the default
	}
	if limit > 500 {
		limit = 500 // the max — clamped, not an error
	}
	var (
		conds []string
		args  []any
	)
	addEq := func(col string, v any) {
		args = append(args, v)
		conds = append(conds, col+" = $"+strconv.Itoa(len(args)))
	}
	addCmp := func(col string, op string, v any) {
		args = append(args, v)
		conds = append(conds, col+" "+op+" $"+strconv.Itoa(len(args)))
	}
	if f.AuthorID != "" {
		addEq("author_id", f.AuthorID)
	}
	if f.ChannelID != "" {
		addEq("channel_id", f.ChannelID)
	}
	if f.Path != "" {
		addEq("path", f.Path)
	}
	if f.Deleted != nil {
		addEq("deleted", *f.Deleted)
	}
	if f.ScoreMin != nil {
		addCmp("score", ">=", *f.ScoreMin)
	}
	if f.ScoreMax != nil {
		addCmp("score", "<=", *f.ScoreMax)
	}
	if f.Since != nil {
		addCmp("created_at", ">=", *f.Since)
	}
	if f.Until != nil {
		addCmp("created_at", "<=", *f.Until)
	}
	query := `SELECT id, message_id, channel_id, author_id, content, path, score, threshold, word, learned, deleted, reject_reason, created_at
		 FROM derpies_decisions`
	if len(conds) > 0 {
		query += "\n\t\t WHERE " + strings.Join(conds, " AND ")
	}
	// The id tiebreaker (appended to the ORDER BY below) makes same-timestamp
	// created_at rows deterministic (newest id first) instead of
	// nondeterministic.
	args = append(args, limit)
	query += "\n\t	 ORDER BY created_at DESC, id DESC LIMIT $" + strconv.Itoa(len(args))
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mcp.DecisionRow
	for rows.Next() {
		var r mcp.DecisionRow
		if err := rows.Scan(&r.ID, &r.MessageID, &r.ChannelID, &r.AuthorID, &r.Content, &r.Path, &r.Score, &r.Threshold, &r.Word, &r.Learned, &r.Deleted, &r.RejectReason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// realOps is the production Discord REST surface (the flow's single
// outgoing REST call).
type realOps struct{ d *discordgo.Session }

func (o *realOps) deleteMessage(channelID, messageID string) error {
	return o.d.ChannelMessageDelete(channelID, messageID)
}

func (o *realOps) channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error) {
	return o.d.ChannelMessage(channelID, messageID)
}

// setNickname — set the guild nickname (the reset value; Discord's
// member PATCH accepts a nick string — no null trick needed now).
func (o *realOps) setNickname(guildID, memberID, nick string) error {
	_, err := o.d.Request("PATCH", "/guilds/"+guildID+"/members/"+memberID, map[string]any{"nick": nick})
	return err
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// punctTrim is the ASCII edge-punctuation set trimmed from each token
// before the exact match (a trailing "sw1ft." must hit "sw1ft"). Split
// in two consts because a raw string cannot contain the backtick inside
// it cleanly. It is the ASCI arm of the token-edge trim: edgePunct
// combines it with the unicode punctuation/symbol categories, so a
// native-script token trims the same way (a trailing "خفيف؟" must hit
// "خفيف"; ؟ is Po).
const punctA = `!"#$%&()*+,-./:;<=>?@[]^_`
const punctB = "`{|}~"

var punctTrim = punctA + punctB

// edgePunct: a rune that is stripped from token EDGES only (never
// mid-token — "s-w1ft" keeps its interior dash) — the ASCII punctTrim
// set PLUS the punctuation/symbol unicode categories: Po (other
// punctuation — ؟ U+061F, ، U+060C), Pd (dashes), Pi/Pf (quotes),
// Ps/Pe (brackets), and the symbol categories Sc/Sm/So/Sk (a symbol
// wedged at a token edge is punctuation, not part of the word).
// Space/format need no entry: FoldToASCII's Cf drop and strings.Fields
// already handle them.
func edgePunct(r rune) bool {
	return strings.ContainsRune(punctTrim, r) ||
		unicode.Is(unicode.Po, r) || unicode.Is(unicode.Pd, r) ||
		unicode.Is(unicode.Pi, r) || unicode.Is(unicode.Pf, r) ||
		unicode.Is(unicode.Ps, r) || unicode.Is(unicode.Pe, r) ||
		unicode.Is(unicode.Sc, r) || unicode.Is(unicode.Sm, r) ||
		unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r)
}

// tokensForMatch: fold each fielded token (foldToASCII — which includes
// the lowercasing), and trim leading and trailing punctuation off each
// FOLDED token — ASCII punctTrim PLUS the unicode punctuation/symbol
// categories at the token edges only (edgePunct — the LLM's verdict
// word and the stored word space now include Arabic, so a message
// «خفيف؟» anchors «خفيف»). Keys of the result map are the folded
// tokens. "Who's giving me a sw1ft." -> {who's, giving, me, a, sw1ft}
// (ASCII — unchanged). "A świft cog" -> {a, swift, cog}.
// "خفيف؟" -> {خفيف}. A token whose edges trim it entirely (a pure-
// punctuation token) is dropped: an empty key would match nothing.
//
// The map ALSO carries COLLAPSED runs of single-rune tokens (the SPLIT
// evasion, the 2026-09-17 prod case: a known word spread over spaces —
// "z w i f t" is zwift). A run is a maximal stretch of consecutive tokens
// whose folded form is a SINGLE rune; a token that folded to "" (pure
// punctuation) does NOT break the run (a dot wedged between letters is
// part of the split, not a word boundary); a multi-rune token breaks it.
// A run of >=2 single-rune tokens contributes its collapsed (space-free)
// form, capped at 32 runes (wordValid's max — each run element is one
// rune, so the collapsed form has exactly len(run) runes and can never
// match a stored word beyond the charset bound). The collapse is ADDITIVE
// — the individual single-rune tokens stay in the map — so the fast path
// (zwift is a seed) and the verdict gate (anchor) both see "zwift" in a
// message that posted "z w i f t".
func tokensForMatch(content string) map[string]bool {
	tokens := strings.Fields(content)
	out := make(map[string]bool, len(tokens))
	folded := make([]string, len(tokens))
	for i, tok := range tokens {
		key := strings.TrimFunc(wordmatch.FoldToASCII(tok), edgePunct)
		folded[i] = key
		if key != "" {
			out[key] = true
		}
	}
	var run []string
	flush := func() {
		if len(run) >= 2 && len(run) <= 32 {
			out[strings.Join(run, "")] = true
		}
		run = nil
	}
	for _, k := range folded {
		if k == "" {
			continue // pure punctuation: skip, do NOT break the run
		}
		if len([]rune(k)) == 1 {
			run = append(run, k)
			continue
		}
		flush()
	}
	flush()
	return out
}

// parseVerdictScore: the score-model verdict parser. Scans ALL lines (not
// just the first non-empty): each line is TrimSpace'd first (the prompt
// displays the reply format indented — "  SCORE:<0-100>" — and an LLM
// echoing the indentation must still parse), then:
//   - the first line matching ^SCORE:\s*(\d+)$ (case-insensitive) whose
//     captured value is 0..100 is the score (\s* absorbs a space after the
//     colon — "Score: 95" is a very common LLM shape). A SCORE line out of
//     range (SCORE:150) is NOT a valid score line — it is skipped, and a
//     later in-range SCORE line can still be the score.
//   - the first line matching ^WORD:\s*(.+)$ (case-insensitive) is the
//     word — the remainder of the line (trimmed, lowercased), so a spaced
//     SPLIT answer (WORD:z w i f t) is captured verbatim and handed to the
//     unchanged foldVerdictWord whitespace-collapse in the gate (ADR 0009's
//     belt-and-suspenders: both the spaced and the collapsed answer form
//     learn the collapsed word).
//
// No valid SCORE line -> hasScore=false (the caller treats it as
// "unrecognized").
var (
	scoreLineRe = regexp.MustCompile(`(?i)^SCORE:\s*(\d+)$`)
	wordLineRe  = regexp.MustCompile(`(?i)^WORD:\s*(.+)$`)
)

func parseVerdictScore(text string) (score int, hasScore bool, word string) {
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if !hasScore {
			if m := scoreLineRe.FindStringSubmatch(l); m != nil {
				v, err := strconv.Atoi(m[1])
				if err == nil && v >= 0 && v <= 100 {
					score, hasScore = v, true
				}
			}
		}
		if word == "" {
			if m := wordLineRe.FindStringSubmatch(l); m != nil {
				word = strings.ToLower(strings.TrimSpace(m[1]))
			}
		}
	}
	return
}

// wordLike (the anchor-gate token classifier, the emoji fix A): is tok a
// word-shape token — the ASCII arm (wordValid) or the non-ASCII arm
// (unicodeVerdictShape), with the shared precondition (a pure digit string
// is never a word) and the empty guard? A non-word token (an emoji, a
// bare number, a punctuation blob) does not count as an anchor.
func wordLike(tok string) bool {
	if tok == "" || allDigitVerdict(tok) {
		return false
	}
	return wordmatch.WordValid(tok) || unicodeVerdictShape(tok)
}

// foldedTokenSequence: the ordered, edge-trimmed, folded token sequence
// (for the phrase match): strings.Fields, each token wordmatch.FoldToASCII
// + strings.TrimFunc(edgePunct), preserving order and duplicates, dropping
// tokens that trim to "". (No dedup, no collapsed-run handling — that
// stays in tokensForMatch, unchanged.)
func foldedTokenSequence(content string) []string {
	out := make([]string, 0)
	for _, tok := range strings.Fields(content) {
		key := strings.TrimFunc(wordmatch.FoldToASCII(tok), edgePunct)
		if key != "" {
			out = append(out, key)
		}
	}
	return out
}

// decisionRecord — the in-memory decision row (mirrors the derpies_decisions
// columns; the nullable fields are pointers so a zero value writes SQL
// NULL). Path is a *string to match the DDL's
// "CHECK (path IN ('fast','slow','slowmode'))" (migration 000007 extended
// the two-value form): a NULL path passes the CHECK (PostgreSQL evaluates
// CHECK on NULL as satisfied), so the pre-path arms (which never assign
// Path) persist as "path IS NULL" instead of being rejected by a NOT
// NULL/empty-string CHECK.
type decisionRecord struct {
	MessageID    string
	ChannelID    string
	AuthorID     string
	Content      string
	Path         *string // "fast" | "slow" | "slowmode" | NULL (NULL for every arm that never reached a path)
	Score        *int    // NULL for fast rows + every arm that never reached the matrix
	Threshold    *int    // NULL, same as Score
	Word         *string
	Learned      bool
	Deleted      bool
	RejectReason *string // NULL when no rejection
}

// clampThreshold: pure — if v < 41 || v > 100 { return defaultThreshold }
// return v. The matrix is defined for T > learnFloor only; a T ≤ 40 value
// is unrepresentable — the CHECK rejects it at write time, this clamps a
// pre-CHECK read.
func clampThreshold(v int) int {
	if v < learnFloor+1 || v > 100 {
		return defaultThreshold
	}
	return v
}

// unicodeVerdictShape (ADR 0008): the FOLDED non-ASCII verdict word's
// plausible-word shape — letters-only (every rune in the COMBINED
// Unicode L category: no whitespace, no punctuation, no digits — which
// also guarantees at least one letter), 2..32 runes (inclusive, on the
// RUNE count). Deliberately NOT [a-z0-9]-based — that is the ASCII arm
// (wordValid); the flow checks arm (a) first, so a word that had
// already passed wordValid never reaches this check, and an all-digit
// string is rejected upstream (the shared precondition).
func unicodeVerdictShape(fw string) bool {
	if fw == "" {
		return false
	}
	n := 0
	for _, r := range fw {
		if !unicode.Is(unicode.L, r) {
			return false
		}
		n++
	}
	return n >= 2 && n <= 32
}

// allDigitVerdict: shared precondition (ADR 0008) — is fw a PURE digit
// string (every rune a Unicode digit)? wordValid's charset admits
// digits, but a twist word must contain at least ONE letter, so
// "12345" is invalid on both arms regardless of its 2..32 shape.
func allDigitVerdict(fw string) bool {
	if fw == "" {
		return false
	}
	for _, r := range fw {
		if !unicode.Is(unicode.Nd, r) {
			return false
		}
	}
	return true
}

// foldVerdictWord: the verdict word's FOLDED form with ALL whitespace
// collapsed away (the SPLIT evasion, 2026-09-17: a known word spread over
// spaces — "z w i f t" — is judged and answered as the spaced form, but
// the stored / matched space is the COLLAPSED form, so the verdict is
// normalized to it before the two-arm gate). FoldToASCII drops Mn/Cf but
// NOT regular spaces, so the space collapse is explicit. The collapse is
// safe: a collapsed form only passes the gate when it is a token of the
// message (the collapsed run in tokensForMatch), so it is anchored — a
// hallucinated word (spaced or not) that is not in the message still fails
// the token check. A whitespace-only word collapses to "" and is rejected
// by the same fw == "" guard as before.
func foldVerdictWord(word string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, wordmatch.FoldToASCII(word))
}

// sortedKeys returns the map keys sorted (deterministic prompt text, so the
// prompt is byte-stable per list state and unit tests can assert on it).
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The image leg (mention mirror — attachments + embeds, isSafeURL-guarded,
// extension→MIME, per-URL failure logged + skipped; the ONLY differences
// from mention's versions are the slog module tag and the downloadPlan /
// downloadImages split so the current+referenced plans can merge before a
// single download pass)
// ---------------------------------------------------------------------------

// imagePlanEntry is the planned (url, mime, source) triple before download.
type imagePlanEntry struct {
	url    string
	mime   string
	source string // "attachment" | "embed"
}

// imageURLPlan mirrors mention's plan: attachment urls with an image/* content
// type (empty content_type falls back to application/octet-stream and is
// skipped, like mention), then embed image/thumbnail urls deduped against the
// ATTACHMENT urls and MIME'd by extension (query/fragment stripped first:
// png→image/png, gif→image/gif, webp→image/webp, else image/jpeg), then — the
// derpies-specific video extension — EVERY gifv embed's video URL (a gifv embed
// whose Video URL is non-empty) as an embed-video entry, isSafeURL-guarded.
// The video mime is IRRELEVANT (the govid decoder sniffs the container), so the
// entry's mime is left empty; a non-gifv embed carrying a video is NOT planned,
// and an unsafe video URL is skipped with a log (mirroring the embed-URL guard).
// Every url must pass isSafeURL.
func imageURLPlan(m *discordgo.Message) []imagePlanEntry {
	if m == nil {
		return nil
	}
	var plan []imagePlanEntry
	var attachmentURLs []string
	for _, a := range m.Attachments {
		attachmentURLs = append(attachmentURLs, a.URL)
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if !strings.HasPrefix(contentType, "image/") {
			continue
		}
		if !isSafeURL(a.URL) {
			slog.Info("Skipping unsafe URL: "+a.URL, "module", module)
			continue
		}
		plan = append(plan, imagePlanEntry{url: a.URL, mime: contentType, source: "attachment"})
	}
	for _, e := range m.Embeds {
		var url string
		if e.Image != nil {
			url = e.Image.URL
		}
		if url == "" && e.Thumbnail != nil {
			url = e.Thumbnail.URL
		}
		if url == "" {
			continue
		}
		deduped := false
		for _, du := range attachmentURLs {
			if du == url {
				deduped = true
				break
			}
		}
		if deduped {
			continue
		}
		if !isSafeURL(url) {
			slog.Info("Skipping unsafe embed URL: "+url, "module", module)
			continue
		}
		plan = append(plan, imagePlanEntry{url: url, mime: mimeForURL(url), source: "embed"})
	}
	// The derpies video extension: every gifv embed carries the actual
	// animation in MessageEmbed.Video (the thumbnail is only the first
	// frame). Plan that video URL (isSafeURL-guarded, mime-irrelevant) so it
	// downloads + decodes into <=8 frames. A non-gifv embed (e.g. Type
	// "video") is NOT planned; an unsafe URL is skipped with a log.
	for _, e := range m.Embeds {
		if e.Type != "gifv" || e.Video == nil || e.Video.URL == "" {
			continue
		}
		if !isSafeURL(e.Video.URL) {
			slog.Info("Skipping unsafe embed video URL: "+e.Video.URL, "module", module)
			continue
		}
		plan = append(plan, imagePlanEntry{url: e.Video.URL, source: "embed-video"})
	}
	return plan
}

// isSafeURL: http:// or https:// prefix only.
func isSafeURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// mimeForURL: extension map (png/gif/webp; everything else image/jpeg),
// query/fragment stripped first.
func mimeForURL(url string) string {
	path := url
	for _, sep := range []string{"?", "#"} {
		if i := strings.Index(path, sep); i >= 0 {
			path = path[:i]
		}
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	ext := ""
	if i := strings.LastIndex(path, "."); i >= 0 && i < len(path)-1 {
		ext = path[i+1:]
	}
	switch ext {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// downloadPlan is the per-URL GET leg: request with the explicit-timeout
// client, io.ReadAll, base64 standard-alphabet encoded. A failed download
// (request error or non-nil ReadAll error) is logged (module derpies) and
// that URL is skipped — the flow continues with the rest. Attachment
// downloads log the url + mime; embed downloads log the url. 5xx/4xx
// responses are NOT an error here (mention parity: the body is read whatever
// the status carries) — mirror mention EXACTLY. The ONE derpies-specific
// branch: a gif mime expands in-process (expandGIFFrames) into ≤8 evenly-
// spaced JPEG frames (the gifFrames return counts emitted frames); a
// single-frame / undecodable gif degrades to the status-quo RAW send with
// an slog.Warn degradation log, and per-frame encode failures are logged.
// The derpies VIDEO leg branch (source == "embed-video", what imageURLPlan
// appends per gifv embed video URL): downloadVideoBytes on the SAME client.
// ANY download error (request failure, non-2xx, over-cap Content-Length or
// body), a nil videoFrameDecoder, or ANY decoder error is logged (slog.Warn,
// module derpies) + skipped — the thumbnail (if one was planned) still rides on
// its own entry, so that IS the degrade. On success the emitted frames are
// appended to the images and counted into gifFrames.
//
// Returns (images, gifFrames).
func (h *Derpies) downloadPlan(ctx context.Context, plan []imagePlanEntry, client *http.Client) ([]app.PiImage, int) {
	var images []app.PiImage
	gifFrames := 0
	for _, entry := range plan {
		// The gifv embed-video leg: download + decode the actual animation
		// into <=8 frames. Every failure arm (download, decoder wiring,
		// decoder error) logs + skips — the thumbnail, if planned, rides on
		// its own entry, so this IS the degrade. Never aborts the flow.
		if entry.source == "embed-video" {
			slog.Info("Downloading embed video: "+entry.url, "module", module)
			body, err := downloadVideoBytes(ctx, entry.url, client)
			if err != nil {
				slog.Warn("derpies video download failed — skipping (degrade to thumbnail-only)", "module", module, "url", entry.url, "error", err)
				continue
			}
			if h.videoFrameDecoder == nil {
				slog.Warn("derpies videoFrameDecoder not wired — skipping video frames (degrade)", "module", module, "url", entry.url)
				continue
			}
			frames, err := h.videoFrameDecoder(body)
			if err != nil {
				slog.Warn("derpies video decode failed — skipping (degrade to thumbnail-only)", "module", module, "url", entry.url, "error", err)
				continue
			}
			if len(frames) == 0 {
				slog.Warn("derpies video yielded no frames — skipping (degrade to thumbnail-only)", "module", module, "url", entry.url)
				continue
			}
			gifFrames += len(frames)
			images = append(images, frames...)
			continue
		}
		if entry.source == "attachment" {
			slog.Info("Downloading image: "+entry.url+" ("+entry.mime+")", "module", module)
		} else {
			slog.Info("Downloading embed image: "+entry.url, "module", module)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.url, nil)
		if err != nil {
			slog.Error("Failed to download image", "module", module, "url", entry.url, "error", err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Error("Failed to download image", "module", module, "url", entry.url, "error", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			slog.Error("Failed to read image bytes", "module", module, "url", entry.url, "error", err)
			continue
		}
		if entry.mime == "image/gif" {
			// Animated-gif expansion: frames join the same single ask;
			// a single-frame or undecodable gif degrades to the status-
			// quo raw send (log, never abort the flow).
			expanded, skipped, err := expandGIFFrames(body)
			if err != nil {
				// Each budget refusal is a DISTINCT arm: the log must name
				// WHICH bound fired (input bytes over the cap vs. decoded
				// dimensions over the cap — a small-bytes / large-screen gif
				// rejected by the dims bound), the decode work is never
				// attempted in either — same raw-send shape as the other
				switch {
				case errors.Is(err, errGIFTooLarge):
					slog.Warn("derpies gif over pre-decode input cap, raw send", "module", module, "url", entry.url, "bytes", len(body))
				case errors.Is(err, errGIFDimsTooLarge):
					slog.Warn("derpies gif over pre-decode decoded-dimension cap, raw send", "module", module, "url", entry.url, "bytes", len(body))
				default:
					slog.Warn("derpies gif expansion degraded to raw send", "module", module, "url", entry.url, "error", err)
				}
				images = append(images, app.PiImage{
					MimeType: entry.mime,
					Data:     base64.StdEncoding.EncodeToString(body),
				})
				continue
			}
			if skipped > 0 {
				slog.Warn("derpies gif frame encode failures skipped", "module", module, "url", entry.url, "skipped", skipped)
			}
			gifFrames += len(expanded)
			images = append(images, expanded...)
			continue
		}
		images = append(images, app.PiImage{
			MimeType: entry.mime,
			Data:     base64.StdEncoding.EncodeToString(body),
		})
	}
	return images, gifFrames
}

// downloadImages mirrors mention's one-message shape: plan then download
// (the gifFrame count is flow-only, so the wrapper discards it — the
// flow calls downloadPlan against the merged plan directly).
func (h *Derpies) downloadImages(ctx context.Context, m *discordgo.Message, client *http.Client) []app.PiImage {
	images, _ := h.downloadPlan(ctx, imageURLPlan(m), client)
	return images
}

// defaultPromptTemplate — the code-pinned default (exact text of the plan's
// "defaultPromptTemplate" block: from "A Discord message was just posted by
// a user…" through the "…the innocent reading is obvious." protocol block).
// It is the fallback the flow uses when the DB template is missing/invalid —
// the filter never runs with a broken prompt.
const defaultPromptTemplate = `A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of short, repetitive, annoying gimmicks — and of evading, over and over, the word filters built to catch them. He is notorious for this.

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
- Score 0-39 only when the message carries NO trace of the roster at all and the innocent reading is obvious.`

// validTemplate: the two MANDATORY literal markers are present. Absent
// optional markers ({{IMAGES}} / {{GIFS}} / {{REF}} / {{EMBED}}) are fine — the element
// is simply omitted.
func validTemplate(t string) bool {
	return strings.Contains(t, "{content}") && strings.Contains(t, "{known}")
}

// gimmickPrompt substitutes the SIX markers with a TWO-PHASE pass so a
// payload can never re-trigger a later marker scan. Pass 1 runs on the
// TEMPLATE ONLY (before any payload exists): each marker becomes a unique
// inert placeholder wrapped in NUL bytes. Pass 2 swaps the placeholders
// for the real payloads. content / refText are NUL-free Discord message
// text (Discord content cannot contain U+0000), and known words pass
// wordValid's ^[a-z0-9]{2,32}$ charset — so a message that literally
// contains "{known}" / "{{IMAGES}}" / "{{GIFS}}" / "{{EMBED}}" / "{{REF}}" survives verbatim instead of
// pulling the known block inside the untrusted fence or having its
// marker bytes silently deleted (the single-pass ReplaceAll ordering this
// replaces re-scanned the already-inserted content). embedTitles is
// EXTERNAL text (a third-party service's title, not Discord message
// content — it CAN contain arbitrary bytes); the invariant still holds
// because pass 1 runs on the template only, so a payload can never
// re-trigger a marker scan — a title that literally contained
// "\x00EMBEDTITLES\x00" would at worst swap inertly. The
// fence and the images line / gif-frames block / referenced block bytes are code-pinned — the
// template carries only the bare markers. The {known} block carries a
// second sub-block for the stored phrases (the optimizer): the phrases
// sub-block is emitted ONLY when phrases exist, appended after the
// words; the words sub-block is unchanged. (`known` arrives sorted from
// the flow — sortedKeys — and is joined one per line; the pi RPC always
// appends the anti-injection system fallback on top of this.)
func gimmickPrompt(tmpl string, content string, known []string, nImages, nGifFrames int, embedTitles string, refText string, phrases []string) string {
	// Pass 1: markers -> NUL-wrapped placeholders, template only.
	marked := tmpl
	marked = strings.ReplaceAll(marked, "{content}", "\x00CONTENT\x00")
	marked = strings.ReplaceAll(marked, "{known}", "\x00KNOWN\x00")
	marked = strings.ReplaceAll(marked, "{{IMAGES}}", "\x00IMAGES\x00")
	marked = strings.ReplaceAll(marked, "{{EMBED}}", "\x00EMBEDTITLES\x00")
	marked = strings.ReplaceAll(marked, "{{GIFS}}", "\x00GIFFRAMES\x00")
	marked = strings.ReplaceAll(marked, "{{REF}}", "\x00REF\x00")

	// Pass 2: placeholders -> payloads. A payload can never re-trigger a
	// later marker scan because pass 1 ran on the template only; the
	// content/refText/known payloads are NUL-free as the doc comment
	// argues, and embedTitles at worst swaps inertly.
	out := strings.ReplaceAll(marked, "\x00CONTENT\x00",
		"\n<<<UNTRUSTED MESSAGE\n"+content+"\n               UNTRUSTED MESSAGE>>>\n")
	knownBlock := ""
	if len(known) > 0 {
		knownBlock = "-----< known gimmick words (sorted ascending) >-----\n" + strings.Join(known, "\n")
	}
	// The phrases sub-block (the optimizer): appended after the words,
	// emitted ONLY when phrases exist (the words sub-block is unchanged).
	if len(phrases) > 0 {
		if knownBlock != "" {
			knownBlock += "\n"
		}
		knownBlock += "-----< known gimmick phrases (exact multi-word patterns) >-----\n" + strings.Join(phrases, "\n")
	}
	out = strings.ReplaceAll(out, "\x00KNOWN\x00", knownBlock)
	var imagesBlock string
	if nImages > 0 {
		imagesBlock = fmt.Sprintf("The message also has %d attached image(s) (screenshots or pasted images — a text filter would not see their content). Judge the text AND the images. If the anchor word appears in an image rather than the message text, name it as if it were in the message.", nImages)
	}
	out = strings.ReplaceAll(out, "\x00IMAGES\x00", imagesBlock)
	var gifsBlock string
	if nGifFrames > 0 {
		gifsBlock = "The message also has animated gif frame(s). In addition to the summary images above, below are up to 8 SAMPLED frames from each animated gif — gifs loop and change over time, so a gimmick's word can appear in ANY frame; judge all of the frames too."
	}
	out = strings.ReplaceAll(out, "\x00GIFFRAMES\x00", gifsBlock)
	var embedBlock string
	if embedTitles != "" {
		embedBlock = "TITLES OF MEDIA EMBEDDED WITH THE MESSAGE (provider-furnished untrusted text — part of what was posted, judge it as if written):\n" + embedTitles + "\nA known word appearing in an embed title counts as if it were typed in the message."
	}
	out = strings.ReplaceAll(out, "\x00EMBEDTITLES\x00", embedBlock)
	var refBlock string
	if refText != "" {
		refBlock = "<<<REFERENCED MESSAGE\n" + refText + "\nREFERENCED MESSAGE>>>\nThe message replies to a previous message (often the author's own) — the quoted content is above between the REFERENCED MESSAGE markers. Judge the posted text / images AND the quoted content together; a respelling may live in the quote rather than the new message."
	}
	return strings.ReplaceAll(out, "\x00REF\x00", refBlock)
}

// phraseWindowMatch: does the phrase's folded token sequence appear as an
// EXACT consecutive run of the content's folded token sequence? (v1 is
// exact-consecutive only — a SPLIT word inside the phrase does not
// match.) Both sequences are already folded (FoldToASCII + edge-punct
// trim), so the comparison is a plain string match — a curated "Buy me a
// bike" matches the posted "buy".
func phraseWindowMatch(seq, phrase []string) bool {
	if len(phrase) == 0 || len(seq) < len(phrase) {
		return false
	}
	for i := 0; i+len(phrase) <= len(seq); i++ {
		match := true
		for j := range phrase {
			if seq[i+j] != phrase[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// strPtr — a pointer to a string (the decision record's nullable fields
// take a pointer per rejection reason).
func strPtr(s string) *string { return &s }

// ReadDecisions — the mcp.DecisionSource implementation (the
// read_derpies_decisions tool's read): the Limit default/clamp is
// applied here (limit <= 0 → 50, limit > 500 → 500 — clamped, not an
// error), Since/Until are normalized to UTC before binding (the
// created_at column is timestamp without time zone; pgx encodes a
// *time.Time with its offset, so a non-UTC value would compare against
// the session's timezone interpretation), and a DB error propagates
// (the MCP tool surfaces it as a tool error — a read tool failing is
// not a silent degradation).
func (h *Derpies) ReadDecisions(ctx context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Since != nil {
		s := f.Since.UTC()
		f.Since = &s
	}
	if f.Until != nil {
		u := f.Until.UTC()
		f.Until = &u
	}
	return h.store.queryDecisions(ctx, f)
}

// recordDecision — the decision log's best-effort write (C): a failure
// logs (module derpies) and does not abort — the delete/learn already
// happened.
func (h *Derpies) recordDecision(ctx context.Context, m *discordgo.Message, d *decisionRecord) {
	if err := h.store.recordDecision(ctx, d); err != nil {
		slog.Error("derpies decision record failed", "module", module, "message", m.ID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// The flow
// ---------------------------------------------------------------------------

// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
// Burst amplification note (superseded for single-token posts, decision 0011):
// the single-slowmode gate (step 3.4) deletes a gated author's 3rd+
// single-token post per channel inside 30 s before the fast path (zero pi
// asks). LONG posts (≥2 tokens) are still unthrottled — N novel long posts
// still yield N serialized pi asks (the pi RPC queue is shared with the mention handler).
func (h *Derpies) MessageCreate(m *discordgo.Message) { go h.flowGated(m, true) }

// flow — the MessageCreate entry: the full flow WITH the single-slowmode
// gate. Existing tests call this; production MessageCreate (below) routes
// through flowGated directly for the gate. The edit flow (edits.go) calls
// flowGated directly with the gate OFF (approved rule: edits are out of
// gate scope — the gate watches MessageCreate only).
func (h *Derpies) flow(m *discordgo.Message) { h.flowGated(m, true) }

// gateSlowmode — the single-slowmode check (S1): called after the
// author-ID gate and BEFORE the decision-record defer. Returns true when
// it deleted the post (the caller must return — no fast path, no slow
// path, no pi ask). Ineligible (0-token or ≥2-token) posts fall through.
// Overflow (≥ slowmodeMaxPosts recent, window = now.Sub(ts) <=
// slowmodeWindow, inclusive) deletes via the ops seam (best-effort on
// failure — logged; the row is still written) and records its OWN
// path='slowmode' row. A pass-through appends its timestamp. REST + DB
// writes happen AFTER the lock (the delete can block on a Discord 429;
// do not hold rateMu across it).
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

// flowGated — the full message flow (gates → [single-slowmode gate,
// when slowmodeOn] → fast path → images → slow path → learn/delete).
// Each post costs at most one list SELECT + one pi ask. The
// MessageCreate entry (flow below) runs it with slowmodeOn=true; the
// edit flow (edits.go) runs it with slowmodeOn=false — the gate is
// the only flow element that differs between the two entries (approved
// v1 rule: edits are out of gate scope).
func (h *Derpies) flowGated(m *discordgo.Message, slowmodeOn bool) {
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

	// 3.4 Single-slowmode gate (decision 0011): the 3rd+ single-token post
	// within 30 s is deleted here — before the fast path (zero pi asks,
	// zero list fetch, zero image leg). A gate hit writes its own
	// path='slowmode' row; the deferred "C" record below never fires.
	if slowmodeOn && h.gateSlowmode(ctx, m) {
		return
	}

	// The decision log (C): one defer for every terminal arm — the
	// pointer is captured at defer-time, so the flow populates dec as it
	// progresses and the deferred write sees the final state. Pre-gate
	// arms (feature off, not a guild, author not gated) returned before
	// this point and write no row.
	dec := &decisionRecord{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		AuthorID:  m.Author.ID,
		Content:   m.Content,
	}
	defer h.recordDecision(ctx, m, dec)

	// 3.5 Referenced message (reply/quote-reply): one REST GET (mention
	//     parity, via the discordOps seam). On failure (already deleted,
	//     rate limit) log and continue WITHOUT a reference — never abort
	//     the flow (mention's step-7 discipline).
	var referenced *discordgo.Message
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		ref, err := h.ops.channelMessageRetrieve(m.ChannelID, m.MessageReference.MessageID)
		if err != nil {
			slog.Error("derpies referenced fetch failed", "module", module, "message", m.ID, "error", err)
		} else {
			referenced = ref
		}
	}

	// 4. Fast path: one list SELECT; exact token match. The token set is
	//     the UNION of the posted message and, when a referenced message
	//     was fetched, its content (a reply re-quoting a seeded word hits
	//     here without retyping). The posted content's token set is computed
	//     ONCE (postedToks) and reused for both the union build and the
	//     anchor gate's posted-only word-like check (step 9); the union
	//     (toks) is a copy so augmenting it with the referenced + embed-
	//     title tokens leaves postedToks posted-only.
	list, err := h.store.listGimmicks(ctx)
	if err != nil {
		slog.Error("derpies gimmick list fetch failed", "module", module, "error", err)
		dec.RejectReason = strPtr("list fetch failed")
		return
	}
	postedToks := tokensForMatch(m.Content)
	toks := make(map[string]bool, len(postedToks))
	for t := range postedToks {
		toks[t] = true
	}
	if referenced != nil {
		for t := range tokensForMatch(referenced.Content) {
			toks[t] = true
		}
	}
	// The non-empty embed titles join the union (the observed Klipy gifv
	// vector: a bare-URL post whose brand word lives in the embed title —
	// titled link the user chose to post counts as posted content; the
	// stance is adversarial, so false negatives are the worse error).
	for t := range tokensForMatch(embedTitleText(m)) {
		toks[t] = true
	}
	if referenced != nil {
		for t := range tokensForMatch(embedTitleText(referenced)) {
			toks[t] = true
		}
	}
	for tok := range toks {
		if list[tok] {
			fast := "fast"
			dec.Path = &fast
			dec.Word = &tok
			if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
				slog.Error("derpies delete (fast) failed", "module", module, "word", tok, "channel", m.ChannelID, "message", m.ID, "error", err)
				dec.RejectReason = strPtr("delete failed")
			} else {
				slog.Info("derpies delete (fast)", "module", module, "word", tok, "channel", m.ChannelID, "message", m.ID)
				dec.Deleted = true
			}
			return
		}
	}

	// 4b. Phrase fast path (the optimizer): the stored multi-word phrases
	//     (derpies_gimmick_phrases, manual-only) matched as an EXACT
	//     consecutive run of the folded token sequence — a curated phrase
	//     hits with zero asks, the same way the word fast path does. A
	//     fetch error skips the phrase match (log + continue to the slow
	//     path — never act on a half-loaded phrase list). The posted and
	//     the referenced content are scanned SEPARATELY, never
	//     concatenated (a phrase spanning the message/reply boundary must
	//     not match); embed titles are NOT scanned for phrases in v1. A
	//     SPLIT word inside a phrase is NOT fast-matched (v1 is
	//     exact-consecutive only — it falls to the slow path).
	var phrases []string
	if pl, err := h.store.listPhrases(ctx); err != nil {
		slog.Error("derpies phrase list fetch failed", "module", module, "error", err)
	} else {
		phrases = pl
		postSeq := foldedTokenSequence(m.Content)
		var refSeq []string
		if referenced != nil {
			refSeq = foldedTokenSequence(referenced.Content)
		}
		for _, phrase := range phrases {
			phr := foldedTokenSequence(phrase)
			if len(phr) == 0 {
				continue // a phrase with no non-empty tokens matches nothing
			}
			// refSeq may be nil (no referenced message); phraseWindowMatch's
			// len(seq) < len(phrase) guard already returns false for it, so
			// no nil check is needed here.
			if phraseWindowMatch(postSeq, phr) || phraseWindowMatch(refSeq, phr) {
				fast := "fast"
				dec.Path = &fast
				dec.Word = &phrase
				if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
					slog.Error("derpies delete (fast phrase) failed", "module", module, "phrase", phrase, "channel", m.ChannelID, "message", m.ID, "error", err)
					dec.RejectReason = strPtr("delete failed")
				} else {
					slog.Info("derpies delete (fast phrase)", "module", module, "phrase", phrase, "channel", m.ChannelID, "message", m.ID)
					dec.Deleted = true
				}
				return
			}
		}
	}

	// 4.5 Images (mention parity: attachments + embeds, isSafeURL-guarded,
	//     10s client, per-URL failure logged + skipped — the flow degrades
	//     to a text-only ask when nothing downloads). The plan is the union
	//     of the posted message and, when present, the referenced message,
	//     URL-deduped (the same screenshot in both must not double the
	//     base64 payload).
	imgPlan := imageURLPlan(m)
	if referenced != nil {
		imgPlan = append(imgPlan, imageURLPlan(referenced)...)
	}
	var uniqPlan []imagePlanEntry
	seenURLs := map[string]bool{}
	for _, e := range imgPlan {
		if seenURLs[e.url] {
			continue
		}
		seenURLs[e.url] = true
		uniqPlan = append(uniqPlan, e)
	}
	images, nGifFrames := h.downloadPlan(ctx, uniqPlan, &http.Client{Timeout: 10 * time.Second})

	// 5. Slow path: pi unavailable -> silent return (the mention feature's
	//    degradation path, same shape).
	if h.app.Pi == nil {
		slog.Info("derpies pi RPC not available, skipping", "module", module)
		dec.RejectReason = strPtr("pi unavailable")
		return
	}

	// 6-7. One ask (the 300s deadline lives in the pi package). AskWithImages
	//     when images downloaded; plain Ask otherwise (in production the two
	//     are equivalent — PiRpc.Ask is askWithImages(ctx, prompt, nil) — the
	//     branch keeps the text path's test seam clean). The template comes
	//     from the DB seam — a fetch error OR an invalid row (missing
	//     mandatory marker) falls back to the code default so the filter
	//     never runs with a broken prompt.
	tmpl, err := h.store.promptText(ctx)
	if err != nil || !validTemplate(tmpl) {
		tmpl = defaultPromptTemplate
		if err != nil {
			slog.Warn("derpies prompt template unavailable — using default", "module", module, "error", err)
		}
	}
	refContent := ""
	if referenced != nil {
		refContent = referenced.Content
	}
	// The embed titles (posted + referenced, exact lines deduped) join the
	// judged text in the prompt, the same way the referenced content does.
	embedTitles := embedTitleText(m)
	if referenced != nil {
		lines := strings.Split(embedTitles, "\n")
		seen := make(map[string]bool, len(lines))
		for _, line := range lines {
			seen[line] = true
		}
		for _, line := range strings.Split(embedTitleText(referenced), "\n") {
			if line == "" || seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, line)
		}
		embedTitles = strings.Join(lines, "\n")
	}
	prompt := gimmickPrompt(tmpl, m.Content, sortedKeys(list), len(images), nGifFrames, embedTitles, refContent, phrases)
	var (
		text   string
		askErr error
	)
	if len(images) > 0 {
		text, askErr = h.app.Pi.AskWithImages(ctx, prompt, images)
	} else {
		text, askErr = h.app.Pi.Ask(ctx, prompt)
	}
	if askErr != nil {
		slog.Error("derpies pi ask failed", "module", module, "error", askErr)
		dec.RejectReason = strPtr("ask failed")
		return
	}

	// 8. Parse the verdict (the score model). No valid SCORE line -> the
	//    existing degradation arm: log + do nothing (a legacy CLEAN / old
	//    GIMMICK verdict carries no score line and is unrecognized here;
	//    the nickname flow parses the same way).
	score, hasScore, word := parseVerdictScore(text)
	if !hasScore {
		slog.Warn("derpies unrecognized verdict — doing nothing", "module", module, "verdict", strings.TrimSpace(text))
		dec.RejectReason = strPtr("unrecognized verdict")
		return
	}

	// The live threshold T (derpies_config, one row; the store clamps a
	// valid-but-out-of-range read). A DB error OR a missing row degrades
	// to the default — the matrix still runs on it.
	t, err := h.store.configThreshold(ctx)
	if err != nil {
		t = defaultThreshold
		slog.Warn("derpies config threshold unavailable — using default", "module", module, "error", err)
	}
	slowPath := "slow"
	dec.Path = &slowPath
	dec.Score = &score
	dec.Threshold = &t

	// 9. Learn (independent of the delete), gated on score >= learnFloor:
	//    the two-arm gate (ADR 0008) on the FOLDED verdict word — the
	//    SPLIT evasion: foldVerdictWord collapses the whitespace, so a
	//    spaced split verdict ("z w i f t") normalizes to "zwift" before
	//    the gate, is anchored to the collapsed run, and learns the
	//    collapsed form. The gate: the shared pure-digit precondition
	//    (a twist word must contain at least one letter), arm (a)
	//    wordValid, arm (b) unicodeVerdictShape — with the anchor
	//    requirement scoped to the POSTED message's word-like tokens
	//    (the emoji fix A): a mention-snowflake (all-digit) / emoji-ref
	//    (colon) / pure-emoji token (folds to "") is not word-like, so a
	//    "mention + emoji" post is judged like an image-only post — a
	//    valid ASCII word passes wordValid alone and the score decides.
	//    A rejection only stops the LEARN — the delete (step 10) is an
	//    independent score, and a delete failure overrides any word-
	//    rejection reason on the decision row.
	if score >= learnFloor {
		fw := foldVerdictWord(word)
		switch {
		case fw == "":
			// An absent or whitespace-only word collapses to nothing:
			// no valid word to learn.
			slog.Warn("derpies invalid verdict word — not learning", "module", module, "word", word, "message", m.ID)
			dec.RejectReason = strPtr("no valid word")
		case allDigitVerdict(fw):
			slog.Warn("derpies all-digit verdict word — not learning", "module", module, "word", word, "message", m.ID)
			dec.RejectReason = strPtr("all-digit word")
		default:
			asc := wordmatch.WordValid(fw)
			if !asc && !unicodeVerdictShape(fw) {
				// Neither arm: not a plausible word shape at all.
				slog.Warn("derpies invalid verdict word — not learning", "module", module, "word", word, "message", m.ID)
				dec.RejectReason = strPtr("invalid word")
			} else {
				// The anchor gate (the emoji fix A): the anchor
				// requirement applies only when the POSTED message
				// carries word-like text tokens (the prompt's "the
				// message has no other text words" scope) — hasWordLikeTokens
				// is computed from the posted content ONLY. The anchor
				// check itself still uses the UNION toks (posted +
				// referenced + embed titles — the word may legitimately
				// anchor to the quoted content). Arm (b) stays
				// text-anchored on the same word-like scope: a non-ASCII
				// word needs a word-like anchor, and a word-less post has
				// none — the frame-only dead-end stays dead (ADR 0007). The
				// cost also lands on quoted-anchored non-ASCII words: a
				// word-less post that quotes a referenced message containing
				// a non-ASCII anchor gets its LEARN rejected by the
				// !asc && !hasWordLikeTokens arm even though the word is
				// genuinely anchored in the quote (the ASCII arm would pass).
				hasWordLikeTokens := false
				for tok := range postedToks {
					if wordLike(tok) {
						hasWordLikeTokens = true
						break
					}
				}
				if hasWordLikeTokens && !toks[fw] {
					slog.Warn("derpies verdict word not in the message — not learning", "module", module, "word", word, "message", m.ID)
					dec.RejectReason = strPtr("verdict word not in message")
				} else if !asc && !hasWordLikeTokens {
					slog.Warn("derpies non-ASCII verdict word not anchored to the message — not learning", "module", module, "word", word, "message", m.ID)
					dec.RejectReason = strPtr("verdict word not in message")
				} else {
					// A hallucinated word can never enter the list: a pass
					// means the word is a folded token of the judged text
					// (or a word-less post, bounded by the shape arm alone
					// — the message is being filtered, so a wrong word can
					// only delete the gated user's own future message
					// containing that word). Learn the FOLDED word (ASCII
					// words stay pure-ASCII; no-confusable non-ASCII words
					// fold to themselves and are matched by the fast path
					// on exact token) so the next occurrence is a fast hit.
					// Record the verdict word always (it passed the gate);
					// mark it learned only when the insert succeeds — a failed
					// insert is recorded separately (reject_reason) so the
					// decision log never reports a failed learn as successful.
					dec.Word = &fw
					if err := h.store.addGimmick(ctx, fw, SourceLLM); err != nil {
						slog.Error("derpies add gimmick failed", "module", module, "word", fw, "error", err)
						dec.RejectReason = strPtr("learn failed")
					} else {
						dec.Learned = true
					}
				}
			}
		}
	}

	// 10. Delete (independent of the learn), score >= T. A delete failure
	//     is LOG ONLY — a word learned in step 9 stays learned (the next
	//     occurrence is a fast hit); the failure overrides any word-
	//     rejection reason on the decision row.
	if score >= t {
		if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
			slog.Error("derpies delete (llm) failed", "module", module, "score", score, "channel", m.ChannelID, "message", m.ID, "error", err)
			dec.Deleted = false
			dec.RejectReason = strPtr("delete failed")
		} else {
			slog.Info("derpies delete (llm)", "module", module, "score", score, "channel", m.ChannelID, "message", m.ID)
			dec.Deleted = true
		}
	}
}
