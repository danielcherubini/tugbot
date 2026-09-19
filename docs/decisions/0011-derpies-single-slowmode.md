---
status: accepted
date: 2026-09-19
superseded-by:
---

# Derpies single-slowmode gate deletes over-limit posts without judging them

A gated derpies author was fast-signalling channels with one-word posts; the pre-existing flow (and spec) deliberately had no per-author rate limiting, so each burst word cost a full pi ask. We decided on an enforced slowmode that *deletes* the 3rd+ single-token post inside a rolling 30 s window (per author, per channel) — immediately, before the fast path, with zero pi asks — rather than silently skipping the judgement or warn-and-delete: the point is to stop the spam and its LLM cost, and the operator chose deletion over silence.

**Considered options** (rejected):

- *Silent skip* (no judge, no delete, no ask): zero LLM cost, but the spam stays visible and a real gimmick word in the burst is never cleaned.
- *Delete + warn*: contradicts the bot's silence axiom (no responses, no reactions, no trace).

**Consequences:** the bot's only action is still `DeleteMessage`, but a message can now be deleted *without* the flow ever seeing its content (audit row: `path = 'slowmode'`, NULL score/threshold/word). The counter is in-memory on the handler and resets on bot restart (v1 accepted — a burst is a ≤30 s event). Edits (the re-judgement flow) are intentionally out of gate scope for v1.
