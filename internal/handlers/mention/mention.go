// Package mention is the Go port of the Rust bot's
// src/handlers/mention.rs (553 lines) — the bot's core LLM mention loop:
// feature gate, bot-mention / guild / #ask-tugbot channel guards, the
// slow-user auto-gulag (fired BEFORE question extraction), question
// extraction, referenced-message fetch, 5m/2h cooldown, the 👀 then 🤔
// reactions, image download (attachment vs embed MIME sourcing, safe-URL
// validation, dedupe), the pi ask (300s timeout lives in the pi package),
// trim-then-check-empty response handling, and the delivered-only cooldown
// write-back.
//
// The Rust source numbers its own flow in comments (steps 1-15); this port
// preserves Rust's numbering AND order in flow(). The behavior points are
// pinned in docs/parity/checklist.md (the mention section).
//
// Error-path discipline (per repo convention): the flow's failure arms log
// and continue, mirroring Rust's `if let Err(e) { eprintln!(...) }`;
// when a Discord 404 has to be distinguished (not needed on this handler's
// own arms, but part of the shared discipline), errors.As on
// *discordgo.RESTError is the path — never string-matching err.Error().
// Every HTTP client used here carries an explicit Timeout (10s for image
// downloads, as in Rust); Discord IDs are int64 with checked conversions
// (core.DiscordID) at the DB boundary.
package mention

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// Pinned constants (Rust's const block, values pinned; the channel ID is the
// u64 literal 1515343076401479790 in Rust — Discord IDs are strings in the
// pinned discordgo v0.29.0 API).
const (
	// FeatureKey: the DB key is still "is_this_real" for backward compat
	// (Rust comment: rename via migration later).
	FeatureKey = "is_this_real"

	// SlowUserAutoGulagFeatureKey (Rust: SLOW_USER_AUTO_GULAG_FEATURE —
	// default OFF via migration 2026-06-13-200000).
	SlowUserAutoGulagFeatureKey = "slow_user_auto_gulag"

	// AskTugbotChannelID — #ask-tugbot (Rust: ASK_TUGBOT_CHANNEL_ID, the
	// u64 literal 1515343076401479790, pinned). The bot must NOT answer
	// mentions elsewhere in the guild.
	AskTugbotChannelID = "1515343076401479790"

	// CooldownSeconds (Rust: COOLDOWN_SECS) — 5m between uses.
	CooldownSeconds = 300

	// SlowCooldownSeconds (Rust: SLOW_COOLDOWN_SECS) — 2h between uses.
	SlowCooldownSeconds = 7200

	// GulagDurationSeconds (Rust: GULAG_DURATION_SECS) — 5 minutes.
	GulagDurationSeconds = 300
)

// Message / cooldown text, byte-for-byte with Rust.
const (
	emptyQuestionMessage = "You mentioned me but didn't ask anything — what's up?"
	piFailureMessage     = "I'm having trouble thinking right now, try again later"
	// slowUserGulagMessage is the slow-user auto-gulag channel message,
	// formatted per Rust (the mention carries the <@ID>).
	slowUserGulagMessage = "%s wanted to know if something was real... now they're in the gulag for 5m. Irony."
)

// Mention handles the bot-mention flow in #ask-tugbot (flow() in Rust's
// step order).
type Mention struct {
	app *app.App

	// New() wires the production implementations (features/pgx pool for
	// the DB surface, *discordgo.Session for the REST surface, the Task-4
	// gulag core for the step-5 auto-gulag). Tests inject fakes; the
	// behavior is identical either way.
	store mentionStore
	ops   discordOps
	core  *core.Gulag
}

// New builds the handler from the shared *app.App (mirrors Rust's
// TypeMap + get_pool / get_config wiring). Pins the canonical Task-4 Gulag
// core for the step-5 auto-gulag path — never re-declares any of its
// helpers.
func New(a *app.App) *Mention {
	return &Mention{
		app:   a,
		store: &poolStore{pool: a.Pool},
		ops:   &realOps{d: a.D},
		core:  core.New(a),
	}
}

// ---------------------------------------------------------------------------
// Dependency seams (production: features/pgx + discordgo; tests: fakes)
// ---------------------------------------------------------------------------

