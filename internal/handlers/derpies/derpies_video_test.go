// Package derpies — gifv embed-video expansion tests. The gifv embed's
// MessageEmbed.Video (an mp4/webm on the provider's static CDN) is the ONLY
// place the actual animation lives — the embed's thumbnail is just the first
// frame. The video-frame leg (videoURLPlan / downloadVideoBytes /
// govidVideoFrames behind the videoFrameDecoder seam) samples the clip into
// <=8 JPEG frames that join the same single AskWithImages call. The decoder
// sits behind a seam (tests use a fake; New() wires the real govid decoder);
// a nil decoder / any decoder error / a download failure / an over-cap
// Content-Length all degrade to the status quo (thumbnail-only) with a log and
// NEVER abort the flow.
//
// Generation note for testdata/clip.mp4: generated once with
//
//	ffmpeg -f lavfi -i testsrc=duration=1.6:rate=10:size=320x180 \
//	    -pix_fmt yuv420p -c:v libx264 -profile:v high testdata/clip.mp4
//
// (16 frames, 10 fps, H.264/High, ~8KB). The fixture test MUST NOT skip on a
// missing file — a missing fixture is a real failure.
package derpies

import (
	"bytes"
	"encoding/base64"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/app"
)

// msgWithGifvEmbed — a message from the filtered user carrying one gifv embed
// whose video lives at videoURL (the only leg that expands to frames).
func msgWithGifvEmbed(videoURL string) *discordgo.Message {
	m := derpMsg("totally safe words")
	m.Embeds = []*discordgo.MessageEmbed{{
		Type:  "gifv",
		Video: &discordgo.MessageEmbedVideo{URL: videoURL},
	}}
	return m
}

// fourFrameImages — four distinct app.PiImages (distinct payloads + mimes so a
// "the ask carries exactly the decoder's frames" comparison is unambiguous).
func fourFrameImages() []app.PiImage {
	payloads := []string{"frame-0", "frame-1", "frame-2", "frame-3"}
	var imgs []app.PiImage
	for _, p := range payloads {
		imgs = append(imgs, app.PiImage{
			MimeType: "image/jpeg",
			Data:     base64.StdEncoding.EncodeToString([]byte(p)),
		})
	}
	return imgs
}

// ---------------------------------------------------------------------------
// videoURLPlan — the plan leg (inside imageURLPlan): the gifv embed's video
// URL is planned, non-gifv embeds are not, and an unsafe video URL is skipped.
// ---------------------------------------------------------------------------

func TestVideoPlan(t *testing.T) {
	// A gifv embed with a video URL -> an embed-video entry in the plan.
	gifv := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{
			Type:  "gifv",
			Video: &discordgo.MessageEmbedVideo{URL: "https://cdn.example/clip.mp4"},
		}},
	}
	plan := imageURLPlan(gifv)
	found := false
	for _, e := range plan {
		if e.source == "embed-video" && e.url == "https://cdn.example/clip.mp4" {
			found = true
		}
	}
	if !found {
		t.Errorf("a gifv embed must plan its video URL as an embed-video entry; plan = %+v", plan)
	}

	// A NON-gifv embed carrying a video (e.g. Type "video") -> NO video entry.
	nonGifv := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{
			Type:  "video",
			Video: &discordgo.MessageEmbedVideo{URL: "https://cdn.example/other.mp4"},
		}},
	}
	for _, e := range imageURLPlan(nonGifv) {
		if e.source == "embed-video" {
			t.Errorf("a non-gifv embed must NOT plan a video entry; got %+v", e)
		}
	}

	// An unsafe video URL (ftp://) is skipped (mirroring the embed-URL guard).
	unsafe := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{
			Type:  "gifv",
			Video: &discordgo.MessageEmbedVideo{URL: "ftp://cdn.example/clip.mp4"},
		}},
	}
	for _, e := range imageURLPlan(unsafe) {
		if e.source == "embed-video" {
			t.Errorf("an unsafe video URL must be skipped; got %+v", e)
		}
	}
}

// ---------------------------------------------------------------------------
// flow — video frames join the ask / degrade on failure
// ---------------------------------------------------------------------------

