// Nickname-flow tests (the GUILD_MEMBER_UPDATE reset flow). The flow is
// tested exactly like the message flow: same-state construction via
// newTestDerpies, an injectable fixed clock, and the fake seams.
package derpies

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
)

// ---------------------------------------------------------------------------
// Shared nickname-test helpers
// ---------------------------------------------------------------------------

// filteredNickUser — the exact gated-user ID constant the derpMsg /
// newTestDerpies message tests use (newTestDerpies hard-codes
// Cfg.DerpiesUserIDs to that single ID, so only it passes the author
// gate).
const filteredNickUser = "163055057254875136"

// newTestNickDerpies builds a Derpies over the fakes with the nickname
// flow's state-zeroed and a fixed injectable clock (markEdit snapshots
// h.clock() at event time, so a relative bump satisfies the 60s window
// math).
func newTestNickDerpies(store *fakeStore, ops *fakeOps, pi app.PiBackend, fixed *time.Time) *Derpies {
	h := newTestDerpies(store, ops, pi)
	h.lastNick = map[string]string{}
	h.lastEdit = map[string]time.Time{}
	h.busy = map[string]bool{}
	h.clock = func() time.Time { return *fixed }
	return h
}

// nickEvent — the GUILD_MEMBER_UPDATE payload for one member (the v0.29.0
// struct embeds *Member, so Nick/GuildID/User are promoted fields).
func nickEvent(g, u, nick string) *discordgo.GuildMemberUpdate {
	return &discordgo.GuildMemberUpdate{Member: &discordgo.Member{GuildID: g, Nick: nick, User: &discordgo.User{ID: u}}}
}

// assertNoEdits fails if ops.clears != 0.
func assertNoEdits(t *testing.T, ops *fakeOps) {
	t.Helper()
	if ops.clears != 0 {
		t.Errorf("clears = %d, want 0 (clearArgs=%v)", ops.clears, ops.clearArgs)
	}
}

// ---------------------------------------------------------------------------
// Gates
// ---------------------------------------------------------------------------

func TestNickFlowFeatureFlagOff(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: false}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "who's giving me a sw1ft."))

	assertNoEdits(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (feature gate is the first gate)", store.listCalls)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (feature disabled)", pi.asks)
	}
}

func TestNickFlowNoGuild(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("", filteredNickUser, "sw1ft"))

	assertNoEdits(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (no guild -> return before the list fetch)", store.listCalls)
	}
}

func TestNickFlowNilMemberGuard(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(&discordgo.GuildMemberUpdate{}) // Member is nil — must not panic

	assertNoEdits(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (the nil-member guard returns before the list fetch)", store.listCalls)
	}
}

func TestNickFlowAuthorNotFiltered(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	// A valid int64 member ID NOT in Cfg.DerpiesUserIDs.
	h.nickFlow(nickEvent("g", "123456789012345678", "sw1ft"))

	assertNoEdits(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (author gate must block before the list fetch)", store.listCalls)
	}
}

// ---------------------------------------------------------------------------
// The empty-nick arm (pre-list-fetch)
// ---------------------------------------------------------------------------

func TestNickFlowEmptyNickSkipsAndCaches(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, ""))

	if store.listCalls != 0 {
		t.Errorf("first event: listCalls = %d, want 0 (the empty-nick arm is pre-list-fetch)", store.listCalls)
	}
	assertNoEdits(t, ops)

	// A second empty-nick event: still no list fetch (the empty arm is
	// before everything state-touching).
	h.nickFlow(nickEvent("g", filteredNickUser, ""))
	if store.listCalls != 0 {
		t.Errorf("second event: listCalls = %d, want 0 (both events hit the empty-nick arm)", store.listCalls)
	}
	assertNoEdits(t, ops)
}

// ---------------------------------------------------------------------------
// Fast path
// ---------------------------------------------------------------------------

func TestNickFlowFastHitClearsNoLearn(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Who's giving me a sw1ft."))

	if ops.clears != 1 {
		t.Errorf("clears = %d (args %v), want exactly one fast clear", ops.clears, ops.clearArgs)
	}
	if len(ops.clearArgs) != 1 {
		t.Fatalf("clearArgs = %v, want exactly 1", ops.clearArgs)
	}
	if ops.clearArgs[0] != "g|"+filteredNickUser {
		t.Errorf("clearArgs[0] = %q, want %q", ops.clearArgs[0], "g|"+filteredNickUser)
	}
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (fast path does not learn)", store.added)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}

	// A same-nick event again (the gateway echo shape): the successful
	// clear marked the window, so the repeat is skipped by the COOLDOWN
	// check, which precedes the list fetch.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "Who's giving me a sw1ft."))
	if ops.clears != 1 {
		t.Errorf("after the repeat: clears = %d, want still 1 (cooldown blocked the echo)", ops.clears)
	}
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1 (the cooldown check precedes the list fetch)", store.listCalls)
	}
}

