---
name: derpies-gimmick
description: "Update the derpies filter's gimmick word list (derpies_gimmicks) or its live prompt (derpies_prompt): add/remove gimmick words, or change detection behavior. Use when asked to 'update the derpies gimmick', 'add a gimmick word', 'remove a gimmick word', 'manage the derpies word list', or when the derpies filter missed a new respelling."
---

# Derpies Gimmick

Update the derpies filter's **gimmick word list** (the `derpies_gimmicks` table) and, when the detection *behavior* needs changing, its **live prompt** (`derpies_prompt`). This is the operational surface for the derpies filter — the passive handler that silently deletes a gated user's gimmick posts.

The derpies filter is a two-layer filter:
- **Fast path** — exact folded-token match against `derpies_gimmicks` (zero pi asks).
- **Slow path** — a pi RPC verdict that *learns* new words into `derpies_gimmicks` at runtime.

You are updating the **stored state** (the word list / prompt), not the code. DB changes are **live immediately** (the filter re-reads the list per event) — no deploy needed. A deploy is only for *code* changes.

## When to Use

- "update the derpies gimmick", "add a gimmick word", "remove a gimmick word"
- "manage the derpies word list", "the derpies filter missed a new respelling"
- A new respelling of a known gimmick word was observed and should be caught
- The detection *behavior* (not just the words) needs tuning — update the prompt

## When NOT to Use

- Changing the derpies *code* (tokenization, gate, flow) — that's a code change; see the deploy section
- Toggling the derpies feature flag — that's the `/feature` command, not the gimmick list
- The `mention` or other handlers' word lists — this skill is derpies-only

## The tables

| Table | Shape | What it holds |
|-------|-------|---------------|
| `derpies_gimmicks` | `id`, `word varchar(64) UNIQUE`, `source varchar(8) DEFAULT 'seed'`, `created_at` | The word list. `source` ∈ `seed` / `llm` / `manual`. |
| `derpies_prompt` | single row: `body text`, `updated_at` | The live prompt template (the code default is the fallback). |

**`source` values** (do not conflate them):
- `seed` — migration-seeded (the baseline word list).
- `llm` — learnt at runtime by the derpies handler's pi RPC verdict.
- `manual` — added by a human via the `/gimmick` command or a direct SQL insert.

## The word contract (critical)

Every word is normalized with `wordmatch.FoldToASCII` (NFD-decompose, drop combining marks **and** format chars, fold confusable lookalikes to ASCII, lowercase) and validated with `wordmatch.WordValid` = `^[a-z0-9]{2,32}$`.

- **No punctuation trim.** `Swift.` folds to `swift.`, which is **INVALID** (the trailing `.` fails the charset). To add `swift`, pass `swift` — not `Swift.`.
- **2–32 chars, lowercase letters or digits only.** At least one letter (a pure-digit word is rejected by the verdict gate).
- **Unicode respellings fold to ASCII.** `świft` → `swift`, `zwіft` (Cyrillic і) → `zwift`, `z w i f t` (SPLIT, ADR 0009) → the collapsed `zwift`. So you usually add the **ASCII base form** and the fold catches the respellings.

## Adding a word

**Primary (runtime, no deploy):** the `/gimmick` Discord slash command — gated by the **role gate only** (Highly Regarded or admin; **NOT** the derpies feature flag, ADR 0003, so it works while the feature is off):

```
/gimmick add <word>      # e.g. /gimmick add sw1ft  -> source='manual'
```

Idempotent: `INSERT ... ON CONFLICT (word) DO NOTHING` — an existing row's `source` is **never** modified.

**Direct SQL** (when you can't use the command, or via the postgres MCP / psql):

```sql
INSERT INTO derpies_gimmicks (word, source)
VALUES ($1, 'manual')
ON CONFLICT (word) DO NOTHING;
```

Pass the **folded** word (lowercase, ASCII, 2–32 chars). Verify with `SELECT word, source FROM derpies_gimmicks WHERE word = $1;`.

## Listing words

```
/gimmick list                    # Discord (chunked, word + source)
SELECT word, source FROM derpies_gimmicks ORDER BY word;   -- SQL / MCP
```

## Removing a word

```
/gimmick delete <word>           # Discord
DELETE FROM derpies_gimmicks WHERE word = $1;   -- SQL / MCP (exact match on the folded word)
```

No cascade (no FK on the table). Takes effect on the next message.

## Updating the prompt (detection behavior)

When the *behavior* (not just the words) needs changing — e.g. a new evasion class, a new answer rule:

```sql
UPDATE derpies_prompt SET body = $1, updated_at = now();   -- $1 = the new template
```

- **Live immediately** (the flow fetches the row per message; no deploy, no restart).
- **Contract:** the literal `{content}` and `{known}` markers are **mandatory** (a template missing either is INVALID and falls back to the code default). `{{IMAGES}}`, `{{REF}}`, `{{EMBED}}`, `{{GIFS}}` are optional.
- **Keep the code default + migration in sync.** The code default is `internal/handlers/derpies/derpies.go` (`defaultPromptTemplate`) and the migration seed is `migrations/000003_derpies_prompt.up.sql` (single quotes doubled `''`). `TestMigration000003AppliesAndSeeds` is the **forever sync guard** — it fails if the migration seed ≠ the Go constant. If you change the code default, update the migration seed to match (byte-for-byte, modulo the `''` doubling).
- **Rollback** = restore the previous `body` text.

## Deploy (code changes only)

DB changes (word list, prompt) are **live immediately** — no deploy. A **code** change (tokenization, gate, flow) requires a deploy:

```
ssh root@tugbot update-tugbot
```

The script pulls `main`, runs migrations, builds, and restarts the service. Confirm the head commit matches your pushed commit and the service is `active (running)`.

## Gotchas

- **The command is ungated by the derpies feature flag** (ADR 0003) — you can pre-seed/prune while the feature is off. It *is* gated by the role (Highly Regarded or admin) and the guild guard.
- **An existing row's `source` is never modified** (idempotent upsert) — re-adding a `seed` word as `manual` is a no-op on the source.
- **The list is the folded-token space** — store the folded (ASCII) form; the fast path matches by exact folded token.
- **A learned word's repetition is a fast hit** (zero asks); a *novel* post is one pi ask. There's no rate limiting (out of scope per spec).
- **The SPLIT evasion (ADR 0009)** is caught by the tokenization's collapsed runs — you usually don't need to manually add `z w i f t`; add the base `zwift` and the fold handles the split.
- **Remaining dead-end:** a word wedged with *interior* punctuation in one token (`s.w.i.f.t`) is still not caught (edge-trim only) — a known limitation, not something the word list fixes.

## Verification

After any change, confirm it took effect:

```sql
SELECT word, source, created_at FROM derpies_gimmicks ORDER BY created_at DESC LIMIT 10;
```

A subsequent post by the gated user containing the word should be deleted (fast path, zero asks) — check the `derpies delete (fast)` log line. For a prompt change, the next slow-path ask uses the new template (no deploy needed).
