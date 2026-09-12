---
status: accepted
date: 2026-09-12
superseded-by:
---

# Derpies gif handling — embed titles join the judged text, animated media expand to sampled frames (step-9 URL-only gate relaxation declined)

## Problem

The observed survival vector is a Klipy `gifv` embed. The two observed survival
clips (Klipy `gifv` messages, the animation living ONLY in
`MessageEmbed.Video` on the provider's static CDN — e.g.
`https://klipy.com/gifs/cycling-indoorcycling` → embed title "Indoor Cycling
with Zwift Virtual Ride"; the second clip's title "Peloton Lady
Kendalltoole" — `peloton` sits in `derpies_gimmicks` as an LLM-learn row,
`source='llm'`, created 2026-09-09) exposed two independent blind spots:

1. **Title blindness.** The message text is a bare URL; the brand word lives in
   the embed TITLE. The flow tokenized, prompted, and gated on content + the
   one-hop reference only — never on embed titles — so neither the fast path nor
   the LLM verdict could ever see the word. A titled Klipy link (a "…with
   Zwift…" / "Peloton …" post) passed with zero asks, forever.
2. **First-frame blindness.** The actual animation (an uploaded `image/gif`
   attachment, or the `gifv` embed video) was judged as its first frame only.
   The rasterized-frame dead-end chain: pirpc `shrinkOne` uses `image.Decode`
   (single-frame); pi's own photon resize is still-image; the VLM therefore saw
   the first frame alone — while the burned-in gimmick text (e.g. the "PEL ON"
   cap, class-wide shots) lives in LATER frames. Frames 2..N were never
   judgment input.

## Decision (shipped 2026-09-12: `2032a69`, `95097fb`, `65bd2ef`)

1. **Embed titles join the judged TEXT — the observed vector becomes
   fast-path catchable.** Every non-empty `MessageEmbed.Title` (up to 10 titles,
   each trimmed to 200 runes; `Description`/`Provider`/`Footer` deliberately
   excluded) joins the fast-path token union the same way referenced-message
   content already does, so a titled link carrying a list word deletes with
   ZERO asks. Titles are part of what was posted (a titled link the user CHOSE
   to post counts as posted content; the stance is adversarial — false
   negatives are the worse error) and also join the slow path: the new
   OPTIONAL `{{EMBED}}` marker (NUL-fence-safe through the existing two-phase
   substitution; absent marker degrades by omission — a pre-feature live row
   keeps working) substitutes the "TITLES OF MEDIA EMBEDDED WITH THE MESSAGE
   …" block, which tells the model a known word in a title counts as if typed.
   The step-9 gate reuses the SAME token union, so a non-empty title makes a
   post "have text" for the gate (identical to typed text — pinned by
   `TestFlowFrameWordVerdictRejectedWhenTitleHasTokens`).
2. **Animated media expand to ≤8 sampled frames, both gif paths, joining the
   ONE existing ask.** An uploaded `image/gif` attachment expands in-process
   (stdlib `image/gif`, zero new dependencies; the input is capped at 16MB
   BEFORE decode — an over-cap input is a budget refusal, never a decode
   — and the DECODED DIMENSIONS are similarly pre-decode capped, read via
   `gif.DecodeConfig` (the global HEADER alone, no frame
   materialization): a logical screen over 16MP is a DISTINCT budget
   refusal (`errGIFDimsTooLarge` — a uniform large-screen gif
   compresses to KBs on disk, so the byte cap alone cannot bound the
   decode work; the degrade log names WHICH bound fired) — and every
   selected frame is composited onto the full canvas, a
   partial-rectangle delta frame never encoded as-is: keep-previous
   disposal emulated with draw.Over on a persistent canvas); up to 60
   frames walked from the stream; consecutive-duplicate skip via 32×32
   grayscale sampling, tolerance 8) into ≤8 evenly-spaced JPEG q80
   frames. A `gifv` embed's video URL (mp4
   H.264 / webm VP8) is downloaded (8MB cap, same `isSafeURL` guard, 10s
   client; non-2xx is an error and the body is never read) and decoded with
   `github.com/liqmix/govid` behind the `videoFrameDecoder` seam (the same
   seam shape as `clock`/`ops`) — ≤8 SEEK-sampled JPEG q80 frames, with a
   bounded sequential fallback (up to 32 packets after a seek; up to 600
   packets walked). Frames join the SAME single `AskWithImages` call — no new
   asks — and pass through the existing pirpc per-ask image guard unchanged;
   the new OPTIONAL `{{GIFS}}` marker tells the model the frames are samples of
   one animated gif "gifs loop and change over time, so a gimmick's word can
   appear in ANY frame". Every failure arm degrades per the table below.
