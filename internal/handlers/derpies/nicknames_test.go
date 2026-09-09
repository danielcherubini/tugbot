// Nickname-flow tests (the GUILD_MEMBER_UPDATE reset flow). The flow is
// tested exactly like the message flow: same-state construction via
// newTestDerpies, an injectable fixed clock, and the fake seams.
package derpies

import (
	"errors"
	"strings"
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

// assertNoEdits fails if ops.sets != 0.
func assertNoEdits(t *testing.T, ops *fakeOps) {
	t.Helper()
	if ops.sets != 0 {
		t.Errorf("sets = %d, want 0 (setArgs=%v)", ops.sets, ops.setArgs)
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

func TestNickFlowFastHitResetsNoLearn(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Who's giving me a sw1ft."))

	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want exactly one fast reset", ops.sets, ops.setArgs)
	}
	if len(ops.setArgs) != 1 {
		t.Fatalf("setArgs = %v, want exactly 1", ops.setArgs)
	}
	if ops.setArgs[0] != "g|"+filteredNickUser+"|Derpies" {
		t.Errorf("setArgs[0] = %q, want %q", ops.setArgs[0], "g|"+filteredNickUser+"|Derpies")
	}
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (fast path does not learn)", store.added)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}

	// A same-nick event again (the gateway echo shape): the successful
	// reset marked the window, so the repeat is skipped by the COOLDOWN
	// check, which precedes the list fetch.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "Who's giving me a sw1ft."))
	if ops.sets != 1 {
		t.Errorf("after the repeat: sets = %d, want still 1 (cooldown blocked the echo)", ops.sets)
	}
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1 (the cooldown check precedes the list fetch)", store.listCalls)
	}
}

// TestNickFlowFastPathResetsToDerpies is the pin for the decoupled name action:
// the fast path RESETS (not clears) to the fixed value, with no learn and no
// pi ask.
func TestNickFlowFastPathResetsToDerpies(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Who's giving me a sw1ft."))

	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 (a fast hit is already-known: it resets the name)", ops.sets, ops.setArgs)
	}
	if len(ops.setArgs) != 1 || !strings.HasSuffix(ops.setArgs[0], "|Derpies") {
		t.Errorf("setArgs = %v, want the single set to end |Derpies (the reset is a fixed value, not a null clear)", ops.setArgs)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (fast path does not learn)", store.added)
	}
}

func TestNickFlowFastHitUnicode(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "give me a žwift"))

	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want exactly one fast reset (the fold makes it a fast hit — message-flow parity)", ops.sets, ops.setArgs)
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

	// NOT learnable (the two-arm gate fails) — but the name action is
	// decoupled from the learning gate: the gimmick name is STILL reset
	// (to the fixed neutral value), just not learned.
	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want 1 (a not-learnable verdict still resets the name)", ops.sets, ops.setArgs)
	}
	if len(ops.setArgs) != 1 || ops.setArgs[0] != "g|"+filteredNickUser+"|Derpies" {
		t.Errorf("setArgs = %v, want [g|<user>|Derpies] (the reset SETS the fixed value, not a null clear)", ops.setArgs)
	}
	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (the invalid verdict word is not learned)", store.added)
	}
}

// TestNickFlowDeadEndVerdictResetsName pins the dead-end arm: the verdict
// word is valid and a token of the message text would be expected, but the
// NICK toks (non-ASCII lookalike, not in the confusable fold table) do not
// hit — so the word is NOT learnable, while the name action still resets.
// (Supersedes the old TestNickFlowVerdictNotInNickname, whose second-half
// cached-equal-repeat assertion no longer applies: after a reset the cache
// holds "Derpies", not the nick.)
func TestNickFlowDeadEndVerdictResetsName(t *testing.T) {
	var fixed time.Time
	nick := "swiftы"
	// Fixture pre-check in the test setup: the nick's folded token set
	// contains the as-appears token but NOT the ASCII base — ы is not in
	// the confusable table and is NFD-inert, so the folded token stays
	// non-ASCII and toks["swift"] is false (the two-arm gate's token arm
	// fails; the word IS WordValid).
	toks := tokensForMatch(nick)
	if !toks[nick] || toks["swift"] {
		t.Fatalf("fixture pre-check failed: tokensForMatch(%q) = %v — want the as-appears token present and the ascii token absent", nick, toks)
	}
	pi := &fakePi{resp: "GIMMICK:swift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, nick))

	if len(store.added) != 0 {
		t.Errorf("added = %v, want empty (the verdict word is NOT a nick token — dead-end, not learned)", store.added)
	}
	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want 1 (dead-end still RESETS the name)", ops.sets, ops.setArgs)
	}
	if len(ops.setArgs) != 1 || !strings.HasSuffix(ops.setArgs[0], "|Derpies") {
		t.Errorf("setArgs = %v, want the single set to end |Derpies", ops.setArgs)
	}
}

