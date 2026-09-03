package gulag

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/features"
)

// reaction.go ports src/handlers/gulag/gulag_reaction.rs (179 lines) 1:1,
// including the manual get_reaction_users pagination. Rust's ready()
// wires BOTH reaction_add and reaction_remove to this one handler; the
// handler re-reads the LIVE message, so a removed reaction simply
// re-counts the remaining voters.
//
// Pagination discipline (Rust fetch_all_voters, lines 24-59):
//  - PAGE_SIZE = 100 per call (Discord's cap)
//  - MAX_PAGES = 50 safety cap
//  - IMMEDIATE paging with NO inter-page delay (a per-page sleep would
//    stall up to 50s per reaction event)
//  - ALL bots are filtered (Rust `!u.bot` — every bot, not just this bot)
//  - the exclusion belongs to this handler via the voter LIST, and the
//    FOR UPDATE SKIP LOCKED locking belongs to the background loop
//    queries, NOT here (sync_from_discord is plain non-transactional SQL)

// reactionPageSize is Rust's PAGE_SIZE (Discord's per-call cap).
const reactionPageSize = 100

// reactionMaxPages is Rust's MAX_PAGES safety cap.
const reactionMaxPages = 50

// reactionUserFetcherFunc is the REST seam for reacting user fetches:
// the signature mirrors discordgo.Session.MessageReactions (what Rust's
// get_reaction_users wraps). The production default is the session call;
// tests inject a fake to pin the manual pagination math.
type reactionUserFetcherFunc func(ctx context.Context, channelID, messageID, emojiID string, limit int, beforeID, afterID string) ([]*discordgo.User, error)

// ReactionAdd is the OnReactionAdd entry point (Rust reaction_add). Both
// add and remove route into the same handler.
func (g *Gulag) ReactionAdd(r *discordgo.MessageReaction) {
	g.reactionHandler(context.Background(), r)
}

// ReactionRemove is the OnReactionRemove entry point (Rust reaction_remove).
func (g *Gulag) ReactionRemove(r *discordgo.MessageReaction) {
	g.reactionHandler(context.Background(), r)
}

// reactionEmojiString is the Rust `reaction_type.to_string()` equivalent
// for this discordgo version: a custom emoji serializes to the
// `<name:ID>` form (so the `gulag` trigger check and the per-reaction
// check both see the name); a unicode emoji serializes to
// the glyph itself (it can never contain `gulag`).
func reactionEmojiString(e *discordgo.Emoji) string {
	if e == nil {
		return ""
	}
	return e.MessageFormat()
}

// reactionMatchesGulag mirrors Rust's plain case-SENSITIVE
// `.contains("gulag")` — both the trigger check (gulag_reaction.rs:70-72)
// and the per-reaction scan (:118-121) use the same check, a plain
// substring match with no prefix anchor.
func reactionMatchesGulag(format string) bool {
	return strings.Contains(format, "gulag")
}

// find_emoji — ported for parity; the handler itself does not use
// it, the Rust file carries it as a standalone helper.
//
//nolint:unused // ported for parity (unused in Rust too; kept for the 1:1 port)
func (g *Gulag) findGulagEmoji(ctx context.Context, guildID string) *discordgo.Emoji {
	emojiList, err := g.d.GuildEmojis(guildID)
	if err != nil || len(emojiList) == 0 {
		return nil
	}
	for i := range emojiList {
		if emojiList[i].Name == "gulag" {
			return emojiList[i]
		}
	}
	return nil
}

