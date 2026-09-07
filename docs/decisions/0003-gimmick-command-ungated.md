---
status: accepted
date: 2026-09-07
superseded-by:
---

# The /gimmick management command is not feature-flagged

Every Go-native slash command (`gulag`, `feature`, `cull`, `ai-slop`) gates itself on its own `features`-table flag. The `/gimmick` command (add / list / delete over `derpies_gimmicks`) deliberately does not: it is protected only by the guild guard and the role gate (Highly Regarded + admin). Reason: the gimmick word list is the filter's only learned state, and gating the command on the derpies flag would make the list unmanageable precisely while the feature is off — pre-seeding and pruning before re-enabling is a real use case, and an empty list would make the LLM slow-path relearn from scratch. The write cost is covered by the existing role gate.

**Considered options**: (1) Gate all three subcommands on the derpies flag — rejected: blocks pre-seeding and makes the list unmanageable during feature-off. (2) Gate writes only (add/delete), list ungated — rejected: the command's behavior would vary per subcommand, and the flag's cost was not justified for a read.

**Consequences**: `/gimmick` remains usable when the derpies feature flag row is missing or false. Any gate change to this command goes through the role gate and the guild guard — a feature flag is intentionally not part of its surface.
