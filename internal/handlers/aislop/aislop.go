// Package aislop is the Go port of the Rust bot's source/handlers/aislop.rs
// (211 lines).
//
// Taxonomy (verified against source/handlers/mod.rs): "AI Slop" is a
// MESSAGE CONTEXT-MENU command (kind = ApplicationCommandTypeMessage),
// registered by ready() among the per-guild commands (mod.rs:291). This
// task registers exactly this one command — kind=Message, name "AI Slop",
// EMPTY description, no options. (The "Add Gulag Vote" context-menu
// registration belongs to Task 6's gulag package; this task must not
// register it.)
//
// Behavior parity (docs/parity/checklist.md — aislop section): silent
// feature-flag gate via IsEnabled (unlike the prefix handler, which uses
// the propagating CheckEnabled); the guild guard; the "Highly Regarded"
// / "admin" role gate via the core's MemberHasAnyRole (one guild-roles
// fetch + member role-id scan); resolution of the FIRST resolved message
// (Rust HashMap.values().next(); Go's map iteration is likewise
// arbitrary — pinned in the checklist); the self-slop and bot-target
// guards; the server-row lookup; the atomic usage increment; the
// exponential offense duration via the canonical core (with the
// caller-side int32 clamp); the the-gulag notification and the final
// offense / next-offense message. All responses are ephemeral.
package aislop

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// FeatureKey pins the feature flag key (aislop.rs:31).
const FeatureKey = "ai_slop"

// AiSlop handles the "AI Slop" message-context-menu command (Rust:
// AiSlopHandler).
type AiSlop struct {
	app *app.App
	g   *core.Gulag
}

// New builds the handler (consumes the canonical Task 4 gulag core —
// never re-declares any of its helpers).
func New(app *app.App) *AiSlop {
	return &AiSlop{app: app, g: core.New(app)}
}

// Response is this module's share of Rust's HandlerResponse shape: all
// AI Slop responses are ephemeral; none defers and none sets components
// (parity with Rust's Option::None defaults).
type Response struct {
	Content   string
	Ephemeral bool
}

func ephemeralResponse(content string) Response {
	return Response{Content: content, Ephemeral: true}
}

// AllowedRoles pins the role allowlist (aislop.rs:51), in Rust order
// (the core's MemberHasAnyRole scans exactly these names).
func AllowedRoles() []string {
	return []string{"Highly Regarded", "admin"}
}

// SetupCommand mirrors `AiSlopHandler::setup_command` (aislop.rs:22-26)
// field-by-field: kind Message, name "AI Slop", EMPTY description, no
// options — byte-identical to the Rust registration.
func (h *AiSlop) SetupCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.MessageApplicationCommand,
		Name:        "AI Slop",
		Description: "",
	}
}

// OffenseDuration mirrors the offense conversion this handler uses
// (aislop.rs:98-110: new_count.saturating_sub(1) -> the exponential
// duration lookup, the try_into arm preserved) — delegated to the
// canonical core.
func OffenseDuration(newCount int64) (int64, error) {
	return core.OffenseSecondsForCount(newCount)
}

// NextOffenseSeconds mirrors the "next offense" conversion (aislop.rs
// 179-185, the int32 -> u32 try_into arm with the 30-day fallback)
// delegated to the canonical core.
func NextOffenseSeconds(newCount int64) int64 {
	return core.NextTotalSecondsForCount(newCount)
}

// responseTexts pins the byte-for-byte gate texts from aislop.rs
// (what the AI-slop heuristics decide on). sendFailPrefix is the format
// prefix of "Error: Failed to send to gulag: {err}".
func responseTexts() map[string]string {
	return map[string]string{
		"disabled":       "This feature is currently disabled.",
		"guildOnly":      "Error: This command can only be used in a guild",
		"permVerify":     "Error: Could not verify your permissions",
		"roleRequired":   "Error: You need Highly Regarded or admin role to use this command",
		"noTarget":       "Error: Could not find target message",
		"selfSlop":       "Error: You cannot AI Slop yourself!",
		"botTarget":      "Error: You cannot AI Slop the bot!",
		"botVerify":      "Error: Could not verify bot status",
		"unconfigured":   "Error: This server is not configured. Please ensure a gulag role exists.",
		"recordFail":     "Error: Failed to record AI slop usage",
		"countTooHigh":   "Error: Usage count too high for gulag calculation",
		"sendFailPrefix": "Error: Failed to send to gulag: ",
	}
}

