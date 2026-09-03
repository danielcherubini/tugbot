// Package teh is the Go port of the Rust bot's src/handlers/teh.rs.
//
// Behavior parity: when the "teh" feature flag is enabled and the message
// content contains "teh" (case-insensitive), the bot reacts to the message
// with "🇹", then "🇪", then "🇭" — sequentially, each error logged
// separately. Ported 1:1 from teh.rs; see docs/parity/checklist.md.
package teh

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// FeatureKey is the features-table key this handler is gated on (Rust: the
// literal "teh" at teh.rs:12).
const FeatureKey = "teh"

// Teh reacts to "teh" content.
type Teh struct {
	app *app.App
}

// New builds the handler (Rust: TypeMap-injected context gone — the App is
// the whole dependency surface).
func New(app *app.App) *Teh {
	return &Teh{app: app}
}

// MessageCreate runs on every message create (Rust `Teh::handler`, wired in
// src/handlers/mod.rs's message event, first in dispatch order).
func (h *Teh) MessageCreate(m *discordgo.Message) {
	if !features.IsEnabled(context.Background(), h.app.Pool, FeatureKey) {
		return
	}
	if !containsTeh(m.Content) {
		return
	}
	// Rust: three sequential `msg.react` calls, each with its own error log
	// (teh.rs:14-25).
	emojis := reactionEmojis()
	errMsgs := reactionErrorMessages()
	for i, emoji := range emojis {
		if err := h.app.D.MessageReactionAdd(m.ChannelID, m.ID, emoji); err != nil {
			slog.Error(errMsgs[i], "module", "teh", "error", err)
		}
	}
}

// containsTeh mirrors Rust: msg.content.to_lowercase().contains("teh").
func containsTeh(content string) bool {
	return strings.Contains(strings.ToLower(content), "teh")
}

// reactionEmojis mirrors the three sequential reactions in teh.rs:14-25:
// "🇹", then "🇪", then "🇭".
func reactionEmojis() []string {
	return []string{"🇹", "🇪", "🇭"}
}

// reactionErrorMessages mirrors the three distinct eprintln texts (Rust kept
// them per-emoji: "T", "E", "H") — the message keys are byte-identical so
// `journalctl -u tugbot-go` greps keep working.
func reactionErrorMessages() []string {
	return []string{
		"Error reacting with emoji T",
		"Error reacting with emoji E",
		"Error reacting with emoji H",
	}
}
