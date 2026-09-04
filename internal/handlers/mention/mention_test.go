package mention

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/config"
	"github.com/danielcherubini/tugbot/internal/db"
	core "github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// ---------------------------------------------------------------------------
// formatRemaining — ports the Rust unit tests (mention.rs tests module) 1:1,
// plus the 0s boundary.
// ---------------------------------------------------------------------------

func TestFormatRemaining(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{time.Duration(0) * time.Second, "0s"},
		{time.Duration(1) * time.Second, "1s"},
		{time.Duration(45) * time.Second, "45s"},
		{time.Duration(59) * time.Second, "59s"},
		{time.Duration(60) * time.Second, "1m"},
		{time.Duration(3599) * time.Second, "59m"},
		{time.Duration(3600) * time.Second, "1h"},
		{time.Duration(3660) * time.Second, "1h 1m"},
		{time.Duration(7200) * time.Second, "2h"},
		{time.Duration(7260) * time.Second, "2h 1m"},
	}
	for _, tt := range tests {
		if got := formatRemaining(tt.d); got != tt.want {
			t.Errorf("formatRemaining(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// cooldownDecision — Rust step 8: elapsed = now - last_used (integer
// seconds, clamped at 0 under clock skew like Rust's unwrap_or_default);
// blocking when elapsed < limit, remaining = limit - elapsed.
// ---------------------------------------------------------------------------

func TestCooldownDecision(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		lastUse      time.Time
		limit        time.Duration
		wantBlock    bool
		wantLeftSecs int64
	}{
		{"fresh: 90s ago, 5m limit", now.Add(-90 * time.Second), 5 * time.Minute, true, 210},
		{"edge: exactly 300s ago", now.Add(-300 * time.Second), 5 * time.Minute, false, 0},
		{"just short of limit", now.Add(-299 * time.Second), 5 * time.Minute, true, 1},
		{"slow near end", now.Add(-7199 * time.Second), 2 * time.Hour, true, 1},
		{"slow at limit", now.Add(-7200 * time.Second), 2 * time.Hour, false, 0},
		{"clock skew: future lastUse clamps to 0 elapsed", now.Add(10 * time.Second), 5 * time.Minute, true, 300},
		{"far in the past", now.Add(-72 * time.Hour), 2 * time.Hour, false, 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			blocking, remaining := cooldownDecision(tt.lastUse, now, tt.limit)
			if blocking != tt.wantBlock {
				t.Errorf("blocking = %v, want %v", blocking, tt.wantBlock)
			}
			if int64(remaining/time.Second) != tt.wantLeftSecs {
				t.Errorf("remaining = %v, want %ds", remaining, tt.wantLeftSecs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bot mention check — via the API-provided mention list, never content.
// ---------------------------------------------------------------------------

func TestMentionsBotViaAPIMentionList(t *testing.T) {
	t.Run("bot in mentions list, not in content -> true (no content scraping)", func(t *testing.T) {
		m := &discordgo.Message{
			Author:   &discordgo.User{ID: "2000"},
			Content:  "hello",
			Mentions: []*discordgo.User{{ID: "1000"}},
		}
		if !mentionsBot(m, "1000") {
			t.Error("mentionsBot = false, want true (API list contains the bot)")
		}
	})
	t.Run("bot ID in content but NOT in the mentions list -> false", func(t *testing.T) {
		m := &discordgo.Message{
			Author:  &discordgo.User{ID: "2000"},
			Content: "<@1000> pretend this is a mention", // API did not resolve a mention
		}
		if mentionsBot(m, "1000") {
			t.Error("mentionsBot = true, want false (content scraping must not happen)")
		}
	})
	t.Run("nil mentions -> false", func(t *testing.T) {
		m := &discordgo.Message{Author: &discordgo.User{ID: "2000"}, Content: "hi"}
		if mentionsBot(m, "1000") {
			t.Error("mentionsBot = true on zero Mentions, want false")
		}
	})
	t.Run("other bots only -> false", func(t *testing.T) {
		m := &discordgo.Message{
			Author:   &discordgo.User{ID: "2000"},
			Content:  "hi",
			Mentions: []*discordgo.User{{ID: "999"}, {}},
		}
		if mentionsBot(m, "1000") {
			t.Error("mentionsBot = true without the bot in the list, want false")
		}
	})
}

// ---------------------------------------------------------------------------
// extractQuestion — Rust's tokenization (split_whitespace, trim "<@", trim
// "!", trim ">", drop tokens equal to the bot ID, join, trim).
// ---------------------------------------------------------------------------

func TestExtractQuestion(t *testing.T) {
	tests := []struct {
		content string
		want    string
	}{
		{"is this real?", "is this real?"},
		{"<@1000> is this real?", "is this real?"},
		{"<@!1000> hello there", "hello there"},
		{"<@1000> hello <@1000> world", "hello world"},
		{"<@1000>", ""},
		{"  <@1000>   ", ""},
		{"<@456> who is this?", "<@456> who is this?"},
		{"", ""},
		{"   \t  ", ""},
		{"<@999> what is this?", "<@999> what is this?"},
	}
	for _, tt := range tests {
		if got := extractQuestion(tt.content, "1000"); got != tt.want {
			t.Errorf("extractQuestion(%q) = %q, want %q", tt.content, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// mimeForURL — port of the eight Rust mime_for_url tests, 1:1.
// ---------------------------------------------------------------------------

func TestMimeForURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://cdn.discordapp.com/foo.png", "image/png"},
		{"https://cdn.discordapp.com/foo.png?v=2&hm=abc", "image/png"},
		{"https://cdn.discordapp.com/foo.png#section", "image/png"},
		{"https://example.com/a/b/c.gif", "image/gif"},
		{"https://example.com/img.webp", "image/webp"},
		{"https://example.com/photo.jpg", "image/jpeg"},
		{"https://example.com/photo", "image/jpeg"},
		{"https://example.com/photo.bmp", "image/jpeg"},
		{"https://example.com/photo.PNG", "image/jpeg"}, // Rust does NOT lowercase
		{"https://example.com/file.tar.gz", "image/jpeg"},
		{"https://example.com/.", "image/jpeg"}, // trailing dot: no extension in Rust either
	}
	for _, tt := range tests {
		if got := mimeForURL(tt.url); got != tt.want {
			t.Errorf("mimeForURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestIsSafeURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://example.com/x.png", true},
		{"http://example.com/x.png", true},
		{"ftp://example.com/x.png", false},
		{"file:///etc/passwd", false},
		{"/local/path.png", false},
		{"", false},
		{"httpx://example.com", false},
	}
	for _, tt := range tests {
		if got := isSafeURL(tt.url); got != tt.want {
			t.Errorf("isSafeURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// imageURLPlan — port of the two loops in Rust step 10 in deterministic
// order: attachment-vs-embed MIME sourcing + dedupe + safe-URL filter.
// ---------------------------------------------------------------------------

func TestImageURLPlan(t *testing.T) {
	imgA1 := "https://x/attached.png"
	imgA2 := "https://x/doc.pdf" // non-image attachment
	imgA3 := "ftp://x/bad.png"   // unsafe URL (image content type)
	imgA4 := "https://x/no-type" // empty content type

	ref := &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{URL: imgA1, ContentType: "image/png"},
			{URL: imgA2, ContentType: "application/pdf"},
			{URL: imgA3, ContentType: "image/png"},
			{URL: imgA4, ContentType: ""},
		},
		Embeds: []*discordgo.MessageEmbed{
			// Deduplicated against the attachment URL.
			{Image: &discordgo.MessageEmbedImage{URL: imgA1}},
			// Extension MIME from the URL (query + fragment stripped).
			{Image: &discordgo.MessageEmbedImage{URL: "https://y/embed.gif?v=1#frag"}},
			// Falls back to the thumbnail.
			{Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://y/thumb.jpg"}},
			// Deduplicated against a NON-image attachment URL (Rust dedupes
			// against ALL attachment URLs, not only the image ones).
			{Image: &discordgo.MessageEmbedImage{URL: imgA2}},
			// Unsafe URL is skipped.
			{Image: &discordgo.MessageEmbedImage{URL: "ftp://y/a.gif"}},
			// No image or thumbnail -> skipped.
			{Title: "no image"},
		},
	}

	plan := imageURLPlan(ref)

	want := []imagePlanEntry{
		{url: imgA1, mime: "image/png", source: "attachment"},
		{url: "https://y/embed.gif?v=1#frag", mime: "image/gif", source: "embed"},
		{url: "https://y/thumb.jpg", mime: "image/jpeg", source: "embed"},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan length = %d, want %d:\n%+v", len(plan), len(want), plan)
	}
	for i := range want {
		if plan[i] != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, plan[i], want[i])
		}
	}

	// A nil referenced message produces an empty plan (not a nil-panic).
	if got := imageURLPlan(nil); len(got) != 0 {
		t.Errorf("imageURLPlan(nil) = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// downloadImages — the download leg (shared client with an explicit 10s
// timeout), exercising both the attachment leg and the embed leg against a
// local httptest server.
// ---------------------------------------------------------------------------

func TestDownloadImages(t *testing.T) {
	body := []byte{0x89, 0x50, 0x4e, 0x47}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ref := &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{URL: srv.URL + "/a.png", ContentType: "image/png"},
		},
		Embeds: []*discordgo.MessageEmbed{
			{Image: &discordgo.MessageEmbedImage{URL: srv.URL + "/e.gif?v=1"}},
		},
	}

	h := &Mention{}
	images := h.downloadImages(context.Background(), ref, &http.Client{Timeout: 10 * time.Second})

	if len(images) != 2 {
		t.Fatalf("images = %d, want 2: %+v", len(images), images)
	}
	if images[0].MimeType != "image/png" {
		t.Errorf("attachment mime = %q, want image/png", images[0].MimeType)
	}
	if images[1].MimeType != "image/gif" {
		t.Errorf("embed mime = %q, want image/gif", images[1].MimeType)
	}
	for i, img := range images {
		b, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil || len(b) != len(body) {
			t.Errorf("image[%d] did not decode to the served body: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// buildPrompt — the two branches are referenced-message present vs absent
// (the slow/normal distinction NEVER branches the prompt); byte-identical
// to the Rust templates, including the 4-way content x image matrix.
// ---------------------------------------------------------------------------

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name       string
		author     string
		refPresent bool
		refContent string
		imgCount   int
		question   string
		want       string
	}{
		{
			name:     "no referenced message",
			author:   "Alice",
			question: "is this real?",
			want:     `Alice asked: "is this real?"`,
		},
		{
			name:       "referenced: content + images",
			author:     "Bob",
			refPresent: true,
			refContent: "hello",
			imgCount:   2,
			question:   "what?",
			want:       `Bob replied to: "hello [also shared an image]" and asked: "what?"`,
		},
		{
			name:       "referenced: no content, images",
			author:     "Bob",
			refPresent: true,
			imgCount:   1,
			question:   "what?",
			want:       `Bob replied to: "[shared an image (1)]" and asked: "what?"`,
		},
		{
			name:       "referenced: content only",
			author:     "Bob",
			refPresent: true,
			refContent: "hello",
			question:   "what?",
			want:       `Bob replied to: "hello" and asked: "what?"`,
		},
		{
			name:       "referenced: no content, no images",
			author:     "Bob",
			refPresent: true,
			question:   "what?",
			want:       `Bob replied to: "[replied to an image]" and asked: "what?"`,
		},
	}
	for _, tt := range tests {
		if got := buildPrompt(tt.author, tt.refPresent, tt.refContent, tt.imgCount, tt.question); got != tt.want {
			t.Errorf("%s: buildPrompt() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cooldown-message MAPPING (Rust line 196): slow users get the "Easy
// there" variant with the mention; every other user gets "I'm still
// waking up". Testing both variants proves the mapping, not just the
// literals.
// ---------------------------------------------------------------------------

func TestCooldownMessageMapping(t *testing.T) {
	if got := cooldownMessage(true, "<@2000>", "1h 58m"); got != "Easy there, <@2000> — give it a rest for 1h 58m" {
		t.Errorf("slow cooldown message = %q, want the 'Easy there' variant with the mention", got)
	}
	if got := cooldownMessage(false, "<@2000>", "4m"); got != "I'm still waking up — try again in 4m" {
		t.Errorf("normal cooldown message = %q, want the 'I'm still waking up' variant", got)
	}
}

// ---------------------------------------------------------------------------
// Flow tests — the full 15-step flow with a fake PiBackend, a fake store
// (feature-flag + is_this_real usage), and a fake Discord ops layer.
// ---------------------------------------------------------------------------

const (
	testBotID = "100"
	testUser  = "2000"
)

func testMessage(channelID, content string) *discordgo.Message {
	m := &discordgo.Message{
		ID:        "3000",
		ChannelID: channelID,
		GuildID:   "9000",
		Author:    &discordgo.User{ID: testUser, Username: "Alice"},
		Content:   content,
		Mentions:  []*discordgo.User{{ID: testBotID}},
	}
	return m
}

// fakePi implements app.PiBackend and records every ask.
type fakePi struct {
	asks   int
	prompt string
	imgs   int
	resp   string
	err    error
}

func (f *fakePi) Ask(_ context.Context, prompt string) (string, error) {
	f.asks++
	f.prompt = prompt
	return f.resp, f.err
}

func (f *fakePi) AskWithImages(_ context.Context, prompt string, images []app.PiImage) (string, error) {
	f.asks++
	f.prompt = prompt
	f.imgs = len(images)
	return f.resp, f.err
}

func (f *fakePi) Stop() {}

// fakeStore mirrors the mentionStore DB surface.
type fakeStore struct {
	enabled      map[string]bool
	haveUsage    bool
	lastUsedAt   time.Time
	resetCalls   int
	resetUserIDs []int64
}

func (s *fakeStore) featureEnabled(_ context.Context, key string) bool {
	return s.enabled[key]
}

func (s *fakeStore) usageLastUsed(_ context.Context, _, _ int64) (time.Time, bool) {
	return s.lastUsedAt, s.haveUsage
}

func (s *fakeStore) usageReset(_ context.Context, userID, _ int64) error {
	s.resetCalls++
	s.resetUserIDs = append(s.resetUserIDs, userID)
	return nil
}

// fakeOps mirrors the discordOps seam; every call is recorded and succeeds
// unless the corresponding field says otherwise.
type fakeOps struct {
	sent         []string
	sentChannels []string // channel ID per plain channelMessageSend (parallel to sent)
	ref          *discordgo.Message
	refErr       error
}

func (o *fakeOps) reactionAdd(_, _, _ string) error { return nil }

func (o *fakeOps) reactionRemove(_, _, _ string) error { return nil }

func (o *fakeOps) channelMessageSend(channelID, content string) error {
	o.sent = append(o.sent, content)
	o.sentChannels = append(o.sentChannels, channelID)
	return nil
}

func (o *fakeOps) channelMessageReply(_, _, content string) error {
	o.sent = append(o.sent, "reply:"+content)
	return nil
}

func (o *fakeOps) channelMessageRetrieve(_, _ string) (*discordgo.Message, error) {
	return o.ref, o.refErr
}

// captureLog records slog messages for assertions.
type captureLog struct {
	lines []string
}

func (c *captureLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureLog) Handle(_ context.Context, r slog.Record) error {
	c.lines = append(c.lines, r.Message)
	return nil
}
func (c *captureLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLog) WithGroup(string) slog.Handler      { return c }
func (c *captureLog) has(substr string) bool {
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// newTestMention builds a Mention with a default discordgo Session (State
// carries the bot user, mirroring the ready event) and injectable seams.
func newTestMention(ops *fakeOps, store *fakeStore, pi app.PiBackend, slow, exempt bool, logs *captureLog) *Mention {
	d := &discordgo.Session{}
	// v0.29.0: State embeds Ready, which carries the current user.
	d.State = &discordgo.State{Ready: discordgo.Ready{User: &discordgo.User{ID: testBotID}}}
	a := &app.App{D: d, Cfg: &config.Config{}, Pi: pi}
	if slow {
		a.Cfg.SlowUserIDs = map[int64]struct{}{2000: {}}
	}
	if exempt {
		a.Cfg.CooldownExemptUserIDs = map[int64]struct{}{2000: {}}
	}
	if logs != nil {
		slog.SetDefault(slog.New(logs))
	}
	return &Mention{app: a, ops: ops, store: store}
}

// ---------------------------------------------------------------------------
// 1. Feature flag gate + channel restriction (Rust steps 1-4, in order).
// ---------------------------------------------------------------------------

func TestFlowFeatureFlagGate(t *testing.T) {
	pi := &fakePi{resp: "hello from pi"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: false}}
	ops := &fakeOps{}
	h := newTestMention(ops, store, pi, false, false, nil)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (feature flag disabled)", pi.asks)
	}
}

func TestFlowChannelRestriction(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true, SlowUserAutoGulagFeatureKey: true}}

	t.Run("a mention in another guild channel is ignored", func(t *testing.T) {
		pi := &fakePi{resp: "hello"}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, false, false, nil)
		h.flow(testMessage("999999", "<@"+testBotID+"> is this real?"))
		if pi.asks != 0 {
			t.Errorf("pi.asks = %d, want 0 (channel restriction must block)", pi.asks)
		}
	})

	t.Run("a bot mention in #ask-tugbot goes through", func(t *testing.T) {
		pi := &fakePi{resp: "helo"}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, false, false, nil)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1", pi.asks)
		}
	})

	t.Run("guild check: a DM (empty guild) is ignored", func(t *testing.T) {
		pi := &fakePi{resp: "hello"}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, false, false, nil)
		m := testMessage(AskTugbotChannelID, "<@"+testBotID+"> hi")
		m.GuildID = ""
		h.flow(m)
		if pi.asks != 0 {
			t.Errorf("pi.asks = %d, want 0 (no guild)", pi.asks)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Empty question -> plain (non-reply) "You mentioned me but didn't ask
//    anything — what's up?" and return before the pi ask.
// ---------------------------------------------------------------------------

func TestFlowEmptyQuestionSendsPlainMessage(t *testing.T) {
	pi := &fakePi{resp: "ignored"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestMention(ops, store, pi, false, false, nil)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+">"))

	want := "You mentioned me but didn't ask anything — what's up?"
	if len(ops.sent) != 1 || ops.sent[0] != want {
		t.Errorf("sent = %v, want [%q] and NOTHING else", ops.sent, want)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	for _, s := range ops.sent {
		if strings.HasPrefix(s, "reply:") {
			t.Errorf("empty-question message was sent as a reply: %v (Rust sends a PLAIN channel message)", ops.sent)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Cooldown check (Rust step 8): normal mapping, slow mapping, exempt
//    bypass, no-usage-row pass-through.
// ---------------------------------------------------------------------------

func TestFlowCooldownBlocksBeforePi(t *testing.T) {
	newMk := func(slow bool) (*Mention, *fakeOps, *fakePi) {
		pi := &fakePi{resp: "nope"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-100 * time.Second)}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, slow, false, nil)
		return h, ops, pi
	}

	t.Run("normal user: 'I'm still waking up' reply (5m mapping)", func(t *testing.T) {
		h, ops, pi := newMk(false)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))
		want := "reply:I'm still waking up — try again in 3m" // 300 - 100 = 200s = 3m
		if len(ops.sent) != 1 || ops.sent[0] != want {
			t.Errorf("sent = %v, want [%q] (mapping: normal user)", ops.sent, want)
		}
		if pi.asks != 0 {
			t.Errorf("pi.asks = %d, want 0 (blocked by cooldown)", pi.asks)
		}
	})

	t.Run("slow user: 'Easy there' reply (2h mapping)", func(t *testing.T) {
		h, ops, pi := newMk(true)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))
		want := "reply:Easy there, <@" + testUser + "> — give it a rest for 1h 58m" // 7200 - 100 = 7100s
		if len(ops.sent) != 1 || ops.sent[0] != want {
			t.Errorf("sent = %v, want [%q] (mapping: slow user)", ops.sent, want)
		}
		if pi.asks != 0 {
			t.Errorf("pi.asks = %d, want 0 (blocked by cooldown)", pi.asks)
		}
	})

	t.Run("exempt user bypasses the cooldown", func(t *testing.T) {
		pi := &fakePi{resp: "ok"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-100 * time.Second)}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, false, true, nil)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (exempt bypass)", pi.asks)
		}
	})

	t.Run("no usage row: no check, flows through", func(t *testing.T) {
		pi := &fakePi{resp: "ok"}
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		ops := &fakeOps{}
		h := newTestMention(ops, store, pi, false, false, nil)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))
		if pi.asks != 1 {
			t.Errorf("pi.asks = %d, want 1 (no usage row -> no cooldown gate)", pi.asks)
		}
	})
}

// ---------------------------------------------------------------------------
// 4. Nil-Pi path: log `pi RPC not available` and return SILENTLY (no error
//    reply, no cooldown write). The 🤔 reaction stays (it was added in
//    step 9 and is only removed on the ask-error / empty / posted paths).
// ---------------------------------------------------------------------------

func TestNilPiSilentReturn(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-time.Hour)}
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMention(ops, store, nil, false, false, logs)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))

	if !logs.has("pi RPC not available") {
		t.Errorf("logs: %s — want to contain \"pi RPC not available\"", strings.Join(logs.lines, ", "))
	}
	for _, s := range ops.sent {
		if strings.HasPrefix(s, "reply:") {
			t.Fatalf("nil Pi sent a reply: %v (must return silent BEFORE the error reply)", ops.sent)
		}
		if len(ops.sent) > 0 {
			t.Fatalf("nil Pi sent a message: %v (must return silent before any send)", ops.sent)
		}
	}
	if store.resetCalls != 0 {
		t.Errorf("store.resetCalls = %d, want 0 (no cooldown write)", store.resetCalls)
	}
}

// ---------------------------------------------------------------------------
// 5. Pi ask ERROR: 🤔 removed, the exact error reply sent, no cooldown
//    write.
// ---------------------------------------------------------------------------

func TestFlowPiAskError(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-time.Hour)}
	ops := &fakeOps{}
	want := "reply:" + `I'm having trouble thinking right now, try again later`
	pi := &fakePi{err: errors.New("boom")}
	h := newTestMention(ops, store, pi, false, false, nil)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))

	if len(ops.sent) != 1 || ops.sent[0] != want {
		t.Errorf("sent = %v, want [%q]", ops.sent, want)
	}
	if store.resetCalls != 0 {
		t.Errorf("resetCalls = %d, want 0 (ask error -> no cooldown write)", store.resetCalls)
	}
}

