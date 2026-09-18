// Package derpies is the filter for the user(s) in
// config.Config.DerpiesUserIDs: a fast path (exact gimmick-word token
// match against the derpies_gimmicks list) then a slow path (a pi RPC
// verdict — SCORE:<0-100> (+ WORD:<anchor>) — scored against the decision
// matrix, that learns new words into the list at runtime).
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
	got := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", "", nil)
	for _, part := range []string{
		"<<<UNTRUSTED MESSAGE",
		content,
		"UNTRUSTED MESSAGE>>>",
		"HE WILL TEST THIS FILTER",
		"Techniques he uses",
		"Judgement rules",
		"  SCORE:<0-100>",
		"  WORD:<anchor>",
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
	if gotEmpty := gimmickPrompt(defaultPromptTemplate, content, nil, 0, 0, "", "", nil); strings.Contains(gotEmpty, "known gimmick words (sorted ascending)") {
		t.Errorf("empty known list must omit the block header")
	}

	// 2-image form: the pinned images line with N = 2.
	gotImg := gimmickPrompt(defaultPromptTemplate, content, known, 2, 0, "", "", nil)
	if !strings.Contains(gotImg, "The message also has 2 attached image(s) (screenshots or pasted images \u2014 a text filter would not see their content). Judge the text AND the images. If the anchor word appears in an image rather than the message text, name it as if it were in the message.") {
		t.Errorf("2-image form must contain the pinned images line with 2 in: %q", gotImg)
	}
	// Ref form: the pinned ref block around the ref text.
	const refText = "the quoted earlier message"
	gotRef := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", refText, nil)
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
	got := gimmickPrompt(noOptional, "holler at zswiftf now", []string{"bike"}, 2, 0, "", "the quoted earlier message", nil)

	for _, part := range []string{
		"<<<UNTRUSTED MESSAGE",
		"holler at zswiftf now",
		"UNTRUSTED MESSAGE>>>",
		"HE WILL TEST THIS FILTER",
		"  SCORE:<0-100>",
		"  WORD:<anchor>",
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
	got := gimmickPrompt(`<<<{content}>>>{known}`, "holler at zswiftf now", []string{"bike"}, 0, 0, "", "", nil)
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

func TestGimmickPromptPhrasesSubBlock(t *testing.T) {
	// The {known} phrases sub-block (the optimizer): emitted ONLY when
	// phrases exist, appended AFTER the words sub-block; the words
	// sub-block is unchanged.
	const phrHeader = "-----< known gimmick phrases (exact multi-word patterns) >-----"
	const wordsHeader = "-----< known gimmick words (sorted ascending) >-----"
	content := "holler at zswiftf now"

	// Both words and phrases: the phrases header follows the words
	// header, with the phrase lines after it.
	got := gimmickPrompt(defaultPromptTemplate, content, []string{"bike"}, 0, 0, "", "", []string{"buy me a bike", "give me a sw1ft"})
	wordsIdx := strings.Index(got, wordsHeader)
	phrIdx := strings.Index(got, phrHeader)
	if wordsIdx < 0 || phrIdx < 0 {
		t.Fatalf("prompt must contain both sub-block headers:\n%q", got)
	}
	if phrIdx <= wordsIdx {
		t.Errorf("phrases sub-block must be positioned AFTER the words sub-block (words at %d, phrases at %d)", wordsIdx, phrIdx)
	}
	// The phrase lines follow the phrases header.
	if !strings.Contains(got[phrIdx:], "buy me a bike") || !strings.Contains(got[phrIdx:], "give me a sw1ft") {
		t.Errorf("phrases sub-block must carry the phrase lines:\n%q", got[phrIdx:])
	}

	// Empty words + phrases: the phrases sub-block is emitted on its own
	// (the words header is absent), and the phrase lines are present.
	gotPhrOnly := gimmickPrompt(defaultPromptTemplate, content, nil, 0, 0, "", "", []string{"buy me a bike"})
	if strings.Contains(gotPhrOnly, wordsHeader) {
		t.Errorf("empty words list must omit the words header:\n%q", gotPhrOnly)
	}
	if !strings.Contains(gotPhrOnly, phrHeader) || !strings.Contains(gotPhrOnly, "buy me a bike") {
		t.Errorf("phrases-only form must carry the phrases sub-block:\n%q", gotPhrOnly)
	}

	// No phrases: the phrases sub-block is omitted and the words
	// sub-block is unchanged (no trailing header or line of its own).
	gotNoPhr := gimmickPrompt(defaultPromptTemplate, content, []string{"bike"}, 0, 0, "", "", nil)
	if strings.Contains(gotNoPhr, phrHeader) {
		t.Errorf("no-phrases form must omit the phrases sub-block:\n%q", gotNoPhr)
	}
	if !strings.Contains(gotNoPhr, wordsHeader) || !strings.Contains(gotNoPhr, "bike") {
		t.Errorf("no-phrases form must keep the words sub-block:\n%q", gotNoPhr)
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
	content := "hey {content} {known} {{IMAGES}} {{GIFS}} {{EMBED}} {{REF}} look"
	got := gimmickPrompt(defaultPromptTemplate, content, known, 0, 0, "", "", nil)

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
	for _, lit := range []string{content, "{content}", "{known}", "{{IMAGES}}", "{{GIFS}}", "{{EMBED}}", "{{REF}}"} {
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
	pi := &fakePi{resp: "SCORE:95\nWORD:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// "SCORE:95\nWORD:žwift" evaluates as the folded word "zwift": the gate
	// passes and the FOLDED word is stored.
	pi := &fakePi{resp: "SCORE:95\nWORD:žwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	pi := &fakePi{resp: "SCORE:95\nWORD:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// unchanged for it): not learned, not deleted. The 40..T-1 score band
	// attempts the learn (the word is what is rejected, not the score),
	// and the delete stays out on score — the rejection's discriminating
	// intent is preserved.
	pi := &fakePi{resp: "SCORE:45\nWORD:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// verdict words. Not learned, not deleted (the 40..T-1 band attempts
	// the learn; the word rejection is what keeps it out).
	pi := &fakePi{resp: "SCORE:45\nWORD:خفيف,"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// letter. Not learned, not deleted (the 40..T-1 band attempts the
	// learn; the word rejection is what keeps it out).
	pi := &fakePi{resp: "SCORE:45\nWORD:12345"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	pi := &fakePi{resp: "SCORE:95\nWORD:خفيف"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{}, threshold: 60}
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
	// folded token of a "zwift" message). The 40..T-1 band attempts the
	// learn; the word rejection is what keeps it out.
	pi := &fakePi{resp: "SCORE:45\nWORD:swift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// gate keeps a hallucinated word out of the list. The 40..T-1 band
	// attempts the learn; the word rejection is what keeps it out.
	pi := &fakePi{resp: "SCORE:45\nWORD:xyzzy"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(derpMsg("completely clean text"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowVerdictInvalidWord(t *testing.T) {
	t.Run("charset gate rejects s-w1ft", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:45\nWORD:s-w1ft"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("s-w1ft here"))

		assertNothingLearned(t, store)
		assertNoDeletes(t, ops)
	})
	t.Run("token gate rejects sw1ft for content sw-1ft", func(t *testing.T) {
		// wordValid("sw1ft") passes, but sw1ft is not a token of
		// "sw-1ft" (tokenization yields "sw-1ft").
		pi := &fakePi{resp: "SCORE:45\nWORD:sw1ft"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("sw-1ft here"))

		assertNothingLearned(t, store)
		assertNoDeletes(t, ops)
	})
}

func TestFlowVerdictLearnsAndDeletes(t *testing.T) {
	pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "", nil)
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
	pi := &fakePi{resp: "SCORE:95\nWORD:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	pi := &fakePi{resp: "SCORE:95\nWORD:z w i f t"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	// is still rejected. The 40..T-1 band attempts the learn; the word
	// rejection is what keeps it out.
	pi := &fakePi{resp: "SCORE:45\nWORD:zwift"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "", nil)
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
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "", nil)
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want the code-default substitution %q", pi.prompts[0], want)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowConfigFallback(t *testing.T) {
	// The config fallback (1): a configThreshold error (or a missing row)
	// degrades to defaultThreshold (50) — the matrix still runs on the
	// default T, and the decision row carries it.
	t.Run("threshold error: the default T (50) applies", func(t *testing.T) {
		// The errored value (90) must NOT apply: 55 >= 50 deletes, 55 < 90
		// would not — the delete is the assertion that the default ran.
		pi := &fakePi{resp: "SCORE:55\nWORD:zswiftf"}
		store := &fakeStore{
			enabled:      map[string]bool{FeatureKey: true},
			threshold:    90,
			thresholdErr: errors.New("db down"),
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the default T=50 applied, not the errored 90)", ops.deleted)
		}
		if len(store.decisions) != 1 {
			t.Fatalf("decisions = %d rows, want 1: %+v", len(store.decisions), store.decisions)
		}
		d := store.decisions[0]
		if d.Score == nil || *d.Score != 55 {
			t.Errorf("score = %v, want 55", d.Score)
		}
		if d.Threshold == nil || *d.Threshold != 50 {
			t.Errorf("threshold = %v, want 50 (the default)", d.Threshold)
		}
	})
	t.Run("threshold error, score below the default: no delete", func(t *testing.T) {
		// The other side of the assertion: a score below the default T
		// (but above the errored value's complement) must not delete —
		// the default T is what the matrix ran on.
		pi := &fakePi{resp: "SCORE:45\nWORD:zswiftf"}
		store := &fakeStore{
			enabled:      map[string]bool{FeatureKey: true},
			threshold:    40, // clamps to the default 50 anyway; the error is the point
			thresholdErr: errors.New("db down"),
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		// 45 >= 40 learns, 45 < 50 (the default) does not delete.
		if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
			t.Errorf("added = %v, want [zswiftf|llm] (the learn band)", store.added)
		}
		assertNoDeletes(t, ops)
	})
}

// TestFlowDecisionLogArms — the decision log (C): every terminal arm writes
// exactly one derpies_decisions row with the arm's shape; pre-gate arms
// write none; a write failure degrades (log, no abort).
func TestFlowDecisionLogArms(t *testing.T) {
	// assertDecision — the identity fields are set up front (after the
	// author gate) for every row.
	assertIdentity := func(t *testing.T, d *decisionRecord, content string) {
		t.Helper()
		if d.MessageID != "msg1" || d.ChannelID != "c1" || d.AuthorID != "163055057254875136" || d.Content != content {
			t.Errorf("identity = (msg=%q chan=%q author=%q content=%q), want (msg1 c1 163055057254875136 %q)",
				d.MessageID, d.ChannelID, d.AuthorID, d.Content, content)
		}
	}
	assertPtr := func(t *testing.T, got *string, want string, name string) {
		t.Helper()
		if (got == nil) != (want == "") {
			t.Errorf("%s = %v, want %q (nil-ness mismatch)", name, got, want)
			return
		}
		if got != nil && *got != want {
			t.Errorf("%s = %v, want %q", name, *got, want)
		}
	}
	assertIntPtr := func(t *testing.T, got *int, want int, name string) {
		t.Helper()
		if (got == nil) != (want < 0) {
			t.Errorf("%s = %v, want %v (nil-ness mismatch)", name, got, want)
			return
		}
		if got != nil && *got != want {
			t.Errorf("%s = %v, want %v", name, *got, want)
		}
	}

	run := func(t *testing.T, store *fakeStore, ops *fakeOps, pi app.PiBackend, content string) *decisionRecord {
		t.Helper()
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg(content))
		if len(store.decisions) != 1 {
			t.Fatalf("decisions = %d rows, want exactly 1: %+v", len(store.decisions), store.decisions)
		}
		return store.decisions[0]
	}

	t.Run("list fetch failed: reject_reason, no path/score/threshold", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, listErr: errors.New("db down")}
		d := run(t, store, &fakeOps{}, pi, "completely clean text")
		assertIdentity(t, d, "completely clean text")
		assertPtr(t, d.Path, "", "path")
		assertIntPtr(t, d.Score, -1, "score")
		assertIntPtr(t, d.Threshold, -1, "threshold")
		assertPtr(t, d.Word, "", "word")
		assertPtr(t, d.RejectReason, "list fetch failed", "reject_reason")
		if d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both false", d.Learned, d.Deleted)
		}
	})
	t.Run("fast word hit: path=fast, word, deleted", func(t *testing.T) {
		pi := &fakePi{}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
		d := run(t, store, &fakeOps{}, pi, "who's giving me a sw1ft.")
		assertIdentity(t, d, "who's giving me a sw1ft.")
		assertPtr(t, d.Path, "fast", "path")
		assertIntPtr(t, d.Score, -1, "score")
		assertIntPtr(t, d.Threshold, -1, "threshold")
		assertPtr(t, d.Word, "sw1ft", "word")
		assertPtr(t, d.RejectReason, "", "reject_reason")
		if !d.Deleted || d.Learned {
			t.Errorf("deleted=%v learned=%v, want deleted=true learned=false", d.Deleted, d.Learned)
		}
	})
	t.Run("fast word hit, delete failed: reject_reason overrides, deleted=false", func(t *testing.T) {
		pi := &fakePi{}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
		d := run(t, store, &fakeOps{delErr: errors.New("discord 500")}, pi, "who's giving me a sw1ft.")
		assertPtr(t, d.Path, "fast", "path")
		assertPtr(t, d.Word, "sw1ft", "word")
		assertPtr(t, d.RejectReason, "delete failed", "reject_reason")
		if d.Deleted {
			t.Errorf("deleted = true, want false (the delete failed)")
		}
	})
	t.Run("pi unavailable: reject_reason, no path", func(t *testing.T) {
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		d := run(t, store, &fakeOps{}, nil, "completely clean text")
		assertPtr(t, d.Path, "", "path")
		assertPtr(t, d.RejectReason, "pi unavailable", "reject_reason")
		if d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both false", d.Learned, d.Deleted)
		}
	})
	t.Run("ask failed: reject_reason, no path", func(t *testing.T) {
		pi := &fakePi{askErr: errors.New("pi down")}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		d := run(t, store, &fakeOps{}, pi, "completely clean text")
		assertPtr(t, d.Path, "", "path")
		assertIntPtr(t, d.Score, -1, "score")
		assertPtr(t, d.RejectReason, "ask failed", "reject_reason")
	})
	t.Run("unrecognized verdict: reject_reason, no path", func(t *testing.T) {
		// A legacy CLEAN / old GIMMICK verdict carries no SCORE line.
		pi := &fakePi{resp: "CLEAN"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		d := run(t, store, &fakeOps{}, pi, "completely clean text")
		assertPtr(t, d.Path, "", "path")
		assertIntPtr(t, d.Score, -1, "score")
		assertIntPtr(t, d.Threshold, -1, "threshold")
		assertPtr(t, d.RejectReason, "unrecognized verdict", "reject_reason")
		if d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both false", d.Learned, d.Deleted)
		}
	})
	t.Run("score < 40: slow path + score + threshold, no rejection", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:10\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		d := run(t, store, &fakeOps{}, pi, "completely clean text")
		assertPtr(t, d.Path, "slow", "path")
		assertIntPtr(t, d.Score, 10, "score")
		assertIntPtr(t, d.Threshold, 60, "threshold")
		assertPtr(t, d.Word, "", "word")
		assertPtr(t, d.RejectReason, "", "reject_reason")
		if d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both false (the do-nothing band)", d.Learned, d.Deleted)
		}
	})
	t.Run("40 <= score < T: learned, no delete, no rejection", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:45\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		d := run(t, store, &fakeOps{}, pi, "holler at zswiftf now")
		assertPtr(t, d.Path, "slow", "path")
		assertIntPtr(t, d.Score, 45, "score")
		assertIntPtr(t, d.Threshold, 60, "threshold")
		assertPtr(t, d.Word, "zswiftf", "word")
		assertPtr(t, d.RejectReason, "", "reject_reason")
		if !d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want learned=true deleted=false", d.Learned, d.Deleted)
		}
	})
	t.Run("score >= T: learned + deleted", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		d := run(t, store, &fakeOps{}, pi, "holler at zswiftf now")
		assertPtr(t, d.Path, "slow", "path")
		assertIntPtr(t, d.Score, 95, "score")
		assertIntPtr(t, d.Threshold, 60, "threshold")
		assertPtr(t, d.Word, "zswiftf", "word")
		assertPtr(t, d.RejectReason, "", "reject_reason")
		if !d.Learned || !d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both true", d.Learned, d.Deleted)
		}
	})
	t.Run("score >= T, delete failed: learned, deleted=false, reject_reason overrides", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		d := run(t, store, &fakeOps{delErr: errors.New("discord 500")}, pi, "holler at zswiftf now")
		if !d.Learned {
			t.Errorf("learned = false, want true (a delete failure must not un-learn the word)")
		}
		if d.Deleted {
			t.Errorf("deleted = true, want false")
		}
		assertPtr(t, d.RejectReason, "delete failed", "reject_reason")
	})
	t.Run("word rejected: the specific reject_reason, no learn, no delete", func(t *testing.T) {
		// 40..T-1 band: the learn is attempted, the word is rejected (not
		// in the message), the delete stays out on score.
		pi := &fakePi{resp: "SCORE:45\nWORD:xyzzy"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		d := run(t, store, &fakeOps{}, pi, "completely clean text")
		assertPtr(t, d.Path, "slow", "path")
		assertIntPtr(t, d.Score, 45, "score")
		assertIntPtr(t, d.Threshold, 60, "threshold")
		assertPtr(t, d.Word, "", "word")
		assertPtr(t, d.RejectReason, "verdict word not in message", "reject_reason")
		if d.Learned || d.Deleted {
			t.Errorf("learned=%v deleted=%v, want both false", d.Learned, d.Deleted)
		}
	})
	t.Run("recordDecision insert failure: degrades, does not abort the delete", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60, recordErr: errors.New("db down")}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now")) // must not panic

		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the delete must not be aborted by a record failure)", ops.deleted)
		}
		if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
			t.Errorf("added = %v, want [zswiftf|llm] (the learn must not be aborted)", store.added)
		}
		if len(store.decisions) != 0 {
			t.Errorf("decisions = %d rows, want 0 (the insert failed)", len(store.decisions))
		}
	})
	t.Run("pre-gate arms write no row", func(t *testing.T) {
		// Feature off / not a guild / author not gated: the defer is never
		// reached, so no decision row.
		for _, tc := range []struct {
			name  string
			store *fakeStore
			msg   *discordgo.Message
		}{
			{"feature off", &fakeStore{enabled: map[string]bool{FeatureKey: false}}, derpMsg("x")},
			{"not a guild", &fakeStore{enabled: map[string]bool{FeatureKey: true}}, func() *discordgo.Message { m := derpMsg("x"); m.GuildID = ""; return m }()},
			{"author not gated", &fakeStore{enabled: map[string]bool{FeatureKey: true}}, otherMsg("x")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newTestDerpies(tc.store, &fakeOps{}, &fakePi{})
				h.flow(tc.msg)
				if len(tc.store.decisions) != 0 {
					t.Errorf("decisions = %d rows, want 0 (pre-gate arm)", len(tc.store.decisions))
				}
			})
		}
	})
}

