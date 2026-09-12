# tugbot

Discord bot (Go). This file documents the build/test gate for agent runs —
see `Makefile` and `.github/workflows/test.yml` for the canonical commands.

## Build & Testing

Gate order (mirrors CI's `vet, lint, test, build` job):

```bash
go build ./...
go vet ./...
gofmt -l .            # must print nothing (CI's linter set does NOT include gofmt)
make lint             # golangci-lint (version pinned in CI: v2.13.2) + go vet
go test ./...         # without PG: DB-touching tests self-skip cleanly
```

## Database-touching tests (full green gate)

CI has no Postgres, so DB-touching tests (the `internal/handlers/derpies` and
`internal/handlers/gimmick` integration tests, `internal/dbmigrate`,
`internal/features`, and `cmd/tugbot`'s registration/selftest tests)
self-skip in CI and only run locally:

```bash
make db-up   # docker compose up -d postgres (credentials postgres:postgres, database `tugbot`)
TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...
go run ./cmd/tugbot --selftest   # must log "Discord session and all thirteen handlers constructed", exit 0
```

- The DB-touching tests default to a `tugbot_test` URL and self-skip without the
  `TUGBOT_TEST_DATABASE_URL` override — a skip under the full gate is a missing
  override, not a legitimate result.
- `-p 1`: the DB-touching packages DROP/recreate shared tables in the ONE shared
  compose DB; parallel package execution cross-taints each other.
- `-count=1`: the Go test cache masks DB-package results — a cached green is a
  false green.
- State residue (per the Makefile's own warning): DB tests can leave the shared
  compose DB with dropped/recreated tables and an unsettled migration tracker.
  Remedy before a later `make migrate`: `docker compose down -v`.