// ---------------------------------------------------------------------------
// 6. Whitespace-only pi response: trim THEN check empty. Skip post AND
//    skip the cooldown write; the 🤔 is removed on the empty branch.
// ---------------------------------------------------------------------------

func TestFlowEmptyResponseSkipsPostAndCooldown(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-time.Hour)}
	ops := &fakeOps{}
	logs := &captureLog{}
	pi := &fakePi{resp: "   \n\t  "}
	h := newTestMention(ops, store, pi, false, false, logs)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))

	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none (whitespace-only is empty after trim: no post, no error reply)", ops.sent)
	}
	if store.resetCalls != 0 {
		t.Errorf("resetCalls = %d, want 0 (cooldown write-back skipped)", store.resetCalls)
	}
	if !logs.has("skipping post and cooldown") {
		t.Errorf("logs: %s — want the empty-response log line", strings.Join(logs.lines, ", "))
	}

	// Control: a non-empty response posts AND writes the cooldown back
	// (posted && !exempt) — so the skip above is meaningful.
	store2 := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-time.Hour)}
	ops3 := &fakeOps{}
	pi3 := &fakePi{resp: "  real answer  "}
	h3 := newTestMention(ops3, store2, pi3, false, false, nil)
	h3.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))
	if len(ops3.sent) != 1 || ops3.sent[0] != "reply:real answer" {
		t.Errorf("control sent = %v, want ['reply:real answer'] (trimmed before post, sent as a reply)", ops3.sent)
	}
	if store2.resetCalls != 1 {
		t.Errorf("control resetCalls = %d, want 1", store2.resetCalls)
	}
}

