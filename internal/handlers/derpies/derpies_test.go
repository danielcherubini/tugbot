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
	"github.com/danielcherubini/tugbot/internal/mcp"
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
	prompt    string // promptText returns this (+ promptErr)
	promptErr error
	// configThreshold returns (clampThreshold(threshold), thresholdErr) —
	// the zero value yields the default 50 (a zero-threshold fake must not
	// make every scored verdict delete).
	threshold    int
	thresholdErr error
	// listPhrases returns phrases (+ phrasesErr).
	phrases    []string
	phrasesErr error
	// recordDecision returns recordErr if set; otherwise appends a COPY of
	// the record to decisions (later mutations must not alias).
	recordErr error
	decisions []*decisionRecord
	// queryDecisions records the filter in queried and returns
	// (queryRows, queryErr).
	queried   mcp.DecisionFilter
	queryRows []mcp.DecisionRow
	queryErr  error
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

func (s *fakeStore) promptText(_ context.Context) (string, error) {
	return s.prompt, s.promptErr
}

func (s *fakeStore) configThreshold(_ context.Context) (int, error) {
	return clampThreshold(s.threshold), s.thresholdErr
}

func (s *fakeStore) listPhrases(_ context.Context) ([]string, error) {
	return s.phrases, s.phrasesErr
}

func (s *fakeStore) recordDecision(_ context.Context, d *decisionRecord) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	cp := *d // a copy: later mutations of the caller's record do not alias the stored one
	s.decisions = append(s.decisions, &cp)
	return nil
}

func (s *fakeStore) queryDecisions(_ context.Context, f mcp.DecisionFilter) ([]mcp.DecisionRow, error) {
	s.queried = f
	return s.queryRows, s.queryErr
}

// fakeOps implements the discordOps seam.
type fakeOps struct {
	deleted [][]string // each {channelID, messageID}
	delErr  error
	// channelMessageRetrieve returns ref/refErr; refCalls counts calls.
	ref      *discordgo.Message
	refErr   error
	refCalls int

	// setNickname records EVERY attempt (including a failed one) then
	// returns setErr — the nickname flow's window discipline is asserted
	// on attempt counts, not successes.
	setErr  error
	sets    int
	setArgs []string // "guildID|memberID|nick"
}

func (o *fakeOps) deleteMessage(channelID, messageID string) error {
	if o.delErr != nil {
		return o.delErr
	}
	o.deleted = append(o.deleted, []string{channelID, messageID})
	return nil
}

func (o *fakeOps) channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error) {
	o.refCalls++
	return o.ref, o.refErr
}

func (o *fakeOps) setNickname(guildID, memberID, nick string) error {
	// Unconditionally record the attempt (sets increments, the args
	// append with the "guildID|memberID|nick" triple) BEFORE returning
	// the error — a failed attempt is still an attempt (the 429 window
	// discipline asserts on this).
	o.sets++
	o.setArgs = append(o.setArgs, guildID+"|"+memberID+"|"+nick)
	return o.setErr
}

// fakePi implements ALL THREE app.PiBackend methods (a fake with only
// Ask will not compile as App.Pi). AskWithImages tracks its OWN counters
// (imageAsks / imagePrompts / images) so the flow tests can tell the
// image ask path apart from the text ask path — `asks`/`prompts` belong
// to Ask alone (the pre-existing flow tests assert those).
type fakePi struct {
	resp    string
	askErr  error
	asks    int
	prompts []string

	imageAsks    int
	imagePrompts []string
	images       [][]app.PiImage
}

func (f *fakePi) Ask(_ context.Context, prompt string) (string, error) {
	f.asks++
	f.prompts = append(f.prompts, prompt)
	return f.resp, f.askErr
}

func (f *fakePi) AskWithImages(_ context.Context, prompt string, images []app.PiImage) (string, error) {
	f.imageAsks++
	f.imagePrompts = append(f.imagePrompts, prompt)
	f.images = append(f.images, images)
	return f.resp, f.askErr
}

func (f *fakePi) Stop() {}