func TestFlowDeleteFailsAfterSuccessfulAdd(t *testing.T) {
	// A delete failure is LOG ONLY — the word was actually used and stays
	// learned (the next occurrence is a fast hit).
	pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
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

// ---------------------------------------------------------------------------
// The decision matrix (the score model)
// ---------------------------------------------------------------------------

// TestFlowDecisionMatrix — the matrix is defined for T > learnFloor: learn
// at score >= 40, delete at score >= T, INDEPENDENTLY; the word gate only
// bounds the learn (a rejected word never blocks a score-driven delete).
func TestFlowDecisionMatrix(t *testing.T) {
	t.Run("score < 40: no learn, no delete (the do-nothing band)", func(t *testing.T) {
		// The word is valid + anchored — the score alone keeps it out.
		pi := &fakePi{resp: "SCORE:10\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		assertNoDeletes(t, ops)
		assertNothingLearned(t, store)
	})
	t.Run("40 <= score < T: learn, no delete", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:45\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
			t.Errorf("added = %v, want [zswiftf|llm] (the learn band)", store.added)
		}
		assertNoDeletes(t, ops)
	})
	t.Run("score >= T: learn + delete", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95\nWORD:zswiftf"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
			t.Errorf("added = %v, want [zswiftf|llm]", store.added)
		}
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]]", ops.deleted)
		}
	})
	t.Run("score >= T with no word: delete, no learn", func(t *testing.T) {
		// No WORD line: the learn gate has no word to gate on, the delete
		// is pure score.
		pi := &fakePi{resp: "SCORE:95"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("holler at zswiftf now"))

		assertNothingLearned(t, store)
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the delete is pure score)", ops.deleted)
		}
	})
	t.Run("all-digit word rejected: delete still reflects score >= T", func(t *testing.T) {
		// The word gate rejects (no learn), but the delete is independent —
		// a rejected word never blocks a score-driven delete.
		pi := &fakePi{resp: "SCORE:95\nWORD:12345"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("12345"))

		assertNothingLearned(t, store)
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (delete-despite-rejection)", ops.deleted)
		}
	})
	t.Run("invalid word rejected: delete still reflects score >= T", func(t *testing.T) {
		// "s-w1ft" is neither wordValid (the dash) nor a unicode shape.
		pi := &fakePi{resp: "SCORE:95\nWORD:s-w1ft"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("s-w1ft here"))

		assertNothingLearned(t, store)
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (delete-despite-rejection)", ops.deleted)
		}
	})
	t.Run("unanchored word rejected: delete still reflects score >= T", func(t *testing.T) {
		// "xyzzy" is a valid word but NOT a token of the message: the
		// anchor gate rejects the learn, the delete still fires on score.
		pi := &fakePi{resp: "SCORE:95\nWORD:xyzzy"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("completely clean text"))

		assertNothingLearned(t, store)
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (delete-despite-rejection)", ops.deleted)
		}
	})
}

