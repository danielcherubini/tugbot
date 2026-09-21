---
status: committed
done-when: A new guild member runs /strava in a thread, clicks the posted link, and Accepts activity:read_all — the "Done" page renders, the athlete row exists (valid token, thread target, correct label), the confirm message is in the thread, and the next poll tick posts their first finished activity; verified end-to-end on the production box with a second real athlete (the Strava app is already upgraded to 10 athletes).
---

# Strava Self-Serve Onboarding Plan

**Goal:** Any guild member onboards their own Strava account via `/strava` in a thread; the consent redirect completes the flow automatically (exchange, insert, confirm) with no operator.
**Architecture:** The `/strava` slash command (added to the existing strava handler) issues a 128-bit `state` + authorize link and stores a pending row in a new `strava_onboardings` table; Strava's redirect lands on a new in-process HTTP handler on `:8643` (caddy-proxied at `tugbot.wizards.town/strava/callback`) that exchanges the code, verifies the `activity:read_all` scope, fetches the athlete, upserts the athlete row, and confirms in the thread. Decision 0012 (in-process callback — deliberate deviation from the zero-public-surface constraint) is the governing trade-off.
**Tech Stack:** Go, discordgo v0.29.0 (house `Response`/`SetupCommand` pattern), pgx pool (raw SQL, house pattern), net/http (in-process listener, MCP `:8642` precedent), Postgres (new table via `migrations/000007`).

**House gate (every task, in order — mirrors CI):** `go build ./...` → `go vet ./...` → `gofmt -l .` (must print nothing) → `make lint` → `go test ./...` (DB tests self-skip without the env). **Full gate (DB tasks, after the unit tasks land):** `make db-up` → `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` → `go run ./cmd/tugbot --selftest` (must log the gate string for the current task, exit 0). `-p 1` and `-count=1` are mandatory (shared compose DB; test cache masks DB results). Note: a prior strava task left the shared compose DB with residue — if a DB task fails with missing-table errors, run `docker compose down -v` then `make db-up` and re-run.

