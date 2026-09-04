package gulag

// commands.go ports the three slash command handlers and the "Add
// Gulag Vote" context-menu command from
// src/handlers/gulag/{gulag_handler.rs, gulag_remove_handler.rs,
// gulag_list_handler.rs, gulag_message_command.rs}, plus the
// SetupCommand registration (Rust ready(): per-guild set_commands for
// the servers-table list) and the interaction_create dispatch.
//
// The dead `gulag-vote` command (gulag_vote.rs) is NOT registered
// anywhere in Rust; per the port's parity contract no registration or
// dispatch arm is ported for it.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/db"
	"github.com/danielcherubini/tugbot/internal/features"
)

// Response is the module's share of Rust's HandlerResponse shape (all
// gulag responses: Content + Ephemeral; none defers — Rust's
// defer_response is never Some on these paths).
type Response struct {
	Content   string
	Ephemeral bool
}

func ephemeralResponse(content string) Response {
	return Response{Content: content, Ephemeral: true}
}

// sendError mirrors Rust's Gulag::send_error: prefix "Error: {}", and
// the response is always ephemeral.
func sendError(err string) Response {
	return ephemeralResponse("Error: " + err)
}

// AllowedRoles pins the port's role allowlist (both slash handlers) and
// preserves the Rust order for the core MemberHasAnyRole scan.
func AllowedRoles() []string {
	return []string{"Highly Regarded", "admin"}
}

// commandShapes returns the four registered command shapes, option
// strings byte-identical to Rust (test-pinned in gulag_test.go). Note
// the /gulag length option's Rust description is the verbatim "How Long
// minutes".
func (g *Gulag) commandShapes() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "gulag",
			Description: "Send a user to the Gulag",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The user to lookup",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "reason",
					Description: "Why Are you sending them",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "length",
					Description: "How Long minutes",
					Required:    true,
				},
			},
		},
		{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "gulag-release",
			Description: "Release a user from the Gulag",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "The user to lookup",
					Required:    true,
				},
			},
		},
		{
			Type:        discordgo.ChatApplicationCommand,
			Name:        "gulag-list",
			Description: "List users in the Gulag",
		},
		{
			Type: discordgo.MessageApplicationCommand,
			Name: "Add Gulag Vote",
			// Rust: CreateCommand::new("Add Gulag Vote").kind(Message)
			// — no description set (empty string).
			Description: "",
		},
	}
}

// SetupCommand mirrors Rust ready(): registers exactly these four
// shapes onto every configured guild from the servers table (NOT a
// hardcoded guild list — matching `for server in servers {
// server.guild_id.set_commands(&ctx.http, vec![...]) }` — the Rust
// call implied the app id through http; discordgo requires it
// explicitly, hence the appID parameter). A registration failure is
// collected, not fatal: one missing guild does not skip the others.
func (g *Gulag) SetupCommand(d *discordgo.Session, appID string) []error {
	ctx := context.Background()
	var guildIDs []int64
	if g.pool != nil {
		rows, err := g.pool.Query(ctx, `SELECT guild_id FROM servers`)
		if err != nil {
			slog.Error("failed to load servers for command registration", "module", "gulag", "error", err)
			return []error{fmt.Errorf("failed to load servers: %w", err)}
		}
		defer rows.Close()
		for rows.Next() {
			var gid int64
			if err := rows.Scan(&gid); err != nil {
				return []error{fmt.Errorf("failed to scan server row: %w", err)}
			}
			guildIDs = append(guildIDs, gid)
		}
		if err := rows.Err(); err != nil {
			return []error{fmt.Errorf("failed to iterate servers: %w", err)}
		}
	}
	var errs []error
	for _, gid := range guildIDs {
		guildID := strconv.FormatInt(gid, 10)
		for _, cmd := range g.commandShapes() {
			if _, err := d.ApplicationCommandCreate(appID, guildID, cmd); err != nil {
				errs = append(errs, fmt.Errorf("register %q in guild %s: %w", cmd.Name, guildID, err))
			}
		}
	}
	return errs
}

// HandleCommandCreate is the command-side dispatch (Rust's
// interaction_create, the command-name match). Task 7's
// OnApplicationCommandCreate routes each name to its owning handler;
// the fallthrough mirrors Rust's "Not Implemented" arm. Per Rust, the
// gate order lives in each handler.
func (g *Gulag) HandleCommandCreate(i *discordgo.Interaction) Response {
	ctx := context.Background()
	name := interactionCommandName(i)
	switch name {
	case "gulag":
		return g.handleGulag(ctx, i)
	case "gulag-release":
		return g.handleGulagRelease(ctx, i)
	case "gulag-list":
		return g.handleGulagList(ctx, i)
	case "Add Gulag Vote":
		return g.handleAddGulagVote(ctx, i)
	default:
		return ephemeralResponse("Not Implemented")
	}
}

