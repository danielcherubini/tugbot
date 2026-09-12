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
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/liqmix/govid"

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

// ---------------------------------------------------------------------------
// sampleVideoFrames / pickSeekFrame / sequentialFallback — the per-sample walk
// driven through the fake videoDemuxer / videoCodec interfaces (the decode-
// ERROR path; the success path is locked by TestVideoFramesGovidFixture and
// asserted in the fake tests too: (nil, nil) buffering still continues, a good
// frame with planes is kept, an error AFTER a good frame keeps the frame).
// ---------------------------------------------------------------------------

// errFakeDecode is a synthetic codec decode error (e.g. a corrupted NAL unit /
// unsupported packet) — distinct from the (nil, nil) buffering shape, which is
// the NORMAL mid-walk condition and must never be treated as an error.
var errFakeDecode = errors.New("fake codec: corrupted packet")

// fakeVideoDemuxer is a fixed duration + fps, serving a fixed number of
// packets before NextPacket returns io.EOF. A nil packet is fine: the codec
// is the fake too and never touches it.
type fakeVideoDemuxer struct {
	dur       time.Duration
	fps       float64
	packetCap int // NextPacket returns io.EOF after this many packets
	given     int
}

func (f *fakeVideoDemuxer) Seek(t time.Duration) (time.Duration, error) { return t, nil }
func (f *fakeVideoDemuxer) Duration() time.Duration                     { return f.dur }
func (f *fakeVideoDemuxer) VideoInfo() govid.VideoInfo                  { return govid.VideoInfo{FrameRate: f.fps} }
func (f *fakeVideoDemuxer) NextPacket() (govid.Packet, error) {
	if f.given >= f.packetCap {
		return govid.Packet{}, io.EOF
	}
	f.given++
	return govid.Packet{}, nil
}
func (f *fakeVideoDemuxer) Close() error { return nil }

// fakeDecodeStep is one scripted Codec.Decode response.
type fakeDecodeStep struct {
	frame *govid.Frame
	err   error
}

// fakeVideoCodec plays back a script of Decode responses by call index; past the
// end it returns the (nil, nil) buffering shape forever. It counts calls and
// Flushes so a test can assert the walk STOPPED at the error (call count) and
// the per-sample post-seek reset happened (flushes).
type fakeVideoCodec struct {
	steps   []fakeDecodeStep
	calls   int
	flushes int
}

func (f *fakeVideoCodec) Decode(_ govid.Packet) (*govid.Frame, error) {
	if f.calls < len(f.steps) {
		s := f.steps[f.calls]
		f.calls++
		return s.frame, s.err
	}
	return nil, nil
}
func (f *fakeVideoCodec) Flush() { f.flushes++ }

// fakeVideoFrame is a usable 4:2:0 frame (4x2 pixels, all planes non-empty,
// neutral grey) that passes frameHasPlanes and encodes through frameToPiImage.
func fakeVideoFrame(ts time.Duration) *govid.Frame {
	const w, h = 4, 2
	// 4:2:0 planes for a 4x2 frame: 8 Y samples, 2 Cb, 2 Cr.
	ycb := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	ycb.YStride = w
	ycb.CStride = w / 2
	for i := range ycb.Y {
		ycb.Y[i] = 128
	}
	for i := range ycb.Cb {
		ycb.Cb[i] = 128
	}
	for i := range ycb.Cr {
		ycb.Cr[i] = 128
	}
	return &govid.Frame{
		YCbCr:     ycb,
		Timestamp: ts,
		Width:     w,
		Height:    h,
	}
}

// TestVideoWalkStopsOnDecodeError — a Codec.Decode ERROR (not the (nil, nil)
// buffering shape) must STOP the per-sample walk at that point and RECORD the
// error (the thing the degrade log carries), rather than walking the whole
// window quietly.
func TestVideoWalkStopsOnDecodeError(t *testing.T) {
	d := &fakeVideoDemuxer{dur: time.Second, fps: 1, packetCap: videoSeekDecodeWindow + 8}
	c := &fakeVideoCodec{steps: []fakeDecodeStep{
		{},                   // (nil, nil) buffering — the normal shape, not an error
		{err: errFakeDecode}, // a decode error — the walk must stop here
	}}
	res := pickSeekFrame(d, c, 333*time.Millisecond)
	if res.frame != nil {
		t.Fatalf("a walk interrupted by a decode error must not yield a frame; got %v", res.frame)
	}
	if res.decodeErr == nil {
		t.Fatalf("a walk interrupted by a decode error must record it; got nil")
	}
	if !errors.Is(res.decodeErr, errFakeDecode) {
		t.Errorf("recorded decode error = %v; want it to be/wrap errFakeDecode", res.decodeErr)
	}
	if c.calls != 2 {
		t.Errorf("codec calls = %d, want 2 (one buffering step + the error step — the walk must stop at the error, not traverse all %d packets)", c.calls, videoSeekDecodeWindow)
	}
}

