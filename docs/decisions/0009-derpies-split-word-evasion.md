---
status: accepted
date: 2026-09-17
superseded-by:
---

# Derpies — catch the SPLIT evasion (a word spread over spaces)

## Problem

The 2026-09-17 prod observation (channel 840674638375551047, message
1550204691059839039): the filtered user began evading the `zwift` gimmick by
spreading the word over spaces —

> `I want to make sure you get the points and free dlc for recommending the z w i f t`
> `Hey <@…> when can I expect my z w I f t`

— and the messages survived. `zwift` is a SEED word (`derpies_gimmicks`,
2026-09-07), yet the post was not deleted. The structural dead end is the
tokenization: `tokensForMatch` runs `strings.Fields`, so `z w i f t` yields the
five single-rune tokens `{z, w, i, f, t}` — never the collapsed `zwift`. Two
consequences:

1. **Fast path miss.** The fast path is an exact `list[tok]` hit; `zwift` is
   not in the token set, so no zero-ask delete — even though `zwift` is seeded.
2. **Verdict dead end.** The slow path's LLM judges the post a GIMMICK (the
   `SPLIT` class is already named in the prompt), but the step-9 gate (ADR
   0008) rejects the verdict word on BOTH arms: `z w i f t` (the as-appears
   form) fails the shape checks (it contains whitespace — not `wordValid`, not
   `unicodeVerdictShape`), and `zwift` (the collapsed form) fails the token
   anchor (`toks["zwift"]` is false — it is not a token of the message under
   the old tokenization). So the verdict can never act: the word is never
   learned and the message is never deleted.

This is the same shape as the ADR 0007 / 0008 dead ends (a respelling for which
no valid verdict exists), now in the SPLIT variant.

## Decision

Two coordinated changes, both in the folded-token space (the store, the
`wordmatch.WordValid` contract, and the prompt's mandatory markers are all
unchanged):

- **`tokensForMatch` also yields collapsed runs of single-rune tokens.** A run
  is a maximal stretch of consecutive tokens whose FOLDED form is a SINGLE
  rune; a token that folded to `""` (pure punctuation) does NOT break the run
  (a dot wedged between letters is part of the split, not a word boundary); a
  multi-rune token breaks it. A run of `>=2` single-rune tokens contributes its
  collapsed (space-free) form, capped at 32 runes (`wordValid`'s max — each run
  element is one rune, so the collapsed form has exactly `len(run)` runes and
  can never match a stored word beyond the charset bound). The collapse is
  ADDITIVE — the individual single-rune tokens stay in the map. So `z w i f t`
  yields `{z, w, i, f, t, zwift}`. Because the fast path, the step-9 gate, and
  the nickname flow ALL share `tokensForMatch`, this one change: makes the fast
  path delete `z w i f t` with zero asks (the seed `zwift` now hits), gives the
  step-9 gate an anchor for a new split word (`toks["zwift"]` is true), and
  makes a split nickname fast-reset.

- **The step-9 gate collapses whitespace in the verdict word.** The verdict word
  is folded AND its whitespace collapsed (`foldVerdictWord` — `FoldToASCII`
  drops Mn/Cf but NOT regular spaces, so the collapse is explicit) before the
  two-arm gate. `GIMMICK:z w i f t` normalizes to `zwift`, passes the shape
  checks, is anchored to the collapsed run, and LEARNS the collapsed form. This
  makes the learning path robust to LLM compliance on the answer shape (the
  LLM may answer the spaced form or the collapsed form; both learn `zwift`).

- **Prompt (code default + live `derpies_prompt` row, kept byte-identical):** a
  new answer-rule bullet — for a SPLIT word, answer the COLLAPSED form
  (`"z w i f t" -> "zwift"`), never the spaced form. This aligns the LLM's
  output with the token space (the learned word is the collapsed form) and is
  the belt to the gate-collapse's suspenders.

The learned/stored word is the COLLAPSED form (`zwift`), so the next identical
split post is a fast delete with zero asks.

## Considerations

- **False-positive surface is bounded and adversarially acceptable.** The fast
  path runs ONLY on the gated (filtered) user, so a false positive deletes at
  most one of the spammer's own messages. A collapsed run matching a stored
  word requires the spammer to have typed that word's letters as separate
  single-rune tokens — which IS the evasion. A run of `>=2` single letters
  collapsing to a stored word is a legitimate catch; the seed list has no
  2-letter words, so the `>=2` floor adds no real risk. The collapsed run is
  capped at 32 runes, so it can never match a word beyond `wordValid`'s bound.
- **The anchor is preserved — no hallucination hole.** A collapsed form passes
  the gate ONLY when it is a token of the message (the collapsed run in
  `tokensForMatch`), which requires the message to actually contain the split.
  A hallucinated `zwift` on a message with NO `z w i f t` is still rejected
  (`toks["zwift"]` false) — the collapse does not create an anchor out of
  nothing. The gate-collapse likewise: `GIMMICK:z wift` (one space) collapses to
  `zwift`, but `z wift` yields tokens `{z, wift}` (no collapsed run — `wift` is
  multi-rune), so `toks["zwift"]` is false and it is rejected.
- **The `WordValid` stored-space contract and the fast-path exact match are
  unchanged.** The relaxation lives in `tokensForMatch` (the token space gains
  collapsed forms) and the gate (whitespace collapse), not in `wordmatch`.
  `WordValid` stays `^[a-z0-9]{2,32}$`; the collapsed forms are simply additional
  keys in the token map.
- **Remaining SPLIT dead-end (out of scope here): punctuation-wedged single
  tokens.** A word wedged with INTERIOR punctuation in a SINGLE token
  (`s.w.i.f.t`, `s/w/i/f/t`) is NOT caught by this change — `tokensForMatch`
  trims edge punctuation only, so `s.w.i.f.t` stays one token and does not
  collapse. The SPLIT class (spaces between letters) is closed; the
  punctuation-wedged class remains a structural dead-end (the prompt names it,
  the LLM judges it GIMMICK, but the verdict `swift`/`s.w.i.f.t` cannot anchor).
  Closing it would require interior-punctuation collapse in `tokensForMatch` — a
  larger semantic change deferred as a follow-up.

## Live operation (context, 2026-09-17)

`zwift` was already a seed (2026-09-07), so the `tokensForMatch` change alone
makes the observed `z w i f t` posts a zero-ask fast delete on the next
occurrence — no new `derpies_gimmicks` row is required for the observed case.
The gate-collapse + prompt bullet extend the fix to NEW split words (not yet in
the list): the LLM judges them GIMMICK, the verdict (spaced or collapsed) is
anchored to the collapsed run and learns the collapsed form, and the next
identical post is a fast delete.