func TestNickFlowFastHitUnicode(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "give me a žwift"))

	if ops.clears != 1 {
		t.Errorf("clears = %d (args %v), want exactly one fast clear (the fold makes it a fast hit — message-flow parity)", ops.clears, ops.clearArgs)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (a fast hit must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestNickFlowFastMissListErrorSkips(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, listErr: errors.New("db down")}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	assertNoEdits(t, ops)
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (a list error degrades: log + skip, never act on a half-loaded list)", pi.asks)
	}
}

func TestNickFlowFastMissNilPiSilent(t *testing.T) {
	var fixed time.Time
	// Mirrors TestFlowNilPiSilentReturn: a true nil App.Pi via the
	// interface type.
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, nil, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "completely clean nick"))

	assertNoEdits(t, ops)
	assertNothingLearned(t, store)
	if store.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1 (the fast path ran before the Pi==nil guard)", store.listCalls)
	}
}

// ---------------------------------------------------------------------------
// Slow-path verdicts
// ---------------------------------------------------------------------------

func TestNickFlowVerdictClean(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "nice bike buyer"))

	assertNoEdits(t, ops)
	assertNothingLearned(t, store)

	// A same-nick event: the clean verdict cached the nick, and NO edit
	// was performed so NO cooldown was marked — the repeat is blocked by
	// the CACHED-EQUAL skip (either way, the list must not re-fetch).
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "nice bike buyer"))
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1 (the cached-equal skip blocks the re-judge)", store.listCalls)
	}
	assertNoEdits(t, ops)
}

func TestNickFlowVerdictUnknown(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "gimmick without colon"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "nice bike buyer"))

	assertNoEdits(t, ops)
	assertNothingLearned(t, store)

	// Same-nick repeat: the unknown verdict cached the nick — no re-fetch.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "nice bike buyer"))
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1", store.listCalls)
	}
}

func TestNickFlowVerdictInvalidWord(t *testing.T) {
	var fixed time.Time
	// "x" is 1 char — fails wordmatch.WordValid.
	pi := &fakePi{resp: "GIMMICK:x"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "get me a x"))

	assertNoEdits(t, ops)
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (the invalid verdict word is not learned)", store.added)
	}
}

func TestNickFlowVerdictNotInNickname(t *testing.T) {
	var fixed time.Time
	// "zwift" is a valid word but NOT a token of "hello world": the
	// token gate keeps a hallucinated word out of the list (mirrors
	// TestFlowVerdictHallucinatedWordAbsentFromMessage).
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "hello world"))

	assertNoEdits(t, ops)
	assertNothingLearned(t, store)

	// Same-nick repeat: the not-in-nickname arm cached the nick; no edit
	// -> no cooldown; blocked by the CACHED-EQUAL skip.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "hello world"))
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1", store.listCalls)
	}
}

func TestNickFlowVerdictLearnsAndClears(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (the folded word is learned with source llm)", store.added)
	}
	if ops.clears != 1 {
		t.Errorf("clears = %d (args %v), want exactly one llm clear", ops.clears, ops.clearArgs)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}

	// A same-nick event after the successful clear (no clock bump — the
	// point is that the cooldown BLOCKS, lastEdit was marked at the fixed
	// time and the clock has not left the 60s window): NO second edit.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))
	if ops.clears != 1 {
		t.Errorf("after the repeat: clears = %d, want still 1 (the successful clear marked the window; the cooldown blocks the repeat)", ops.clears)
	}
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1 (the cooldown check precedes the list fetch)", store.listCalls)
	}
}

// ---------------------------------------------------------------------------
// Cooldown / busy / clear-failure window
// ---------------------------------------------------------------------------

func TestNickFlowCooldownBlocksWithinWindow(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.clears != 1 {
		t.Fatalf("clears = %d (args %v), want 1 after event A", ops.clears, ops.clearArgs)
	}
	fixed = fixed.Add(59 * time.Second)
	// Event B: a DIFFERENT nick, so the cached-equal skip does not apply —
	// only the cooldown can block it.
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))

	if ops.clears != 1 {
		t.Errorf("after event B: clears = %d, want still 1 (within the 60s window)", ops.clears)
	}
	if store.listCalls != 1 {
		t.Errorf("after event B: listCalls = %d, want still 1 (the cooldown check precedes the list fetch)", store.listCalls)
	}
}

