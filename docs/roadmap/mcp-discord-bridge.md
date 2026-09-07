---
status: committed
done-when: tugbot starts with the EXISTING env vars only (no new ones required); while running, an external MCP client connects to http://127.0.0.1:8642/mcp (Streamable HTTP) and, with zero extra config, can: list the bot's guilds, list a guild's channels, read (and author-filter) a channel's recent messages, post a message (optionally as a reply), and add a reaction — all acting as the bot; `curl -s http://127.0.0.1:8642/healthz` returns 200; `go run ./cmd/tugbot --selftest` logs "Discord session and all thirteen handlers and the MCP server constructed" and exits 0
---

# MCP Discord Bridge (v1) Plan

**Goal:** An always-on, zero-config, in-process MCP server embedded in tugbot so external agents (pi, Claude, …) can read and write Discord through the bot's own shared `*discordgo.Session` (the embedded-provider pattern; never a second session — the token allows one gateway connection).
**Architecture:** One new package `internal/mcp` taking the shared `*app.App`. Five "bridge tools" (list_guilds, list_channels, read_messages, post_message, react) are typed closures registered on the official `github.com/modelcontextprotocol/go-sdk` stateless Streamable-HTTP handler, mounted at `:8642/mcp` (port overridable via `TUGBOT_MCP_PORT`, the only new env var, default 8642). No auth middleware by default (LAN-trust posture — `docs/decisions/0004-mcp-layer-always-open-lan-trust.md`). No new DB tables, no migrations; v1 uses only `App.D`. Tools execute through the bot's own connection and consume its REST budget; 429s surface as tool errors **because** the `realDiscord` wrapper passes `discordgo.WithRetryOnRatelimit(false)` on every REST call (the default session would otherwise block-and-retry internally and never surface the 429) — documented behavior, not a bug.
**Tech Stack:** Go 1.25 (Task 1), `github.com/modelcontextprotocol/go-sdk` v1.7.0+, `bwmarrin/discordgo` v0.29.0 (unchanged).

**SDK API shape (verified against the v1.7.0 sources — use exactly this form):**

```go
// Server + tool registration.
srv := mcp.NewServer(&mcp.Implementation{Name: "tugbot", Version: "1.0.0"}, nil)
mcp.AddTool(srv, &mcp.Tool{Name: "list_guilds", Description: "..."},
    func(ctx context.Context, req *mcp.CallToolRequest, args listGuildsArgs) (*mcp.CallToolResult, any, error) {
        return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "..."}}}, nil, nil
    })
// Streamable HTTP handler — FIRST ARG ISA FUNCTION; RETURNS A SINGLE VALUE (NOT a tuple).
handler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return srv },
    &mcp.StreamableHTTPOptions{Stateless: true}) // *StreamableHTTPHandler implements http.Handler
```

