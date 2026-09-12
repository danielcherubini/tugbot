package derpies

// ---------------------------------------------------------------------------
// Uploaded animated-gif expansion (stdlib image/gif — zero new
// dependencies, no new seam: this runs in-process for every gif
// attachment, the frames join the same single AskWithImages call and
// pass through the existing pirpc per-ask guard unchanged).
// ---------------------------------------------------------------------------

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"

	"github.com/danielcherubini/tugbot/internal/app"
)

const (
	gifOutputFrames  = 8        // max frames emitted per gif
	gifDecodeCap     = 60       // max frames walked from the stream
	gifJpegQuality   = 80       // house quality
	gifInputMaxBytes = 16 << 20 // pre-decode input cap (16MB)
)

var (
	// errSingleFrame: the bytes decode to 0 or 1 frame — the caller
	// sends the RAW bytes instead (the status-quo raw send; the
	// first-frame flattening is then the pipeline's business, exactly
	// as today).
	errSingleFrame = errors.New("single-frame gif")
	// errGIFTooLarge: the input exceeds gifInputMaxBytes BEFORE any
	// decode — a budget refusal (different log semantics than
	// errGIFUnreadable: the decode work is never attempted at all);
	// the caller sends the RAW bytes instead.
	errGIFTooLarge = errors.New("gif input over the pre-decode cap")
	// errGIFUnreadable: the bytes cannot be reliably decoded as a gif
	// — the caller sends the RAW bytes instead (degrade, never lose the
	// payload).
	errGIFUnreadable = errors.New("gif unreadable")
)

