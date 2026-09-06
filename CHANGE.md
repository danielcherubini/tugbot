## 2026-09-06

### derpies: images + referenced messages (squash 1c34f08)
- The derpies filter now sees what text cannot: attachment + embed images are downloaded (isSafeURL-guarded, per-URL failure logged + skipped, mention-parity leg) and join the pi ask via AskWithImages — the prompt's {{IMAGES}} marker substitutes the image-count line. A GIMMICK verdict on an image-only message learns the word (wordValid gate only); a word valid but absent from a text+image message's text and its quote is neither deleted nor learned.
- Reply/quote-reply messages fetch one hop of the referenced message (ChannelMessage REST GET via the discordOps seam); its text joins the fast-path token union (a reply re-quoting a seeded word fast-hits without retyping), its images join the download union (URL-deduped), and its content is quoted between <<<REFERENCED MESSAGE / REFERENCED MESSAGE>>> markers ({{REF}}). Fetch failure (deleted/rate-limited) degrades to judging the posted message only — logged, never aborts the flow.
- Learned-word gate is two-arm on the FOLDED word: wordValid always; the folded word must be a token of the (posted + referenced) text when that text has tokens.
- The flow's only outgoing Discord REST remains DeleteMessage + the referenced data GET; no bot message, reaction, or gulag on any path.
- Code: internal/handlers/derpies/derpies.go, derpies_images_test.go (new, 16 tests), derpies_test.go (fake shapes); docs/features/derpies.md updated. 46 derpies tests green.

### derpies: unicode fold + live prompt (DB) (squash 6318252), shipped earlier today
- Token space folded: unicode respellings (świft → swift, žwift → zwift) fast-hit seeds and are learned under their folded ASCII form.
- The verdict prompt is long-form adversarial ("HE WILL TEST THIS FILTER") and lives in the single-row derpies_prompt table (migration 000003) — editable via psql with no deploy; code-pinned default is the fallback.
- Migration 000004 seeds the cog respellings (cog, cogs, coggs, c0g, c0gs, coq, coqs, kog, kogs) into derpies_gimmicks.