// ---------------------------------------------------------------------------
// 7. Exempt users never get the cooldown write-back, even when posted.
// ---------------------------------------------------------------------------

func TestFlowExemptNoCooldownWrite(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, haveUsage: true, lastUsedAt: time.Now().Add(-time.Hour)}
	ops := &fakeOps{}
	pi := &fakePi{resp: "ok"}
	h := newTestMention(ops, store, pi, false, true /* exempt */, nil)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> q"))

	if store.resetCalls != 0 {
		t.Errorf("resetCalls = %d, want 0 (exempt users skip the cooldown write-back)", store.resetCalls)
	}
}

// ---------------------------------------------------------------------------
// 8. Referenced-message fetch (Rust step 7): failure -> treat as None, the
//    flow CONTINUES (pi is asked, the prompt uses the absent branch);
//    present -> 4-way-matrix branch.
// ---------------------------------------------------------------------------

func TestFlowReferencedMessageFetchFailureContinues(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{refErr: errors.New("404: no such message")}
	pi := &fakePi{resp: "ok"}
	h := newTestMention(ops, store, pi, false, false, nil)
	m := testMessage(AskTugbotChannelID, "<@"+testBotID+"> q")
	// Reply: Rust's `msg.message_reference.as_ref().and_then(|r| r.message_id)`.
	m.MessageReference = &discordgo.MessageReference{MessageID: "3001"}
	h.flow(m)

	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (fetch failure must not abort the flow)", pi.asks)
	}
	if pi.prompt != `Alice asked: "q"` {
		t.Errorf("prompt = %q, want the absent-reference template", pi.prompt)
	}
}

