// Package prefixhandler is the Go port of the Rust bot's
// source/handlers/prefix_handler.rs (207 lines).
//
// Wiring parity (verified in source/handlers/mod.rs): NOT a message
// handler — slash commands only. ready() registers
// PrefixHandler::setup_command("horny", "Mark yourself as horny/lfg") and
// setup_command("phony", "Mark yourself as phony/watching") among the
// per-guild commands (mod.rs:292-293); interaction_create dispatches
// those command names to setup_interaction. PORT: this module owns BOTH
// commands — SetupCommand(name, description) produces the registration for
// each, and HandleInteraction is the single interaction entry point.
//
// Behavior parity (docs/parity/checklist.md — prefixhandler section):
//   - The interaction's COMMAND NAME IS THE FLAG KEY. Rust checks
//     Features::check_enabled(&pool, &command.data.name) (prefix_handler.rs:50).
//     The live feature rows are "horny" and "phony" — NOT "is_this_real"
//     (the AGENTS.md note about the shared prefix is about the COMMAND
//     prefix, not the flag key). Parity note: these two flag rows exist
//     only in the live production DB — on a fresh DB CheckEnabled returns
//     (false, nil), so both commands are "disabled" in BOTH languages.
//   - A CheckEnabled failure (DB down) propagates into the exact Rust
//     error-path response text: "Error: Could not connect to the database.
//     Please try again later."
//   - Toggle semantics: strip "<prefix> | " (toggle off) or add it (on)
//     including across an existing " | "-separated nick.
package prefixhandler

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
)

// live flag rows (parity note — see package comment): "horny" and
// "phony" exist only in the live production DB.
const (
	CommandHorny = "horny"
	CommandPhony = "phony"
)

// PrefixHandler toggles a "<prefix> | <nick>" style prefix on the
// calling member's nickname.
type PrefixHandler struct {
	app *app.App
}

// New builds the handler.
func New(app *app.App) *PrefixHandler {
	return &PrefixHandler{app: app}
}

// Response is this module's share of Rust's HandlerResponse shape: all
// prefixhandler responses are ephemeral, none defers and none sets
// components (Defer responses stays nil, parity with Rust's default
// Option::None).
type Response struct {
	Content   string
	Ephemeral bool
}

func ephemeralResponse(content string) Response {
	return Response{Content: content, Ephemeral: true}
}

// FeatureKey pins the DB key: the command name IS the flag key
// (prefix_handler.rs:50-72 — command.data.name is passed verbatim to
// Features::check_enabled).
func FeatureKey(commandName string) string { return commandName }

// SetupCommand mirrors `PrefixHandler::setup_command`
// (prefix_handler.rs:24-26): a plain slash command, name + description,
// no options.
func (h *PrefixHandler) SetupCommand(name, description string) *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        name,
		Description: description,
	}
}

// HandleInteraction mirrors `PrefixHandler::setup_interaction`
// (prefix_handler.rs:28-114) 1:1, including the exact response texts. The
// Rust entry point never returns an error — failures become error
// responses.
func (h *PrefixHandler) HandleInteraction(i *discordgo.Interaction) Response {
	ctx := context.Background()
	var name string
	if data, ok := i.Data.(*discordgo.ApplicationCommandInteractionData); ok {
		name = data.Name
	}
	prefix := FeatureKey(name)

	// Rust: features::check_enabled(&pool, &prefix) — propagates.
	enabled, err := features.CheckEnabled(ctx, h.app.Pool, prefix)
	if err != nil {
		slog.Error("failed to check feature status", "module", "prefixhandler", "command", prefix, "error", err)
		return ephemeralResponse("Error: Could not connect to the database. Please try again later.")
	}
	if !enabled {
		return ephemeralResponse("This feature is currently disabled")
	}

	// Rust: command.member + command.guild_id guards.
	if i.Member == nil || i.GuildID == "" {
		return ephemeralResponse("Error: This command can only be used in a server")
	}

	// Rust: ctx.http.get_member(guild_id, user.id) — user.id is the
	// invoking user (Discord-go fills Interaction.User for guild
	// command interactions; the member-embedded user is the fallback).
	memberUserID := ""
	if i.User != nil {
		memberUserID = i.User.ID
	} else if i.Member.User != nil {
		memberUserID = i.Member.User.ID
	}
	if memberUserID == "" {
		return ephemeralResponse("Error: This command can only be used in a server")
	}
	currentNick := i.Member.Nick
	if currentNick == "" { // Rust: member.nick.as_deref().unwrap_or(member.display_name())
		currentNick = i.Member.User.Username
	}
	newNick := FixNickname(currentNick, prefix)
	wasAlreadyPrefixed := hasPrefix(currentNick, prefix)
	actionWord := "Added"
	if wasAlreadyPrefixed {
		actionWord = "Removed"
	}
	if _, err := h.app.D.GuildMember(i.GuildID, memberUserID); err != nil {
		slog.Error("failed to fetch member", "module", "prefixhandler", "command", prefix, "error", err)
		return ephemeralResponse("Error: Could not fetch your member info. Please try again later.")
	}

	// Rust: mem.edit(EditMember::new().nickname(new_nick))
	nick := newNick
	if _, err := h.app.D.GuildMemberEdit(i.GuildID, memberUserID, &discordgo.GuildMemberParams{Nick: nick}); err != nil {
		slog.Error("failed to update nickname", "module", "prefixhandler", "command", prefix, "error", err)
		return ephemeralResponse(nicknameErrorContent(err))
	}

	return ephemeralResponse(actionWord + " | " + prefix + " your nickname")
}

// nicknameErrorContent ports the Rust error mapping (prefix_handler.rs:96-109).
// Rust matches on e.to_string() (which embeds the Discord API message
// text) with case-sensitive `.contains`; this mirrors that on the REST
// error's combined status + body text — NOT a 404 probe (this module has
// no 404 cleanup need).
func nicknameErrorContent(err error) string {
	detail := err.Error()
	switch {
	case strings.Contains(detail, "Missing Permissions"):
		return "Error: I don't have permission to change nicknames. Please check my role permissions."
	case strings.Contains(detail, "Cannot exceed the limit"):
		return "Error: Nickname is too long. Please shorten your nickname first."
	default:
		return "Error: Could not update nickname: " + detail
	}
}

// cleanUsername mirrors clean_username (prefix_handler.rs:9-11): strips
// the "phony | " and "horny | " prefixes (String::replace, all
// occurrences).
func cleanUsername(nick string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nick, "phony | ", ""), "horny | ", "")
}

// FixNickname mirrors fix_nickname (prefix_handler.rs:14-23):
//   - "<prefix> | " present -> strip it (toggle off)
//   - else " | " present   -> add "<prefix> | " before the cleaned nick
//   - else                 -> add "<prefix> | " before the nick
func FixNickname(nick, prefix string) string {
	nickToFind := prefix + " | "
	if strings.Contains(nick, nickToFind) {
		return cleanUsername(nick)
	}
	if strings.Contains(nick, " | ") {
		return prefix + " | " + cleanUsername(nick)
	}
	return prefix + " | " + nick
}

// hasPrefix mirrors `was_already_prefixed = current_nick.contains("prefix | ")`
// (prefix_handler.rs:81).
func hasPrefix(nick, prefix string) bool {
	return strings.Contains(nick, prefix+" | ")
}
