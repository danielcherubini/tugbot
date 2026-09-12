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
	gifOutputFrames     = 8        // max frames emitted per gif
	gifDecodeCap        = 60       // max frames walked from the stream
	gifJpegQuality      = 80       // house quality
	gifInputMaxBytes    = 16 << 20 // pre-decode input cap (16MB)
	gifMaxDecodedPixels = 16 << 20 // pre-decode decoded-dimension cap (16MP logical screen)
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
	// errGIFDimsTooLarge: the LOGICAL SCREEN (logged via gif.DecodeConfig
	// — the global header alone, no frame materialization) exceeds
	// gifMaxDecodedPixels BEFORE any full decode — a budget refusal on
	// the DECODED dimensions; a DISTINCT sentinel from errGIFTooLarge
	// (input bytes) because the input bytes can be tiny (a uniform
	// large-screen gif compresses to KBs on disk) and the caller's
	// degrade log must name WHICH bound fired (input bytes vs. decoded
	// dimensions); the caller sends the RAW bytes instead (same
	// degrade shape as the other gif refusal arms — log, never abort).
	errGIFDimsTooLarge = errors.New("gif decoded dimensions over the pre-decode cap")
	// errGIFUnreadable: the bytes cannot be reliably decoded as a gif
	// — the caller sends the RAW bytes instead (degrade, never lose the
	// payload).
	errGIFUnreadable = errors.New("gif unreadable")
)