// TestFlowAnchorGateEmoji — the emoji fix (A): the anchor requirement is
// scoped to the POSTED message's word-like tokens. A "mention + emoji"
// post (a mention snowflake is all-digit, an emoji-ref has a colon, a
// pure-emoji token folds to "") has NO word-like tokens, so the anchor
// requirement drops: a valid ASCII word passes wordValid alone and the
// score decides (the observed case: 💸 + 🚵 = "buy me a bike" = zwift).
func TestFlowAnchorGateEmoji(t *testing.T) {
	t.Run("mention + emoji: no word-like tokens, the word passes, the score decides", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:95\nWORD:zwift"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("<@272889785318768641> 💸 <:derpies:1021692390177775657> 🚵"))

		if len(store.added) != 1 || store.added[0] != "zwift|llm" {
			t.Errorf("added = %v, want [zwift|llm] (a word-less post's valid ASCII word passes wordValid alone)", store.added)
		}
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the score decides)", ops.deleted)
		}
	})
	t.Run("word-like text tokens + a word not in the message: rejected", func(t *testing.T) {
		// The posted message HAS word-like tokens, so the anchor
		// requirement applies: "xyzzy" is not a folded token of the
		// message and the learn is rejected (the delete still fires on
		// score — delete-despite-rejection is pinned in
		// TestFlowDecisionMatrix).
		pi := &fakePi{resp: "SCORE:95\nWORD:xyzzy"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, threshold: 60}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("completely clean text"))

		assertNothingLearned(t, store)
		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the delete is independent of the word rejection)", ops.deleted)
		}
	})
}

