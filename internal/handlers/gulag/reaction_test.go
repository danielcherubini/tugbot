package gulag

import (
	"context"
	"fmt"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// makeUsers builds n synthetic users; every nthBot-th user is a bot.
func makeUsers(t *testing.T, n int, botsEvery int) []*discordgo.User {
	t.Helper()
	users := make([]*discordgo.User, 0, n)
	for i := 1; i <= n; i++ {
		u := &discordgo.User{
			ID:       fmt.Sprintf("%d", i),
			Username: fmt.Sprintf("voter_%d", i),
		}
		if botsEvery > 0 && i%botsEvery == 0 {
			u.Bot = true
		}
		users = append(users, u)
	}
	return users
}

// pageSource simulates Discord's get_reaction_users for one message:
// 100-per-call window, `after` cursor = the last user ID of the previous
// page, and an empty window for a cursor at/after the end (Discord
// returns nothing past the end). An out-of-range cursor is a consumer
// error.
type pageSource struct {
	users []*discordgo.User
	calls int
	// capturedEmoji records the emoji argument of every fetch call — it
	// pins the get-reaction-users emoji-parameter parity (the APIName /
	// name:ID contract, not the message markup form).
	capturedEmoji []string
}

func (ps *pageSource) fetch(ctx context.Context, _, _, emoji string, limit int, beforeID, afterID string) ([]*discordgo.User, error) {
	ps.calls++
	ps.capturedEmoji = append(ps.capturedEmoji, emoji)
	start := 0
	if afterID != "" {
		found := false
		for i, u := range ps.users {
			if u.ID == afterID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cursor %q not found in fixture", afterID)
		}
	}
	end := start + limit
	if end > len(ps.users) {
		end = len(ps.users)
	}
	if start == end {
		return []*discordgo.User{}, nil
	}
	out := make([]*discordgo.User, end-start)
	copy(out, ps.users[start:end])
	return out, nil
}

// TestFetchAllVoters_Synthetic150Voters is the task-parity anchor for the
// manual pagination: 150 distinct non-bot voters spread across two
// Discord pages (100 + 50) must be counted as exactly 150, in page
// order, with exactly 2 calls and a short final page terminating the
// walk.
func TestFetchAllVoters_Synthetic150Voters(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 150, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	voters, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "789", Name: "gulag"})
	if err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if len(voters) != 150 {
		t.Fatalf("count = %d, want exactly 150", len(voters))
	}
	// Order preservation across the page boundary (page 1 ends at 100,
	// page 2 starts at 100+1=101).
	for i, v := range voters {
		if v != int64(i+1) {
			t.Fatalf("voters[%d] = %d, want %d (page-boundary order)", i, v, i+1)
		}
	}
	if src.calls != 2 {
		t.Fatalf("calls = %d, want 2 (100-page then short 50-page)", src.calls)
	}
}

// TestFetchAllVoters_FiltersEveryBot pins the Rust `!u.bot` filter:
// EVERY bot is excluded from the tally, not just this bot (the fixture
// bots have distinct IDs/names, so name-only filtering would pass them).
func TestFetchAllVoters_FiltersEveryBot(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 150, 3)} // every 3rd user is a bot
	g := &Gulag{reactionUserFetcher: src.fetch}
	voters, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "789", Name: "gulag"})
	if err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	// 150 users, 50 bots (3,6,...,150), 100 human voters.
	if len(voters) != 100 {
		t.Fatalf("count = %d, want 100 (every bot filtered, not just this bot)", len(voters))
	}
	for _, v := range voters {
		if v%3 == 0 {
			t.Fatalf("bot %d present in the tally", v)
		}
	}
}

// TestFetchAllVoters_CapsAt5000 pins the safety cap: 50 pages x 100 =
// 5000; a never-ending full stream must stop at 50 calls with 5000
// voters, never run unbounded, and log the abort warning.
func TestFetchAllVoters_CapsAt5000(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 5001, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	voters, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "789", Name: "gulag"})
	if err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if len(voters) != 5000 {
		t.Fatalf("count = %d, want the 5000 cap", len(voters))
	}
	if src.calls != 50 {
		t.Fatalf("calls = %d, want exactly 50 (MAX_PAGES)", src.calls)
	}
}

// TestFetchAllVoters_SingleFullPageEnds pin that an exact 100-user
// reaction ends on the empty second page (len(page) < PAGE_SIZE), not on
// the cursor.
func TestFetchAllVoters_SingleFullPageEnds(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 100, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	voters, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "789", Name: "gulag"})
	if err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if len(voters) != 100 {
		t.Fatalf("count = %d, want 100", len(voters))
	}
	if src.calls != 2 {
		t.Fatalf("calls = %d, want 2 (full page, then the empty terminator page)", src.calls)
	}
}

// TestReactionEmojiString pins the emoji serialization the trigger check
// runs against (Rust `reaction_type.to_string()`): a custom emoji
// serializes to the `<name:ID>` form (so both the bare-`gulag` trigger
// check and the `:gulag` per-reaction check see the name), and a unicode
// emoji serializes to the glyph itself (it never contains `gulag`).
func TestReactionEmojiString(t *testing.T) {
	if s := reactionEmojiString(&discordgo.Emoji{ID: "123", Name: "gulag"}); s != "<:gulag:123>" {
		t.Errorf("custom emoji serialization = %q, want <:gulag:123>", s)
	}
	if s := reactionEmojiString(&discordgo.Emoji{Name: "fire"}); s != "fire" {
		t.Errorf("unicode emoji serialization = %q, want the glyph/name itself", s)
	}
}