// expandGIFFrames expands an uploaded animated-gif byte slice into at
// most gifOutputFrames evenly-spaced JPEG frames, in-process.
//
// Input cap (pre-decode): len(data) > gifInputMaxBytes rejects with
// errGIFTooLarge BEFORE any decode. Decoded-dimension cap
// (pre-decode): a logical screen of more than gifMaxDecodedPixels
// (16MP — read via gif.DecodeConfig, the global HEADER ALONE, no
// frame materialization) rejects with the DISTINCT sentinel
// errGIFDimsTooLarge BEFORE gif.DecodeAll runs — a uniform
// large-screen gif compresses to KBs on disk, so the input-byte cap
// alone cannot bound the decode work of an in-bounds-bytes
// transmission. A DecodeConfig ERROR is NO HARD REJECTION (a
// malformed / truncated header is not a budget ground) — the decode
// falls through to the DecodeAll path and lands in errGIFUnreadable
// (or errSingleFrame). Residual constraint (documented): this
// toolchain's image/gif is all-or-nothing (no streaming NewDecoder),
// so the bounds are on the INPUT bytes and the INPUT logical
// screen, not the decode work — DecodeAll still materializes EVERY
// frame of an in-bounds gif, and a fully-uncompressed large-frame
// gif at the caps still costs proportional decode work. The
// pre-decode caps are the bounded option this toolchain offers
// (input bounded to ≤16MB, a 16MP logical screen bounds the per-
// walked-frame canvas — worst case 60 walked frames ≈ 960MB of canvas
// materialization — and a crafted gif's decoded memory is
// proportionally bounded, not arbitrary). Any stream error — first
// frame or mid-stream — lands in errGIFUnreadable.
//
// Selection: k = min(gifOutputFrames, n) evenly-spaced indices over
// the collected frames (i * (n-1) / (k-1) for k > 1 — the same
// even-spacing math as the video task). These indices pick which
// walked canvas states get snapshotted and encoded; every other
// frame is still walked (with its disposal honored) so the selected
// canvases composite onto the true animation state. At most
// gifDecodeCap frames walk from the stream (the rest of a long gif
// is never touched).
//
// Compositing (full replay with disposal — the point of this fix):
// a gif encoded as full-canvas frames followed by SUB-RECTANGLE delta
// frames (partial frame bounds with transparent-pixel animations)
// decodes here as sub-rectangles RETAINING their in-canvas offset in
// Bounds().Min (verified empirically and re-asserted by
// TestExpandGIFFramesDeltaComposited). Encoding a delta frame AS-IS
// would hand the model an incomplete sub-rectangle and hide the
// burned-in gimmick text this feature exists to catch. The walk
// therefore REPLAYS the animation with default gif semantics: ALL
// collected frames (bounded: at most gifDecodeCap are walked) are
// drawn IN INDEX ORDER onto the persistent *image.NRGBA canvas —
// a full-canvas frame re-primes it (draw.Over, full coverage), a
// sub-rectangle delta overlays its partial rectangle, transparent
// palette entries (alpha 0) leave the canvas beneath untouched
// (draw.Over, the "keep previous" default). Drawing EVERY frame —
// not just the selected ones — matters for delta animation: an
// UNSELECTED intermediate frame can modify the canvas, and a later
// selected delta, being relative to that intermediate state, must
// composite onto a canvas that HAS the intermediate change
// (TestExpandGIFFramesWalksUnselectedDeltaFrames); skipping it would
// hand the model a canvas that was never visible during playback. AFTER
// each frame is drawn, its DISPOSAL (g.Disposal[i], one entry per
// frame as this toolchain's DecodeAll returns it) is applied before
// the next frame draws: gif.DisposalBackground (0x02, spec "restore
// to background") clears ONLY the just-drawn frame's OWN rectangle
// to blank — CLEARED BY DIRECT PER-ROW SPAN ZERO of the parent
// canvas's own Pix (subscript-bounded to the rectangle; INDEPENDENT
// of SubImage semantics — on this toolchain
// *image.NRGBA.SubImage returns a Pix slice backed to the PARENT
// buffer's remaining tail, so ranging over SubImage().Pix clears
// pixels OUTSIDE the frame rectangle — the in-toolchain Pix-tail
// caveat is noted in the code comment and the clear sidesteps
// SubImage entirely) — a transparent wipe over the frame's bounds
// (the gif screen background is transparent on this NRGBA canvas,
// and a full-canvas wipe would lose content a previous frame
// retained OUTSIDE the frame's bounds;
// TestExpandGIFFramesBackgroundDisposalKeepsOutsideContent, which
// now ALSO regression-asserts that rows/columns OUTSIDE the frame
// rectangle are untouched — the old SubImage-Pix-tail clear loses
// them). gif.DisposalPrevious (0x03) restores the canvas
// to the state BEFORE that frame was drawn; everything else
// (gif.DisposalNone 0x01, the raw 0x00 read-back of an unset flag,
// and reserved 0x04+ values) keeps the canvas (the gif default).
// The restore is bounded on memory: the canvas is
// snapshotted exactly ONCE before each frame's draw (one NRGBA
// copy, at most one live at a time, freed each iteration), and
// DisposalPrevious restores that immediately-prior snapshot. A full
// disposal-chain walk (how the spec resolves a Previous chain —
// walking back through earlier frames' dispositions to their
// canonical state) would require the whole frame history (one
// snapshot per frame, up to gifDecodeCap full-canvas copies); the
// one-step restore is the bounded behavior here and is spec-correct
// for the common single-Previous pair (a full-cover frame +
// DisposalPrevious over a full prior canvas)
// (TestExpandGIFFramesDisposalPreviousRestoresCanvas). A
// selected frame's canvas is captured in the state the frame WAS
// DISPLAYED IN (its disposal applies only to what the NEXT frame
// draws onto), and the consecutive-duplicate skip (a selected
// canvas equal (32x32 grayscale, strided sampling, tolerance 8) to
// the previously ADDED canvas is dropped — a looping gif emitting
// fewer than k frames is correct) collapses any loop-internal
// repeats.
// Canvas size = the union of all collected frames' bounds (a
// superset of the decoded logical screen size for in-bounds
// frames). The JPEG-encoded is the CANVAS, not the bare frame.
//
// Encode: each kept canvas is image/jpeg at gifJpegQuality. An encode
// error for a frame skips that frame (counted in returned `skipped` so
// the caller logs it — THIS function is pure, it returns only data).
//
// Degradation contract (status quo): errGIFTooLarge /
// errGIFDimsTooLarge / errSingleFrame / errGIFUnreadable all mean
// "send the raw bytes" (log, never abort); the bytes cap and the
// DIMENSIONS cap are DISTINCT sentinels — different log semantics
// (which budget refusal fired vs. decode failure) — so the caller
// can log which bound tripped.
func expandGIFFrames(data []byte) (frames []app.PiImage, skipped int, err error) {
	if len(data) > gifInputMaxBytes {
		return nil, 0, errGIFTooLarge
	}
	// PRE-DECODE DIMENSION BOUND: gif.DecodeConfig reads the global
	// HEADER ALONE (the logical screen size — no frame
	// materialization) and is cheap even in a large file. A
	// DecodeConfig ERROR is NOT a rejection ground (malformed /
	// truncated header) — it falls to the full DecodeAll path and
	// lands in errGIFUnreadable there. ONLY W×H exceeding the pixel
	// cap is a budget refusal with its own sentinel; a 0×0 header is
	// NOT rejected — only the `> gifMaxDecodedPixels` comparison
	// exists (the DecodeAll path handles it, as it handles any other
	// in-bounds case).
	if cfg, err := gif.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width*cfg.Height > gifMaxDecodedPixels {
		return nil, 0, errGIFDimsTooLarge
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
	// Selection — which walked frames' canvas states get snapshotted
	// (the even-spacing math, now a boolean mask over the walk rather
	// than a pre-walk frame list).
	selected := make([]bool, n)
	if k > 1 {
		for i := 0; i < k; i++ {
			selected[i*(n-1)/(k-1)] = true
		}
	} else {
		selected[0] = true
	}
	for i := 0; i < n; i++ {
		// Bounded (one canvas-sized snapshot, at most one live at a
		// time — it goes out of scope each iteration): snapshot the
		// canvas as it stands BEFORE frame i is drawn. This pre-draw
		// state is exactly what frame i's DisposalPrevious restores
		// to — the bounded one-step restore (NOT the spec's full
		// disposal-chain walk, which would need the whole frame
		// history; see the doc comment above).
		prev := image.NewNRGBA(canvas.Bounds())
		draw.Draw(prev, prev.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
		frame := decoded[i]

		// This toolchain's DecodeAll returns frames in CANNOT canvas
		// coordinates — a delta frame's Bounds().Min IS its in-canvas
		// offset. draw.Draw(dst, r, src, sp, op) sticks the src onto the
		// dst at translation sp (dst(x, y) ← src(x+sp.X, y+sp.Y)), so
		// sp = Point{} aligns each frame pixel with its own in-canvas
		// position; a full-canvas frame covers the whole canvas (draw.Over
		// re-primes it), a sub-rectangle delta frame overlays its partial
		// rectangle, and transparent palette entries (alpha 0) leave the
		// canvas beneath untouched (draw.Over — the "keep previous"
		// default). EVERY frame is drawn, not just the selected ones —
		// see the compositing paragraph above.
		draw.Draw(canvas, canvas.Bounds(), frame, image.Point{}, draw.Over)

		// A selected frame's canvas is captured NOW — the canvas as
		// DISPLAYED (the spec captures the frame state before its
		// disposal applies; the disposal only changes what the NEXT
		// frame draws onto). This is the pre-disposal capture point.
		if selected[i] && lastCanvas != nil && sameFrame32(lastCanvas, canvas) {
			// Consecutive duplicate (a loop-internal repeat) — drop
			// the later one; no window shift, just skip it.
			// (No early `continue` here: frame i's disposal below
			// must still apply before frame i+1 draws.)
		} else if selected[i] {
			buf := &bytes.Buffer{}
			if err := jpeg.Encode(buf, canvas, &jpeg.Options{Quality: gifJpegQuality}); err != nil {
				skipped++
			} else {
				frames = append(frames, app.PiImage{
					MimeType: "image/jpeg",
					Data:     base64.StdEncoding.EncodeToString(buf.Bytes()),
				})
				// Snapshot the canvas for the next frame's duplicate check.
				snap := image.NewNRGBA(canvas.Bounds())
				draw.Draw(snap, snap.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
				lastCanvas = snap
			}
		}
		// Apply frame i's disposal AFTER the frame's display was
		// captured, BEFORE frame i+1 draws. Per go doc
		// image/gif: DisposalNone (0x01) keeps; the raw 0x00 (an unset
		// flag — this toolchain's encoder writes disposal 0 for an
		// absent/zero entry, and the decoder reads it back as 0 rather
		// than padding it to 1) and the reserved 0x04+ values are all
		// "do nothing" per the gif spec, i.e. keep too. So: only 0x02
		// and 0x03 act.
		d := byte(0)
		if i < len(g.Disposal) {
			d = g.Disposal[i]
		}
		switch d {
		case gif.DisposalBackground: // 0x02 — spec: restore to background = clear the frame's own area.
			// Clear ONLY this frame's rectangle to blank (the previous
			// full-canvas replacement lost content frames retained
			// OUTSIDE the frame's bounds). This toolchain's
			// draw.Draw does not erase with a transparent source
			// pixel under either Src or Over (verified empirically —
			// alpha-0 contributes nothing: Src even leaves the
			// destination OPAQUE), so the rectangle is zeroed
			// directly: transparent is this NRGBA canvas's "background
			// color" (it has none of its own). The manual
			// intersection guards frames extending past the canvas
			// bounds (an empty rect needs no zeroing — defensive, no
			// panic). canvas is a *image.NRGBA throughout (it starts
			// as one; DisposalPrevious restores the NRGBA snapshot
			// `prev`). The clear is DIRECT PER-ROW SPAN ZERO of the
			// canvas's own Pix — NOT canvas.SubImage(rect).Pix: on
			// this toolchain's *image.NRGBA.SubImage returns a Pix
			// slice backed to the PARENT buffer's remaining tail
			// (confirmed by an in-toolchain caveat, and empirically
			// — ranging over SubImage().Pix zeroed pixels outside the
			// rectangle), so the clear is subscript-bounded to the
			// rectangle itself, independent of SubImage's Pix-tail
			// semantics. (The helper intersects once more — double
			// safety, both are cheap — and this toolchain's
			// image.Rectangle has no Inter method, so the intersect is
			// spelled out).
			rectClearNRGBA(canvas, canvas.Bounds().Intersect(frame.Bounds()))
		case gif.DisposalPrevious: // 0x03 — restore.
			// Restore to the canvas state before frame i was drawn —
			// the immediately-prior snapshot, the bounded one-step
			// behavior (NOT the spec's full disposal-chain walk, which
			// would require the whole frame history; see the doc
			// comment). Spec-correct for the common single-Previous
			// pair (a full-cover frame + DisposalPrevious over a full
			// prior canvas).
			canvas = prev
		default: // DisposalNone + raw 0x00 + reserved 0x04+ = keep.
			// Nothing to do — the canvas stands as drawn.
		}
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

// rectClearNRGBA clears the exact rectangle r (intersected with m's
// bounds) to transparent by zeroing each row's pixel span directly in
// m.Pix — subscript-bounded to r, independent of SubImage's Pix-tail
// semantics (which on this toolchain extend to the parent buffer's
// end).
func rectClearNRGBA(m *image.NRGBA, r image.Rectangle) {
	r = r.Intersect(m.Rect)
	if r.Empty() {
		return
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		off := (y-m.Rect.Min.Y)*m.Stride + (r.Min.X-m.Rect.Min.X)*4
		n := (r.Max.X - r.Min.X) * 4
		for i := off; i < off+n; i++ {
			m.Pix[i] = 0
		}
	}
}

// luma8 — 8-bit luma (0..255) of 16-bit RGBA components (0..65535);
// widen to uint64 before the luma weighted sum (the components are uint32
// — an un-widened multiply wraps).
func luma8(c color.Color) int {
	r, g, b, _ := c.RGBA()
	sum := uint64(r)*299 + uint64(g)*587 + uint64(b)*114
	return int(sum / 257000)
}
