// Package gokupoll is the Go port of the Rust bot's
// source/handlers/goku_poll.rs (169 lines).
//
// Taxonomy (verified against source/handlers/mod.rs): the goku poll is a
// MESSAGE-UPDATE handler, NOT a command — mod.rs's message_update()
// (lines 121-142) fetches the message when the update payload doesn't
// carry it, then calls GokuPoll::handle_message_update. The
// OnMessageUpdate wiring lands in Task 7; this task delivers the handler
// body + tests (docs/parity/checklist.md — gokupoll section).
package gokupoll

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	"github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// FeatureKey pins the feature flag key checked by the handler body
// (goku_poll.rs:36).
const FeatureKey = "goku_poll"

// GokuPoll is the message-update handler (Rust: GokuPoll).
type GokuPoll struct {
	app *app.App
	g   *gulag.Gulag
}

// New builds the handler (consumes the canonical Task 4 Gulag core —
// never re-declares any of its helpers).
func New(app *app.App) *GokuPoll {
	return &GokuPoll{app: app, g: gulag.New(app)}
}

// MessageUpdate runs on every message update (Task 7 wires the event; the
// fetch branch is ported here, exactly as mod.rs performs it).
func (h *GokuPoll) MessageUpdate(u *discordgo.MessageUpdate) {
	ctx := context.Background()

	// mod.rs:126-136 — when the update payload has no usable message
	// content, fetch it by channel + id.
	if messageNeedsFetch(u.Message) {
		if u.Message == nil {
			slog.Error("failed to fetch message for update event: no message in payload", "module", "gokupoll")
			return
		}
		fetched, err := h.app.D.ChannelMessage(u.Message.ChannelID, u.Message.ID)
		if err != nil {
			slog.Error("failed to fetch message for update event", "module", "gokupoll", "error", err)
			return
		}
		u.Message = fetched
	}
	h.handle(ctx, u.Message)
}