// TestFlowPhraseFastMatch — the phrase fast path (B): a stored phrase
// matched as an EXACT consecutive run of the posted (or referenced)
// content's folded tokens -> fast delete, zero pi asks. A SPLIT word
// inside the phrase is NOT fast-matched (v1 is exact-consecutive only —
// it falls to the slow path); a phrase spanning the message/reply
// boundary is NOT matched (the sequences are scanned separately, never
// concatenated).
func TestFlowPhraseFastMatch(t *testing.T) {
	t.Run("exact consecutive run: fast delete, zero asks, the decision row", func(t *testing.T) {
		pi := &fakePi{}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			phrases: []string{"buy me a bike"},
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("who wants to buy me a bike"))

		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want [[c1 msg1]] (the phrase fast path)", ops.deleted)
		}
		if pi.asks != 0 || pi.imageAsks != 0 {
			t.Errorf("pi.asks = %d, pi.imageAsks = %d, want both 0 (zero asks)", pi.asks, pi.imageAsks)
		}
		// The decision row: path=fast, word=the phrase, deleted.
		if len(store.decisions) != 1 {
			t.Fatalf("decisions = %d rows, want exactly 1: %+v", len(store.decisions), store.decisions)
		}
		d := store.decisions[0]
		if d.Path == nil || *d.Path != "fast" {
			t.Errorf("path = %v, want fast", d.Path)
		}
		if d.Word == nil || *d.Word != "buy me a bike" {
			t.Errorf("word = %v, want buy me a bike", d.Word)
		}
		if !d.Deleted {
			t.Errorf("deleted = false, want true")
		}
		if d.RejectReason != nil {
			t.Errorf("reject_reason = %v, want nil", *d.RejectReason)
		}
	})
	t.Run("curation casing: the folded phrase matches", func(t *testing.T) {
		// Curation is SQL-only, so operator casing is a real case: the
		// curated phrase is folded (FoldToASCII + edge trim) before the
		// window match, so "Buy me a bike" matches the folded "buy".
		pi := &fakePi{}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			phrases: []string{"Buy me a bike"},
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("who wants to buy me a bike."))

		if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
			t.Errorf("deleted = %v, want one (the curated casing folds; the trailing . trims)", ops.deleted)
		}
		if pi.asks != 0 {
			t.Errorf("pi.asks = %d, want 0", pi.asks)
		}
	})
	t.Run("a SPLIT word inside the phrase: NOT fast-matched, falls to the slow path", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			phrases: []string{"buy me a bike"},
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("who wants to buy me a b i k e"))

		if len(ops.deleted) != 0 {
			t.Errorf("deleted = %v, want none (a split word is not exact-consecutive)", ops.deleted)
		}
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (fell to the slow path)", pi.asks)
		}
	})
	t.Run("a phrase spanning the message/reply boundary: NOT matched", func(t *testing.T) {
		// The posted + referenced sequences are scanned SEPARATELY, never
		// concatenated: a phrase that would match across the boundary
		// must not fast-hit.
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			phrases: []string{"to buy me a bike"},
		}
		ops := &fakeOps{ref: &discordgo.Message{Content: "me a bike"}}
		h := newTestDerpies(store, ops, pi)
		h.flow(msgWithRef("who wants to buy"))

		if len(ops.deleted) != 0 {
			t.Errorf("deleted = %v, want none (the phrase spans the message/reply boundary)", ops.deleted)
		}
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (fell to the slow path)", pi.asks)
		}
	})
	t.Run("a pure-punctuation stored phrase: folded to nothing, skipped (no match, no panic)", func(t *testing.T) {
		// A stored phrase with no non-empty tokens (e.g. pure
		// punctuation) folds to an empty sequence: it is skipped, never
		// matches, and must not panic the window match.
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			phrases: []string{"!!!"},
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("!!! who wants to buy me a bike"))

		if len(ops.deleted) != 0 {
			t.Errorf("deleted = %v, want none (the phrase folds to no tokens)", ops.deleted)
		}
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (fell to the slow path)", pi.asks)
		}
	})
	t.Run("a phrases fetch failure: degrade to the slow path, never act on a half-loaded list", func(t *testing.T) {
		// The post WOULD fast-match the phrase if the list loaded —
		// the fetch failure must degrade to the slow path (log +
		// continue), never act on a half-loaded phrase list.
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled:    map[string]bool{FeatureKey: true},
			phrasesErr: errors.New("db down"),
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("who wants to buy me a bike"))

		if len(ops.deleted) != 0 {
			t.Errorf("deleted = %v, want none (the phrase list never loaded)", ops.deleted)
		}
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (degraded to the slow path)", pi.asks)
		}
	})
}