// TestNickFlowValidVerdictLearnsAndResets pins the happy path: a valid
// verdict word that IS a nick token is learned AND the name is reset.
func TestNickFlowValidVerdictLearnsAndResets(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:sw1ft"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}} // "sw1ft" not in the store words
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if len(store.added) != 1 || store.added[0] != "sw1ft|llm" {
		t.Errorf("added = %v, want [sw1ft|llm] (the valid verdict word is learned with source llm)", store.added)
	}
	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want 1 (learn AND reset)", ops.sets, ops.setArgs)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

func TestNickFlowVerdictLearnsAndResets(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (the folded word is learned with source llm)", store.added)
	}
	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want exactly one llm reset", ops.sets, ops.setArgs)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}

	// A same-nick event after the successful reset (no clock bump — the
	// point is that the cooldown BLOCKS, lastEdit was marked at the fixed
	// time and the clock has not left the 60s window): NO second edit.
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the repeat", store.listCalls)
	}
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))
	if ops.sets != 1 {
		t.Errorf("after the repeat: sets = %d, want still 1 (the successful reset marked the window; the cooldown blocks the repeat)", ops.sets)
	}
	if store.listCalls != 1 {
		t.Errorf("after the repeat: listCalls = %d, want still 1 (the cooldown check precedes the list fetch)", store.listCalls)
	}
}

// ---------------------------------------------------------------------------
// Cooldown / busy / reset-failure window
// ---------------------------------------------------------------------------

// TestNickFlowResetThenEchoSkippedByChangeDetection is the pin for the
// echo-skip gate: after a successful reset, a gateway echo of our OWN set
// (Nick == "Derpies", delivered AFTER the 60s window) is skipped by the
// CHANGE-DETECTION check (cur == evt.Nick) — which only works because the
// success arm caches the non-empty reset value, not "". The clock bump to
// +61s is REQUIRED: without it the next event is blocked by the 60s
// COOLDOWN (the check BEFORE change-detection), which would mask the exact
// regression this test pins (a leftover saveNick(key, "") on the success
// arm only shows up post-window: cache "" != "Derpies" -> a second set).
func TestNickFlowResetThenEchoSkippedByChangeDetection(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after the initial reset", ops.sets, ops.setArgs)
	}
	if store.listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 before the echo", store.listCalls)
	}
	// The echo arrives AFTER the 60s window — the cooldown no longer
	// blocks, so only the change-detection check can (and must) skip: a
	// success arm that cached "" would let this event through ("" !=
	// "Derpies") and set a second time.
	fixed = fixed.Add(61 * time.Second)
	h.nickFlow(nickEvent("g", filteredNickUser, "Derpies"))

	if ops.sets != 1 {
		t.Errorf("after the echo: sets = %d, want still 1 (the change-detection skip blocked a second set; it only works because the success arm caches the non-empty reset value)", ops.sets)
	}
	if store.listCalls != 1 {
		t.Errorf("after the echo: listCalls = %d, want still 1 (the skip is pre-list-fetch — no re-judge of our own set)", store.listCalls)
	}
}

func TestNickFlowCooldownBlocksWithinWindow(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after event A", ops.sets, ops.setArgs)
	}
	fixed = fixed.Add(59 * time.Second)
	// Event B: a DIFFERENT nick, so the cached-equal skip does not apply —
	// only the cooldown can block it.
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))

	if ops.sets != 1 {
		t.Errorf("after event B: sets = %d, want still 1 (within the 60s window)", ops.sets)
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

	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after event A", ops.sets, ops.setArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	// Event B: a different nick past the 60s window.
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))

	if ops.sets != 2 {
		t.Errorf("after event B: sets = %d (args %v), want 2 (past the 60s window the fast path acts again)", ops.sets, ops.setArgs)
	}
}

