---
status: committed
done-when: the /strava/webhook route is live in production on :8643, the app-level push subscription is registered (runbook ops step, subscription id recorded in the runbook), and a real ride is posted via the webhook path before the next poll tick (the seen row's dispositioned_at + post timestamp prove it) — with the 15-min poll running unchanged as the backstop (decision 0013)
---

# Strava Webhooks Plan

**Goal:** Strava webhook events (app-level push subscription) become an early trigger that posts a finished ride/run in ~1 minute, with the existing 15-min poll unchanged as the backstop (decision 0013).
**Architecture:** The existing `:8643` listener (decision 0012) gains a second route (`/strava/webhook`) via a small mux. The handler's synchronous part (route guard, its own closure-local throttle, payload parse, `owner_id` → athlete-row lookup, classification, `hub.challenge` verification GET, deauth flag UPDATE) finishes inside the 2-second ack and enqueues a job onto a bounded channel; 2 workers do the detail fetch + the shared pure decision function (`decideActivity`, extracted from the poll path in Task 1) + a per-event seen-table transaction + the 2-line post. The cursor stays purely poll-owned; the seen table's `ON CONFLICT DO NOTHING` absorbs all duplicates from both sources.
**Tech Stack:** Go, net/http (mux), pgx (per-event transactions), the existing `StravaAPI` client, the existing `sendFn` (mention-suppressed Discord post).

**House gate (run after EVERY task, in order):**
1. `go build ./...`
2. `go vet ./...`
3. `gofmt -l .` (must print nothing)
4. `make lint` (0 issues)
5. `go test ./...` (DB-touching tests self-skip cleanly)
For DB-touching tests (the `internal/handlers/strava` integration tests): `make db-up` then `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/` — a skip under the full gate is a missing override, not a legitimate result. Task 4 additionally requires `go run ./cmd/tugbot --selftest` to log the new gate string and exit 0.

**Do NOT change:** the poll loop's cadence/flags (the 15-min backstop stays exactly as-is — decision 0013); the onboarding route (`GET /strava/callback` — behavior unchanged, only re-wired into the mux); the cursor math (`loadColumnTimes`/`nextCursor`); the seen table schema (no migration — everything reuses existing columns); the 2-line post format; the handler count (stays fourteen — the webhook is a route on the existing strava handler).

---

### Task 1: Extract `decideActivity` — the pure decision function

**Context:** The per-activity decision logic currently lives inline in `passForAthlete`'s step c (per new listed id, `strava.go:314-370`) and step d (carried-pending retry, `strava.go:371-420`): the in-family `sport_type` gate (the 9-value set, `strava.go:33-37`), the detail-fetch error arms (404 `ErrGone` → `skipped` on first sight; 429/transient → `pending`; `isProcessing` → `pending`), the `resolveTarget` closure (`strava.go:303-308` — per-athlete thread → `STRAVA_SHARED_THREAD_ID` config → none), and `makePost`/`buildPost` (`strava.go:514-530`, `strava.go:691-693` — the 2-line post, newline-sanitized). This task extracts that decision into a pure function the webhook worker (Task 3) can call. The function takes the fetch **outcome** (not just a ready detail) so both callers share the same error arms. It **never persists** — the poll path keeps persisting in its batched transaction; the webhook worker persists in its own per-event transaction. This is a re-slice of existing code: the decision logic today only appends to deferred `inserts`/`updates`/`posts` slices and never touches the transaction directly, so behavior is preserved **except for the two documented unifications below** (the empty-`SportType` case and the step-c post-fetch detail-family gate — the existing integration tests still pass unchanged because no test pins the divergent cases). The existing integration tests (`TestStravaPassPostsNewFamily`, `TestStravaNoRepost`, `TestStravaPendingHoldAndRetry`, the 404/429 arms, the no-target arm — `strava_integration_test.go:367-1213`) pin every arm and are the regression net: they run **unchanged**.

**Files:**
- Modify: `internal/handlers/strava/strava.go`
- Create: `internal/handlers/strava/decide_test.go` (unit, no DB)

**What to implement:**

Add to `strava.go` (line references below are approximate — the helper **names** are authoritative):

```go
// fetchStatus is the outcome class of a detail fetch (shared by the poll path and the webhook worker).
type fetchStatus int

const (
    fetchReady fetchStatus = iota  // detail fetched, not processing
    fetchProcessing                // fetched, but isProcessing(detail)
    fetchGone                      // 404 / ErrGone
    fetchTransient                 // 429 or other transient error
)

// fetchOutcome is the result of a detail fetch. detail is valid only when status == fetchReady || fetchProcessing.
type fetchOutcome struct {
    status fetchStatus
    detail Activity
}

// disposition is the per-activity decision. post is non-nil only when status == "posted".
type disposition struct {
    status string // "posted" | "pending" | "skipped"
    post   *postItem
    reason string // "gone" | "out-of-family" | "no-target" ("" for posted/pending)
}

// decideActivity is the pure per-activity decision shared by the poll path and the webhook worker.
// It is a METHOD (it needs s.app for resolveTarget's config access and makePost). It never
// persists (no transaction, no slices, no cursor), never touches the network, and never
// touches s.app.Pool (so a unit test's &Strava{app: &app.App{Cfg: …}} with a nil Pool is safe).
func (s *Strava) decideActivity(ath *athleteRow, out fetchOutcome) disposition
```

Behavior (identical to the current inline logic, with ONE deliberate unification noted below):
- `fetchGone` → `skipped` (reason `"gone"`), **only as a first-sight decision** — the caller decides whether an existing terminal row overrides (the poll path's step d never re-decides a terminal row; the webhook worker's per-event transaction leaves terminal rows untouched — see Task 3).
- `fetchTransient` / `fetchProcessing` → `pending` (no reason).
- `fetchReady` → the in-family gate: `family(detail.SportType)` — the existing **single-argument** helper (`func family(sportType string) bool`, backed by the fixed global `familySet` — there is no per-athlete sport set; do not invent one): out-of-family → `skipped` (reason `"out-of-family"`); in-family → `resolveTarget(ath)` (the existing **closure** at `strava.go:303-308`, promoted to a method `(s *Strava) resolveTarget(ath *athleteRow) int64` during the extraction — it reads `s.app.Cfg.StravaSharedThreadID` — per-athlete thread → shared config → none): no target → `skipped` (reason `"no-target"`); target present → `posted` + `s.makePost(ath.label, detail, target, detail.ID)` (the real signature is `makePost(label string, a Activity, threadID, activityID int64) postItem` — label from `ath.label`, activity id from `detail.ID`; the post item shape matches the existing deferred `posts` slice element exactly — reuse the same type). **Second documented delta (alongside the empty-`SportType` unification below):** today step c posts a fetched detail **without re-checking the detail's** sport type (the family gate ran on the summary pre-fetch); wiring step c's post-fetch arm through `decideActivity` applies `family(detail.SportType)` there for the first time — a fetched detail whose non-empty sport type differs from the (in-family) summary's is now `skipped` where today it is `posted`. Realistically Strava summaries and details agree and no test pins the divergent case — but it is a second, documented behavior delta, not a silent one.
- **The empty-`SportType` unification (deliberate, documented):** today the two arms diverge for a *fetched* detail with an empty `SportType` — step c made no post-fetch family check at all (the family gate ran on the **list summary pre-fetch**, so a detail could even reach `posted`), while step d did `!family("")` → `skipped`. `decideActivity` applies `family(detail.SportType)` uniformly, so a fetched detail with an empty `SportType` resolves to `skipped`/`out-of-family` on **both** paths. This is a deliberate unification of the divergent arms, not a behavior preservation — a fetched detail realistically always carries a `sport_type` (the *summary* can be empty while processing, and that summary-level empty-check stays inline in step c's pre-fetch, unchanged). The `family("")` exclusion is pinned by `TestFamilyPin` (`strava_test.go:57-75`).

Rewire `passForAthlete`: step c and step d's ready/error arms call `s.decideActivity` and persist its result into the same deferred slices as today. **What stays inline in step c (pre-fetch, caller-side):** the summary-level checks — empty `SportType` → `pending` (no fetch), out-of-family summary → `skipped` (no fetch). The post-fetch path then goes through `decideActivity`. **What NOT to change:** the list walk (client `ListActivities`), the carried-pending snapshot, the per-athlete transaction + cursor math (`loadColumnTimes`/`nextCursor`), the retry-budget arms (`retries += 1`, drop-at-5), the post-after-commit ordering, the 401 pass-abort (`abortReauth`). The 401 is NOT a `fetchStatus` — it stays caller-handled (poll: pass abort; worker: the refresh-and-retry path, Task 3).

**Steps:**
- [ ] Write failing unit tests in `decide_test.go` (**no DB**: construct `&Strava{app: &app.App{Cfg: &config.Config{…}}}` via struct literal — `Pool` nil is fine because `decideActivity` never touches the pool; this is the `newTestStrava` construction pattern, `strava_integration_test.go:~233` — **NOT** a bare `&Strava{}`, which would nil-deref `s.app.Cfg` inside `resolveTarget`). Use `athleteRow` literals with `targetThreadID` set/unset + a config with/without `StravaSharedThreadID`; canned `Activity` literals): one per arm — ready+in-family+target → `posted` with the post item (assert the 2-line content via the existing `buildPost` shape); ready+in-family+no-target → `skipped`/`no-target`; ready+out-of-family → `skipped`/`out-of-family`; ready+empty-sport-type → `skipped`/`out-of-family` (the unification, above); processing → `pending`; gone → `skipped`/`gone`; transient → `pending`.
- [ ] Run `go test ./internal/handlers/strava/ -run TestDecide -v`
  - Did it fail (function undefined)? If it passed unexpectedly, stop and investigate.
- [ ] Implement `decideActivity` + the types in `strava.go`; rewire `passForAthlete` steps c/d to call it.
- [ ] Run `go test ./internal/handlers/strava/ -v` (unit)
  - Did all tests pass? If not, fix and re-run.
- [ ] Run the DB-touching net: `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/`
  - Did the existing integration tests pass **unchanged** (no test edits in this task)? If any failed, the extraction changed behavior — fix the extraction, not the tests.
- [ ] Run the house gate (all 5 commands).
- [ ] Commit: `refactor(strava): extract decideActivity — the pure per-activity decision shared by poll + webhook`

**Acceptance criteria:**
- [ ] `decideActivity` is pure (no DB, no network, no mutation of its inputs).
- [ ] The new unit tests pass; **zero** edits to existing test files.
- [ ] The full existing `internal/handlers/strava` suite (unit + DB integration) passes unchanged.
- [ ] House gate green.

---

### Task 2: Webhook config + route + synchronous surface

**Context:** This task puts the `/strava/webhook` route live on the existing `:8643` listener and implements the handler's **synchronous** part — everything that must finish inside Strava's 2-second ack. The async worker body is a **stub** in this task (workers drain the queue, log, and discard the job — the poll backstop covers the gap; Task 3 replaces the stub with the real body). The design (spec sections 1–2): the listener switches from a single-route handler to a small mux; the webhook route gets its **own** closure-local throttle (the documented bare-`&Strava{}` constraint at `strava.go:895-903` means each handler factory carries its own throttle state — do NOT share onboarding's); classification enqueues only `activity`/`create|update` events; `delete` is a no-op log; the deauth event (`athlete`/`updates.authorized="false"`) is a synchronous `UPDATE strava_athletes SET needs_reauth=TRUE` (one small write, inside the 2 s budget); the `hub.challenge` verification GET is answered only when `hub.verify_token` matches the new optional config var. **Line references in this plan are approximate — helper/type NAMES are authoritative.**

**Files:**
- Modify: `internal/config/config.go` (the optional var)
- Modify: `internal/handlers/strava/strava.go` (mux in `Start`; `WebhookHandler()` factory; the job channel + 2 workers with the stub body; the deauth UPDATE)
- Modify: `internal/handlers/strava/strava_test.go` (route-wiring unit tests)
- Modify: `internal/handlers/strava/strava_integration_test.go` (PG: deauth flag, unknown owner_id no-op)
- Modify: `internal/handlers/strava/client.go` — **only if** the `StravaAPI` interface needs the stub worker's logging to compile (it does not — the stub uses no new client methods; listed so the executing agent knows NOT to touch it)

**What to implement:**

1. **Config** (`internal/config/config.go`): `StravaWebhookVerifyToken string` — env `STRAVA_WEBHOOK_VERIFY_TOKEN`, **optional** (empty = the verification GET is refused; the event POSTs are unaffected). No validation (any string is valid) — the existing optional-var pattern (like `STRAVA_SHARED_THREAD_ID`).

2. **Mux** — extract the mux construction into a new method `(s *Strava) routes() http.Handler` (the seam `Start` and the tests both use — without it, nothing can drive the mux: `Start` blocks on `ListenAndServe` and binds `:8643`):
   ```go
   func (s *Strava) routes() http.Handler {
       m := http.NewServeMux()
       m.Handle("/strava/callback", s.OnboardingHandler())
       m.Handle("/strava/webhook", s.WebhookHandler())
       return m
   }
   ```
   **Unguarded patterns, deliberately** (Go ≥1.22 method patterns are available — go.mod pins go 1.25 — but a `GET /strava/callback` pattern would make the mux return **405** for `POST /strava/callback`, contradicting the plan's "everything else 404s" claim; the unguarded pattern lets the onboarding handler's own method+path guard 404 as it does today). `Start` (`strava.go:1212-1260` — the bind/shutdown logic is route-agnostic and stays) becomes `Handler: s.routes()`. Everything else → the mux default 404. The existing onboarding route-guard test (`TestOnboardingWrongRoute`, `strava_integration_test.go:1874-1887`) must still pass (it drives `OnboardingHandler()` directly — unchanged); **plus a new `TestRoutesMux`** (drives `s.routes().ServeHTTP`: `POST /strava/callback` → 404 via the onboarding guard; `POST /strava/webhook` → the webhook handler; `GET /` → 404 mux default) — this closes the mux-routing blind spot the handler-direct tests can't see.

3. **`WebhookHandler()`** (a new factory method mirroring `OnboardingHandler()`'s structure, `strava.go:905-1210`):
   - **Route guard:** `r.URL.Path != "/strava/webhook"` → 404 (the mux already routes, but the guard keeps the handler safe when driven directly by tests — same pattern as onboarding's).
   - **Throttle:** its own closure-local `thMu`/`thState` + the same constants (10 per 60 s, last-XFF-hop keying, window sweep, 10 000-entry hard cap, port-stripped `RemoteAddr` fallback, `Retry-After: 60`) — copy the onboarding block's shape (`strava.go:~930-990`); the comment explaining the closure-local placement carries over.
   - **`hub.challenge` verification GET:** `r.Method == GET && r.URL.Query().Get("hub.mode") == "subscribe"` → if `r.URL.Query().Get("hub.verify_token") == s.app.Cfg.StravaWebhookVerifyToken && s.app.Cfg.StravaWebhookVerifyToken != ""` → `200` + `{"hub.challenge":"<echoed>"}` (JSON, the exact `hub.challenge` query value); else → `403` (the challenge is NOT echoed on a mismatch — no oracle). A GET without `hub.mode=subscribe` → 404. (The config field is read via `s.app.Cfg.*` — the `Strava` struct has no `config` field of its own; all config access in this package is `s.app.Cfg.*`.)
   - **Event POST** (`r.Method == POST`): read the body (cap at 64 KB — the documented payload is small; over-cap → `413` log), parse into:
     ```go
     type webhookEvent struct {
         ObjectType   string            `json:"object_type"`   // "activity" | "athlete"
         ObjectID     int64            `json:"object_id"`
         AspectType   string            `json:"aspect_type"`   // "create" | "update" | "delete"
         OwnerID      int64            `json:"owner_id"`
         SubscriptionID int64          `json:"subscription_id"`
         EventTime    int64            `json:"event_time"`
         Updates      map[string]string `json:"updates"`
     }
     ```
     Unparseable → log + `200` (no-op — the backstop is unaffected).
   - **Classification** (synchronous, in order):
     - `ObjectType == "athlete" && Updates["authorized"] == "false"` → `UPDATE strava_athletes SET needs_reauth = TRUE WHERE strava_athlete_id = $1` (the owner_id; idempotent — a duplicate event is a no-op) + log + `200`. (No job.)
     - `ObjectType == "activity" && (AspectType == "create" || AspectType == "update")` → fetch the athlete row by `strava_athlete_id = OwnerID` via a **new** helper `loadAthleteByStravaID(ctx context.Context, id int64) (*athleteRow, error)` — **create it** (no existing DB helper fetches one `strava_athletes` row by `strava_athlete_id`: `loadAthletes` at `strava.go:473-492` loads all rows, and `GetAthlete` at `client.go:~100` is the **Strava HTTP API** call — do NOT call the API here, the DB row is needed *to obtain* the token). Mirror `loadAthletes`'s column list, keyed on `strava_athlete_id`, **no `needs_reauth` filter** (a flagged row is still fetched — the worker's 401→refresh path (Task 3) handles the token state; a genuinely revoked refresh fails the refresh and re-sets the flag as a no-op). **Implement the DB-failure arm with an explicit nil guard — `if s.app == nil || s.app.Pool == nil { return nil, errors.New("strava: no pool") }`** (a nil `*pgxpool.Pool` receiver **panics** on `QueryRow`, it does not return an error — the guard is what makes the unit tests' nil-pool construction genuinely safe, and it doubles as a defensive production guard). **DB error → log + `200` (no-op — the backstop covers it).** No row (unknown owner_id) → log + `200` (no-op). Row found → enqueue the job (below) → `200` **immediately** (before any worker work).
     - Everything else (`delete`, other `athlete` shapes, unknown `object_type`) → log + `200` (no-op).
   - **The job + queue + workers (concrete design — resolves the lazy-init/server-ctx tension):**
     ```go
     type webhookJob struct {
         ath    *athleteRow
         actID  int64
         source string // "create" | "update" (the aspect — for logs)
     }
     ```
     ```
     New `*Strava` struct fields: `jobs chan webhookJob` (nil until started), `workerOnce sync.Once`, `workersDone sync.WaitGroup`, `workerFn func(ctx context.Context, job webhookJob)` (nil until defaulted), `serverCtx context.Context` + `serverCtxCancel context.CancelFunc`.
     - `startWorkers()`: the `workerOnce` body — create `s.jobs = make(chan webhookJob, 32)`; default `s.workerFn` to the real worker body if nil (the test hook, below); derive the worker context from `s.serverCtx` if set, else `context.Background()`; start 2 worker goroutines (each: `for { select { case <-ctx.Done(): return; case job := <-s.jobs: s.workerFn(ctx, job) } }`), tracked in `workersDone`.
     - `Start` (strava.go:1212): **before** wiring the mux — `s.serverCtx, s.serverCtxCancel = context.WithCancel(ctx); s.startWorkers()`. **Shutdown** (after the existing `srv.Shutdown`): `s.serverCtxCancel()` + `s.workersDone.Wait()` bounded by a 10 s grace (a `select` on a timer; the real body is bounded by the 30 s HTTP timeout, so the grace then abandons in-flight jobs — safe, the backstop covers a dropped job). The bind-fail → `os.Exit(1)` arm is unchanged.
     - `WebhookHandler()`: calls `s.startWorkers()` (idempotent — covers the handler-direct test construction that never goes through `Start`; the `workerOnce` makes repeated calls a no-op). The bare-`&Strava{}` throttle constraint (the comment at `strava.go:895-903`) still applies to the **throttle map** (closure-local); the channel/fields above are struct fields initialized lazily, so a bare `&Strava{app: …}` that calls `WebhookHandler()` gets a working queue (the unit tests rely on this — the bare `&Strava{}` with nil `app` is only used where no `app.Cfg`/`app.Pool` access happens).
     - **Enqueue:** non-blocking — `select { case s.jobs <- job: // 200; default: log "webhook queue full — dropping (backstop covers)" + 200 }`.
     - **Test hook:** `workerFn` — a unit test sets `s.workerFn = func(ctx context.Context, j webhookJob) { <-ctx.Done() }` (blocks) BEFORE the first `startWorkers()` call to prove the 200-before-work ordering (the handler's response arrives while the worker is blocked).
     - **Stub body (this task, the `workerFn` default):** log `webhook job received (stub) owner=%d activity=%d aspect=%s` and discard. Task 3 replaces the default with the real body.

**Steps:**
- [ ] Add `STRAVA_WEBHOOK_VERIFY_TOKEN` to `internal/config/config.go` (optional, no validation) + the config test (set/unset — the existing config-test pattern).
- [ ] Run the house gate (the config change must not break `--selftest`-adjacent construction — `NewStravaAPI`/`New` stay network-free).
- [ ] Write failing unit tests in `strava_test.go` (construction: `&Strava{app: &app.App{Cfg: &config.Config{…}}}` — `Pool` nil is fine: with a nil pool, `loadAthleteByStravaID` errors → the graceful DB-failure arm (log + 200, no job) — so the unit test asserts **200 + no job** for create/update, and the **full enqueue / queue-full / 200-before-work assertions live in the PG tests below** (they need a real athlete row to get past the DB-failure arm). The deauth UPDATE needs a pool, so its assertion lives in the PG test `TestWebhookDeauthSetsFlag` below, NOT here): `TestWebhookRouteGuard` (GET `/` + POST `/strava/callback` via the new handler → 404); `TestRoutesMux` (drive `s.routes().ServeHTTP` — `POST /strava/callback` → 404 via the onboarding guard; `POST /strava/webhook` → the webhook handler (200 via the DB-failure arm); `GET /` → 404 mux default); `TestWebhookVerifyTokenEcho` (matching token → 200 + echoed challenge JSON; mismatching → 403, no echo; empty config → 403); `TestWebhookEventClassification` (drive a POST through `s.WebhookHandler().ServeHTTP` — create/update → 200 + no job (nil-pool DB-failure arm); delete → 200 no job; unparseable → 200 no job); `TestWebhookThrottle` (the 10/60 s pattern — copy `TestOnboardingThrottle`'s shape).
- [ ] Run `go test ./internal/handlers/strava/ -run 'TestWebhook' -v`
  - Did it fail (undefined)? If it passed unexpectedly, stop and investigate.
- [ ] Implement the config var, the mux in `Start`, `WebhookHandler()`, the job/queue/stub workers, the deauth UPDATE.
- [ ] Write the PG integration tests in `strava_integration_test.go` (a real pool + a seeded athlete row, so the full enqueue path is reachable — the unit tests above can't get past the DB-failure arm): `TestWebhookEnqueuesJob` (a `create` event for a known athlete row → **capture at the worker seam**: set `s.workerFn = func(ctx context.Context, j webhookJob) { captured <- j }` (a `chan webhookJob`) BEFORE the POST, then assert on the captured job (athlete row + activity id) — do NOT read the raw `s.jobs` channel, which the two live workers actively consume and would race; this also survives Task 3's real body unchanged); `TestWebhookQueueFull` (seed the athlete row, saturate the 32-cap channel, POST → 200 + drop log, no panic); `TestWebhook200BeforeWork` (seed the athlete row, set `s.workerFn = func(ctx context.Context, j webhookJob) { <-ctx.Done() }` (blocks) before the first `startWorkers()`, POST → the response arrives while the worker is blocked); `TestWebhookDeauthSetsFlag` (a deauth event for a known athlete row → `needs_reauth=TRUE`; a duplicate → still TRUE, no error); `TestWebhookUnknownOwnerNoop` (an event for an unknown owner_id → no row created, no post, 200).
- [ ] Run `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/`
  - Did all tests pass (the existing suite unchanged + the new ones)? If not, fix and re-run.
- [ ] Run the house gate.
- [ ] Commit: `feat(strava): webhook route on :8643 — mux, verify-token handshake, event classification, stub workers`

**Acceptance criteria:**
- [ ] `GET /strava/callback` behavior is byte-identical (the existing onboarding tests pass unchanged).
- [ ] The verification GET echoes the challenge only on a matching non-empty `STRAVA_WEBHOOK_VERIFY_TOKEN`.
- [ ] create/update events enqueue a job and 200 before any worker work; queue-full and unknown-owner paths 200 + log, no panic.
- [ ] A deauth event sets `needs_reauth=TRUE` synchronously (idempotent).
- [ ] House gate green + the DB integration suite green.

---

### Task 3: The worker body — fetch + per-event transaction + post

**Context:** This task replaces Task 2's stub worker body with the real async path (spec section 3): per job, the detail fetch with the athlete's token (the existing `StravaAPI.GetActivity`), the shared `decideActivity` (Task 1) over the fetch outcome, and the per-event seen-table transaction — `INSERT … ON CONFLICT (strava_athletes_id, strava_activity_id) DO NOTHING`-then-`UPDATE` (first-sight insert; an existing **terminal** row is left untouched; an existing `pending` row is updated to the new disposition — this is how a later `update` event resolves it) → commit → the post is sent via `sendFn` **after** commit (persist before post, never the reverse — the existing order). `dispositioned_at` is set at first creation (whichever source creates the row first). The 401 arm: **refresh-and-retry-once** (refresh succeeds → persist the rotated pair + retry the fetch; refresh fails → `needs_reauth=TRUE` + job end) — no cursor impact (a single event). The cursor is **never** touched by the webhook path. **Concurrency safety:** the idempotency invariants are held by the seen table's unique constraint **plus rows-affected gates on the post** (a post is earned only when the worker's own INSERT/UPDATE wrote a row — `tag.RowsAffected() == 1`); the sequential no-re-post invariants are held by the terminal-row check. A residual poll-vs-webhook race window is accepted and documented (see the transaction section below).

**Files:**
- Modify: `internal/handlers/strava/strava.go` (the worker body; a small `upsertSeen` helper for the per-event transaction if the existing SQL shape doesn't fit the first-sight-then-update in one place)
- Modify: `internal/handlers/strava/strava_integration_test.go` (the PG integration tests below)

**What to implement:**

The worker body (replacing the stub), per job:
1. `detail, err := s.api.GetActivity(ctx, ath.AccessToken, actID)` — map the error to the fetch outcome (the 401 type is **`ErrUnauthorized`**, `client.go:52`, classified at `client.go:337` — there is no `ErrReauth` in the codebase):
   - **401** → **refresh-and-retry-once** (NOT a bare flag — a bare flag would conflate a merely *expired* token (bot down ≥6h; tokens last 6h) with a *revoked* one and permanently disable the athlete until manual re-consent): `s.api.RefreshToken(ctx, clientID, clientSecret, ath.RefreshToken)` — the **4-arg** signature (`client.go:94`), sourcing `clientID`/`clientSecret` from `s.app.Cfg.StravaClientID`/`s.app.Cfg.StravaClientSecret` (the step-a pattern, `strava.go:232-233`) → on **success**: persist the rotated pair (the existing step-a pattern — `UPDATE strava_athletes SET access_token = $1, refresh_token = $2, token_expires_at = $3 WHERE id = $4` with the **new** refresh token; the rotated refresh token MUST be persisted or the chain dies) and **retry the fetch once** with the new access token; on a **refresh failure** (the refresh itself 401s — genuinely revoked) **or a 401 on the retry fetch** (the explicit loop guard — the refresh succeeded but the fresh token is still rejected, so treat it as refresh failure): `UPDATE strava_athletes SET needs_reauth = TRUE WHERE id = $1` (bind `ath.id` — `athleteRow` has **no** `strava_athlete_id` field; `id` and `strava_athlete_id` identify the same row. The Task 2 deauth UPDATE is different: it binds the event payload's `owner_id` to `WHERE strava_athlete_id = $1` — do not conflate the two) + log + **job ends** (no seen row, no post; the poll loop's next tick sees the flag and the athlete is paused; re-consent self-heals). No further retries inside the worker (the next event or the next poll tick is the retry).
   - 404/`ErrGone` → `fetchGone`; 429/other transient → `fetchTransient`; success + `isProcessing(detail)` → `fetchProcessing`; else `fetchReady`.
2. `d := decideActivity(ath, outcome)` — **but only for the outcomes with a VALID detail** (`fetchReady`/`fetchProcessing`); `fetchGone`/`fetchTransient` skip straight to the no-row arm below.
3. **Per-event transaction (rows-affected-gated — the concurrency-safe dedupe) — the `start_date` rule:** `strava_seen_activities.start_date` is `timestamptz NOT NULL` (`migrations/000006_strava.up.sql:24`), and a valid `detail.StartDate` exists ONLY for `fetchReady`/`fetchProcessing` (a 404/429 outcome has no detail — a zero `start_date` on a `pending` row would poison the poll cursor's `min(pending)` hold and force a full re-walk from page 1 every tick). So: **`fetchGone` / `fetchTransient` → NO row — log (`webhook event had no valid detail — the poll backstop will create the row with the real summary start_date`) + job ends** (the poll's step c creates the `skipped`/`pending` row with the real summary `start_date` on its next tick — the backstop is the remedy, per decision 0013); `fetchProcessing` → a `pending` row; `fetchReady` → the `decideActivity` disposition row. The row check and the write happen in **ONE transaction** (a bare pre-tx SELECT would race a concurrent duplicate):
   - `BEGIN` → `SELECT status FROM strava_seen_activities WHERE strava_athletes_id = $1 AND strava_activity_id = $2` **inside the tx**:
     - existing **terminal** row (`posted`/`skipped`) → commit, **no post, job ends** (absorbed — the no-re-post / no-re-evaluation invariant).
     - existing `pending` row → `UPDATE … SET status = <d.status> WHERE strava_athletes_id = $1 AND strava_activity_id = $2 AND status = 'pending'` (retries/`dispositioned_at` untouched — `dispositioned_at` was set at first creation) → **post only if `tag.RowsAffected() == 1`** (a concurrent duplicate's committed `pending`→terminal update makes the second worker's WHERE match 0 rows → no double post; the `status = 'pending'` guard is what makes this work under READ COMMITTED — the second worker's UPDATE re-evaluates the WHERE against the first worker's committed row after waiting on the row lock).
     - no row → `INSERT … ON CONFLICT (strava_athletes_id, strava_activity_id) DO NOTHING` (first-sight; `dispositioned_at = DEFAULT now()`; `retries = 0`; **`start_date = detail.StartDate` — always a valid detail here, per the rule above**) → **post only if `tag.RowsAffected() == 1`** (a concurrent duplicate's committed row makes the second worker's INSERT a `DO NOTHING` → 0 rows → no double post — this is the concurrency-safe dedupe; the unique constraint does the work, the rows-affected check is the gate).
   - Commit, then — only if the post was earned (rows-affected == 1 and `d.status == "posted"`) — send `d.post` via `sendFn` **after** commit (the existing order: persist before post, never the reverse; a send failure is log-only — the seen row stays `posted`, so the next poll tick's carried-pending does NOT re-post a `posted` row; a lost send is a logged gap, consistent with the existing post-failure behavior in the poll path — the onboarding confirm's log-only pattern, `strava.go:~1197` line region).
   - **Residual poll-vs-webhook race window (accepted, documented):** the poll path snapshots `loadSeenIDs` before its detail fetches and appends posts *before* executing its `INSERT`s, so it cannot cheaply adopt the rows-affected gate without a restructure (out of scope — it would change pinned poll semantics). If a webhook event and a poll pass race on the same activity within the pass's window, a double post is possible (narrow window; the row state is still consistent). Document in the runbook's Webhooks section as the accepted residual risk. The webhook-vs-webhook case (the likely case — Strava's documented duplicate deliveries arrive close together) is fully closed by the rows-affected gate.
4. Log the disposition (`webhook disposition owner=%d activity=%d status=%s source=%s` — for the no-row arms, log the arm instead).

**What NOT to change:** the poll path (it keeps its batched transaction + cursor math); the retry-budget arms (a webhook-created `pending` row is picked up by the next tick's carried-pending path with the existing retry budget + drop rule — no new retry state in the webhook path); `sendFn`'s wiring.

**Steps:**
- [ ] Write failing PG integration tests in `strava_integration_test.go` (extend the `stubStrava` fake with a scriptable `GetActivity` — it already has per-method `fn` fields + call counters, `strava_integration_test.go:~115-160`; add a `doWebhook` helper driving a POST through `s.routes()`, mirroring `doCallback`):
  - `TestWebhookPostsNewFamily` — a `create` event for a known athlete (stub `GetActivity` → a ready in-family detail) → a `posted` seen row + the 2-line post in the thread (the `capture`/`byThread` assertions, `strava_integration_test.go:~166-202`).
  - `TestWebhookDedupe` — the same event twice → **one** post, one row.
  - `TestWebhookCreateAfterPostedAbsorbed` — pre-insert a `posted` row, then a `create` event → no second post, the row untouched.
  - `TestWebhookUpdateResolvesPending` — pre-insert a `pending` row (with `dispositioned_at` set), then an `update` event (stub → ready in-family) → the row becomes `posted`, exactly one post, `dispositioned_at` unchanged.
  - `TestWebhook401RefreshFailsSetsFlag` — stub `GetActivity` → 401 AND `RefreshToken` → 401 (genuinely revoked — the refresh failure is what sets the flag, per the refresh-and-retry arm) → `needs_reauth=TRUE`, no seen row, no post, the athlete's cursor (`last_polled_at`) unchanged. (The refresh-SUCCEEDS case — expired token, refresh + retry → the fetch succeeds and no flag is set — is covered by `TestWebhook401RefreshSucceeds` in the same file: stub `GetActivity` → 401-then-success, `RefreshToken` → a new pair → the post happens, `needs_reauth` stays FALSE, the rotated refresh token is persisted in the athlete row. **Stub extension required:** `stubStrava.refreshFn` is `func() error` (`strava_integration_test.go:~129`) — change it to `func() (string, string, time.Time, error)` so the rotated pair is scriptable; the 401 case returns `ErrUnauthorized{}`.)
  - `TestWebhookGoneNoRow` — stub → 404 → **no seen row** (the `start_date` rule: a gone outcome has no valid detail), no post, 200 (the poll's gone arm creates the `skipped` row on its next tick — the backstop).
  - `TestWebhookTransientNoRow` — stub → 429 → **no seen row**, no post, 200 (same rule; the poll's 429 arm creates the `pending` row with the real summary `start_date`).
  - `TestWebhookProcessingHolds` — stub → `isProcessing` → a `pending` row, no post, **`start_date` = the detail's `StartDate` (assert it is NOT the zero time — the `start_date` rule pin)**.
  - `TestWebhookConcurrentDuplicates` — **two goroutines** POST the same `create` event (a `sync.WaitGroup`; the stub `GetActivity` succeeds for both) → **exactly one** post, one row — this is the test the rows-affected gate exists for (the sequential `TestWebhookDedupe` above cannot catch the concurrent race: two workers both `ON CONFLICT DO NOTHING` and both post without it).
  - `TestWebhookNoRepostAcrossSources` — a `posted` row created by the **poll path** (the existing `TestStravaNoRepost` setup), then a `create` event for the same activity → absorbed (cross-source dedupe — the load-bearing invariant, decision 0013).
- [ ] Run `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./internal/handlers/strava/ -run 'TestWebhook' -v`
  - Did it fail (stub body)? If it passed unexpectedly, stop and investigate.
- [ ] Implement the worker body + the `upsertSeen` helper (if needed).
- [ ] Run the same command — did all `TestWebhook*` pass? Then run the **full** `internal/handlers/strava` suite (unit + DB) — the Task 1 refactor net + the Task 2 suite must still pass.
- [ ] Run the house gate.
- [ ] Commit: `feat(strava): webhook worker — detail fetch, per-event seen transaction, post-after-commit`

**Acceptance criteria:**
- [ ] All 11 integration tests pass; the full existing suite (poll path + onboarding + Task 2) passes unchanged.
- [ ] No cursor column is written by the webhook path (asserted in `TestWebhook401RefreshFailsSetsFlag` + the post tests).
- [ ] Terminal rows are never overwritten (absorbed in both the duplicate and cross-source tests).
- [ ] The post happens after commit (the existing ordering — a failed send leaves the row `posted`, log-only).
- [ ] The `start_date` rule holds: `gone`/`transient` outcomes create no row; a `pending` row's `start_date` is the detail's `StartDate`, never the zero time (`TestWebhookProcessingHolds`).
- [ ] House gate green.

---

### Task 4: Selftest extension + runbook + .env.example

**Context:** The final task wires the verification gate and the operational docs. The selftest gate string extends by one clause (house pattern — the 4-string ripple: 2 in `cmd/tugbot/main.go` (the check + the log), `AGENTS.md`, and the runbook's `verified-by`), mirroring the earlier MCP/onboarding ripples. The runbook's final `## Webhooks` section (currently beginning "Deferred, and treated as additive…") is **replaced** by a live "Webhooks" section (spec section 5) — this is the operator's manual for the one-time registration and the standing remedies. `.env.example` gains the optional var.

**Files:**
- Modify: `cmd/tugbot/main.go` (the selftest check + the gate string)
- ~~`cmd/tugbot/main_test.go`~~ — **NOT modified** (there is no gate-string assertion test there — `TestRegisterCommandsUsesReadySliceRustOrder`'s `want` list asserts shape-registration order, not the gate string; the gate string is pinned by `AGENTS.md` + the manual `--selftest` step below — do not create a new one, YAGNI)
- Modify: `AGENTS.md` (the quoted gate string)
- Modify: `docs/features/strava.md` (replace the final `## Webhooks` section — currently beginning "Deferred, and treated as additive…"; the `verified-by` string)
- Modify: `.env.example` (the optional var)

**What to implement:**

1. **Selftest** (`cmd/tugbot/main.go` — the existing clause region, `main.go:321-325`): add a parallel check — `s.strava.WebhookHandler() == nil` → the existing failure path; the gate string becomes:
   `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback and the strava webhook constructed`
   (the handler count stays **fourteen** — the webhook is a route on the existing strava handler, not a new handler). **No `main_test.go` change** — the gate string is pinned by `AGENTS.md` + the `--selftest` step below (there is no gate-string assertion test to update, and we do not create one).

2. **`AGENTS.md`**: the quoted gate string → the new string (exact match — it's the CI/local gate).

3. **`.env.example`**: `STRAVA_WEBHOOK_VERIFY_TOKEN=` (optional — commented: "the push-subscription verification token; empty = the hub.challenge verification GET is refused (matters only at one-time registration); see docs/features/strava.md").

4. **`docs/features/strava.md`** — replace the final `## Webhooks` section (currently beginning "Deferred, and treated as additive…") with a live **"Webhooks"** section (keep the section's position; the `verified-by` front-matter gets the new gate string + a fresh `last-verified` date at rollout):
   - **The push subscription** — app-level (one per app, `client_id`/`client_secret`, no athlete token; covers every athlete who authorized the app; re-consent does not touch it). The bot's `:8643` answers its `hub.challenge` verification GET and its event POSTs at `/strava/webhook` (caddy: `handle /strava/webhook` → `10.0.0.44:8643`, everything else still 404s).
   - **One-time registration** (the ops step): (1) choose a `verify_token` → `STRAVA_WEBHOOK_VERIFY_TOKEN` + restart; (2) `POST https://www.strava.com/api/v3/push_subscriptions` (`callback_url=https://tugbot.wizards.town/strava/webhook`); (3) the bot answers the challenge echo automatically; the POST returns `{"id": <n>}`; (4) record `<n>` here (a note next to the other app credentials). Health check: `GET /push_subscriptions` (client credentials). Changing the callback: `DELETE /push_subscriptions/<n>` + re-create (the documented path).
   - **Event scoping** — the table: `activity`/`create|update` → fetch + dispose (the 2-line post via the shared decision); `activity`/`delete` → no-op log; `athlete`/deauth → `needs_reauth=TRUE` (early revocation detection — the poll loop self-heals on re-consent); everything else → no-op log.
   - **The backstop** (decision 0013) — the 15-min poll is unchanged; Strava documents no delivery guarantee (≤3 attempts; duplicates and losses both observed) — the seen table absorbs duplicates from both sources; the cursor stays purely poll-owned.
   - **Security model** — unauthenticated POST; the `X-Strava-Signature` signing secret is undocumented (verification is optional); the trust model = the per-IP throttle + bounded work (queue cap 32, 2 workers) + seen-table dedupe (same as the onboarding callback).
   - **Rate budget** — 1 read per actionable event (the detail fetch); a burst of N simultaneous finishes = N reads — trivially inside 200/15 min at 10 athletes.
   - **Verify empirically** (the standing gap list; the backstop is the remedy for all three): subscription survival across re-consent; event latency (15+ min reported); silent subscription death (if events stop: delete + re-create + check the scope — `activity:read` is required for activity events; our tokens carry it).

**Steps:**
- [ ] Update `cmd/tugbot/main.go` (the `WebhookHandler() == nil` check + the string) — run `go test ./cmd/tugbot/ -v` (the DB-touching selftest test self-skips without the override; the existing unit tests must pass — **no `main_test.go` change**: the gate string is pinned by `AGENTS.md` + the `--selftest` step, not by a unit test).
- [ ] Update `AGENTS.md` + `.env.example`.
- [ ] Rewrite the runbook section (the content above) + the `verified-by`/`last-verified` front-matter.
- [ ] Run `make db-up` + `TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test -p 1 -count=1 ./...` (the full DB gate — the registration/selftest tests run).
- [ ] Run `go run ./cmd/tugbot --selftest`
  - Did it log `selftest: Discord session and all fourteen handlers and the MCP server and the strava onboarding callback and the strava webhook constructed` and exit 0? If not, fix and re-run.
- [ ] Run the house gate.
- [ ] Commit: `chore(strava): selftest gate + runbook webhooks section + .env.example`

**Acceptance criteria:**
- [ ] `--selftest` logs the new gate string verbatim and exits 0 (with the DB override env set for the selftest's DB-touching arm — the same conditions as the house gate's selftest step).
- [ ] `AGENTS.md`'s quoted string matches the code's string exactly.
- [ ] The runbook's old deferred-`## Webhooks` section is gone; the live "Webhooks" section is present with the registration ops step + the subscription-id placeholder.
- [ ] House gate green + the full DB gate green.

---

## Rollout (post-merge — the runbook's ops sequence)

1. `update-tugbot` on the box (pull + build + migrate — **no migration in this feature** — restart).
2. Caddy: add `handle /strava/webhook` → `10.0.0.44:8643` to the scoped `tugbot.wizards.town` block (backup first; `caddy validate`; reload).
3. The one-time registration ops step (runbook Webhooks §) — set `STRAVA_WEBHOOK_VERIFY_TOKEN`, restart, `POST /push_subscriptions`, record the subscription id in the runbook.
4. **The end-to-end proof:** a real ride — the seen row's `dispositioned_at` + the post timestamp show the webhook beat the next poll tick (done-when). The "verify empirically" list gets checked off as events accumulate.
