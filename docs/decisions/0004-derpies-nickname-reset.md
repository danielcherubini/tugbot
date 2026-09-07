---
status: accepted
date: 2026-09-07
superseded-by:
---

# Derpies filter watches nickname changes; "reset" = clearing the nickname

The derpies filter previously matched message content only, so a barrage user could move a
gimmick word into their per-server nickname and the filter was blind (mentions render as
`<@id>` and tokenize away). We decided the filter watches `GuildMemberUpdate` for the
gated user(s) and "resets the name to default" by CLEARING the nickname: Discord's member
update accepts `"nick": null` (verified empirically against the real member on 2026-09-07,
where the member PATCH is permitted and the display then falls back to the global
display name). Note: discordgo's `GuildMemberParams.Nick` is an `omitempty` string and
cannot send null, so the reset is issued via the raw `Session.Request` PATCH.

## Considered Options

- **Fast-path-only nickname matching** — rejected: denylist words are respellable by
  design; consistency with the message flow means the slow pi-RPC path judges misses.
- **Separate package for the nickname flow** — rejected: it shares ~90% of the derpies
  plumbing (gates, store, prompt, verdict, sequenced discipline) — the package-storm's
  package-per-handler rule still holds: one handler, two events.
- **Race every re-set (no cooldown)** — rejected: the user can re-set immediately; an
  unbounded loop burns REST + RPC budget against Discord's own nickname-edit limits.
- **Per-member cooldown, no timer** (chosen): at most one reset per member per 60 s.
  No timer is needed — a ping-pong produces a stream of events (the next event re-checks
  the live state), and a quiet window means the last reset already cleared the name. A
  reset is a value-only write ("set to global name"), so a pending write is always
  current. Discord-go v0.29.0's `Member` carries neither `ActorID` nor `OldNick`, so
  both the change detection and the bot-echo filter run on an in-process last-nick
  cache: events whose nick equals the cache are skipped, and a successful reset
  updates the cache BEFORE the gateway echo arrives — the bot's own edit can never
  re-judge itself (no recursion, no wasted RPC).

## Consequences

- Only gated users (`TUGBOT_DERPIES_USER_IDS`) are ever checked; the `derpies` feature
  flag gates both flows (the `/gimmick` command stays ungated per 0003, so the list is
  still pre-seedable while the filter is off).
- A cleared nickname still displays the global name — if the user's GLOBAL name itself
  contains a word in the list, clearing cannot hide it (no API touches the global name);
  the clear bakes in no nickname, so the closed vector is not re-opened by the reset.
- The message filter was intentionally NOT extended to look at mentions/nicknames inside
  messages — the nickname-ITSELF vector is closed here; the mention vector remains a
  separate known gap.
