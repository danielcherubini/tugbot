// Package instagram is the Go port of the Rust bot's
// src/handlers/instagram.rs.
//
// Mechanic parity (port 1:1, see docs/parity/checklist.md — instagram
// section): when the "instagram" feature flag is enabled and the message
// content matches the instagram-URL regex, the bot (1) edits the ORIGINAL
// message to suppress its embeds, then (2) posts a NEW message containing
// only the matched URL with its domain rewritten to kkinstagram.com while
// the optional "www." prefix is preserved. This is NOT an in-place edit —
// Rust does the same: `edit(...suppress_embeds(true))` followed by
// `channel.say(fixed)`.
package instagram

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
// literal "instagram" at instagram.rs:13).
const FeatureKey = "instagram"

// reInstagram is Rust's `https://(www\.)?(instagram\.com)/.+`
// (instagram.rs:28). Group 1 is the optional "www." prefix (untouched); the
// replacement applies to group 2, the domain.
var reInstagram = regexp.MustCompile(`https://(www\.)?(instagram\.com)/.+`)

// Instagram suppresses embeds on instagram-URL messages and reposts the
// rewritten URL.
type Instagram struct {
	app *app.App
}

// New builds the handler.
func New(app *app.App) *Instagram {
	return &Instagram{app: app}
}

// MessageCreate runs on every message create (Rust `Instagram::handler`,
// wired in src/handlers/mod.rs's message event).
func (h *Instagram) MessageCreate(m *discordgo.Message) {
	if !features.IsEnabled(context.Background(), h.app.Pool, FeatureKey) {
		return
	}
	fixed := rewrite(m.Content)
	if fixed == "" {
		return
	}
	// Rust instagram.rs:17-30 — port verbatim, including the unconditional
	// "Suppressed Embed"/"Posted Instagram" prints that follow each attempt.
	if err := h.suppressEmbeds(m); err != nil {
		slog.Error("Error supressing embeds", "module", "instagram", "error", err)
	}
	slog.Info("Suppressed Embed", "module", "instagram")
	if _, err := h.app.D.ChannelMessageSend(m.ChannelID, fixed); err != nil {
		slog.Error("Error posting Instagram message", "module", "instagram", "error", err)
	}
	slog.Info("Posted Instagram", "module", "instagram")
}

// suppressEmbeds mirrors Rust's
// `msg.clone().edit(..., EditMessage::new().suppress_embeds(true))` — SET the
// SUPPRESS_EMBEDS message flag (1<<2) on the message via a (flags-only) edit.
func (h *Instagram) suppressEmbeds(m *discordgo.Message) error {
	edit := discordgo.NewMessageEdit(m.ChannelID, m.ID)
	edit.Flags = discordgo.MessageFlagsSuppressEmbeds
	_, err := h.app.D.ChannelMessageEditComplex(edit)
	return err
}

// rewrite mirrors Rust's fx_rewriter (instagram.rs:26-40): find the first
// match and replace EVERY occurrence of the domain group ("instagram.com",
// group 2 — group 1 "www." is untouched) inside the matched substring with
// "kkinstagram.com" (Rust's String::replace replaces all occurrences).
// Returns "" when there is no match. It returns only the matched URL —
// Rust's `channel.say(fixed_message)` posts exactly that string.
func rewrite(content string) string {
	m := reInstagram.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[0], m[2], "kkinstagram.com")
}
