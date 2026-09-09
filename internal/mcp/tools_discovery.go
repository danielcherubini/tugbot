package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Discovery tool norm: name→ID maps, not ID dumps — the agent keeps its
// context small and feeds the IDs back into the follow-up tools.

// listGuildsArgs has no input fields: list_guilds takes no arguments (the
// SDK's AddTool requires a map or struct In type).
type listGuildsArgs struct{}

// listChannelsArgs — plain omitempty tag (no schema-exclusion tag; emptiness
// is validated handler-side).
type listChannelsArgs struct {
	GuildID string `json:"guild_id,omitempty"`
}

// registerDiscoveryTools adds the two discovery tools to the SDK server
// (called from NewServer).
func registerDiscoveryTools(srv *mcpSDK.Server, d DiscordAPI) {
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "list_guilds",
		Description: "List the guilds the bot is in, as a name -> ID map. No arguments. State-backed (no REST) unless the bot's state has no guilds, in which case a single UserGuilds page (max 100) is returned.",
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest, _ listGuildsArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleListGuilds(d)
	})
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "list_channels",
		Description: "List the textable channels (text + news) of one guild, as a name -> ID map. Categories and voice channels are excluded.",
	}, func(_ context.Context, _ *mcpSDK.CallToolRequest, args listChannelsArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleListChannels(d, args)
	})
}

// handleListGuilds: in-memory StateGuilds (populated on READY — zero REST)
// first; the UserGuilds REST is the fallback ONLY when state is empty.
func handleListGuilds(d DiscordAPI) (*mcpSDK.CallToolResult, any, error) {
	m := make(map[string]string)
	text := ""
	if state := d.StateGuilds(); len(state) > 0 {
		for _, g := range state {
			m[g.Name] = g.ID
		}
		text = nameIDSummary(fmt.Sprintf("%d guilds", len(m)), m)
	} else {
		gs, err := d.UserGuilds(100, "", "", false)
		if err != nil {
			return wrapDiscordErr(err), nil, nil
		}
		for _, g := range gs {
			m[g.Name] = g.ID
		}
		text = nameIDSummary(fmt.Sprintf("%d guilds", len(m)), m)
		// One page; Discord caps at 100 — fine for discovery.
		if len(gs) == 100 {
			text += " (more than 100 guilds; first page only)"
		}
	}
	return textResult(text), m, nil
}

// handleListChannels: the one REST call per invocation (v1 has no cache —
// one extra round-trip beats maintaining a persisted mapping).
func handleListChannels(d DiscordAPI, args listChannelsArgs) (*mcpSDK.CallToolResult, any, error) {
	// We deliberately ship no default-guild env — discovery is the path.
	if args.GuildID == "" {
		return toolErr("tugbot", "no default guild is configured; pass guild_id"), nil, nil
	}
	chans, err := d.GuildChannels(args.GuildID)
	if err != nil {
		return wrapDiscordErr(err), nil, nil
	}
	// Only the two text-like guild channel types (v0.29.0; there is no
	// ChannelTypeGuildAnnouncement). Categories/voice excluded.
	m := make(map[string]string)
	for _, c := range chans {
		if c.Type == discordgo.ChannelTypeGuildText || c.Type == discordgo.ChannelTypeGuildNews {
			m[c.Name] = c.ID
		}
	}
	return textResult(nameIDSummary(fmt.Sprintf("%d channels", len(m)), m)), m, nil
}

// textResult builds a success result carrying the one-line text; the
// structured map rides in the handler's Out value (domain errors are
// IsError results — see toolErr / wrapDiscordErr — never a Go error).
func textResult(text string) *mcpSDK.CallToolResult {
	return &mcpSDK.CallToolResult{
		Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: text}},
	}
}

// nameIDSummary renders "N guilds: alpha=1, beta=2, …" — sorted for
// determinism, capped at 50 entries with "…" after.
func nameIDSummary(heading string, m map[string]string) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(m))
	for _, name := range names {
		parts = append(parts, name+"="+m[name])
	}
	if len(parts) > 50 {
		parts = parts[:50]
	}
	s := heading + ": " + strings.Join(parts, ", ")
	if len(m) > 50 {
		s += " …"
	}
	return s
}