3. **govid over the alternatives — why:** PURE Go (no cgo, no runtime binary
   dependency), verified bit-exact for H.264/High and VP8, and the demuxers
   expose `Seek` — keyframe-accurate — which is what makes O(K) sampling
   possible (≤8 seeks instead of a full decode), with the bounded sequential
   fallback covering a seek that yields nothing.

## Considered Options (rejected)

- **ffmpeg subprocess** — rejected: a runtime binary dependency (the host must
  ship the right ffmpeg build; embedding / spawning makes every decode a
  process round-trip and a new failure surface on an adversarial-input
  filter).
- **hi264 (the external Go decoder)** — rejected: it decodes KEYFRAMES ONLY —
  exactly the frames this decision exists to see are NOT keyframes, so it
  would close nothing.
- **Per-frame micro-asks (one ask per sampled frame)** — rejected: a burst
  multiplier (up to 8× the ask cost per gif) on the single shared pi RPC
  channel, for no extra information — one ask with the frames attached judges
  them all.
- **CLIP/OCR pre-classification (a cheap model picks which frames matter)** —
  rejected: nothing to supervise — there is no labeled frame/word dataset to
  train or tune a pre-filter on; the VLM ask IS the classifier (OCR on clip
  frames would only re-encode the same first-frame-limitation dead end).

## Considered and DECLINED: relaxing the step-9 gate for URL-only text

The natural follow-up to the frame expansion would be to let a
`wordValid`-only verdict (the image-only arm) act on a post whose only text is
a bare URL, so a frame-anchored word can delete a titled/URL-text post.
**Declined — operator decision pending.** A word that appears ONLY in a frame of a texted- or titled-text post is still
rejected by the step-9 gate (a `wordValid`-only dead end — the same class as
the unicode dead end documented in the feature doc: the 2026-09-07
confusable-fold history, where a structural no-valid-verdict case passed
rather than act). Keeping the gate unchanged is the conservative position
while the operator decides; the intended follow-up **ADR 0008** will record
that decision. The `wordValid`/folded-token-in-text gate is otherwise
UNCHANGED.

## Degrade discipline (every failure arm → log + status quo — never abort the flow)

| Failure arm | Behavior |
|---|---|
| gif: single-frame or undecodable (`errSingleFrame` / `errGIFUnreadable`) | `slog.Warn` ("degraded to raw send") + the RAW gif bytes ride along (status quo) |
| gif: over the 16MB pre-decode input cap (`errGIFTooLarge` — budget refusal, the decode work is never attempted) | `slog.Warn` (url + byte length) + the RAW gif bytes ride along (status quo) |
| gif: logical screen over the 16MP pre-decode decoded-dimension cap (`errGIFDimsTooLarge` — budget refusal via `gif.DecodeConfig`, the full decode is never attempted) | `slog.Warn` (url + byte length; the log names the DECODED-DIMENSION bound, distinct from the input-byte-cap arm) + the RAW gif bytes ride along (status quo) |
| gif: per-frame JPEG encode failure | `slog.Warn` with the skip count; that frame is skipped (fewer than 8 frames is correct) |
| gifv video: download failure (request error, non-2xx, over-cap Content-Length, over-cap body) | `slog.Warn` ("degrade to thumbnail-only") + skip — the thumbnail, if planned, still rides on its own entry (status quo) |
| gifv video: decoder not wired, decoder error, or zero frames | `slog.Warn` + skip — thumbnail-only (status quo); a sniffed container that yields zero frames is NOT an error (degrade to what we got) |
| gifv video: unsupported container (magic-byte sniff miss — truncated body or HTML error page) | `errUnsupported` → `slog.Warn` + skip (never panics the adversarial-input filter) — thumbnail-only |
| gifv video: a seek fails / a sample yields nothing / a frame is undecodable | `slog.Warn` per sample + skip that sample (bounded sequential fallback first); a word therefore seen in MORE of the frames is the best-available approximation |
| image leg (attachments + embeds): per-URL download failure | log + skip that URL; when nothing downloads the ask degrades to text-only (mentioned parity, pre-existing) |
| prompt template: DB fetch error / invalid row | `slog.Warn` (fetch error; invalid-but-present row falls back silently) + code-pinned default — the filter never runs with a broken prompt (pre-existing) |
| embed titles: a titled post with a word that appears only in a frame | step-9 gate rejects (no learning, no delete, `slog.Warn`) — the DECLINED relaxation above, logged as the status quo |
