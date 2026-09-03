# Cutover runbook — Rust `tugbot` → Go `tugbot`

Both services live on the same host, against the same Discord app,
server, and database. The Rust service (`tugbot.service`) and the Go
service (`tugbot-go.service`) must **never be running at the same
time** (two gateways on one application id collide).

Pinned unit names:

| | Go | Rust |
|---|---|---|
| Unit | `tugbot-go.service` | `tugbot.service` |
| ExecStart | `/opt/tugbot/tugbot` | `/root/.cargo/bin/tugbot` |
| WorkingDirectory | `/opt/tugbot` | — (Rust install) |

## 1. Pick a low-activity window

Run the cutover when the server is quiet (few live messages, no
gulag operations in flight, no polls about to end). The bot is down
for the whole procedure; an idle window bounds that.

## 2. Rollback drill FIRST (do not skip)

Prove the Rust service still works after the Go binary is on the
machine, *before* removing anything:

1. `systemctl start tugbot-go.service`
2. Verify: `journalctl -u tugbot-go -f` shows the ready lifecycle
   (connected + the 9 slash commands re-registered + the servers
   upsert), and the bot answers one test action.
3. `systemctl stop tugbot-go.service`
4. `systemctl start tugbot.service` (Rust)
5. Verify: `journalctl -u tugbot` shows the Rust bot re-EGNS —
   ready + command re-registration.

If any of this fails, stop here: you do not have a working rollback
path.

## 3. Deploy the Go bot

- Copy the binary + `skills/` into `/opt/tugbot` (binary at
  `/opt/tugbot/tugbot`).
- `systemctl stop tugbot.service` (Rust goes down).
- Run `update-tugbot` (migrates, builds, restarts `tugbot-go.service`).
- Note (window discipline): while the Go bot owns the migration
  history, **no new diesel migrations in the Rust repo** — the Go
  `go run ./cmd/migrate` runner applies the shared `migrations/` tree.

## 4. Verify the Go `OnReady` lifecycle

`journalctl -u tugbot-go -f` must show:

- the ready / connected log,
- all **9** commands re-registered (7 slash + 2 message-kind),
- the `servers` upsert (the three-way).

## 5. Production sanity pass

With the Go bot live, exercise the surface — deliberately **no
elon / derpies item** (excluded from the port; do not test it):

- a **mention** ping (the pi-ask flow),
- a **link post** for each of `twitter` / `bsky` / `instagram`,
- one **slash command** (e.g. `/feature`),
- a **gulag reaction vote**,
- a `--selftest`-style init check (the config/pool/session/handlers
  construction path).

## 6. Monitor + parity pass

- `journalctl -u tugbot` for **24 hours** watching for gaps, errors,
  or missed dispatches (the Rust unit name is the historical log;
  watch the Go unit if it is the one live).
- Run the weekly **parity-checklist** pass against production
  traffic (the 11 sections in `docs/parity/checklist.md`).

## Rollback window (≈ 2 weeks)

Keep the Rust binary and `tugbot.service` installable for roughly two
weeks after cutover. Rollback = stop `tugbot-go.service`, start
`tugbot.service` (the drill from step 2).

## Afterwards (once the window closes)

- Remove the Rust binary.
- `systemctl disable --now tugbot.service` and delete the unit.
- Archive the Rust repository.
