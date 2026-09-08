package pirpc

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"log/slog"
	"os"
	"testing"
)

func solidPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Flat mid-grey: cheap to fill, deterministic; the large dimensions
	// trigger the shrink path without a multi-MB fixture.
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 128, 128, 128, 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// noisePNG returns a deterministic xorshift-noise PNG — a stand-in for a
// real photo: incompressible enough that PNG carries near-raw size and
// JPEG can genuinely shrink it.
func noisePNG(w, h, seed int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := uint32(seed)
	for i := 0; i < len(img.Pix); i += 4 {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(s), byte(s>>8), byte(s>>16), 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func imFor(raw []byte) Image {
	return Image{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestShrinkImagesDedupesIdentical(t *testing.T) {
	raw := solidPNG(4000, 3000)
	in := []Image{imFor(raw), imFor(raw), imFor(raw)}
	out := shrinkImages(discardLog(), in)
	if len(out) != 1 {
		t.Fatalf("expected 1 image after dedupe, got %d", len(out))
	}
}

func TestShrinkOneShrinksLargeImage(t *testing.T) {
	raw := noisePNG(2049, 2049, 42) // photo-like: ~raw PNG size, oversized dimension
	out := shrinkOne(imFor(raw))
	if out.MimeType != "image/jpeg" {
		t.Fatalf("expected re-encode to image/jpeg, got %s", out.MimeType)
	}
	rawOut, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		t.Fatalf("shrunken data is not valid base64: %v", err)
	}
	if len(rawOut) >= len(raw) {
		t.Fatalf("expected shrunken image smaller than original: %d >= %d", len(rawOut), len(raw))
	}
	pic, _, err := image.Decode(bytes.NewReader(rawOut))
	if err != nil {
		t.Fatalf("shrunken image does not decode: %v", err)
	}
	b := pic.Bounds()
	if b.Dx() > maxSide || b.Dy() > maxSide {
		t.Fatalf("expected dimensions <= %d, got %dx%d", maxSide, b.Dx(), b.Dy())
	}
}

func TestShrinkOneKeepsFlatImageJpegCannotBeat(t *testing.T) {
	// A flat image PNG-compresses to a few KB; a flat JPEG at q80 is
	// worse — the never-make-it-worse guard must keep the original.
	in := imFor(solidPNG(4000, 3000))
	if out := shrinkOne(in); out != in {
		t.Fatal("flat image the JPEG cannot beat must be kept as-is")
	}
}

func TestShrinkOnePassesThroughSmallImage(t *testing.T) {
	raw := solidPNG(640, 480) // well under minShrinkBytes and maxSide
	in := imFor(raw)
	out := shrinkOne(in)
	if out != in {
		t.Fatalf("small image must pass through untouched")
	}
}

func TestShrinkImageKeepsOriginalOnBadData(t *testing.T) {
	in := Image{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 1024))}
	out := shrinkOne(in)
	if out != in {
		t.Fatalf("undecodable image must be kept as-is")
	}
}

func TestShrinkImagesEnforcesBudget(t *testing.T) {
	// 10 unique undecodable images, each 2MB raw (2.7MB base64): only
	// 6 fit the 12MB raw budget — the rest must be dropped, in order.
	var in []Image
	for i := 0; i < 10; i++ {
		raw := append(bytes.Repeat([]byte{0x07}, (2<<20)-1), byte(i)) // 2MB, unique per image
		in = append(in, Image{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)})
	}
	out := shrinkImages(discardLog(), in)
	if len(out) == 10 {
		t.Fatal("expected the budget to drop some images")
	}
	var total int
	for _, im := range out {
		total += len(im.Data) * 3 / 4
	}
	if total > totalBudgetRaw {
		t.Fatalf("budget exceeded: %d > %d", total, totalBudgetRaw)
	}
	// Order must be preserved: the kept set is a prefix.
	for i, im := range out {
		if im.Data != in[i].Data {
			t.Fatalf("order not preserved at index %d", i)
		}
	}
}