**In-process driving pattern (from the SDK's own tests — there is NO `NewInMemoryClient` constructor, do not hunt for one):**

```go
ct, st := mcp.NewInMemoryTransports() // (client, server) transport pair
ss, err := srv.Connect(ctx, st, nil)      // server side
c := mcp.NewClient(impl, nil)
cs, err := c.Connect(ctx, ct, nil)          // client side
// drive:
tools, err := cs.ListTools(ctx, nil)    // *ListToolsResult
res,  err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_guilds", Arguments: anyArgs}) // *CallToolResult
```

Typed input structs get their JSON-schema auto-generated from `json` struct tags. **Do NOT rely on `jsonschema:"-"` exclusion tags** (unverified against the SDK's schema generator): keep optional fields plain `json:"name,omitempty"` and validate emptiness handler-side. For success return the `*mcp.CallToolResult` (text and/or structured `any` payload); for domain errors return `&mcp.CallToolResult{IsError: true, Content: [...]}` with a human-readable message — never a Go error for recoverable Discord failures.

**discordgo v0.29.0 error types (reviewer-verified — there is NO `*discordgo.Error` type):** REST failures surface as `*discordgo.RESTError` (`Response *http.Response` — `Response.Status` is the status **string**; plus `ResponseBody`, `Message *APIErrorMessage`) and as `*discordgo.RateLimitError` (wraps `*RateLimit{RetryAfter, URL}`). Test fixtures must construct one of these two real types.

**discordgo v0.29.0 session method shapes (reviewer-verified — do NOT "verify" against the plan's earlier guesses; these ARE the shapes; confirm with `go doc` when in doubt):**
- `ChannelMessages(channelID string, limit int, before, after, around string) ([]*Message, error)` — on error returns `(nil, err)`; there is NO partial page
- `ChannelMessageSend(channelID, content string) (*Message, error)`
- `ChannelMessageSendComplex(channelID string, data *MessageSend) (*Message, error)` — `MessageSend`'s reply field is **`Reference *MessageReference`** (NOT `MessageReference`); `MessageReference{MessageID: id}` is valid
- **`MessageReactionAdd(channelID, messageID, emojiID string, ...)` — the session method is `MessageReactionAdd`; there is no `ChannelMessageReactionAdd`**
- **`UserGuilds(limit int, beforeID, afterID string, withCounts bool, ...) ([]*UserGuild, error)` — FIVE params, returns `[]*UserGuild` (has `ID`/`Name` — NOT `[]*Guild`)**
- `GuildChannels(id string) ([]*Channel, error)`
- `s.State.Guilds` — `[]*Guild`, populated on READY (StateEnabled defaults true)
- `ChannelType` enum: `ChannelTypeGuildText (0)` and `ChannelTypeGuildNews (5)` are the only text-like guild types; there is **NO `ChannelTypeGuildAnnouncement`**

**Verification commands (the repo's gate order, AGENTS.md):**
- `go build ./...`
- `go vet ./...`
- `gofmt -l .` (must print nothing)
- `make lint`
- `go test ./...` (self-skip DB tests without `TUGBOT_TEST_DATABASE_URL` — fine for this plan; the DB gate is unchanged)

---

### Task 1: Go toolchain bump 1.22 → 1.25 (standalone, merges before Task 2)

**Context:** Both candidate MCP SDKs (we chose the official one) require Go ≥ 1.25; tugbot's `go.mod` says `go 1.22`. This decision was made deliberately as a **standalone change** so the infra change is reviewable and roll-backable per the cutover's 2-week rollback window (no MCP code may appear in this task). Existing deps (`bwmarrin/discordgo` v0.29.0, `jackc/pgx/v5` v5.7.3, `joho/godotenv`, `golang.org/x/*`) are plain-Go and expected to work under 1.25 — this task proves it.

**Files:**
- Modify: `go.mod` (the `go` directive only; run `go mod tidy` if it changes sums)
- Modify: `.github/workflows/test.yml` (Go version references — check for `go-version` / `versions:` inputs and any `go 1.22` literal)
- Verify: `Makefile` / `AGENTS.md` golangci-lint pin (v1.64.8) still runs after the bump; if that lint version no longer supports Go 1.25, bump the pin in workflow + Makefile together to the latest 1.x or the 2.x series and record the new pin in `AGENTS.md` — do nothing else in that case
- Test: `go test ./...` (full gate, per AGENTS.md)

**What to implement:**
- `go mod edit -go=1.25.x` (use the latest Go 1.25 patch available; check `https://go.dev/dl` or `go install golang.org/dl/go1.25@latest` locally) and `go mod tidy`.
- Do NOT change any `.go` source files. Do NOT add any dependency.
- If `make lint` fails because golangci-lint is too old for 1.25: bump the pinned version in BOTH the Makefile and the workflow, update `AGENTS.md`'s pin note to match, and document the bump in the commit body.

**Steps:**
- [ ] `go mod edit -go=<1.25 patch> && go mod tidy`
- [ ] `go build ./... && go vet ./...`
  - Did it succeed? If build breaks on a dep, stop and report — this task is infra-only.
- [ ] `gofmt -l .` (must print nothing)
- [ ] `make lint`
  - Did it succeed? If the linter pin is incompatible, apply the pin bump described above and re-run.
- [ ] `go test ./...` (no `TUGBOT_TEST_DATABASE_URL` — DB tests self-skip; if the DB is up locally, also run the DB gate per AGENTS.md)
  - Did all tests pass? If not, fix and re-run.
- [ ] Commit with message: "chore: bump toolchain to Go 1.25.x"

**Acceptance criteria:**
- [ ] `go.mod` begins with `go 1.25.x`; zero `.go` file diffs in the PR
- [ ] Full AGENTS.md gate green (build, vet, gofmt, lint, test)
- [ ] workflow + Makefile lint pin consistent and working

---

### Task 2: `internal/mcp` skeleton — package, DiscordAPI seam, config, selftest

**Context:** Everything in the MCP layer hangs on one seam: the tools must be testable without a live Discord connection, but `*app.App` carries a concrete `*discordgo.Session`. So `internal/mcp` defines a small `DiscordAPI` interface covering exactly the session methods the bridge tools use; production wraps the real session, tests use a fake. This task creates the package, the seam, the SDK server object, the config knob, and the selftest extension — with NO tools registered yet (tasks 3–5). It also establishes the error-handling convention (Discord API error → `IsError` tool result, no Go error for recoverable failures).

**Files:**
- Create: `internal/mcp/mcp.go` (package, `DiscordAPI`, `realDiscord`, `Server`, `NewServer`, `toolErr` helper)
- Create: `internal/mcp/mcp_test.go`
- Modify: `internal/config/config.go` (+ its test `internal/config/config_test.go`)
- Modify: `cmd/tugbot/main.go` (selftest path only — `runSelftest`)
- Modify: `cmd/tugbot/main_test.go` (expected selftest log string)
- Go module: add `github.com/modelcontextprotocol/go-sdk` (Task 1's Go 1.25 is required)

**What to implement:**

`internal/mcp/mcp.go`:
```go
package mcp

// DiscordAPI is the seam: the exact surface of *discordgo.Session the bridge
// tools use. Production wraps App.D; tests use a fake.
type DiscordAPI interface {
	// State-backed (no REST):
	StateGuilds() []*discordgo.Guild
	// REST — the realDiscord impl passes discordgo.WithRetryOnRatelimit(false) on every one of these, so 429s surface as *discordgo.RateLimitError instead of being absorbed by the session's default block-and-retry:
	UserGuilds(limit int, before, after string, withCounts bool) ([]*discordgo.UserGuild, error)
	GuildChannels(id string) ([]*discordgo.Channel, error)
	ChannelMessages(id string, limit int, before, after, around string) ([]*discordgo.Message, error)
	ChannelMessageSend(channelID, content string) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
	// seam name is the plan's; the underlying SESSION method is MessageReactionAdd (v0.29.0 — there is no ChannelMessageReactionAdd):
	MessageReactionAdd(channelID, messageID, emoji string) error
}

type realDiscord struct{ s *discordgo.Session }
// Each method delegates to the corresponding *discordgo.Session method with
// IDENTICAL semantics (StateGuilds returns s.State.Guilds), EXCEPT every REST
// method (all except StateGuilds) additionally passes the request option
// discordgo.WithRetryOnRatelimit(false) so 429s surface to the tool layer.
// Use the verified SESSION names/shapes: MessageReactionAdd; UserGuilds with
// five params (withCounts=false) returning []*UserGuild. Verify with `go doc`
// when in doubt — do not guess argument order.

// Server owns the SDK server + http.Server.
type Server struct {
	discord DiscordAPI
	port    int
	srv     *mcpSDK.Server // import the SDK as mcpSDK or an alias to avoid the package-name clash
}

// NewServer constructs (does NOT start, does NOT bind a port).
func NewServer(d DiscordAPI, port int) *Server

// Start runs the http.Server on 0.0.0.0:{port} (mux: "/mcp" → stateless
// Streamable-HTTP handler; "/healthz" → 200 "ok"). Blocks until ctx cancels,
// then http.Server.Shutdown with a ≤10s grace. Returns the listen error
// (e.g. address-in-use) OR ctx.Err()-equivalent nil on clean shutdown.
func (s *Server) Start(ctx context.Context) error

// HandlerForTest returns the mux for httptest-based tests.
func (s *Server) HandlerForTest() http.Handler
```

Also:
- All tools registered on the SDK server happen inside `NewServer` (later tasks add registrations here).
- `func toolErr(bots, msg string) *mcpSDK.CallToolResult` → `&mcpSDK.CallToolResult{IsError: true, Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: bots + ": " + msg}}}` convention + a `wrapDiscordErr(err error) *mcpSDK.CallToolResult` that type-switches on the TWO REAL discordgo error types (there is no `*discordgo.Error` in v0.29.0): `*discordgo.RESTError` → `discord <Response.Status>: <ResponseBody>` (Response.Status is the status string); `*discordgo.RateLimitError` → `discord rate_limited (retry after <RetryAfter>)`; anything else → generic `discord error: <err.Error()>`. This is the one place these types are understood; the tools just pass errors through it. Test fixtures must construct `&discordgo.RESTError{...}` / `&discordgo.RateLimitError{...}` — do NOT invent a `discordgo.Error` type.

`internal/config`:
- Add `MCPPort int` on `config.Config`, read from `TUGBOT_MCP_PORT`, **default 8642** (always-on: no enabled flag). NOTE: this is a **new convention, not a mirror** of existing style — existing optional envs (e.g. `ADMIN_USER_IDS`) are non-fatal on garbage (skip/0), but a mistyped port should fail LOUD. Behavior: **unset/empty → 8642; set-but-non-numeric → `LoadError` via the same `errs`/`LoadError` path the three required envs use** (both `run()` and `runSelftest()` fail fast on LoadError — check the existing config tests for that path). Port parsed as int.
- No other config changes.

`cmd/tugbot/main.go` — `runSelftest` (only; production wiring is Task 6):
- After the `newHandlers` construction, construct the MCP server: `mcpSrv := mcp.NewServer(&realDiscord{d}, cfg.MCPPort)` (log its construction like the handlers; do NOT call Start). POINTER RULE: declare the `DiscordAPI` methods on `*realDiscord` and construct with `&realDiscord{d}` in EVERY task (including Task 6) — never the value form.
- The existing success log is `"selftest: Discord session and all thirteen handlers constructed"` (WITH the `selftest: ` prefix included — preserve it). Change it to `"selftest: Discord session and all thirteen handlers and the MCP server constructed"`; update `cmd/tugbot/main_test.go`'s expected string to match if it asserts the line (check — `TestSelftestBoundedPoolStart` may only check exit code, in which case no test change is needed there).

**Steps:**
- [ ] Write failing tests in `internal/mcp/mcp_test.go`: (a) `NewServer` returns non-nil with a fake `DiscordAPI` (constructed as `NewServer(fake, 0)`); (b) `Start` on a random free port serves `/healthz` 200 and `/mcp` reachable (GET /mcp → any 4xx from the SDK is OK — it's not the MCP request shape), via `httptest.NewServer(srv.HandlerForTest())`; (c) `Start` on an already-bound port returns an error, not a panic; (d) **Start nil-on-cancel contract** — pins the invariant Task 6's `os.Exit(1)` branch depends on: `Start` on a free port, cancel the ctx after a moment, assert `Start` returns `nil` (NOT `context.Canceled`, NOT `http.ErrServerClosed`). If the natural http.Server idiom leaks `srv.Err()` on cancel, the wrapper must map clean-cancel to `nil`.
- [ ] `go test ./internal/mcp/ -count=1` — confirm it fails (package missing).
- [ ] Add the SDK dep: `go get github.com/modelcontextprotocol/go-sdk@v1.7.0` (or latest v1).
- [ ] Implement `internal/mcp/mcp.go` per the spec above.
- [ ] `go test ./internal/mcp/ -count=1` — pass; fix if not.
- [ ] Add config knob + `internal/config/config_test.go` cases: (unset → 8642); (set `TUGBOT_MCP_PORT=1234` → 1234); (set garbage `TUGBOT_MCP_PORT=abc` → `LoadError`, assert via the existing required-env failure test pattern — check `config_test.go` for that pattern and mirror it).
- [ ] `go test ./internal/config/ -count=1` — pass.
- [ ] Implement the selftest extension + test string change.
- [ ] `go test ./cmd/tugbot/ -run TestSelftest -count=1` (and whatever the existing selftest test name is — check `main_test.go`) — pass.
- [ ] `go build ./... && go vet ./... && gofmt -l . && make lint`
- [ ] `go test ./... -count=1` (no DB URL — self-skip is expected)
- [ ] Commit with message: "feat(mcp): internal/mcp skeleton, DiscordAPI seam, TUGBOT_MCP_PORT config, selftest extension"

**Acceptance criteria:**
- [ ] `internal/mcp` compiles standalone with zero tests needing a live Discord connection
- [ ] Selftest logs the new line and exits 0; does not bind a port
- [ ] Config defaults verified by test: 8642 when unset

---

### Task 3: Discovery tools — `list_guilds`, `list_channels`

**Context:** Discovery tools keep the agent's context small (the prior-art norm: name→ID maps, not ID dumps). `list_guilds` is the top of the chain; it makes the zero-env-var mode self-sufficient (agent discovers guild → channels → messages without any server-side default). Guilds come from in-memory `StateGuilds()` (populated by the READY payload — zero REST); `UserGuilds()` REST is the fallback only when state is empty. `list_channels` is the one REST call per invocation (no cache in v1 — a single extra round-trip beats maintaining a persisted mapping, per the rejected Approach B).

**Files:**
- Modify: `internal/mcp/mcp.go` (registration inside `NewServer`)
- Create: `internal/mcp/tools_discovery.go` (arg structs + handler closures)
- Test: `internal/mcp/mcp_test.go` (in-process client cases)

**What to implement:**

- `listGuildsArgs` has no input fields (empty struct). Handler: `d.StateGuilds()`; if `len == 0`, fall back to `d.UserGuilds(100, "", "", false)` (FIVE-param signature; returns `[]*discordgo.UserGuild` — map each `User{Name/ID}` into the map; one page; Discord caps at 100 — that is fine for discovery; if it returns 100, append the note "(more than 100 guilds; first page only)"). Output: a JSON-serializable map `map[string]string` (name → ID) as the `any` payload AND a one-line text `N guilds: <name>=<id>, …` (capped at 50 entries with "…" after).
- `listChannelsArgs{ GuildID string `json:"guild_id,omitempty"` }` (plain omitempty tag — NO schema-exclusion tag: validate `GuildID == ""` handler-side) — empty `guild_id` is an error: `"no default guild is configured; pass guild_id"` (we deliberately ship no default-guild env — discovery is the path). Handler: `d.GuildChannels(guildID)`, filter text-like channels: `c.Type == discordgo.ChannelTypeGuildText || c.Type == discordgo.ChannelTypeGuildNews` (the v0.29.0 enum has exactly these two text-like guild types — there is NO `ChannelTypeGuildAnnouncement`; do not reference it; exclude categories/voice), output name→ID map + text summary, same shape as list_guilds.
- Both: Discord API error → `wrapDiscordErr`.
- Register both in `NewServer` after `NewServer` builds the SDK server.

**Test harness (establish here, reuse in tasks 4–5):** a `fakeDiscord` in `mcp_test.go` (implements `DiscordAPI` with configurable fixtures + captured calls) and an in-process client helper using the SDK's OWN in-process transport pattern — **there is NO `NewInMemoryClient` constructor, do not hunt for one** (use exactly the pattern from the verified header block of this plan):
```go
ct, st := mcp.NewInMemoryTransports() // (client, server) transport pair
// build the Server (runs NewServer → tools registered), then join both sides (verify the exact join-method names against the SDK's own test file — mcp_test.go / client_test.go — before writing; do not guess):
//   ss, _ := srv.Connect(ctx, st, nil)          // server side
//   c := mcp.NewClient(impl, nil)
//   cs, _ := c.Connect(ctx, ct, nil)            // client side
// drive:
//   tools, _ := cs.ListTools(ctx, nil)
//   res,  _ := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_guilds", Arguments: args})
```
The SDK's own test files are the source of truth for the exact join shape.

**Steps:**
- [ ] Write failing tests: `list_guilds` from state (fixture 3 guilds → map matches); `list_guilds` state-empty → calls `UserGuilds` fallback (fake records the five-arg call); `list_channels` happy path (fixture: 2 text + 1 news + 1 voice + 1 category → map has exactly the 2 text-like: text + news); `list_channels` with empty guild_id → `IsError` + the exact message; Discord error (fake returns `&discordgo.RESTError{Response: &http.Response{Status: "500 Internal Server Error"}, ...}` per `go doc` — NOT a nonexistent `discordgo.Error` type) → `IsError` with the `discord 500 ...` shape.
- [ ] `go test ./internal/mcp/ -count=1` — confirm failure.
- [ ] Implement `tools_discovery.go` + registration.
- [ ] `go test ./internal/mcp/ -count=1` — pass.
- [ ] `go build ./... && go vet ./... && gofmt -l . && make lint`
- [ ] Commit with message: "feat(mcp): list_guilds + list_channels bridge tools"

**Acceptance criteria:**
- [ ] Both tools appear in an in-process client's `ListTools` with the exact names `list_guilds` / `list_channels`
- [ ] All 5 test cases pass; zero tests touch the network

---

### Task 4: `read_messages` (author filter, incremental offsets, 429 rate_limited error surface)

**Context:** The workhorse tool. Prior-art norm: pure REST fetch (no privileged intent, no event buffering) — maps onto `ChannelMessages`. Three deliberately-chosen behaviors: (a) channel addressing accepts snowflake **or** name (name resolved via one `GuildChannels` call per invocation — no cache in v1); (b) author filter (`author_id` / `author_name`) is an in-Go filter of the fetched page — the REST API has no author parameter, so a second page fetch would be a cost we deliberately do not pay in v1 (loops with `before_id` are the agent's job — the stateless design is built for that); (c) Discord 429 → `rate_limited` tool error with retry-after and empty payload (`ChannelMessages` returns `(nil, err)` on failure — there is no partial page to return), NO retry loop.

**Files:**
- Create: `internal/mcp/tools_read.go`
- Modify: `internal/mcp/mcp.go` (registration)
- Test: `internal/mcp/mcp_test.go`

**What to implement:**

```go
type readMessagesArgs struct {
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	Limit     int    `json:"limit,omitempty"` // default 50; >100 → clamp to 100 (silently, with a "limit clamped" note) (0/omitted → 50)
	BeforeID  string `json:"before_id,omitempty"` // named to match the before_arg semantics: messages BEFORE (older than) this ID
	AfterID   string `json:"after_id,omitempty"`
	AuthorID  string `json:"author_id,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
}
```

Run snowflake validation FIRST in-handler (before any REST): `BeforeID`/`AfterID` non-empty and non-numeric → immediate `IsError` result "before_id/after_id must be a snowflake ID". `ChannelID`: numeric → use; non-numeric → resolve via name (reuse Task 3's resolution — factor `resolveChannelID(d DiscordAPI, guildID, nameOrID string) (string, error)` into a small shared helper in `tools.go` for tasks 5 to use).

Handler logic:
1. Resolve channel (guild from `GuildID` arg; if empty and resolution-by-name is needed, `IsError` "guild required to resolve channel by name"; channel by numeric ID needs no guild).
2. `d.ChannelMessages(cid, limit, before, after, "")`.
3. On `*discordgo.RateLimitError` (this is what a 429 surfaces as, because the `realDiscord` wrapper passes `discordgo.WithRetryOnRatelimit(false)` — see Task 2; `ChannelMessages` returns `(nil, err)` on failure — there is NO partial page on a failed fetch): return an **`IsError=true`** result rendered by `wrapDiscordErr(err)` (the single canonical 429 wording: `discord rate_limited (retry after <RetryAfter>)`), with **no structured payload (nil output — do not fabricate an empty `[]`/`{}`**). Do NOT retry in v1 (no retry loop — the agent retries via `after_id`/`before_id` or later calls).
4. Author filter: `AuthorID` (ID is exact match, case-sensitive) OR `AuthorName` (exact match on `m.Author.Username`). Both set → OR semantics. v1 matches `Username` only (documented in the tool description as "filters by username").
5. Output: `any` payload = `[]map[string]any{ {"id","author","timestamp","text","attachment_count"} }`; text summary = `N messages` + first/last timestamps. Zero results → text "0 messages", payload `[]` (not nil).

The tool description string must state: "Reads a channel's recent message history. channel_id accepts a numeric snowflake or a channel name (name form requires guild_id). Author filter matches exact username (author_name) or user ID (author_id). On Discord rate-limit (429) the tool returns a rate_limited error with a retry-after — no retry loop; re-call later."

**Steps:**
- [ ] Write failing tests (fake fixtures): happy path 3-message page; limit clamp (250 → 100 + note); before/after validation (non-numeric → IsError, and assert fake's `ChannelMessages` was called with the right offset args); author_id filter (2 of 3 match); author_name filter (username match, exact; both set → OR); 429 error (fake returns `&discordgo.RateLimitError{...}` per `go doc`) → `IsError=true` + `rate_limited (retry after ...)` text + empty payload (NOT a successful degraded result — a failed fetch has no partial page to return); channel-by-name resolution (fixture channels + assert `GuildChannels` called, wrong name → IsError "channel not found"); empty fixture → "0 messages".
- [ ] `go test ./internal/mcp/ -count=1` — confirm failure.
- [ ] Implement `tools_read.go` + the shared `resolveChannelID` helper + registration.
- [ ] `go test ./internal/mcp/ -count=1` — pass.
- [ ] `go build ./... && go vet ./... && gofmt -l . && make lint`
- [ ] Commit with message: "feat(mcp): read_messages bridge tool (author filter, 429 rate_limited error surface)"

**Acceptance criteria:**
- [ ] All listed test cases pass; the 429 case is an `IsError=true` result with the retry-after text and empty payload
- [ ] Tool description documented as specified

---

### Task 5: `post_message` + `react`

**Context:** The write side, both thin wrappers (prior-art observation: the bridge is boring on purpose — post/read/react is the 90% surface). One deliberate safety scoping: `post_message` truncates text at 1999 chars with a truncation note (Discord's 2000 cap; chunking is v1 explicitly NOT done — an agent that needs a long message can post multiple parts; a note says so).

**Files:**
- Create: `internal/mcp/tools_write.go`
- Modify: `internal/mcp/mcp.go` (registration)
- Test: `internal/mcp/mcp_test.go`

**What to implement:**

`post_message`:
```go
type postMessageArgs struct {
	ChannelID string `json:"channel_id"` // id-or-name, guild_id as in read
	GuildID   string `json:"guild_id,omitempty"`
	Text      string `json:"text"`
	ReplyToID string `json:"reply_to_id,omitempty"`
}
```
- Validate `ReplyToID` numeric before REST. `Text` empty → IsError. >1999 runes → truncate + text note `text truncated to 1999 chars (Discord cap); for longer content post in parts`.
- No `ReplyToID` → `ChannelMessageSend(cid, text)`; with it → `ChannelMessageSendComplex(cid, &discordgo.MessageSend{Content: text, Reference: &discordgo.MessageReference{MessageID: replyToID}})` — the reply field is **`Reference`** (NOT `MessageReference`); verify via `go doc github.com/bwmarrin/discordgo MessageSend` before writing.
- Output: `posted message <id>` + payload `{"id"}`. Discord error → `wrapDiscordErr`.

`react`:
```go
type reactArgs struct {
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}
```
- Validate `MessageID` numeric; empty emoji → IsError. `d.MessageReactionAdd(cid, mid, emoji)` (the session method is `MessageReactionAdd`; the seam method shares that name — there is no `ChannelMessageReactionAdd` in discordgo). Emoji passes through as-is (unicode or `name`/`:name:`/custom-emoji format — document in the tool description "any emoji token Discord accepts; unicode or custom-emoji name"). `IsError` on Discord error. Output `reacted <emoji> on <messageID>`.

**Steps:**
- [ ] Write failing tests: post happy (assert fake captured channel+text); post with reply (assert `MessageSend.Reference.MessageID` set — the field is `Reference`, NOT `MessageReference`); post truncation (2500 runes → 1999 + note); post empty text → IsError; post with non-numeric reply_to → IsError before any call; react happy (assert args captured); react with non-numeric message_id → IsError; react with empty emoji → IsError; Discord error (fake returns `&discordgo.RESTError{...}`) → IsError with the `discord <status>` shape.
- [ ] `go test ./internal/mcp/ -count=1` — confirm failure.
- [ ] Implement `tools_write.go` + registration.
- [ ] `go test ./internal/mcp/ -count=1` — pass.
- [ ] `go build ./... && go vet ./... && gofmt -l . && make lint`
- [ ] Commit with message: "feat(mcp): post_message + react bridge tools"

**Acceptance criteria:**
- [ ] All listed test cases pass; fake captures prove the right REST method + args
- [ ] 5 tools total in `ListTools` at end of this task

---

### Task 6: Production wiring + end-to-end test

**Context:** Tasks 2–5 built the package and tested it in-process; nothing runs it in the live binary yet. This task mounts `Start` in `run()` (same fatal class as pool/bind: port-in-use → `slog.Error` + non-zero exit, documented in the spec), and proves the real wire: httptest + REAL `http.Server` path driven by the SDK client over actual HTTP (not in-process) — the single test that exercises mux, healthz, the stateless handler, and all 5 tools over the wire.

**Files:**
- Modify: `cmd/tugbot/main.go` (`run()` — production path)
- Test: `internal/mcp/mcp_test.go` (wire test)

**What to implement:**

In `run()`, AFTER `newHandlers` (which constructs all 13 handlers) and BEFORE the errgroup block that starts the background loops / `d.Open()`:
- Construct: `mcpSrv := mcp.NewServer(&realDiscord{d}, cfg.MCPPort)`; log `MCP server constructed (tools: list_guilds, list_channels, read_messages, post_message, react; http://0.0.0.0:{cfg.MCPPort}/mcp)` at info level.
- Inside the existing errgroup block (alongside the gulag loops, using the shared `ctx`), with the fatal path EXPLICIT (a bare `eg.Go(func() error { return mcpSrv.Start(ctx) })` is NOT sufficient: a port-in-use error returned that way only surfaces in `eg.Wait()` after SIGTERM, where the existing code logs a `slog.Warn` and continues — that would silently fail the plan's port-conflict acceptance criterion):
```go
eg.Go(func() error {
	if err := mcpSrv.Start(ctx); err != nil {
		// Start returns nil on clean ctx-cancel; anything else is a startup-failure class (port-in-use, etc.)
		slog.Error("MCP server failed to start and bind port", "module", "main", "error", err)
		os.Exit(1)
	}
	return nil
})
```
- Read `cmd/tugbot/main.go` around the errgroup block and SIGTERM handling first: `Start` must return nil-equivalent on ctx cancellation (matching how the existing gulag-loop closures return nil on cancel) so `eg.Wait()` stays clean; the `os.Exit(1)` path is only for the startup-failure class.
- Do NOT change selftest (Task 2 already handles it: constructed, not started — `runSelftest` must NOT call Start; verify this holds).

Wire test (in `mcp_test.go`):
```go
// 1. fake := newFakeDiscord(...fixtures incl. 1 guild, its channels, a 2-message page, post & reaction captures)
// 2. srv := NewServer(fake, 0) // port 0 = ephemeral: bind a net.Listener first to learn the port, or have Start support port 0 → random; simplest: Start binds :0 and exposes the actual port — decide by what Start's signature allows; if Start hardcodes the port, add an (unexported) method that accepts a pre-bound listener? NO — keep it simple: for the test, construct the http.Server path via HandlerForTest() + httptest.NewServer, then run the SDK client against the actual httptest URL. This exercises the full mux + stateless handler over real HTTP without touching Start's port logic.
// 3. client, _ := <SDK client + real HTTP transport to the httptest URL>
// 4. healthz 200; ListTools shows exactly 5 names; one happy-path call per tool; one error shape (bad snowflake).
```
(Adapt step 3 to the SDK's actual client API — check `go doc` + the SDK's own examples/tests; do not guess the constructor.)

**Steps:**
- [ ] Write the wire test (fake, `NewServer(fake, 0)`, `httptest.NewServer(srv.HandlerForTest())`, SDK client with a real HTTP transport pointed at the httptest URL; hit healthz + all 5 tools + one bad-snowflake error shape). It may pass by construction — that is fine; its job is to lock the wire path in.
- [ ] Implement the `run()` wiring with the fatal port-conflict path above.
- [ ] `go test ./internal/mcp/ -count=1` — pass (includes the wire test).
- [ ] `go test ./cmd/tugbot/ -run 'TestSelftest' -count=1` (exact name per `main_test.go`) — pass; verify selftest does NOT bind 8642 (watch `lsof -i :8642` or assert no listen in the test environment output).
- [ ] Manual smoke (if a dev env is available): start the binary with existing env, `curl -s http://127.0.0.1:8642/healthz` → 200. Otherwise note "smoke deferred to done-when verification."
- [ ] `go build ./... && go vet ./... && gofmt -l . && make lint`
- [ ] `go test ./... -count=1` (no DB URL)
- [ ] Commit with message: "feat(mcp): wire the MCP server into the bot's lifecycle; e2e over real HTTP"

**Acceptance criteria:**
- [ ] Bot binary starts with the existing env vars only and serves on 8642 (or `TUGBOT_MCP_PORT`)
- [ ] Port conflict at startup → `slog.Error` + non-zero exit (no panic, no hang)
- [ ] SIGTERM drains cleanly (Start returns nil-equivalent on ctx cancel; errgroup does not fail the process)
- [ ] Wire test green; full AGENTS.md gate green

---

## Out of scope (planned follow-on, the "feature tools")
Feature tools (feature toggles, `/gimmick`, gulag, ask-pi) require a small `Invoke`-style public surface on the event-callback-shaped handlers; they are explicitly NOT part of this plan (v1 = bridge only, five tools). The `DiscordAPI` seam and `*app.App`-only-uses-`App.D` mean feature tools can use `App.Pool`/`App.Pi` without changing this layer's shape.
