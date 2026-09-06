// Package derpies — image + referenced-message tests. The image leg
// (imageURLPlan / downloadPlan / isSafeURL / mimeForURL) and the
// referenced-message flow (channelMessageRetrieve via the discordOps
// seam) mirror the mention package's proven mechanisms, self-contained.
// Image-download tests use a real net/http/httptest server + real
// discordgo.Message attachments (no new seam on the Derpies struct); the
// referenced fetch goes through the existing fakeOps seam.
package derpies

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newImgServer — a server answering 200 with a stable body; returns the
// server and the body bytes (for the base64 round-trip assertions).
func newImgServer(t *testing.T) (*httptest.Server, []byte) {
	body := []byte{0x89, 0x50, 0x4e, 0x47}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, body
}

// msgWithImage — a message from the filtered user carrying one image
// attachment at url (URL/ID/GuildID/ChannelID/Author as in derpMsg).
func msgWithImage(content, url string) *discordgo.Message {
	m := derpMsg(content)
	m.Attachments = []*discordgo.MessageAttachment{{URL: url, ContentType: "image/png"}}
	return m
}

// msgWithRef — a message from the filtered user carrying a
// MessageReference (the tests set ops.ref themselves — the builder only
// sets the reference pointer so the fake's fetch path triggers).
func msgWithRef(content string) *discordgo.Message {
	m := derpMsg(content)
	m.MessageReference = &discordgo.MessageReference{MessageID: "ref1"}
	return m
}

// ---------------------------------------------------------------------------
// isSafeURL / mimeForURL (pure image-leg helpers)
// ---------------------------------------------------------------------------

func TestIsSafeURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://dl.discordapp.com/attachments/x.png", true},
		{"http://dl.discordapp.com/attachments/x.png", true},
		{"ftp://dl.discordapp.com/attachments/x.png", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isSafeURL(tt.url); got != tt.want {
			t.Errorf("isSafeURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestMimeForURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://x/y.png?sig=1#frag", "image/png"}, // query + fragment stripped first
		{"https://x/a.webp", "image/webp"},
		{"https://x/g.gif", "image/gif"},
		{"https://x/photo", "image/jpeg"}, // no extension -> default
		{"https://x/x.jpg", "image/jpeg"}, // jpg is not in the map -> default
	}
	for _, tt := range tests {
		if got := mimeForURL(tt.url); got != tt.want {
			t.Errorf("mimeForURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// imageURLPlan — the plan leg: attachment image/* (content_type), embed
// image/thumbnail with extension MIME, deduped against the attachment
// urls, isSafeURL-guarded on every entry.
// ---------------------------------------------------------------------------

func TestImagePlan(t *testing.T) {
	imgPng := "https://x/attached.png"
	imgJpg := "https://x/photo.jpg"
	imgTxt := "https://x/a.txt"
	imgNoType := "https://x/no-type"
	imgFtp := "ftp://x/bad.png"

	m := &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{URL: imgPng, ContentType: "image/png"},
			{URL: imgJpg, ContentType: "image/jpeg"},
			{URL: imgTxt, ContentType: "text/plain"},
			{URL: imgNoType, ContentType: ""},
			{URL: imgFtp, ContentType: "image/png"},
		},
		Embeds: []*discordgo.MessageEmbed{
			// Extension MIME from the URL.
			{Image: &discordgo.MessageEmbedImage{URL: "https://y/embed.gif?v=1"}},
			// Empty Image.URL falls back to the thumbnail.
			{Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://y/thumb.webp"}},
			// Deduped against the attachment url (absent from the plan).
			{Image: &discordgo.MessageEmbedImage{URL: imgPng}},
		},
	}

	plan := imageURLPlan(m)

	want := []imagePlanEntry{
		{url: imgPng, mime: "image/png", source: "attachment"},
		{url: imgJpg, mime: "image/jpeg", source: "attachment"},
		{url: "https://y/embed.gif?v=1", mime: "image/gif", source: "embed"},
		{url: "https://y/thumb.webp", mime: "image/webp", source: "embed"},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan length = %d, want %d:\n%+v", len(plan), len(want), plan)
	}
	for i := range want {
		if plan[i] != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, plan[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// downloadImages — the download leg (shared client with an explicit 10s
// timeout) against a local httptest server, exercising the 500-body-still-
// read mention parity and the closed-server skip.
// ---------------------------------------------------------------------------

func TestDownloadImages(t *testing.T) {
	bodyA := []byte{0x89, 0x50, 0x4e, 0x47}
	bodyB := []byte{0xff, 0xd8, 0xff, 0xe0}
	bodyC := []byte{0x47, 0x49, 0x46, 0x38}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/a.png":
			_, _ = w.Write(bodyA)
		case r.URL.Path == "/b.png":
			_, _ = w.Write(bodyB)
		default:
			w.WriteHeader(500)
			_, _ = w.Write(bodyC)
		}
	}))
	t.Cleanup(srv.Close)

	// A server whose listener is already closed: connecting fails, the URL
	// is logged + skipped, the rest survive.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	closed.Close()

	m := &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{URL: srv.URL + "/a.png", ContentType: "image/png"},
			{URL: srv.URL + "/b.png", ContentType: "image/jpeg"},
			{URL: srv.URL + "/c.png", ContentType: "image/png"},
			{URL: closed.URL + "/d.png", ContentType: "image/png"},
		},
	}

	h := &Derpies{}
	images := h.downloadImages(context.Background(), m, &http.Client{Timeout: 10 * time.Second})

	if len(images) != 3 {
		t.Fatalf("images = %d, want 3 (the closed-server download must be skipped): %+v", len(images), images)
	}
	wantMime := []string{"image/png", "image/jpeg", "image/png"}
	want := [][]byte{bodyA, bodyB, bodyC}
	for i, img := range images {
		if img.MimeType != wantMime[i] {
			t.Errorf("image[%d] mime = %q, want %q", i, img.MimeType, wantMime[i])
		}
		// Round-trip: base64 decode back and compare against the served body.
		b, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil {
			t.Errorf("image[%d] base64 decode failed: %v", i, err)
			continue
		}
		if len(b) != len(want[i]) {
			t.Errorf("image[%d] decoded length = %d, want %d", i, len(b), len(want[i]))
			continue
		}
		for j := range want[i] {
			if b[j] != want[i][j] {
				t.Errorf("image[%d] byte %d = %d, want %d", i, j, b[j], want[i][j])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// gimmickPrompt — the 5-arg SHIPPED form (images line + referenced block).
// ---------------------------------------------------------------------------

func TestGimmickPromptImages(t *testing.T) {
	content := "content"
	known := []string{"bike"}

	// 2 images, no ref: the pinned images line with N = 2.
	got := gimmickPrompt(defaultPromptTemplate, content, known, 2, "")
	if !strings.Contains(got, "The message also has 2 attached image(s)") {
		t.Errorf("2-image form must contain the pinned images line with 2")
	}

	// 0 images, ref text: the pinned referenced block; no images line.
	gotRef := gimmickPrompt(defaultPromptTemplate, content, known, 0, "ref text")
	for _, part := range []string{"<<<REFERENCED MESSAGE", "ref text", "REFERENCED MESSAGE>>>", "The message replies to a previous message (often the author's own)"} {
		if !strings.Contains(gotRef, part) {
			t.Errorf("ref form must contain %q", part)
		}
	}
	if strings.Contains(gotRef, "The message also has") {
		t.Errorf("0-image ref form must not contain the images line")
	}

	// 2 images + ref text: BOTH elements.
	gotBoth := gimmickPrompt(defaultPromptTemplate, content, known, 2, "ref text")
	if !strings.Contains(gotBoth, "The message also has 2 attached image(s)") || !strings.Contains(gotBoth, "<<<REFERENCED MESSAGE") {
		t.Errorf("form with both must contain the images line AND the referenced block")
	}

	// 0 images / no ref: the shipped 0-image/no-ref prompt form — neither
	// optional element, the rest intact (the shipped TestGimmickPromptDefault
	// pins the full bytes; this restates it at the flow level).
	gotZero := gimmickPrompt(defaultPromptTemplate, content, known, 0, "")
	if strings.Contains(gotZero, "The message also has") {
		t.Errorf("0-image no-ref form must not contain the images line")
	}
	if strings.Contains(gotZero, "REFERENCED MESSAGE") {
		t.Errorf("no-ref form must not contain the referenced block")
	}
	for _, part := range []string{"<<<UNTRUSTED MESSAGE", content, "UNTRUSTED MESSAGE>>>"} {
		if !strings.Contains(gotZero, part) {
			t.Errorf("0-image no-ref form must still contain the shipped %q", part)
		}
	}
}

// ---------------------------------------------------------------------------
// The flow — images
// ---------------------------------------------------------------------------

func TestFlowImagesUseAskWithImages(t *testing.T) {
	srv, _ := newImgServer(t)
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithImage("totally safe words", srv.URL+"/a.png"))

	if pi.imageAsks != 1 {
		t.Errorf("pi.imageAsks = %d, want 1 (a downloaded image must use AskWithImages)", pi.imageAsks)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the text ask counter must stay untouched)", pi.asks)
	}
	if len(pi.imagePrompts) != 1 {
		t.Fatalf("imagePrompts = %v, want exactly one", pi.imagePrompts)
	}
	if !strings.Contains(pi.imagePrompts[0], "The message also has 1 attached image(s)") {
		t.Errorf("image prompt must contain the image line with 1")
	}
	if len(pi.images) != 1 || len(pi.images[0]) != 1 {
		t.Errorf("images = %v, want exactly one image in the ask", pi.images)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowImageOnlyLearnsAndDeletes(t *testing.T) {
	// Image-only message (no text tokens): the verdict word may come from
	// the image — wordValid alone bounds it, then learn + delete.
	srv, _ := newImgServer(t)
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithImage("", srv.URL+"/a.png"))

	if pi.imageAsks != 1 {
		t.Errorf("pi.imageAsks = %d, want 1", pi.imageAsks)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0", pi.asks)
	}
	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want exactly one delete [[c1 msg1]]", ops.deleted)
	}
}

func TestFlowImageOnlyVerdictInvalidWordRejected(t *testing.T) {
	// The charset gate still applies to an image-only message.
	srv, _ := newImgServer(t)
	pi := &fakePi{resp: "GIMMICK:s-w1ft"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithImage("", srv.URL+"/a.png"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowTextAndImageVerdictWordNotInTextRejected(t *testing.T) {
	// Text + image: text tokens EXIST, so the folded verdict word must be
	// a token of the text — a word only in the image is not learned (the
	// text-token gate is preserved whenever text tokens exist).
	srv, _ := newImgServer(t)
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithImage("hi there", srv.URL+"/a.png"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

func TestFlowImageDownloadFailureDegradesToTextAsk(t *testing.T) {
	// Every URL fails (closed listener): nothing downloads -> the flow
	// degrades to a plain text ask (0 images, no ref bytes).
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	closed.Close()
	content := "totally safe words"
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithImage(content, closed.URL+"/a.png"))

	if pi.imageAsks != 0 {
		t.Errorf("pi.imageAsks = %d, want 0 (nothing downloaded)", pi.imageAsks)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (degraded to a text ask)", pi.asks)
	}
	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, "")
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want the 0-image text prompt %q", pi.prompts[0], want)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

// ---------------------------------------------------------------------------
// The flow — referenced messages
// ---------------------------------------------------------------------------

func TestFlowReferencedFetchFailureDegrades(t *testing.T) {
	// The referenced fetch fails (already deleted): log + continue WITHOUT
	// the reference — no abort; the flow judges the current message only.
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{refErr: errors.New("gone")}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithRef("totally safe words"))

	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want 1", ops.refCalls)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (the flow survives the fetch failure)", pi.asks)
	}
	if pi.imageAsks != 0 {
		t.Errorf("pi.imageAsks = %d, want 0", pi.imageAsks)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowReferencedFastHitDeletes(t *testing.T) {
	// The gimmick word lives ONLY in the referenced message: the token
	// union makes it a fast hit — no pi ask, one delete.
	pi := &fakePi{}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}, words: map[string]bool{"sw1ft": true}}
	ops := &fakeOps{ref: &discordgo.Message{Content: "who's giving me a sw1ft."}}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithRef("↑↑↑"))

	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want one fast delete [[c1 msg1]]", ops.deleted)
	}
	if ops.refCalls != 1 {
		t.Errorf("refCalls = %d, want 1", ops.refCalls)
	}
	if pi.asks != 0 || pi.imageAsks != 0 {
		t.Errorf("pi.asks = %d, pi.imageAsks = %d, want both 0 (the fast path must not reach pi)", pi.asks, pi.imageAsks)
	}
	assertNothingLearned(t, store)
}

func TestFlowReferencedPromptCarriesRefContent(t *testing.T) {
	// No fast hit, no images: the prompt quotes the fetched referenced
	// content between the REFERENCED MESSAGE markers.
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{Content: "holler at zswiftf now"}}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithRef("holler"))

	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
	if pi.imageAsks != 0 {
		t.Errorf("pi.imageAsks = %d, want 0 (no images)", pi.imageAsks)
	}
	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	for _, part := range []string{"<<<REFERENCED MESSAGE", "holler at zswiftf now", "REFERENCED MESSAGE>>>"} {
		if !strings.Contains(pi.prompts[0], part) {
			t.Errorf("prompt must contain %q", part)
		}
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowReferencedVerdictWordOnlyInRefLearns(t *testing.T) {
	// A valid verdict word present ONLY in the referenced text is learned
	// (the quote is part of what's being posted): the union gate passes.
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{Content: "holler at zswiftf now"}}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithRef("???"))

	if len(store.added) != 1 || store.added[0] != "zswiftf|llm" {
		t.Errorf("added = %v, want [zswiftf|llm]", store.added)
	}
	if len(ops.deleted) != 1 || ops.deleted[0][0] != "c1" || ops.deleted[0][1] != "msg1" {
		t.Errorf("deleted = %v, want [[c1 msg1]]", ops.deleted)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1", pi.asks)
	}
}

