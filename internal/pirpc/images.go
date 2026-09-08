package pirpc

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"math"

	draw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the webp decoder with image.Decode
)

// Per-ask image budget. Discord allows 10 attachments per message (25 with
// boost), each up to ~8MB, so an unbounded ask can carry >200MB of base64 —
// a single vision ask that a one-at-a-time pi agent cannot chew (observed
// dead-end: 10× 4.6MB attachments → ~46MB prompt → empty agent_end). The
// dedupe/shrink/cap pipeline (shrinkImages) keeps any ask inside these
// bounds.
const (
	maxSide        = 2048     // longest dimension after the resize
	jpegQuality    = 80       // re-encode quality
	minShrinkBytes = 1 << 20  // images at/below this (and ≤ maxSide) pass through untouched
	totalBudgetRaw = 12 << 20 // per-ask cap on raw bytes (→ ~16MB base64 on the wire)
)

// shrinkOne decodes one image; if it exceeds the size/dimension budget it is
// resized to maxSide (only shrinks, never upscales) and re-encoded as JPEG.
// Any decode/encode failure keeps the original (mime, data) — an unreadable
// image is still more information to the agent than no image.
func shrinkOne(im Image) Image {
	raw, err := base64.StdEncoding.DecodeString(im.Data)
	if err != nil {
		return im
	}
	pic, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return im
	}
	b := pic.Bounds()
	w, h := b.Dx(), b.Dy()
	if len(raw) <= minShrinkBytes && w <= maxSide && h <= maxSide {
		return im // small enough — skip the decode cost of a resize
	}
	nw, nh := w, h
	if w > maxSide || h > maxSide {
		scale := math.Min(float64(maxSide)/float64(w), float64(maxSide)/float64(h))
		nw = max(1, int(math.Round(float64(w)*scale)))
		nh = max(1, int(math.Round(float64(h)*scale)))
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), pic, pic.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return im
	}
	// Never make an image worse: if the re-encoded JPEG is at least as
	// large, keep the original (the per-ask budget cap still applies).
	if buf.Len() >= len(raw) {
		return im
	}
	return Image{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}
}

// shrinkImages is the per-ask image pipeline: dedupe byte-identical
// attachments (re-uploads of the same file), shrink the oversized ones, and
// cap the total so a single ask can never exceed totalBudgetRaw of raw bytes.
// Images are kept in their original order; drops are logged.
func shrinkImages(log *slog.Logger, imgs []Image) []Image {
	seen := make(map[[32]byte]struct{}, len(imgs))
	out := make([]Image, 0, len(imgs))
	var (
		droppedDup int
		droppedCap int
		total      int
	)
	for _, im := range imgs {
		sum := sha256.Sum256([]byte(im.Data))
		if _, ok := seen[sum]; ok {
			droppedDup++
			continue
		}
		seen[sum] = struct{}{}
		shr := shrinkOne(im)
		rawLen := len(shr.Data) * 3 / 4
		if total+rawLen > totalBudgetRaw {
			droppedCap++
			continue
		}
		total += rawLen
		out = append(out, shr)
	}
	if droppedDup > 0 {
		log.Info(fmt.Sprintf("dropped %d duplicate attachment image(s) (identical content)", droppedDup), "module", Module)
	}
	if droppedCap > 0 {
		log.Warn(fmt.Sprintf("dropped %d image(s) over the %d-byte per-ask budget", droppedCap, totalBudgetRaw), "module", Module)
	}
	return out
}

// (PiRpc delegates)
func (p *PiRpc) shrinkImages(imgs []Image) []Image {
	return shrinkImages(p.log, imgs)
}
