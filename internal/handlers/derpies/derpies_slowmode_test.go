// The single-slowmode gate tests (decision 0011): the 3rd+ single-token
// post by a gated author per channel inside a rolling 30 s is deleted
// before the fast path (zero pi asks, zero list fetch, zero image leg),
// and a path='slowmode' decision row is written.
package derpies

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// gateMsg — a derpies message with an explicit id and channel (the
// derpMsg factory hard-codes ID "msg1").
func gateMsg(id, ch, content string) *discordgo.Message {
	m := derpMsg(content)
	m.ID, m.ChannelID = id, ch
	return m
}

// newGateDerpies — a derpies over fakes with a pinned clock (the test
// moves `tt` per step). `resp` is the slow-path verdict:
// SCORE:3 < the learn floor 40 < the default threshold 50 → path
// 'slow', no learn, no delete.
func newGateDerpies(words map[string]bool) (*fakeStore, *fakeOps, *fakePi, *Derpies, *time.Time) {
	store := &fakeStore{
		enabled: map[string]bool{FeatureKey: true},
		words:   words,
		prompt:  defaultPromptTemplate,
	}
	ops := &fakeOps{}
	pi := &fakePi{resp: "SCORE:3"}
	h := newTestDerpies(store, ops, pi)
	var tt time.Time
	h.clock = func() time.Time { return tt }
	return store, ops, pi, h, &tt
}

// TestGateDeletesThirdSingleToken (S6.1): two single-token posts within
// 30 s pass; the 3rd is deleted at the gate (no list fetch, no pi ask)
// and its decision row is path='slowmode' with NULL score/threshold/word,
// learned=false, deleted=true, NULL reject_reason. The A and B rows are
// the normal slow-path rows (score 3 < learn floor 40 < threshold 50: no
// learn, no delete).
func TestGateDeletesThirdSingleToken(t *testing.T) {
	store, ops, pi, h, _ := newGateDerpies(map[string]bool{"sw1ft": true})

	h.flow(gateMsg("a1", "c1", "cat"))
	h.flow(gateMsg("a2", "c1", "has"))
	h.flow(gateMsg("a3", "c1", "hit"))

	if len(ops.deleted) != 1 {
		t.Fatalf("deleted = %v, want exactly [c1 a3]", ops.deleted)
	}
	if ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "a3" {
		t.Errorf("deleted[0] = %v, want [c1 a3]", ops.deleted[0])
	}
	if store.listCalls != 2 {
		t.Errorf("listCalls = %d, want 2 (A and B only — C never reached the fast path)", store.listCalls)
	}
	if pi.asks != 2 {
		t.Errorf("asks = %d, want 2 (A and B only)", pi.asks)
	}
	var rowC *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "a3" {
			rowC = d
		}
	}
	if rowC == nil {
		t.Fatal("no decision row for a3")
	}
	if rowC.Path == nil || *rowC.Path != "slowmode" {
		t.Errorf("C row Path = %v, want 'slowmode'", rowC.Path)
	}
	if rowC.Score != nil || rowC.Threshold != nil || rowC.Word != nil || rowC.RejectReason != nil {
		t.Errorf("C row should have NULL score/threshold/word/reject_reason: score=%v threshold=%v word=%v reject=%v",
			rowC.Score, rowC.Threshold, rowC.Word, rowC.RejectReason)
	}
	if rowC.Learned {
		t.Error("C row Learned = true, want false")
	}
	if !rowC.Deleted {
		t.Error("C row Deleted = false, want true")
	}
	for _, id := range []string{"a1", "a2"} {
		var row *decisionRecord
		for _, d := range store.decisions {
			if d.MessageID == id {
				row = d
			}
		}
		if row == nil {
			t.Fatalf("no decision row for %s", id)
		}
		if row.Path == nil || *row.Path != "slow" {
			t.Errorf("%s row Path = %v, want 'slow'", id, row.Path)
		}
	}
	if len(store.decisions) != 3 {
		t.Errorf("decision rows = %d, want 3", len(store.decisions))
	}
}