func TestFlowReferencedMessagePresent(t *testing.T) {
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{ID: "3001", Content: "previous question"}}
	pi := &fakePi{resp: "ok"}
	h := newTestMention(ops, store, pi, false, false, nil)
	m := testMessage(AskTugbotChannelID, "<@"+testBotID+"> q")
	m.MessageReference = &discordgo.MessageReference{MessageID: "3001"}
	h.flow(m)

	// 4-way matrix, content-only arm: non-empty content, zero images.
	want := `Alice replied to: "previous question" and asked: "q"`
	if pi.prompt != want {
		t.Errorf("prompt = %q, want %q (referenced-message branch, content-only arm)", pi.prompt, want)
	}
	if pi.imgs != 0 {
		t.Errorf("pi.imgs = %d, want 0 (no attachments or embeds on the referenced message)", pi.imgs)
	}
}

// ---------------------------------------------------------------------------
// Step 5 — slow-user auto-gulag (Rust's handle_slow_user_auto_gulag,
// mention.rs:412-473), executed through the injected *core.Gulag
// (core.NewWithSeams: fake server lookup, fake Discord surface, fake DB
// executor — the real AddToGulag logic runs; a Discord failure is the
// "AddToGulag failure" arm).
//
// The channel message is pinned byte-for-byte against the Rust format!
// (mention.rs:455-460): "real... now" (three ASCII dots), "they're"
// (ASCII apostrophe, 0x27), "for 5m. Irony." (dot inside "5m."). Every
// arm's log text is pinned against the Rust eprintln text (the "[mention]"
// prefix is the Rust log category; the Go port carries the module key
// instead — pinned in the code comments).
// ---------------------------------------------------------------------------

