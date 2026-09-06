// Package derpies is the other-side filter for the user(s) in
// config.Config.DerpiesUserIDs: every message they post is first checked
// against the derpies_gimmicks word list (fast path — exact token match);
// a miss falls through to a pi RPC verdict (the slow path) that judges
// respellings / fresh gimmicks and learners new words into the list at
// runtime (a GIMMICK:<word> verdict learns <word> with source 'llm' and
// deletes the message).
//
// Degradation discipline (mirroring the mention feature): every failure
// arm logs and stops — the flow never acts on a half-loaded list, and a
// delete failure after a successful learn keeps the word learned.
package derpies

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

const (
	FeatureKey = "derpies"
	SourceSeed = "seed"
	SourceLLM  = "llm"

	module = "derpies" // slog module tag
)

// Derpies handles the derpies flow (feature gate → guild guard →
// author-ID gate → fast-path token match → pi RPC verdict).
type Derpies struct {
	app *app.App
	// New() wires the production store/ops. Tests (same package) assign
	// the fakes directly, mirroring mention_test.go.
	store store
	ops   discordOps
}

// New builds the handler from the shared *app.App (mirrors the mention
// package's constructor).
func New(a *app.App) *Derpies {
	return &Derpies{app: a, store: &poolStore{pool: a.Pool}, ops: &realOps{d: a.D}}
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
}

type discordOps interface {
	// deleteMessage — the flow's ONLY outgoing REST call.
	deleteMessage(channelID, messageID string) error
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

// realOps is the production Discord REST surface (the flow's single
// outgoing REST call).
type realOps struct{ d *discordgo.Session }

func (o *realOps) deleteMessage(channelID, messageID string) error {
	return o.d.ChannelMessageDelete(channelID, messageID)
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// punctTrim is the edge-punctuation set trimmed from each token before the
// exact match (a trailing "sw1ft." must hit "sw1ft"). Split in two consts
// because a raw string cannot contain the backtick inside it cleanly.
const punctA = `!"#$%&()*+,-./:;<=>?@[]^_`
const punctB = "`{|}~"

var punctTrim = punctA + punctB

// tokensForMatch: lowercase the content, strings.Fields, trim leading and
// trailing punctuation off each token; keys of the result map.
// "Who's giving me a sw1ft." -> {who's, giving, me, a, sw1ft}.
func tokensForMatch(content string) map[string]bool {
	tokens := strings.Fields(strings.ToLower(content))
	out := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		out[strings.Trim(tok, punctTrim)] = true
	}
	return out
}

// wordValid: ^[a-z0-9]{2,32}$ — token charset only (no punctuation, no
// unicode), 2..32 chars. Precompiled regexp.
var wordRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

func wordValid(w string) bool {
	return wordRe.MatchString(w)
}

// parseVerdict: scan the lines of the pi response, take the FIRST non-empty
// (trimmed) line; "clean" (case-insensitive, exact) -> ("clean", "");
// prefix "gimmick:" (case-insensitive) -> ("gimmick", remainder trimmed and
// lowercased); anything else, including "GIMMICK" WITHOUT the colon, ->
// ("unknown", "").
func parseVerdict(text string) (kind, word string) {
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		lower := strings.ToLower(l)
		if lower == "clean" {
			return "clean", ""
		}
		if strings.HasPrefix(lower, "gimmick:") {
			return "gimmick", strings.ToLower(strings.TrimSpace(strings.TrimPrefix(lower, "gimmick:")))
		}
		return "unknown", ""
	}
	return "unknown", ""
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

// gimmickPrompt: the prompt below, with the content substituted and the
// KNOWN gimmick words injected (sorted, one per line). The LLM judges the
// residue the fast path missed: the list lets it pattern-match respellings
// against the known family AND recognize fresh gimmicks in the same roster
// style (the roster rotates, so old anchor words stay relevant). The pi RPC
// always appends the anti-injection system fallback on top of this.
func gimmickPrompt(content string, known []string) string {
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[k] = true
	}
	return "A Discord message was just posted by a user with a " +
		"documented history of spamming this server with a ROTATING " +
		"ROSTER of repetitive annoying gimmicks, and of evading the " +
		"word filters built against them via respellings. He is " +
		"notorious for this.\n\n" +
		"<<<UNTRUSTED MESSAGE\n" +
		content +
		"\nUNTRUSTED MESSAGE>>>\n\n" +
		"His gimmicks are short, repetitive solicitations he posts over " +
		"and over. Example from the roster: trying to get other users " +
		"to buy HIM a Zwift subscription, or to give him a free " +
		"bicycle. The roster rotates — old gimmicks come back — so " +
		"the known-word list below spans EVERY past gimmick, not just " +
		"the current one.\n\n" +
		"Known gimmick words (each was the anchor word of a past " +
		"gimmick; respellings of them are how he dodges the fast " +
		"filter):\n" +
		strings.Join(sortedKeys(set), "\n") +
		"\n\n" +
		"Judge the message. Is it (a) a respelling of a known gimmick " +
		"word, (b) another instance of a known gimmick that dodged the " +
		"filter some other way, or (c) a FRESH gimmick — a new " +
		"repetitive solicitation in the same style as the roster? " +
		"Reply with EXACTLY one line, one of:\n" +
		"  GIMMICK:<word>\n" +
		"  CLEAN\n" +
		"where <word> is the anchor word: the respelled known word for " +
		"(a)/(b), or the single most distinctive word of the fresh " +
		"gimmick for (c) (lowercase, no punctuation, must appear in the " +
		"message).\n"
}

// ---------------------------------------------------------------------------
// The flow
// ---------------------------------------------------------------------------

// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
// Burst amplification: there is no per-author coalescing or cooldown — N novel
// posts from a filtered user yield N serialized pi asks (the pi RPC queue is shared with the mention handler); rate limiting is out of scope per the spec.
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

	// 4. Fast path: one list SELECT; exact token match.
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
