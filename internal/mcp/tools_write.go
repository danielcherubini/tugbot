package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwmarrin/discordgo"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The write side, both thin wrappers — the bridge is boring on purpose
// (post/read/react is the 90% surface). Deliberate safety scoping:
// post_message truncates at Discord's 2000-char cap (runes 1999 keeps
// the common case ASCII-safe; v1 does NOT chunk — the note tells the
// agent to post in parts).

// discordMessageCap is the truncation ceiling (runes) — one under
// Discord's hard 2000 limit.
const discordMessageCap = 1999

// postMessageArgs — posts a text message, optionally as a reply.
type postMessageArgs struct {
	ChannelID string `json:"channel_id,omitempty"` // id-or-name, guild_id as in read
	GuildID   string `json:"guild_id,omitempty"`
	Text      string `json:"text"`
	ReplyToID string `json:"reply_to_id,omitempty"`
}

// reactArgs — adds an emoji reaction to a message. The emoji passes
// through as-is: any emoji token Discord accepts (unicode or the custom-
// emoji name format) — documented in the tool description.
type reactArgs struct {
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Emoji     string `json:"emoji"`
}

// registerWriteTools adds the post_message and react tools to the SDK
// server (called from NewServer).
func registerWriteTools(srv *mcpSDK.Server, d DiscordAPI) {
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "post_message",
		Description: "Posts a text message to a channel. channel_id accepts a numeric snowflake or a channel name (name form requires guild_id). Optional reply_to_id posts as a reply to that message. Text longer than the Discord cap is truncated with a note — for longer content post in parts.",
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest, args postMessageArgs) (*mcpSDK.CallToolResult, any, error) {
		return handlePostMessage(d, args)
	})
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "react",
		Description: "Adds an emoji reaction to a message. channel_id accepts a numeric snowflake or a channel name (name form requires guild_id). emoji is any emoji token Discord accepts; unicode or custom-emoji name.",
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest, args reactArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleReact(d, args)
	})
}

// handlePostMessage validates (run before REST), truncates, and posts via
// ChannelMessageSend (plain) or ChannelMessageSendComplex with the
// MessageSend.Reference reply reference (the field is Reference, NOT
// MessageReference in discordgo v0.29.0).
func handlePostMessage(d DiscordAPI, args postMessageArgs) (*mcpSDK.CallToolResult, any, error) {
	if args.Text == "" {
		return toolErr("tugbot", "text required"), nil, nil
	}
	if args.ReplyToID != "" && !isSnowflake(args.ReplyToID) {
		return toolErr("tugbot", "reply_to_id must be a snowflake ID"), nil, nil
	}

	cid, err := resolveChannelID(d, args.GuildID, args.ChannelID)
	switch {
	case err == nil:
		// resolved.
	case errors.Is(err, errChannelRequired):
		return toolErr("tugbot", errChannelRequired.Error()), nil, nil
	case errors.Is(err, errGuildRequired):
		return toolErr("tugbot", errGuildRequired.Error()), nil, nil
	case errors.Is(err, errChannelNotFound):
		return toolErr("tugbot", errChannelNotFound.Error()+": "+args.ChannelID), nil, nil
	default:
		// A REST failure (e.g. 404 guild / permissions) is a Discord error.
		return wrapDiscordErr(err), nil, nil
	}

	text, note := args.Text, ""
	if r := []rune(text); len(r) > discordMessageCap {
		text = string(r[:discordMessageCap])
		note = "text truncated to 1999 chars (Discord cap); for longer content post in parts"
	}

	var msg *discordgo.Message
	switch args.ReplyToID {
	case "":
		msg, err = d.ChannelMessageSend(cid, text)
	default:
		// Reference is the v0.29.0 reply field (NOT MessageReference).
		msg, err = d.ChannelMessageSendComplex(cid, &discordgo.MessageSend{
			Content:   text,
			Reference: &discordgo.MessageReference{MessageID: args.ReplyToID},
		})
	}
	if err != nil {
		return wrapDiscordErr(err), nil, nil
	}

	id := ""
	if msg != nil {
		id = msg.ID
	}
	out := fmt.Sprintf("posted message %s", id)
	if note != "" {
		out += " (" + note + ")"
	}
	return textResult(out), map[string]any{"id": id}, nil
}

// handleReact validates (run before REST) and passes the emoji token
// through as-is — Unicode, :name:, or the custom-emoji name format all
// reach Discord unchanged. The seam method is MessageReactionAdd (the
// session method shares that name — there is no ChannelMessageReactionAdd).
func handleReact(d DiscordAPI, args reactArgs) (*mcpSDK.CallToolResult, any, error) {
	if args.MessageID == "" {
		return toolErr("tugbot", "message_id required"), nil, nil
	} else if !isSnowflake(args.MessageID) {
		return toolErr("tugbot", "message_id must be a snowflake ID"), nil, nil
	}
	if args.Emoji == "" {
		return toolErr("tugbot", "emoji required"), nil, nil
	}

	cid, err := resolveChannelID(d, args.GuildID, args.ChannelID)
	switch {
	case err == nil:
		// resolved.
	case errors.Is(err, errChannelRequired):
		return toolErr("tugbot", errChannelRequired.Error()), nil, nil
	case errors.Is(err, errGuildRequired):
		return toolErr("tugbot", errGuildRequired.Error()), nil, nil
	case errors.Is(err, errChannelNotFound):
		return toolErr("tugbot", errChannelNotFound.Error()+": "+args.ChannelID), nil, nil
	default:
		// A REST failure (e.g. 404 guild / permissions) is a Discord error.
		return wrapDiscordErr(err), nil, nil
	}

	if err := d.MessageReactionAdd(cid, args.MessageID, args.Emoji); err != nil {
		return wrapDiscordErr(err), nil, nil
	}
	return textResult(fmt.Sprintf("reacted %s on %s", args.Emoji, args.MessageID)), nil, nil
}