const (
	// testGulagChannelID — the fake "the-gulag" channel.
	testGulagChannelID = "4000"
	// testGulagRoleID — the fake server's gulag role (server.GulagID).
	testGulagRoleID = 123
)

// fakeCoreDiscord implements core.DiscordSurface for the auto-gulag
// tests (find-channel scan + add-to-gulag member/role lookups).
type fakeCoreDiscord struct {
	channels     []*discordgo.Channel
	channelsErr  error
	channelCalls int
	memberErr    error
	memberCalls  int
	roleAdds     int
	roleAddErr   error
	roleAddGuild string
	roleAddUser  string
	roleAddRole  string
}

func (f *fakeCoreDiscord) GuildChannels(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	f.channelCalls++
	return f.channels, f.channelsErr
}

func (f *fakeCoreDiscord) GuildRoles(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	return nil, nil
}

func (f *fakeCoreDiscord) GuildMember(_, _ string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.memberCalls++
	return &discordgo.Member{}, f.memberErr
}

func (f *fakeCoreDiscord) GuildMemberRoleAdd(guildID, userID, roleID string, _ ...discordgo.RequestOption) error {
	f.roleAdds++
	f.roleAddGuild, f.roleAddUser, f.roleAddRole = guildID, userID, roleID
	return f.roleAddErr
}

