// Package derpies — gifv embed-video frame expansion (govid, pure Go — no cgo,
// no ffmpeg binary).
//
// The two observed survival clips (Klipy gifv embeds) carry the actual
// animation ONLY in MessageEmbed.Video (an mp4/webm on the provider's static
// CDN; the clip lives there, not in the thumbnail). This leg downloads that
// video (<=8MB, the same isSafeURL guard, a 10s client) and decodes it with
// github.com/liqmix/govid by SEEK-sampling <=8 frames (keyframe-accurate; the
// demuxers expose Seek), with a bounded sequential fallback. Per-sample
// failures degrade (log + skip) and NEVER abort the flow.
//
// Container selection is a magic-byte sniff, LENGTH-GUARDED (a truncated CDN
// body or an HTML error page must never panic the adversarial-input filter):
//
//	ftyp at offset 4            -> mp4  + H.264 (h264 codec)
//	EBML 0x1A45DFA3 at offset 0 -> webm + VP8  (vp8 codec)
//	anything else              -> errUnsupported (the decoder caller warns + skips)
//
// The decoder sits behind the videoFrameDecoder seam exactly like the clock/ops
// seams: New() wires govidVideoFrames (the real thing); tests wire a fake or
// leave it nil. A nil decoder or ANY decoder error means "no frames for that
// video" (slog.Warn, module derpies, degrade to thumbnail-only, never abort).
//
// Plan decision (documented by the header, per the task): the gifv video plan
// entries live INLINE in imageURLPlan (derpies.go) — one plan, one URL-dedup
// union, one download pass — rather than a parallel videoURLPlan. A video
// entry carries source == "embed-video" and an IRRELEVANT mime (""), because
// govidVideoFrames sniffs the container from the bytes; the flow's existing
// seenURLs dedup already prevents double-sending the same video (posted +
// referenced).
package derpies

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/liqmix/govid"
	"github.com/liqmix/govid/h264"
	"github.com/liqmix/govid/mp4"
	"github.com/liqmix/govid/vp8"
	"github.com/liqmix/govid/webm"

	"github.com/danielcherubini/tugbot/internal/app"
)

const (
	videoMaxBytes         = 8 << 20 // 8MB hard cap on a downloaded video
	videoOutputFrames     = 8       // max seek-sampled frames emitted per video
	videoSequentialCap    = 600     // max packets walked in the sequential fallback
	videoJpegQuality      = 80      // house JPEG quality
	videoSeekDecodeWindow = 32      // max packets to walk after a seek before falling back
)

var (
	// errUnsupported: the byte payload is not a recognized mp4/webm container —
	// the caller warns + skips (degrade, never panic).
	errUnsupported = errors.New("unsupported video container")
	// errNoPixels: a frame with nil/empty YCbCr planes (a documented govid
	// partial-frame shape) — skipped in the picker, never encoded.
	errNoPixels = errors.New("frame carries no usable pixel planes")
)

// eBMLHeader is the EBML magic (the WebM container signature at offset 0).
var eBMLHeader = []byte{0x1A, 0x45, 0xDF, 0xA3}

// videoDemuxer / videoCodec are the minimal method sets the mp4+webm demuxers
// and the h264+vp8 codecs share, so the seek-sample + fallback + encode legs are
// written ONCE and only the demuxer/codec construction branches per container.
// Both *mp4.Demuxer and *webm.Demuxer satisfy videoDemuxer; both *h264.Codec
// and *vp8.Codec satisfy videoCodec.
type videoDemuxer interface {
	Seek(t time.Duration) (time.Duration, error)
	Duration() time.Duration
	VideoInfo() govid.VideoInfo
	NextPacket() (govid.Packet, error)
	Close() error
}

type videoCodec interface {
	Decode(pkt govid.Packet) (*govid.Frame, error)
	Flush()
}

