# tugbot

A Discord bot for a crappy discord — the Go port of tugbot-rs. Shares the Rust bot's Postgres DB, bot token, Application ID, and guild roles; it replaces the Rust binary on the same host, not alongside it.

## Language

**App**:
The single struct holding `*discordgo.Discordgo`, `*pgxpool.Pool`, `*PiRpc`, and `Config`. Replaces Serenity's three TypeMap keys (`DbPoolKey`, `ConfigKey`, `PiRpcKey`) — every handler is a constructor that takes `*App`.
_Avoid_: TypeMap, context plumbing, DI container

**Re-scoped port**:
The Go rewrite ports the 11 **live** handler modules/packages of the Rust bot (including the gulag package; the command shapes: 7 slash + 2 message-context-menu + the goku message-update + the reaction voting) and drops dead code: `tiktok` and `elkmen` (already disabled in Rust) and `elon` and `derpies` (defined in Rust but never dispatched from `mod.rs`, and no feature-flag rows in any migration). It is not a 1:1 port; behavior parity is matched to live behavior only. The Go bot additionally introduces the derpies filter as its own original feature: the Rust `derpies` module was a never-dispatched stub, so the Go handler is a NEW feature, not a port (see **Derpies filter**).
_Avoid_: 1:1 port, literal port, full rewrite

**Gimmick word**
An obfuscated token (e.g. `sw1ft`) that Derpie uses to evade the derpies filter's word matching. Persisted in the `derpies_gimmicks` table with `source` distinguishing `seed` (migration-seeded), `llm` (learnt at runtime via the pi RPC verdict), and `manual` (curated at runtime via the `/gimmick` slash command). Matched by lowercase, punctuation-trimmed exact token match — never substring.
_Avoid_: blocklist entry, banned word, filter entry

**Derpies filter**
The Go bot's own original feature: silently deletes messages from author IDs in `TUGBOT_DERPIES_USER_IDS` via a fast-path token match against `derpies_gimmicks`, falling back on a fast-path miss to a pi RPC `GIMMICK:<word>` / `CLEAN` verdict that persists valid words back into the table. It also re-judges `GuildMessageUpdate` by a gated author — the full flow on the updated content (at most one ask per edit). The slow-path prompt carries the FULL known gimmick list (already in memory from the fast-path fetch) so the LLM pattern-matches respellings against the known family — the list lives in the DB, not in a static skills/ file. The only action is `DeleteMessage` — no bot response, no reaction, no gulag involvement on any message- or edit- path (the nickname flow's action is the member-reset PATCH — see **Nickname reset**). Repeated images are re-judged, never fast-deleted: the 2026-09-08 repeat-image fast delete (flow 4.6 — seen-content cache, zero-ask re-post delete) was REMOVED on 2026-09-10 (operator decision 2026-09-10) — a post carrying an already-judged image (or a re-post of one) goes into a fresh pi ask bounded by the per-ask image guard. The image leg (4.5: attachment + embed download, URL-deduped) is kept; no image post is ever fast-deleted on account of its content having been previously judged (the separate word fast path, which runs before any image handling, is unchanged).
_Avoid_: mod action, anti-spam handler, derpies handler, repeat-image fast delete, seen-image cache

**Nickname reset**
The derivative of the Derpies filter on `GuildMemberUpdate`: when a gated derpies user changes their per-server nickname, the new nickname is judged the same way as a message (fast-path token match against `derpies_gimmicks`, then the slow pi-RPC verdict); a GIMMICK verdict resets the nickname to the fixed neutral name `Derpies` (`PATCH .../members/{member} {"nick":"Derpies"}` — a null-clear is not used: it would expose the global display name, which the bot cannot modify); the reset action fires on any parseable GIMMICK verdict (the learning gate — wordValid + folded-token-in-nick — now gates learning only). Resets are per-member coalesced: at most one reset attempt per 60-second window (success or failure marks the window), and the bot's own reset is never re-judged (a successful reset records the reset value in the cache BEFORE the gateway echo arrives). The `derpies` feature flag gates both the message flow and this one.
_Avoid_: nickname ban, display name reset, member rename

**Baseline migration**:
The single migration file representing the entire pre-cutover schema (diesel history treated as settled fact). Go-owned migration history begins with it; the Go runner stamps it applied on first run over a live DB without executing the DDL.
_Avoid_: schema snapshot import, diesel history

**Cutover**:
The production switchover: stop the Rust systemd unit, point the unit at the Go binary, start. The Rust binary and old unit file are kept ~2 weeks for one-click rollback. There is no shadow/gray period — the token allows a single gateway connection.
_Avoid_: migration, gray rollout, shadow bot, blue-green

**MCP layer**:
The in-process `internal/mcp` package: an always-on, zero-config Streamable-HTTP MCP server embedded in the tugbot process that shares the bot's single `*discordgo.Session` (the embedded-provider pattern — no second session, since the token allows one gateway connection). Agents (pi, Claude, …) connect to `:8642/mcp` and act as the bot.
_Avoid_: MCP sidecar, MCP daemon, MCP plugin

**Bridge tools**:
The five v1 raw-Discord MCP tools: `list_guilds`, `list_channels`, `read_messages`, `post_message`, `react` — thin wrappers over session methods, no Postgres, no Pi.
_Avoid_: MCP tools (unqualified), Discord API tools

**Feature tools**:
Follow-on MCP tools wrapping the bot's feature handlers (feature toggles, gimmick, gulag, ask-pi) — distinct from bridge tools; each needs a small `Invoke`-style public surface on the event-callback-shaped handlers.
_Avoid_: feature MCP, command tools
