---
status: accepted
date: 2026-09-18
superseded-by:
---

# Derpies — the slow-path verdict is a SCORE (0–100) + optional WORD, not a binary GIMMICK/CLEAN

## Problem

The 2026-09-18 prod observation (channel 1044752345583599626, message
1550528637319512095): the filtered user posted

> `<@…> 💸 <:derpies:…> 🚵`

— a mention + a money emoji + his custom "derpies" emoji (the person he
pretends to be) + a cyclist emoji. The meaning is a **combo**: *buy me a
bike* = `zwift`. The message survived, and the failure is structural, at
**two layers at once**:

1. **Fast path miss.** `tokensForMatch` yields exactly two tokens —
   `272889785318768641` (the mention snowflake) and
   `derpies:1021692390177775657` (the custom-emoji ref). The emojis fold to
   nothing (they are `So` symbol runes, trimmed by `edgePunct`). Neither
   token is a stored word → no zero-ask delete.
2. **Verdict dead end.** The LLM judged `CLEAN` (it cannot decode the combo —
   the middle emoji is a personal reference it has no way to know), but even
   a correct `GIMMICK:zwift` could never act: the step-9 gate (ADR 0008)
   requires the verdict word to be a token of the message, and the only
   tokens are the snowflake (all-digit → rejected by `allDigitVerdict`) and
   the emoji-ref (not a word shape → rejected on both arms). So the message
   could **never** be deleted; `CLEAN` was the only possible outcome.

The deeper insight (operator): a single emoji (💸, 🚵) is **harmless alone**;
the **combo** is the gimmick. A deterministic matcher (fast path, phrases)
sees tokens, not meaning — only a whole-message semantic judgement can score
a combo. And the binary `GIMMICK`/`CLEAN` verdict has no middle ground: the
prompt's "when torn: GIMMICK" stance forces an all-or-nothing call with no
calibrated confidence, so there is no dial to tune as the evasion evolves.

## Decision

The slow-path verdict becomes a **score** plus an optional **word**:

```
SCORE:<0-100>
WORD:<anchor>        (optional — the evidence for learning)
```

- **SCORE** is the LLM's calibrated "how much of a gimmick is this" — the
  combo judgement (💸 alone ≈ 10, 💸+🚵 ≈ 90). The binary `GIMMICK`/`CLEAN`
  is **removed**; the score subsumes it and kills the contradiction case
  (a `GIMMICK` label + a low score).
- **The code's decision matrix** (slow path):

  ```
  score < 40   (learn floor, code constant):   do nothing
  40 ≤ score < T  (delete threshold, in DB):  learn the word (if valid+anchored); no delete
  score ≥ T:                                 learn the word (if valid+anchored) + delete
  ```

- **T** lives in a new single-row `derpies_config` table
  (`delete_threshold`, default **50**, `updated_at`) — fetched per-message
  like the prompt and the list, live-tunable, **no deploy**. Fetch failure →
  code default (the existing degradation discipline).
- The **learn floor (40)** is a code constant: "is there a real trace worth
  remembering" is stable semantics, not an operator dial. Default T=50 > L=40
  is a small step *more* conservative than today: a 45-score message stays up
  but gets learned → the next occurrence is a fast delete (self-healing).
- **The anchor gate (the emoji fix):** "anchored" now means *the word is a
  token of the message, checked only when the message has word-like tokens* —
  word-like = a valid non-digit word (`wordValid` and not `allDigitVerdict`)
  or a valid non-ASCII shape (`unicodeVerdictShape`). A mention snowflake /
  emoji-ref / pure-emoji token is **not** word-like, so a "mention + emoji"
  message is judged like an image-only post: a valid word passes, the score
  decides. Emojis are never stored (harmless alone, unenumerable in combo) —
  they are always slow-path, always scored.
- **Fast path unchanged:** a known word/phrase = delete, zero asks, no score.

## Considered options

- **Keep the binary verdict + score alongside** — rejected: the contradiction
  case (`GIMMICK` label + low score) forces an arbitrary decision matrix; the
  score already encodes the binary call, so keeping both is redundant.
- **A single threshold gating both learn and delete** — rejected: a consistent
  sub-threshold evasion class would never be learned, so it would never become
  a fast hit — a permanent leak. The two-threshold design self-heals (a
  sub-threshold trace is learned, the next occurrence is a fast delete).
- **T in an env var** — rejected: changing it needs a deploy, defeating the
  "improve over time" goal. T is a live dial by design.
- **Store emoji / emoji-combos as fast-path words** — rejected: the word
  contract (`^[a-z0-9]{2,32}$` / letters-only non-ASCII) cannot hold an emoji,
  and a combo is unenumerable (the user swaps one lookalike emoji and the
  exact-match misses). The score is the only layer that judges a combo.

## Consequences

- Emoji posts are **always slow-path** (one pi ask each); they can never be a
  fast hit.
- A sub-threshold trace (40 ≤ score < T) is **learned but not deleted** — the
  message stays up once, the next identical occurrence is a zero-ask fast
  delete.
- The adversarial stance ("when torn: GIMMICK") moves from the prompt into the
  **threshold** (the operator's live dial). The prompt still scores
  adversarially; T is where the operator accepts the risk.
- The prompt, the verdict parser (`parseVerdict` → a score parser), and the
  step-9 gate all change together. The `derpies_prompt` code default, the
  migration seed, and the live row must stay in sync (the ADR 0003 /
  `TestMigration000003AppliesAndSeeds` sync guard).