// downloadVideoBytes does ONE GET of url (the exact http.NewRequestWithContext
// pattern as the image leg's downloadPlan), on the explicit-timeout client, and
// reads the body within an 8MB cap.
//
// It deviates from the image leg's downloadPlan INTENTIONALLY (mention parity
// there reads and ships whatever body an error status carries, because an error
// page is at least image-ish bytes worth showing): a NON-2xx STATUS IS AN ERROR
// here and the body is NOT read — a 4xx/5xx video body can never be a decodable
// clip, so it must not reach the decoder. This is what makes a 404 video and an
// over-cap Content-Length both pass NOTHING to the decoder (a 404 never, and an
// over-cap body never read at all).
//
//   - Content-Length > videoMaxBytes -> error BEFORE any read.
//   - a non-2xx status -> error WITHOUT reading the body.
//   - the body is read with io.LimitReader(resp.Body, videoMaxBytes+1); a body
//     LONGER than videoMaxBytes (the Content-Length lied) is an error.
func downloadVideoBytes(ctx context.Context, url string, client *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Intentional deviation from the image leg: a non-2xx is an error and the
	// body is never read (a 4xx/5xx body is never a decodable clip).
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("video download status %d is not 2xx", resp.StatusCode)
	}
	// The Content-Length cap short-circuits BEFORE the read (defense when the
	// server announces an over-cap size).
	if resp.ContentLength > videoMaxBytes {
		return nil, fmt.Errorf("video content-length %d exceeds the %d-byte cap", resp.ContentLength, videoMaxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, videoMaxBytes+1))
	if err != nil {
		return nil, err
	}
	// Defense when Content-Length LIES (announces a small size, streams more).
	if len(body) > videoMaxBytes {
		return nil, fmt.Errorf("video body length %d exceeds the %d-byte cap", len(body), videoMaxBytes)
	}
	return body, nil
}

// govidVideoFrames is the REAL frame decoder behind the videoFrameDecoder seam.
// It sniffs the container (length-guarded), builds the matching demuxer + codec,
// and seek-samples <=8 JPEG frames. A non-mp4/webm payload returns errUnsupported
// (the caller warns + skips, degrades, never panics). A demuxer-construction
// error is propagated to the caller (which also warns + skips). A successfully
// sniffed container that yields ZERO frames is NOT an error — it degrades to
// whatever was got (an empty slice, nil).
func govidVideoFrames(data []byte) ([]app.PiImage, error) {
	// Length-guarded sniff: a truncated body or an HTML error page must
	// never panic. ftyp at offset 4 -> mp4/H.264; EBML at offset 0 -> webm/VP8.
	if len(data) >= 8 && bytes.Equal(data[4:8], []byte("ftyp")) {
		return decodeVideoFrames(data, "mp4")
	}
	if len(data) >= 4 && bytes.Equal(data[0:4], eBMLHeader) {
		return decodeVideoFrames(data, "webm")
	}
	return nil, errUnsupported
}

// decodeVideoFrames runs the one shared seek-sample pass for whichever demuxer +
// codec the container selected. It is the single seek-sampled-frame algorithm:
//
//	k = min(videoOutputFrames, max(1, int(dur.Seconds()*fps)+1)), fps guarded.
//
// For each sample i in [0, k): target t = (i + 0.5)/k * dur; Seek(t); then
// codec.Flush() IMMEDIATELY after the seek resolves (govid documents Flush as the
// post-seek reset — it discards the H.264 reorder buffer and resets decoder
// state; WITHOUT it the first frame after a seek is a STALE buffered frame from
// the previous segment and SPS/ref state carries across); then walk up to
// videoSeekDecodeWindow packets, decoding, and keep the first usable frame at >= t
// (or the first usable frame when timestamps are zero). If the seek-window yields
// nothing, a bounded sequential fallback (up to videoSequentialCap packets from the
// previous stop position) takes the last usable frame before the cap. A sample
// that still yields nothing is skipped (log, no error).
func decodeVideoFrames(data []byte, container string) ([]app.PiImage, error) {
	var d videoDemuxer
	var c videoCodec
	switch container {
	case "mp4":
		m, err := mp4.NewDemuxer(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("mp4 demux: %w", err)
		}
		d, c = m, h264.NewCodec()
	case "webm":
		w, err := webm.NewDemuxer(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("webm demux: %w", err)
		}
		d, c = w, vp8.NewCodec()
	default:
		return nil, errUnsupported
	}
	defer func() { _ = d.Close() }()

	fps := d.VideoInfo().FrameRate
	if fps <= 0 {
		fps = 16.67
	}
	dur := d.Duration()
	k := int(dur.Seconds()*fps) + 1
	if k < 1 {
		k = 1
	}
	if k > videoOutputFrames {
		k = videoOutputFrames
	}

	var out []app.PiImage
	for i := 0; i < k; i++ {
		frac := (float64(i) + 0.5) / float64(k)
		t := time.Duration(frac * float64(dur))
		if _, err := d.Seek(t); err != nil {
			slog.Warn("derpies video seek failed — skipping sample (degrade)", "module", module, "idx", i, "error", err)
			continue
		}
		// Post-seek reset: discard the reorder buffer + reset decoder state so
		// the first decoded frame is the seek-target, not a stale buffer.
		c.Flush()
		frame := pickSeekFrame(d, c, t)
		if frame == nil {
			frame = sequentialFallback(d, c)
			if frame != nil {
				slog.Warn("derpies video sample fell back to sequential decode", "module", module, "idx", i)
			}
		}
		if frame == nil {
			// A sample that yields nothing is skipped (log, no error) — a
			// 0-frame result is NOT an error (degrade to what we got).
			slog.Warn("derpies video sample yielded no frame — skipping (degrade)", "module", module, "idx", i)
			continue
		}
		img, err := frameToPiImage(frame)
		if err != nil {
			// A doc'd partial/undecodable frame is logged + skipped (degrade).
			slog.Warn("derpies video frame skipped — not encodable (degrade)", "module", module, "idx", i, "error", err)
			continue
		}
		out = append(out, img)
	}
	return out, nil
}