func TestNickFlowCooldownExpires(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.clears != 1 {
		t.Fatalf("clears = %d (args %v), want 1 after event A", ops.clears, ops.clearArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	// Event B: a different nick past the 60s window.
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))

	if ops.clears != 2 {
		t.Errorf("after event B: clears = %d (args %v), want 2 (past the 60s window the fast path acts again)", ops.clears, ops.clearArgs)
	}
}

func TestNickFlowClearFailureMarksWindow(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{clearErr: errors.New("discord 429")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.clears != 1 {
		t.Errorf("after event A: clears = %d (args %v), want 1 (the attempt happens — it fails, but it is an attempt)", ops.clears, ops.clearArgs)
	}
	fixed = fixed.Add(59 * time.Second)
	// Event B (different nick, within window): a failed attempt must NOT
	// be retried inside the window (429 discipline).
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))
	if ops.clears != 1 {
		t.Errorf("after event B: clears = %d, want still 1 (a failed attempt marks the window — no retry inside it)", ops.clears)
	}
	fixed = fixed.Add(2 * time.Second) // now 61s past A
	ops.clearErr = nil
	// Event C (different nick, outside window): the window has expired.
	h.nickFlow(nickEvent("g", filteredNickUser, "Another sw1ft now"))
	if ops.clears != 2 {
		t.Errorf("after event C: clears = %d (args %v), want 2 (past the window, with the failure cleared, it succeeds)", ops.clears, ops.clearArgs)
	}
}

func TestNickFlowRetryAfterWindowSameNick(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.clears != 1 {
		t.Fatalf("clears = %d (args %v), want 1 after event A", ops.clears, ops.clearArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	// Event B: the SAME nick past the 60s window. The successful clear
	// wrote "" into the cache, so the same nick is a cache MISMATCH
	// ("" != nick) and is re-cleared — this pins the re-clear
	// mechanism; the success-path saveNick(key, "") keeps the cache
	// state explicit (absent behaves identically under the ok && guard).
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.clears != 2 {
		t.Errorf("after event B: clears = %d, want 2 (the same nick is re-cleared once the window expires — the successful clear's cache write is empty-string, not the nick)", ops.clears)
	}
	if store.listCalls != 2 {
		t.Errorf("after event B: listCalls = %d, want 2 (the repeat re-fetches — it was not cached-equal-skipped)", store.listCalls)
	}
}

func TestNickFlowRetryAfterWindowSameNickAfterClearFailure(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{clearErr: errors.New("discord 500")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.clears != 1 {
		t.Fatalf("clears = %d (args %v), want 1 after event A (the attempt happens — it fails, but it is an attempt)", ops.clears, ops.clearArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	ops.clearErr = nil
	// Event B: the SAME nick past the window. A failed attempt must
	// normalize the cache (""), so the same nick is re-attempted after
	// the window expires (429 discipline still holds: markEdit bounds it
	// to one attempt per window, no retry INSIDE it). (If a clear path
	// ever left the NICK in the cache, the cached-equal arm would
	// swallow this repeat and the gimmick nick would stay; this test
	// pins against that. With the line removed today the entry is absent,
	// and the ok && guard treats absent identically to "" — so the retry
	// works either way; the line makes the state explicit and spec-proof.)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.clears != 2 {
		t.Errorf("after event B: clears = %d, want 2 (a failed clear normalizes the cache; the same nick is re-attempted once the window expires)", ops.clears)
	}
}

func TestNickFlowBusySkips(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	// Pre-set the in-flight guard (an event is already in flight for this
	// member).
	h.busy[nickKey("g", filteredNickUser)] = true
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	assertNoEdits(t, ops)
	if store.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (the busy check precedes the list fetch)", store.listCalls)
	}
}

func TestNickFlowLearnSurvivesClearFailure(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{clearErr: errors.New("discord 500")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (a clear failure after a successful learn keeps the word learned — message-flow parity)", store.added)
	}
	if ops.clears != 1 {
		t.Errorf("clears = %d (args %v), want 1 (the attempt happens — it fails, but it is an attempt)", ops.clears, ops.clearArgs)
	}
}