// newTestDerpies builds a Derpies over the fakes. `pi` is the interface
// type so a passed untyped nil yields a TRULY-nil App.Pi (a typed-nil
// *fakePi pointer would defeat the flow's h.app.Pi == nil guard).
// The store's prompt defaults to the code-pinned template so every flow
// test's prompt assertion (computed against
// gimmickPrompt(defaultPromptTemplate, ...)) stays self-consistent.
func newTestDerpies(store *fakeStore, ops *fakeOps, pi app.PiBackend) *Derpies {
	if store.prompt == "" && store.promptErr == nil {
		store.prompt = defaultPromptTemplate
	}
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
// parseVerdictScore
// ---------------------------------------------------------------------------

func TestParseVerdictScore(t *testing.T) {
	tests := []struct {
		in        string
		wantScore int
		wantHas   bool
		wantWord  string
	}{
		{"SCORE:95\nWORD:zwift", 95, true, "zwift"},
		{"SCORE:95", 95, true, ""},
		{"CLEAN", 0, false, ""},                                             // the old verdict: no score line
		{"GIMMICK:zwift", 0, false, ""},                                     // the old verdict: no score line
		{"SCORE:150", 0, false, ""},                                         // out of range: not a valid score line
		{"SCORE:55\nWORD:زويفت", 55, true, "زويفت"},                         // non-ASCII word: lowercased (a no-op here)
		{"  SCORE:95\n  WORD:zwift", 95, true, "zwift"},                     // the prompt displays the format indented: an LLM echoing the indentation must still parse
		{"Score: 95", 95, true, ""},                                         // a space after the colon: the most common LLM shape
		{"Sure!\nSCORE:80\nWORD:swift\nHope that helps", 80, true, "swift"}, // prose around the lines
		{"SCORE:70\nWORD:z w i f t", 70, true, "z w i f t"},                 // a spaced SPLIT answer is captured verbatim (the gate's foldVerdictWord collapses it)
		{"SCORE:150\nSCORE:50", 50, true, ""},                               // an out-of-range line is skipped; the next in-range line is the score
		{"", 0, false, ""},
	}
	for _, tt := range tests {
		score, hasScore, word := parseVerdictScore(tt.in)
		if score != tt.wantScore || hasScore != tt.wantHas || word != tt.wantWord {
			t.Errorf("parseVerdictScore(%q) = (%d, %v, %q), want (%d, %v, %q)", tt.in, score, hasScore, word, tt.wantScore, tt.wantHas, tt.wantWord)
		}
	}
}

// ---------------------------------------------------------------------------
// wordLike (the anchor-gate token classifier)
// ---------------------------------------------------------------------------

func TestWordLike(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"zwift", true},
		{"272889785318768641", false},          // a pure digit string is never word-like
		{"derpies:1021692390177775657", false}, // punctuation + digits: not a word shape
		{"", false},
		{"خفيف", true}, // non-ASCII letters-only shape (unicodeVerdictShape)
		{"a", false},   // 1 rune: below the 2-rune floor on both arms
	}
	for _, tt := range tests {
		if got := wordLike(tt.in); got != tt.want {
			t.Errorf("wordLike(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// foldedTokenSequence (the ordered, edge-trimmed, folded token sequence)
// ---------------------------------------------------------------------------

func TestFoldedTokenSequence(t *testing.T) {
	got := foldedTokenSequence("who wants to buy me a bike.")
	want := []string{"who", "wants", "to", "buy", "me", "a", "bike"}
	if len(got) != len(want) {
		t.Fatalf("foldedTokenSequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("foldedTokenSequence[%d] = %q, want %q (order must be preserved)", i, got[i], want[i])
		}
	}

	// NO collapsed-run handling (that stays in tokensForMatch): a split word
	// keeps its single-rune tokens in order.
	got2 := foldedTokenSequence("buy me a b i k e")
	want2 := []string{"buy", "me", "a", "b", "i", "k", "e"}
	if len(got2) != len(want2) {
		t.Fatalf("foldedTokenSequence = %v, want %v (no collapse)", got2, want2)
	}
	for i := range want2 {
		if got2[i] != want2[i] {
			t.Errorf("foldedTokenSequence[%d] = %q, want %q", i, got2[i], want2[i])
		}
	}

	// A pure-punctuation token trims to "" and is dropped.
	got3 := foldedTokenSequence("a ؟ b")
	want3 := []string{"a", "b"}
	if len(got3) != len(want3) {
		t.Fatalf("foldedTokenSequence = %v, want %v (pure-punct dropped)", got3, want3)
	}
	for i := range want3 {
		if got3[i] != want3[i] {
			t.Errorf("foldedTokenSequence[%d] = %q, want %q", i, got3[i], want3[i])
		}
	}
}

// ---------------------------------------------------------------------------
// clampThreshold
// ---------------------------------------------------------------------------

func TestClampThreshold(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{30, 50},   // below the floor (T > learnFloor only): the default
		{41, 41},   // the lowest representable T: passes
		{50, 50},   // the default
		{100, 100}, // the top: passes
		{101, 50},  // over the top: the default
	}
	for _, tt := range tests {
		if got := clampThreshold(tt.in); got != tt.want {
			t.Errorf("clampThreshold(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// The zero-value fake's configThreshold must yield the default 50 — a
// zero-threshold fake must not make every scored verdict delete.
func TestFakeStoreConfigThresholdZeroValue(t *testing.T) {
	s := &fakeStore{}
	got, err := s.configThreshold(context.Background())
	if err != nil {
		t.Fatalf("configThreshold on a zero-value fake = error %v, want nil", err)
	}
	if got != 50 {
		t.Errorf("configThreshold on a zero-value fake = %d, want 50 (the default)", got)
	}
}

// ---------------------------------------------------------------------------
// validTemplate / gimmickPrompt (template substitution)
// ---------------------------------------------------------------------------

func TestValidTemplate(t *testing.T) {
	if !validTemplate(defaultPromptTemplate) {
		t.Errorf("validTemplate(defaultPromptTemplate) = false, want true")
	}
	// Missing the mandatory {content} marker: invalid.
	if validTemplate(strings.Replace(defaultPromptTemplate, "{content}", "DELETED", 1)) {
		t.Errorf("a template missing {content} must be invalid")
	}
	// Missing the mandatory {known} marker: invalid.
	if validTemplate(strings.Replace(defaultPromptTemplate, "{known}", "DELETED", 1)) {
		t.Errorf("a template missing {known} must be invalid")
	}
	// Absent OPTIONAL markers: still valid (the elements are simply omitted).
	noOptional := strings.ReplaceAll(defaultPromptTemplate, "{{IMAGES}}\n", "")
	noOptional = strings.ReplaceAll(noOptional, "{{REF}}\n", "")
	if !validTemplate(noOptional) {
		t.Errorf("a template with the optional markers removed must still be valid")
	}
	if !validTemplate(`a {content} b {known}`) {
		t.Errorf("a minimal template with both mandatory markers must be valid")
	}
}

func TestGimmickPromptDefault(t *testing.T) {
	// Los depends on the sorted literal (the function joins; the CALLER
	// sorts via sortedKeys — asserted in the flow tests).
	known := []string{"bike", "sw1ft"}
	content := "holler at zswiftf now"

	// 0-image / no-ref form.
	got := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", "")
	for _, part := range []string{
		"<<<UNTRUSTED MESSAGE",
		content,
		"UNTRUSTED MESSAGE>>>",
		"HE WILL TEST THIS FILTER",
		"Techniques he uses",
		"Judgement rules",
		"  GIMMICK:<word>",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("prompt must contain %q", part)
		}
	}
	// The known-word block: both words, in the passed (sorted) order —
	// bike before sw1ft.
	bikeIdx := strings.Index(got, "\nbike\n")
	sw1ftIdx := strings.Index(got, "\nsw1ft\n")
	if bikeIdx < 0 || sw1ftIdx < 0 {
		t.Fatalf("prompt must contain the known-word block (bike and sw1ft, one per line):\n%q", got)
	}
	if bikeIdx > sw1ftIdx {
		t.Errorf("known words must stay in order: bike at %d must come before sw1ft at %d", bikeIdx, sw1ftIdx)
	}
	// 0 images / no ref: the optional blocks must be absent.
	if strings.Contains(got, "The message also has") {
		t.Errorf("0-image form must not contain the images line")
	}
	if strings.Contains(got, "REFERENCED MESSAGE") {
		t.Errorf("no-ref form must not contain the referenced block")
	}
	// An empty known list: the block is empty, not a header without a body.
	if gotEmpty := gimmickPrompt(defaultPromptTemplate, content, nil, 0, 0, "", ""); strings.Contains(gotEmpty, "known gimmick words (sorted ascending)") {
		t.Errorf("empty known list must omit the block header")
	}

	// 2-image form: the pinned images line with N = 2.
	gotImg := gimmickPrompt(defaultPromptTemplate, content, known, 2, 0, "", "")
	if !strings.Contains(gotImg, "The message also has 2 attached image(s) (screenshots or pasted images \u2014 a text filter would not see their content). Judge the text AND the images. If the anchor word appears in an image rather than the message text, name it as if it were in the message.") {
		t.Errorf("2-image form must contain the pinned images line with 2 in: %q", gotImg)
	}
	// Ref form: the pinned ref block around the ref text.
	const refText = "the quoted earlier message"
	gotRef := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", refText)
	wantRef := "<<<REFERENCED MESSAGE\n" + refText + "\nREFERENCED MESSAGE>>>\nThe message replies to a previous message (often the author's own) \u2014 the quoted content is above between the REFERENCED MESSAGE markers. Judge the posted text / images AND the quoted content together; a respelling may live in the quote rather than the new message."
	if !strings.Contains(gotRef, wantRef) {
		t.Errorf("ref form must contain the pinned ref block with the ref text")
	}

	// sortedKeys: 3-key map -> sorted keys (deterministic prompt order).
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

func TestGimmickPromptMissingOptionalMarkers(t *testing.T) {
	// The default template with the optional markers removed: a 2-image / ref form must substitute normally — no crash, the payoff elements are simply absent, everything else intact.
	noOptional := strings.ReplaceAll(defaultPromptTemplate, "{{IMAGES}}\n", "")
	noOptional = strings.ReplaceAll(noOptional, "{{REF}}\n", "")
	got := gimmickPrompt(noOptional, "holler at zswiftf now", []string{"bike"}, 2, 0, "", "the quoted earlier message")

	for _, part := range []string{
		"<<<UNTRUSTED MESSAGE",
		"holler at zswiftf now",
		"UNTRUSTED MESSAGE>>>",
		"HE WILL TEST THIS FILTER",
		"  GIMMICK:<word>",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("prompt must still contain %q", part)
		}
	}
	if strings.Contains(got, "The message also has") {
		t.Errorf("the images line must not appear when the marker is absent; got:\n%q", got)
	}
	if strings.Contains(got, "REFERENCED MESSAGE") {
		t.Errorf("the referenced block must not appear when the marker is absent; got:\n%q", got)
	}
}

func TestGimmickPromptCustomTemplate(t *testing.T) {
	// A minimal valid custom template: markers replaced, the wrapper bytes
	// come from the code (the fence is code-pinned, not in the template).
	got := gimmickPrompt(`<<<{content}>>>{known}`, "holler at zswiftf now", []string{"bike"}, 0, 0, "", "")
	want := "<<<\n<<<UNTRUSTED MESSAGE\nholler at zswiftf now\n               UNTRUSTED MESSAGE>>>\n>>>-----< known gimmick words (sorted ascending) >-----\nbike"
	if got != want {
		t.Errorf("custom template substitution:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(got, "<<<UNTRUSTED MESSAGE\nholler at zswiftf now\n               UNTRUSTED MESSAGE>>>") {
		t.Errorf("custom template must carry the code-pinned fence around the content; got:\n%q", got)
	}
	for _, part := range []string{"{content}", "{known}"} {
		if strings.Contains(got, part) {
			t.Errorf("template marker %q must have been replaced", part)
		}
	}
}

func TestGimmickPromptNoMarkerReTrigger(t *testing.T) {
	// The substitution must be two-phase: the markers are turned into inert
	// placeholders on the TEMPLATE before any payload is inserted, so a
	// message that literally contains "{content}" / "{known}" /
	// "{{IMAGES}}" / "{{REF}}" can never be re-scanned by a later pass.
	// Under the old single-pass ReplaceAll ordering, this content
	// re-triggered the later passes: the real known block landed INSIDE the
	// untrusted message region and the images/ref passes silently deleted
	// their marker bytes from the very message the LLM judges.
	known := []string{"bike", "sw1ft"}
	content := "hey {content} {known} {{IMAGES}} {{REF}} look"
	got := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", "")

	// 1) The legitimate known block appears EXACTLY ONCE — at the template
	//    marker position, never inside the message region.
	const header = "-----< known gimmick words (sorted ascending) >-----"
	if n := strings.Count(got, header); n != 1 {
		t.Errorf("known block header appears %d times, want exactly 1:\n%s", n, got)
	}

	// 2) 0 images / no ref: neither optional element may appear at all.
	if strings.Contains(got, "The message also has") {
		t.Errorf("0-image form must not contain the images line")
	}
	if strings.Contains(got, "REFERENCED MESSAGE") {
		t.Errorf("no-ref form must not contain the referenced block")
	}

	// 3) The template's own {content} marker is substituted exactly at its
	//    template position — the content appears EXACTLY ONCE in the whole
	//    prompt (never duplicated by a re-scan of the payload).
	if n := strings.Count(got, content); n != 1 {
		t.Errorf("content appears %d times, want exactly 1 (substituted once at the template position):\n%s", n, got)
	}

	// 4) The message region (between the fence markers) keeps the raw
	//    literals VERBATIM — {content} / {known} byte-present, not
	//    substituted — and holds no trace of the known block.
	start := strings.Index(got, "<<<UNTRUSTED MESSAGE")
	end := strings.Index(got, "UNTRUSTED MESSAGE>>>")
	if start < 0 || end < 0 {
		t.Fatalf("prompt must contain the fenced message region:\n%s", got)
	}
	region := got[start : end+len("UNTRUSTED MESSAGE>>>")]
	for _, lit := range []string{content, "{content}", "{known}", "{{IMAGES}}", "{{REF}}"} {
		if !strings.Contains(region, lit) {
			t.Errorf("message region must keep %q verbatim (re-trigger bug); region:\n%s", lit, region)
		}
	}
	if strings.Contains(region, header) {
		t.Errorf("message region must not contain the known block header; region:\n%s", region)
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

// ---------------------------------------------------------------------------
// tokensForMatch (unicode fold)
// ---------------------------------------------------------------------------

func TestTokensForMatchUnicode(t *testing.T) {
	got := tokensForMatch("A świft cog")
	want := map[string]bool{"a": true, "swift": true, "cog": true}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want keys %v (folded)", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("tokens missing key %q: %v", k, got)
		}
	}

	// Folding is applied before the punctuation trim (the trimmed folded
	// token "zwift" survives the trailing ".").
	got2 := tokensForMatch("get me a žwift.")
	want2 := map[string]bool{"get": true, "me": true, "a": true, "zwift": true}
	if len(got2) != len(want2) {
		t.Fatalf("tokens = %v, want keys %v (folded)", got2, want2)
	}
	for k := range want2 {
		if !got2[k] {
			t.Errorf("tokens missing key %q: %v", k, got2)
		}
	}
}

// ---------------------------------------------------------------------------
// tokensForMatch (unicode edge punctuation — PR #3 review follow-up)
// ---------------------------------------------------------------------------

func TestTokensForMatchArabicPunctTrim(t *testing.T) {
	// «خفيف؟» (؟ U+061F ARABIC QUESTION MARK, category Po): the edge
	// punctuation must be trimmed off the folded token so the key is the
	// verdict word خفيف (slow path) and the stored word (fast path).
	got := tokensForMatch("ما هذا؟ خفيف؟")
	if !got["خفيف"] {
		t.Errorf("tokens %v: missing key خفيف (؟ edge punctuation must be trimmed)", got)
	}
	if got["خفيف؟"] {
		t.Errorf("tokens %v: key خفيف؟ must not survive (؟ is edge punctuation)", got)
	}

	// Leading Arabic comma (، U+060C, Po) trims the same way.
	got2 := tokensForMatch("،خفيف")
	if len(got2) != 1 || !got2["خفيف"] {
		t.Errorf("tokens = %v, want {خفيف} (leading ، trimmed)", got2)
	}

	// An all-punctuation token must not yield an empty key.
	if got3 := tokensForMatch("؟"); len(got3) != 0 {
		t.Errorf("tokens = %v, want {} (a pure-punctuation token yields no key)", got3)
	}

	// Controls: the ASCII path is byte-identical (trim is EDGES ONLY —
	// interior "s-w1ft" survives, "sw1ft." loses the dot).
	got4 := tokensForMatch("swift sw1ft. s-w1ft")
	if len(got4) != 3 || !got4["swift"] || !got4["sw1ft"] || !got4["s-w1ft"] {
		t.Errorf("tokens = %v, want {swift, sw1ft, s-w1ft} (ASCII path unchanged; interior - preserved)", got4)
	}
}

// ---------------------------------------------------------------------------
// tokensForMatch (SPLIT — a known word spread over spaces)
// ---------------------------------------------------------------------------

func TestTokensForMatchCollapsedSplit(t *testing.T) {
	// The SPLIT evasion (the 2026-09-17 prod case): a known word spread over
	// spaces. "z w i f t" must yield the COLLAPSED "zwift" (in addition to
	// the individual single-rune tokens) so the fast path (zwift is a seed)
	// and the verdict gate (anchor) both see it.
	got := tokensForMatch("I want to make sure you get the points and free dlc for recommending the z w i f t")
	if !got["zwift"] {
		t.Errorf("tokens = %v: missing collapsed key zwift (z w i f t must collapse)", got)
	}
	// The individual single-rune tokens are still present (the collapse is
	// additive, not a replacement).
	if !got["z"] || !got["w"] || !got["i"] || !got["f"] || !got["t"] {
		t.Errorf("tokens = %v: single-rune tokens must still be present", got)
	}

	// A run broken by a multi-rune word does NOT collapse across it: "z wift"
	// has "wift" (4 runes) breaking the single-rune run, so no "zwift".
	got2 := tokensForMatch("z wift")
	if got2["zwift"] {
		t.Errorf("tokens = %v: 'z wift' must NOT collapse to zwift (wift is multi-rune and breaks the run)", got2)
	}

	// A lone single-rune token does not collapse (a run needs >=2).
	got3 := tokensForMatch("a")
	if len(got3) != 1 || !got3["a"] {
		t.Errorf("tokens = %v: want exactly {a}", got3)
	}

	// A pure-punctuation token between letters does NOT break the run (a dot
	// wedged between letters is part of the split, not a word boundary):
	// "z . w i f t" still collapses to "zwift".
	got4 := tokensForMatch("z . w i f t")
	if !got4["zwift"] {
		t.Errorf("tokens = %v: 'z . w i f t' must collapse to zwift (pure-punct does not break the run)", got4)
	}

	// A run longer than 32 runes is NOT added (wordValid's max — the
	// collapsed form can never match a stored word beyond it): 33 single-rune
	// tokens collapse to a 33-rune string that must be absent.
	got5 := tokensForMatch(strings.Repeat("a ", 33))
	if got5[strings.Repeat("a", 33)] {
		t.Errorf("tokens = %v: a >32-rune collapsed run must not be added", got5)
	}
}

// ---------------------------------------------------------------------------
// The flow (unicode cases)
// ---------------------------------------------------------------------------

func TestFlowUnicodeFastHit(t *testing.T) {
	// The prod case: the unicode respelling of a seed word folds to the
	// seed, so the fast path hits with no pi ask.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"swift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("A świft cog wrapped in some vintage Gianna mags"))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one fast delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the fast path must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

// The prod leak (2026-09-07 20:36): a message whose only roster trace is
// a NON-DECOMPOSABLE respelling — before the confusable fold, no valid
// verdict existed for it (the base word "zwift" is not a folded token of
// "zwіft", and the variant itself is not wordValid), so the LLM's best
// available answer was CLEAN and the message leaked. Now the token folds
// to the anchor, so the fast path catches it directly.
func TestFlowConfusableNonDecomposableFastHit(t *testing.T) {
	// и = U+0438 (Cyrillic small i, short i) — NFD does not decompose it.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("im pretty sure they make one for big handed individuals, zw\u0438ft tries to cater to all groups"))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one fast delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the fast path must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestFlowZeroWidthSpacedFastHit(t *testing.T) {
	// Zero-width space (Cf, U+200B) wedged inside the token: stripped by
	// the fold, the message token folds to the learned word.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("fish me a Zwi\u200Bft please."))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one fast delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	assertNothingLearned(t, store)
}

// PR #3 review follow-up: the message token «خفف؟» (؟ = U+061F ARABIC
// QUESTION MARK, category Po) — before the unicode edge-trim the folded
// token «خفف؟» was NOT equal to the stored word, so the fast path
// missed; now the edge punctuation is trimmed off and the fast path
// catches it with no pi ask.
func TestFlowFastPathArabicPunctHit(t *testing.T) {
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"خفف": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("إعطني خفف؟"))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one fast delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the fast path must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

// Verdict coherence on the same shape: with an EMPTY word list the
// fast path cannot hit, the slow path judging "GIMMICK:zwift" must now
// PASS the two-arm gate (the folded verdict word IS a folded token
// thanks to the confusable fold) — learning "zwift" and deleting.
// Pre-fold this exact verdict was rejected ("verdict word not in the
// message") and the message leaked.
func TestFlowConfusableVerdictCoherence(t *testing.T) {
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("im pretty sure they make one for big handed individuals, zw\u0438ft tries to cater to all groups"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (the folded word is stored)", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

func TestFlowUnicodeVerdictLearnsFolds(t *testing.T) {
	// "GIMMICK:žwift" evaluates as the folded word "zwift": the gate
	// passes and the FOLDED word is stored.
	pi := &fakePi{resp: "GIMMICK:žwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("You should save the money and get me a žwift instead"))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (the folded word is stored)", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

// ---------------------------------------------------------------------------
// ADR 0008 — non-ASCII verdict words, text-anchored
// ---------------------------------------------------------------------------

func TestFlowUnicodeVerdictWordLearnsAndDeletes(t *testing.T) {
	// خفيف (Arabic, "light") passes the fold UNCHANGED (it has no Latin
	// confusable, so it is not wordValid), but — ADR 0008 — it IS a
	// verbatim folded token of the message, so the extended gate accepts
	// it: the FOLDED word (= the same string) is learned and the message
	// deleted. The fast path then catches the next verbatim occurrence.
	// Pre-fix this FAILS ("derpies invalid verdict word — doing nothing").
	pi := &fakePi{resp: "GIMMICK:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("once you guy me a خفيف no problem"))

	if len(store.added) != 1 || store.added[0] != "خفيف|llm" {
		t.Errorf("added = %v, want [خفيف|llm] (the folded form of a no-confusable non-ASCII word folds to itself)", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

func TestFlowUnicodeVerdictWordNotInMessageStillRejected(t *testing.T) {
	// The text-anchored arm still REJECTS a non-ASCII word that is absent
	// from the judged text (word not in the message — the toks[fw] gate is
	// unchanged for it): not learned, not deleted.
	pi := &fakePi{resp: "GIMMICK:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("once you guy me a no problem"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (the gate rejects after the ask)", pi.asks)
	}
}

func TestFlowUnicodeVerdictWordPunctuationRejected(t *testing.T) {
	// A verdict word with PUNCTUATION wedged in (a trailing comma) is not
	// a letters-only shape: the shape check rejects it — the token-trim
	// escape ("خفيف!" would fold+trim to a token) does NOT apply to
	// verdict words. Not learned, not deleted.
	pi := &fakePi{resp: "GIMMICK:خفيف,"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("a b c"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowUnicodeVerdictAllDigitsRejected(t *testing.T) {
	// A PURE digit string is NEVER a valid verdict word — even though it
	// satisfies wordValid's ASCII charset and IS a folded token of the
	// (verbatim numeric) text: the twist word must contain at least one
	// letter. Not learned, not deleted.
	pi := &fakePi{resp: "GIMMICK:12345"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("12345"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

// PR #3 review follow-up: the message «خفف؟» (Arabic, خفف followed by the
// ARABIC QUESTION MARK U+061F, category Po). Before the unicode
// token-edge trim the folded token was «خفف؟» — NOT equal to the verdict
// word, so the gate rejected it ("verdict word not in the message —
// doing nothing") and the message leaked. Now the trailing native-script
// punctuation is trimmed off the folded token, so the word anchors: it is
// learned and the message deleted.
func TestFlowArabicPunctAnchoredVerdictLearnsAndDeletes(t *testing.T) {
	pi := &fakePi{resp: "GIMMICK:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("خفيف؟ لا مشكلة"))

	if len(store.added) != 1 || store.added[0] != "خفيف|llm" {
		t.Errorf("added = %v, want [خفيف|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

func TestFlowVerdictBaseFormStillRejected(t *testing.T) {
	// A base/known word that is NOT a token of the message is rejected
	// by the token gate (the prod rejection case: "swift" is not a
	// folded token of a "zwift" message).
	pi := &fakePi{resp: "GIMMICK:swift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("Can it fish me a zwift ?"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (the gate rejects after the ask)", pi.asks)
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
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "")
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want %q", pi.prompts[0], want)
	}
}

func TestFlowSplitFastHit(t *testing.T) {
	// The 2026-09-17 prod case: zwift is a seed, and "z w i f t" (the SPLIT
	// evasion) collapses to "zwift" in the token union -> fast delete, zero
	// pi asks (the LLM is never consulted for a fast hit).
	content := "I want to make sure you get the points and free dlc for recommending the z w i f t"
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"zwift": true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg(content))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want [[c1 msg1]] (fast path)", ops.deleted)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (fast path must not reach pi)", pi.asks)
	}
	assertNothingLearned(t, store)
}

func TestFlowSplitVerdictLearnsAndDeletes(t *testing.T) {
	// A NEW split word (not in the list): the LLM answers the COLLAPSED form
	// "zwift"; the gate anchors it to the collapsed run and learns it, so the
	// next identical post is a fast delete.
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	content := "recommending the z w i f t"
	h.flow(derpMsg(content))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm]", store.added)
	}
	if len(ops.deleted) != 1 {
		t.Errorf("deleted = %v, want one (llm path)", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (fell through to the slow path)", pi.asks)
	}
}

func TestFlowSplitVerdictSpacedFormLearns(t *testing.T) {
	// The LLM answers the word AS IT APPEARS (with the spaces): "z w i f t".
	// The gate collapses the whitespace -> "zwift", anchors it to the
	// collapsed run, and learns the COLLAPSED form (robust to LLM compliance
	// on the answer shape).
	pi := &fakePi{resp: "GIMMICK:z w i f t"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	content := "recommending the z w i f t"
	h.flow(derpMsg(content))

	if len(store.added) != 1 || store.added[0] != "zwift|llm" {
		t.Errorf("added = %v, want [zwift|llm] (a spaced verdict collapses to the folded form)", store.added)
	}
	if len(ops.deleted) != 1 {
		t.Errorf("deleted = %v, want one", ops.deleted)
	}
}

func TestFlowSplitVerdictNotAnchoredStillRejected(t *testing.T) {
	// "zwift" is a valid word but the message has NO split "z w i f t" (no
	// collapsed run): the token gate keeps it out. The collapse does NOT
	// create an anchor out of nothing — a hallucinated "zwift" on clean text
	// is still rejected.
	pi := &fakePi{resp: "GIMMICK:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowPromptFallbackOnStoreError(t *testing.T) {
	// The store seam errors (DB unavailable): the flow must fall back to the code default — the filter never runs with a broken prompt.
	content := "completely clean text"
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, promptErr: errors.New("db down")}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg(content))

	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "")
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want the code-default substitution %q", pi.prompts[0], want)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowPromptFallbackOnInvalidTemplate(t *testing.T) {
	// The store returns a present-but-invalid template (operator cleared the markers): silent fallback to the code default.
	content := "completely clean text"
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, prompt: "no markers here"}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg(content))

	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "")
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want the code-default substitution %q", pi.prompts[0], want)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
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
