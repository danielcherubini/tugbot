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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
	"github.com/danielcherubini/tugbot/internal/wordmatch"
)

const (
	FeatureKey = "derpies"
	SourceSeed = "seed"
	SourceLLM  = "llm"

	module = "derpies" // slog module tag
)

// Derpies handles the derpies flow (feature gate → guild guard →
// author-ID gate → fast-path token match → pi RPC verdict). It also
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
	nickMu   sync.Mutex // guards the four maps (lastNick, lastEdit, busy, seenImages)
	// seenImages keys sha256(image content) → last-judged time; the
	// repeat-image fast path (flow 4.6) — a message whose posted content
	// is image-only and whose images were all already LLM-judged is a
	// re-post: the repetition is the gimmick, delete without an ask.
	seenImages map[string]time.Time
}

const (
	repeatImageTTL = 24 * time.Hour
	repeatImageMax = 2048 // bounded cache: evict the oldest on overflow
)

// New builds the handler from the shared *app.App (mirrors the mention
// package's constructor). Initializes ALL nickname-flow state: the maps
// are keyed by guild+member and the first live event assigns into them,
// so a nil map here would panic on the first live event (the selftest
// constructs the handler but never dispatches events, so no gate would
// catch it).
func New(a *app.App) *Derpies {
	return &Derpies{app: a, store: &poolStore{pool: a.Pool}, ops: &realOps{d: a.D},
		clock: time.Now, lastNick: map[string]string{},
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

// punctTrim is the edge-punctuation set trimmed from each token before the
// exact match (a trailing "sw1ft." must hit "sw1ft"). Split in two consts
// because a raw string cannot contain the backtick inside it cleanly.
const punctA = `!"#$%&()*+,-./:;<=>?@[]^_`
const punctB = "`{|}~"

var punctTrim = punctA + punctB

// tokensForMatch: fold each fielded token (foldToASCII — which includes the
// lowercasing), trim leading and trailing punctuation off each FOLDED
// token; keys of the result map are the folded tokens.
// "Who's giving me a sw1ft." -> {who's, giving, me, a, sw1ft} (ASCII —
// unchanged). "A świft cog" -> {a, swift, cog}.
func tokensForMatch(content string) map[string]bool {
	tokens := strings.Fields(content)
	out := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		out[strings.Trim(wordmatch.FoldToASCII(tok), punctTrim)] = true
	}
	return out
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
// png→image/png, gif→image/gif, webp→image/webp, else image/jpeg). Every url
// must pass isSafeURL.
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
// the status carries) — mirror mention EXACTLY.
func (h *Derpies) downloadPlan(ctx context.Context, plan []imagePlanEntry, client *http.Client) []app.PiImage {
	var images []app.PiImage
	for _, entry := range plan {
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
		images = append(images, app.PiImage{
			MimeType: entry.mime,
			Data:     base64.StdEncoding.EncodeToString(body),
		})
	}
	return images
}

// downloadImages mirrors mention's one-message shape: plan then download.
func (h *Derpies) downloadImages(ctx context.Context, m *discordgo.Message, client *http.Client) []app.PiImage {
	return h.downloadPlan(ctx, imageURLPlan(m), client)
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

// imageHash — the sha256 of the base64 payload (the same comparison
// basis as pirpc's per-ask dedupe, so a repeat of an image whose ask
// payload was deduped still matches the content the model saw).
func imageHash(im app.PiImage) string {
	sum := sha256.Sum256([]byte(im.Data))
	return hex.EncodeToString(sum[:])
}

// seenImage reports whether this content hash was already LLM-judged
// within the TTL window (the cache is LLM-judged content, not merely
// seen content: an ask failure marks nothing, so a re-post of an
// UNjudged image is judged — never fast-deleted blind).
func (h *Derpies) seenImage(hash string) bool {
	h.nickMu.Lock()
	defer h.nickMu.Unlock()
	t, ok := h.seenImages[hash]
	if !ok {
		return false
	}
	if time.Since(t) > repeatImageTTL {
		delete(h.seenImages, hash)
		return false
	}
	return true
}

// markImagesSeen records content hashes as LLM-judged (flow 4.6, called
// exactly once after a completed ask, on every verdict leg).
func (h *Derpies) markImagesSeen(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	now := time.Now()
	if h.clock != nil {
		now = h.clock()
	}
	h.nickMu.Lock()
	defer h.nickMu.Unlock()
	if h.seenImages == nil {
		h.seenImages = make(map[string]time.Time)
	}
	for _, hs := range hashes {
		h.seenImages[hs] = now
	}
	if len(h.seenImages) > repeatImageMax {
		var oldest string
		var oldestT time.Time
		for k, t := range h.seenImages {
			if oldest == "" || t.Before(oldestT) {
				oldest, oldestT = k, t
			}
		}
		delete(h.seenImages, oldest)
	}
}

// MessageCreate spawns the goroutine (the flow can block up to the pi
// RPC's 300s ask deadline; the event thread is never held).
// Burst amplification: there is no per-author coalescing or cooldown — N novel
// posts from a filtered user yield N serialized pi asks (the pi RPC queue is shared with the mention handler); rate limiting is out of scope per the spec.
func (h *Derpies) MessageCreate(m *discordgo.Message) { go h.flow(m, false) }

// flow — the full message flow (gates → fast path → images/repeat →
// slow path → learn/delete). `isEdit` distinguishes the origin: the
// create path passes false, the edit path (edits.go) passes true. The
// ONLY behavioral difference is flow 4.6's pure-repeat delete arm, which
// an EDIT must never trigger: on an edit, an all-seen image-only state
// is a content-REMOVING change of a previously-judged message (the LLM
// already cleared those images), so it returns without deleting or
// asking instead of treating the repetition as the gimmick. Everything
// else is origin-agnostic — an edit costs at most one list SELECT + one
// pi ask, same as a create.
func (h *Derpies) flow(m *discordgo.Message, isEdit bool) {
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
	//     here without retyping).
	list, err := h.store.listGimmicks(ctx)
	if err != nil {
		slog.Error("derpies gimmick list fetch failed", "module", module, "error", err)
		return
	}
	toks := tokensForMatch(m.Content)
	if referenced != nil {
		for t := range tokensForMatch(referenced.Content) {
			toks[t] = true
		}
	}
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
	images := h.downloadPlan(ctx, uniqPlan, &http.Client{Timeout: 10 * time.Second})

	// 4.6 Repeat-image fast path: an image whose CONTENT was already LLM-
	//     judged is a re-post — the repetition itself is the gimmick.
	//     Seen images are dropped from the ask (they are never re-judged);
	//     a message whose content is image-only AND whose images are all
	//     seen is the pure repeat: delete without an ask. A message with
	//     new text still gets its (image-free) text judged — the seen
	//     images just drop out of the payload. The word-fast path (step 4)
	//     already handled text hits before this point.
	var freshImages []app.PiImage
	seenImgCount := 0
	for _, im := range images {
		if h.seenImage(imageHash(im)) {
			seenImgCount++
			continue
		}
		freshImages = append(freshImages, im)
	}
	if seenImgCount > 0 {
		if len(freshImages) == 0 {
			postedTextTokens := false
			for range tokensForMatch(m.Content) {
				postedTextTokens = true
				break
			}
			if !postedTextTokens {
				if isEdit {
					// EDIT path: the all-seen image-only state must never
					// reach the pure-repeat delete. An edit to that state
					// REMOVED content (e.g. the caption dropped from a
					// previously-judged message) — the seen images were
					// already LLM-judged, so there is nothing fresh to
					// judge: return without deleting and without asking (a
					// clean no-op; the message stays).
					slog.Info("derpies edit all-seen image-only — no-op", "module", module, "channel", m.ChannelID, "message", m.ID, "seen", seenImgCount)
					return
				}
				// The pure repeat (image-only, all content previously
				// LLM-judged): the repetition is the gimmick — delete
				// without an ask.
				if err := h.ops.deleteMessage(m.ChannelID, m.ID); err != nil {
					slog.Error("derpies delete (repeat image) failed", "module", module, "channel", m.ChannelID, "message", m.ID, "seen", seenImgCount, "error", err)
				} else {
					slog.Info("derpies delete (repeat image)", "module", module, "channel", m.ChannelID, "message", m.ID, "seen", seenImgCount)
				}
				return
			}
			// New text with all-images-seen: the text is still judged,
			// now image-free (the seen images dropped out).
		}
		slog.Info("derpies skipping seen image(s) from the ask", "module", module, "message", m.ID, "seen", seenImgCount)
		images = freshImages
	}

	// 5. Slow path: pi unavailable -> silent return (the mention feature's
	//    degradation path, same shape).
	if h.app.Pi == nil {
		slog.Info("derpies pi RPC not available, skipping", "module", module)
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
	prompt := gimmickPrompt(tmpl, m.Content, sortedKeys(list), len(images), refContent)
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
		return
	}
	// Judged (a completed ask, every verdict leg — a failed ask marked
	// nothing): record the FRESH image contents as seen (flow 4.6's
	// repeat detection). Seen images were dropped earlier and need no
	// re-mark.
	for _, im := range images {
		h.markImagesSeen([]string{imageHash(im)})
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

	// 9. SANITY before learning: charset/length — on the FOLDED verdict word
	//    (shipped form) — AND, when the (union of posted + referenced) text
	//    has tokens, the folded word must have appeared as a folded token of
	//    that text (same tokenization as the fast path). A message with NO
	//    text tokens at all (image-only, or empty text with an empty/absent
	//    reference) is bounded by wordValid alone: the verdict word may come
	//    from image text (the message is being filtered — a wrong word can
	//    only delete the gated user's own future message containing that
	//    word). A hallucinated word can never enter the list.
	fw := wordmatch.FoldToASCII(word)
	if !wordmatch.WordValid(fw) {
		slog.Warn("derpies invalid verdict word — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}
	hasTextTokens := false
	for tok := range toks {
		if tok != "" {
			hasTextTokens = true
			break
		}
	}
	if hasTextTokens && !toks[fw] {
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
