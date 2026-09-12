// Edit-flow tests (the GUILD_MESSAGE_UPDATE re-judgment). The flow is
// tested exactly like the message flow: same-state construction via
// newTestDerpies and the fake seams — the update payload is judged by
// calling h.editFlow(evt) DIRECTLY (synchronous, the house discipline of
// every existing flow test); MessageUpdate only spawns the goroutine and
// is not called here (it would race the assertions).
package derpies

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// editUser — the exact gated-user ID constant the derpMsg /
// newTestDerpies message tests hard-code (newTestDerpies hard-codes
// Cfg.DerpiesUserIDs to that single ID, so only it passes the author
// gate; nicknames_test.go's filteredNickUser is the same value under its
// own name).
const editUser = "163055057254875136"

// editEvent — a GUILD_MESSAGE_UPDATE payload from the filtered user (m1
// in c1 in guild g1) with the given content.
func editEvent(content string) *discordgo.MessageUpdate {
	return &discordgo.MessageUpdate{Message: &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author:  &discordgo.User{ID: editUser},
		Content: content,
	}}
}

// ---------------------------------------------------------------------------
// Gates
// ---------------------------------------------------------------------------

func TestEditFlowNotGatedAuthor(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	evt := editEvent("who's giving me a sw1ft.")
	evt.Author = &discordgo.User{ID: "222"}
	h.editFlow(evt)

	assertNoDeletes(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (author gate must block before the list fetch)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	if ops.refCalls != 0 {
		t.Errorf("refCalls = %d, want 0 (author gate must block before any fetch)", ops.refCalls)
	}
}

func TestEditFlowNoGuild(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	evt := editEvent("who's giving me a sw1ft.")
	evt.GuildID = ""
	h.editFlow(evt)

	assertNoDeletes(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (no guild -> return before the list fetch)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	if ops.refCalls != 0 {
		t.Errorf("refCalls = %d, want 0 (guild guard runs before the fetch)", ops.refCalls)
	}
}

func TestEditFlowFeatureOff(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: false}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent("who's giving me a sw1ft."))

	assertNoDeletes(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (feature gate is the first gate)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	if ops.refCalls != 0 {
		t.Errorf("refCalls = %d, want 0 (feature gate runs before the fetch)", ops.refCalls)
	}
}

// ---------------------------------------------------------------------------
// The flow (payload carries content — judged in place, no fetch)
// ---------------------------------------------------------------------------

func TestEditFlowFastPath(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent("who's giving me a sw1ft."))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "m1" {
		t.Errorf("deleted = %v, want [[c1 m1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}
	if ops.refCalls != 0 {
		t.Errorf("refCalls = %d, want 0 (the payload had content — no fetch)", ops.refCalls)
	}
	assertNothingLearned(t, store)
}

func TestEditFlowSlowPathLearnsDeletes(t *testing.T) {
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent("holler at zswiftf now"))

	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (novel content goes to the slow path)", pi.asks)
	}
	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "m1" {
		t.Errorf("deleted = %v, want [[c1 m1]]", ops.deleted)
	}
}

// ---------------------------------------------------------------------------
// The bare-payload fetch branch (the mod.rs:126-136 port — the STRICT
// trigger deviation: fetch only when there is NO content AND NO
// attachments; an attachments-present payload is judged in place)
// ---------------------------------------------------------------------------

func TestEditFlowEmptyPayloadFetches(t *testing.T) {
	// The fetched ref MUST carry GuildID + Author — flow's gates re-run on
	// the fetched message (a missing Author panics in flow's author gate;
	// a missing GuildID silently no-ops it). The fake's ref/refErr double
	// as the channelMessageRetrieve stub.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{ref: &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author:  &discordgo.User{ID: editUser},
		Content: "who's giving me a sw1ft.",
	}}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent(""))

	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want 1 (empty payload with no attachments must fetch once)", ops.refCalls)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "m1" {
		t.Errorf("deleted = %v, want [[c1 m1]] (the fetched content deleted)", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path hit on the fetched content)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestEditFlowEmptyPayloadFetchFails(t *testing.T) {
	// A fetch failure degrades: log + skip, never abort — no list fetch,
	// no ask, no delete.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{refErr: errors.New("boom")}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent(""))

	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want 1 (the fetch was attempted exactly once)", ops.refCalls)
	}
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (fetch failure degrades before the flow)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fetch failure degrades before the flow)", pi.asks)
	}
	assertNoDeletes(t, ops)
}

