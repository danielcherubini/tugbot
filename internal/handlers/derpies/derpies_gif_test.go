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

// rgbFrameAt — a w×h solid Paletted frame at RGBA c whose bounds are
// offset (x0, y0) (a real sub-rectangle — a delta frame; a
// full-canvas RGB frame when the bounds cover the whole screen).
func rgbFrameAt(x0, y0, w, h int, c color.RGBA) *image.Paletted {
	f := image.NewPaletted(image.Rect(x0, y0, x0+w, y0+h), color.Palette{c})
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			f.SetColorIndex(x, y, 0)
		}
	}
	return f
}

// transparentFrame — a w×h Paletted frame at (x0, y0) whose sole palette
// entry is alpha-0 — a no-op delta under draw.Over (the canvas beneath
// is preserved; the encoder must emit the transparent index, which the
// assertions below implicitly re-assert through the composite result).
func transparentFrame(x0, y0, w, h int) *image.Paletted {
	f := image.NewPaletted(image.Rect(x0, y0, x0+w, y0+h), color.Palette{color.RGBA{0, 0, 0, 0}})
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			f.SetColorIndex(x, y, 0)
		}
	}
	return f
}

// buildGIFWithDisposal — buildGIF plus per-frame disposal methods (the
// gif.GIF.Disposal field; a nil disposal leaves the flag off, the
// decoder pads the raw 0x00 read-back to DisposalNone-equivalent
// "keep previous").
func buildGIFWithDisposal(t *testing.T, frames []*image.Paletted, disposal []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	delays := make([]int, len(frames))
	for i := range delays {
		delays[i] = 10
	}
	g := &gif.GIF{Image: frames, Delay: delays, Disposal: disposal}
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("gif encode: %v", err)
	}
	return buf.Bytes()
}

// colAt — a decoded image's pixel, 8-bit channels (solid colors
// round-trip within a few levels through gif + jpeg q80 — 4:2:0
// subsampling spreads soft edges ~1-2px, so probe well inside blocks).
func colAt(t *testing.T, data []byte, x, y int) (uint32, uint32, uint32) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	r, g, b, _ := img.At(x, y).RGBA() // 16-bit — collapse to 8-bit for the checks
	return r >> 8, g >> 8, b >> 8
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
// Full replay — unselected intermediate frames are drawn (and their
// disposal applied) before a later selected frame composites onto the
// canvas; per-frame GIF disposal methods are honored.
// ---------------------------------------------------------------------------