// TestGateSlidingEdge (S6.2): the window is sliding and inclusive —
// posts at t=0 and t=+29 s count, but the t=0 post is pruned by
// t=+30.5 s (now.Sub(ts) > slowmodeWindow), so the 3rd post passes.
func TestGateSlidingEdge(t *testing.T) {
	_, ops, pi, h, tt := newGateDerpies(nil)
	base := time.Now()
	for i, content := range []string{"a", "b", "c"} {
		*tt = base.Add([]time.Duration{0, 29 * time.Second, 30500 * time.Millisecond}[i])
		h.flow(gateMsg("s"+string(rune('1'+i)), "c1", content))
	}
	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v, want none (the t=0 post is pruned at t=+30.5 s)", ops.deleted)
	}
	if pi.asks != 3 {
		t.Errorf("asks = %d, want 3 (the 3rd post passed the gate)", pi.asks)
	}
}

// TestGateIgnoresLongPosts (S6.3): ≥2-token posts never enter the
// window — 5 two-token posts inside 10 s all run the full flow (5 asks,
// no gate deletes).
func TestGateIgnoresLongPosts(t *testing.T) {
	store, ops, pi, h, tt := newGateDerpies(nil)
	base := time.Now()
	for i, content := range []string{"very nice", "quite good", "so great", "so cool", "fine enough"} {
		*tt = base.Add(time.Duration(i) * 2 * time.Second)
		h.flow(gateMsg("l"+string(rune('1'+i)), "c1", content))
	}
	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v, want none (≥2-token posts are never gated)", ops.deleted)
	}
	if pi.asks != 5 {
		t.Errorf("asks = %d, want 5 (every long post ran the full flow)", pi.asks)
	}
	if store.listCalls != 5 {
		t.Errorf("listCalls = %d, want 5", store.listCalls)
	}
}

// TestGateCountsFastPathDeletes (S6.4): the window counts POSTS, not
// outcomes — a fast-path-deleted single token still counts. `sw1ft` is
// fast-deleted (zero asks), then `hit` passes and is asked; `has` is
// the 3rd single-token post in the window → gate-deleted with zero
// asks.
func TestGateCountsFastPathDeletes(t *testing.T) {
	store, ops, pi, h, tt := newGateDerpies(map[string]bool{"sw1ft": true})
	base := time.Now()
	for i, content := range []string{"sw1ft", "hit", "has"} {
		*tt = base.Add(time.Duration(i) * time.Second)
		h.flow(gateMsg("f"+string(rune('1'+i)), "c1", content))
	}
	if len(ops.deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 (sw1ft fast-deleted, has gate-deleted)", ops.deleted)
	}
	if ops.deleted[1][0] != "c1" || ops.deleted[1][1] != "f3" {
		t.Errorf("deleted[1] = %v, want [c1 f3]", ops.deleted[1])
	}
	if pi.asks != 1 {
		t.Errorf("asks = %d, want 1 (only 'hit'; sw1ft fast-deleted, has gate-deleted)", pi.asks)
	}
	var rowH *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "f3" {
			rowH = d
		}
	}
	if rowH == nil {
		t.Fatal("no decision row for f3 (the gate-deleted post)")
	}
	if rowH.Path == nil || *rowH.Path != "slowmode" {
		t.Errorf("f3 row Path = %v, want 'slowmode'", rowH.Path)
	}
	if !rowH.Deleted || rowH.Learned {
		t.Errorf("f3 row Deleted/Learned = %v/%v, want true/false", rowH.Deleted, rowH.Learned)
	}
	var rowF1 *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "f1" {
			rowF1 = d
		}
	}
	if rowF1 != nil && (rowF1.Path == nil || *rowF1.Path != "fast") {
		t.Errorf("f1 row Path = %v, want 'fast'", rowF1.Path)
	}
}

