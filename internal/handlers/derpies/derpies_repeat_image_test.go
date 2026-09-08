package derpies

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Repeat-image fast path (flow 4.6). Matching the derpies_test.go
// convention, tests drive h.flow synchronously over the fakes; image
// download goes through the real http.Client against an httptest server.
// ---------------------------------------------------------------------------

// bodyServer — a server answering 200 with the given body; the body is
// the image CONTENT (two server bodies = two image contents).
func bodyServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *requestAlias) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type requestAlias = http.Request

func newRepeatTest(t *testing.T) (*Derpies, *fakeStore, *fakeOps, *fakePi) {
	t.Helper()
	store := &fakeStore{enabled: make(map[string]bool)}
	store.enabled[FeatureKey] = true
	ops := &fakeOps{}
	pi := &fakePi{}
	h := newTestDerpies(store, ops, pi)
	return h, store, ops, pi
}

// imgMsg — a filtered-user message carrying one image attachment; the URL
// distinctness is irrelevant (the CONTENT hash decides).
func imgMsg(id, url string) *discordgo.Message {
	m := derpMsg("")
	m.ID = id
	m.Attachments = []*discordgo.MessageAttachment{{URL: url, ContentType: "image/png"}}
	return m
}

// TestRepeatImageFastDelete — the core rule: the first sighting of an
// image content is LLM-judged (and learned, on a GIMMICK verdict); the
// SECOND post of the same content is the repeat: deleted with ZERO asks.
func TestRepeatImageFastDelete(t *testing.T) {
	srv := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x01})
	h, _, ops, pi := newRepeatTest(t)
	pi.resp = "GIMMICK:sw1ft"

	h.flow(imgMsg("m1", srv.URL+"/a.png"))
	if pi.imageAsks != 1 {
		t.Fatalf("first sighting: imageAsks = %d, want 1", pi.imageAsks)
	}
	if len(ops.deleted) != 1 {
		t.Fatalf("first sighting: deletes = %v, want 1", ops.deleted)
	}

	h.flow(imgMsg("m2", srv.URL+"/a.png-reupload")) // same content, new attachment id
	if pi.imageAsks != 1 {
		t.Fatalf("repeat: imageAsks = %d, want 1 (NO ask)", pi.imageAsks)
	}
	if len(ops.deleted) != 2 {
		t.Fatalf("repeat: deletes = %v, want 2", ops.deleted)
	}
}

// TestRepeatImageWithNewTextGetsTextOnlyAsk — a seen image + NEW text is
// NOT a pure repeat: the seen image drops out of the payload and the text
// is judged as a PLAIN (image-free) ask.
func TestRepeatImageWithNewTextGetsTextOnlyAsk(t *testing.T) {
	srv := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x02})
	h, _, _, pi := newRepeatTest(t)
	pi.resp = "CLEAN"

	h.flow(imgMsg("m1", srv.URL+"/a.png"))
	if pi.imageAsks != 1 || pi.asks != 0 {
		t.Fatalf("first sighting: imageAsks=%d asks=%d, want 1/0", pi.imageAsks, pi.asks)
	}

	m2 := derpMsg("hello entirely fresh text")
	m2.ID = "m2"
	m2.Attachments = []*discordgo.MessageAttachment{{URL: srv.URL + "/a.png-again", ContentType: "image/png"}}
	h.flow(m2)

	if pi.imageAsks != 1 {
		t.Fatalf("text+seen-image: imageAsks = %d, want 1 (the seen image must not be re-asked)", pi.imageAsks)
	}
	if pi.asks != 1 {
		t.Fatalf("text+seen-image: plain asks = %d, want 1 (image-free text ask)", pi.asks)
	}
}

// TestUnjudgedImageRepostIsJudged — an ask FAILURE marks nothing: the
// re-post of an UNjudged image is judged (never fast-deleted blind).
func TestUnjudgedImageRepostIsJudged(t *testing.T) {
	srv := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x03})
	h, _, _, pi := newRepeatTest(t)
	pi.askErr = errors.New(" boom")

	h.flow(imgMsg("m1", srv.URL+"/a.png"))
	pi.askErr = nil
	pi.resp = "CLEAN"

	h.flow(imgMsg("m2", srv.URL+"/a.png-again"))
	if pi.imageAsks != 2 {
		t.Fatalf("unjudged re-post: imageAsks = %d, want 2 (the failed ask marks nothing)", pi.imageAsks)
	}
}

// TestMixedSeenAndFreshImages — seen + fresh images in one message: the
// fresh one is asked (the seen one drops out of the payload, so the ask
// carries exactly one image: B); the pure repeat of both afterwards is a
// fast delete (no asks).
func TestMixedSeenAndFreshImages(t *testing.T) {
	a := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x0a})
	b := bodyServer(t, []byte{0x89, 0x50, 0x4e, 0x50, 0x0b})
	h, _, ops, pi := newRepeatTest(t)
	pi.resp = "CLEAN"

	msgAB := func(id string) *discordgo.Message {
		m := derpMsg("")
		m.ID = id
		m.Attachments = []*discordgo.MessageAttachment{
			{URL: a.URL + "/a.png", ContentType: "image/png"},
			{URL: b.URL + "/b.png", ContentType: "image/png"},
		}
		return m
	}

	h.flow(imgMsg("m1", a.URL)) // judges A
	if pi.imageAsks != 1 {
		t.Fatalf("setup: imageAsks = %d, want 1", pi.imageAsks)
	}
	seenDeletes := len(ops.deleted)

	h.flow(msgAB("m2")) // A seen (dropped out), B fresh
	if pi.imageAsks != 2 {
		t.Fatalf("m2: imageAsks = %d, want 2 (the fresh image is asked)", pi.imageAsks)
	}
	if len(pi.images[1]) != 1 {
		t.Fatalf("m2: ask image count = %d, want 1 (the seen image dropped out of the payload)", len(pi.images[1]))
	}

	h.flow(msgAB("m3")) // both contents seen now — pure repeat, fast delete
	if pi.imageAsks != 2 {
		t.Fatalf("m3: imageAsks = %d, want 2 (no ask)", pi.imageAsks)
	}
	if len(ops.deleted) != seenDeletes+1 {
		t.Fatalf("m3: deletes = %d, want %d (the fast delete; CLEAN verdicts add none)", len(ops.deleted), seenDeletes+1)
	}
}
