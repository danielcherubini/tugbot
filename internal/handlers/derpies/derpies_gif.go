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
	"image/gif"
	"image/jpeg"

	"github.com/danielcherubini/tugbot/internal/app"
)

const (
	gifOutputFrames = 8  // max frames emitted per gif
	gifDecodeCap    = 60 // max frames walked from the stream
	gifJpegQuality  = 80 // house quality
)

var (
	// errSingleFrame: the bytes decode to 0 or 1 frame — the caller
	// sends the RAW bytes instead (the status-quo raw send; the
	// first-frame flattening is then the pipeline's business, exactly
	// as today).
	errSingleFrame = errors.New("single-frame gif")
	// errGIFUnreadable: the bytes cannot be reliably decoded as a gif
	// — the caller sends the RAW bytes instead (degrade, never lose the
	// payload).
	errGIFUnreadable = errors.New("gif unreadable")
)

// expandGIFFrames expands an uploaded animated-gif byte slice into at
// most gifOutputFrames evenly-spaced JPEG frames, in-process. This
// toolchain's stdlib image/gif is all-or-nothing (gif.DecodeAll reads
// the whole stream; there is no streaming Decoder), so any stream error
// — first frame or mid-stream — lands in errGIFUnreadable.
//
// Selection: k = min(gifOutputFrames, n) evenly-spaced indices over the
// collected frames (i * (n-1) / (k-1) for k > 1 — the same
// even-spacing math as the video task); at most gifDecodeCap frames
// walk from the stream (the rest of a long gif is never touched).
//
// Consecutive-duplicate skip: a selected frame equal (32x32 grayscale,
// strided sampling, tolerance 8) to the previously ADDED frame is
// dropped — a looping gif emitting fewer than k frames is correct.
//
// Encode: each kept frame is image/jpeg at gifJpegQuality. An encode
// error for a frame skips that frame (counted in returned `skipped` so
// the caller logs it — THIS function is pure, it returns only data).
//
// Degradation contract (status quo): errSingleFrame /
// errGIFUnreadable mean "send the raw bytes".
func expandGIFFrames(data []byte) (frames []app.PiImage, skipped int, err error) {
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
	var last *image.Paletted
	for _, frame := range selected {
		if last != nil && sameFrame32(last, frame) {
			// Consecutive duplicate (a loop-internal repeat) — drop
			// the later one; no window shift, just skip it.
			continue
		}
		buf := &bytes.Buffer{}
		if err := jpeg.Encode(buf, frame, &jpeg.Options{Quality: gifJpegQuality}); err != nil {
			skipped++
			continue
		}
		frames = append(frames, app.PiImage{
			MimeType: "image/jpeg",
			Data:     base64.StdEncoding.EncodeToString(buf.Bytes()),
		})
		last = frame
	}
	return frames, skipped, nil
}

// sameFrame32: cheap grayscale equivalence of two frames — strided
// sampling of the pixel grid, 32x32 samples extracted proportionally
// (sample s lands at offset s*dim/32 — the step is max(1, dim/32); NO
// full pixel copy), compared with a tolerance of 8 on the 8-bit luma.
// All-equal -> duplicate. A frame whose grid does not line up
// (mismatched bounds) is not a duplicate.
func sameFrame32(a, b *image.Paletted) bool {
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
