// Package derpies is the filter for the user(s) in
// config.Config.DerpiesUserIDs: a fast path (exact gimmick-word token
// match against the derpies_gimmicks list) then a slow path (a pi RPC
// verdict — CLEAN / GIMMICK:<word> — that learners new words into the
// list at runtime).
package derpies

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
)

// ---------------------------------------------------------------------------
// Fakes (in-package; mirrors mention_test.go's house style)
// ---------------------------------------------------------------------------

// fakeStore implements the store seam.
type fakeStore struct {
	enabled   map[string]bool
	listErr   error
	words     map[string]bool
	added     []string // each entry "word|source"
	listCalls int
}

func (s *fakeStore) featureEnabled(_ context.Context, key string) bool {
	return s.enabled[key]
}

func (s *fakeStore) listGimmicks(_ context.Context) (map[string]bool, error) {
	s.listCalls++
	return s.words, s.listErr
}

func (s *fakeStore) addGimmick(_ context.Context, word, source string) error {
	s.added = append(s.added, word+"|"+source)
	return nil
}

// fakeOps implements the discordOps seam.
type fakeOps struct {
	deleted [][]string // each {channelID, messageID}
	delErr  error
}

func (o *fakeOps) deleteMessage(channelID, messageID string) error {
	if o.delErr != nil {
		return o.delErr
	}
	o.deleted = append(o.deleted, []string{channelID, messageID})
	return nil
}

// fakePi implements ALL THREE app.PiBackend methods (a fake with only
// Ask will not compile as App.Pi).
type fakePi struct {
	resp    string
	askErr  error
	asks    int
	prompts []string
}

func (f *fakePi) Ask(_ context.Context, prompt string) (string, error) {
	f.asks++
	f.prompts = append(f.prompts, prompt)
	return f.resp, f.askErr
}

func (f *fakePi) AskWithImages(_ context.Context, prompt string, _ []app.PiImage) (string, error) {
	f.asks++
	f.prompts = append(f.prompts, prompt)
	return f.resp, f.askErr
}

func (f *fakePi) Stop() {}

// newTestDerpies builds a Derpies over the fakes. `pi` is the interface
// type so a passed untyped nil yields a TRULY-nil App.Pi (a typed-nil
// *fakePi pointer would defeat the flow's h.app.Pi == nil guard).
func newTestDerpies(store *fakeStore, ops *fakeOps, pi app.PiBackend) *Derpies {
	return &Derpies{
		app: &app.App{
			Cfg: &config.Config{
				DerpiesUserIDs: map[int64]struct{}{163055057254875136: {}},
			},
			Pi: pi,
		},
		store: store,
		ops:   ops,
	}
}

// derpMsg — a message from the filtered user (163055057254875136).
func derpMsg(content string) *discordgo.Message {
	return &discordgo.Message{
		ID:        "msg1",
		GuildID:   "g1",
		ChannelID: "c1",
		Author:    &discordgo.User{ID: "163055057254875136"},
		Content:   content,
	}
}

// otherMsg — a message from a user who is NOT in DerpiesUserIDs.
func otherMsg(content string) *discordgo.Message {
	m := derpMsg(content)
	m.Author = &discordgo.User{ID: "222"}
	return m
}

// assertNoDeletes / assertNothingLearned — shared assertions.
func assertNoDeletes(t *testing.T, ops *fakeOps) {
	t.Helper()
	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v, want none", ops.deleted)
	}
}

func assertNothingLearned(t *testing.T, store *fakeStore) {
	t.Helper()
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty", store.added)
	}
}

// ---------------------------------------------------------------------------
// tokensForMatch
// ---------------------------------------------------------------------------

func TestTokensForMatch(t *testing.T) {
	got := tokensForMatch("Who's giving me a sw1ft.")
	want := map[string]bool{"who's": true, "giving": true, "me": true, "a": true, "sw1ft": true}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want keys %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("tokens missing key %q: %v", k, got)
		}
	}

	if got2 := tokensForMatch("SWIFT A"); len(got2) != 2 || !got2["swift"] || !got2["a"] {
		t.Errorf("tokensForMatch(\"SWIFT A\") = %v, want {swift, a}", got2)
	}
}

// ---------------------------------------------------------------------------
// wordValid
// ---------------------------------------------------------------------------