func TestFlowReferencedVerdictWordNowhereRejected(t *testing.T) {
	// A valid word in NEITHER the posted nor the referenced text is
	// rejected by the (union) token gate.
	pi := &fakePi{resp: "GIMMICK:zswiftf"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{Content: "bbb"}}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithRef("aaa"))

	assertNothingLearned(t, store)
	assertNoDeletes(t, ops)
}

// ---------------------------------------------------------------------------
// The flow — current + referenced image union
// ---------------------------------------------------------------------------

func TestDownloadPlanDedupesReferenced(t *testing.T) {
	// Same URL on both sides (posted attachment + referenced attachment):
	// the union plan dedupes by URL — one download of X, plus the unique
	// referenced URL Y.
	srv, _ := newImgServer(t)
	urlX := srv.URL + "/x.png"
	urlY := srv.URL + "/y.png"
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{ref: &discordgo.Message{
		Attachments: []*discordgo.MessageAttachment{
			{URL: urlX, ContentType: "image/png"},
			{URL: urlY, ContentType: "image/png"},
		},
	}}
	h := newTestDerpies(store, ops, pi)
	m := msgWithImage("", urlX)
	m.MessageReference = &discordgo.MessageReference{MessageID: "ref1"}
	h.flow(m)

	if pi.imageAsks != 1 {
		t.Errorf("pi.imageAsks = %d, want 1", pi.imageAsks)
	}
	if len(pi.images) != 1 || len(pi.images[0]) != 2 {
		t.Errorf("images = %v, want exactly 2 downloaded (X deduped against the referenced copy, Y included)", pi.images)
	}
}