// TestVideoSampleDecodeErrorDegradesAndFlowContinues — a per-sample decode
// error degrades (sample skipped, recorded error observable) and NEVER aborts
// the run: a subsequent normal sample still decodes and yields a frame; an
// error in a walk AFTER its frame was already captured (buffering keeps, good
// frame kept) does not lose the frame.
func TestVideoSampleDecodeErrorDegradesAndFlowContinues(t *testing.T) {
	// dur 2s, fps 1 -> k = 3; seek targets t: 1/3 s, 1 s, 5/3 s.
	d := &fakeVideoDemuxer{dur: 2 * time.Second, fps: 1, packetCap: 200}
	c := &fakeVideoCodec{steps: []fakeDecodeStep{
		// sample 0 seek walk: buffering, error -> stop (2 calls).
		{}, {err: errFakeDecode},
		// sample 0 sequential fallback: error -> stop (1 call).
		{err: errFakeDecode},
		// sample 1 seek walk: good frame, buffering, error -> stop (3 calls).
		// The good frame must SURVIVE the later error (no behavior change to
		// the success path: an error after a good frame keeps the frame).
		{frame: fakeVideoFrame(time.Second)},
		{}, {err: errFakeDecode},
		// sample 2 seek walk: good frame first, then the window keeps walking
		// (32 packets consumed; leftover steps play back as (nil, nil)
		// buffering — the SUCCESS path: a good frame with planes is kept).
		{frame: fakeVideoFrame(1700 * time.Millisecond)},
	}}
	res := sampleVideoFrames(d, c)
	if len(res.frames) != 2 {
		t.Fatalf("frames = %d; want 2 (sample 0 degrades; samples 1-2 survive — per-sample degrade is log + skip, never abort)", len(res.frames))
	}
	for i, f := range res.frames {
		if f.MimeType != "image/jpeg" || f.Data == "" {
			t.Errorf("frame[%d] = %+v, want a JPEG payload", i, f)
		}
	}
	if res.decodeErr == nil {
		t.Fatalf("the first recorded decode error must be observable; got nil")
	}
	if !errors.Is(res.decodeErr, errFakeDecode) {
		t.Errorf("recorded decode error = %v; want it to be/wrap errFakeDecode", res.decodeErr)
	}
	if c.calls != 7 {
		t.Errorf("codec calls = %d, want 7 (2 seek + 1 fallback + 3 sample-1 seek: error-interrupted walks stop at the error; sample 2's clean window then walks all 32 real packets as uncounted (nil, nil) buffering)", c.calls)
	}
	if c.flushes != 3 {
		t.Errorf("flushes = %d, want 3 (one post-seek reset per seek-resolved sample)", c.flushes)
	}
	if d.given != 38 {
		t.Errorf("demuxer packets consumed = %d, want 38 (3 sample-0 + 3 sample-1 + 32 sample-2) — the interrupted walks stop, the clean window walks in full", d.given)
	}
}

// TestVideoSamplePersistentDecodeErrorStopsOnce — when TWO CONSECUTIVE samples
// both fail with a decode error (a persistent decoder failure), the walk stops
// for the remaining samples too: they don't call the decoder at all, and the
// first recorded error is still observable.
func TestVideoSamplePersistentDecodeErrorStopsOnce(t *testing.T) {
	d := &fakeVideoDemuxer{dur: 2 * time.Second, fps: 1, packetCap: 64}
	c := &fakeVideoCodec{steps: []fakeDecodeStep{
		// sample 0: seek walk [buffering, error] + fallback [error] (3 calls).
		{}, {err: errFakeDecode}, {err: errFakeDecode},
		// sample 1: seek walk [error] + fallback [error] (2 calls).
		{err: errFakeDecode}, {err: errFakeDecode},
		// sample 2: N/A — skipped whole (2 consecutive errored samples);
		// if it ran, the exhausted-script (nil, nil) default would show up
		// in c.calls.
	}}
	res := sampleVideoFrames(d, c)
	if len(res.frames) != 0 {
		t.Fatalf("frames = %d, want 0 (a persistent decode failure degrades to what we got)", len(res.frames))
	}
	if res.decodeErr == nil {
		t.Fatalf("the recorded decode error must be observable; got nil")
	}
	if !errors.Is(res.decodeErr, errFakeDecode) {
		t.Errorf("recorded decode error = %v; want it to be/wrap errFakeDecode", res.decodeErr)
	}
	if c.calls != 5 {
		t.Errorf("codec calls = %d, want 5 (2 seek + 1 fallback per errored sample; sample 2 never calls the decoder)", c.calls)
	}
}