// expandGIFFrames expands an uploaded animated-gif byte slice into at
// most gifOutputFrames evenly-spaced JPEG frames, in-process.
//
// Input cap (pre-decode): len(data) > gifInputMaxBytes rejects with
// errGIFTooLarge BEFORE gif.DecodeAll runs. Residual constraint
// (documented): this toolchain's image/gif is all-or-nothing (no
// streaming NewDecoder), so the bound is on the INPUT bytes, not the
// decode work — DecodeAll still materializes EVERY frame of an
// in-bounds gif, and a fully-uncompressed large-frame gif at the cap
// still costs proportional decode work. The pre-decode cap is the
// bounded option this toolchain offers (input bounded to ≤16MB, and a
// crafted gif's decoded memory is proportionally bounded, not
// arbitrary). Any stream error — first frame or mid-stream — lands in
// errGIFUnreadable.
//
// Selection: k = min(gifOutputFrames, n) evenly-spaced indices over the
// collected frames (i * (n-1) / (k-1) for k > 1 — the same
// even-spacing math as the video task); at most gifDecodeCap frames
// walk from the stream (the rest of a long gif is never touched).
//
// Compositing (delta-frame correction — the point of this fix): a
// gif encoded as full-canvas frames followed by SUB-RECTANGLE delta
// frames (partial frame bounds with transparent-pixel animations)
// decodes here as sub-rectangles RETAINING their in-canvas offset in
// Bounds().Min (verified empirically and re-asserted by
// TestExpandGIFFramesDeltaComposited). Encoding a delta frame AS-IS
// would hand the model an incomplete sub-rectangle and hide the
// burned-in gimmick text this feature exists to catch; instead each
// selected frame is composited via draw.Draw (draw.Over, at the
// frame's bounds origin) onto a persistent *image.NRGBA canvas —
// GIF's default disposal is "keep previous", and draw.Over preserves
// the canvas through transparent palette entries (alpha 0). Canvas
// size = the union of all collected frames' bounds (the superset of
// the decoded logical screen size for in-bounds frames). A
// full-canvas cover frame re-primes the canvas (draw.Over, full
// coverage); a delta frame overlays its partial rectangle.
// The JPEG-encoded is the CANVAS, not the bare frame.
//
// Consecutive-duplicate skip: a selected canvas equal (32x32 grayscale,
// strided sampling, tolerance 8) to the previously ADDED canvas is
// dropped — a looping gif emitting fewer than k frames is correct.
// Canvas comparison (vs. the pre-fix per-frame comparison) is more
// correct for delta animation: two DIFFERENT delta frames that
// produce the same full canvas state collapse to one.
//
// Encode: each kept canvas is image/jpeg at gifJpegQuality. An encode
// error for a frame skips that frame (counted in returned `skipped` so
// the caller logs it — THIS function is pure, it returns only data).
//
// Degradation contract (status quo): errGIFTooLarge / errSingleFrame /
// errGIFUnreadable all mean "send the raw bytes"; the cap is a
// DISTINCT sentinel — different log semantics (budget refusal vs.
// decode failure) — so the caller can log the size.
func expandGIFFrames(data []byte) (frames []app.PiImage, skipped int, err error) {
	if len(data) > gifInputMaxBytes {
		return nil, 0, errGIFTooLarge
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(g.Image) == 0 {
		return nil, 0, errGIFUnreadable
	}
	if len(g.Image) == 1 {
		return nil, 0, errSingleFrame
	}
	decoded := g.Image
	if len(decoded) > gifDecodeCap {
		decoded = decoded[:gifDecodeCap]
	}
	// Persistent compositing canvas ("keep previous" default disposal).
	// Size = the union of all collected frames' in-canvas bounds — a
	// superset of the gif's logical screen size for in-bounds frames
	// (the frames are emitted to the logical screen, so their bounds
	// span it; DrawConfig-style size is therefore not needed.
	// Off-origin bounds are retained by this toolchain's DecodeAll —
	// see the compositing paragraph above and the delta test that
	// re-asserts it).
	maxX, maxY := 0, 0
	for _, f := range decoded {
		b := f.Bounds()
		if b.Max.X > maxX {
			maxX = b.Max.X
		}
		if b.Max.Y > maxY {
			maxY = b.Max.Y
		}
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, maxX, maxY))
	var lastCanvas image.Image // the previously ADDED canvas (snapshot)

	n := len(decoded)
	k := gifOutputFrames
	if k > n {
		k = n
	}
	var selected []*image.Paletted
	for i := 0; i < k; i++ {
		if k > 1 {
			selected = append(selected, decoded[i*(n-1)/(k-1)])
		} else {
			selected = append(selected, decoded[0])
		}
	}
	for _, frame := range selected {
		// This toolchain's DecodeAll returns frames in CANNOT canvas
		// coordinates — a delta frame's Bounds().Min IS its in-canvas
		// offset. draw.Draw(dst, r, src, sp, op) sticks the src onto the
		// dst at translation sp (dst(x, y) ← src(x+sp.X, y+sp.Y)), so
		// sp = Point{} aligns each frame pixel with its own in-canvas
		// position; a full-canvas frame covers the whole canvas (draw.Over
		// re-primes it), a sub-rectangle delta frame overlays its partial
		// rectangle, and transparent palette entries (alpha 0) leave the
		// canvas beneath untouched (draw.Over — the GIF "keep previous"
		// default).
		draw.Draw(canvas, canvas.Bounds(), frame, image.Point{}, draw.Over)
		if lastCanvas != nil && sameFrame32(lastCanvas, canvas) {
			// Consecutive duplicate (a loop-internal repeat) — drop
			// the later one; no window shift, just skip it.
			continue
		}
		buf := &bytes.Buffer{}
		if err := jpeg.Encode(buf, canvas, &jpeg.Options{Quality: gifJpegQuality}); err != nil {
			skipped++
			continue
		}
		frames = append(frames, app.PiImage{
			MimeType: "image/jpeg",
			Data:     base64.StdEncoding.EncodeToString(buf.Bytes()),
		})
		// Snapshot the canvas for the next frame's duplicate check.
		snap := image.NewNRGBA(canvas.Bounds())
		draw.Draw(snap, snap.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
		lastCanvas = snap
	}
	return frames, skipped, nil
}

// sameFrame32: cheap grayscale equivalence of two images — strided
// sampling of the pixel grid, 32x32 samples extracted proportionally
// (sample s lands at offset s*dim/32 — the step is max(1, dim/32); NO
// full pixel copy), compared with a tolerance of 8 on the 8-bit luma.
// All-equal -> duplicate. A frame whose grid does not line up
// (mismatched bounds) is not a duplicate. Takes image.Image so the
// duplicate check runs on the COMPOSITED canvases (more correct for
// delta-animation dupes than pre-composite per-frame comparison — see
// expandGIFFrames).
func sameFrame32(a, b image.Image) bool {
	ab, bb := a.Bounds(), b.Bounds()
	for sy := 0; sy < 32; sy++ {
		y := ab.Min.Y + (sy*ab.Dy())/32
		if y > bb.Max.Y-1 {
			return false
		}
		for sx := 0; sx < 32; sx++ {
			x := ab.Min.X + (sx*ab.Dx())/32
			if x > bb.Max.X-1 {
				return false
			}
			d := luma8(a.At(x, y)) - luma8(b.At(x, y))
			if d > 8 || d < -8 {
				return false
			}
		}
	}
	return true
}

// luma8 — 8-bit luma (0..255) of 16-bit RGBA components (0..65535);
// widen to uint64 before the luma weighted sum (the components are uint32
// — an un-widened multiply wraps).
func luma8(c color.Color) int {
	r, g, b, _ := c.RGBA()
	sum := uint64(r)*299 + uint64(g)*587 + uint64(b)*114
	return int(sum / 257000)
}