// fakeCoreDB implements core.QueryExec: the gulag_users SELECT (the
// IsUserInGulag lookup) reports a missing row (so the fresh send_to_gulag
// branch runs); the INSERT ... RETURNING id fills id; the UPDATE animates
// successfully.
type fakeCoreDB struct {
	selects int
	inserts int
	updates int
}

type fakeCoreRow struct {
	err error
	id  *int32
}

func (r *fakeCoreRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.id != nil {
		*r.id = 1
	}
	return nil
}

func (f *fakeCoreDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM gulag_users"):
		f.selects++
		return &fakeCoreRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "RETURNING id"):
		f.inserts++
		return &fakeCoreRow{id: new(int32)}
	default:
		return &fakeCoreRow{}
	}
}

func (f *fakeCoreDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE ") {
		f.updates++
	}
	return pgconn.CommandTag{}, nil
}

// testGulagHarness — a working *core.Gulag over the fake seams, plus
// access to the fakes for assertions.
type testGulagHarness struct {
	core    *core.Gulag
	discord *fakeCoreDiscord
	db      *fakeCoreDB
}

func newTestGulagHarness(server *db.Server, lookupErr error) *testGulagHarness {
	d := &fakeCoreDiscord{
		channels: []*discordgo.Channel{{ID: testGulagChannelID, Name: core.GulagChannelName}},
	}
	dbExec := &fakeCoreDB{}
	return &testGulagHarness{
		core: core.NewWithSeams(d, dbExec,
			func(_ context.Context, _ int64) (*db.Server, bool, error) {
				if lookupErr != nil {
					return nil, false, lookupErr
				}
				if server == nil {
					return nil, false, nil // the no-row arm (Rust's None)
				}
				return server, true, nil
			}),
		discord: d,
		db:      dbExec,
	}
}

// newTestMentionWithCore — newTestMention plus the I2 core injection (the
// step-5 auto-gulag path needs a working *core.Gulag that runs; the
// existing tests pass nil and never enter that path).
func newTestMentionWithCore(ops *fakeOps, store *fakeStore, pi app.PiBackend, slow, exempt bool, logs *captureLog, core *core.Gulag) *Mention {
	h := newTestMention(ops, store, pi, slow, exempt, logs)
	h.core = core
	return h
}

// slowGulagStore — step-1 gate + the step-5 auto-gulag flag, both on.
func slowGulagStore() *fakeStore {
	return &fakeStore{enabled: map[string]bool{FeatureKey: true, SlowUserAutoGulagFeatureKey: true}}
}

// pinGulagSideEffectsNeverFired — asserts the auto-gulag path never
// reached the core (no member/role lookups, no DB rows).
func pinGulagSideEffectsNeverFired(t *testing.T, harness *testGulagHarness) {
	t.Helper()
	if harness.discord.memberCalls != 0 {
		t.Errorf("core member lookups = %d, want 0 (no AddToGulag)", harness.discord.memberCalls)
	}
	if harness.discord.roleAdds != 0 {
		t.Errorf("core role-add calls = %d, want 0 (no AddToGulag)", harness.discord.roleAdds)
	}
	if harness.db.inserts != 0 {
		t.Errorf("gulag_users inserts = %d, want 0 (no AddToGulag)", harness.db.inserts)
	}
}

