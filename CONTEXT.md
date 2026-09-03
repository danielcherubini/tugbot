# tugbot

A Discord bot for a crappy discord — the Go port of tugbot-rs. Shares the Rust bot's Postgres DB, bot token, Application ID, and guild roles; it replaces the Rust binary on the same host, not alongside it.

## Language

**App**:
The single struct holding `*discordgo.Discordgo`, `*pgxpool.Pool`, `*PiRpc`, and `Config`. Replaces Serenity's three TypeMap keys (`DbPoolKey`, `ConfigKey`, `PiRpcKey`) — every handler is a constructor that takes `*App`.
_Avoid_: TypeMap, context plumbing, DI container

**Re-scoped port**:
The Go rewrite ports the 11 **live** handler modules/packages of the Rust bot (including the gulag package; the command shapes: 7 slash + 2 message-context-menu + the goku message-update + the reaction voting) and drops dead code: `tiktok` and `elkmen` (already disabled in Rust) and `elon` and `derpies` (defined in Rust but never dispatched from `mod.rs`, and no feature-flag rows in any migration). It is not a 1:1 port; behavior parity is matched to live behavior only.
_Avoid_: 1:1 port, literal port, full rewrite

**Baseline migration**:
The single migration file representing the entire pre-cutover schema (diesel history treated as settled fact). Go-owned migration history begins with it; the Go runner stamps it applied on first run over a live DB without executing the DDL.
_Avoid_: schema snapshot import, diesel history

**Cutover**:
The production switchover: stop the Rust systemd unit, point the unit at the Go binary, start. The Rust binary and old unit file are kept ~2 weeks for one-click rollback. There is no shadow/gray period — the token allows a single gateway connection.
_Avoid_: migration, gray rollout, shadow bot, blue-green
