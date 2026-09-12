// Package derpies — uploaded animated-gif expansion tests. The expansion
// leg (expandGIFFrames) is in-process and seam-free; the flow tests use
// a real net/http/httptest server serving a gif built in-test with
// image/gif.Encode into a buffer (no binary fixtures), the attachment URL
// injected the same way TestDownloadImages does, and the download running
// against the real http.Client.
package derpies

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildGIF encodes frames into a real multi-frame gif byte sequence (one
// gif.EncodeAll call writes ALL frames of the gif.GIF struct).
func buildGIF(t *testing.T, frames []*image.Paletted) []byte {
	t.Helper()
	var buf bytes.Buffer
	delays := make([]int, len(frames))
	for i := range delays {
		delays[i] = 10
	}
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: frames, Delay: delays}); err != nil {
		t.Fatalf("gif encode: %v", err)
	}
	return buf.Bytes()
}

// solidFrame — a 4x4 solid Paletted frame at gray v (distinct frames differ
// by >= 16 gray levels — well outside the duplicate check's tolerance 8).
func solidFrame(v uint8) *image.Paletted {
	f := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.RGBA{R: v, G: v, B: v, A: 255}})
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			f.SetColorIndex(x, y, 0)
		}
	}
	return f
}

// grayOfLuma at the decoded frame's center pixel, 8-bit luma (a solid
// frame's luma round-trips its gray within a few levels through gif +
// jpeg q80).
func grayOfLuma(t *testing.T, data []byte) int {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	r, g, b, _ := img.At(2, 2).RGBA()
	return int((uint64(r)*299 + uint64(g)*587 + uint64(b)*114) / 257000)
}

// ---------------------------------------------------------------------------
// expandGIFFrames — the pure helper
// ---------------------------------------------------------------------------