// HandleInteraction mirrors `AiSlopHandler::setup_interaction`
// (aislop.rs:28-197) 1:1 in gate order and exact texts. The Rust entry
// point never returns an error — failures become ephemeral error
// responses. (The 404/timeout reply semantics are Task 7's orchestration
// job, not this handler's.)
func (h *AiSlop) HandleInteraction(i *discordgo.Interaction) Response {
	ctx := context.Background()
	resp := responseTexts()

	// Silent IsEnabled gate (Rust uses the silent flavor here — no
	// DB-error response path, unlike the prefix handler).
	if !features.IsEnabled(ctx, h.app.Pool, FeatureKey) {
		return ephemeralResponse(resp["disabled"])
	}

	guildID := i.GuildID
	if guildID == "" {
		return ephemeralResponse(resp["guildOnly"])
	}

	// Permissions: fetch the invoking member (Rust:
	// ctx.http.get_member(guild_id, command.user.id)).
	var member *discordgo.Member
	switch {
	case i.User != nil:
		fetched, err := h.app.D.GuildMember(guildID, i.User.ID)
		if err != nil || fetched == nil {
			return ephemeralResponse(resp["permVerify"])
		}
		member = fetched
	case i.Member != nil:
		member = i.Member
	default:
		return ephemeralResponse(resp["permVerify"])
	}

	if !h.g.MemberHasAnyRole(ctx, guildID, member, AllowedRoles()...) {
		return ephemeralResponse(resp["roleRequired"])
	}

	// Target message: Rust command.data.resolved.messages.values().next()
	// — a HashMap() iteration, so the choice is arbitrary; Go's map
	// iteration is likewise arbitrary (parity pinned in the checklist).
	var targetMessage *discordgo.Message
	if data, ok := i.Data.(*discordgo.ApplicationCommandInteractionData); ok && data.Resolved != nil && len(data.Resolved.Messages) > 0 {
		for _, m := range data.Resolved.Messages {
			targetMessage = m
			break
		}
	}
	if targetMessage == nil || targetMessage.Author == nil {
		// The nil-author branch is a defensive extension; Rust's Author
		// field is non-Option, so only the first arm is reachable there.
		return ephemeralResponse(resp["noTarget"])
	}
	targetUser := targetMessage.Author

	// Prevent self-slop.
	if i.User != nil && targetUser.ID == i.User.ID {
		return ephemeralResponse(resp["selfSlop"])
	}

	// Prevent targeting the bot (Rust: Some(true) -> error, Some(false)
	// -> continue, None -> error).
	if isTugbot := h.g.IsTugbot(targetUser); isTugbot != nil {
		if *isTugbot {
			return ephemeralResponse(resp["botTarget"])
		}
	} else {
		return ephemeralResponse(resp["botVerify"])
	}

	guildIDi, err := core.DiscordID("guild", guildID)
	if err != nil {
		slog.Error("failed to convert guild ID", "module", "aislop", "error", err)
		return ephemeralResponse(resp["unconfigured"])
	}
	server, found, err := core.SelectServerByGuildID(ctx, h.app.Pool, guildIDi)
	if err != nil || !found {
		return ephemeralResponse(resp["unconfigured"])
	}

	targetUserID, err := core.DiscordID("user", targetUser.ID)
	if err != nil {
		slog.Error("failed to convert user ID", "module", "aislop", "error", err)
		return ephemeralResponse(resp["recordFail"])
	}

	// Record the offense (atomic upsert returning the NEW count).
	usage, err := core.IncrementAiSlopUsage(ctx, h.app.Pool, targetUserID, guildIDi)
	if err != nil {
		slog.Error("failed to record AI slop usage", "module", "aislop", "error", err)
		return ephemeralResponse(resp["recordFail"])
	}
	newCount := int64(usage)

	// Duration for the offense that just occurred (new_count - 1).
	duration, err := OffenseDuration(newCount)
	if err != nil {
		return ephemeralResponse(resp["countTooHigh"])
	}

	// The caller-side int32 clamp lives in DurationToGulagLength.
	if _, err := h.g.AddToGulag(ctx, core.GulagParams{
		GuildID:     guildID,
		UserID:      targetUser.ID,
		GulagRoleID: strconv.FormatInt(server.GulagID, 10),
		GulagLength: core.DurationToGulagLength(duration),
		ChannelID:   i.ChannelID,
		MessageID:   targetMessage.ID,
	}); err != nil {
		slog.Error("Failed to send user to gulag", "module", "aislop", "error", err)
		return ephemeralResponse(resp["sendFailPrefix"] + err.Error())
	}

	// Post the notification to #the-gulag (Rust: only when the channel
	// exists; a lookup error (including 404) and a send failure are both
	// swallowed).
	if channel, channelFound, _ := h.g.FindChannel(ctx, guildID, core.GulagChannelName); channelFound {
		channelMessage := targetUser.Mention() + " has been sent to the gulag for " +
			core.FormatDuration(duration) + " for posting AI slop: " +
			messageLink(guildID, targetMessage) +
			"\nThis is offense #" + strconv.FormatInt(newCount, 10)
		_, _ = h.app.D.ChannelMessageSend(channel.ID, channelMessage)
	}

	nextDuration := NextOffenseSeconds(newCount)
	return ephemeralResponse(
		"Sent " + targetUser.Username + " to the gulag for " + core.FormatDuration(duration) +
			" for posting AI slop!\nThis is their offense #" + strconv.FormatInt(newCount, 10) +
			" (next offense will be " + core.FormatDuration(nextDuration) + ")")
}

// messageLink mirrors Rust's target_message.link()
// (https://discord.com/channels/{guild}/{channel}/{message}).
func messageLink(guildID string, message *discordgo.Message) string {
	return "https://discord.com/channels/" + guildID + "/" + message.ChannelID + "/" + message.ID
}
