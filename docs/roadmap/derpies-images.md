---
status: committed
done-when: In prod with the `derpies` flag enabled and `TUGBOT_DERPIES_USER_IDS` set: (1) an image attachment (screenshot/pasted image) joins the pi ask — `journalctl` shows the `Downloading image` lines and the verdict fires; (2) a `GIMMICK:<word>` verdict on an image-only message deletes the message and LEARNS the word (`wordValid` gate only, the word may be absent from the text); (3) a text+image message where the verdict word is valid but appears ONLY in the image (not a text token) is neither deleted nor learned (the verdict gate rejects — doing nothing, logged); (4) a reply/quote-reply from the gated user that quotes one of his own earlier messages containing a seeded word (or a screenshot) — without retyping it — deletes the CURRENT message on the fast path, and a `GIMMICK:<word>` where the word appears only in the quoted message learns the word; (5) a referenced-message fetch failure (old/deleted message) degrades to judging the current message only (logged, no flow abort); (6) text-only behavior is byte-identical to before — every pre-existing derpies test passes unchanged when the message has no attachments and no `MessageReference`.
---

# Derpies Images + Referenced Messages Plan

**Goal:** Make the derpies filter see what a text filter cannot: downloaded attachment/embed images (the reported gap — Derpie WILL test this), and one-hop referenced messages (replies/quote-replies — he can point at his own earlier message instead of retyping).
**Architecture:** Mirror the mention handler's proven mechanisms VERBATIM in shape into the derpies package (self-contained, mention untouched): (a) the image leg — attachments + embeds (`image/*` content type, `isSafeURL`, `mimeForURL`, 10 s HTTP client, per-URL failure logged + skipped); (b) the referenced-message fetch — `m.MessageReference != nil && MessageID != ""` → `ops.channelMessageRetrieve` (a `*discordgo.Message` via the `discordOps` seam; REST GET; per-failure degrade to no-reference, NEVER abort the flow — same discipline as mention). Current + referenced content merge: the fast-path token set is the UNION, images are the URL-deduped union of both plans, the verdict gate is automatically the union (a word in EITHER text passes; a message with NO text tokens at all stays `wordValid`-only). `gimmickPrompt` ALREADY has the `nImages` + `refText` parameters with the pinned images line and referenced block (shipped by the respell plan via the `{{IMAGES}}` / `{{REF}}` markers — this plan only PASSES REAL values; `0`/`""` → byte-identical prompt, already pinned by the shipped `TestGimmickPromptDefault`). The flow uses `AskWithImages` when images downloaded, plain `Ask` otherwise (production-equivalent — `PiRpc.Ask` IS `askWithImages(ctx, prompt, nil)` — the branch keeps the test seam clean).

**Decided design calls (user-confirmed):** (a) learn from image-only messages (`wordValid` alone; bounded risk — a wrong word only deletes the gated user's own future messages containing that word); (b) full mention parity for images: attachments + embeds, not just attachments; (c) referenced messages: fetch one hop (mention parity — no chains), include their text in the fast-path union, their images in the download union, and their content in the prompt; a word that appears only in the quote IS learnable (the quote is part of what's being posted).

**Tech Stack:** Go, bwmarrin/discordgo, net/http + httptest (tests), the existing `app.PiBackend` / `internal/pirpc` (no protocol change — the RPC already carries `images` frames), the existing `discordOps` seam (gains one method).

Conventions this plan follows:
- Mirror targets: `internal/handlers/mention/mention.go` — `imageURLPlan` / `downloadImages` / `isSafeURL` / `mimeForURL` (image leg) and the step-7 referenced fetch (message_reference check → `ops.channelMessageRetrieve` → log-and-treat-as-absent on failure). Self-contained copies in the derpies package; do NOT refactor mention into a shared package.
- The flow's only outgoing DISCORD REST calls: `DeleteMessage` (action) + the referenced-message fetch (data — mention does the identical REST GET; no bot message, no reaction, no gulag involvement on any path). Image downloads are direct CDN GETs (identical to mention).
- Unit-test shape: in-package fakes; image flow tests use a real `httptest` server + real `discordgo.Message` attachments/embeds (no new seam on the `Derpies` struct — `downloadImages` stays a plain method); the referenced fetch goes through the existing `discordOps` seam (fake).

