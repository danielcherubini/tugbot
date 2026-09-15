---
status: accepted
date: 2026-09-15
superseded-by:
---

# Derpies verdict gate — accept non-ASCII verdict words when text-anchored

## Problem

The derpies filter is a two-layer filter: a fast path (exact folded-token match
against `derpies_gimmicks`) and a slow path (a pi RPC verdict whose
`GIMMICK:<word>` result learns `<word>` and deletes the message). The pi
prompt is shown the message text VERBATIM — and the observed evasion (prod
logs 2026-09-15 06:53/06:54 UTC) is a non-Latin-script one:

> `GIMMICK:خفيف` → `derpies invalid verdict word — doing nothing`

The user is evasion-testing the free-bicycle gimmick in Arabic script —
«خفيف» ("light"), «سريع» ("quick") — while the Latin-script transliterations
(`sarie`, `rapida`, `wift`-class variants) the LLM named for the earlier
probes were all door-learned and deleted. The structural reason the native
script could not act: `wordmatch.WordValid` is
`^[a-z0-9]{2,32}$` — pure ASCII by design (it is the stored-space contract).
`FoldToASCII` NFD-decomposes, drops Mn/Cf, folds confusable lookalikes,
and lowercases — but a letter with NO Latin confusable (the rest of a
non-Latin script, e.g. Arabic) passes the fold UNCHANGED, so
`FoldToASCII("خفيف") == "خفيف"` and `WordValid` rejects it. The step-9 gate
logged "invalid verdict word — doing nothing" and the
gimmick survived — while the same probe in Latin letters was caught. The
non-Latin dead end is a pure gate artifact, not a storage problem: `listGimmicks`
fetches `SELECT word FROM derpies_gimmicks` and the fast path matches by
exact folded token, so a stored non-ASCII word IS catchable the moment it is
learned.

## Decision

The step-9 gate (message flow only — the nickname flow's two-arm gate is
unchanged) is extended to a two-arm gate with a shared prefix:

- **Shared prefix** — a PURE digit string is NEVER a valid verdict word (the
  twist word must contain at least one letter): `GIMMICK:12345` is invalid on
  both arms, even though it satisfies `WordValid`'s charset.
- **Arm (a) ASCII** — `wordmatch.WordValid(fw)` (the unchanged
  `^[a-z0-9]{2,32}$` contract), plus: when the (posted + referenced +
  embed-title union) text has tokens, the folded word must have appeared as a
  folded token of that text (same tokenization as the fast path). A message
  with NO text tokens at all (image-only) stays bounded by `WordValid` alone
  — image-anchored words are bounded by charset only, unchanged.
- **Arm (b) non-ASCII** — for a word whose folded form is NOT `WordValid`
  (no-confusable scripts), the only accepted path is TEXT-ANCHORED: the word
  must be a plausible word shape (letters-only — every rune in the COMBINED
  Unicode L category: no whitespace, no punctuation, no digits — 2..32 runes,
  which already implies at least one letter) AND a verbatim folded token of the
  judged text (the `toks[fw]` hit; the fold leaves a no-confusable script
  unchanged, so a verbatim «خفيف» in the message hits that exact key).

A learned word is the FOLDED word (`derpies_gimmicks` still stores the folded
form — for a no-confusable word that is the same string); it is `INSERT ... ON
CONFLICT (word) DO NOTHING` with `source='llm'`, then the message is deleted.
The fast path, the store, the prompt template, and the `WordValid` regex are
all unchanged (WordValid is the stored-space contract; the relaxation lives
in the gate, not in `wordmatch`).

## Considerations

- **The ASCII stored-space invariant vs the non-Latin dead end.** The
  strongest counter-argument was to keep the list pure-ASCII: a third surface
  (the learned-word space) would now be non-ASCII, and every future tooling
  assumption ("the list is ASCII") would quietly become false. The decision
  weighs it the other way: the invariant is only ONE invariant (in the store
  SQL, not in any API — `listGimmicks` returns a `map[string]bool` keyed by
  stored word, and the fast path is an exact `list[tok]` hit), the dead end is
  a demonstrated evasion vector (probes survived for the whole observation
  window — 06:53–06:54 UTC — while the Latin-evading same probes were deleted),
  and because the fold is a fixed point for no-confusable scripts, a learned
  Arabic word is immediately caught by the existing fast path — the learned
  word is a fast hit on the next verbatim post, with zero further pi asks.
- **The image / frame-only arm is NOT relaxed.** A non-ASCII verdict word
  still requires `toks[fw]` (a token of the posted + referenced + embed-title
  union text), so a word that appears ONLY in a frame of a textless post is
  still rejected (the ADR 0007 dead end is unchanged — the URL-only
  relaxation is still operator decision pending). This keeps the fast path,
  the store shape, and the image-leg semantics exactly as they were; the only
  accepted word that was previously structurally rejected is a word the
  filtered user POSTED himelf in text.
- **The shared all-digit prefix also changes arm (a) slightly**: `GIMMICK:12345`
  on an image-only post used to be learned (pure-ASCII chars, `WordValid`
  true); it is now rejected (a digit string is never a twist word). This is
  intentional — a test word should contain at least one letter — and is trivially
  capped (the prefix is applied to both arms before any arm is checked).
- **Hallucination does not enter the list** — via any arm: a fabricated ASCII
  word on a texted post is still rejected by the token gate, and a fabricated
  non-ASCII word is rejected by arm (b)'s `toks[fw]` requirement (it must be
  a token the user actually posted in that message's text union). At most the
  filtered user still teaches a word that he posted himself.

## Live operation (context, 2026-09-15)

While the gate was still the ASCII-only one, seven manual seeds were inserted
to cover the observed evasion until this ADR shipped (2026-09-15T09:20:47Z):
`خفيف`, `خفيفة`, `khafif`, `khfif`, `khafeef`, `سريع`, `saree` (i.e. the
native-script anchors and their transliterations). After this change, the
native-script words are also caught through LLM learner in a verbatim post,
and the Latin transliterations are caught by the fast path. The manual seeds
are kept: the transliterations are captured variants that the gate would not
necessarily re-evaluate (the LLM might name the native form instead), and they
are `source='seed'` rows — the manual list remains a documented operational
tool.