func TestEditFlowFetchedMessageNoAuthor(t *testing.T) {
	// The bare-payload fetch replaces m with the fetched message, which
	// then re-runs flow's gates — including the step-3 author gate that
	// dereferences m.Author.ID with NO nil check. discordgo v0.29.0
	// documents Message.Author as not guaranteed, so a nil-Author fetched
	// message must be caught at the new post-fetch guard (log + return),
	// never panicked on in the flow.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{ref: &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Content: "seededword", // no Author set — the guard must catch it
	}}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent(""))

	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want 1 (the empty payload fired the fetch)", ops.refCalls)
	}
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (the no-author guard returns before flow's list fetch)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the flow never ran)", pi.asks)
	}
	assertNoDeletes(t, ops)
}

// bodyServer — a server answering 200 with the given body; the body is
// the image CONTENT (two server bodies = two image contents).
type requestAlias = http.Request

func bodyServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *requestAlias) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEditFlowEmptyTextWithAttachmentsJudgedInPlace(t *testing.T) {
	// The STRICT trigger deviation, pinned: Content == "" WITH an
	// attachment present is judged in place — ZERO fetches (gokupoll
	// would fetch on empty content alone). The payload's image goes to
	// the slow path exactly once; a CLEAN verdict deletes nothing.
	srv := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x09})
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	evt := &discordgo.MessageUpdate{Message: &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author:      &discordgo.User{ID: editUser},
		Content:     "",
		Attachments: []*discordgo.MessageAttachment{{URL: srv.URL + "/a.png", ContentType: "image/png"}},
	}}
	h.editFlow(evt)

	if ops.refCalls != 0 {
		t.Errorf("refCalls = %d, want 0 (an attachment-bearing payload is judged in place — NO fetch)", ops.refCalls)
	}
	if pi.imageAsks != 1 {
		t.Errorf("pi.imageAsks = %d, want 1 (the attachment-only payload went to the slow path in place)", pi.imageAsks)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the judge is the image ask)", pi.asks)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestEditFlowFetchedSlowPathLearnsDeletes(t *testing.T) {
	// The bare payload fetches; the fetched message carries NOVEL content
	// (no seeded token) -> the slow path runs exactly once on the FETCHED
	// message: one ask, one learn, one delete of the fetched ref's ID.
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{
		ID: "m1", ChannelID: "c1", GuildID: "g1",
		Author:  &discordgo.User{ID: editUser},
		Content: "holler at zswiftf now",
	}}
	h := newTestDerpies(store, ops, pi)
	h.editFlow(editEvent(""))

	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want exactly 1", ops.refCalls)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (novel fetched content goes to the slow path exactly once)", pi.asks)
	}
	// The judgment must be OF THE FETCHED content, not the empty event
	// payload: the ask prompt embeds the judged content, so it must carry
	// the fetched ref's text. The other assertions pass vacuously if the
	// bare-payload ask judged Content "" instead (wordValid still passes
	// on a no-text ask — the token-membership gate is skipped — and the
	// learn+delete still fire on the ref's ID); the prompt check is what
	// discriminates that flow really ran on the fetched message.
	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one (the fetched content is what got judged)", pi.prompts)
	}
	if !strings.Contains(pi.prompts[0], "holler at zswiftf now") {
		t.Errorf("prompt = %q, want it to contain the fetched content %q — the ask must judge the FETCHED message, not the empty payload", pi.prompts[0], "holler at zswiftf now")
	}
	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "m1" {
		t.Errorf("deleted = %v, want [[c1 m1]] (the FETCHED message's ID deleted)", ops.deleted)
	}
}
