---
status: accepted
date: 2026-09-10
superseded-by:
---

# Derpies repeat-image fast delete (4.6) removed — re-posts are re-judged, never blind-deleted

The derpies filter shipped a repeat-image fast delete on 2026-09-08 (commit
`bae6467`, flow 4.6): after any completed LLM ask, image CONTENT (sha256 of the
downloaded bytes) was memorized in an in-process cache (24h TTL, bounded at 2048
entries, oldest-evicted), and a message whose content was image-only and whose
images were all previously seen was deleted with ZERO asks. A journal incident on
2026-09-10 — a filtered user's same-bytes PNG re-posts, the original having been
LLM-judged clean, that disappeared in under a second with zero asks — made the
operator decide that images must NEVER be blindly deleted on previously judged
content: a clean verdict on a post is not a standing license to annihilate its
re-posts.

We decided:

1. **A re-posted image is re-judged in a fresh pi ask — there is no zero-ask
   deletion on any image state.** The seen-content cache, the seen-skip
   (already-seen images leaving mixed asks), and the `isEdit` origin are all
   GONE: edits are origin-agnostic, carrying at most one list SELECT + one pi
   ask — the same cost as a create. N re-posts of a previously judged image =
   N bounded asks in the shared pi queue (plus N list SELECTs); the cost is
   accepted, and each ask is bounded per-ask by the pirpc per-ask image guard
   (byte-identical dedupe, >1MB / >2048px resize to 2048 / JPEG q80, 12MB
   raw cap — shared with the mention handler). The image leg (4.5: attachment
   + embed download, isSafeURL-guarded, URL-deduped, joining the ask via
   `AskWithImages`) and the word fast path (pre-image-handling token hits)
   are unchanged.

## Considered Options

- **Keep 4.6 as shipped** — rejected: the 2026-09-10 journal incident shows the
  operator does not accept blind image deletions; a previously clean verdict must
  never delete a re-post without a fresh look.
- **Feature-gate 4.6 off by default** — rejected: dead machinery — the
  2048-entry in-memory LRU kept for a path nobody wants on; it would still be
  compiled, tested, and documented, for a decision-inverted default.
- **Keep the cache, drop only the delete arm** (seen-skip still trims
  already-judged images out of the payload) — rejected: it keeps the cache
  machinery the decision discarded, and entangles seen-content semantics with
  no-delete — a future "the cache says seen, so skip it" recurrence is one
  refactor away, for a payload savings the guard already bounds.

## Consequences

- Re-posted images are re-judged in fresh asks; the re-post burst-amplification
  cost is accepted and bounded (per-ask image guard; the shared pi request
  channel is already the mention handler's bottleneck).
- Edits are origin-agnostic: a `GUILD_MESSAGE_UPDATE` cost profile equals a
  create — at most one list SELECT + one ask; the `isEdit` origin flag and the
  edit no-op arm are gone.
- No zero-ask deletion on any image state — the image leg (4.5) and the word
  fast path (pre-image) are unchanged.
- 0005 (edit re-judgment) still stands as written; this decision removes the
  image-cache machinery it recited, not its edit flow.
- The 4.6 tests are deleted with their feature (the derpies test package went
  from 91 to 85; the history record in the feature doc's front-matter is
  preserved as a record).
