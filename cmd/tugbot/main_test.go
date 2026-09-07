package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
)

// testDBURL returns the integration test database URL: env override or the
// docker-compose PG test database.
func testDBURL() string {
	if u := os.Getenv("TUGBOT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres@127.0.0.1:5432/tugbot_test?timezone=UTC"
}

// setupTestDB returns a pool to a reset servers table, or nil (and marks
// the test skipped) when PG is unreachable. Mirrors the internal/features
// and internal/gulag integration-test pattern.
func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Skipf("cannot create pool: %v (is the test PG running?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v (is the test PG running?)", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS servers (
		id serial PRIMARY KEY,
		guild_id bigint NOT NULL,
		gulag_id bigint NOT NULL
	); TRUNCATE servers RESTART IDENTITY;`); err != nil {
		pool.Close()
		t.Skipf("cannot reset servers table: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestApp is the integration-test constructor: a real pool (test PG)
// and an UNOPENED session — the seams stand in for every REST call, so
// no gateway is ever connected.
func newTestApp(pool *pgxpool.Pool) *app.App {
	return app.NewApp(nil, pool, &discordgo.Session{})
}

// TestReadyThreeWayKeepsNegativeIDRows locks the Rust servers.rs:142-156
// parity arm (M6): a row whose guild_id/gulag_id is negative (u64::try_from
// domain) must log a conversion error and KEEP the row — never call the
// REST verification arm, never delete (a negative id would 404 and the
// buggy arm deletes it).
func TestReadyThreeWayKeepsNegativeIDRows(t *testing.T) {
	pool := setupTestDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO servers (guild_id, gulag_id)
		VALUES (-123, 456), (123, -789)`); err != nil {
		t.Fatalf("seed servers: %v", err)
	}

	h := &handlers{app: newTestApp(pool)}
	called := map[string]bool{}
	h.checkGuildFu = func(gid string) (bool, error) {
		called[gid] = true
		return true, nil
	}

	rows := h.readyThreeWay(ctx)
	for g := range called {
		if len(g) > 0 && g[0] == '-' {
			t.Fatalf("REST verification arm was called for negative guild id %q — Rust keeps the row (conversion error, no REST, no delete)", g)
		}
	}
	if len(rows) > 0 {
		t.Errorf("negative-id rows must be excluded from the result (kept in DB, not verified), got %d rows", len(rows))
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM servers`).Scan(&n); err != nil {
		t.Fatalf("count servers: %v", err)
	}
	if n != 2 {
		t.Errorf("negative-id rows must be KEPT in the database, got %d rows (want 2)", n)
	}
}

// TestRegisterCommandsUsesReadySliceRustOrder locks the single-load reuse
// (M3: registerCommands takes the readyThreeWay slice, no second read) and
// the Rust mod.rs:285-302 vector order on the six non-gulag shapes
// (M11: AI Slop, horny, phony, feature, cull, gimmick).
func TestRegisterCommandsUsesReadySliceRustOrder(t *testing.T) {
	pool := setupTestDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO servers (guild_id, gulag_id)
		VALUES (999, 888)`); err != nil {
		t.Fatalf("seed servers: %v", err)
	}

	a := newTestApp(pool)
	h := newHandlers(a)
	var setups, calls int
	h.guildSetupFu = func() []error {
		setups++
		return nil
	}
	var shapes []string
	h.applyShapeFu = func(gid string, name string) error {
		calls++
		shapes = append(shapes, gid+"/"+name)
		return nil
	}
	h.checkGuildFu = func(string) (bool, error) { return true, nil }

	rows := h.readyThreeWay(ctx)
	if len(rows) != 1 {
		t.Fatalf("readyThreeWay: want the 1 seeded row, got %v", rows)
	}
	h.registerCommands(ctx, rows)

	if setups != 1 {
		t.Errorf("gulag command setup ran %d times (want exactly 1)", setups)
	}
	want := []string{
		"999/AI Slop", "999/horny", "999/phony", "999/feature", "999/cull",
		"999/gimmick",
	}
	if len(shapes) != len(want) {
		t.Fatalf("shape registrations: got %v, want %v", shapes, want)
	}
	for i := range want {
		if shapes[i] != want[i] {
			t.Errorf("shape %d: got %q, want %q (Rust mod.rs vector order: AI Slop, horny, phony, feature, cull, gimmick)", i, shapes[i], want[i])
		}
	}
}