func TestExpandGIFFrames(t *testing.T) {
	t.Run("ten distinct frames yield 8 evenly-spaced jpegs", func(t *testing.T) {
		var frames []*image.Paletted
		for i := 0; i < 10; i++ {
			frames = append(frames, solidFrame(uint8(10+i*16))) // gray 10..154, step 16
		}
		got, skipped, err := expandGIFFrames(buildGIF(t, frames))
		if err != nil {
			t.Fatalf("expandGIFFrames(%d frames) err = %v, want nil", len(frames), err)
		}
		if skipped != 0 {
			t.Errorf("skipped = %d, want 0 (no encode failures on solid frames)", skipped)
		}
		if len(got) != 8 {
			t.Fatalf("frames = %d, want 8 (gifOutputFrames)", len(got))
		}
		// n=10, k=8 even-spacing indices: i*(10-1)/(8-1) = [0,1,2,3,5,6,7,9].
		wantGrays := []int{10, 26, 42, 58, 90, 106, 122, 154}
		for i, img := range got {
			if img.MimeType != "image/jpeg" {
				t.Errorf("frame[%d] mime = %q, want image/jpeg", i, img.MimeType)
			}
			data, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				t.Fatalf("frame[%d] base64 decode: %v", i, err)
			}
			g := grayOfLuma(t, data)
			if d := g - wantGrays[i]; d > 12 || d < -12 {
				t.Errorf("frame[%d] gray = %d, want ~%d (even-spacing index)", i, g, wantGrays[i])
			}
		}
	})

	t.Run("single-frame gif degrades", func(t *testing.T) {
		_, _, err := expandGIFFrames(buildGIF(t, []*image.Paletted{solidFrame(60)}))
		if err != errSingleFrame {
			t.Errorf("err = %v, want errSingleFrame", err)
		}
	})

	t.Run("undecodable bytes degrade", func(t *testing.T) {
		_, _, err := expandGIFFrames([]byte("garbage"))
		if err != errGIFUnreadable {
			t.Errorf("err = %v, want errGIFUnreadable", err)
		}
	})

	t.Run("garbage over the input cap degrades with the distinct cap sentinel", func(t *testing.T) {
		// Garbage (a real giant gif is not needed) — the cap check runs
		// BEFORE the decode, so undecodable-but-oversized input must be
		// a budget refusal, not a decode failure.
		_, _, err := expandGIFFrames(make([]byte, gifInputMaxBytes+1))
		if !errors.Is(err, errGIFTooLarge) {
			t.Errorf("err = %v, want errGIFTooLarge (the distinct cap sentinel)", err)
		}
		if errors.Is(err, errGIFUnreadable) {
			t.Errorf("err must NOT be errGIFUnreadable — different log semantics (budget refusal, not decode failure)")
		}
	})

	t.Run("below-cap garbage stays a decode failure", func(t *testing.T) {
		_, _, err := expandGIFFrames(make([]byte, 1<<20))
		if !errors.Is(err, errGIFUnreadable) {
			t.Errorf("err = %v, want errGIFUnreadable (a below-cap refusal must not fire)", err)
		}
	})

	t.Run("consecutive-duplicate skip is provably exercised", func(t *testing.T) {
		// 20 frames in the running pattern A x5, B x10, A x5 (A/B distinct
		// solid colors). The 8 even-spaced selection indices [0,2,5,8,10,
		// 13,16,19] resolve to [A,A,B,B,B,B,A,A] — three runs. After the
		// consecutive-duplicate skip the result is EXACTLY 3 images (A, B,
		// A). (A simple 2-color ABAB loop would select A,B,A,B,... and never
		// trigger the skip — deliberately not used.) Asserting the exact
		// count 3 (not <= 8) is what proves the skip runs.
		a, b := solidFrame(20), solidFrame(230)
		var frames []*image.Paletted
		for i := 0; i < 5; i++ {
			frames = append(frames, a)
		}
		for i := 0; i < 10; i++ {
			frames = append(frames, b)
		}
		for i := 0; i < 5; i++ {
			frames = append(frames, a)
		}
		got, skipped, err := expandGIFFrames(buildGIF(t, frames))
		if err != nil {
			t.Fatalf("expandGIFFrames err = %v, want nil", err)
		}
		if skipped != 0 {
			t.Errorf("skipped = %d, want 0", skipped)
		}
		if len(got) != 3 {
			t.Fatalf("frames = %d, want exactly 3 (A, B, A after the consecutive-duplicate skip)", len(got))
		}
		data, err := base64.StdEncoding.DecodeString(got[0].Data)
		if err != nil {
			t.Fatalf("frame[0] base64 decode: %v", err)
		}
		if g := grayOfLuma(t, data); g < 0 || g > 40 {
			t.Errorf("frame[0] gray = %d, want ~%d (the A block)", g, 20)
		}
		data, err = base64.StdEncoding.DecodeString(got[1].Data)
		if err != nil {
			t.Fatalf("frame[1] base64 decode: %v", err)
		}
		if g := grayOfLuma(t, data); g < 200 || g > 255 {
			t.Errorf("frame[1] gray = %d, want ~%d (the B block)", g, 230)
		}
	})
}

// ---------------------------------------------------------------------------
// The pre-decode input cap — errGIFTooLarge
// ---------------------------------------------------------------------------

func TestFlowOversizeGIFDegradesToRaw(t *testing.T) {
	// Oversize attachment (well over the 16MB pre-decode cap): budget
	// refusal before any decode — raw send, no frames (the degrade arm
	// mirrors the single-frame / undecodable arms).
	body := make([]byte, gifInputMaxBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithGif("totally safe words", srv.URL+"/huge.gif"))

	if pi.imageAsks != 1 {
		t.Fatalf("pi.imageAsks = %d, want 1 (the raw gif must still be judged)", pi.imageAsks)
	}
	if len(pi.images) != 1 || len(pi.images[0]) != 1 {
		t.Fatalf("ask images = %d, want exactly ONE (the raw gif — no frames)", len(pi.images[0]))
	}
	img := pi.images[0][0]
	if img.MimeType != "image/gif" {
		t.Errorf("mime = %q, want image/gif (oversize gif degrades to the raw send)", img.MimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, body) {
		t.Errorf("decoded gif bytes must equal the raw served bytes (the status-quo raw send)")
	}
	if strings.Contains(pi.imagePrompts[0], "animated gif frame") {
		t.Errorf("image prompt must NOT contain the gif-frames block for an oversize gif")
	}
	assertNoDeletes(t, ops)
}

// ---------------------------------------------------------------------------
// Delta-rectangle compositing
// ---------------------------------------------------------------------------

// solidFrameAt — a w×h solid Paletted frame at gray v whose bounds are
// offset (x0, y0) (a real sub-rectangle — frame 2 of a delta gif).
func solidFrameAt(x0, y0, w, h int, v uint8) *image.Paletted {
	f := image.NewPaletted(image.Rect(x0, y0, x0+w, y0+h), color.Palette{color.RGBA{R: v, G: v, B: v, A: 255}})
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			f.SetColorIndex(x, y, 0)
		}
	}
	return f
}