// TestGateIgnoresEdits (S6.5): the gate watches MessageCreate only — an
// edit runs the FULL flow (one ask), is NOT counted in the gate window,
// and can NOT be gate-deleted. Two single-token creates hold the window;
// a single-token edit between them changes nothing; the next CREATE is
// the 3rd single-token post in the window and is gate-deleted. The
// edit's decision row is never path='slowmode'.
func TestGateIgnoresEdits(t *testing.T) {
	store, ops, pi, h, tt := newGateDerpies(nil)
	base := time.Now()

	// Step 1: two distinct single-token creates (t=base and t=base+1 s)
	// — both pass the gate and enter the window.
	*tt = base
	h.flow(gateMsg("a1", "c1", "cat"))
	*tt = base.Add(1 * time.Second)
	h.flow(gateMsg("a2", "c1", "has0"))
	if pi.asks != 2 {
		t.Fatalf("asks = %d after step 1, want 2 (both creates ran the slow path)", pi.asks)
	}
	if len(ops.deleted) != 0 {
		t.Fatalf("deleted = %v after step 1, want none", ops.deleted)
	}
	if got := len(h.rateWindow[editUser+"|c1"]); got != 2 {
		t.Fatalf("window entries after step 1 = %d, want 2 (the two creates)", got)
	}

	// Step 2 (t=base+2 s): a single-token edit. It runs the FULL flow
	// (one ask), is NOT gate-deleted, and is NOT counted in the window.
	*tt = base.Add(2 * time.Second)
	h.editFlow(editEvent("hit"))
	if pi.asks != 3 {
		t.Errorf("asks = %d after step 2, want 3 (the edit ran the full flow — exactly one more ask)", pi.asks)
	}
	if len(ops.deleted) != 0 {
		t.Errorf("deleted = %v after step 2, want none (edits can NOT be gate-deleted)", ops.deleted)
	}
	if got := len(h.rateWindow[editUser+"|c1"]); got != 2 {
		t.Errorf("window entries after step 2 = %d, want 2 (the edit is NOT counted, the window still holds the two creates)", got)
	}

	// Step 3 (t=base+3 s): the 3rd single-token CREATE in the window —
	// gate-deleted, with its path='slowmode' decision row.
	*tt = base.Add(3 * time.Second)
	h.flow(gateMsg("a3", "c1", "a"))
	if len(ops.deleted) != 1 {
		t.Fatalf("deleted = %v, want exactly [c1 a3]", ops.deleted)
	}
	if ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "a3" {
		t.Errorf("deleted[0] = %v, want [c1 a3]", ops.deleted[0])
	}
	var rowA3 *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "a3" {
			rowA3 = d
		}
	}
	if rowA3 == nil {
		t.Fatal("no decision row for a3")
	}
	if rowA3.Path == nil || *rowA3.Path != "slowmode" {
		t.Errorf("a3 row Path = %v, want 'slowmode'", rowA3.Path)
	}
	if rowA3.Score != nil || rowA3.Threshold != nil || rowA3.Word != nil {
		t.Errorf("a3 row should have NULL score/threshold/word: score=%v threshold=%v word=%v", rowA3.Score, rowA3.Threshold, rowA3.Word)
	}
	if rowA3.Learned {
		t.Error("a3 row Learned = true, want false")
	}
	if !rowA3.Deleted {
		t.Error("a3 row Deleted = false, want true")
	}
	// The edit's row (the slow-path row for m1) is never path='slowmode' —
	// no decision row at all carries that path for the edit.
	var editSlowmode *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "m1" && d.Path != nil && *d.Path == "slowmode" {
			editSlowmode = d
		}
	}
	if editSlowmode != nil {
		t.Errorf("m1 (the edit) has a path='slowmode' decision row — edits must never be gate-judged")
	}
}