// TestDeliverInteractionACKRace pins the in-flight ACK-deadline race
// (see deliverInteraction in main.go): fast work is delivered as the
// plain reply (the bad-token session's 401 is logged, not asserted in
// the stub arm); a fast defer response (cull's contract) ACKs on its
// own without touching the watchdog seams; a fast-path respond FAILURE
// falls back to the slow path (single defer-ACK, then the whole
// delivery set as follow-ups); slow work (work outlives
// interactionAckBudget) first ACKs via the watchdog's defer, then
// delivers the delivery set (chunks, else content) as follow-ups, with
// the ephemeral flag passed through. Counters are reset per arm: the
// per-arm expectations hold only with fresh counters.
func TestDeliverInteractionACKRace(t *testing.T) {
	s, err := discordgo.New("123456789012345678")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	h := &handlers{app: app.NewApp(nil, nil, s)}
	var ackCalls int
	var followupContents [][]string
	var followupEphemeral []bool
	h.deferAckFu = func() error {
		ackCalls++
		return nil
	}
	h.followupFu = func(chunks []string, ephemeral bool) error {
		followupContents = append(followupContents, chunks)
		followupEphemeral = append(followupEphemeral, ephemeral)
		return nil
	}
	i := &discordgo.Interaction{ID: "1", Type: discordgo.InteractionApplicationCommand}

	// Shrink the budget for the test (5x margin over the fast paths
	// keeps the race deterministic even under scheduling load).
	old := interactionAckBudget
	interactionAckBudget = 30 * time.Millisecond
	defer func() { interactionAckBudget = old }()

	reset := func() {
		ackCalls = 0
		followupContents = nil
		followupEphemeral = nil
	}
	wantFollowUps := func(name string, want [][]string, wantFlags ...bool) {
		if len(followupContents) != len(want) {
			t.Fatalf("%s: followups=%v, want %v", name, followupContents, want)
		}
		for n, got := range followupContents {
			if len(got) != len(want[n]) {
				t.Fatalf("%s: followup[%d]=%v, want %v", name, n, got, want[n])
			}
			for k, c := range got {
				if c != want[n][k] {
					t.Fatalf("%s: followup[%d]=%v, want %v", name, n, got, want[n])
				}
			}
		}
		if len(wantFlags) > 0 && (len(followupEphemeral) != len(wantFlags) || followupEphemeral[0] != wantFlags[0]) {
			t.Fatalf("%s: flags=%v, want %v", name, followupEphemeral, wantFlags)
		}
	}

	// Fast plain, single message: stub respond, watchdog untouched.
	reset()
	h.respondFu = func(reply) error { return nil }
	h.deliverInteraction(i, func() reply { return reply{content: "fresh"} })
	if ackCalls != 0 || len(followupContents) != 0 {
		t.Fatalf("fast: ack=%d followup=%v, want none", ackCalls, followupContents)
	}
	// Fast defer response (cull's contract): its own ACK, watchdog
	// untouched.
	reset()
	h.respondFu = func(reply) error { return nil }
	h.deliverInteraction(i, func() reply { return reply{content: "cull", deferResp: true, ephemeral: true} })
	if ackCalls != 0 || len(followupContents) != 0 {
		t.Fatalf("fast deferResp: ack=%d followup=%v, want none", ackCalls, followupContents)
	}
	// Fast plain, respond FAILURE: the real session's 401 exercises the
	// fallback — single defer-ACK, then the delivery set as follow-ups.
	reset()
	h.respondFu = nil
	h.deliverInteraction(i, func() reply { return reply{content: "fresh"} })
	if ackCalls != 1 {
		t.Fatalf("fast failure: ack=%d, want 1", ackCalls)
	}
	wantFollowUps("fast failure", [][]string{{"fresh"}}, false)
	// Fast plain, multi-chunk (success arm of the fan-out): chunk 0
	// rides the interaction; the remainder follow up.
	reset()
	h.respondFu = func(reply) error { return nil }
	h.deliverInteraction(i, func() reply { return reply{content: "c0", chunks: []string{"c0", "c1", "c2"}, ephemeral: true} })
	if ackCalls != 0 {
		t.Fatalf("fast multi: ack=%d, want 0", ackCalls)
	}
	wantFollowUps("fast multi", [][]string{{"c1", "c2"}}, true)
	// Fast plain multi-chunk, respond failure: full-set fallback (no
	// mix: nothing was delivered initially).
	reset()
	h.respondFu = nil
	h.deliverInteraction(i, func() reply { return reply{content: "c0", chunks: []string{"c0", "c1"}} })
	if ackCalls != 1 {
		t.Fatalf("fast multi failure: ack=%d, want 1", ackCalls)
	}
	wantFollowUps("fast multi failure", [][]string{{"c0", "c1"}}, false)
	// Slow single message: watchdog ACK, then the delivery set as a
	// follow-up (non-ephemeral content).
	reset()
	h.deliverInteraction(i, func() reply { time.Sleep(150 * time.Millisecond); return reply{content: "solo"} })
	if ackCalls != 1 {
		t.Fatalf("slow: ack=%d, want 1", ackCalls)
	}
	wantFollowUps("slow", [][]string{{"solo"}}, false)
	// Slow multi-chunk: the whole delivery set follows up.
	reset()
	h.deliverInteraction(i, func() reply {
		time.Sleep(150 * time.Millisecond)
		return reply{content: "s0", chunks: []string{"s0", "s1", "s2"}}
	})
	if ackCalls != 1 {
		t.Fatalf("slow multi: ack=%d, want 1", ackCalls)
	}
	wantFollowUps("slow multi", [][]string{{"s0", "s1", "s2"}}, false)
	// Slow multi-chunk ephemeral: the follow-up carries the flag
	// through.
	reset()
	h.deliverInteraction(i, func() reply {
		time.Sleep(150 * time.Millisecond)
		return reply{content: "s0", chunks: []string{"s0", "s1"}, ephemeral: true}
	})
	if ackCalls != 1 {
		t.Fatalf("slow multi ephemeral: ack=%d, want 1", ackCalls)
	}
	wantFollowUps("slow multi ephemeral", [][]string{{"s0", "s1"}}, true)
}