// interactionCommandName extracts Rust's command.data.name (the command
// name on an application-command interaction; non-command interactions
// fall to the default arm).
func interactionCommandName(i *discordgo.Interaction) string {
	if data, ok := i.Data.(discordgo.ApplicationCommandInteractionData); ok {
		return data.Name
	}
	return ""
}

// commandOptionByName mirrors Rust's options find by name.
func commandOptionByName(i *discordgo.Interaction, name string) *discordgo.ApplicationCommandInteractionDataOption {
	data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok {
		return nil
	}
	for _, o := range data.Options {
		if o != nil && o.Name == name {
			return o
		}
	}
	return nil
}

// firstCommandOption mirrors Rust's options.first().
func firstCommandOption(i *discordgo.Interaction) *discordgo.ApplicationCommandInteractionDataOption {
	data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok || len(data.Options) == 0 {
		return nil
	}
	return data.Options[0]
}

// invokerMember mirrors Rust's get_member(guild_id, command.user.id):
// fetch the invoking member; the DM fallback hands back the bare user
// (DM interactions carry no guild, which the guild guards above catch).
func (g *Gulag) invokerMember(i *discordgo.Interaction) *discordgo.Member {
	var candidate *discordgo.User
	switch {
	case i.User != nil:
		candidate = i.User
	case i.Member != nil:
		candidate = i.Member.User
	}
	if candidate == nil {
		return nil
	}
	if i.GuildID == "" {
		return &discordgo.Member{User: candidate}
	}
	fetched, err := g.d.GuildMember(i.GuildID, candidate.ID)
	if err != nil || fetched == nil || fetched.User == nil {
		return nil
	}
	return fetched
}

// invokerIsEmpty folds the Rust `guild_id None` arms: those arms fire
// when the interaction has no guild (they precede the member fetch in
// Rust).
func invokerIsEmpty(i *discordgo.Interaction) bool {
	return i.GuildID == ""
}

// handleGulag mirrors gulag_handler.rs:14-221 (gate order and exact
// texts pinned).
func (g *Gulag) handleGulag(ctx context.Context, i *discordgo.Interaction) Response {
	// Silent IsEnabled gate — NO trailing period (distinct from the
	// message command's text).
	if !features.IsEnabled(ctx, g.pool, FeatureKey) {
		return ephemeralResponse("Gulag feature is currently disabled")
	}
	// The guild guard — Rust's FIRST guard (gulag_handler.rs:39-49):
	// ephemeral. (The dead inner-match "no member" arm it used to cite
	// is unreachable behind this guard in Rust and is not ported here.)
	guildID := i.GuildID
	if guildID == "" {
		return ephemeralResponse("Error: This command can only be used in a guild")
	}
	// Check permissions: require Highly Regarded or admin role.
	member := g.invokerMember(i)
	if member == nil {
		return ephemeralResponse("Error: Could not verify your permissions")
	}
	if !g.MemberHasAnyRole(ctx, guildID, member, AllowedRoles()...) {
		return ephemeralResponse("Error: You need Highly Regarded or admin role to use this command")
	}

	userOption := commandOptionByName(i, "user")
	if userOption == nil {
		return ephemeralResponse("Error: Missing required user option")
	}
	reasonOption := commandOptionByName(i, "reason")
	if reasonOption == nil {
		return ephemeralResponse("Error: Missing required reason option")
	}
	lengthOption := commandOptionByName(i, "length")
	if lengthOption == nil {
		return ephemeralResponse("Error: Missing required length option")
	}

	channelID := i.ChannelID

	// Default 300 seconds; the integer option is clamped to
	// (0, 10080] minutes -> *60 seconds (Rust lines 96-112).
	var gulagLength uint32 = 300
	if lengthOption.Value != nil {
		if length, ok := lengthOption.Value.(int64); ok {
			if length > 0 && length <= 10080 {
				gulagLength = uint32(length * 60)
			} else if length <= 0 {
				return ephemeralResponse("Gulag length must be positive")
			} else {
				return ephemeralResponse("Gulag length cannot exceed 10080 minutes (1 week)")
			}
		}
	}

	user, ok := userOption.Value.(*discordgo.User)
	if !ok {
		return Response{Content: "Please provide a valid user"}
	}

	gulagRole := g.FindGulagRole(ctx, guildID)
	if gulagRole == nil {
		return Response{Content: "couldn't find gulag role"}
	}

	gulagUser, err := g.AddToGulag(ctx, GulagParams{
		GuildID:     guildID,
		UserID:      user.ID,
		GulagRoleID: gulagRole.ID,
		GulagLength: gulagLength,
		ChannelID:   channelID,
		MessageID:   "0",
	})
	if err != nil {
		return ephemeralResponse("Failed to send to gulag: " + err.Error())
	}

	// Rust's success arm: with a string reason the reason is folded
	// into the message; the {} is the CUMULATIVE length (the DB row's
	// add branch extends it), not the raw option.
	if reason, ok := reasonOption.Value.(string); ok {
		return Response{Content: fmt.Sprintf("Sending %s to the Gulag for %d minutes, because %s",
			user.Mention(), gulagUser.GulagLength/60, reason)}
	}
	return Response{Content: fmt.Sprintf("Sending %s to the Gulag for %d minutes",
		user.Mention(), gulagUser.GulagLength/60)}
}