// TestGateExclusions (S6.4): the gate is inert for everything the flow
// already short-circuits — feature off (the feature gate returns before
// the gate), non-gated author (the author gate), no guild (the guild
// guard) — and for 0-token posts, which fall through to the FULL flow.
// Each sub-case asserts len(ops.deleted) == 0 and NO slowmode row in
// store.decisions (pinned clock: every post is inside the window, so
// only these short-circuits excuse the absence of a gate delete).
func TestGateExclusions(t *testing.T) {
	// run — the shared sub-case body (feature off / non-gated author /
	// no guild): a fresh handler over fresh fakes (words nil, the code
	// default prompt, the fakePi default SCORE:3 verdict), one flow
	// run, then the three gate assertions (no deletes, the expected ask
	// count, no slowmode row).
	run := func(name string, setup func(*fakeStore, *fakeOps, *fakePi, *Derpies), m *discordgo.Message, wantAsks int) {
		t.Helper()
		store, ops, pi, h, _ := newGateDerpies(nil)
		if setup != nil {
			setup(store, ops, pi, h)
		}
		h.flow(m)
		if len(ops.deleted) != 0 {
			t.Errorf("deleted = %v, want none (%q is ungated)", ops.deleted, name)
		}
		if pi.asks != wantAsks {
			t.Errorf("asks = %d, want %d (%q)", pi.asks, wantAsks, name)
		}
		for _, d := range store.decisions {
			if d.Path != nil && *d.Path == "slowmode" {
				t.Errorf("%q wrote a path='slowmode' decision row — it must not be gated", name)
			}
		}
	}
	t.Run("feature off", func(t *testing.T) {
		// The feature gate (the flow's step 1) returns before the
		// author and gate steps: zero asks, no row at all.
		m := gateMsg("a1", "c1", "cat")
		run("feature off",
			func(s *fakeStore, _ *fakeOps, _ *fakePi, _ *Derpies) { s.enabled[FeatureKey] = false },
			m, 0)
	})
	t.Run("non-gated author", func(t *testing.T) {
		// The author-ID gate (mirroring otherMsg's semantics — a user
		// who is NOT in DerpiesUserIDs) returns before the gate:
		// zero asks, no row at all.
		m := gateMsg("a1", "c1", "cat")
		m.Author = &discordgo.User{ID: "222"}
		run("non-gated author", nil, m, 0)
	})
	t.Run("no guild", func(t *testing.T) {
		// The guild guard returns before the gate: zero asks, no row
		// at all.
		m := gateMsg("a1", "c1", "cat")
		m.GuildID = ""
		run("no guild", nil, m, 0)
	})
	t.Run("0-token post", func(t *testing.T) {
		// Empty content (the attachment-only shape): the gate skips it
		// (len(toks) != 1) and the flow runs to the slow path —
		// exactly one ask, no gate row. A normal path='slow' row
		// (score 3 < 40 → no learn, no delete) IS expected.
		store, ops, pi, h, _ := newGateDerpies(nil)
		h.flow(gateMsg("a1", "c1", ""))
		if len(ops.deleted) != 0 {
			t.Fatalf("deleted = %v, want none (a 0-token post is ungated)", ops.deleted)
		}
		if pi.asks != 1 {
			t.Errorf("asks = %d, want 1 (the flow continues to the slow path)", pi.asks)
		}
		for _, d := range store.decisions {
			if d.Path != nil && *d.Path == "slowmode" {
				t.Errorf("a 0-token post wrote a path='slowmode' decision row — it must not be gated")
			}
		}
		if len(store.decisions) != 1 {
			t.Fatalf("decision rows = %d, want 1 (the full flow's single row)", len(store.decisions))
		}
		if store.decisions[0].Path == nil || *store.decisions[0].Path != "slow" {
			t.Errorf("row Path = %v, want 'slow' (the ordinary slow-path verdict row)", store.decisions[0].Path)
		}
	})
}

// TestGateDeleteFailure (S6.5): a gate delete failure degrades
// best-effort — the 3rd post's DeleteMessage fails (the blanket fake
// delErr), the flow does NOT panic, and the fake records only
// successes (len(ops.deleted) == 0), BUT the path='slowmode' decision
// row is still written with Deleted == true (the write is
// unconditional — the row is an audit of the GATE decision, not of the
// REST outcome). After clearing ops.delErr the 4th post is
// gate-deleted (the single success is recorded).
func TestGateDeleteFailure(t *testing.T) {
	store, ops, pi, h, _ := newGateDerpies(nil)
	ops.delErr = errors.New("429")
	for i, content := range []string{"cat", "has", "hit"} {
		h.flow(gateMsg("a"+string(rune('1'+i)), "c1", content))
	}
	if len(ops.deleted) != 0 {
		t.Fatalf("deleted = %v, want none (the fake records only successes — the 3rd post's delete failed)", ops.deleted)
	}
	if pi.asks != 2 {
		t.Errorf("asks = %d, want 2 (posts 1 and 2 passed the gate; the 3rd never reached it)", pi.asks)
	}
	var rowA3 *decisionRecord
	for _, d := range store.decisions {
		if d.MessageID == "a3" {
			rowA3 = d
		}
	}
	if rowA3 == nil {
		t.Fatal("no decision row for a3 (the gate-deleted post)")
	}
	if rowA3.Path == nil || *rowA3.Path != "slowmode" {
		t.Errorf("a3 row Path = %v, want 'slowmode'", rowA3.Path)
	}
	if !rowA3.Deleted {
		t.Errorf("a3 row Deleted = false, want true (best-effort recorded — the delete failed, the row stands)")
	}
	if rowA3.Learned {
		t.Errorf("a3 row Learned = true, want false")
	}
	// The blanket fake otherwise keeps failing — the 4th post is
	// only meaningful after clearing it.
	ops.delErr = nil
	h.flow(gateMsg("a4", "c1", "e"))
	if len(ops.deleted) != 1 {
		t.Fatalf("deleted = %v, want exactly [c1 a4] (the 4th post, after the error cleared)", ops.deleted)
	}
	if ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "a4" {
		t.Errorf("deleted[0] = %v, want [c1 a4]", ops.deleted[0])
	}
}

