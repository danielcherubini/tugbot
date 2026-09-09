## 2026-09-09

### mcp: embedded MCP Discord bridge — five bridge tools over the bot's shared session
- An always-on, zero-config Streamable-HTTP MCP server inside the bot process (`TUGBOT_MCP_PORT`, default 8642; a mistyped value fails loud at `LoadConfig`). ADR 0004's LAN-trust posture holds — no auth by default, acts as the bot through the single shared `*discordgo.Session`; every REST call passes `WithRetryOnRatelimit(false)` so 429s surface to the tool layer as `rate_limited (retry after …)` (no retry loop; the agent re-calls).
- Five bridge tools: `list_guilds` (state-first, one-page REST fallback), `list_channels` (text + news only), `read_messages` (channel id-or-name, author ID/name OR-filter, 50→100 clamp, snowflake-validated before any REST), `post_message` (1999-rune truncation, no chunking, optional reply via `MessageSend.Reference`), `react` (emoji token passthrough).
- Port conflict at startup: `slog.Error` + exit 1 (never the SIGTERM `Warn` path); SIGTERM drains `Start` cleanly (≤10s shutdown grace); `--selftest` constructs the server without Start (binds nothing).
- Code: `internal/mcp/` (new package, 50+ tests), `cmd/tugbot/main.go` (wiring), `internal/config/config.go` (+`TUGBOT_MCP_PORT`), go.mod (+`modelcontextprotocol/go-sdk` v1.7.0 and indirects). Docs: docs/roadmap/mcp-discord-bridge.md, docs/decisions/0004-mcp-layer-always-open-lan-trust.md.

### derpies: edit re-judgment + the name reset is "Derpies" (not a clear)
- A gated author's `GUILD_MESSAGE_UPDATE` now re-runs the full create flow on the updated content — fast path / images / repeat-image / one-hop ref / one slow ask / learn / delete carry over (at most one list SELECT + one pi ask per edit). Bare payloads — no text, no attachments, and no embeds — fetch via the `channelMessageRetrieve` seam, the same fetch pattern as the gokupoll port of mod.rs:126-136, with the strict trigger deviation stated: gokupoll fetches on empty content ALONE; here an attachments- or embeds-present payload is judged in place (fewer REST calls, identical image leg). Fetch failure degrades (log + skip), never aborts. The bot never edits, so there is no echo recursion.
- The nickname reset now SETS the guild nick to the fixed neutral name `Derpies` (const `derpiesNickReset`) instead of clearing to null — a null clear exposes the member's global display name, which the bot cannot modify; the fixed value masks it. The name reset action runs on ANY parseable GIMMICK verdict (the "as-appears word not learnable" dead end no longer lets a recognizable gimmick name survive); the learning gate (wordValid + folded-token-in-nick) is UNCHANGED and now gates learning only. Message flow is untouched (an unlearnable word there still deletes nothing).
- Code: `internal/handlers/derpies/edits.go` (new), `nicknames.go` (rework), `derpies.go` (seam `clearNickname` → `setNickname`), `derpies_edits_test.go` (new), `nicknames_test.go`/`derpies_test.go` (fake shape), `cmd/tugbot/main.go` (one-line wiring).

## 2026-09-08

### pi rpc: per-ask image guard — dedupe, shrink, budget (fix for the 46MB image-ask dead end)
- Observed: a filtered user's single message with 10 re-uploaded ~4.6MB attachments became one ~46MB base64 pi ask; the one-at-a-time pi agent dead-ended (empty agent_end → "unrecognized verdict"), and every ask queued behind it for those seconds was rejected ("Agent is already processing"), including the shared mention channel.
- `askWithImages` now runs the image pipeline (pirpc.images): byte-identical attachments are deduped (sha256 on the base64), oversized images (>1MB or >2048 on a side) are resized to 2048 and re-encoded JPEG q80, a never-make-it-worse guard keeps the original when the JPEG would be larger, and the total is capped at 12MB raw per ask (excess dropped in order, logged). Any decode/encode failure keeps the original. One choke point: mention and derpies are both bounded.
- An agent_end that completes with an empty assistant response now logs ERROR (req_id) in pi rpc so the agent failure no longer masquerades as an unrecognized verdict; the "" return (mention's existing empty-skip path) is unchanged.
- Code: internal/pirpc/images.go (new), images_test.go (6 tests), pirpc.go (pipeline hook + empty-response log). go.mod: golang.org/x/image (already-cached version; x/sync, x/text bumped).

### derpies: repeat-image fast delete (the re-post IS the gimmick)
- Image CONTENT (sha256, same basis as the pirpc dedupe) is memorized after any completed LLM ask — an ask FAILURE marks nothing, so an un-judged image is always judged and never fast-deleted blind. A message whose content is image-only and whose images are all previously judged is the repeat of a judged post: deleted with ZERO asks (log `derpies delete (repeat image)`). Seen images drop out of mixed asks: fresh images and new text are still judged, seen ones just leave the payload (text-only ask when all images are seen and new text is present). In-memory map (guarded by the handler's existing mutex), 24h TTL, bounded at 2048 entries with oldest-eviction; survives nothing across restarts (a fresh start judges the first sighting again — safe by construction), no table, no migration.
- Code: internal/handlers/derpies/derpies.go (flow 4.6 + cache), derpies_repeat_image_test.go (new, 4 tests). Docs: docs/features/derpies.md burst-amplification note updated.
- Note: the in-memory cache is per-process — a bot restart clears it, and the first re-post after a restart is judged once (one ask) before becoming a fast delete again.

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