func assertNoGulagMessage(t *testing.T, ops *fakeOps) {
	t.Helper()
	for _, s := range ops.sent {
		if strings.Contains(s, "in the gulag for 5m. Irony.") {
			t.Fatalf("sent the auto-gulag message when it must not have: %v", ops.sent)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. No server config (missing row and DB error — Rust's single None arm)
//    -> log + return: no AddToGulag, no channel message.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagNoServerConfig(t *testing.T) {
	run := func(harness *testGulagHarness, label string) func(*testing.T) {
		return func(t *testing.T) {
			store := slowGulagStore()
			ops := &fakeOps{}
			logs := &captureLog{}
			h := newTestMentionWithCore(ops, store, nil, true /* slow */, false, logs, harness.core)
			h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

			want := "No server config for guild 9000 (or DB unavailable)" // Rust eprintln text
			if !logs.has(want) {
				t.Errorf("%s: logs: %s — want to contain %q", label, strings.Join(logs.lines, ", "), want)
			}
			if len(ops.sent) != 0 {
				t.Errorf("%s: sent = %v, want none (no channel message)", label, ops.sent)
			}
			pinGulagSideEffectsNeverFired(t, harness)
		}
	}
	t.Run("no row for the guild", run(newTestGulagHarness(nil, nil), "no-row"))
	t.Run("DB lookup error (Rust's None arm covers both)", run(
		newTestGulagHarness(nil, errors.New("db down")), "db-error"))
}

// ---------------------------------------------------------------------------
// 2. GulagID < 0 — the Go home of Rust's u64::try_from(server.gulag_id)
//    error arm (the column is int64: only negative IDs overflow a u64).
//    Rust checks this AFTER the channel lookup (during params construction),
//    so with a present channel (FindChannel -> Some) the channel lookup RUNS
//    (exactly once) BEFORE the overflow arm returns — assert channelCalls == 1.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagNegativeGulagIDOverflow(t *testing.T) {
	server := &db.Server{ID: 1, GuildID: 9000, GulagID: -5}
	harness := newTestGulagHarness(server, nil) // working "the-gulag" channel (FindChannel -> Some)
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), nil, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	if !logs.has("gulag role ID -5 overflows u64") { // Rust eprintln text
		t.Errorf("logs: %s — want to contain the overflow-arm text", strings.Join(logs.lines, ", "))
	}
	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none", ops.sent)
	}
	if harness.discord.channelCalls != 1 {
		t.Errorf("FindChannel calls = %d, want 1 (the channel lookup runs BEFORE the overflow arm returns)", harness.discord.channelCalls)
	}
	pinGulagSideEffectsNeverFired(t, harness)
}

// ---------------------------------------------------------------------------
// 2b. COMBINED FAILURE (mirrors Rust's channel-arm-wins): a server with a
//     negative GulagID AND a guild that has NO "the-gulag" channel. The
//     channel arm is checked BEFORE the role-ID overflow arm, so the
//     "No gulag channel found" log fires — NOT the overflow log.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagNegativeIDMissingChannelChannelArmWins(t *testing.T) {
	server := &db.Server{ID: 1, GuildID: 9000, GulagID: -5}
	harness := newTestGulagHarness(server, nil)
	harness.discord.channels = nil // the guild has no "the-gulag" channel (FindChannel -> None)
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), nil, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	// The channel arm is checked first -> "No gulag channel found" fires.
	if !logs.has("No gulag channel found") {
		t.Errorf("logs: %s — want to contain the missing-channel text (the channel arm wins)", strings.Join(logs.lines, ", "))
	}
	// The overflow arm must NOT have fired (it is checked AFTER the channel).
	if logs.has("gulag role ID -5 overflows u64") {
		t.Errorf("logs: %s — must NOT contain the overflow-arm text (the channel arm is checked first)", strings.Join(logs.lines, ", "))
	}
	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none", ops.sent)
	}
	if harness.discord.channelCalls != 1 {
		t.Errorf("FindChannel calls = %d, want 1 (the channel lookup ran before the missing-channel return)", harness.discord.channelCalls)
	}
	pinGulagSideEffectsNeverFired(t, harness)
}

// ---------------------------------------------------------------------------
// 3. FindChannel("the-gulag") returns None -> log + return, no message.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagChannelNotFound(t *testing.T) {
	harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
	harness.discord.channels = nil // the guild has no "the-gulag" channel
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), nil, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	if !logs.has("No gulag channel found") {
		t.Errorf("logs: %s — want to contain the missing-channel text", strings.Join(logs.lines, ", "))
	}
	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none", ops.sent)
	}
	pinGulagSideEffectsNeverFired(t, harness)
}

// ---------------------------------------------------------------------------
// 4. FindChannel ERROR -> log + return, no message (the Discord-error arm,
//    propagated so the caller can distinguish "channel disappeared" from
//    "guild is gone").
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagFindChannelError(t *testing.T) {
	harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
	harness.discord.channelsErr = errors.New("channel 404: Unknown Channel")
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), nil, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	if !logs.has("Error looking up gulag channel:") {
		t.Errorf("logs: %s — want to contain the lookup-error text", strings.Join(logs.lines, ", "))
	}
	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none", ops.sent)
	}
	pinGulagSideEffectsNeverFired(t, harness)
}

// ---------------------------------------------------------------------------
// 5. AddToGulag FAILS (the real core: member lookup error) -> log +
//	    return; the "sent to the gulag" message is NOT posted.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagAddFailure(t *testing.T) {
	harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
	harness.discord.memberErr = errors.New("member lookup failed: 404") // AddToGulag's first Discord call
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), nil, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	if !logs.has("Failed to gulag slow user:") {
		t.Errorf("logs: %s — want to contain the add-failure text", strings.Join(logs.lines, ", "))
	}
	assertNoGulagMessage(t, ops)
	if len(ops.sent) != 0 {
		t.Errorf("sent = %v, want none (an add failure posts nothing)", ops.sent)
	}
	if harness.discord.roleAdds != 0 {
		t.Errorf("role-add calls = %d, want 0 (the member failure short-circuits AddToGulag)", harness.discord.roleAdds)
	}
	if harness.db.inserts != 0 {
		t.Errorf("gulag_users inserts = %d, want 0", harness.db.inserts)
	}
}