// TestGatePerChannelIsolation (S6.6): the window is per-(author,
// CHANNEL) — c1's window is driven to the cap (2 single-token posts)
// while c2 stays below it (its two posts are ≥2-token and never
// enter the window). The 3rd in c1 is gate-deleted; the 3rd single
// token in c2 passes (its channel's window only ever accumulated 2
// entries — a buggy shared/author-wide counter would have deleted it
// as the 3rd single token overall). Sole delete: [c1 a3].
//
// Deviation (recorded per the plan's construction-faults rule): the
// plan's sequence (2 single tokens in c1 + 2 single tokens in c2, same
// gated author) would cap in BOTH channels — with per-(author, channel)
// windows the 3rd single-token post in c2 is itself gate-deleted (2
// entries accumulated in c2's own window), so the plan's "len(
// ops.deleted) == 1" assertion is unattainable in that sequence. The
// adjustment above (c2's two early posts are 2-token, uncounted) is
// the minimal change that still pins the isolation property the test
// is named for: a cap full in c1 never gates c2, and vice-versa.
func TestGatePerChannelIsolation(t *testing.T) {
	_, ops, pi, h, _ := newGateDerpies(nil)
	h.flow(gateMsg("a1", "c1", "cat"))
	h.flow(gateMsg("b1", "c2", "very nice")) // 2-token: never enters c2's window
	h.flow(gateMsg("a2", "c1", "has"))
	h.flow(gateMsg("b2", "c2", "quite good")) // 2-token: never enters c2's window
	h.flow(gateMsg("a3", "c1", "hit"))        // 3rd in c1 -> gate-deleted (c1 window at the cap)
	h.flow(gateMsg("b3", "c2", "fox"))        // 3rd single token in c2 -> passes
	if len(ops.deleted) != 1 {
		t.Fatalf("deleted = %v, want exactly one (c1's 3rd post; c2's 3rd passes its own window)", ops.deleted)
	}
	if ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "a3" {
		t.Errorf("deleted[0] = %v, want [c1 a3]", ops.deleted[0])
	}
	if pi.asks != 5 {
		t.Errorf("asks = %d, want 5 (a1, b1, a2, b2, b3 — a3 was gate-deleted)", pi.asks)
	}
	// The direct window-state check: c1's window is at the cap (both
	// surviving entries kept, the over-limit post was NOT added), c2's
	// window still holds... nothing but that b3's timestamp (b1/b2 were
	// never added).
	if got := len(h.rateWindow[editUser+"|c1"]); got != 2 {
		t.Errorf("c1 window entries = %d, want 2 (the over-limit a3 appends nothing — the cap remains at 2)", got)
	}
	if got := len(h.rateWindow[editUser+"|c2"]); got != 1 {
		t.Errorf("c2 window entries = %d, want 1 (only b3's timestamp — the 2-token b1/b2 are never added)", got)
	}
}

// TestGateEmptyOnRestart (S6.9): the counter is in-memory on the
// handler — a bot restart yields a FRESH handler whose window is empty.
// Two single-token posts on h hold h's window at 2; a fresh h2 (fresh
// fakes + a pinned clock) receiving two identical single-token posts
// never sees 3 entries, so nothing is deleted.
func TestGateEmptyOnRestart(t *testing.T) {
	_, ops, pi, h, _ := newGateDerpies(nil)
	h.flow(gateMsg("a1", "c1", "cat"))
	h.flow(gateMsg("a2", "c1", "has"))
	if len(ops.deleted) != 0 {
		t.Fatalf("deleted = %v, want none (only 2 single-token posts so far — below the gate)", ops.deleted)
	}
	if pi.asks != 2 {
		t.Fatalf("asks = %d, want 2", pi.asks)
	}
	// The "restart": a fresh handler over fresh fakes (its rateWindow
	// is empty — nothing carries over).
	_, ops2, pi2, h2, _ := newGateDerpies(nil)
	h2.flow(gateMsg("b1", "c1", "cat"))
	h2.flow(gateMsg("b2", "c1", "has"))
	if len(ops2.deleted) != 0 {
		t.Errorf("deleted after restart = %v, want none (the fresh window is empty — 2 posts < the gate)", ops2.deleted)
	}
	if pi2.asks != 2 {
		t.Errorf("asks after restart = %d, want 2 (both posts ran the full flow)", pi2.asks)
	}
}