func TestFlowVideoFramesJoinAsk(t *testing.T) {
	srv, _ := newImgServer(t) // serves a small 200 body (deemed video bytes here)
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	wire := fourFrameImages()
	h.videoFrameDecoder = func(_ []byte) ([]app.PiImage, error) { return wire, nil }
	h.flow(msgWithGifvEmbed(srv.URL + "/clip.mp4"))

	if pi.imageAsks != 1 {
		t.Fatalf("pi.imageAsks = %d, want 1 (decoded video frames use AskWithImages)", pi.imageAsks)
	}
	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the video frames drive the image ask)", pi.asks)
	}
	if len(pi.images) != 1 {
		t.Fatalf("pi.images = %d asks, want 1", len(pi.images))
	}
	got := pi.images[0]
	if len(got) != len(wire) {
		t.Fatalf("the ask carried %d frame(s), want %d (the decoder's frames): %+v", len(got), len(wire), got)
	}
	for i := range wire {
		if got[i].MimeType != wire[i].MimeType || got[i].Data != wire[i].Data {
			t.Errorf("ask image[%d] = %+v, want %+v", i, got[i], wire[i])
		}
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowVideoDownloadFailureDegrades(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.videoFrameDecoder = func(_ []byte) ([]app.PiImage, error) { calls++; return nil, nil }
	h.flow(msgWithGifvEmbed(srv.URL + "/clip.mp4"))

	if pi.asks+pi.imageAsks != 1 {
		t.Fatalf("total asks = %d (text %d + image %d), want 1 — a 404 video must not abort the flow",
			pi.asks+pi.imageAsks, pi.asks, pi.imageAsks)
	}
	if pi.imageAsks != 0 {
		t.Errorf("a 404 video must not yield an AskWithImages call; imageAsks = %d", pi.imageAsks)
	}
	if calls != 0 {
		t.Errorf("the decoder must not be called with a 404 body (a 404 body is never a decodable clip); calls = %d", calls)
	}
	assertNoDeletes(t, ops)
	assertNothingLearned(t, store)
}

func TestFlowVideoLargeContentLengthSkipped(t *testing.T) {
	calls := 0
	sawBody := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody = true
		w.Header().Set("Content-Length", "9999999") // > 8MB cap
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tiny"))
	}))
	t.Cleanup(srv.Close)

	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.videoFrameDecoder = func(_ []byte) ([]app.PiImage, error) { calls++; return nil, nil }
	h.flow(msgWithGifvEmbed(srv.URL + "/clip.mp4"))

	if !sawBody {
		t.Fatalf("the test server was never reached — the video URL must be requested")
	}
	if calls != 0 {
		t.Errorf("an over-cap Content-Length must short-circuit the download BEFORE decode; calls = %d", calls)
	}
	if pi.asks+pi.imageAsks != 1 {
		t.Errorf("the flow must still ask (total asks = %d, want 1)", pi.asks+pi.imageAsks)
	}
}

// ---------------------------------------------------------------------------
// govidVideoFrames — the REAL decoder (no fake), end-to-end on the fixture.
// ---------------------------------------------------------------------------

func TestVideoFramesGovidFixture(t *testing.T) {
	// The fixture is a HARD dependency: a missing file is a real failure, not
	// a skip. The test working directory is the package directory, so
	// testdata/clip.mp4 resolves relative to it.
	data, err := os.ReadFile("testdata/clip.mp4")
	if err != nil {
		t.Fatalf("fixture testdata/clip.mp4 is required (not skippable): %v", err)
	}
	frames, err := govidVideoFrames(data)
	if err != nil {
		t.Fatalf("govidVideoFrames(clip.mp4) returned an error: %v", err)
	}
	// 16 frames at 10fps / 1.6s -> k = min(8, ...) = 8 sampled frames.
	if len(frames) != 8 {
		t.Fatalf("frames = %d, want 8 (16 frames -> 8 seek-sampled frames)", len(frames))
	}
	for i, f := range frames {
		if f.MimeType != "image/jpeg" {
			t.Errorf("frame[%d] MimeType = %q, want image/jpeg", i, f.MimeType)
		}
		raw, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			t.Errorf("frame[%d] base64 decode failed: %v", i, err)
			continue
		}
		if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
			t.Errorf("frame[%d] is not a decodable JPEG: %v", i, err)
		}
	}
}