// ---------------------------------------------------------------------------
// 6. AddToGulag succeeds -> the BYTE-EXACT slowUserGulagMessage posted on
//    #the-gulag (the mention carries the <@ID>; Rust's format! verbatim).
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagSuccessPostsByteExactMessage(t *testing.T) {
	harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
	ops := &fakeOps{}
	logs := &captureLog{}
	h := newTestMentionWithCore(ops, slowGulagStore(), &fakePi{}, true, false, logs, harness.core)
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))

	// Rust's format!("{} wanted to know if something was real... now they're in the gulag for 5m. Irony.", msg.author.mention())
	want := "<@" + testUser + "> wanted to know if something was real... now they're in the gulag for 5m. Irony."
	if len(ops.sent) != 1 || ops.sent[0] != want {
		t.Fatalf("sent = %v, want [ %q ] (byte-for-byte Rust message)", ops.sent, want)
	}
	// Pinned: posted ON #the-gulag (the channel's ID), as a PLAIN channel
	// message (not a reply).
	if len(ops.sentChannels) != 1 || ops.sentChannels[0] != testGulagChannelID {
		t.Errorf("sentChannels = %v, want [%s] (#the-gulag)", ops.sentChannels, testGulagChannelID)
	}
	for _, s := range ops.sent {
		if strings.HasPrefix(s, "reply:") {
			t.Errorf("gulag message was sent as a reply: %v (Rust posts a PLAIN channel message)", ops.sent)
		}
	}
	// The real AddToGulag ran end-to-end: the guild member was fetched,
	// the role was added with the exact params, and the fresh row was
	// inserted.
	if harness.discord.memberCalls != 1 || harness.discord.roleAdds != 1 {
		t.Errorf("memberCalls/roleAdds = %d/%d, want 1/1 (the real AddToGulag ran)", harness.discord.memberCalls, harness.discord.roleAdds)
	}
	if harness.discord.roleAddGuild != "9000" || harness.discord.roleAddUser != testUser || harness.discord.roleAddRole == "" {
		t.Errorf("roleAdd args = (%q, %q, %q), want (9000, %s, <server gulag role id>)",
			harness.discord.roleAddGuild, harness.discord.roleAddUser, harness.discord.roleAddRole, testUser)
	}
	if harness.db.inserts != 1 {
		t.Errorf("gulag_users inserts = %d, want 1 (fresh send_to_gulag row)", harness.db.inserts)
	}
}

// ---------------------------------------------------------------------------
// 7. ORDER INVARIANT (Rust step order): the auto-gulag fires BEFORE
//    question extraction. Flag on + slow user + EMPTY question -> the
//    auto-gulag path runs and returns first: the gulag side effects
//    happen, the "didn't ask anything" message is NOT sent.
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagFiresBeforeEmptyQuestion(t *testing.T) {
	harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
	pi := &fakePi{resp: "should never be asked"}
	ops := &fakeOps{}
	h := newTestMentionWithCore(ops, slowGulagStore(), pi, true /* slow */, false, nil, harness.core)
	// An EMPTY question: only the bot mention (extractQuestion -> "").
	h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+">"))

	// The gulag side effects happened through the REAL core.
	if harness.db.inserts != 1 {
		t.Fatalf("gulag_users inserts = %d, want 1 (the auto-gulag must run BEFORE the empty-question return)", harness.db.inserts)
	}
	want := "<@" + testUser + "> wanted to know if something was real... now they're in the gulag for 5m. Irony."
	if len(ops.sent) != 1 || ops.sent[0] != want {
		t.Fatalf("sent = %v, want the byte-for-byte gulag message %q", ops.sent, want)
	}
	// The empty-question message must NOT have been sent (in either form).
	joined := strings.Join(ops.sent, "|")
	if strings.Contains(joined, emptyQuestionMessage) {
		t.Errorf("the empty-question message WAS sent: %v (step 5 must return BEFORE question extraction)", ops.sent)
	}
	if strings.Contains(joined, "reply:") {
		t.Errorf("a reply message was sent: %v (the auto-gulag path returns before any reply path)", ops.sent)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the flow returned at step 5)", pi.asks)
	}
}

// ---------------------------------------------------------------------------
// 8. The gate: feature flag OFF, or the user not in SLOW_USER_IDS ->
//    NO auto-gulag at all (the flow continues on the normal path).
// ---------------------------------------------------------------------------

func TestSlowUserAutoGulagFlagOffOrNotSlow(t *testing.T) {
	t.Run("flag off (user in SLOW_USER_IDS, slow_user_auto_gulag disabled)", func(t *testing.T) {
		harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
		// Only the base FeatureKey is enabled — the gulag flag is off.
		store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
		ops := &fakeOps{}
		h := newTestMentionWithCore(ops, store, nil /* pi: the flow returns on nil-pi, after step 5 */, true /* slow */, false, nil, harness.core)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))
		assertNoGulagMessage(t, ops)
		pinGulagSideEffectsNeverFired(t, harness)
	})
	t.Run("flag on, user NOT in SLOW_USER_IDS", func(t *testing.T) {
		harness := newTestGulagHarness(&db.Server{ID: 1, GuildID: 9000, GulagID: testGulagRoleID}, nil)
		ops := &fakeOps{}
		h := newTestMentionWithCore(ops, slowGulagStore(), nil /* pi */, false /* not slow */, false, nil, harness.core)
		h.flow(testMessage(AskTugbotChannelID, "<@"+testBotID+"> is this real?"))
		assertNoGulagMessage(t, ops)
		pinGulagSideEffectsNeverFired(t, harness)
	})
}