func TestNickFlowResetFailedMarksWindow(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:sw1ft"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{setErr: errors.New("429")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after the GIMMICK verdict (the attempt happens — it fails, but it is an attempt)", ops.sets, ops.setArgs)
	}
	// A same-nick re-set immediately (same fixed clock): a failed attempt
	// must NOT be retried inside the window (429 discipline).
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 1 {
		t.Errorf("after the immediate re-set: sets = %d, want still 1 (a failed attempt marks the window — no retry inside it)", ops.sets)
	}
	// Past the 60s window: the same-nick re-set is attempted again.
	fixed = fixed.Add(61 * time.Second)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 2 {
		t.Errorf("after the past-window re-set: sets = %d (args %v), want 2 (the window has expired, with the failure still set the attempt happens again)", ops.sets, ops.setArgs)
	}
}

func TestNickFlowClearFailureMarksWindow(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{setErr: errors.New("discord 429")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))

	if ops.sets != 1 {
		t.Errorf("after event A: sets = %d (args %v), want 1 (the attempt happens — it fails, but it is an attempt)", ops.sets, ops.setArgs)
	}
	fixed = fixed.Add(59 * time.Second)
	// Event B (different nick, within window): a failed attempt must NOT
	// be retried inside the window (429 discipline).
	h.nickFlow(nickEvent("g", filteredNickUser, "Sw1ft again!"))
	if ops.sets != 1 {
		t.Errorf("after event B: sets = %d, want still 1 (a failed attempt marks the window — no retry inside it)", ops.sets)
	}
	fixed = fixed.Add(2 * time.Second) // now 61s past A
	ops.setErr = nil
	// Event C (different nick, outside window): the window has expired.
	h.nickFlow(nickEvent("g", filteredNickUser, "Another sw1ft now"))
	if ops.sets != 2 {
		t.Errorf("after event C: sets = %d (args %v), want 2 (past the window, with the failure cleared, it succeeds)", ops.sets, ops.setArgs)
	}
}

func TestNickFlowRetryAfterWindowSameNick(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after event A", ops.sets, ops.setArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	// Event B: the SAME nick past the 60s window. The successful
	// reset wrote the reset value into the cache, so the same nick is
	// a cache MISMATCH ("Derpies" != nick) and is re-reset — this pins
	// the re-reset mechanism; the success-path saveNick(key,
	// derpiesNickReset) keeps the cache state explicit (absent behaves
	// identically under the ok && guard).
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 2 {
		t.Errorf("after event B: sets = %d, want 2 (the same nick is re-reset once the window expires — the successful reset's cache write is the reset value, not the nick)", ops.sets)
	}
	if store.listCalls != 2 {
		t.Errorf("after event B: listCalls = %d, want 2 (the repeat re-fetches — it was not cached-equal-skipped)", store.listCalls)
	}
}

func TestNickFlowRetryAfterWindowSameNickAfterResetFailure(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{setErr: errors.New("discord 500")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 1 {
		t.Fatalf("sets = %d (args %v), want 1 after event A (the attempt happens — it fails, but it is an attempt)", ops.sets, ops.setArgs)
	}
	fixed = fixed.Add(61 * time.Second)
	ops.setErr = nil
	// Event B: the SAME nick past the window. A failed attempt must
	// normalize the cache (""), so the same nick is re-attempted after
	// the window expires (429 discipline still holds: markEdit bounds it
	// to one attempt per window, no retry INSIDE it). (If a reset path
	// ever left the NICK in the cache, the cached-equal arm would
	// swallow this repeat and the gimmick nick would stay; this test
	// pins against that. With the line removed today the entry is absent,
	// and the ok && guard treats absent identically to "" — so the retry
	// works either way; the line makes the state explicit and spec-proof.)
	h.nickFlow(nickEvent("g", filteredNickUser, "sw1ft"))
	if ops.sets != 2 {
		t.Errorf("after event B: sets = %d, want 2 (a failed reset normalizes the cache; the same nick is re-attempted once the window expires)", ops.sets)
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

func TestNickFlowLearnSurvivesResetFailure(t *testing.T) {
	var fixed time.Time
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{setErr: errors.New("discord 500")}
	h := newTestNickDerpies(store, ops, pi, &fixed)
	h.nickFlow(nickEvent("g", filteredNickUser, "Purchase me a zwift for 9/11"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (a reset failure after a successful learn keeps the word learned — message-flow parity)", store.added)
	}
	if ops.sets != 1 {
		t.Errorf("sets = %d (args %v), want 1 (the attempt happens — it fails, but it is an attempt)", ops.sets, ops.setArgs)
	}
}