// handleGulagRelease mirrors gulag_remove_handler.rs:14-173. NOTE: NO
// feature gate — parity; its guild-None arm is Ephemeral "no member"
// (a live Rust arm, unlike /gulag's dead inner-match arm).
func (g *Gulag) handleGulagRelease(ctx context.Context, i *discordgo.Interaction) Response {
	guildID := i.GuildID
	if guildID == "" {
		return sendError("This command can only be used in a server")
	}
	member := g.invokerMember(i)
	if member == nil {
		return sendError("Could not verify your permissions")
	}
	if !g.MemberHasAnyRole(ctx, guildID, member, AllowedRoles()...) {
		return ephemeralResponse("Error: You need Highly Regarded or admin role to use this command")
	}

	userOption := firstCommandOption(i)
	if userOption == nil {
		return ephemeralResponse("Expected user option")
	}
	user, ok := userOption.Value.(*discordgo.User)
	if !ok {
		return Response{Content: "Please provide a valid user"}
	}
	if invokerIsEmpty(i) {
		return ephemeralResponse("no member")
	}

	gulagRole := g.FindGulagRole(ctx, guildID)
	if gulagRole == nil {
		return sendError("Couldn't find gulag Role")
	}

	userIDi, convErr := DiscordID("user", user.ID)
	if convErr != nil {
		return sendError("Couldn't find user in Database")
	}
	dbGulagUser := g.IsUserInGulag(ctx, userIDi)
	if dbGulagUser == nil {
		return sendError("Couldn't find user in Database")
	}

	gulagChannel, found, err := g.FindChannel(ctx, guildID, GulagChannelName)
	if err != nil {
		slog.Error("Error looking up Gulag Channel", "module", "gulag", "error", err)
		return sendError("Error looking up Gulag Channel")
	}
	if !found {
		return sendError("Couldn't find Gulag Channel")
	}

	targetMember, memberErr := g.d.GuildMember(guildID, user.ID)
	if memberErr != nil || targetMember == nil || targetMember.User == nil {
		return sendError("Couldn't get member")
	}
	if err := g.d.GuildMemberRoleRemove(guildID, user.ID, gulagRole.ID); err != nil {
		return sendError("Couldn't remove role")
	}
	message := "Freeing " + targetMember.Mention() + " from the gulag"
	if _, err := g.d.ChannelMessageSend(gulagChannel.ID, message); err != nil {
		return sendError("Couldn't Send message to release")
	}

	if err := g.deleteGulagUser(ctx, dbGulagUser.ID); err != nil {
		slog.Error("Failed to delete gulag user from DB", "module", "gulag", "error", err)
	} else {
		slog.Debug("Removed from database", "module", "gulag")
	}
	return ephemeralResponse("Releasing User from the Gulag")
}

// handleGulagList mirrors gulag_list_handler.rs:14-98.
func (g *Gulag) handleGulagList(ctx context.Context, i *discordgo.Interaction) Response {
	guildID := i.GuildID
	if guildID == "" {
		return Response{Content: "no member"}
	}
	guildIDi, convErr := DiscordID("guild", guildID)
	if convErr != nil {
		return ephemeralResponse("Failed to connect to database. Please try again later.")
	}
	gulagUsers, err := g.listGulagUsersByGuild(ctx, guildIDi)
	if err != nil {
		return ephemeralResponse("Failed to query gulag users. Please try again later.")
	}
	if len(gulagUsers) == 0 {
		return ephemeralResponse("No users currently in the Gulag.")
	}

	now := time.Now()
	var userlist string
	for _, guser := range gulagUsers {
		userID := strconv.FormatInt(guser.UserID, 10)
		user, userErr := g.d.User(userID)
		if userErr != nil || user == nil {
			userlist += fmt.Sprintf("\nUnknown user (ID: %d)", guser.UserID)
			continue
		}
		userlist += "\n" + user.Mention() + " - " + listTimeInfo(guser.ReleaseAt.Time, now)
	}
	return ephemeralResponse("Users in the Gulag:" + userlist)
}

