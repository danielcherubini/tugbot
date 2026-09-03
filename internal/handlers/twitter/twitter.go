// Package twitter is the Go port of the Rust bot's src/handlers/twitter.rs.
//
// Mechanic parity (port 1:1, see docs/parity/checklist.md — twitter section):
// when the "twitter" feature flag is enabled and the message content matches
// the status-URL regex, the bot (1) edits the ORIGINAL message to suppress
// its embeds, then (2) posts a NEW message containing only the matched URL
// with its domain rewritten to girlcockx.com. This is NOT an in-place edit —
// Rust does the same: `edit(...suppress_embeds(true))` followed by
// `channel.say(fixed)`.
package twitter

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// FeatureKey is the features-table key this handler is gated on (Rust: the
// literal "twitter" at twitter.rs:13).
const FeatureKey = "twitter"

// reTwitter is Rust's `https://(twitter.com|x.com)/.+/status/\d+` (twitter.rs:30).
// A `.` matches any character except a newline in both regex engines.
var reTwitter = regexp.MustCompile(`https://(twitter\.com|x\.com)/.+/status/\d+`)

// Twitter suppresses embeds on status-URL messages and reposts the rewritten
// URL.
type Twitter struct {
	app *app.App
}

// New builds the handler.
func New(app *app.App) *Twitter {
	return &Twitter{app: app}
}

// MessageCreate runs on every message create (Rust `Twitter::handler`, wired
// in src/handlers/mod.rs's message event).
func (h *Twitter) MessageCreate(m *discordgo.Message) {
	if !features.IsEnabled(context.Background(), h.app.Pool, FeatureKey) {
		return
	}
	fixed := rewrite(m.Content)
	if fixed == "" {
		return
	}
	// Rust twitter.rs:18-26 — port verbatim, including the unconditional
	// "Suppressed Embed"/"Posted Tweet" prints that follow each attempt.
	if err := h.suppressEmbeds(m); err != nil {
		slog.Error("Error supressing embeds", "module", "twitter", "error", err)
	}
	slog.Info("Suppressed Embed", "module", "twitter")
	if _, err := h.app.D.ChannelMessageSend(m.ChannelID, fixed); err != nil {
		slog.Error("Error Editing Message to Tweet", "module", "twitter", "error", err)
	}
	slog.Info("Posted Tweet", "module", "twitter")
}

// suppressEmbeds mirrors Rust's
// `msg.clone().edit(..., EditMessage::new().suppress_embeds(true))` — SET the
// SUPPRESS_EMBEDS message flag (1<<2) on the message via a (flags-only)
// edit. discordgo's MessageEdit has no SuppressEmbeds field; its `Flags`
// field carries the same flag.
func (h *Twitter) suppressEmbeds(m *discordgo.Message) error {
	edit := discordgo.NewMessageEdit(m.ChannelID, m.ID)
	edit.Flags = discordgo.MessageFlagsSuppressEmbeds
	_, err := h.app.D.ChannelMessageEditComplex(edit)
	return err
}

// rewrite mirrors Rust's fx_rewriter (twitter.rs:29-43): find the first
// match and replace EVERY occurrence of the matched domain group inside the
// matched substring with "girlcockx.com" (Rust's String::replace replaces all
// occurrences). It returns "" when there is no match. Importantly it returns
// only the matched URL — Rust's `channel.say(fixed_message)` posts exactly
// that string.
func rewrite(content string) string {
	m := reTwitter.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[0], m[1], "girlcockx.com")
}