func TestWordValid(t *testing.T) {
	tests := []struct {
		w    string
		want bool
	}{
		{"sw1ft", true},
		{"zswiftf", true},
		{strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false},
		{"a", false},
		{"s-w1ft", false},
		{"swïft", false},
		{"sw1ft!", false},
	}
	for _, tt := range tests {
		if got := wordValid(tt.w); got != tt.want {
			t.Errorf("wordValid(%q) = %v, want %v", tt.w, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseVerdict
// ---------------------------------------------------------------------------

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		in       string
		wantKind string
		wantWord string
	}{
		{"GIMMICK:sw1ft", "gimmick", "sw1ft"},
		{"gimmick:SW1FT", "gimmick", "sw1ft"},
		{"clean", "clean", ""},
		{"\n  CLEAN  ", "clean", ""},
		{"GIMMICK sw1ft", "unknown", ""}, // no colon
		{"MAYBE", "unknown", ""},
		{"", "unknown", ""},
		{"GIMMICK: zswiftf", "gimmick", "zswiftf"}, // space after the colon: remainder trimmed
		{"GIMMICK:", "gimmick", ""},                // empty word: the validity gate then rejects it
	}
	for _, tt := range tests {
		kind, word := parseVerdict(tt.in)
		if kind != tt.wantKind || word != tt.wantWord {
			t.Errorf("parseVerdict(%q) = (%q, %q), want (%q, %q)", tt.in, kind, word, tt.wantKind, tt.wantWord)
		}
	}
}

// ---------------------------------------------------------------------------
// gimmickPrompt + sortedKeys
// ---------------------------------------------------------------------------

func TestGimmickPrompt(t *testing.T) {
	known := []string{"sw1ft", "bike"} // deliberately unsorted: the prompt must sort them
	content := "holler at zswiftf now"
	got := gimmickPrompt(content, known)

	for _, part := range []string{
		"<<<UNTRUSTED MESSAGE",
		content,
		"UNTRUSTED MESSAGE>>>",
		"  GIMMICK:<word>",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("prompt must contain %q", part)
		}
	}

	// The known-word block: both words, in sorted order (bike before sw1ft).
	bikeIdx := strings.Index(got, "\nbike\n")
	sw1ftIdx := strings.Index(got, "\nsw1ft\n")
	if bikeIdx < 0 || sw1ftIdx < 0 {
		t.Fatalf("prompt must contain the known-word block (bike and sw1ft, one per line):\n%q", got)
	}
	if bikeIdx > sw1ftIdx {
		t.Errorf("known words must be sorted: bike at %d must come before sw1ft at %d", bikeIdx, sw1ftIdx)
	}

	// sortedKeys: 3-key map -> sorted keys.
	keys := sortedKeys(map[string]bool{"sw1ft": true, "bike": true, "give": true})
	want := []string{"bike", "give", "sw1ft"}
	if len(keys) != len(want) {
		t.Fatalf("sortedKeys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("sortedKeys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// The flow
// ---------------------------------------------------------------------------

func TestFlowFeatureFlagGate(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: false}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("who's giving me a sw1ft."))

	assertNoDeletes(t, ops)
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (feature disabled)", pi.asks)
	}
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (feature gate is the first gate)", store.listCalls)
	}
}

func TestFlowNoGuild(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	m := derpMsg("who's giving me a sw1ft.")
	m.GuildID = ""
	h.flow(m)

	assertNoDeletes(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (no guild -> return before the list fetch)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
}

func TestFlowAuthorNotFiltered(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(otherMsg("who's giving me a sw1ft."))

	assertNoDeletes(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (author gate must block before the list fetch)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
}

func TestFlowFastPathDeletesWithoutPi(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("who's giving me a sw1ft."))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestFastPathExactTokenBothWords(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"swift": true, "bike": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("I'll sell you a bike that is swift."))

	if len(ops.deleted) != 1 {
		t.Errorf("deleted = %v, want exactly one fast delete", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestFastPathNearTokenFallsThrough(t *testing.T) {
	// "swiftly" is NOT an exact token of the list ("swift") — fall through
	// to the pi path; a CLEAN verdict deletes nothing and learns nothing.
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"swift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("swiftly"))

	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v, want empty (no fast hit, CLEAN verdict)", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (fell through to the slow path)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestFlowFastPathListErrorSkips(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, listErr: errors.New("db down")}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("who's giving me a sw1ft."))

	assertNoDeletes(t, ops)
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (a list error degrades: log + skip, never act)", pi.asks)
	}
}

func TestFlowNilPiSilentReturn(t *testing.T) {
	// No fast hit, Pi == nil (true nil via the interface type): silent
	// return — the mention feature's degradation path, same shape.
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, nil)
	h.flow(derpMsg("completely clean text"))

	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowPiAskError(t *testing.T) {
	pi := &fakePi{askErr: errors.New("pi down")}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowVerdictClean(t *testing.T) {
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowVerdictUnknownGibberish(t *testing.T) {
	pi := &fakePi{resp: "I think maybe..."}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowVerdictHallucinatedWordAbsentFromMessage(t *testing.T) {
	// "xyzzy" is a valid word but NOT a token of the content: the token
	// gate keeps a hallucinated word out of the list.
	pi := &fakePi{resp: "GIMMICK:xyzzy"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowVerdictInvalidWord(t *testing.T) {
	t.Run("charset gate rejects s-w1ft", func(t *testing.T) {
		pi := &fakePi{resp: "GIMMICK:s-w1ft"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("s-w1ft here"))

		assertNothingLearned(t, store)
		assertNoDeletes(t, ops)
	})
	t.Run("token gate rejects sw1ft for content sw-1ft", func(t *testing.T) {
		// wordValid("sw1ft") passes, but sw1ft is not a token of
		// "sw-1ft" (tokenization yields "sw-1ft").
		pi := &fakePi{resp: "GIMMICK:sw1ft"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("sw-1ft here"))

		assertNothingLearned(t, store)
		assertNoDeletes(t, ops)
	})
}

func TestFlowVerdictLearnsAndDeletes(t *testing.T) {
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	content := "holler at zswiftf now"
	h.flow(derpMsg(content))

	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want [[c1 msg1]]", ops.deleted)
	}
	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	want := gimmickPrompt(content, sortedKeys(store.words))
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want %q", pi.prompts[0], want)
	}
}

func TestFlowDeleteFailsAfterSuccessfulAdd(t *testing.T) {
	// A delete failure is LOG ONLY — the word was actually used and stays
	// learned (the next occurrence is a fast hit).
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{delErr: errors.New("discord 500")}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("holler at zswiftf now"))

	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm] (a delete failure must not un-learn the word)", store.added)
	}
	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v, want empty (the delete failed)", ops.deleted)
	}
}