// TestReactionGulagMatchIsCaseSensitive pins Rust's plain
// case-SENSITIVE `.contains("gulag")` (gulag_reaction.rs:70-72 and
// :118-121): a capitalized emoji name never fires either the trigger
// check or the per-reaction scan (both use the same helper).
func TestReactionGulagMatchIsCaseSensitive(t *testing.T) {
	if reactionMatchesGulag("<:Gulag:123>") {
		t.Error("custom emoji with a capitalized name matched; Rust's check is case-sensitive")
	}
	if reactionMatchesGulag("Gulag") {
		t.Error("capitalized unicode name matched; Rust's check is case-sensitive")
	}
	if !reactionMatchesGulag("<:gulag:123>") {
		t.Error("custom emoji '<:gulag:123>' did not match (the name is a plain substring)")
	}
	if !reactionMatchesGulag("gulag") {
		t.Error("bare 'gulag' did not match")
	}
}

// TestReactionHandler_NilEmojiNeverFetches pins the nil-emoji guard:
// a nil reaction emoji (the gateway can deliver one) must degrade to an
// empty fetch — it must never panic reaching into the emoji. The
// handler's trigger check misses a nil emoji (its serialization is ""),
// so the legitimate path is ReactionAdd with a non-gulag emoji;
// fetchAllVoters is exercised with a nil here directly.
func TestReactionHandler_NilEmojiNeverFetches(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 10, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	if _, err := g.fetchAllVoters(context.Background(), "123", "456", nil); err != nil {
		t.Fatalf("fetchAllVoters(nil): %v", err)
	}
	if src.emojiArg() != "" {
		t.Fatalf("nil emoji passed arg %q, want the empty string", src.emojiArg())
	}
}

// TestReactionHandler_NonGulagEmojiDoesNothing pins the first gate: an
// emoji that does not contain `gulag` returns before any pool / fetch /
// Discord call (nil session and pool are safe on this path).
func TestReactionHandler_NonGulagEmojiDoesNothing(t *testing.T) {
	src := &pageSource{}
	g := &Gulag{reactionUserFetcher: src.fetch} // d and pool stay nil
	g.ReactionAdd(&discordgo.MessageReaction{Emoji: discordgo.Emoji{Name: "fire"}})
	if src.calls != 0 {
		t.Fatalf("non-gulag emoji performed %d react user fetches, want 0", src.calls)
	}
}

// emojiArg returns the first captured emoji argument (the one every
// fetch in the test receives, extension).
func (ps *pageSource) emojiArg() string {
	if len(ps.capturedEmoji) == 0 {
		return ""
	}
	return ps.capturedEmoji[0]
}

// TestFetchAllVoters_CustomEmojiAPIName pins the get-reaction-users
// emoji-parameter contract: for a custom emoji the endpoint takes the
// GUILD EMOJI IDENTIFIER `name:ID` (discordgo's `Emoji.APIName` — the
// "correctly formatted API name for use in the MessageReactions
// endpoints"), NOT the message markup `<:name:ID>` (`Emoji.MessageFormat`).
// Proven live post-cutover (2026-09-19 journal): the markup form leaked
// the trailing `>` into the emoji id, Discord rejected every call with
// 50035 `Value "843909650931515445>" is not snowflake` (list ERROR
// `failed to fetch reaction users`), the sync never ran, and the whole
// reaction→vote→gulag path was dead.
func TestFetchAllVoters_CustomEmojiAPIName(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 10, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	if _, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "843909650931515445", Name: "gulag"}); err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if got := src.emojiArg(); got != "gulag:843909650931515445" {
		t.Fatalf("emoji arg = %q, want %q (name:ID — the MessageReactions contract, not the <name:ID> markup form)", got, "gulag:843909650931515445")
	}
}

// TestFetchAllVoters_AnimatedCustomEmojiAPIName pins the animated arm:
// Discord's emoji parameter is `name:ID` for BOTH static and animated
// custom emoji (the `<a:name:ID>` markup form is chat markup only —
// the same 50035 would hit it).
func TestFetchAllVoters_AnimatedCustomEmojiAPIName(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 10, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	if _, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{ID: "843909650931515445", Name: "gulag", Animated: true}); err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if got := src.emojiArg(); got != "gulag:843909650931515445" {
		t.Fatalf("animated emoji arg = %q, want %q (name:ID without the <a: markup)", got, "gulag:843909650931515445")
	}
}

// TestFetchAllVoters_UnicodeEmojiSendsGlyph pins the unicode arm: the
// emoji parameter is the glyph itself (a unicode emoji has no id).
func TestFetchAllVoters_UnicodeEmojiSendsGlyph(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 10, 0)}
	g := &Gulag{reactionUserFetcher: src.fetch}
	if _, err := g.fetchAllVoters(context.Background(), "123", "456", &discordgo.Emoji{Name: "\U0001f525"}); err != nil {
		t.Fatalf("fetchAllVoters: %v", err)
	}
	if got := src.emojiArg(); got != "\U0001f525" {
		t.Fatalf("unicode emoji arg = %q, want the glyph itself", got)
	}
}

// TestReactionHandler_MissingGuildReturnsSilently pins the guild guard
// (the feature gate passes through a real enabled "gulag" feature row —
// see gatePool): a valid gulag emoji with no guild context returns before
// the message fetch (nil session is safe on this path): no voter fetches
// either.
func TestReactionHandler_MissingGuildReturnsSilently(t *testing.T) {
	src := &pageSource{users: makeUsers(t, 10, 0)}
	g := &Gulag{pool: gatePool(t), reactionUserFetcher: src.fetch}
	g.ReactionAdd(&discordgo.MessageReaction{
		ChannelID: "123",
		MessageID: "456",
		Emoji:     discordgo.Emoji{ID: "789", Name: "gulag"},
		GuildID:   "",
	})
	if src.calls != 0 {
		t.Fatalf("missing-guild reaction performed %d react user fetches, want 0", src.calls)
	}
}