**Do NOT change:** the mention package (mirror, do not touch), `internal/pirpc` (no protocol change), `main.go` wiring (the handler stays passive; the count stays 12 and the "twelve" texts stay), `internal/config`, migrations, `features`, or the 0-image/no-ref prompt bytes.

---

### Task 1: Derived images + referenced-message support (image leg, referenced fetch, prompt, flow, gate)

**Context:** The derpies flow only ever looks at `m.Content` and its own attachments. Two evasions are unpatched: (1) images — a screenshot/pasted image is invisible to the token path and to the LLM (an image-only post asks a verdict on EMPTY text, gets `CLEAN`, and can never teach the filter); (2) references — replying to or quote-replying a previous message lets the gimmick live in the QUOTE instead of the new message. Both need the already-proven mention mechanisms, mirrored self-contained: the image leg and the one-hop referenced fetch (mention's step 7: `MessageReference` check → `ops.channelMessageRetrieve` → on failure log + treat as absent, never abort). Current + referenced content merge by UNION: fast-path tokens, images (URL-deduped), and the verdict gate (a word in EITHER text passes; a message with no text tokens at all stays `wordValid`-only).

**Files:**
- Modify: `internal/handlers/derpies/derpies.go`
- Modify: `internal/handlers/derpies/derpies_test.go` (fakeOps gains the retrieve method + fields; fakePi's `AskWithImages` gains split image tracking)
- Create: `internal/handlers/derpies/derpies_images_test.go` (the image/referenced flow tests + the image-leg/network tests using `net/http/httptest` — mirror mention_test.go's shape)

**What to implement:**

1. `derpies.go` — new imports added to the import block: `encoding/base64`, `io`, `net/http`, `time`.

2. `derpies.go` — `discordOps` seam gains ONE method (the referenced fetch — mirror mention's `ops.channelMessageRetrieve`; add the comment pin below):

```go
type discordOps interface {
	// deleteMessage — the flow's ONLY outgoing REST call that takes an
	// action (the referenced fetch is a data GET, not a bot action).
	deleteMessage(channelID, messageID string) error
	// channelMessageRetrieve — the one-hop referenced-message fetch
	// (mention parity): a REST GET of the earlier message behind a
	// reply/quote-reply. A failure (already deleted, rate limit) is
	// handled by the CALLER as "no reference" — log and continue, never
	// abort the flow.
	channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error)
}
```

`realOps` gains:

```go
func (o *realOps) channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error) {
	return o.d.ChannelMessage(channelID, messageID)
}
```

(mirror mention's realOps body EXACTLY — discordgo v0.29.0 exposes `ChannelMessage`, not `ChannelMessageRetrieve`).

3. `derpies.go` — image leg (self-contained mirror of mention; the ONLY differences from mention's versions are the slog module tag `"module", module` (the derpies const) and the split below into a `downloadPlan` core + a `downloadImages` wrapper so the current+referenced plans can merge before a single download pass):

```go
// imagePlanEntry is the planned (url, mime, source) triple before download.
type imagePlanEntry struct {
	url    string
	mime   string
	source string // "attachment" | "embed"
}

// imageURLPlan mirrors mention's plan: attachment urls with an image/* content
// type (empty content_type falls back to application/octet-stream and is
// skipped, like mention), then embed image/thumbnail urls deduped against the
// ATTACHMENT urls and MIME'd by extension (query/fragment stripped first:
// png→image/png, gif→image/gif, webp→image/webp, else image/jpeg). Every url
// must pass isSafeURL.
func imageURLPlan(m *discordgo.Message) []imagePlanEntry

// isSafeURL: http:// or https:// prefix only.
func isSafeURL(url string) bool

// mimeForURL: extension map (png/gif/webp; everything else image/jpeg),
// query/fragment stripped first.
func mimeForURL(url string) string

// downloadPlan is the per-URL GET leg: request with the explicit-timeout
// client, io.ReadAll, base64 standard-alphabet encoded. A failed download
// (request error or non-nil ReadAll error) is logged (module derpies) and
// that URL is skipped — the flow continues with the rest. Attachment
// downloads log the url + mime; embed downloads log the url. 5xx/4xx
// responses are NOT an error here (mention parity: the body is read whatever
// the status carries) — mirror mention EXACTLY.
func (h *Derpies) downloadPlan(ctx context.Context, plan []imagePlanEntry, client *http.Client) []app.PiImage

// downloadImages mirrors mention's one-message shape: plan then download.
func (h *Derpies) downloadImages(ctx context.Context, m *discordgo.Message, client *http.Client) []app.PiImage {
	return h.downloadPlan(ctx, imageURLPlan(m), client)
}
```

4. `derpies.go` — `gimmickPrompt`: NO signature or byte change this plan. The shipped 5-arg `gimmickPrompt(tmpl, content, known, nImages, refText)` (respell plan) already substitutes the pinned images line (`The message also has N attached image(s) (screenshots or pasted images — a text filter would not see their content). Judge the text AND the images. If the anchor word appears in an image rather than the message text, name it as if it were in the message.`, `N` = nImages) and the pinned referenced block (`<<<REFERENCED MESSAGE` + refText + `REFERENCED MESSAGE>>>` + the "The message replies to a previous message (often the author's own)…" line) via the `{{IMAGES}}` / `{{REF}}` markers, and `0`/`""` returns byte-identical output to the shipped prompt (asserted by the shipped `TestGimmickPromptDefault` / `TestGimmickPromptMissingOptionalMarkers`). This plan only ever PASSES REAL VALUES: `nImages = len(images)`, `refText = referenced.Content`. (The previous stale pin — a 4-arg `gimmickPrompt(content, known, nImages, refContent)` — was the respell-era signature; do NOT revert the shipped 5-arg.)

5. `derpies.go` — flow changes (existing step numbers/comments keep; pinned comment wording for the new parts):

After the author-ID gate (step 3) and BEFORE the fast path, add:

```go
	// 3.5 Referenced message (reply/quote-reply): one REST GET (mention
	//     parity, via the discordOps seam). On failure (already deleted,
	//     rate limit) log and continue WITHOUT a reference — never abort
	//     the flow (mention's step-7 discipline).
	var referenced *discordgo.Message
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		ref, err := h.ops.channelMessageRetrieve(m.ChannelID, m.MessageReference.MessageID)
		if err != nil {
			slog.Error("derpies referenced fetch failed", "module", module, "message", m.ID, "error", err)
		} else {
			referenced = ref
		}
	}
```

Replace the step-4 token fetch with the UNION (the fast path then needs no further change — it already iterates `toks`):

```go
	// 4. Fast path: one list SELECT; exact token match. The token set is
	//     the UNION of the posted message and, when a referenced message
	//     was fetched, its content (a reply re-quoting a seeded word hits
	//     here without retyping).
	toks := tokensForMatch(m.Content)
	if referenced != nil {
		for t := range tokensForMatch(referenced.Content) {
			toks[t] = true
		}
	}
```

(Keep the existing `listGimmicks` fetch + the iteration/delete block exactly as-is.)

Replace the step-4.5 position (images) — the SHIPPED flow has NO 4.5 (the respell-era 6-7 comment says this plan re-pins that block when it lands); INSERT the new step 4.5 block immediately AFTER the step-4 fast path (after its early-return block) and BEFORE the step-5 `h.app.Pi == nil` check, URL-deduped UNION:

```go
	// 4.5 Images (mention parity: attachments + embeds, isSafeURL-guarded,
	//     10s client, per-URL failure logged + skipped — the flow degrades
	//     to a text-only ask when nothing downloads). The plan is the union
	//     of the posted message and, when present, the referenced message,
	//     URL-deduped (the same screenshot in both must not double the
	//     base64 payload).
	imgPlan := imageURLPlan(m)
	if referenced != nil {
		imgPlan = append(imgPlan, imageURLPlan(referenced)...)
	}
	var uniqPlan []imagePlanEntry
	seenURLs := map[string]bool{}
	for _, e := range imgPlan {
		if seenURLs[e.url] {
			continue
		}
		seenURLs[e.url] = true
		uniqPlan = append(uniqPlan, e)
	}
	images := h.downloadPlan(ctx, uniqPlan, &http.Client{Timeout: 10 * time.Second})
```

Replace the ask step (6-7) with (the SHIPPED block already fetches the template from the DB seam with the code-default fallback — KEEP that intact; only the `gimmickPrompt` call and the ask branch change):

```go
	// 6-7. One ask (the 300s deadline lives in the pi package). AskWithImages
	//     when images downloaded; plain Ask otherwise (in production the two
	//     are equivalent — PiRpc.Ask is askWithImages(ctx, prompt, nil) — the
	//     branch keeps the text path's test seam clean).
	tmpl, err := h.store.promptText(ctx)
	if err != nil || !validTemplate(tmpl) {
		tmpl = defaultPromptTemplate
		if err != nil {
			slog.Warn("derpies prompt template unavailable — using default", "module", module, "error", err)
		}
	}
	refContent := ""
	if referenced != nil {
		refContent = referenced.Content
	}
	prompt := gimmickPrompt(tmpl, m.Content, sortedKeys(list), len(images), refContent)
	var (
		text   string
		askErr error
	)
	if len(images) > 0 {
		text, askErr = h.app.Pi.AskWithImages(ctx, prompt, images)
	} else {
		text, askErr = h.app.Pi.Ask(ctx, prompt)
	}
	if askErr != nil {
		slog.Error("derpies pi ask failed", "module", module, "error", askErr)
		return
	}
```

Replace the step-9 gate with (KEEP the shipped FOLDED form — `fw := foldToASCII(word)` — and add the two-arm semantics; the union makes the referenced-text case hold AUTOMATICALLY since `toks` now carries the quote's folded tokens):

```go
	// 9. SANITY before learning: charset/length — on the FOLDED verdict word
	//    (shipped form) — AND, when the (union of posted + referenced) text
	//    has tokens, the folded word must have appeared as a folded token of
	//    that text (same tokenization as the fast path). A message with NO
	//    text tokens at all (image-only, or empty text with an empty/absent
	//    reference) is bounded by wordValid alone: the verdict word may come
	//    from image text (the message is being filtered — a wrong word can
	//    only delete the gated user's own future message containing that
	//    word). A hallucinated word can never enter the list.
	fw := foldToASCII(word)
	if !wordValid(fw) {
		slog.Warn("derpies invalid verdict word — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}
	hasTextTokens := false
	for tok := range toks {
		if tok != "" {
			hasTextTokens = true
			break
		}
	}
	if hasTextTokens && !toks[fw] {
		slog.Warn("derpies verdict word not in the message — doing nothing", "module", module, "word", word, "message", m.ID)
		return
	}
```

(Steps 1-3, 10, and the error/success log lines of the fast path stay EXACTLY as pinned before this plan.)

6. Fakes — `derpies_test.go`:

- `fakeOps` gains the seam method + fields: `ref *discordgo.Message`, `refErr error`, `refCalls int`:

```go
func (o *fakeOps) channelMessageRetrieve(channelID, messageID string) (*discordgo.Message, error) {
	o.refCalls++
	return o.ref, o.refErr
}
```

- `fakePi` — `AskWithImages` ALREADY exists (the interface requires all three methods) but shares the `asks`/`prompts` counters with `Ask`, so the new flow tests could not tell the two paths apart. Split the tracking: `AskWithImages` keeps its signature but increments its OWN fields — `imageAsks int`, `imagePrompts []string`, `images [][]app.PiImage` — leaving `asks`/`prompts` to `Ask` alone:

```go
func (f *fakePi) AskWithImages(ctx context.Context, prompt string, images []app.PiImage) (string, error) {
	f.imageAsks++
	f.imagePrompts = append(f.imagePrompts, prompt)
	f.images = append(f.images, images)
	return f.resp, f.askErr
}
```

(`resp`/`askErr` stay shared by both methods; `Stop` stays a no-op; the pre-existing flow tests assert `pi.asks`/`pi.prompts` — they keep passing unchanged because those counters now mean the TEXT path only.)

Existing tests: the `derpMsg`/`otherMsg` helpers never set `MessageReference`, so the fetch path stays inert; only `fakeOps` needs the new method to compile. ALL pre-existing derpies tests pass UNCHANGED — their `gimmickPrompt` call sites ALREADY pass the 5-arg form with `0, ""`; `TestGimmickPromptDefault`'s 0-image/no-ref byte assertions are the same ones this plan's `0`/`""` flow path must keep producing, and `TestFlowVerdictLearnsAndDeletes`'s full-equality assertion stays intact because the flow computes `len(images)==0` and `refContent==""` for a no-attachment/no-reference message, so the 5-arg call returns the identical string. NO pre-existing call-site changes.

**Tests to write (names exact) — `derpies_images_test.go`:**

Helper (plus the build helpers): `newImgServer(t) (*httptest.Server, []byte)` — a server answering 200 with a stable body; `msgWithImage(content, url string)` — `derpMsg(content)` + `Attachments: []*discordgo.MessageAttachment{{URL: url, ContentType: "image/png"}}` (its `URL`/`ID`/`GuildID`/`ChannelID`/`Author` are as before); `msgWithRef(content string)` — `derpMsg(content)` + `MessageReference: &discordgo.MessageReference{MessageID: "ref1"}` (the tests set `ops.ref = &discordgo.Message{Content: <ref text>}` themselves — the builder only sets the reference pointer so the fake's fetch path triggers).

- `TestIsSafeURL` — `"https://dl..."→true`, `"http://dl..."→true`, `"ftp://dl..."→false`, `""→false`.
- `TestMimeForURL` — `"https://x/y.png?sig=1#frag"→image/png`, `"https://x/a.webp"→image/webp`, `"https://x/g.gif"→image/gif`, `"https://x/photo"→image/jpeg` (default), `"https://x/x.jpg"→image/jpeg` (default).
- `TestImagePlan` — one `*discordgo.Message`, table-asserted: attachment `image/png` kept (mime from content_type); attachment `image/jpeg` kept; attachment `text/plain` skipped; attachment with `ContentType: ""` skipped (octet-stream fallback is not image/*); attachment with a `ftp://` URL skipped (isSafeURL); embed `Image.URL` kept (mime by extension); embed with empty `Image.URL` falls back to `Thumbnail.URL`; an embed whose `Image.URL` equals an attachment URL is deduped (absent from the plan).
- `TestDownloadImages` — two attachment URLs against one `httptest.NewServer` (distinct paths, both 200 with distinct bodies): assert two `app.PiImage`s with the attachments' content_type mimes AND `base64.StdEncoding.EncodeToString(body)` data (round-trip: decode back and compare). Then a third attachment URL whose server path 500s (mention parity: body still read) is included; a fourth attachment URL against a CLOSED server (closed listener / closed server) → logged + skipped, the rest survive.
- `TestGimmickPromptImages` — the 5-arg SHIPPED form: `gimmickPrompt(defaultPromptTemplate, "content", []string{"bike"}, 2, "")` contains the pinned image line with `2`; with `0, "ref text"` contains `<<<REFERENCED MESSAGE`, `ref text`, `REFERENCED MESSAGE>>>` AND the pinned referenced line, and does NOT contain the image line; with `2, "ref text"` contains BOTH; with `0, ""` equals the 0-image/no-ref shipped prompt form (byte-identical — the shipped `TestGimmickPromptDefault` already pins this; this test restates it at the flow level).
- `TestFlowImagesUseAskWithImages` — flag on, `words = {}` (no fast hit), `m = msgWithImage("totally safe words", srv.URL+"/a.png")` → `pi.imageAsks == 1`, `pi.asks == 0`, `pi.imagePrompts[0]` contains the image line with `1`, `len(pi.images[0]) == 1`; `pi.resp = "CLEAN"` → no delete, `added` empty.
- `TestFlowImageOnlyLearnsAndDeletes` — flag on, `words = {}`, `m = msgWithImage("", srv.URL+"/a.png")` (no text tokens) → `pi.imageAsks == 1`, `pi.asks == 0`; `pi.resp = "GIMMICK:zswiftf"` (zswiftf is NOT a token of the empty text) → `added == ["zswiftf|llm"]` and one delete `["c1","msg1"]`.
- `TestFlowImageOnlyVerdictInvalidWordRejected` — same shape, `pi.resp = "GIMMICK:s-w1ft"` → `added` empty, no delete (charset gate still applies to image-only).
- `TestFlowTextAndImageVerdictWordNotInTextRejected` — flag on, `words = {}`, `m = msgWithImage("hi there", srv.URL+"/a.png")`, `pi.resp = "GIMMICK:zswiftf"` (valid, NOT a token of "hi there") → `added` empty, no delete (text-token gate preserved whenever text tokens exist — including text+image).
- `TestFlowImageDownloadFailureDegradesToTextAsk` — flag on, `words = {}`, `m = msgWithImage("totally safe words", closedSrvURL)` (per-URL failure) → `images` empty → `pi.asks == 1`, `pi.imageAsks == 0`, `pi.prompts[0]` equals `gimmickPrompt("totally safe words", sortedKeys(words), 0, "")`; `pi.resp = "CLEAN"` → no delete.
- `TestFlowReferencedFetchFailureDegrades` — flag on, `words = {}`, `m = msgWithRef("totally safe words")` with `fakeOps.refErr = errors.New("gone")` (the fetch fails, so the reference's content never joins the union) → no abort: the flow reaches the ask (no fast hit — `toks` is the current message only), `fakeOps.refCalls == 1`, `pi.asks == 1`, `pi.imageAsks == 0`; `pi.resp = "CLEAN"` → no delete.
- `TestFlowReferencedFastHitDeletes` — flag on, `words = {"sw1ft"}`, `m = msgWithRef("↑↑↑")` with `fakeOps.ref = &discordgo.Message{Content: "who's giving me a sw1ft."}` → ONE fast delete `["c1","msg1"]`, `pi.asks == 0`, `pi.imageAsks == 0`, `added` empty.
- `TestFlowReferencedPromptCarriesRefContent` — flag on, `words = {}`, `m = msgWithRef("holler")` with `fakeOps.ref = &discordgo.Message{Content: "holler at zswiftf now"}` → no fast hit, no images → `pi.asks == 1`, `pi.prompts[0]` contains `<<<REFERENCED MESSAGE`, `holler at zswiftf now`, `REFERENCED MESSAGE>>>`; `pi.resp = "CLEAN"` → no delete.
- `TestFlowReferencedVerdictWordOnlyInRefLearns` — flag on, `words = {}`, `m = msgWithRef("???")` with `fakeOps.ref = &discordgo.Message{Content: "holler at zswiftf now"}`; `pi.resp = "GIMMICK:zswiftf"` (a valid word, present ONLY in the referenced text) → `added == ["zswiftf|llm"]` and one delete.
- `TestFlowReferencedVerdictWordNowhereRejected` — flag on, `words = {}`, `m = msgWithRef("aaa")` with `fakeOps.ref = &discordgo.Message{Content: "bbb"}`; `pi.resp = "GIMMICK:zswiftf"` (valid, in neither text) → `added` empty, no delete.
- `TestDownloadPlanDedupesReferenced` — `m = msgWithImage("", urlX)` plus `fakeOps.ref` = a message with the SAME attachment `urlX` (two-sided same URL) and second URL `urlY` → `len(images) == 2` (X deduped, Y included) — assert the union plan dedupes by URL across current + referenced.

TDD note: write ALL tests (plus the fake shapes) FIRST; they fail with compile errors (`imageURLPlan`/`downloadPlan`/`downloadImages`/`isSafeURL`/`mimeForURL` undefined, `gimmickPrompt` arity, `fakeOps.channelMessageRetrieve` missing) — that IS the RED state.

**Steps:**
- [ ] Write ALL the tests above first (plus the fakeOps `channelMessageRetrieve` shape and the fakePi split image tracking; NO changes to the pre-existing `gimmickPrompt` call sites — they already pass `0, ""`)
- [ ] Run `go test ./internal/handlers/derpies/ -count=1 -short`
  - Did it fail with compile errors (undefined image-leg functions / missing seam method on fakeOps / fakePi missing the AskWithImages tracking)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement per the exact pinned code above: seam method + `realOps`, image leg (`imagePlanEntry`/`imageURLPlan`/`isSafeURL`/`mimeForURL`/`downloadPlan`/`downloadImages` — the `gimmickPrompt` body is UNTOUCHED, already 5-arg with the images/ref blocks), flow 3.5 fetch / 4 union / 4.5 deduped union / 6-7 branch (5-arg call with `len(images)`/`refContent`) / 9 two-arm folded gate
- [ ] Run `go test ./internal/handlers/derpies/ -count=1 -short`
  - Did all tests pass (ALL the pre-existing tests + all the new image/referenced tests)? If not, fix the failures and re-run before continuing.
- [ ] Run `gofmt -l internal/handlers/derpies/` — clean
- [ ] Run `go build ./...` — succeeds
- [ ] Run `go vet ./...` — clean
- [ ] Run `golangci-lint run` — no issues
- [ ] Commit with message: "derpies: images (attachment+embed) and referenced messages in the fast/slow paths"

**Acceptance criteria:**
- [ ] `go test ./internal/handlers/derpies/ -count=1 -short` green — ALL pre-existing tests pass UNCHANGED (no-attachment/no-reference messages behave byte-identically: same prompt bytes via the 0/"" path, plain `Ask`, same gate — the 0-image/no-ref bytes being the shipped `TestGimmickPromptDefault`'s own)
- [ ] A message with a downloadable image uses `AskWithImages` with the pinned image line; a message with a `MessageReference` triggers exactly ONE `channelMessageRetrieve` (and its text joins the fast-path union + the prompt's referenced block + the learning gate)
- [ ] Learned-word gate: word in EITHER posted or referenced text passes; a message with no text tokens at all is `wordValid`-only; a word in neither is rejected
- [ ] A per-URL download failure skips that URL (logged); nothing-downloaded degrades to a text-only ask; a referenced-fetch failure degrades to no-reference (logged, no abort)
- [ ] `gofmt`/`go vet`/`golangci-lint` clean; `git status` shows ONLY the 3 derpies files (mention, pirpc, main.go, config, migrations untouched)
- [ ] The flow's outgoing Discord REST calls are still `DeleteMessage` + the referenced fetch only (no bot message, no reaction, no gulag on any path)

---

### Task 2: Full-suite green + feature note update

**Context:** New behavior in a shipped handler. No wiring change (the handler stays passive; the count stays 12), no migration, no config. This task proves the whole repo green and updates the durable feature note (`docs/features/derpies.md`) so the image + referenced semantics (and the image-only learning decision) are documented where the feature lives.

**Files:**
- Modify: `docs/features/derpies.md`

**What to implement:**

In `docs/features/derpies.md`:
- **Fast path** paragraph: the token set is the union of the posted message and (when fetchable) the one-hop referenced message's content.
- Add a new **`Referenced messages`** paragraph (after the Fast path): reply/quote-reply messages fetch one hop of their referenced message (a REST GET via the `discordOps` seam, mention parity); on fetch failure log + continue without it (never abort); its text joins the fast-path union, its images join the download union, and its content is quoted in the prompt between `<<<REFERENCED MESSAGE>` markers.
- **Slow path (learning)** paragraph: the ask carries the message's downloaded images (attachments + embed images, mirror-leg, per-URL failure logged + skipped, degrades to text-only when nothing downloads) when any; add a referenced sentence (the prompt quotes the fetched referenced content); update the gate to the two-arm semantics — the word must be a token of the (union of posted + referenced) text WHEN that text has tokens; a message with no text tokens at all is `wordValid`-only (decided trade-off, bounded risk: a wrong word only deletes the gated user's own future messages containing that word).
- **Known limitations**: remove the image-blindness implication; ADD: "Image downloads are best-effort — a per-URL failure skips that URL (logged); when nothing downloads the ask degrades to text-only. No image-size/count cap beyond Discord's own message limits (10 attachments / 25 boosted); a 10-image message burns one heavier (base64) pi ask." AND: "Referenced-message fetch is one hop (mention parity) — chains are not followed; a fetch failure (deleted/rate-limited) degrades to judging the posted message only, so a gimmick that lives ONLY in a deleted quote passes through."
- The production-acceptance paragraph at the end (production acceptance) gains the reference + image cases (condensed).
- Front-matter: `last-verified` → the ship date; `verified-by` ADDS: `go test ./internal/handlers/derpies/ -count=1 -short green`.
- Remove any line that says derpies is blind to images / text-only (the premise is gone).

**Steps:**
- [ ] Run `go test ./... -count=1` (PG-dependent tests skip cleanly; the `cmd/tugbot` ~30 s bounded-pool test always runs) — all pass or skip
- [ ] Run `golangci-lint run` — no issues
- [ ] Run `go vet ./...` — clean
- [ ] Make the feature-note edits per the section above
- [ ] Commit with message: "docs: derpies feature note — image + referenced-message support"

**Acceptance criteria:**
- [ ] `go test ./... -count=1` green
- [ ] `golangci-lint run` + `go vet ./...` clean
- [ ] `docs/features/derpies.md` documents the image support + referenced handling + two-arm gate, and no longer claims image-blindness

---

## Out of scope (do not implement)

- Refactoring the mention package's image leg / referenced fetch into a shared package (mirror, don't DRY — house convention).
- Following reference CHAINS (beyond one hop — mention parity).
- Image size/count caps beyond Discord's own message limits, image format filters (webp/gif pass through), or exif/OCR pre-scrubbing.
- Any new seam on the `Derpies` struct (`downloadImages`/`downloadPlan` stay plain methods; tests use httptest); any change beyond the ONE added `discordOps` method.
- Anything in `main.go`, `internal/pirpc`, `internal/config`, migrations, or the mention handler.