**Branch:** `feature/strava-onboarding` (create from `main` before Task 1; the gitflow-branching skill's naming).

**Source of truth:** this plan replaced the approved spec content in-place (the spec text lives in git history — `git show 1e7a20b:docs/roadmap/strava-onboarding.md` — and is also committed in `CONTEXT.md`'s **Onboarding** entry and `docs/decisions/0012`); where this plan cites "the spec," that is the reference. The gate-string ripple is exactly **one** `main.go` string (the selftest log line) + the `AGENTS.md` quote — do NOT "helpfully" rewrite the two other `main.go` strings that mention "fourteen handlers" (they describe the handler count, which is unchanged). **Note:** decision 0012's "4-string ripple: 3 in `cmd/tugbot/main.go` + `AGENTS.md"` consequence is stale — this plan's 1-string ripple supersedes it (the handler count is unchanged, so only the log line gains a clause).

---

### Task 1: Migration 000007 — the `strava_onboardings` table

**Context:** Onboarding needs a home for pending consent states: the `/strava` command (Task 2) writes one row per invocation, the callback (Task 3) consumes it (claim `pending → done`, or mark `failed`), and the poll tick deletes expired rows. The table is tiny (at most a handful of live rows — one per outstanding 1h-TTL link), so it gets no index. `timestamptz` follows the declared 000006 deviation (values are box-local `now()`, not Strava epochs). This task is schema-only — no Go code.

**Files:**
- Create: `migrations/000007_strava_onboarding.up.sql`
- Test: `internal/dbmigrate/migrate_test.go` (extend — the file's existing tests (`TestCleanDBExecutesBaselineAndLaterMigrations` et al.) establish the pattern: run `migrate.Run` against a clean test DB and assert on the resulting schema)

**What to implement:**

`migrations/000007_strava_onboarding.up.sql` — exactly:

```sql
-- 000007_strava_onboarding — pending onboarding states (self-serve /strava flow).
-- timestamptz: consistent with the 000006 declared deviation (box-local now(),
-- not Strava epoch values).
CREATE TABLE public.strava_onboardings (
    state      text PRIMARY KEY,           -- 32 hex (16 crypto/rand bytes); unguessable
    thread_id  bigint NOT NULL,           -- the /strava interaction's channel (thread or regular channel — both are valid post targets)
    label      text,                     -- the command's label arg; NULL = default to the Strava first name
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL      -- created_at + 1h (set by the inserter)
);
```

- **Do NOT** add a down file (house convention: `.up.sql` only — see `migrations/000006_strava.up.sql`).
- **Do NOT** touch any existing table or the `features` table (the `strava` feature row already exists from 000006; onboarding is not flag-gated by design).

**Steps:**
- [ ] **Pre-flight (migration number):** the committed `docs/roadmap/derpies-slowmode.md` plan (status: committed, not yet executed) claims `000007_derpies_slowmode_path.{up,down}.sql` with no pre-flight of its own — so to avoid a duplicate `000007` number no matter which plan lands first, this task uses **`000008_strava_onboarding.up.sql`** unconditionally (if a `000007_*` file already exists, 000008 is still correct; if none exists, 000008 is still correct — no branch). Use `000008_strava_onboarding.up.sql` throughout this plan (filename, the test below, the CHANGE.md entry, the rollout section) and skip the "no down file" note below (the derpies plan introduces the repo's first `.down.sql`, which falsifies the convention).
- [ ] Add a test `TestStravaOnboardingTable` to `internal/dbmigrate/migrate_test.go`. **Do NOT** follow the file's existing synthetic-migrations pattern (`writeTestMigrations` writes fake migrations into `t.TempDir()` — that tests the runner, not the repo's real migration files). Instead: a new `resetAllPublicTables` helper that drops **every object the real migrations create — tables AND types AND functions AND the runner's tracker** — before `migrate.Run`: `DROP TABLE IF EXISTS … CASCADE` for `servers`, `features`, `ai_slop_usage`, `goku_poll_usage`, `gulag_users`, `gulag_votes`, `is_this_real_usage`, `message_votes`, `reversal_of_fortunes`, `user_activity`, `__diesel_schema_migrations`, `derpies_gimmicks`, `derpies_prompt`, `derpies_config`, `derpies_decisions`, `derpies_gimmick_phrases`, `strava_athletes`, `strava_seen_activities`, `strava_onboardings`, **`schema_migrations`** (the runner's own tracker — NOT created by a migration file, but `Run` skips any version it contains, and the package's other tests stamp `000001_baseline` into it — the same version string as the real baseline, so a stale tracker row makes the real baseline skip → `features` never recreated → 000002 fails); **plus** `DROP TYPE IF EXISTS job_status;` (the baseline's `CREATE TYPE public.job_status AS ENUM` is a plain create — `DROP TABLE … CASCADE` does not drop types — second run fails with "type already exists"); **plus** `DROP FUNCTION IF EXISTS diesel_manage_updated_at(regclass); DROP FUNCTION IF EXISTS diesel_set_updated_at();` (plain creates, same second-run collision). THEN run `migrate.Run` against the repo's real `migrations` directory (the test CWD is `internal/dbmigrate`, so the dir is `../../migrations` — this executes 000001 through the latest in order), then assert: the table exists with exactly the six columns and types above; `state` is the primary key; `status` has the CHECK constraint with the three values and default `'pending'`; `created_at` defaults to `now()`. (The existing `resetMigrateState` drops only 4 objects — it is NOT sufficient; write the new helper. The shared compose DB + `-p 1`/`-count=1` rules from the house gate apply — this test recreates the whole schema, same residue profile as the other dbmigrate tests. **Red-run failure mode note:** the first (red) run may fail with "relation features does not exist" / tracker-residue errors rather than "table missing" — that is the expected red for a missing migration file, not a misdiagnosis.)
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/dbmigrate/`
  - Did it fail (table missing — the migration doesn't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Create `migrations/000007_strava_onboarding.up.sql` with the SQL above.
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/dbmigrate/`
  - Did all tests pass? If not, fix and re-run before continuing.
- [ ] Run the house gate (build → vet → gofmt → `make lint` → `go test ./...`).
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Commit with message: `strava: migration 000007 — strava_onboardings (pending consent states)`

**Acceptance criteria:**
- [ ] `migrations/000007_strava_onboarding.up.sql` exists with exactly the SQL above.
- [ ] `TestStravaOnboardingTable` passes under the full DB gate (and self-skips without the env var, like the file's other tests).
- [ ] House gate green; commit landed.

---

### Task 2: The `/strava` command (handler method + pure helpers + main wiring)

**Context:** The command is the first half of onboarding: any member runs it in the thread they want posts to, and the bot replies with a one-time, state-scoped authorize link. It is added to the **existing** strava handler (no new handler — the selftest handler count stays fourteen; the handler is already one of the fourteen, constructed by `newHandlers`). The pattern mirrors `internal/handlers/feat/feat.go` (per-handler `Response` type + `SetupCommand()` + `HandleInteraction()`; DB failures become an error `Response`, never a panic) and the dispatch/registration in `cmd/tugbot/main.go` (`dispatchCommand` switch + the `registerCommands` shapes slice + the logged name list).

**Files:**
- Modify: `internal/handlers/strava/strava.go` (add the `Response` type, `SetupCommand`, `HandleInteraction`, the three pure helpers, and the `onboardingTTL` constant)
- Modify: `cmd/tugbot/main.go` (dispatch case + shapes slice + logged name list + the two stale `registerCommands` doc comments)
- Modify: `cmd/tugbot/main_test.go` (the `TestRegisterCommandsUsesReadySliceRustOrder` `want` list + its "six non-gulag shapes" comment)
- Test: `internal/handlers/strava/strava_test.go` (unit — the file's existing test groups show the pattern)
- Test: `internal/handlers/strava/strava_integration_test.go` (DB — the file's existing 15 integration tests show the pattern: real test-DB pool, fake `StravaAPI` where needed, `features` table manipulated for flag state)

**What to implement:**

In `internal/handlers/strava/strava.go`:

- `const onboardingTTL = time.Hour`
- `type Response struct { Content string; Ephemeral bool; DeferResponse *bool }` (the per-handler shape every other handler carries — `feat.Response`, `cull.Response` — this one is new to the strava package)
- Pure helpers (package-level functions, no receiver — unit-testable without a handler):
  - `func sanitizeLabel(s string) string` — `strings.TrimSpace`, then replace newline sequences with a single space **in this order: `"\r\n"` as a unit first, then lone `"\r"`, then lone `"\n"`** (a naive two-pass `ReplaceAll` would turn `"A\r\nB"` into `"A  B"` — two spaces — the pinned case is one), then cap at 32 **runes** (`[]rune` slice; longer input → first 32 runes). Returns `""` for empty/whitespace-only input.
  - `func newState() (string, error)` — 16 bytes from `crypto/rand.Read` → 32 lowercase hex chars (`encoding/hex`). Returns the read error if any.
  - `func authorizeURL(clientID, state string) string` — exactly: `https://www.strava.com/oauth/authorize?client_id=` + clientID + `&redirect_uri=https%3A%2F%2Ftugbot.wizards.town%2Fstrava%2Fcallback&response_type=code&scope=activity:read_all&state=` + state. (The redirect URI is a **code constant** — public, not a secret; the client_id is already public in the authorize URL.)
- `func (s *Strava) SetupCommand() *discordgo.ApplicationCommand` — name `"strava"`, description `"Add your Strava account — finished runs and rides post to this thread"`, exactly one option: `label`, type `discordgo.ApplicationCommandOptionString`, description `"Display name for posts (defaults to your Strava first name)"`, `Required: false` (the feat handler's option shape is the template).
- `func (s *Strava) HandleInteraction(i *discordgo.Interaction) Response` — the command body:
  1. **Creds preflight:** if `s.app.Cfg.StravaClientID == "" || s.app.Cfg.StravaClientSecret == ""` → return `Response{Content: "strava is not configured on this bot — the owner needs to set the client credentials first"}` (no row insert; the link would be dead).
  2. **Extract the label:** first option (if any), `Value.(string)`; `label := sanitizeLabel(...)`.
  3. `state, err := newState()`; on error → `Response{Content: "onboarding setup failed — try again"}` (log the error, `module=strava`).
  4. **Insert** the pending row — raw pool SQL (the house pattern, `$n` placeholders): `thread_id` bound from `strconv.ParseInt(i.ChannelID, 10, 64)` (the interaction's `ChannelID` is a string; the column is `bigint`); an empty/unparsable `ChannelID` → the `"onboarding setup failed — try again"` reply (no row).
     ```sql
     INSERT INTO strava_onboardings (state, thread_id, label, status, created_at, expires_at)
     VALUES ($1, $2, $3, 'pending', now(), now() + $4::interval)
     ```
     with `label` bound as `nil` (SQL NULL) when `label == ""`, and `$4` = `onboardingTTL` (pass `onboardingTTL.String()`). On error → `Response{Content: "onboarding setup failed — try again"}` (log, `module=strava`).
  5. **Build the reply:** `authorizeURL(s.app.Cfg.StravaClientID, state)` + ` — this link expires in an hour.` If the `strava` feature flag is currently **off** (`!features.IsEnabled(ctx, s.app.Pool, FeatureKey)` — `ctx := context.Background()`, the feat precedent), append ` (the strava feature is disabled — you'll be tracked once it's enabled)`. Return `Response{Content: ..., Ephemeral: false}` (visible in the thread — the thread is where the later confirm lands; `Ephemeral` stays false).
  - **Do NOT** gate the command on the feature flag (onboarding is valid while the feature is off — the row just sits until it's enabled).
  - **Do NOT** dedupe: a second `/strava` in the same thread inserts a fresh row; the unused one expires.

In `cmd/tugbot/main.go`:

- `dispatchCommand` switch: add
  ```go
  case "strava":
      r := h.strava.HandleInteraction(i)
      return reply{content: r.Content, ephemeral: r.Ephemeral}
  ```
  (the `feature`/`cull` cases are the template — the strava handler has no `DeferResponse` use, matching feat's always-false pattern).
- `registerCommands` shapes slice: **append** `h.strava.SetupCommand()` **after** `h.gimmick.SetupCommand()` (the Go-origin position — the slice currently lists `h.aiSlop.SetupCommand()`, `h.prefix.SetupCommand("horny", …)`, `h.prefix.SetupCommand("phony", …)`, `h.feat.SetupCommand()`, `h.cull.SetupCommand()`, `h.gimmick.SetupCommand()`).
- The logged name list (`"I now have the following guild slash commands:"` loop): add `"strava"` (the list goes from ten to eleven names).
- The two stale `registerCommands` doc comments: "EXACTLY the ten command shapes (8 slash + 2 message-kind; there is no "goku")" → eleven shapes (9 slash + 2 message-kind); "the remaining six shapes" → the remaining seven shapes; **plus** the three name-list comments the change orphans: the `registerCommands` doc comment's parenthetical `"(AI Slop, phony, horny, feature, cull, gimmick)"` → append `strava`; the inline `// Rust ready() vector order (after the four gulag shapes): AI Slop, horny, phony, feature, cull, gimmick.` comment → append `strava`; and `dispatchCommand`'s doc-comment name list ("…phony, horny (both via prefixhandler), feature (Feat), cull, gimmick") → append `strava (Strava)`.
- **`cmd/tugbot/main_test.go` — `TestRegisterCommandsUsesReadySliceRustOrder`** (it pins the non-gulag registration shape list with a hard length + order `t.Fatalf`): append `"999/strava"` to its `want` slice (after `"999/gimmick"` — matching the slice position above) and update its "six non-gulag shapes" comment to seven. Without this, Task 3's full-gate run (`go test -p 1 -count=1 ./...` with the DB env) is guaranteed red — the test self-skips only without the env/compose DB.
- **Do NOT** touch the selftest strings in this task (the gate-string clause lands in Task 3).

**Steps:**
- [ ] Write failing unit tests in `internal/handlers/strava/strava_test.go`:
  - `TestSanitizeLabel` — table: `"  Sam  "` → `"Sam"`; `"An\nna"` → `"An na"`; `"A\r\nB"` → `"A B"`; a 40-rune input → exactly 32 runes; `""` → `""`; `"   "` → `""`; a 32-rune input → unchanged.
  - `TestNewState` — result is exactly 32 chars, all `[0-9a-f]`; 1000 consecutive calls produce 1000 distinct values (collision guard).
  - `TestAuthorizeURL` — `authorizeURL("280525", "abc123")` equals the exact URL above (full-string compare, not substring).
  - `TestSetupCommandShape` — name `"strava"`; exactly one option; option name `"label"`, type string, `Required` false.
  - `TestSanitizeLabelCR` — the newline-ordering pin: `"A\r\nB"` → `"A B"` (**one** space — `\r\n` replaced as a unit, not two passes); `"A\rB"` → `"A B"`; `"A\nB"` → `"A B"`.
- [ ] Run `go test ./internal/handlers/strava/`
  - Did it fail (functions don't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement the four items in `strava.go` (const, `Response`, helpers, `SetupCommand`, `HandleInteraction`).
- [ ] Run `go test ./internal/handlers/strava/`
  - Did all tests pass? If not, fix and re-run before continuing.
- [ ] **Extend `setupStravaTestDB`** (the integration file's setup — it creates `strava_athletes`, `strava_seen_activities`, and `features` manually; it does NOT run migrations): add a `DROP TABLE IF EXISTS strava_onboardings;` + `CREATE TABLE` mirroring `migrations/000007_strava_onboarding.up.sql` verbatim (every new onboarding test seeds or asserts `strava_onboardings` rows and fails with "relation does not exist" without this).
- [ ] Write failing DB integration tests in `internal/handlers/strava/strava_integration_test.go` (construct the handler the file's existing tests do — real test-DB pool; the command path needs no `StravaAPI` calls):
  - `TestStravaCommandInsertsPendingOnboarding` — build a `*discordgo.Interaction` carrying `ApplicationCommandInteractionData{Name: "strava", Options: [label: "Matt"]}` and `ChannelID: 999000111222` (the existing tests show the interaction-construction style; if none does, construct `&discordgo.Interaction{ChannelID: …, Data: …}` directly). Call `HandleInteraction`; assert the reply `Content` contains the authorize URL with a 32-hex `state=`; assert a `strava_onboardings` row: that `state`, `thread_id = 999000111222`, `label = 'Matt'`, `status = 'pending'`, `expires_at` within 60–120 min of `now()`.
  - `TestStravaCommandOmittedLabelIsNULL` — no options → row `label IS NULL`; reply still carries a valid link.
  - `TestStravaCommandUnconfiguredCreds` — construct the handler with a config whose `StravaClientID` is `""` (the existing tests' config-construction pattern; the handler's `New` takes `*app.App` — mirror how they build it) → reply = the "not configured" text, **zero** rows inserted.
  - `TestStravaCommandFlagOffClause` — `UPDATE features SET enabled = false WHERE name = 'strava'` → reply contains the disabled-clause; restore the flag in a `t.Cleanup`.
  - `TestStravaCommandUnparsableChannelID` — interaction with `ChannelID: ""` → the `"onboarding setup failed — try again"` reply, **zero** rows inserted.
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/`
  - Did it fail (the wiring/behavior doesn't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement the `cmd/tugbot/main.go` changes (dispatch case, shapes slice, logged name, the three name-list comment updates) + the `cmd/tugbot/main_test.go` `want`-list update (the `TestRegisterCommandsUsesReadySliceRustOrder` fix above).
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./cmd/tugbot/` (the `TestRegisterCommandsUsesReadySliceRustOrder` fix only executes under the DB env — this catches a slice/`want` mismatch NOW, not at Task 3's gate).
- [ ] Run the house gate (build → vet → gofmt → `make lint` → `go test ./...`).
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Commit with message: `strava: /strava command — state-scoped onboarding link (ungated, optional label)`

**Acceptance criteria:**
- [ ] All five unit tests + all four integration tests pass (integration under the full DB gate; self-skip without the env).
- [ ] `main.go` dispatches `strava` to the handler, registers the command shape, and logs it in the name list.
- [ ] House gate green; commit landed.

---

### Task 3: The onboarding callback (`:8643` in-process listener + selftest clause)

**Context:** The callback is the second half of onboarding: Strava redirects the consenting athlete's browser to `tugbot.wizards.town/strava/callback?code=…&state=…`; the in-process handler (decisions 0012: deliberately inside the bot process — consolidated liveness, one unit, confirm on the bot's own session) completes the flow and renders a "Done" page. The handler is a new method on the existing `*Strava` (it already holds the `StravaAPI` client, config, pool, and the `send` function the posts use — the confirm reuses that same mention-suppressed `ChannelMessageSendComplex` `parse: []` sender). The listener is wired in `cmd/tugbot/main.go` exactly like the MCP's `:8642` (constructed in selftest; `ListenAndServe` in the production errgroup; the port is a **code constant** — no new env var, per the spec). The client gains one new method (`GetAthlete`) — the re-auth work used `GET /api/v3/athlete` manually via curl, but the callback needs it in-process.

**Files:**
- Modify: `internal/handlers/strava/client.go` (add the `Athlete` struct, `GetAthlete`, `ExchangeCode` + the interface entries + the `ErrInvalidCode` typed error)
- Modify: `internal/handlers/strava/strava.go` (add `OnboardingHandler`, the three pure helpers, the per-tick cleanup, and the `stravaOnboardAddr` constant)
- Modify: `cmd/tugbot/main.go` (production listener goroutine + selftest construction + the gate string)
- Modify: `AGENTS.md` (the quoted selftest gate string)
- Test: `internal/handlers/strava/client_test.go` (extend — the file's existing httptest pattern: a `httptest.Server` standing in for `strava.com`, the client's `base` pointed at it)
- Test: `internal/handlers/strava/strava_test.go` (extend — unit: the new pure helpers + the throttle)
- Test: `internal/handlers/strava/strava_integration_test.go` (extend — the httptest matrix against the real test-DB pool, fake `StravaAPI` where the flow allows it)

**What to implement:**

In `internal/handlers/strava/client.go` — the client gains **two** new interface methods (`GetAthlete` + `ExchangeCode`):

- `type Athlete struct { ID int64; Firstname, Lastname, Username string }`
- `GetAthlete(ctx context.Context, token string) (Athlete, error)` — `GET {base}/api/v3/athlete` with `Authorization: Bearer <token>` (the base + the `/api` v3 prefix + the `stravaHTTP` client: the exact pattern of `GetActivity`'s detail call — including its status handling: 401 → `ErrUnauthorized` (the existing typed error — a deauthorized token mid-onboarding), 404 → `ErrGone` (the existing typed error), **other non-200 → the `classifyStatus` error (`ErrTransient` — house-consistent; do NOT invent a plain-error contract that diverges from `GetActivity`)**).
- `ExchangeCode(ctx context.Context, clientID, clientSecret, code string) (accessToken string, refreshToken string, expiresAt time.Time, scope string, err error)` — `POST {base}/oauth/token` (the **root** endpoint, NOT under `/api` — the OAuth endpoints live at the root, exactly like `RefreshToken`'s URL) with form `client_id`, `client_secret`, `grant_type=authorization_code`, `code`. The response JSON: `access_token`, `refresh_token`, `expires_at` (epoch seconds → `time.Unix`), `scope`. Add a `Scope string` field to the existing `tokenWire` (or a new wire type for the exchange response — `tokenWire` today has no `scope` field). **Status mapping is custom — do NOT route through `classifyStatus` unchanged** (it maps 400+`invalid_grant` → `ErrUnauthorized` and other 4xx → `ErrTransient`, which is wrong for the code grant): 401 → `ErrUnauthorized`; 400/422 → a new **`ErrInvalidCode`** (`var ErrInvalidCode = errors.New("strava: invalid or already-used authorization code")` — the code is single-use, so a 400 almost always means it was already consumed or malformed; `client.go` does not currently import `errors` — add it); other non-200 → plain error.

In `internal/handlers/strava/strava.go`:

- `const stravaOnboardAddr = ":8643"` — **unexported, and never referenced from `package main`** (main cannot see it — cross-package compile error; the address is hidden inside the `Start` method below; do not import it in `main.go`).
- **`s.api` initialization fix (required — the callback must never touch a nil `s.api`):** `s.api` is today **lazily initialized inside `iteration`** (strava.go:177-178, *after* the `features.IsEnabled` early return and the creds preflight) — so on a flag-off bot (onboarding is explicitly valid while the flag is off) `s.api` is nil and the first callback would panic on `s.api.ExchangeCode`. Move the initialization into `New()`: `s.api = NewStravaAPI()` (the client's own comment guarantees it: "No network I/O at construction — selftest-safe"), and **delete the lazy init from `iteration`** (this also removes a data race: once the callback exists, the HTTP goroutine and `RunPoll`'s goroutine could both write `s.api` concurrently — today only `iteration` writes it, but the new callback path would be a second writer).
- Pure helpers (package-level, unit-testable):
  - `func scopeHasReadAll(scope string) bool` — split on whitespace (the token response's `scope` is space-separated, e.g. `"activity:read_all read"`); true iff a field equals `"activity:read_all"`.
  - `func resolveLabel(rowLabel, existingLabel, firstname, username string) string` — the first non-empty of `(rowLabel, existingLabel, firstname, username)`, else `"Athlete"`. This is the **display/fallback** resolution used in step 8: for a **new** athlete `existingLabel` is `""` and the label resolves to the command's label or the first name; for an **existing** athlete with an omitted command label, `existingLabel` (the stored label) wins — the spec's edge-case semantics (an explicit label overrides; an omitted one preserves the stored label).
  - `func onboardingPage(status int, title, body string) []byte` — a full HTML page (`<!doctype html>`, `font-family:monospace`, the same shape the retired sidecar rendered): `<h2>` + title + `<p>` + body, all three passed through `html.EscapeString`. (The page never carries secrets: the code is single-use and the tokens are never rendered.) **errcheck note:** every `w.Write(onboardingPage(...))` call site must be `_, _ = w.Write(...)` (the house convention — see `internal/mcp/mcp.go`'s `/healthz`); `make lint` (errcheck enabled) rejects a bare `w.Write`.
- `func (s *Strava) OnboardingHandler() http.Handler` — a single `http.HandlerFunc` (no mux needed — one route):
  1. **Route guard:** only `GET /strava/callback` — any other method or path → `404` (page: `"Not found"`).
  2. **Throttle:** in-memory per-client counter — **the state (a `sync.Mutex` + a `map[string]struct{count int; windowStart time.Time}`) is created INSIDE `OnboardingHandler()` and captured by the returned handler closure — never a struct field initialized in `New()`** (the unit throttle test constructs a bare `&Strava{}` and the integration tests build the handler via struct literal — neither goes through `New()`, so a `New()`-initialized field would be a nil map and panic on first write). Mutex-protected, lazy pruning of stale entries on access. **Key = the first hop of `X-Forwarded-For` (caddy sets it — behind the proxy `r.RemoteAddr` is always caddy's own address, so `RemoteAddr` alone would be one global bucket for every athlete; fall back to `r.RemoteAddr` when the header is absent)**, limit **10 per rolling 60 s** (reset the entry when its window lapses); over the limit → `429` (page: `"Too many requests — try again later"`), **before** any DB work. (Pinned semantics: at onboarding scale — a handful of humans/hour — the 10/min budget is deliberate and sufficient even if it collapses to a global bucket for a direct (non-proxied) hit.)
  3. **Parse:** `code := r.URL.Query().Get("code")`, `state := r.URL.Query().Get("state")`; **if `r.URL.Query().Get("error") != ""` (Strava's denial redirect — the athlete clicked Decline: `error=access_denied`, no `code`) → `200` (page: `"consent was not granted — run /strava again"`); the pending row is left as-is (it expires harmlessly; the athlete may retry — a denial is not a flow failure, so NO `failed` mark). Otherwise, either `code` or `state` empty → `404` (page: `"missing code or state"`).
  4. **State lookup:** `SELECT status, expires_at, thread_id, label FROM strava_onboardings WHERE state = $1` — scan `label` into a **`*string`** (the column is nullable; the happy-path test seeds it NULL — a plain `string` scan errors; the house pattern is the `lastPolledAt *time.Time` nullable scan in `athleteRow`; a NULL label becomes `""` for `resolveLabel`). No row → `404` (page: `"unknown or expired link — run /strava again"`). `status != 'pending'` → `409` (page: `"this link was already used"`). `expires_at < now()` → `DELETE FROM strava_onboardings WHERE state = $1` then `404` (page: `"expired — run /strava again"`).
  5. **Exchange:** `s.api.ExchangeCode(ctx, clientID, clientSecret, code)` (the `StravaAPI` from the handler's existing field — the handler already holds it; the config values from `s.app.Cfg`). Any error → `UPDATE strava_onboardings SET status = 'failed' WHERE state = $1 AND status = 'pending'`, log `slog.Warn("strava onboarding exchange failed", "module", "strava", "state", state, "error", err)` (never log the code or tokens), `400` (page: `"authorization failed — run /strava again"`).
  6. **Scope check:** `!scopeHasReadAll(scope)` → same `failed` + log + `400` (page: `"consent lacked activity:read_all — use the link /strava posts"`).
  7. **Athlete fetch:** `s.api.GetAthlete(ctx, accessToken)`; any error → same `failed` + log + `400` (page: `"could not load the athlete profile — run /strava again"`).
  8. **Label:** `label := resolveLabel(rowLabel, existingLabel, athlete.Firstname, athlete.Username)` — where `existingLabel` comes from step 9's pre-check (below; `""` when the athlete row doesn't exist yet). This implements the spec's edge case exactly: an **explicit command label overrides** (new or existing athlete); an **omitted** label keeps the stored label for an existing athlete (a re-consent never silently relabels a custom label) and resolves to the Strava first name (then username, then `"Athlete"`) for a new athlete.
  9. **Existing-athlete pre-check** (drives both step 8's `existingLabel` and the confirm text): `SELECT label FROM strava_athletes WHERE strava_athlete_id = $1` → `isNew` bool + `existingLabel` (scan into `*string` — the row may not exist; no row → `isNew = true`, `existingLabel = ""`).
  10. **Upsert** — the `label` value is the **step-8 resolved label** (always non-empty), bound as `$1` and applied **unconditionally** on conflict (the resolution already happened in Go — an explicit label overrides, an omitted one keeps the stored label for an existing athlete; there is no SQL-side `COALESCE` to dead-code):
      ```sql
      INSERT INTO strava_athletes (label, strava_athlete_id, access_token, refresh_token, token_expires_at, target_thread_id, needs_reauth)
      VALUES ($1, $2, $3, $4, $5, $6, false)
      ON CONFLICT (strava_athlete_id) DO UPDATE SET
          access_token = EXCLUDED.access_token,
          refresh_token = EXCLUDED.refresh_token,
          token_expires_at = EXCLUDED.token_expires_at,
          needs_reauth = false,
          target_thread_id = EXCLUDED.target_thread_id,
          label = EXCLUDED.label
      ```
      with `$1` = the resolved label, `$2` = `athlete.ID`, `$3`/`$4` = the exchanged access/refresh tokens, `$5` = `expiresAt`, `$6` = the onboarding row's `thread_id`. **The `DO UPDATE` `label` clause is `label = EXCLUDED.label` (unconditional) — NOT `COALESCE`** — because the Go-side resolution already decided the final value (spec edge case: omitted label → stored label preserved; explicit label → override).
  11. **Claim:** `UPDATE strava_onboardings SET status = 'done' WHERE state = $1 AND status = 'pending'` (after the upsert succeeds; a zero-row result is fine — a concurrent replay already claimed it, and the upsert is idempotent).
  12. **Confirm** via the handler's existing `send` (the posts' `ChannelMessageSendComplex` `parse: []` sender): `isNew` → `✅ {label} is now tracked — finished runs and rides will post here.` else `🔄 {label}'s Strava authorization was refreshed.` A send failure → `slog.Error("strava onboarding confirm failed", "module", "strava", "label", label, "error", err)` — the flow still succeeds (the page still says Done; the row is live).
  13. **Log + render:** `slog.Info("strava onboarding completed", "module", "strava", "label", label, "thread", threadID, "new", isNew)`; `200` (page: title `"Done"`, body `"{label} is set up. You can close this tab."`).
  14. **DB-error handling (every DB call in steps 4/9/10/11):** any `pool` error → `slog.Error(...)` (the failing step, `module=strava`, the error — never tokens or codes) + `500` (page: `"internal error — try again"`) and return. A mid-flow failure AFTER a successful upsert leaves the athlete row live and the onboarding row `pending` — that is fine and intentional (the 1h TTL + the per-tick cleanup absorb it; do NOT build a compensating rollback). `ctx` provenance: `r.Context()` (the HTTP request context — client-abort-cancellable; the 30 s `stravaHTTP` timeout bounds the Strava calls).
- **Per-tick cleanup** — at the TOP of `iteration` (before the `features.IsEnabled` check — the cleanup is hygiene, not feature work, and must run even while the flag is off): `DELETE FROM strava_onboardings WHERE expires_at < now()` (a single `pool.Exec`, error → `slog.Error("strava onboarding cleanup failed", "module", "strava", "error", err)`, non-fatal — the tick continues).

In `cmd/tugbot/main.go`:

- **Production wiring** — first, give `*Strava` a **blocking `Start(ctx context.Context) error` method** that encapsulates the `mcp.Server.Start` shape (`internal/mcp/mcp.go:153-209` is the template — do NOT inline a `select` in the errgroup arm; a bare `select { case <-ctx.Done(): …; default: ListenAndServe() }` evaluates ONCE at goroutine start, always takes `default`, and never shuts down → the bot hangs on every SIGTERM until systemd SIGKILLs it, and a bind failure is silently swallowed for the process's whole life). The method: build `http.Server{Addr: stravaOnboardAddr, Handler: s.OnboardingHandler()}`; `errCh := make(chan error, 1)` + `go func() { errCh <- hs.ListenAndServe() }()` (capacity 1 — the single result, buffered send never blocks); then `select`: `case <-ctx.Done()` → `Shutdown` with a bounded 10 s grace (the mcp template's deadline handling verbatim, including the non-blocking `errCh` read on the deadline path) and return nil on clean; `case err := <-errCh` → **return bind failures immediately at boot** (fail fast — so the errgroup arm's `os.Exit(1)` fires at startup, not at the next SIGTERM), nil/`ErrServerClosed` → nil. **Then** wire the errgroup arm in `run()` **exactly like the MCP arm** (main.go:538-546 — the errgroup vars there are `eg, egCtx`):
  ```go
  eg.Go(func() error {
      if err := h.strava.Start(egCtx); err != nil {
          slog.Error("strava onboarding listener failed to start and bind port", "module", "main", "error", err)
          os.Exit(1)
      }
      return nil
  })
  ```
  (import `net/http` only if `Start`'s signature needs it in main — the `http.Server` lives inside the strava package's `Start`, so main likely needs no new import; verify after writing `Start`).
- **Selftest** — in the selftest arm (the block that constructs `mcpSrv` and logs the gate string, ~line 310): after the MCP construction, add
  ```go
  if h.strava.OnboardingHandler() == nil {
      slog.Error("Failed to construct the strava onboarding callback", "module", "main")
      return 1
  }
  ```
  (the check is vacuous in practice — `OnboardingHandler` returns a non-nil closure — but it mirrors the equally-weak `mcpSrv == nil` check the house already carries; keep it for symmetry) and change the gate string to:
  `slog.Info("selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback constructed", "module", "main")`
- **`AGENTS.md`** — the quoted gate string (the line `go run ./cmd/tugbot --selftest   # must log "selftest: Discord session and all fourteen handlers and the MCP server constructed", exit 0`) → the new string, verbatim.
- **Do NOT** change the handler count (stays fourteen — the command and the callback both live on the existing strava handler) or the other two `main.go` strings that mention "fourteen handlers" (the `newHandlers` comment and the `--selftest` help text — they describe the handler count, which is unchanged).

**Steps:**
- [ ] Write failing client tests in `internal/handlers/strava/client_test.go` (the file's existing httptest pattern — a `httptest.Server` standing in for `strava.com`, the client's `base` pointed at it; the existing `RefreshToken` tests show the exact shape):
  - `TestGetAthlete` — 200 with a JSON body `{"id": 2703661, "firstname": "Daniel", "lastname": "Cherubini", "username": "danielcherubini"}` → the struct fields; assert the request hit `/api/v3/athlete` with the `Authorization: Bearer` header; 401 → `ErrUnauthorized`; 404 → `ErrGone`.
  - `TestExchangeCode` — 200 with `{"access_token": "a", "refresh_token": "r", "expires_at": 1789864896, "scope": "read activity:read_all"}` → all five return values (assert `expiresAt.Equal(time.Unix(1789864896, 0))`); assert the request was a `POST` to `/oauth/token` (NOT `/api/…`) with form `grant_type=authorization_code`; 400 → `ErrInvalidCode`; 401 → `ErrUnauthorized`.
- [ ] Run `go test ./internal/handlers/strava/`
  - Did it fail (methods don't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement `Athlete`, `GetAthlete`, `ExchangeCode`, `ErrInvalidCode` in `client.go` + the two interface entries.
- [ ] Run `go test ./internal/handlers/strava/`
  - Did all tests pass? If not, fix and re-run before continuing.
- [ ] Write failing unit tests in `strava_test.go`:
  - `TestScopeHasReadAll` — table: `"activity:read_all read"` → true; `"read"` → false; `""` → false; `"activity:read_all"` → true; `"activity:read_allx read"` → false (exact field match, not substring).
  - `TestResolveLabel` — table (4-arg form): `("Sam", "OldName", "Daniel", "daniel")` → `"Sam"` (explicit wins over everything); `("", "OldName", "Daniel", "daniel")` → `"OldName"` (omitted → stored label preserved — the re-consent case); `("", "", "Daniel", "daniel")` → `"Daniel"` (new athlete → first name); `("", "", "", "daniel")` → `"daniel"` (new athlete, empty first name → username); `("", "", "", "")` → `"Athlete"`.
  - `TestOnboardingThrottle` — a **bare `&Strava{}`** suffices (the unit file never constructs a full handler — only the integration file's `newTestStrava` does, and it needs a pool; the throttle state is closure-local to `OnboardingHandler()`, so a bare struct literal still works, and the throttle fires at the request edge before any `s.app`/pool access). Drive the handler with `httptest.NewRequest` + `req.RemoteAddr = "1.2.3.4:9999"`, **no query params and no `X-Forwarded-For` header** (the key falls back to `RemoteAddr` — the specified keying; a bare `GET /strava/callback` 404s at the parse step — the point is the 429 on the 11th, not the downstream behavior): 10 GETs all get through (any status, but not 429); the 11th → 429.
  - `TestOnboardingDenialRedirect` — `GET /strava/callback?error=access_denied` (no `code`/`state`) → 200, body contains `"consent was not granted"`; **no** `failed` mark (seed a pending onboarding row first and assert it stays `pending`). (This test needs the pool — it belongs in the integration file/step alongside the other row-asserting callback tests, next to `TestOnboardingUnknownState` — not in the unit file.)
- [ ] Run `go test ./internal/handlers/strava/`
  - Did it fail (the handler/helpers don't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement `OnboardingHandler` + the pure helpers + `stravaOnboardAddr` + the per-tick cleanup in `strava.go`.
- [ ] Run `go test ./internal/handlers/strava/`
  - Did all tests pass? If not, fix and re-run before continuing.
- [ ] Write failing DB integration tests in `strava_integration_test.go` (construct the handler with the **real test-DB pool** + a **fake `StravaAPI`** (the file's existing fakes show the pattern — extend the fake with `GetAthlete`/`ExchangeCode` methods that return canned values or injected errors) + a **captured `send`** (the file's existing tests already capture the posts' send — reuse it)):
  - `TestOnboardingHappyPath` — seed a `strava_onboardings` row (`state` = a 32-hex literal, `thread_id = 999000111222`, `label = NULL`, `status = 'pending'`, `expires_at = now() + '1h'`); fake `ExchangeCode` → canned tokens + scope `"read activity:read_all"`; fake `GetAthlete` → `{ID: 31337, Firstname: "Matt", Username: "matt"}`. `GET /strava/callback?code=c1&state=<state>` → 200, body contains `"Matt is set up"`; `strava_athletes` has a row: `label = 'Matt'` (resolved from the first name — the row label was NULL), `strava_athlete_id = 31337`, the canned tokens, `needs_reauth = false`, `target_thread_id = 999000111222`; the onboarding row `status = 'done'`; the captured send got `✅ Matt is now tracked — finished runs and rides will post here.` to thread `999000111222`.
  - `TestOnboardingExplicitLabel` — same but the onboarding row `label = 'M'` → the athlete row `label = 'M'` (the command label wins over the first name).
  - `TestOnboardingUnknownState` — `GET …?code=c1&state=deadbeef` (no row) → 404, body `"unknown or expired link"`; **zero** `strava_athletes` rows.
  - `TestOnboardingExpiredState` — row with `expires_at = now() - '1h'` → 404, body `"expired"`; the onboarding row is **deleted** (no `strava_athletes` row).
  - `TestOnboardingAlreadyUsedState` — row `status = 'done'` → 409, body `"already used"`; no `strava_athletes` row.
  - `TestOnboardingExchangeFailed` — fake `ExchangeCode` returns `ErrInvalidCode` → 400, body `"authorization failed"`; the onboarding row `status = 'failed'`; no `strava_athletes` row.
  - `TestOnboardingScopeMissing` — fake `ExchangeCode` succeeds with scope `"read"`; `GetAthlete` would succeed but must not be reached (assert the fake's `GetAthlete` call count is 0) → 400, body `"activity:read_all"`; row `status = 'failed'`; no `strava_athletes` row.
  - `TestOnboardingAthleteFetchFailed` — fake `GetAthlete` errors → 400, body `"could not load the athlete profile"`; row `status = 'failed'`; no `strava_athletes` row.
  - `TestOnboardingExistingAthleteUpsert` — pre-seed `strava_athletes` (`strava_athlete_id = 31337`, `label = 'OldName'`, `target_thread_id = 111`, old tokens, `needs_reauth = true`); onboarding row `label = NULL`, `thread_id = 222`; fake `GetAthlete` → `{ID: 31337, Firstname: "Matt"}`. → 200; the athlete row: `label = 'OldName'` (**preserved** — the onboarding row's label was NULL, so the resolved label is the stored one: the spec's edge case), `target_thread_id = 222` (moved to the newest), `needs_reauth = false`, the new canned tokens; the captured send got the `🔄 OldName's Strava authorization was refreshed.` variant.
  - `TestOnboardingExistingAthleteExplicitLabelOverrides` — same setup but the onboarding row `label = 'New'` → the athlete row `label = 'New'` (the explicit command label overrides the stored one); the captured send got the `🔄 New's …` variant.
  - `TestOnboardingWrongRoute` — `GET /` → 404; `POST /strava/callback` → 404.
  - `TestOnboardingTickCleanup` — seed two onboarding rows: one `expires_at = now() - '1h'`, one `now() + '1h'`; call `iteration` (the file's existing tests call it directly with a context); the expired row is gone, the live one remains. (Drive it the way the file's existing flag-gated tests do — the cleanup sits before the flag check, so the flag state doesn't matter; leave the flag as-is.)
- [ ] Run `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/`
  - Did it fail (the flow doesn't exist yet)? If it passed unexpectedly, stop and investigate why.
- [ ] Implement the `cmd/tugbot/main.go` changes (production listener goroutine, selftest construction, the gate string) + the `AGENTS.md` string.
- [ ] Run the house gate (build → vet → gofmt → `make lint` → `go test ./...`).
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Run the full gate: `make db-up` → `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` → `go run ./cmd/tugbot --selftest`
  - Did the selftest log `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback constructed` and exit 0? If not, fix and re-run before continuing.
- [ ] Commit with message: `strava: onboarding callback — :8643 in-process handler (decision 0012) + selftest clause`

**Acceptance criteria:**
- [ ] All client, unit, and integration tests pass (integration under the full DB gate; self-skip without the env).
- [ ] `go run ./cmd/tugbot --selftest` logs the new gate string and exits 0; `AGENTS.md` quotes it verbatim.
- [ ] The production path starts the `:8643` listener in the errgroup (graceful shutdown on ctx cancel).
- [ ] House gate green; commit landed.

---

### Task 4: Docs — the self-serve flow in `docs/features/strava.md`

**Context:** The feature doc is the runbook operators (and future agents) follow. Its Setup § currently documents the manual flow (register app → consent in a browser → exchange the code → one-statement SQL insert) — still valid, but no longer the primary path. This task makes the self-serve flow primary, adds the re-auth path, and records the new infrastructure (the `:8643` listener, the scoped caddy block, the sidecar retirement). No Go code — no test step; the gate for this task is `gofmt`-n/a + a clean commit (the doc is Markdown; the repo's CI lints Go only).

**Files:**
- Modify: `docs/features/strava.md`

**What to implement:**

Read the current `docs/features/strava.md` first (it carries the `status: live` front-matter — keep it; bump `last-verified` to today's date). **Two more stale spots to fix (they contradict the plan the moment it lands):** the `verified-by` front-matter quotes the **old** selftest gate string verbatim ("…all fourteen handlers and the MCP server constructed") → update it to the new string; and the intro says "It is a background loop only — no message event handlers, **no slash command**" → reword to reflect the new `/strava` command + the `:8643` callback (decision 0012). Then:

- **Setup §** — restructure: the **primary** path is self-serve: (1) the app exists and is upgraded (the 10-athlete dashboard self-upgrade — keep the existing note); (2) the athlete runs `/strava` in the thread they want posts to (optionally `label:<name>`); (3) clicks the posted link, logs into their Strava, Accepts `activity:read_all`; (4) the `tugbot.wizards.town/strava/callback` page renders "Done — {label} is set up" — the bot exchanged the code, verified the scope, inserted the row (token pair + `target_thread_id` = the command's thread + `needs_reauth = false`), and confirmed in the thread; (5) the next 15-min tick picks them up (24h first-enable lookback). Then a **Fallback** subsection: the manual flow (consent + code exchange + the one-statement SQL insert) stays for operators without a thread or for re-auth without re-running the command — keep the existing SQL verbatim.
- **Re-auth §** — add the self-serve path first: the athlete re-runs `/strava` in their (new) thread and re-consents; the upsert refreshes the token pair, clears `needs_reauth`, moves `target_thread_id` to the newest thread, and lands the resolved label (explicit command label overrides; an omitted one keeps the stored label — a re-consent never silently relabels a custom label). Keep the existing manual re-auth (401 → `needs_reauth` → UPDATE + flag clear) as the fallback.
- **Infrastructure notes** (wherever the doc documents the runtime surface — if it doesn't have a section, add a short one): the consent redirect is served at `tugbot.wizards.town/strava/callback` — an in-process `:8643` listener in the bot (decision 0012: a deliberate deviation from the zero-public-surface constraint; the surface is one unauthenticated GET, guarded by the 128-bit `state`, single-use codes, the atomic claim, and a 10/min per-IP throttle); the caddy block for `tugbot.wizards.town` is scoped to `handle /strava/callback` (everything else 404s) and proxies to the bot host's `:8643`; the earlier standalone `strava-callback` sidecar (box-local, `:8080`) is retired; the `localhost` consent flow stays valid independently (Strava whitelists `localhost` regardless of the declared domain).

**Steps:**
- [ ] Read the current `docs/features/strava.md` in full.
- [ ] Apply the four edits above (the three content edits + the `verified-by`/intro fixes) — keep the doc's existing voice and structure; do not rewrite sections that don't change.
- [ ] Add a `CHANGE.md` entry at the top (house format: `## <date>` + `### strava: self-serve onboarding — /strava command + :8643 in-process callback (decision 0012)` + a short Changed/Code paragraph naming `internal/handlers/strava/{strava,client}.go`, `cmd/tugbot/main.go`, `migrations/000007`, `docs/features/strava.md`).
- [ ] Run `gofmt -l .` (must print nothing — the doc change can't affect Go files, but the gate step is cheap) and `git diff --stat` (confirm only `docs/features/strava.md` + `CHANGE.md` changed).
  - Did it succeed? If not, fix and re-run before continuing.
- [ ] Commit with message: `docs: strava runbook — self-serve onboarding as the primary path` (the CHANGE.md entry rides in the same commit)

**Acceptance criteria:**
- [ ] Setup § leads with the self-serve flow; the manual flow survives as the Fallback subsection (SQL verbatim).
- [ ] Re-auth § documents the re-run-`/strava` path (with the upsert semantics) before the manual path.
- [ ] The infrastructure notes record `:8643` (decision 0012), the scoped caddy block, the sidecar retirement, and the still-valid `localhost` flow.
- [ ] The `verified-by` front-matter quotes the new gate string; the intro's "no slash command" clause is reworded.
- [ ] `CHANGE.md` has the entry; commit landed; only the doc + CHANGE.md changed.

---

### Rollout (post-merge, operational — not a repo task)

The plan's four tasks land the code. Shipping it to the production box (per the spec's rollout order):

1. `ssh root@tugbot.cherub.casa` → `cd /opt/tugbot && bash /opt/tugbot/update-tugbot` (pulls the merge, runs `go run ./cmd/migrate` → applies 000007, rebuilds, restarts — the `:8643` listener comes up with the service).
2. Caddy switch on `root@caddy`: replace the `tugbot.wizards.town` block with the scoped version from the spec (back up `Caddyfile` first — the box's prior backup convention: `Caddyfile.bak.<timestamp>`), `caddy validate`, `systemctl reload caddy`.
3. Verify: `curl -s -o /dev/null -w '%{http_code}' https://tugbot.wizards.town/` → 404 (the scope-out works); `curl -s https://tugbot.wizards.town/strava/callback` → 404 page "missing code or state".
4. `ssh root@tugbot.cherub.casa` → `systemctl disable --now strava-callback` (retire the sidecar).
5. **Done-when (the spec's exit condition):** a second real athlete runs `/strava` in a thread, clicks the link, Accepts `activity:read_all` → the "Done" page renders, the athlete row exists (valid token, thread target, correct label), the confirm is in the thread, and the next poll tick posts their first finished activity.
