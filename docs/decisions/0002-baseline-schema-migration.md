---
status: accepted
date: 2026-09-03
superseded-by:
---

# Treat the pre-cutover schema as given (single baseline migration)

The Go port does not continue the 14-file diesel migration history. Instead it owns a single baseline migration file containing the full current schema (generated deterministically: replay the diesel migrations into a clean database, then `pg_dump --schema-only`), and all future migrations live in the Go repo. This was chosen because the project does not care about preserving per-file history for work that predates the port, and because it deletes the diesel-compatible-runner requirement entirely — the Go migration runner only knows its own tracking table (`schema_migrations`).

**Consequences**: (1) The runner's first run on a live database must *stamp* the baseline as applied without executing its DDL (sentinel: empty `schema_migrations` + first table present) — raw `CREATE` would otherwise collide with existing tables. One transaction per migration file. (2) During the ~2-week rollback window, the Rust-side `diesel migration run` stays a no-op unless a new diesel migration is added to the Rust repo — do not add one while the Go bot owns the history. (3) A golang-migrate-style framework is not used: the runner is in-tree (~100 LOC) to keep the tooling surface controlled.