func (g *Gulag) listGulagUsersByGuild(ctx context.Context, guildID int64) ([]db.GulagUser, error) {
	rows, err := g.pool.Query(ctx,
		`SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
		        gulag_length, created_at, release_at, message_id
		 FROM gulag_users WHERE in_gulag AND guild_id = $1`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.GulagUser
	for rows.Next() {
		var r db.GulagUser
		if err := rows.Scan(&r.ID, &r.UserID, &r.GuildID, &r.GulagRoleID, &r.ChannelID,
			&r.InGulag, &r.GulagLength, &r.CreatedAt, &r.ReleaseAt, &r.MessageID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// listTimeInfo mirrors Rust's format_time_info (gulag_list_handler.rs,
// 92-107) on top of the std Duration humanized Display: the future arm
// shows "releases in <humanized>" (300s -> "5m 0s"; 3661s ->
// "1h 1m 1s"; sub-second -> "0.5s" with trailing-zero trim), the past
// arm shows "overdue for release (<whole seconds>s ago)" (the Rust
// test anchor 41477253s).
func listTimeInfo(releaseAt, now time.Time) string {
	remaining := releaseAt.Sub(now)
	if remaining >= 0 {
		return "releases in " + humanizedDuration(remaining)
	}
	return fmt.Sprintf("overdue for release (%ds ago)", int64(-remaining/time.Second))
}

// humanizedDuration mirrors the Rust std::time::Duration Display (h/m/s
// humanization, zero seconds always shown, sub-second trailing-zero
// trim keeping at least one digit).
func humanizedDuration(d time.Duration) string {
	if d < time.Second {
		nanos := fmt.Sprintf("%09d", d.Nanoseconds())
		trimmed := strings.TrimRight(nanos, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		return "0." + trimmed + "s"
	}
	secs := int64(d / time.Second)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm %ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%dh %dm %ds", secs/3600, (secs/60)%60, secs%60)
}

// handleAddGulagVote mirrors gulag_message_command.rs:16-104: the
// gate, the target_id -> resolved-message resolution, the message's
// AUTHOR as the gulaged user, and the follow-up vote message.
func (g *Gulag) handleAddGulagVote(ctx context.Context, i *discordgo.Interaction) Response {
	// Silent IsEnabled gate — WITH the period (distinct from the
	// /gulag slash gate text).
	if !features.IsEnabled(ctx, g.pool, FeatureKey) {
		return ephemeralResponse("Gulag feature is currently disabled.")
	}
	data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok || data.TargetID == "" {
		return ephemeralResponse("No target message found.")
	}
	var target *discordgo.Message
	if data.Resolved != nil {
		target = data.Resolved.Messages[data.TargetID]
	}
	if target == nil || target.Author == nil {
		return ephemeralResponse("Could not resolve target message.")
	}
	guildID := i.GuildID
	if guildID == "" {
		return ephemeralResponse("This command can only be used in a server.")
	}
	guildIDi, convErr := DiscordID("guild", guildID)
	if convErr != nil {
		return ephemeralResponse(convErr.Error())
	}
	messageIDi, convErr := DiscordID("message", target.ID)
	if convErr != nil {
		return ephemeralResponse(convErr.Error())
	}
	channelIDi, convErr := DiscordID("channel", i.ChannelID)
	if convErr != nil {
		return ephemeralResponse(convErr.Error())
	}
	// Rust: message.author.id — the AUTHOR is the votive user.
	authorIDi, convErr := DiscordID("user", target.Author.ID)
	if convErr != nil {
		return ephemeralResponse(convErr.Error())
	}
	// Rust: command.user.id — the invoker is the vote.
	invokedIDi, convErr := DiscordID("user", invokerUserID(i))
	if convErr != nil {
		return ephemeralResponse(convErr.Error())
	}

	response, err := g.messageVoteCreateOrUpdate(ctx, messageIDi, guildIDi, channelIDi, authorIDi, invokedIDi)
	if err != nil {
		return ephemeralResponse(err.Error())
	}
	link := messageLink(guildID, target)
	switch response.responseType {
	case voteAdded:
		return ephemeralResponse(fmt.Sprintf(
			"A gulag vote has been added to %s\nThere are now %d unique votes total",
			link, response.vote.CurrentVoteTally))
	case voteRemoved:
		return ephemeralResponse(fmt.Sprintf(
			"A gulag vote has been removed from %s\nThere are now %d unique votes total",
			link, response.vote.CurrentVoteTally))
	default:
		return ephemeralResponse("Not Implemented")
	}
}

// invokerUserID returns the invoker's bare user ID (Rust
// command.user.id).
func invokerUserID(i *discordgo.Interaction) string {
	switch {
	case i.User != nil:
		return i.User.ID
	case i.Member != nil:
		return i.Member.User.ID
	}
	return ""
}
