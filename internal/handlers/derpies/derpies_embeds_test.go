// Package derpies — embed-title tests. The observed survival vector is a
// Klipy gifv embed: a bare-URL message whose actual brand word lives in
// the EMBED TITLE. embedTitleText makes every non-empty
// MessageEmbed.Title part of the judged text (fast path + {{EMBED}}
// prompt block), the same way referenced-message content already is.
// In-package test style: newTestDerpies + the fakes from derpies_test.go,
// messages built as real *discordgo.Message.
package derpies

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// derpMsgEmbed — as in derpMsg, but the message carries ONE embed with
// the given title.
func derpMsgEmbed(content, title string) *discordgo.Message {
	m := derpMsg(content)
	m.Embeds = []*discordgo.MessageEmbed{{Title: title}}
	return m
}

// ---------------------------------------------------------------------------
// embedTitleText
// ---------------------------------------------------------------------------

func TestEmbedTitleText(t *testing.T) {
	t.Run("nil message is empty", func(t *testing.T) {
		if got := embedTitleText(nil); got != "" {
			t.Errorf("embedTitleText(nil) = %q, want \"\"", got)
		}
	})
	t.Run("no embeds is empty", func(t *testing.T) {
		if got := embedTitleText(&discordgo.Message{}); got != "" {
			t.Errorf("embedTitleText(no embeds) = %q, want \"\"", got)
		}
	})
	t.Run("two embeds with one empty title yield the non-empty title only", func(t *testing.T) {
		m := &discordgo.Message{
			Embeds: []*discordgo.MessageEmbed{
				{Title: "first clip"},
				{Title: ""},
			},
		}
		if got := embedTitleText(m); got != "first clip" {
			t.Errorf("embedTitleText = %q, want \"first clip\"", got)
		}
	})
	t.Run("12 titles cap at 10, in embed order", func(t *testing.T) {
		var embeds []*discordgo.MessageEmbed
		for i := 1; i <= 12; i++ {
			embeds = append(embeds, &discordgo.MessageEmbed{Title: fmt.Sprintf("title-%02d", i)})
		}
		m := &discordgo.Message{Embeds: embeds}
		var want []string
		for i := 1; i <= 10; i++ {
			want = append(want, fmt.Sprintf("title-%02d", i))
		}
		if got := embedTitleText(m); got != strings.Join(want, "\n") {
			t.Errorf("embedTitleText = %q, want capped at the first 10 (%q)", got, strings.Join(want, "\n"))
		}
	})
	t.Run("a 300-rune (600-byte) title is capped at exactly 200 runes, valid UTF-8", func(t *testing.T) {
		m := &discordgo.Message{
			Embeds: []*discordgo.MessageEmbed{
				{Title: strings.Repeat("\u00e9", 300)}, // é is 2 bytes: 300 runes / 600 bytes
			},
		}
		got := embedTitleText(m)
		if n := utf8.RuneCountInString(got); n != 200 {
			t.Errorf("runes in capped title = %d, want 200", n)
		}
		if !utf8.ValidString(got) {
			t.Errorf("capped title contains invalid UTF-8 (a byte cut must not split a codepoint): %q", got)
		}
		if got != strings.Repeat("\u00e9", 200) {
			t.Errorf("capped title = %d bytes with %q prefix, want exactly 200 repetitions of é", len(got), got[:10])
		}
	})
	t.Run("a 150-rune (300-byte) title is returned in full — the cap is runes, not bytes", func(t *testing.T) {
		m := &discordgo.Message{
			Embeds: []*discordgo.MessageEmbed{
				{Title: strings.Repeat("\u00e9", 150)},
			},
		}
		got := embedTitleText(m)
		if n := utf8.RuneCountInString(got); n != 150 {
			t.Errorf("runes in the 150-rune title = %d, want 150 (a byte-based cap would mangle it)", n)
		}
	})
}

// ---------------------------------------------------------------------------
// gimmickPrompt — the {{EMBED}} block (6-arg shipped form)
// ---------------------------------------------------------------------------