// mentionStore is this handler's DB surface (Rust: get_is_this_real_usage /
// get_or_create_is_this_real_usage / update_is_this_real_usage + the
// feature flags).
type mentionStore interface {
	// featureEnabled — silent flavor (Rust's Features::is_enabled: false on
	// any error, including a missing row).
	featureEnabled(ctx context.Context, key string) bool
	// usageLastUsed — Rust's get_is_this_real_usage: (zero time, false)
	// when there is no row (no cooldown gate applies to a first-time user);
	// a DB error behaves the same way (Rust's `.optional()` drops it to
	// None on error).
	usageLastUsed(ctx context.Context, userID, guildID int64) (time.Time, bool)
	// usageReset — Rust step 15: get_or_create the row, then
	// update_is_this_real_usage (last_used_at = now()). A get_or_create
	// failure is silently skipped (Rust's `if let Ok(...)`); only an
	// UPDATE failure surfaces.
	usageReset(ctx context.Context, userID, guildID int64) error
}

// discordOps is this handler's Discord REST surface (the pinned
// bwmarrin/discordgo v0.29.0 has hardcoded endpoint URLs — no base-URL
// override — so the session-driven surface is mirrored with seams for
// unit tests).
type discordOps interface {
	reactionAdd(channelID, messageID, emoji string) error
	reactionRemove(channelID, messageID, emoji string) error
	channelMessageSend(channelID, content string) error
	channelMessageReply(channelID, messageID, content string) error
	channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error)
}

type realOps struct{ d *discordgo.Session }

func (o *realOps) reactionAdd(c, m, e string) error {
	return o.d.MessageReactionAdd(c, m, e)
}

// reactionRemove deletes the BOT's reaction (Rust: delete_reaction with
// Some(bot_user.id)); "@me" is the equivalent in the pinned API.
func (o *realOps) reactionRemove(c, m, e string) error {
	return o.d.MessageReactionRemove(c, m, e, "@me")
}

func (o *realOps) channelMessageSend(c, content string) error {
	_, err := o.d.ChannelMessageSend(c, content)
	return err
}

func (o *realOps) channelMessageReply(c, messageID, content string) error {
	_, err := o.d.ChannelMessageSendReply(c, content, &discordgo.MessageReference{MessageID: messageID})
	return err
}

func (o *realOps) channelMessageRetrieve(c, messageID string) (*discordgo.Message, error) {
	return o.d.ChannelMessage(c, messageID)
}

// poolStore is the production mentionStore (raw SQL, byte-identical to the
// Task-1 sqlc files: select_is_this_real_usage, insert_is_this_real_usage,
// update_is_this_real_usage_last_used).
type poolStore struct {
	pool *pgxpool.Pool
}

func (p *poolStore) featureEnabled(ctx context.Context, key string) bool {
	return features.IsEnabled(ctx, p.pool, key)
}

func (p *poolStore) usageLastUsed(ctx context.Context, userID, guildID int64) (time.Time, bool) {
	var last pgtype.Timestamp
	err := p.pool.QueryRow(ctx,
		`SELECT last_used_at FROM is_this_real_usage WHERE user_id = $1 AND guild_id = $2`,
		userID, guildID).Scan(&last)
	if err != nil {
		// Rust: `.optional()` — NotFound OR DB error both yield None
		// (no gate) at the step-8 call site.
		return time.Time{}, false
	}
	return last.Time, true
}