// TestFlowPromptPhraseBlock — the prompt half of the phrase optimizer: the
// emitted prompt carries the phrases sub-block when phrases exist, and
// omits it on a phrases fetch failure (the degrade path never prompts
// from a half-loaded list).
func TestFlowPromptPhraseBlock(t *testing.T) {
	const phrHeader = "-----< known gimmick phrases (exact multi-word patterns) >-----"
	t.Run("phrases exist: the prompt carries the phrases sub-block", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled: map[string]bool{FeatureKey: true},
			words:   map[string]bool{"sw1ft": true},
			phrases: []string{"buy me a bike"},
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		content := "completely clean text"
		h.flow(derpMsg(content))

		if len(pi.prompts) != 1 {
			t.Fatalf("prompts = %v, want exactly one", pi.prompts)
		}
		want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "", store.phrases)
		if pi.prompts[0] != want {
			t.Errorf("prompt = %q, want %q", pi.prompts[0], want)
		}
		if !strings.Contains(pi.prompts[0], phrHeader) || !strings.Contains(pi.prompts[0], "buy me a bike") {
			t.Errorf("prompt must carry the phrases sub-block:\n%q", pi.prompts[0])
		}
	})
	t.Run("phrases fetch failed: the prompt omits the phrases sub-block", func(t *testing.T) {
		pi := &fakePi{resp: "SCORE:10"}
		store := &fakeStore{
			enabled:    map[string]bool{FeatureKey: true},
			words:      map[string]bool{"sw1ft": true},
			phrasesErr: errors.New("db down"),
		}
		ops := &fakeOps{}
		h := newTestDerpies(store, ops, pi)
		h.flow(derpMsg("completely clean text"))

		if len(pi.prompts) != 1 {
			t.Fatalf("prompts = %v, want exactly one (the degrade path still asks)", pi.prompts)
		}
		if strings.Contains(pi.prompts[0], phrHeader) {
			t.Errorf("phrases fetch failure must omit the phrases sub-block:\n%q", pi.prompts[0])
		}
	})
}