// buildGIFDelta encodes a full-canvas frame followed by a sub-rectangle
// delta frame into a real on-disk gif. This toolchain's gif.EncodeAll
// preserves a frame's non-origin bounds on disk and gif.DecodeAll retains
// them on read (verified empirically and re-asserted below by inspecting
// the decoded Bounds) — a genuine delta-rectangle animation, the case
// partial-frame gifs with transparent-pixel animations decode into.
func buildGIFDelta(t *testing.T, full *image.Paletted, delta *image.Paletted) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{full, delta}, Delay: []int{10, 10}}); err != nil {
		t.Fatalf("delta gif encode: %v", err)
	}
	return buf.Bytes()
}

// lumaAt — 8-bit luma of a decoded image's pixel (solid colors round-trip
// within a few levels through gif + jpeg q80).
func lumaAt(t *testing.T, data []byte, x, y int) int {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	r, g, b, _ := img.At(x, y).RGBA()
	return int((uint64(r)*299 + uint64(g)*587 + uint64(b)*114) / 257000)
}

func TestExpandGIFFramesDeltaComposited(t *testing.T) {
	// Frame 1 = full-canvas 40×40 solid S1 (gray 230); frame 2 = a 20×20
	// sub-rectangle at (10,10) solid S2 (gray 10). Decoded as-is, frame 2
	// is a 20×20 SUB-rectangle — compositing the expansion must be on the
	// full 40×40 canvas: S1 background everywhere, S2 at the offset.
	s1 := solidFrameAt(0, 0, 40, 40, 230)
	delta := solidFrameAt(10, 10, 20, 20, 10)
	body := buildGIFDelta(t, s1, delta)

	// Re-verify the on-disk delta (the encoded bytes must retain the
	// offset — the toolchain here preserves Bounds; if it normalized to
	// (0,0) the compositing under test no longer holds and this fails).
	g, err := gif.DecodeAll(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("self-decode of the test gif: %v", err)
	}
	if b := g.Image[1].Bounds(); b.Min.X != 10 || b.Min.Y != 10 {
		t.Fatalf("test gif lost its delta offset (decoded frame 2 bounds %v) — the toolchain normalizes; the compositing under test does not hold", b)
	}

	got, skipped, err := expandGIFFrames(body)
	if err != nil {
		t.Fatalf("expandGIFFrames err = %v, want nil", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2 (40×40 solid + the composed delta — not a duplicate: the canvases differ)", len(got))
	}
	data, err := base64.StdEncoding.DecodeString(got[1].Data)
	if err != nil {
		t.Fatalf("frame[1] base64 decode: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("frame[1] jpeg decode: %v", err)
	}
	// CANVAS-sized: the expanded delta frame must be the full 40×40
	// logical screen, not the 20×20 sub-rectangle.
	if b := img.Bounds(); b != image.Rect(0, 0, 40, 40) {
		t.Fatalf("frame[1] bounds = %v, want (0,0)-(40,40) (full canvas, not the bare sub-rectangle)", b)
	}
	// Contains BOTH the S1 background (a pixel away from the sub-rectangle)
	// AND the S2 sub-rectangle at the offset.
	if l := lumaAt(t, data, 5, 5); l < 200 || l > 255 {
		t.Errorf("frame[1] pixel (5,5) luma = %d, want ~230 (the S1 background survives under the delta)", l)
	}
	if l := lumaAt(t, data, 15, 15); l < 0 || l > 40 {
		t.Errorf("frame[1] pixel (15,15) luma = %d, want ~10 (the S2 sub-rectangle composited at the offset)", l)
	}
}

// ---------------------------------------------------------------------------
// The flow — uploaded animated gifs
// ---------------------------------------------------------------------------

// msgWithGif — a message from the filtered user carrying one animated-gif
// attachment at url (mirror of msgWithImage; the URL injection is the
// TestDownloadImages pattern — the real http.Client downloads it).
func msgWithGif(content, url string) *discordgo.Message {
	m := derpMsg(content)
	m.Attachments = []*discordgo.MessageAttachment{{URL: url, ContentType: "image/gif"}}
	return m
}

func TestFlowGIFFrameExpansionJoinsAsk(t *testing.T) {
	var frames []*image.Paletted
	for i := 0; i < 10; i++ {
		frames = append(frames, solidFrame(uint8(10+i*16)))
	}
	body := buildGIF(t, frames)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithGif("", srv.URL+"/anim.gif"))

	if pi.asks != 0 {
		t.Errorf("pi.asks = %d, want 0 (the image path must use AskWithImages)", pi.asks)
	}
	if pi.imageAsks != 1 {
		t.Fatalf("pi.imageAsks = %d, want 1", pi.imageAsks)
	}
	if len(pi.images) != 1 || len(pi.images[0]) != 8 {
		t.Fatalf("ask images = %d, want 8 sampled frames (the gifOutputFrames cap)", len(pi.images[0]))
	}
	for i, img := range pi.images[0] {
		if img.MimeType != "image/jpeg" {
			t.Errorf("frame[%d] mime = %q, want image/jpeg (the raw gif bytes must NOT be sent for an expanded gif)", i, img.MimeType)
		}
	}
	// The prompt declares the sampled frames (the {{GIFS}} block, default
	// template present).
	if len(pi.imagePrompts) != 1 {
		t.Fatalf("imagePrompts = %d, want exactly one", len(pi.imagePrompts))
	}
	if !strings.Contains(pi.imagePrompts[0], "The message also has animated gif frame(s). In addition to the summary images above, below are up to 8") {
		t.Errorf("image prompt must contain the {{GIFS}} block; got:\n%q", pi.imagePrompts[0])
	}
	assertNoDeletes(t, ops)
}

func TestFlowSingleFrameGIFDegradesToRaw(t *testing.T) {
	body := buildGIF(t, []*image.Paletted{solidFrame(80)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithGif("totally safe words", srv.URL+"/still.gif"))

	if pi.imageAsks != 1 {
		t.Fatalf("pi.imageAsks = %d, want 1 (the raw gif must still be judged)", pi.imageAsks)
	}
	if len(pi.images) != 1 || len(pi.images[0]) != 1 {
		t.Fatalf("ask images = %d, want exactly ONE (the raw gif)", len(pi.images[0]))
	}
	img := pi.images[0][0]
	if img.MimeType != "image/gif" {
		t.Errorf("mime = %q, want image/gif (single-frame gif degrades to the raw send)", img.MimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, body) {
		t.Errorf("decoded gif bytes must equal the raw served bytes (the status-quo raw send)")
	}
	// No gif-frames claim in the prompt (0 gif frames).
	if !strings.Contains(pi.imagePrompts[0], "The message also has 1 attached image(s)") {
		t.Errorf("image prompt must contain the 1-image line")
	}
	if strings.Contains(pi.imagePrompts[0], "animated gif frame") {
		t.Errorf("image prompt must NOT contain the gif-frames block for a single-frame gif")
	}
	assertNoDeletes(t, ops)
}

func TestFlowGIFDownloadFailureStillDegradesToTextAsk(t *testing.T) {
	// Closed listener: the gif download fails -> the flow degrades to a
	// text-only ask (mirrors TestFlowImageDownloadFailureDegradesToTextAsk).
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	closed.Close()
	content := "totally safe words"
	pi := &fakePi{resp: "CLEAN"}
	store := &fakeStore{enabled: map[string]bool{FeatureKey: true}}
	ops := &fakeOps{}
	h := newTestDerpies(store, ops, pi)
	h.flow(msgWithGif(content, closed.URL+"/anim.gif"))

	if pi.imageAsks != 0 {
		t.Errorf("pi.imageAsks = %d, want 0 (nothing downloaded)", pi.imageAsks)
	}
	if pi.asks != 1 {
		t.Errorf("pi.asks = %d, want 1 (degraded to a text ask)", pi.asks)
	}
	if len(pi.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", pi.prompts)
	}
	want := gimmickPrompt(defaultPromptTemplate, content, sortedKeys(store.words), 0, 0, "", "")
	if pi.prompts[0] != want {
		t.Errorf("prompt = %q, want the 0-image 0-gif-frame text prompt %q", pi.prompts[0], want)
	}
	assertNoDeletes(t, ops)
}