func TestExpandGIFFramesWalksUnselectedDeltaFrames(t *testing.T) {
	// 9 frames, canvas 40x40. Frame 0 = full-canvas solid gray 128;
	// frames 1-6 = transparent no-op deltas; frame 7 = a 12x12 blue
	// sub-rectangle at (4,4) (UNSELECTED — the even-spacing selection
	// over 9 frames with k=8 is [0,1,2,3,4,5,6,8]); frame 8 = a 12x12
	// red sub-rectangle at (24,4) (SELECTED). The blue block must
	// survive into the selected frame 8's snapshot: a replay that
	// skips the intermediate unselected frame would hand the model a
	// canvas that was never visible during playback (the red block
	// would composite onto the pre-blue state).
	var frames []*image.Paletted
	frames = append(frames, solidFrameAt(0, 0, 40, 40, 128))
	for i := 0; i < 6; i++ {
		frames = append(frames, transparentFrame(0, 0, 1, 1))
	}
	frames = append(frames, rgbFrameAt(4, 4, 12, 12, color.RGBA{0, 0, 255, 255}))
	frames = append(frames, rgbFrameAt(24, 4, 12, 12, color.RGBA{255, 0, 0, 255}))
	body := buildGIF(t, frames)

	got, skipped, err := expandGIFFrames(body)
	if err != nil {
		t.Fatalf("expandGIFFrames err = %v, want nil", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	// Frames 1-6 are no-op deltas — their snapshots equal the
	// previously ADDED gray canvas (the consecutive-duplicate skip
	// folds them all), so EXACTLY two frames: the gray prime and the
	// final canvas (gray + blue@7 + red@8).
	if len(got) != 2 {
		t.Fatalf("frames = %d, want exactly 2 (gray, then the walked-and-composited final canvas)", len(got))
	}
	data, err := base64.StdEncoding.DecodeString(got[1].Data)
	if err != nil {
		t.Fatalf("frame[1] base64 decode: %v", err)
	}
	// The UNSELECTED frame 7's blue block, sampled well inside the
	// 12x12 block (4:2:0 edge softness spans ~2px; 6px inside is safe).
	if g2, _, b := colAt(t, data, 10, 10); g2 > 120 || b < 150 {
		t.Errorf("frame[1] pixel (10,10) g=%d b=%d, want g < 120 and b > 150 (the unselected intermediate frame's blue block must have been walked before the selected frame 8 composited)", g2, b)
	}
	// The selected frame 8's red block, and the untouched gray region.
	if r3, g3, b3 := colAt(t, data, 30, 10); r3 < 150 || b3 > 120 || g3 > 120 {
		t.Errorf("frame[1] pixel (30,10) = (%d,%d,%d), want red-ish (the selected frame 8's block)", r3, g3, b3)
	}
	if r4, g4, b4 := colAt(t, data, 10, 30); r4 > 148 || r4 < 108 || g4 > 148 || g4 < 108 || b4 > 148 || b4 < 108 {
		t.Errorf("frame[1] pixel (10,30) = (%d,%d,%d), want ~128 gray (untouched region)", r4, g4, b4)
	}
}

func TestExpandGIFFramesDisposalBackgroundClearsCanvas(t *testing.T) {
	// 3 frames (k=3, all selected), canvas 40x40. Frame 0 = full-canvas
	// solid red, disposal Background; frame 1 = a 12x12 blue
	// sub-rectangle at (4,4), disposal Background; frame 2 = a 12x12
	// green sub-rectangle at (20,20), disposal None. Per the spec,
	// after frame 1 the canvas is cleared to blank (a new empty
	// canvas — the gif screen background is transparent), so frame 2
	// composites ONLY the green block onto blank: the red and the
	// blue are gone. An implementation that only tracks the default
	// "keep previous" would retain both.
	f0 := rgbFrameAt(0, 0, 40, 40, color.RGBA{255, 0, 0, 255})
	f1 := rgbFrameAt(4, 4, 12, 12, color.RGBA{0, 0, 255, 255})
	f2 := rgbFrameAt(20, 20, 12, 12, color.RGBA{0, 230, 0, 255})
	body := buildGIFWithDisposal(t, []*image.Paletted{f0, f1, f2},
		[]byte{gif.DisposalBackground, gif.DisposalBackground, gif.DisposalNone})

	// Re-verify the on-disk disposal flags (if the toolchain dropped
	// them the test wouldn't hold — the decoder pads a missing flag
	// to keep-previous, silently disabling this case).
	g, err := gif.DecodeAll(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("self-decode of the test gif: %v", err)
	}
	if len(g.Disposal) < 2 || g.Disposal[0] != gif.DisposalBackground || g.Disposal[1] != gif.DisposalBackground {
		t.Fatalf("test gif lost its disposal flags (decoded %v) — the toolchain normalizes; the test does not hold", g.Disposal)
	}

	got, skipped, err := expandGIFFrames(body)
	if err != nil {
		t.Fatalf("expandGIFFrames err = %v, want nil", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3 (all three canvases distinct)", len(got))
	}
	// Snapshot 0: the red prime.
	d0, err := base64.StdEncoding.DecodeString(got[0].Data)
	if err != nil {
		t.Fatalf("frame[0] base64 decode: %v", err)
	}
	if r, _, b := colAt(t, d0, 30, 30); r < 150 || b > 120 {
		t.Errorf("frame[0] pixel (30,30) r=%d b=%d, want red-ish (the full red prime)", r, b)
	}
	// Snapshot 1: the blue block on BLANK — frame 0's Background
	// disposal cleared the red prime before frame 1 was drawn (the
	// spec applies the disposal between frames' displays; the red is
	// visible during frame 0's display only).
	d1, err := base64.StdEncoding.DecodeString(got[1].Data)
	if err != nil {
		t.Fatalf("frame[1] base64 decode: %v", err)
	}
	if _, _, b := colAt(t, d1, 10, 10); b < 150 {
		t.Fatalf("frame[1] pixel (10,10) b = %d, want > 150 (the blue block)", b)
	}
	if r, _, _ := colAt(t, d1, 5, 5); r > 120 {
		t.Errorf("frame[1] pixel (5,5) r = %d, want < 120 (the Background disposal after frame 0 cleared the red prime before frame 1 drew)", r)
	}
	// Snapshot 2: ONLY the green block on blank — the Background
	// disposal after frame 1 cleared the red AND the blue.
	d2, err := base64.StdEncoding.DecodeString(got[2].Data)
	if err != nil {
		t.Fatalf("frame[2] base64 decode: %v", err)
	}
	if _, g2, _ := colAt(t, d2, 26, 26); g2 < 150 {
		t.Errorf("frame[2] pixel (26,26) g = %d, want > 150 (the green block)", g2)
	}
	if _, _, b2 := colAt(t, d2, 10, 10); b2 > 60 {
		t.Errorf("frame[2] pixel (10,10) b = %d, want < 60 (cleared by the Background disposal — not the pre-clear blue)", b2)
	}
	if r, _, _ := colAt(t, d2, 5, 5); r > 120 {
		t.Errorf("frame[2] pixel (5,5) r = %d, want < 120 (cleared by the Background disposal — not the pre-clear red)", r)
	}
}

func TestExpandGIFFramesBackgroundDisposalKeepsOutsideContent(t *testing.T) {
	// 3 frames (k=3, all selected), canvas 40x40. Frame 0 = full-canvas
	// solid S1 red, disposal unset (keep); frame 1 = a 20x20 sub-rectangle
	// S2 blue at (10,10), Disposal[1] = 0x02 (DisposalBackground); frame 2
	// = a 20x8 sub-rectangle S3 green at (0,30), disposal unset (keep).
	// Spec 0x02 "restore to background" clears ONLY the disposing frame's
	// own rectangle — so in frame 2's canvas, S1 must SURVIVE OUTSIDE
	// frame 1's bounds, frame 1's own rectangle must be blank (the area
	// itself cleared), and S3 must be present. A full-canvas wipe (the
	// old behavior) loses the retained S1 outside frame 1's bounds.
	f0 := rgbFrameAt(0, 0, 40, 40, color.RGBA{255, 0, 0, 255})
	f1 := rgbFrameAt(10, 10, 20, 20, color.RGBA{0, 0, 255, 255})
	f2 := rgbFrameAt(0, 30, 20, 8, color.RGBA{0, 230, 0, 255})
	body := buildGIFWithDisposal(t, []*image.Paletted{f0, f1, f2},
		[]byte{0, gif.DisposalBackground, 0})

	// Re-verify the on-disk disposal flag (if the toolchain dropped it
	// the test wouldn't hold — the decoder reads a missing flag back as 0,
	// the "keep" default, silently disabling this case).
	g, err := gif.DecodeAll(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("self-decode of the test gif: %v", err)
	}
	if len(g.Disposal) < 2 || g.Disposal[1] != gif.DisposalBackground {
		t.Fatalf("test gif lost its 0x02 disposal flag (decoded %v) — the toolchain normalizes; the test does not hold", g.Disposal)
	}

	got, skipped, err := expandGIFFrames(body)
	if err != nil {
		t.Fatalf("expandGIFFrames err = %v, want nil", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3 (all three canvases distinct)", len(got))
	}
	// The LAST canvas: S1 + S3 present, frame 1's rectangle cleared to blank.
	d2, err := base64.StdEncoding.DecodeString(got[2].Data)
	if err != nil {
		t.Fatalf("frame[2] base64 decode: %v", err)
	}
	// (5,5): OUTSIDE frame 1's rectangle, inside frame 0's paint — still
	// S1 red (luma ~76, well above blank ~<30). The old full-canvas
	// Background disposal wipes this; spot-checking it is the RED signal.
	if l := lumaAt(t, d2, 5, 5); l < 55 {
		t.Errorf("frame[2] pixel (5,5) luma = %d, want ~>76 (S1 red survives OUTSIDE frame 1's 20x20 rectangle — 0x02 must clear only the frame's own bounds, not the whole canvas)", l)
	}
	// (22,22): inside frame 1's OWN rectangle, fully inside one 8x8
	// JPEG block (no boundary bleed) — now blank (the area itself was
	// cleared by the Background disposal).
	if l := lumaAt(t, d2, 22, 22); l > 30 {
		t.Errorf("frame[2] pixel (22,22) luma = %d, want BLANK <30 (frame 1's own rectangle cleared by its 0x02 disposal)", l)
	}
	// (5,35): well inside frame 2's 20x8 rectangle at (0,30) — S3 green
	// (luma ~135).
	if l := lumaAt(t, d2, 5, 35); l < 100 {
		t.Errorf("frame[2] pixel (5,35) luma = %d, want ~>135 (S3 green in frame 2's own rectangle)", l)
	}
}

func TestExpandGIFFramesDisposalPreviousRestoresCanvas(t *testing.T) {
	// 3 frames (k=3, all selected), canvas 40x40. Frame 0 = full-canvas
	// solid gray 120 (disposal None); frame 1 = full-canvas solid red
	// (disposal Previous); frame 2 = a 12x12 blue sub-rectangle at
	// (4,4) (disposal None). Per the spec, after frame 1 the canvas is
	// restored to its state BEFORE frame 1 was drawn — the gray 120
	// prime — so frame 2 composites the blue block on GRAY, not red.
	// (Bounded approximation: the one-step restore to the immediately
	// prior snapshot, which here IS the chain-correct answer.) An
	// implementation that only tracks the default "keep previous" would
	// leave the red canvas under the blue block for snapshot 2.
	f0 := solidFrameAt(0, 0, 40, 40, 120)
	f1 := rgbFrameAt(0, 0, 40, 40, color.RGBA{255, 0, 0, 255})
	f2 := rgbFrameAt(4, 4, 12, 12, color.RGBA{0, 0, 255, 255})
	body := buildGIFWithDisposal(t, []*image.Paletted{f0, f1, f2},
		[]byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone})

	// Re-verify the on-disk disposal flag (see the Background test).
	g, err := gif.DecodeAll(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("self-decode of the test gif: %v", err)
	}
	if len(g.Disposal) < 2 || g.Disposal[1] != gif.DisposalPrevious {
		t.Fatalf("test gif lost its disposal flag (decoded %v) — the toolchain normalizes; the test does not hold", g.Disposal)
	}

	got, skipped, err := expandGIFFrames(body)
	if err != nil {
		t.Fatalf("expandGIFFrames err = %v, want nil", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3 (all three canvases distinct)", len(got))
	}
	// Snapshot 1: the full red prime (before the Previous disposal applies).
	d1, err := base64.StdEncoding.DecodeString(got[1].Data)
	if err != nil {
		t.Fatalf("frame[1] base64 decode: %v", err)
	}
	if r, _, _ := colAt(t, d1, 30, 30); r < 150 {
		t.Errorf("frame[1] pixel (30,30) r = %d, want > 150 (the full-canvas red prime)", r)
	}
	// Snapshot 2: gray (restored by the Previous disposal) + blue block.
	d2, err := base64.StdEncoding.DecodeString(got[2].Data)
	if err != nil {
		t.Fatalf("frame[2] base64 decode: %v", err)
	}
	if r, _, _ := colAt(t, d2, 30, 30); r > 145 {
		t.Errorf("frame[2] pixel (30,30) r = %d, want < 145 (the Previous disposal must have restored the gray prime — expected ~120, not red ~255)", r)
	}
	if _, _, b := colAt(t, d2, 10, 10); b < 150 {
		t.Errorf("frame[2] pixel (10,10) b = %d, want > 150 (the blue block on the restored canvas)", b)
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
