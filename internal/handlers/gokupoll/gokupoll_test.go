package gokupoll

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestFeatureKey pins the feature flag key (goku_poll.rs:36).
func TestFeatureKey(t *testing.T) {
	if FeatureKey != "goku_poll" {
		t.Errorf("FeatureKey = %q, want %q", FeatureKey, "goku_poll")
	}
}

// winAnswerText is the pure port of the winning-answer extraction
// (goku_poll.rs:51-65): highest vote count; no votes cast (count == 0) ->
// ""; answer missing or text missing -> "". Ties resolve to the FIRST
// highest count entry (Rust iter().max_by_key keeps the first maximal
// element).
//
// Fixtures build the vendored discordgo v0.29.0 Poll types directly.

func str(s string) *string { return &s }

func pollFixture(finalized bool, counts []*discordgo.PollAnswerCount, answers []discordgo.PollAnswer) *discordgo.Poll {
	return &discordgo.Poll{
		Answers: answers,
		Results: &discordgo.PollResults{
			Finalized:    finalized,
			AnswerCounts: counts,
		},
	}
}

func textAnswer(id int, text *string) discordgo.PollAnswer {
	if text == nil {
		return discordgo.PollAnswer{AnswerID: id}
	}
	return discordgo.PollAnswer{
		AnswerID: id,
		Media:    &discordgo.PollMedia{Text: *text},
	}
}

// TestWinningAnswerDetection pins the poll-final-winner->text detection.
func TestWinningAnswerDetection(t *testing.T) {
	tests := []struct {
		name string
		poll *discordgo.Poll
		want string
	}{
		{
			"two answers, second wins",
			pollFixture(true,
				[]*discordgo.PollAnswerCount{{ID: 0, Count: 10}, {ID: 1, Count: 30}},
				[]discordgo.PollAnswer{textAnswer(0, str("slowpoke")), textAnswer(1, str("Goku wins"))},
			),
			"Goku wins",
		},
		{
			"no votes cast (all zero counts)",
			pollFixture(true,
				[]*discordgo.PollAnswerCount{{ID: 0, Count: 0}, {ID: 1, Count: 0}},
				[]discordgo.PollAnswer{textAnswer(0, str("a")), textAnswer(1, str("Goku"))},
			),
			"",
		},
		{
			"winning answer has no media text",
			pollFixture(true,
				[]*discordgo.PollAnswerCount{{ID: 0, Count: 1}},
				[]discordgo.PollAnswer{textAnswer(0, nil)},
			),
			"",
		},
		{
			"winning answer id not in answers",
			pollFixture(true,
				[]*discordgo.PollAnswerCount{{ID: 7, Count: 5}},
				[]discordgo.PollAnswer{textAnswer(0, str("a"))},
			),
			"",
		},
		{
			"tie resolves to first maximal count entry",
			pollFixture(true,
				[]*discordgo.PollAnswerCount{{ID: 0, Count: 5}, {ID: 1, Count: 5}},
				[]discordgo.PollAnswer{textAnswer(0, str("first")), textAnswer(1, str("Goku"))},
			),
			"first",
		},
		{
			"unfinalized poll -> no processing",
			pollFixture(false,
				[]*discordgo.PollAnswerCount{{ID: 0, Count: 3}},
				[]discordgo.PollAnswer{textAnswer(0, str("Goku"))},
			),
			"",
		},
		{
			"nil results -> no processing",
			&discordgo.Poll{Answers: []discordgo.PollAnswer{textAnswer(0, str("Goku"))}},
			"",
		},
		{
			"nil poll -> no processing",
			nil,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := winningAnswerText(tt.poll); got != tt.want {
				t.Errorf("winningAnswerText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNextOffenseDurationError pins the canonical next-offense conversion:
// saturating_add(1) then the duration lookup (goku_poll.rs:114-120 — a
// lapsed count would clamp to the 30-day fallback in the write path, but
// this helper returns the raw value for the formatted message).
func TestNextOffense(t *testing.T) {
	if got := NextOffenseDuration(0); got != 3600 {
		t.Errorf("NextOffenseDuration(0) = %d, want 3600", got)
	}
	if got := NextOffenseDuration(20); got != 3_774_873_600 {
		t.Errorf("NextOffenseDuration(20) (count 21) = %d, want 3_774_873_600", got)
	}
}

// TestMessageNeedsFetch pins the mod.rs message-update fetch branch: the
// payload's message is missing/empty (partial update) -> fetch from the
// channel; nil event message -> nothing to fetch.
func TestMessageNeedsFetch(t *testing.T) {
	if !messageNeedsFetch(nil) {
		t.Error("nil update message: want fetch")
	}
	if !messageNeedsFetch(&discordgo.Message{}) { // empty content/empty poll update
		t.Error("empty update message: want fetch")
	}
	full := &discordgo.Message{Content: "hi"}
	if messageNeedsFetch(full) {
		t.Error("populated update message: want no fetch")
	}
}