// pickSeekFrame walks up to videoSeekDecodeWindow packets after the seek and
// returns the first usable frame at >= t (display time; frames come out of the
// codec in display order). When the stream's frame timestamps are zero it takes
// the first usable frame instead. Codec.Decode returns (nil, nil) while the H.264
// reorder buffer fills and for parameter-set packets — the returned *govid.Frame
// nil-check is done here BEFORE any plane access (a frame.YCbCr deref on a nil
// frame would panic the flow). A frame with nil/empty YCbCr planes is skipped.
func pickSeekFrame(d videoDemuxer, c videoCodec, t time.Duration) *govid.Frame {
	var good []*govid.Frame
	anyZero := false
	for i := 0; i < videoSeekDecodeWindow; i++ {
		pkt, err := d.NextPacket()
		if err != nil { // io.EOF or any other walk-stop.
			break
		}
		frame, _ := c.Decode(pkt)
		if !frameHasPlanes(frame) { // nil frame / nil YCbCr / empty planes all skipped.
			continue
		}
		if frame.Timestamp == 0 {
			anyZero = true
		}
		good = append(good, frame)
	}
	if len(good) == 0 {
		return nil
	}
	if anyZero {
		return good[0] // headers/timestamps are zero: the first usable frame IS the one.
	}
	for _, g := range good {
		if g.Timestamp >= t {
			return g
		}
	}
	return nil
}

// sequentialFallback is the per-sample bounded fallback: the seek-window yielded
// nothing, so it continues (NO re-seek — the demuxer cursor already sits at the
// previous stop position, the packet just past the seek-window end) walking up to
// videoSequentialCap packets in stream order and takes the LAST usable frame
// before the cap. It returns nil when none is usable.
func sequentialFallback(d videoDemuxer, c videoCodec) *govid.Frame {
	var last *govid.Frame
	for i := 0; i < videoSequentialCap; i++ {
		pkt, err := d.NextPacket()
		if err != nil { // io.EOF or any other walk-stop.
			break
		}
		frame, _ := c.Decode(pkt)
		if !frameHasPlanes(frame) {
			continue
		}
		last = frame
	}
	return last
}

// frameHasPlanes is the single validity gate a decoded *govid.Frame must pass
// before it is sampled or encoded: a non-nil frame, a non-nil YCbCr, and all of
// the Y / Cb / Cr planes non-empty (a documented govid partial-frame shape). It
// must be checked on a possibly-nil frame — Codec.Decode returns (nil, nil).
func frameHasPlanes(frame *govid.Frame) bool {
	return frame != nil && frame.YCbCr != nil &&
		len(frame.YCbCr.Y) != 0 && len(frame.YCbCr.Cb) != 0 && len(frame.YCbCr.Cr) != 0
}

// frameToPiImage converts a usable frame (YCbCr 4:2:0) to a JPEG (videoJpegQuality)
// app.PiImage via frame.ConvertRGBA(nil) -> image.NRGBA. Both a nil frame and a
// partial (no-planes) frame are rejected (errNoPixels) so the caller degrades.
func frameToPiImage(frame *govid.Frame) (app.PiImage, error) {
	if !frameHasPlanes(frame) {
		return app.PiImage{}, errNoPixels
	}
	w, h := frame.Width, frame.Height
	if w <= 0 || h <= 0 {
		return app.PiImage{}, errNoPixels
	}
	rgba := frame.ConvertRGBA(nil)
	if len(rgba) != w*h*4 {
		return app.PiImage{}, fmt.Errorf("rgba length %d != %dx%dx4", len(rgba), w, h)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	copy(img.Pix, rgba)
	buf := &bytes.Buffer{}
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: videoJpegQuality}); err != nil {
		return app.PiImage{}, err
	}
	return app.PiImage{
		MimeType: "image/jpeg",
		Data:     base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}