func TestGimmickPromptEmbed(t *testing.T) {
	known := []string{"bike"}

	t.Run("titles set: the block is substituted, the payload marker survives exactly once", func(t *testing.T) {
		got := gimmickPrompt(defaultPromptTemplate, "x {{EMBED}}", known, 0, 0, "some title", "", nil)
		if !strings.Contains(got, "TITLES OF MEDIA EMBEDDED") {
			t.Errorf("prompt with titles must contain the TITLES OF MEDIA EMBEDDED block header:\n%s", got)
		}
		if !strings.Contains(got, "some title") {
			t.Errorf("prompt must carry the title payload:\n%s", got)
		}
		// The substituted block header does NOT contain the marker; the ONE
		// {{EMBED}} occurrence is the payload's literal in `content`, surviving
		// verbatim — a re-scan would have consumed it.
		if n := strings.Count(got, "{{EMBED}}"); n != 1 {
			t.Errorf("{{EMBED}} appears %d times, want exactly 1 (the payload's literal, verbatim):\n%s", n, got)
		}
	})

	t.Run("empty titles: the block is absent", func(t *testing.T) {
		got := gimmickPrompt(defaultPromptTemplate, "x {{EMBED}}", known, 0, 0, "", "", nil)
		if strings.Contains(got, "TITLES OF MEDIA EMBEDDED") {
			t.Errorf("no-title form must not contain the TITLES OF MEDIA EMBEDDED block:\n%s", got)
		}
	})

	t.Run("a template without {{EMBED}} degrades by omission (titles set)", func(t *testing.T) {
		noEmbed := strings.ReplaceAll(defaultPromptTemplate, "{{EMBED}}\n", "")
		got := gimmickPrompt(noEmbed, "x", known, 0, 0, "some title", "", nil)
		if strings.Contains(got, "TITLES OF MEDIA EMBEDDED") {
			t.Errorf("a template without the {{EMBED}} marker must not contain the block (degradation by omission):\n%s", got)
		}
		if !strings.Contains(got, "<<<UNTRUSTED MESSAGE") {
			t.Errorf("the rest of the prompt must stay intact:\n%s", got)
		}
	})
}

// ---------------------------------------------------------------------------
// The flow — embed titles join the judged text
// ---------------------------------------------------------------------------

func TestFlowFrameWordVerdictPassesWhenPostedMessageHasNoWords(t *testing.T) {
	// The anchor gate's scope change (the emoji fix A, generalized): the
	// anchor requirement is scoped to the POSTED message's word-like
	// tokens — the embed title's tokens no longer count as "the message
	// has other text words". The post has NO text (content ""), so a
	// valid ASCII word from the image frame passes wordValid alone: it
	// is learned and deleted, the same as an image-only post. Pre-score-
	// model this exact message was REJECTED (the title's tokens made the
	// union non-empty and the frame word was not in them) — the score
	// model's anchor scope is the intentional relaxation.
	srv, _ := newImgServer(t)
	pi := &fakePi{resp: "SCORE:95\nWORD:frameword"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60} // empty word list
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	m := msgWithImage("", srv.URL+"/a.png") // plain image attachment
	m.Embeds = []*discordgo.MessageEmbed{{Title: "an ordinary clip"}}
	h.flow(m)

	if len(store.added) != 1 || store.added[0] != "frameword|llm" {
		t.Errorf("added = %v, want [frameword|llm] (a word-less post's valid ASCII word passes wordValid alone)", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.imageAsks != 1 {
		t.Errorf("pi.imageAsks = %d, want 1 (the word is judged from the frame)", pi.imageAsks)
	}
}

func TestFlowEmbedTitleFastHitDeletes(t *testing.T) {
	// The prod survival vector: a bare-URL gifv post (no list words in the
	// text) whose embed title carries the known word — the title's token
	// union makes it a fast hit: one delete, ZERO pi RPC asks.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsgEmbed("https://klipy.example/gifs/x", "Indoor Cycling with Zwift Virtual Ride"))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one fast delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 || pi.imageAsks != 0 {
		t.Errorf("pi.asks = %d, pi.imageAsks = %d, want both 0 (fast path, zero asks)", pi.asks, pi.imageAsks)
	}
	assertNothingLearned(t, store)
}

func TestFlowEmbedTitleSlowPathLearnsAndDeletes(t *testing.T) {
	// A novel respelling named only in the embed title: no fast hit, the
	// slow path judges it, and the gate accepts the word because the
	// posted message has no word-like tokens (the anchor requirement
	// drops — the title's tokens no longer count as "the message has
	// other text words"): learned (source 'llm') and the message deleted.
	pi := &fakePi{resp: "SCORE:95\nWORD:swwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsgEmbed("", "unusual swwift"))

	if len(store.added) != 1 || store.added[0] != "swwift|llm" {
		t.Errorf("added = %v, want [swwift|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (slow path)", pi.asks)
	}
	if len(pi.prompts) != 1 || !strings.Contains(pi.prompts[0], "TITLES OF MEDIA EMBEDDED") {
		t.Errorf("the ask prompt must carry the embed-title block with the title")
	}
}

func TestFlowEmbedTitleWordNotLearnedWithoutTitle(t *testing.T) {
	// Regression guard: a verdict word absent from ALL judged text (no
	// embeds) is neither learned nor deleted — the anchor gate is
	// unchanged in kind. The 40..T-1 band attempts the learn; the word
	// rejection is what keeps it out.
	pi := &fakePi{resp: "SCORE:45\nWORD:swwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("hello"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (judged, then the gate rejects)", pi.asks)
	}
}
