// Package bsky is the Go port of the Rust bot's src/handlers/bsky.rs.
//
// Mechanic parity (port 1:1, see docs/parity/checklist.md — bsky section):
// when the "bsky" feature flag is enabled and the message content matches the
// bsky-URL regex, the bot (1) edits the ORIGINAL message to suppress its
// embeds, then (2) posts a NEW message containing only the matched URL with
// its domain rewritten to bsyy.app. This is NOT an in-place edit — Rust does
// the same: `edit(...suppress_embeds(true))` followed by `channel.say(fixed)`.
package bsky

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
// literal "bsky" at bsky.rs:13).
const FeatureKey = "bsky"

// reBsky is Rust's `https://(bsky.app)/.+` (bsky.rs:29).
var reBsky = regexp.MustCompile(`https://(bsky\.app)/.+`)

// Bsky suppresses embeds on bsky-URL messages and reposts the rewritten URL.
type Bsky struct {
	app *app.App
}

// New builds the handler.
func New(app *app.App) *Bsky {
	return &Bsky{app: app}
}

// MessageCreate runs on every message create (Rust `Bsky::handler`, wired in
// src/handlers/mod.rs's message event).
func (h *Bsky) MessageCreate(m *discordgo.Message) {
	if !features.IsEnabled(context.Background(), h.app.Pool, FeatureKey) {
		return
	}
	fixed := rewrite(m.Content)
	if fixed == "" {
		return
	}
	// Rust bsky.rs:17-30 — port verbatim, including the unconditional
	// "Suppressed Embed"/"Posted Tweet" prints that follow each attempt.
	if err := h.suppressEmbeds(m); err != nil {
		slog.Error("Error supressing embeds", "module", "bsky", "error", err)
	}
	slog.Info("Suppressed Embed", "module", "bsky")
	if _, err := h.app.D.ChannelMessageSend(m.ChannelID, fixed); err != nil {
		slog.Error("Error Editing Message to Tweet", "module", "bsky", "error", err)
	}
	slog.Info("Posted Tweet", "module", "bsky")
}

// suppressEmbeds mirrors Rust's
// `msg.clone().edit(..., EditMessage::new().suppress_embeds(true))` — SET the
// SUPPRESS_EMBEDS message flag (1<<2) on the message via a (flags-only) edit.
func (h *Bsky) suppressEmbeds(m *discordgo.Message) error {
	edit := discordgo.NewMessageEdit(m.ChannelID, m.ID)
	edit.Flags = discordgo.MessageFlagsSuppressEmbeds
	_, err := h.app.D.ChannelMessageEditComplex(edit)
	return err
}

// rewrite mirrors Rust's fx_rewriter (bsky.rs:27-41): find the first match
// and replace EVERY occurrence of the domain group ("bsky.app") inside the
// matched substring with "bsyy.app" (Rust's String::replace replaces all
// occurrences). Returns "" when there is no match. It returns only the
// matched URL — Rust's `channel.say(fixed_message)` posts exactly that string.
func rewrite(content string) string {
	m := reBsky.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[0], m[1], "bsyy.app")
}