func (p *poolStore) usageReset(ctx context.Context, userID, guildID int64) error {
	var id int32
	err := p.pool.QueryRow(ctx,
		`SELECT id FROM is_this_real_usage WHERE user_id = $1 AND guild_id = $2`,
		userID, guildID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var insertErr error
		id, insertErr = insertIsThisRealUsage(ctx, p.pool, userID, guildID)
		if insertErr != nil {
			// Rust: `if let Ok(u) = usage_result` — skip silently.
			return nil
		}
	} else if err != nil {
		// Rust: `if let Ok(u) = usage_result` — skip silently.
		return nil
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE is_this_real_usage SET last_used_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	return nil
}

func insertIsThisRealUsage(ctx context.Context, pool *pgxpool.Pool, userID, guildID int64) (int32, error) {
	var id int32
	err := pool.QueryRow(ctx,
		`INSERT INTO is_this_real_usage (user_id, guild_id, last_used_at, created_at) VALUES ($1, $2, now(), now()) RETURNING id`,
		userID, guildID).Scan(&id)
	return id, err
}

// ---------------------------------------------------------------------------
// The flow (Rust's numbered order, preserved)
// ---------------------------------------------------------------------------

// MessageCreate runs on every message create (Task 7 wires it AFTER teh /
// twitter / bsky / instagram, per Rust mod.rs dispatch order). Rust's
// handler is async on the tokio executor; the Go event path is a single
// loop, so the flow runs in a goroutine (convention: no blocking calls on
// the event path — the pi ask can take the full 300s).
func (h *Mention) MessageCreate(m *discordgo.Message) {
	go h.flow(m)
}

// flow is the port of Rust `Mention::handler`, preserving the numbered
// comments as Go comments (Rust lines 74-410).
func (h *Mention) flow(m *discordgo.Message) {
	ctx := context.Background()

	// 1. Feature flag check
	// Note: DB key is still "is_this_real" for backward compat — rename
	// via migration later
	if !h.store.featureEnabled(ctx, FeatureKey) {
		return
	}
	slog.Info("Handler called by", "module", "mention", "author", m.Author.ID, "guild", m.GuildID)

	// 2. Bot mention check — the API-provided mention list
	// (discordgo v0.29.0: Message.Mentions, populated by the gateway),
	// NEVER a string scrape of the raw content.
	botID, err := h.botUserID()
	if err != nil {
		slog.Error("Failed to get current user", "module", "mention", "error", err)
		return
	}
	if !mentionsBot(m, botID) {
		return
	}

	// 3. Guild ID check (needed for special user)
	if m.GuildID == "" {
		return
	}

	// 4. Channel restriction — only respond to mentions in #ask-tugbot
	if m.ChannelID != AskTugbotChannelID {
		return
	}

	// 5. Config — slow_user_ids only affects the per-user cooldown (longer
	//    cooldown for throttled users). The auto-gulag-on-mention behavior
	//    is gated by the `slow_user_auto_gulag` feature flag (default off
	//    via migration 2026-06-13-200000). When the flag is enabled and
	//    the user is in SLOW_USER_IDS, any mention gulags them. Fires
	//    BEFORE question extraction: flag-on + empty question → gulag, not
	//    the empty-question path.
	userID, err := core.DiscordID("user", m.Author.ID)
	if err != nil {
		slog.Error("Failed to convert user ID", "module", "mention", "error", err)
		return
	}
	guildID, err := core.DiscordID("guild", m.GuildID)
	if err != nil {
		slog.Error("Failed to convert guild ID", "module", "mention", "error", err)
		return
	}
	_, slow := h.app.Cfg.SlowUserIDs[userID]
	_, exempt := h.app.Cfg.CooldownExemptUserIDs[userID]
	if slow && h.store.featureEnabled(ctx, SlowUserAutoGulagFeatureKey) {
		h.slowUserAutoGulag(ctx, guildID, m)
		return
	}

	// 6. Extract question — strip bot mentions by tokenizing on whitespace
	//    and filtering out anything that looks like <@...> matching the
	//    bot ID. This handles <@ID>, <@!ID>, and avoids any
	//    false-positive replace() matches if a user types text
	//    containing "<@".
	question := extractQuestion(m.Content, botID)
	slog.Info("Question: '"+question+"'", "module", "mention")

	if question == "" {
		// RUST sends a PLAIN (non-reply) channel message here.
		if err := h.ops.channelMessageSend(m.ChannelID, emptyQuestionMessage); err != nil {
			slog.Error("Failed to send empty question message", "module", "mention", "error", err)
		}
		return
	}

	// 7. Optional: fetch referenced message if this is a reply — Rust:
	//    `msg.message_reference.as_ref().and_then(|r| r.message_id)`. On
	//    failure: log and treat as None — do NOT abort the flow.
	var referenced *discordgo.Message
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		if ref, err := h.ops.channelMessageRetrieve(m.ChannelID, m.MessageReference.MessageID); err != nil {
			slog.Error("Failed to fetch referenced message", "module", "mention", "error", err)
		} else {
			referenced = ref
		}
	}

	// 8. Cooldown check (normal users, admin gets unlimited)
	if !exempt {
		limit := CooldownSeconds
		if slow {
			limit = SlowCooldownSeconds
		}
		if lastUse, ok := h.store.usageLastUsed(ctx, userID, guildID); ok {
			if blocking, remaining := cooldownDecision(lastUse, time.Duration(limit)*time.Second); blocking {
				// Mapping (Rust line 196): slow user → "Easy there,
				// {mention} — give it a rest for {t}"; everyone else →
				// "I'm still waking up — try again in {t}".
				msg := cooldownMessage(slow, m.Author.Mention(), formatRemaining(remaining))
				if err := h.ops.channelMessageReply(m.ChannelID, m.ID, msg); err != nil {
					slog.Error("Failed to send cooldown message", "module", "mention", "error", err)
				}
				return
			}
		}
	}

	// 9. React with 👀 to acknowledge, then 🤔 while processing (exact
	//    order, BEFORE images/ask)
	if err := h.ops.reactionAdd(m.ChannelID, m.ID, "👀"); err != nil {
		slog.Error("Failed to react", "module", "mention", "emoji", "👀", "error", err)
	} else {
		slog.Info("Reacted with :eyes:", "module", "mention")
	}
	if err := h.ops.reactionAdd(m.ChannelID, m.ID, "🤔"); err != nil {
		slog.Error("Failed to react", "module", "mention", "emoji", "🤔", "error", err)
	} else {
		slog.Info("Reacted with 🤔", "module", "mention")
	}

	// 10. Download images from referenced message if it exists (10s timeout
	//     per URL; MIME sourcing differs by source: attachments carry their
	//     own content_type (filtered on the image/ prefix); embed /
	//     thumbnail URLs use the extension→MIME map, deduped against the
	//     ATTACHMENT urls first)
	var images []app.PiImage
	if referenced != nil {
		images = h.downloadImages(ctx, referenced, &http.Client{Timeout: 10 * time.Second})
	}

	// 11. Get pi RPC — a missing backend is a SILENT return, BEFORE the
	//     error reply: the 🤔 reaction stays in place.
	if h.app.Pi == nil {
		slog.Info("pi RPC not available", "module", "mention")
		return
	}

	// 12. Build prompt — include referenced message context. The two
	//     branches are referenced-message PRESENT vs ABSENT (the
	//     slow/normal distinction NEVER branches the prompt); the
	//     referenced branch has the 4-way content × image matrix.
	refContent := ""
	if referenced != nil {
		refContent = referenced.Content
	}
	prompt := buildPrompt(m.Author.Username, referenced != nil, refContent, len(images), question)

	// 13. Ask the pi backend (the 300s timeout lives in the pi package)
	text, err := h.app.Pi.AskWithImages(ctx, prompt, images)
	if err != nil {
		slog.Error("pi RPC ask failed", "module", "mention", "error", err)
		// Remove the 🤔 (Rust: delete_reaction, error ignored).
		_ = h.ops.reactionRemove(m.ChannelID, m.ID, "🤔")
		if err := h.ops.channelMessageReply(m.ChannelID, m.ID, piFailureMessage); err != nil {
			slog.Error("Failed to send error message", "module", "mention", "error", err)
		}
		return
	}

	// 14. TRIM the response, then check empty. Whitespace-only counts as
	//     empty: REMOVE the 🤔, skip the post AND skip the cooldown
	//     write-back. Otherwise remove the 🤔 then post.
	finalText := strings.TrimSpace(text)
	if finalText == "" {
		slog.Info("pi returned empty response, skipping post and cooldown update", "module", "mention")
		_ = h.ops.reactionRemove(m.ChannelID, m.ID, "🤔")
		return
	}
	_ = h.ops.reactionRemove(m.ChannelID, m.ID, "🤔")
	slog.Info("Posting response...", "module", "mention")
	postErr := h.ops.channelMessageReply(m.ChannelID, m.ID, finalText)
	if postErr != nil {
		slog.Error("Failed to post response", "module", "mention", "error", postErr)
	} else {
		slog.Info("Response posted", "module", "mention")
	}
	postSuccess := postErr == nil

	// 15. Update cooldown only if response was delivered — skip exempt users
	if postSuccess && !exempt {
		if err := h.store.usageReset(ctx, userID, guildID); err != nil {
			slog.Error("Failed to update cooldown", "module", "mention", "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// mentionsBot ports Rust's `msg.mentions.iter().any(|m| m.id == bot_user.id)`
// (step 2): the check is on the API-provided mention list ONLY — the raw
// content is never string-scraped.
func mentionsBot(m *discordgo.Message, botID string) bool {
	if m == nil {
		return false
	}
	for _, mu := range m.Mentions {
		if mu != nil && mu.ID == botID {
			return true
		}
	}
	return false
}

// extractQuestion ports Rust steps 6 (lines 126-138): split_whitespace,
// per-token trim_start_matches("<@") → trim_start_matches('!') →
// trim_end_matches('>'), drop tokens equal to the bot ID, join, trim.
func extractQuestion(content, botID string) string {
	tokens := strings.Fields(content)
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		stripped := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(tok, "<@"), "!"), ">")
		// Rust's filter closure uses the STIPPED form only for the
		// comparison: the KEPT token is the ORIGINAL one.
		if stripped != botID {
			out = append(out, tok)
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// cooldownMessage ports the Rust line 196 mapping (the mapping, not just
// the literals): slow user → the "Easy there, {mention} — ..." variant;
// every other user → "I'm still waking up — try again in {t}".
func cooldownMessage(slow bool, mention, remaining string) string {
	if slow {
		return "Easy there, " + mention + " — give it a rest for " + remaining
	}
	return "I'm still waking up — try again in " + remaining
}

// buildPrompt ports Rust step 12 byte-for-byte. The two branches are the
// referenced-message PRESENT vs ABSENT (NOT the slow/normal distinction —
// that never branches the prompt); the referenced branch carries the
// 4-way content × image context matrix.
func buildPrompt(author string, referenced bool, refContent string, imageCount int, question string) string {
	if !referenced {
		return author + ` asked: "` + question + `"`
	}
	var ctx string
	switch {
	case refContent != "" && imageCount > 0:
		ctx = refContent + " [also shared an image]"
	case refContent == "" && imageCount > 0:
		ctx = "[shared an image (" + strconv.Itoa(imageCount) + ")]"
	case refContent != "":
		ctx = refContent
	default:
		ctx = "[replied to an image]"
	}
	return author + ` replied to: "` + ctx + `" and asked: "` + question + `"`
}

// is_safe_url — port 1:1 (Rust: http:// or https:// scheme only).
func isSafeURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// mime_for_url — port 1:1 (Rust: strip the query string and fragment first
// so URLs like image.png?v=2 don't get misclassified; the extension is the
// Rust Path extension of the last path segment — Rust does NOT lowercase,
// and a trailing-dot path has no extension).
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

// imagePlanEntry is one planned image download (step-10 plan, deterministic
// order: attachments in order, then embeds in order).
type imagePlanEntry struct {
	url    string
	mime   string
	source string // "attachment" | "embed"
}

// imageURLPlan is the port of Rust step 10's two loops, unit-testable:
//   - attachments: content_type (defaulting to "application/octet-stream"
//     when absent) filtered on the image/ prefix, then the is_safe_url
//     check (Logging the Rust eprintln text on a skip); the MIME is the
//     attachment's own content_type.
//   - embeds: the embed image url, falling back to the thumbnail url when
//     absent; deduped against the ATTACHMENT urls first (against ALL
//     attachment urls — Rust's dedupe is not filtered by content type);
//     then the is_safe_url check (Logging the 1:1 ported Rust text); the
//     MIME is the extension→MIME map.
func imageURLPlan(ref *discordgo.Message) []imagePlanEntry {
	if ref == nil {
		return nil
	}
	var plan []imagePlanEntry
	var attachmentURLs []string
	for _, a := range ref.Attachments {
		attachmentURLs = append(attachmentURLs, a.URL)
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if !strings.HasPrefix(contentType, "image/") {
			continue
		}
		if !isSafeURL(a.URL) {
			slog.Info("Skipping unsafe URL: "+a.URL, "module", "mention")
			continue
		}
		plan = append(plan, imagePlanEntry{url: a.URL, mime: contentType, source: "attachment"})
	}
	for _, e := range ref.Embeds {
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
			slog.Info("Skipping unsafe embed URL: "+url, "module", "mention")
			continue
		}
		plan = append(plan, imagePlanEntry{url: url, mime: mimeForURL(url), source: "embed"})
	}
	return plan
}

// downloadImages is the port of the Rust step-10 download leg: per-URL GET
// with the explicit-timeout client (10s), base64-encoded with the
// standard alphabet (Rust BASE64_STANDARD). Log parity: attachments log
// the content type, embeds log the url; a failed download is logged and
// that URL is skipped (the flow continues with the rest).
func (h *Mention) downloadImages(ctx context.Context, ref *discordgo.Message, client *http.Client) []app.PiImage {
	if ref == nil {
		return nil
	}
	var images []app.PiImage
	for _, entry := range imageURLPlan(ref) {
		if entry.source == "attachment" {
			slog.Info("Downloading image: "+entry.url+" ("+entry.mime+")", "module", "mention")
		} else {
			slog.Info("Downloading embed image: "+entry.url, "module", "mention")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.url, nil)
		if err != nil {
			slog.Error("Failed to download image", "module", "mention", "url", entry.url, "error", err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Error("Failed to download image", "module", "mention", "url", entry.url, "error", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			slog.Error("Failed to read image bytes", "module", "mention", "url", entry.url, "error", err)
			continue
		}
		images = append(images, app.PiImage{
			MimeType: entry.mime,
			Data:     base64.StdEncoding.EncodeToString(body),
		})
	}
	return images
}

// ---------------------------------------------------------------------------
// Bot identity + step 5 (slow-user auto-gulag, RUST order preserved)
// ---------------------------------------------------------------------------

// botUserID mirrors Rust's `ctx.http.get_current_user()`: the ready state
// is the equivalent source the Go client caches; when it is absent, fall
// back to the REST GET users/@me (Rust unconditionally uses the HTTP
// call).
func (h *Mention) botUserID() (string, error) {
	if h.app.D != nil && h.app.D.State != nil {
		if u := h.app.D.State.User; u != nil {
			return u.ID, nil
		}
	}
	if h.app.D == nil {
		return "", errors.New("discord session not available")
	}
	var me struct {
		ID string `json:"id"`
	}
	resp, err := h.app.D.Request(http.MethodGet, discordgo.EndpointUsers+"@me", nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(resp, &me); err != nil {
		return "", err
	}
	if me.ID == "" {
		return "", errors.New("current user response missing id")
	}
	return me.ID, nil
}

// ---------------------------------------------------------------------------
// Step 5 — Slow-user auto-gulag (RUST: handle_slow_user_auto_gulag,
// lines 412-473) — ported 1:1 including the eprintln texts and the
// u64-overflow arm on the gulag role ID. HITS the canonical Task-4 core
// (SelectServerByGuildID, FindChannel, AddToGulag); never re-declares.
// ---------------------------------------------------------------------------

func (h *Mention) slowUserAutoGulag(ctx context.Context, guildID int64, m *discordgo.Message) {
	// Routed through the injected *core.Gulag (the canonical Task-4 core;
	// tests inject a working one over fake seams via NewWithSeams).
	// 1. Server lookup (Rust: get_server_by_guild_id; the None arm covers
	// both the missing row and a DB failure — identical log text).
	server, found, err := h.core.SelectServerByGuildID(ctx, guildID)
	if err != nil || !found {
		slog.Error("No server config for guild "+strconv.FormatInt(guildID, 10)+" (or DB unavailable)", "module", "mention")
		return
	}

	guildIDStr := strconv.FormatInt(guildID, 10)
	// 2. FindChannel ("the-gulag") — evaluated BEFORE the role-ID u64
	// conversion (Rust: Gulag::find_channel, before the params are built).
	channel, channelFound, err := h.core.FindChannel(ctx, guildIDStr, core.GulagChannelName)
	switch {
	case err != nil:
		slog.Error("Error looking up gulag channel: "+err.Error(), "module", "mention")
		return
	case !channelFound:
		slog.Error("No gulag channel found", "module", "mention")
		return
	}

	// 3. Rust: u64::try_from(server.gulag_id) — overflows only for negative
	// IDs (our column is int64; checked conversions at every boundary).
	// Checked AFTER the channel lookup (Rust's params-construction order),
	// so the channel arms hit first.
	if server.GulagID < 0 {
		slog.Error("gulag role ID "+strconv.FormatInt(server.GulagID, 10)+" overflows u64", "module", "mention")
		return
	}

	// 4. AddToGulag (Rust: Gulag::add_to_gulag).
	params := core.GulagParams{
		GuildID:     guildIDStr,
		UserID:      m.Author.ID,
		GulagRoleID: strconv.FormatInt(server.GulagID, 10),
		GulagLength: GulagDurationSeconds, // Rust: GULAG_DURATION_SECS (=300)
		ChannelID:   channel.ID,
		MessageID:   m.ID,
	}
	if _, err := h.core.AddToGulag(ctx, params); err != nil {
		slog.Error("Failed to gulag slow user: "+err.Error(), "module", "mention")
		return
	}
	// Success: the EXACT channel message text (the mention carries the
	// <@ID>);
	if err := h.ops.channelMessageSend(channel.ID, fmt.Sprintf(slowUserGulagMessage, m.Author.Mention())); err != nil {
		slog.Error("Failed to send gulag message: "+err.Error(), "module", "mention")
	}
}