// TestDiscordgoToken pins the Rust->discordgo token-contract
// adaptation: the .env holds the RAW token (discord-rs convention),
// discordgo needs the "Bot " prefix in the Authorization header, and an
// already-prefixed value must not be double-prefixed.
func TestDiscordgoToken(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"rawtoken.part.1", "Bot rawtoken.part.1"},
		{"Bot rawtoken.part.1", "Bot rawtoken.part.1"},
	}
	for _, tt := range tests {
		if got := discordgoToken(tt.in); got != tt.want {
			t.Errorf("discordgoToken(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSelftestBoundedPoolStart locks the I5 contract at the selftest
// site: with the compose PG unreachable, runSelftest must FAIL FAST on
// the 30s pool deadline — the unbounded context.Background() version
// hangs indefinitely on a dead network path.
func TestSelftestBoundedPoolStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs a black-holed PG (testing.Short)")
	}
	// The test override (TUGBOT_TEST_SELFTEST_DATABASE_URL) now points
	// at an unroutable host, so the selftest's initial Ping blocks
	// until the context deadline (pgxpool.Ping honors ctx).
	t.Setenv("TUGBOT_TEST_SELFTEST_DATABASE_URL",
		"postgres://postgres:postgres@10.255.255.1:5432/tugbot")

	start := time.Now()
	rc := runSelftest()
	elapsed := time.Since(start)
	if rc == 0 {
		t.Fatalf("runSelftest: want failure (PG unreachable), got 0")
	}
	const bound = 35 * time.Second
	if elapsed > bound {
		t.Fatalf("runSelftest took %v — the NewPool call is not bounded by the 30s deadline (I5 breach)", elapsed)
	}
}