// handle mirrors handle_message_update (goku_poll.rs:15-168) 1:1. The
// Rust entry point never returns an error — every failure path logs and
// stops.
func (h *GokuPoll) handle(ctx context.Context, message *discordgo.Message) {
	// Only process polls that have results (Rust: poll and
	// results.is_finalized guards).
	poll := message.Poll
	if poll == nil || poll.Results == nil || !poll.Results.Finalized {
		return
	}

	guildID := message.GuildID
	if guildID == "" {
		return
	}

	if !features.IsEnabled(ctx, h.app.Pool, FeatureKey) {
		return
	}

	// The winning answer (highest vote count; no votes cast -> stop).
	winningText := winningAnswerText(poll)
	if winningText == "" {
		return
	}

	// Case-insensitive "goku" containment check.
	if !strings.Contains(strings.ToLower(winningText), "goku") {
		return
	}

	pollCreator := message.Author
	if pollCreator == nil { // defensive: Rust's author is a non-Option field
		return
	}

	// Don't gulag the bot (Rust: Some(true) | None => return;
	// Some(false) => continue).
	if isTugbot := h.g.IsTugbot(pollCreator); isTugbot == nil || *isTugbot {
		return
	}

	guildIDi, err := gulag.DiscordID("guild", guildID)
	if err != nil {
		slog.Error("goku poll: guild ID conversion error", "module", "gokupoll", "error", err)
		return
	}
	creatorID, err := gulag.DiscordID("user", pollCreator.ID)
	if err != nil {
		slog.Error("goku poll: user ID conversion error", "module", "gokupoll", "error", err)
		return
	}

	server, found, err := gulag.SelectServerByGuildID(ctx, h.app.Pool, guildIDi)
	if err != nil {
		slog.Error("goku poll: failed to find server", "module", "gokupoll", "error", err)
		return
	}
	if !found {
		slog.Error("goku poll: server not configured for guild", "module", "gokupoll", "guild", guildID)
		return
	}

	// Current usage count for the exponential duration.
	currentCount, err := gulag.GetOrCreateGulagPollUsage(ctx, h.app.Pool, creatorID, guildIDi)
	if err != nil {
		slog.Error("goku poll: failed to get usage count", "module", "gokupoll", "error", err)
		return
	}

	// Duration computed BEFORE increment (avoiding a TOCTOU race, per the
	// Rust comment). A lapsed (negative) count uses the 30-day cap.
	var duration int64
	if currentCount < 0 {
		slog.Error("goku poll: usage count overflowed, using max duration", "module", "gokupoll", "count", currentCount)
		duration = int64(gulag.MaxGulagLengthSeconds)
	} else {
		duration = gulag.GulagDurationForOffense(int(currentCount))
	}

	// The-gulag channel.
	gulagChannel, channelFound, err := h.g.FindChannel(ctx, guildID, gulag.GulagChannelName)
	if err != nil {
		slog.Error("goku poll: error looking up the-gulag channel", "module", "gokupoll", "error", err)
		return
	}
	if !channelFound {
		slog.Error("goku poll: could not find the-gulag channel", "module", "gokupoll")
		return
	}

	// Send to gulag with the calculated duration (the caller-side int32
	// clamp lives in DurationToGulagLength).
	if _, err := h.g.AddToGulag(ctx, gulag.GulagParams{
		GuildID:     guildID,
		UserID:      pollCreator.ID,
		GulagRoleID: strconv.FormatInt(server.GulagID, 10),
		GulagLength: gulag.DurationToGulagLength(duration),
		ChannelID:   gulagChannel.ID,
		MessageID:   message.ID,
	}); err != nil {
		slog.Error("goku poll: failed to send user to gulag", "module", "gokupoll", "error", err)
		return
	}

	// Increment usage count AFTER the successful gulag.
	newCount, err := gulag.IncrementGulagPollUsage(ctx, h.app.Pool, creatorID, guildIDi)
	if err != nil {
		slog.Error("goku poll: failed to increment usage count", "module", "gokupoll", "error", err)
		// On error, still increment by 1 to track the offense (Rust
		// current_count.saturating_add(1); a straight successor is the
		// saturation here — no int64 usage count can realistically
		// overflow).
		newCount = currentCount + 1
	}

	// Next-offense duration (Rust: new_count.saturating_add(1)
	// .try_into() with the 30-day fallback).
	nextDuration := NextOffenseDuration(int64(newCount))

	content := pollCreator.Mention() + " created a poll and Goku won. Sent to the gulag for " +
		gulag.FormatDuration(duration) + "!\nThis is offense #" + strconv.FormatInt(int64(newCount), 10) +
		" (next offense will be " + gulag.FormatDuration(nextDuration) + ")"

	// Rust: `let _ = channel.send_message(...)` — a send failure is
	// deliberately swallowed.
	_, _ = h.app.D.ChannelMessageSend(gulagChannel.ID, content)
}

// winningAnswerText mirrors the winning-answer extraction
// (goku_poll.rs:51-65): the highest vote count; "no votes cast"
// (count == 0) -> ""; the answer looked up by id, its text; a missing
// answer or text -> "". Ties resolve to the FIRST highest count entry
// (Rust iter().max_by_key keeps the first maximal element).
func winningAnswerText(p *discordgo.Poll) string {
	if p == nil || p.Results == nil || !p.Results.Finalized {
		return ""
	}
	var winning *discordgo.PollAnswerCount
	for _, c := range p.Results.AnswerCounts {
		if winning == nil || c.Count > winning.Count {
			winning = c
		}
	}
	if winning == nil || winning.Count <= 0 {
		return "" // No votes cast.
	}
	for _, answer := range p.Answers {
		if answer.AnswerID != winning.ID {
			continue
		}
		if answer.Media == nil {
			return ""
		}
		return answer.Media.Text
	}
	return ""
}

// NextOffenseDuration delegates to the canonical core conversion
// (saturating_add(1) + u32-checked duration, 30-day fallback —
// Gulag.IncrementTotalSecondsForCount).
func NextOffenseDuration(newCount int64) int64 {
	return gulag.IncrementTotalSecondsForCount(newCount)
}

// messageNeedsFetch mirrors the mod.rs fetch branch: the update payload
// carries no usable message (nil or no content) -> fetch from the
// channel.
func messageNeedsFetch(m *discordgo.Message) bool {
	return m == nil || m.Content == ""
}
