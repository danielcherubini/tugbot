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
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/unicode/norm"

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
	// promptText — the live prompt template (derpies_prompt, one row;
	// per-message fetch like the flag and the list). Row absent / any
	// error -> error — the flow's fallback engages (code default), so
	// the filter never runs with a broken prompt.
	promptText(ctx context.Context) (string, error)
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

func (p *poolStore) promptText(ctx context.Context) (string, error) {
	var body string
	err := p.pool.QueryRow(ctx, `SELECT body FROM derpies_prompt LIMIT 1`).Scan(&body)
	return body, err
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

// foldToASCII lowers and reduces a token to ASCII: NFD decompose, drop
// combining marks (Mn). "świft" -> "swift", "žwift" -> "zwift". Letters
// that don't decompose to a base (e.g. Cyrillic lookalikes) stay — they
// remain wordValid-ineligible and are only catchable via the LLM naming an
// ASCII form.
func foldToASCII(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// tokensForMatch: fold each fielded token (foldToASCII — which includes the
// lowercasing), trim leading and trailing punctuation off each FOLDED
// token; keys of the result map are the folded tokens.
// "Who's giving me a sw1ft." -> {who's, giving, me, a, sw1ft} (ASCII —
// unchanged). "A świft cog" -> {a, swift, cog}.
func tokensForMatch(content string) map[string]bool {
	tokens := strings.Fields(content)
	out := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		out[strings.Trim(foldToASCII(tok), punctTrim)] = true
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

// defaultPromptTemplate — the code-pinned default (exact text of the plan's
// "defaultPromptTemplate" block: from "A Discord message was just posted by
// a user…" through the "…the innocent reading is obvious." protocol block).
// It is the fallback the flow uses when the DB template is missing/invalid —
// the filter never runs with a broken prompt.
const defaultPromptTemplate = `A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of short, repetitive, annoying gimmicks — and of evading, over and over, the word filters built to catch them. He is notorious for this.

HE WILL TEST THIS FILTER. Every message you judge from him is a probe: he actively measures what gets through, and the respellings in his posts are his evasions, not typos to forgive. Your stance is adversarial, not polite: when a message carries ANY trace of the roster — respelled, bent, squeezed, split, quoted, or dressed up as a question — judge it a GIMMICK. Judge CLEAN only when there is NO trace of the roster at all AND a plainly innocent reading is obvious. For this user a false negative (a gimmick getting through) is the worse error. When you are torn between the two: GIMMICK. His messages are the filter's only queue, so err toward catching the roster, never toward letting it through.

{content}

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

{{IMAGES}}
{{REF}}

His gimmicks are short, repetitive solicitations he posts over and over. Example from the roster: trying to get other users to buy HIM a Zwift subscription, or to give him a free bicycle. The roster rotates — old gimmicks come back — so the known-word list below spans EVERY past gimmick, not just the current one.

Known gimmick words (each was the anchor word of a past gimmick; respellings of them are how he dodges the fast filter):
{known}

Judgement rules (these override politeness):
- A known word or any respelling of one — even when the surrounding text looks mildly innocent — is GIMMICK.
- A known word hidden inside another word, written in non-English letters, or shot full of punctuation and dashes is GIMMICK — dressing does not launder the word.
- An anchor word embedded inside a squeeze/blend is GIMMICK; the anchor word is the most distinctive token of the blend AS IT APPEARS.
- If you have to imagine an innocent reading to call it CLEAN, you are probably wrong — he is very good at making solicitations look like questions.
- When you are torn: GIMMICK.

Reply with EXACTLY one line, one of:
  GIMMICK:<word>
  CLEAN
where <word> is the anchor word: the as-appears respelled token for a known-gimmick trace, or the single most distinctive word of the fresh gimmick. The rules for <word>:
- It MUST be a token of the message text AS IT APPEARS (case and edge punctuation aside; ignore unicode bent — you SHOULD judge "žwift" to be "zwift").
- For a respelling, answer the respelled token AS IT APPEARS. NEVER answer the base/known word unless that base token itself appears in the message text — for "zwift" the answer is "zwift"; "GIMMICK:swift" for it is the INVALID answer. Never answer a known word that is not in the message.
- When the anchor word lives ONLY in an image, answer the most distinctive word of that image as if it were in the message.
- CLEAN only when the message carries NO trace of the roster at all and the innocent reading is obvious.`

// validTemplate: the two MANDATORY literal markers are present. Absent
// optional markers ({{IMAGES}} / {{REF}}) are fine — the element is simply
// omitted.
func validTemplate(t string) bool {
	return strings.Contains(t, "{content}") && strings.Contains(t, "{known}")
}

// gimmickPrompt substitutes the FOUR markers with a TWO-PHASE pass so a
// payload can never re-trigger a later marker scan. Pass 1 runs on the
// TEMPLATE ONLY (before any payload exists): each marker becomes a unique
// inert placeholder wrapped in NUL bytes. Pass 2 swaps the placeholders
// for the real payloads. The payloads are NUL-free — content / refText are
// Discord message text (Discord content cannot contain U+0000), and known
// words pass wordValid's ^[a-z0-9]{2,32}$ charset — so a message that
// literally contains "{known}" / "{{IMAGES}}" / "{{REF}}" survives
// verbatim instead of pulling the known block inside the untrusted fence
// or having its marker bytes silently deleted (the single-pass ReplaceAll
// ordering this replaces re-scanned the already-inserted content). The
// fence and the images line / referenced block bytes are code-pinned — the
// template carries only the bare markers.
// (`known` arrives sorted from the flow — sortedKeys — and is joined one
// per line; the pi RPC always appends the anti-injection system fallback on
// top of this.)
func gimmickPrompt(tmpl string, content string, known []string, nImages int, refText string) string {
	// Pass 1: markers -> NUL-wrapped placeholders, template only.
	marked := tmpl
	marked = strings.ReplaceAll(marked, "{content}", "\x00CONTENT\x00")
	marked = strings.ReplaceAll(marked, "{known}", "\x00KNOWN\x00")
	marked = strings.ReplaceAll(marked, "{{IMAGES}}", "\x00IMAGES\x00")
	marked = strings.ReplaceAll(marked, "{{REF}}", "\x00REF\x00")

	// Pass 2: placeholders -> payloads (all NUL-free, so no re-trigger).
	out := strings.ReplaceAll(marked, "\x00CONTENT\x00",
		"\n<<<UNTRUSTED MESSAGE\n"+content+"\n               UNTRUSTED MESSAGE>>>\n")
	knownBlock := ""
	if len(known) > 0 {
		knownBlock = "-----< known gimmick words (sorted ascending) >-----\n" + strings.Join(known, "\n")
	}
	out = strings.ReplaceAll(out, "\x00KNOWN\x00", knownBlock)
	var imagesBlock string
	if nImages > 0 {
		imagesBlock = fmt.Sprintf("The message also has %d attached image(s) (screenshots or pasted images — a text filter would not see their content). Judge the text AND the images. If the anchor word appears in an image rather than the message text, name it as if it were in the message.", nImages)
	}
	out = strings.ReplaceAll(out, "\x00IMAGES\x00", imagesBlock)
	var refBlock string
	if refText != "" {
		refBlock = "<<<REFERENCED MESSAGE\n" + refText + "\nREFERENCED MESSAGE>>>\nThe message replies to a previous message (often the author's own) — the quoted content is above between the REFERENCED MESSAGE markers. Judge the posted text / images AND the quoted content together; a respelling may live in the quote rather than the new message."
	}
	return strings.ReplaceAll(out, "\x00REF\x00", refBlock)
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

	// 6-7. One ask (the 300s deadline lives in the pi package). The template
	//     comes from the DB seam — a fetch error OR an invalid row (missing
	//     mandatory marker) falls back to the code default so the filter
	//     never runs with a broken prompt. (This plan's build has no
	//     images/referenced input — the call is the 0/"" form; the pending
	//     images plan re-pins this block when it lands.)
	tmpl, err := h.store.promptText(ctx)
	if err != nil || !validTemplate(tmpl) {
		tmpl = defaultPromptTemplate
		if err != nil {
			slog.Warn("derpies prompt template unavailable — using default", "module", module, "error", err)
		}
	}
	prompt := gimmickPrompt(tmpl, m.Content, sortedKeys(list), 0, "")
	text, askErr := h.app.Pi.Ask(ctx, prompt)
	if askErr != nil {
		slog.Error("derpies pi ask failed", "module", module, "error", askErr)
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

	// 9. SANITY before learning: charset/length — judged on the FOLDED
	//    verdict word (a unicode respelling folds to its ASCII base, so
	//    "GIMMICK:žwift" evaluates as "zwift") — AND the folded word must
	//    have appeared as a folded token of the message (same tokenization as the fast path — unicode
	//    respellings in the text fold identically) — the SHIPPED gate shape
	//    holds, no form change: `wordValid` then token-in-message, both now
	//    over FOLDED values. A hallucinated word can never enter the list.
	fw := foldToASCII(word)
	if !wordValid(fw) {
		slog.Warn("derpies invalid verdict word — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}
	if !toks[fw] {
		slog.Warn("derpies verdict word not in the message — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}

	// 10. Learn, then delete. Learn the FOLDED word (the list is a
	//     pure-ASCII token space — the fast path's tokens fold identically,
	//     so the next occurrence of the respelling is a fast hit). A delete
	//     failure is LOG ONLY — the word was actually used and stays learned
	//     (the next occurrence is a fast hit).
	if err := h.store.addGimmick(ctx, fw, SourceLLM); err != nil {
		slog.Error("derpies add gimmick failed", "module", module, "word", fw, "error", err)
	}
	if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
		slog.Error("derpies delete (llm) failed", "module", module, "word", fw, "channel", m.ChannelID, "message", m.ID, "error", err)
	} else {
		slog.Info("derpies delete (llm) learned", "module", module, "word", fw, "channel", m.ChannelID, "message", m.ID)
	}
}
