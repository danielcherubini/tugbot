---
status: accepted
date: 2026-09-03
superseded-by:
---

# Use bwmarrin/discordgo over the maintained dgo fork

tugbot is a text-only Discord bot (gateway message/reaction events, REST for message edits, reactions, slash interactions, member roles, reaction pagination). `darui3018823/dgo` is a maintained hard fork (v0.30.2, 2026) with modern ergonomics (slog logging, context-aware REST, X-RateLimit-Bucket rate limiting), but it has a single maintainer and no community. We chose the canonical, slower-cadence `bwmarrin/discordgo` because it covers 100% of a text-only bot's needs, is battle-tested, and carries no learning cost — staleness only bites when Discord deprecates something, which doesn't affect the features we use.

**Considered Options**: dgo (the maintained fork) was rejected on bus-factor risk and non-canonical docs; a two-library pilot was rejected as extra work for marginal comparison data.

**Consequences**: If bwmarrin/discordgo ever breaks on a Discord-side change, the documented escape hatch is `dgo` — it is a fork, so its APIs are 1:1 compatible and no rewrite is required.
