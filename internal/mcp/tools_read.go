package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readMessagesArgs — one page of a channel's recent message history, plus an
// exact-match author filter (ID and/or username — both set → OR semantics;
// v1 matches Username only, documented in the tool description as
// "filters by username").
type readMessagesArgs struct {
	ChannelID  string `json:"channel_id,omitempty"`
	GuildID    string `json:"guild_id,omitempty"`
	Limit      int    `json:"limit,omitempty"` // default 50; >100 → clamp to 100
	BeforeID   string `json:"before_id,omitempty"`
	AfterID    string `json:"after_id,omitempty"`
	AuthorID   string `json:"author_id,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
}

const (
	readMessagesDefaultLimit = 50
	readMessagesMaxLimit     = 100
)

// registerReadTools adds the read_messages tool to the SDK server (called
// from NewServer).
func registerReadTools(srv *mcpSDK.Server, d DiscordAPI) {
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "read_messages",
		Description: "Reads a channel's recent message history. channel_id accepts a numeric snowflake or a channel name (name form requires guild_id). Author filter matches exact username (author_name) or user ID (author_id). The text result renders one line per message — [id] author at timestamp (RFC3339): content (n attachments) — followed by a trailing \"--- N messages (first .. last)\" summary; embedded newlines in content collapse to \" ⏎ \". On Discord rate-limit (429) the tool returns a rate_limited error with a retry-after — no retry loop; re-call later.",
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest, args readMessagesArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleReadMessages(d, args)
	})
}

// handleReadMessages runs one ChannelMessages page (v1 — no paging loop),
// applies the author filter, and renders the summary + a record payload
// ("messages": …) — top-level arrays are not a valid MCP structuredContent
// (a client rejects the whole call), so the rows travel under a key.
func handleReadMessages(d DiscordAPI, args readMessagesArgs) (*mcpSDK.CallToolResult, any, error) {
	// Snowflake validation FIRST, before any REST.
	if args.BeforeID != "" && !isSnowflake(args.BeforeID) {
		return toolErr("tugbot", "before_id/after_id must be a snowflake ID"), nil, nil
	}
	if args.AfterID != "" && !isSnowflake(args.AfterID) {
		return toolErr("tugbot", "before_id/after_id must be a snowflake ID"), nil, nil
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

	limit, note := args.Limit, ""
	switch {
	case limit <= 0:
		limit = readMessagesDefaultLimit
	case limit > readMessagesMaxLimit:
		limit = readMessagesMaxLimit
		note = fmt.Sprintf(" (limit clamped to %d)", readMessagesMaxLimit)
	}

	// ONE page; on any error there is NO partial page to return — a failed
	// fetch is an IsError result with no structured payload (the realDiscord
	// wrapper runs WithRetryOnRatelimit(false), so a 429 surfaces as
	// wrapDiscordErr's canonical "rate_limited (retry after …)" wording; v1
	// has no retry loop — the agent re-calls later).
	msgs, err := d.ChannelMessages(cid, limit, args.BeforeID, args.AfterID, "")
	if err != nil {
		return wrapDiscordErr(err), nil, nil
	}

	haveFilter := args.AuthorID != "" || args.AuthorName != ""
	// slice, not nil — zero results render as [] in the payload.
	out := make([]map[string]any, 0, len(msgs))
	firstTs, lastTs := "", ""
	for _, m := range msgs {
		if haveFilter {
			idMatch := args.AuthorID != "" && m.Author != nil && m.Author.ID == args.AuthorID
			nameMatch := args.AuthorName != "" && m.Author != nil && m.Author.Username == args.AuthorName // exact, case-sensitive
			if !idMatch && !nameMatch {
				continue
			}
		}
		ts := m.Timestamp.Format(time.RFC3339)
		if firstTs == "" {
			firstTs = ts
		}
		lastTs = ts
		author := "" // webhook messages carry no author
		if m.Author != nil {
			author = m.Author.Username
		}
		out = append(out, map[string]any{
			"id":               m.ID,
			"author":           author,
			"timestamp":        ts,
			"text":             m.Content,
			"attachment_count": len(m.Attachments),
		})
	}

	// Text result: one rendered line per included message (fetch order —
	// Discord REST order, NOT reversed) so any MCP client that surfaces
	// only the text sees the message content itself; the existing summary
	// (count + first/last timestamps, plus the clamp note) is kept as a
	// trailing block. The structured payload is untouched.
	var fb strings.Builder
	for _, row := range out {
		fb.WriteString(renderMessageLine(
			row["id"].(string), row["author"].(string), row["timestamp"].(string),
			row["text"].(string), row["attachment_count"].(int)))
		fb.WriteByte('\n')
	}
	msg := "messages"
	if len(out) == 1 {
		msg = "message"
	}
	var text string
	if len(out) == 0 {
		// Empty page: exactly "0 messages" (no lines, no timestamp range).
		text = "0 messages"
	} else {
		text = fb.String() + fmt.Sprintf("--- %d %s (%s .. %s)", len(out), msg, firstTs, lastTs)
	}
	if note != "" {
		text += note
	}
	return textResult(text), map[string]any{"messages": out}, nil
}

// renderMessageLine renders ONE message row as a single text line:
//
//	[<id>] <author> at <timestamp (RFC3339)>: <content> (<n> attachment[s])
//
// A webhook message (author == "") omits the author token entirely:
// "[<id>] at <timestamp>: …". Empty content renders as "(no text)"
// (with "(0 attachments)" still appended — the count is part of the row
// info). The trailing "(n attachment[s])" is always present and the noun
// is singular only for n == 1. Embedded newlines in content collapse to
// one " ⏎ " (U+23CE LINE SEPARATOR between two spaces) each, so a
// multi-line Discord message stays a single line — deterministic, using
// strings.ReplaceAll.
func renderMessageLine(id, author, timestamp, content string, attachments int) string {
	authorPart := ""
	if author != "" {
		authorPart = author + " "
	}
	var contentPart string
	switch content {
	case "":
		contentPart = "(no text)"
	default:
		contentPart = strings.ReplaceAll(content, "\n", " ⏎ ")
	}
	noun := "attachments"
	if attachments == 1 {
		noun = "attachment"
	}
	return fmt.Sprintf("[%s] %sat %s: %s (%d %s)", id, authorPart, timestamp, contentPart, attachments, noun)
}