// reactionHandler is the port of GulagReaction::handler
// (gulag_reaction.rs:62-168): gate order pinned — marker check, feature
// gate, guild guard, live message fetch, the `:gulag` reaction scan, the
// paginated voter tally, then the sync.
func (g *Gulag) reactionHandler(ctx context.Context, add *discordgo.MessageReaction) {
	// Match the emoji with the known gulag emoji (Rust line 64-67:
	// plain case-SENSITIVE substring check on the trigger serialization).
	if !reactionMatchesGulag(reactionEmojiString(&add.Emoji)) {
		return
	}

	// Check if gulag feature is enabled (Rust line 73): the silent
	// IsEnabled flavor.
	if !features.IsEnabled(ctx, g.pool, FeatureKey) {
		return
	}
	// The guild guard (Rust line 76-79).
	if add.GuildID == "" {
		return
	}
	messageID := add.MessageID
	channelID := add.ChannelID

	// Fetch the message to get actual reaction data from Discord (Rust
	// line 81-93): failure logs and returns.
	msg, err := g.d.ChannelMessage(channelID, messageID)
	if err != nil {
		slog.Debug("failed to fetch message", "module", "gulag", "messageID", messageID, "error", err)
		return
	}
	userID := ""
	if msg.Author != nil {
		userID = msg.Author.ID
	}
	userIDi, convErr := DiscordID("user", userID)
	if convErr != nil {
		slog.Error("failed to convert author ID", "module", "gulag", "error", convErr)
		return
	}
	guildIDi, convErr := DiscordID("guild", add.GuildID)
	if convErr != nil {
		slog.Error("failed to convert guild ID", "module", "gulag", "error", convErr)
		return
	}
	channelIDi, convErr := DiscordID("channel", channelID)
	if convErr != nil {
		slog.Error("failed to convert channel ID", "module", "gulag", "error", convErr)
		return
	}
	messageIDi, convErr := DiscordID("message", messageID)
	if convErr != nil {
		slog.Error("failed to convert message ID", "module", "gulag", "error", convErr)
		return
	}

	slog.Debug("message reactions count", "module", "gulag", "messageID", messageID, "count", len(msg.Reactions))

	// Find the :gulag reaction and get all users who reacted (paginated)
	// (Rust lines 110-127): first match wins, failure logs and returns.
	var gulagVoters []int64
	foundGulagReaction := false
	emojiGulag := []string{}
	for i := range msg.Reactions {
		emojiStr := reactionEmojiString(msg.Reactions[i].Emoji)
		slog.Debug("checking reaction", "module", "gulag", "emoji", emojiStr, "count", msg.Reactions[i].Count)
		if !reactionMatchesGulag(emojiStr) {
			continue
		}
		foundGulagReaction = true
		voters, err := g.fetchAllVoters(ctx, channelID, messageID, msg.Reactions[i].Emoji)
		if err != nil {
			slog.Error("failed to fetch reaction users", "module", "gulag", "error", err)
			return
		}
		gulagVoters = voters
		slog.Debug("found gulag reaction", "module", "gulag", "voters", len(voters))
		break
	}
	if !foundGulagReaction {
		for i := range msg.Reactions {
			emojiGulag = append(emojiGulag, reactionEmojiString(msg.Reactions[i].Emoji))
		}
		slog.Debug("no match - trigger not found", "module", "gulag", "trigger", reactionEmojiString(&add.Emoji), "reactions", emojiGulag)
	}

	// Sync database with actual Discord reaction data (Rust lines 165-170).
	if _, err := g.syncFromDiscord(ctx, messageIDi, guildIDi, channelIDi, userIDi, gulagVoters); err != nil {
		slog.Error("error syncing votes", "module", "gulag", "error", err)
	} else {
		slog.Debug("synced votes for message", "module", "gulag", "messageID", messageID)
	}
}

// fetchAllVoters ports fetch_all_voters (gulag_reaction.rs:24-59):
// manual 100-per-call pagination, 50-page cap, immediate (no inter-page
// delay), ALL bots filtered, MAX_PAGES warning.
func (g *Gulag) fetchAllVoters(ctx context.Context, channelID, messageID string, reactionType *discordgo.Emoji) ([]int64, error) {
	fetch := g.reactionUserFetcher
	if fetch == nil {
		fetch = func(_ context.Context, channelID, messageID, emojiID string, limit int, beforeID, afterID string) ([]*discordgo.User, error) {
			return g.d.MessageReactions(channelID, messageID, emojiID, limit, beforeID, afterID)
		}
	}
	all := make([]*discordgo.User, 0)
	var after string
	for pageNum := 0; pageNum < reactionMaxPages; pageNum++ {
		page, err := fetch(ctx, channelID, messageID, reactionEmojiString(reactionType), reactionPageSize, "", after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		if len(page) < reactionPageSize {
			break
		}
		after = page[len(page)-1].ID
		if pageNum+1 == reactionMaxPages {
			slog.Warn("hit MAX_PAGES cap, aborting pagination", "module", "gulag", "messageID", messageID, "maxPages", reactionMaxPages)
		}
	}
	voters := make([]int64, 0, len(all))
	for _, u := range all {
		if u == nil || u.Bot {
			continue
		}
		v, err := strconv.ParseInt(u.ID, 10, 64)
		if err != nil {
			slog.Error("failed to parse voter ID", "module", "gulag", "id", u.ID, "error", err)
			continue
		}
		voters = append(voters, v)
	}
	return voters, nil
}
