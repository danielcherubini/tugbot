---
status: accepted
date: 2026-09-08
superseded-by:
---

# Derpies re-judges message edits; the name reset is "Derpies" (not a clear)

Two evasions were observed against the shipped derpies filter. First, the filter ran on
`MessageCreate` only — an edit of an already-posted message was a second look-free window: the
slow path judged the ORIGINAL text, and the author could rewrite that same message to carry a
gimmick with no create-time check re-running (only gokupoll watched the update event). Second,
the nickname reset (0004) cleared the nick to null: the display then falls back to the member's
GLOBAL display name — a name the bot has no API to modify — so a dead-end-as-appearance gimmick
name (as-appears word not foldable/learnable, which the 0004 gate left untouched) or a swap of
the global name itself surfaced the gimmick right back.

We decided two things.

1. **Every `GUILD_MESSAGE_UPDATE` by a gated author re-runs the full create flow on the updated
   content** — fast path, images/repeat-image, one-hop reference, one slow ask, learn, delete all
   carry over; an edit costs at most one list SELECT + one pi ask (same as a create). A bare
   update payload — no text AND no attachments AND no embeds — is fetched by channel+id
   through the existing `channelMessageRetrieve` seam (the gokupoll-port pattern of
   mod.rs:126-136), and a fetch failure degrades to log + skip, never aborting. The fetch
   trigger is a STRICT, deliberate deviation from the gokupoll precedent, stated and not
   implied: gokupoll fetches on empty content ALONE; derpies also requires NO attachments
   and NO embeds, so an attachments- or embeds-present payload is judged in place — fewer
   REST calls, identical image leg (the seen-image cache makes the fetch redundant). There
   is no echo recursion: the bot never edits messages.

2. **The nickname reset SETS the guild nick to the fixed neutral name `Derpies`** (const
   `derpiesNickReset`) instead of clearing it to null, and the reset action fires on ANY
   parseable `GIMMICK` verdict. The two-arm learning gate (`wordValid` + folded-token-in-nick)
   is UNCHANGED and now gates LEARNING only: a dead-end-as-appearance name is still reset (the
   word simply isn't learned); what may enter `derpies_gimmicks` is not loosened.

This decision EXTENDS 0004 (which is NOT superseded — its file stands as written): the null-clear
remedy is replaced by the fixed-value set, and the learning gate is introverted onto learning
only. The table's discipline is not loosened, and the MESSAGE flow is byte-for-byte unchanged —
an unlearnable word in a message still deletes nothing (there the gate keeps gating AND-deletion).

## Considered Options

- **Cache last content+attachments per message to skip no-op updates** — rejected: new state for
  little gain (an edit by a gated author is exactly the signal we want re-judged, and a fast-path
  hit costs one list SELECT).
- **Decouple the message flow's delete from its learning gate too** (so an unlearnable-word
  message is still deleted on "I recognize it") — rejected/out of scope this pass: the message
  gate is the table's AND the delete's discipline; loosening it changes the documented dead-end
  semantics (revisit separately).
- **Judge/act on the global display name** — rejected for this pass: the bot has no API to modify
  another user's global name at all; the guild nick (the fixed `Derpies` value) is the only lever,
  and it masks the global name.

## Consequences

- An edit by a gated author is judged exactly like a create — same gates, same discipline, at
  most one list SELECT + one pi ask; rapid-edit burst amplification tracks the create profile
  (accepted; no dedup cache was added).
- A recognized gimmick name resets the nick to `Derpies` regardless of learnability; only what
  may enter the table is gated (the `derpies_gimmicks` token space stays pure-ASCII).
- The message flow is unchanged; the `derpies` feature flag gates all three flows (message
  create, message update, member update).