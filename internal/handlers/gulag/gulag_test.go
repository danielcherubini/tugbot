package gulag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/db"
)

// ---------------------------------------------------------------------------
// Test DB setup (repo convention: compose PG, skip when unreachable)
// ---------------------------------------------------------------------------

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
}

func testURLEnv() string { return os.Getenv("TUGBOT_TEST_DATABASE_URL") }

func testDBURL() string {
	const defaultURL = "postgres://tugbot:tugbot@127.0.0.1:5432/tugbot_test"
	url := testURLEnv()
	if url == "" {
		return defaultURL
	}
	return url
}

// setupGulagTestDB returns a pool to a reset gulag test schema, or marks
// the test skipped when PG is unreachable.
func setupGulagTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	skipIfShort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Skipf("cannot create pool: %v (is the compose PG running?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v (is the compose PG running?)", err)
	}
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS message_votes;
		DROP TABLE IF EXISTS gulag_users;
		DROP TABLE IF EXISTS servers;
		DROP TABLE IF EXISTS features;
		DROP TYPE IF EXISTS job_status;
		CREATE TYPE job_status AS ENUM ('created', 'running', 'done', 'failure');
		CREATE TABLE message_votes (
			message_id bigint NOT NULL PRIMARY KEY,
			channel_id bigint NOT NULL,
			guild_id bigint NOT NULL,
			user_id bigint NOT NULL,
			total_vote_tally integer NOT NULL,
			voters bigint[] NOT NULL,
			job_status job_status NOT NULL,
			current_vote_tally integer DEFAULT 0 NOT NULL
		);
		CREATE TABLE gulag_users (
			id serial PRIMARY KEY,
			user_id bigint NOT NULL,
			guild_id bigint NOT NULL,
			gulag_role_id bigint NOT NULL,
			channel_id bigint NOT NULL,
			in_gulag boolean NOT NULL,
			gulag_length integer NOT NULL,
			created_at timestamp without time zone NOT NULL,
			release_at timestamp without time zone NOT NULL,
			message_id bigint NOT NULL
		);
		CREATE TABLE servers (
			id serial PRIMARY KEY,
			guild_id bigint NOT NULL,
			gulag_id bigint NOT NULL
		);
		CREATE TABLE features (
			id serial PRIMARY KEY,
			name varchar(255) UNIQUE NOT NULL,
			enabled boolean DEFAULT false NOT NULL
		);
	`); err != nil {
		pool.Close()
		t.Skipf("cannot set up tables: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// gatePool returns a pool whose `gulag` feature row is ENABLED (so the
// silent IsEnabled gate in the reaction handler passes), or skips.
func gatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := setupGulagTestDB(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO features (name, enabled) VALUES ('gulag', true)`); err != nil {
		t.Fatalf("enable gulag feature: %v", err)
	}
	return pool
}

func newGulag(_ *discordgo.Session, pool *pgxpool.Pool) *Gulag {
	// A built session (New with a plainly invalid token) initializes the
	// rate limiter, so the Discord-fail arms of the release/rejoin flows
	// error out instead of panicking. The zero-value sessions callers pass
	// are replaced: their nil rate limiter would panic in any REST call.
	s, err := discordgo.New("123456789012345678")
	if err != nil {
		return &Gulag{pool: pool}
	}
	return &Gulag{d: s, pool: pool}
}

// ---------------------------------------------------------------------------
// 404 cleanup — test-proven with synthetic 404 errors (never string-matched)
// ---------------------------------------------------------------------------

// TestCleanupStaleGulagRow_404 pins the cleanup branch of the release
// loop: a canonical Discord 404 (raw, wrapped once, wrapped twice)
// deletes the stale row; a non-404 (403, plain sentinel) leaves it.
func TestCleanupStaleGulagRow_404(t *testing.T) {
	pool := setupGulagTestDB(t)
	ctx := context.Background()
	g := newGulag(&discordgo.Session{}, pool)

	seed := func(userID int64) int32 {
		var id int32
		if _, err := pool.Exec(ctx,
			`INSERT INTO gulag_users (user_id, guild_id, gulag_role_id, channel_id, in_gulag, gulag_length, created_at, release_at, message_id)
			 VALUES ($1, 2, 3, 4, true, 300, now(), now() + interval '5 minutes', 0)`, userID); err != nil {
			t.Fatalf("seed (user %d): %v", userID, err)
		}
		if err := pool.QueryRow(ctx, `SELECT id FROM gulag_users WHERE user_id = $1`, userID).Scan(&id); err != nil {
			t.Fatalf("get id (user %d): %v", userID, err)
		}
		return id
	}

	fourOhFour := func() error {
		return &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}}
	}

	// no matching row: a 404 against it must not error (and must not touch other rows)
	if err := g.cleanupStaleGulagRow(ctx, -1, fourOhFour()); err != nil {
		t.Errorf("raw 404 against a missing row: %v", err)
	}
	// raw 404 deletes the row
	id := seed(2)
	if err := g.cleanupStaleGulagRow(ctx, id, fourOhFour()); err != nil {
		t.Errorf("raw 404 cleanup: %v", err)
	}
	if existsByID(pool, ctx, id) {
		t.Error("raw 404: row still present, want deleted")
	}
	// one-level-wrapped 404 (errors.As walks the chain)
	id2 := seed(3)
	if err := g.cleanupStaleGulagRow(ctx, id2, errors2Wrap(fourOhFour())); err != nil {
		t.Errorf("wrapped-404 cleanup: %v", err)
	}
	if existsByID(pool, ctx, id2) {
		t.Error("wrapped 404: row still present, want deleted")
	}
	// double-wrapped 404 in a nested chain
	id3 := seed(4)
	deep := fmt.Errorf("outer: %w", fmt.Errorf("mid: %w", fourOhFour()))
	if err := g.cleanupStaleGulagRow(ctx, id3, deep); err != nil {
		t.Errorf("double-wrapped 404 cleanup: %v", err)
	}
	if existsByID(pool, ctx, id3) {
		t.Error("double-wrapped 404: row still present, want deleted")
	}
	// non-404 remainders: a 403 and a plain sentinel leave the row
	id4 := seed(5)
	if err := g.cleanupStaleGulagRow(ctx, id4, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}); err != nil {
		t.Errorf("403 cleanup should be a no-op: %v", err)
	}
	if !existsByID(pool, ctx, id4) {
		t.Error("403: row deleted, want kept")
	}
	if err := g.cleanupStaleGulagRow(ctx, id4, errors.New("network down")); err != nil {
		t.Errorf("plain error cleanup should be a no-op: %v", err)
	}
	if !existsByID(pool, ctx, id4) {
		t.Error("plain sentinel error: row deleted, want kept")
	}
}

func existsByID(pool *pgxpool.Pool, ctx context.Context, id int32) bool {
	row := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM gulag_users WHERE id = $1)`, id)
	var got bool
	if err := row.Scan(&got); err != nil {
		return false
	}
	return got
}

func errors2Wrap(err error) error { return fmt.Errorf("outer: %w", err) }

// ---------------------------------------------------------------------------
// Vote state machine — table test (incl. the 30s stale reset)
// ---------------------------------------------------------------------------

// insertMessageVoteRow fixtures directly into the message_votes table
// (bypassing the handler) so the state-machine branches can be pinned.
func insertMessageVoteRow(t *testing.T, pool *pgxpool.Pool, messageID int64, status string, current, total int32, voters ...int64) {
	t.Helper()
	ws := []int64(voters)
	if ws == nil {
		ws = []int64{}
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO message_votes (message_id, channel_id, guild_id, user_id, total_vote_tally, voters, job_status, current_vote_tally)
		 VALUES ($1, 2, 3, 4, $2, $3::bigint[], $4::job_status, $5)`,
		messageID, total, ws, status, current); err != nil {
		t.Fatalf("insert message vote row: %v", err)
	}
}

func voteStatus(t *testing.T, pool *pgxpool.Pool, messageID int64) db.JobStatus {
	t.Helper()
	var s db.JobStatus
	if err := pool.QueryRow(context.Background(),
		`SELECT job_status FROM message_votes WHERE message_id = $1`, messageID).Scan(&s); err != nil {
		t.Fatalf("read status for %d: %v", messageID, err)
	}
	return s
}

// TestMessageVoteCreateOrUpdate pins the create/or-update semantics of
// db/message_vote.rs: fresh insert (status created, tally 1, voters =
// [voter], user_id = the message author), the idempotent one-vote-per-
// user error text, and the tally increment.
func TestMessageVoteCreateOrUpdate(t *testing.T) {
	pool := setupGulagTestDB(t)
	ctx := context.Background()
	g := newGulag(&discordgo.Session{}, pool)

	_, err := g.messageVoteCreateOrUpdate(ctx, 9001, 1, 2, 3, 4) // author 3, voter 4
	if err != nil {
		t.Fatalf("first vote: %v", err)
	}
	row, err := g.selectMessageVoteByID(ctx, 9001)
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.JobStatus != db.JobStatusCreated || row.CurrentVoteTally != 1 || len(row.Voters) != 1 || row.Voters[0] != 4 {
		t.Fatalf("fresh row = status %q tally %d voters %v, want created/1/[4]", row.JobStatus, row.CurrentVoteTally, row.Voters)
	}
	if row.UserID != 3 {
		t.Fatalf("row user_id = %d, want the message author (3), not the voter", row.UserID)
	}
	_, err = g.messageVoteCreateOrUpdate(ctx, 9001, 1, 2, 3, 4) // same voter again
	if err == nil {
		t.Fatal("second vote by the same voter: missing the idempotency error")
	}
	const wantErr = "You have already Voted"
	if err.Error() != wantErr {
		t.Errorf("idempotency error = %q, want %q", err.Error(), wantErr)
	}
	if _, err := g.messageVoteCreateOrUpdate(ctx, 9001, 1, 2, 3, 5); err != nil {
		t.Fatalf("second voter: %v", err)
	}
	row, _ = g.selectMessageVoteByID(ctx, 9001)
	if row.CurrentVoteTally != 2 || len(row.Voters) != 2 {
		t.Fatalf("after second voter = tally %d voters %v, want 2/[4,5]", row.CurrentVoteTally, row.Voters)
	}
}

// TestSyncFromDiscord pins the overwrite/create semantics: an existing
// row gets its tally + voters replaced by the Discord-fetched list (the
// status is untouched — the reaction handler does not drive status); a
// missing row is created fresh (status created, total 0).
func TestSyncFromDiscord(t *testing.T) {
	pool := setupGulagTestDB(t)
	ctx := context.Background()
	g := newGulag(&discordgo.Session{}, pool)

	insertMessageVoteRow(t, pool, 9002, "created", 2, 0, 7, 8)
	if _, err := g.syncFromDiscord(ctx, 9002, 1, 2, 3, []int64{9, 10, 11, 12, 13}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	row, err := g.selectMessageVoteByID(ctx, 9002)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.CurrentVoteTally != 5 || len(row.Voters) != 5 || row.Voters[0] != 9 || row.JobStatus != db.JobStatusCreated {
		t.Fatalf("synced row = tally %d voters %v status %q, want 5/[9..13]/created", row.CurrentVoteTally, row.Voters, row.JobStatus)
	}
	// missing row -> create
	if _, err := g.syncFromDiscord(ctx, 9003, 1, 2, 3, []int64{21}); err != nil {
		t.Fatalf("sync create: %v", err)
	}
	row, err = g.selectMessageVoteByID(ctx, 9003)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.JobStatus != db.JobStatusCreated || row.CurrentVoteTally != 1 || row.UserID != 3 || row.ChannelID != 2 || row.GuildID != 1 {
		t.Fatalf("created row = %+v, want created/1/author 3/chan 2/guild 1", row)
	}
}

// TestPendingGulagVotesThreshold pins the threshold-and-status predicate:
// only rows with current_vote_tally >= 5 AND status in (created, done)
// are pending; running and failure rows - and sub-threshold rows - are
// out.
func TestPendingGulagVotesThreshold(t *testing.T) {
	pool := setupGulagTestDB(t)
	g := newGulag(&discordgo.Session{}, pool)

	insertMessageVoteRow(t, pool, 7001, "created", 5, 0) // in
	insertMessageVoteRow(t, pool, 7002, "done", 5, 4)    // in
	insertMessageVoteRow(t, pool, 7003, "running", 5, 0) // out (running)
	insertMessageVoteRow(t, pool, 7004, "failure", 9, 0) // out (failure)
	insertMessageVoteRow(t, pool, 7005, "created", 4, 0) // out (below threshold)
	pending, err := g.selectPendingGulagVotes(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	ids := map[int64]bool{}
	for _, p := range pending {
		ids[p.MessageID] = true
	}
	for _, want := range []int64{7001, 7002} {
		if !ids[want] {
			t.Errorf("missing pending row %d in %v", want, ids)
		}
	}
	for _, out := range []int64{7003, 7004, 7005} {
		if ids[out] {
			t.Errorf("row %d listed as pending, want excluded", out)
		}
	}
}

// TestStaleRunningResetWindow pin the 30s Running -> Created reset that
// belongs to the vote loop (not the reaction handler): a fresh
// timestamp suppresses the reset, a 31s-stale one applies it and
// advances the stored epoch.
func TestStaleRunningResetWindow(t *testing.T) {
	pool := setupGulagTestDB(t)
	ctx := context.Background()
	g := newGulag(&discordgo.Session{}, pool)

	insertMessageVoteRow(t, pool, 8001, "running", 5, 0)
	now := time.Now().Unix()
	lastStaleVoteResetAt.Store(now) // fresh: within the 30s window
	g.resetStaleRunningVotes(ctx)
	if got := voteStatus(t, pool, 8001); got != db.JobStatusRunning {
		t.Fatalf("within the 30s window: status = %q, want still running", got)
	}
	lastStaleVoteResetAt.Store(now - 31) // stale: beyond the 30s window
	g.resetStaleRunningVotes(ctx)
	if got := voteStatus(t, pool, 8001); got != db.JobStatusCreated {
		t.Fatalf("after stale reset: status = %q, want created", got)
	}
	if got := lastStaleVoteResetAt.Load(); got < now {
		t.Errorf("stored reset time not advanced: %d", got)
	}
}

// TestFinalDoneTransition pins the done commit of gulag_check_handler:
// total = current + total (computed by the caller), current = 0, voters
// cleared, status done.
func TestFinalDoneTransition(t *testing.T) {
	pool := setupGulagTestDB(t)
	g := newGulag(&discordgo.Session{}, pool)
	insertMessageVoteRow(t, pool, 9004, "running", 5, 4, 1, 2, 3, 4, 5)
	if err := g.setMessageVoteFinalDone(context.Background(), 9004, 5+4); err != nil {
		t.Fatalf("final done: %v", err)
	}
	row, err := g.selectMessageVoteByID(context.Background(), 9004)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.JobStatus != db.JobStatusDone || row.TotalVoteTally != 9 || row.CurrentVoteTally != 0 || len(row.Voters) != 0 {
		t.Fatalf("done row = status %q total %d current %d voters %v, want done/9/0/[]", row.JobStatus, row.TotalVoteTally, row.CurrentVoteTally, row.Voters)
	}
}

// TestFailureTransition pins the failure demotion branch of the vote
// loop (update_message_vote_status with 'failure').
func TestFailureTransition(t *testing.T) {
	pool := setupGulagTestDB(t)
	g := newGulag(&discordgo.Session{}, pool)
	insertMessageVoteRow(t, pool, 9005, "running", 5, 0)
	if err := g.setJobStatus(context.Background(), 9005, db.JobStatusFailure); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := voteStatus(t, pool, 9005); got != db.JobStatusFailure {
		t.Fatalf("status = %q, want failure", got)
	}
}

// ---------------------------------------------------------------------------
// Release loop
// ---------------------------------------------------------------------------

// TestSelectGulagUsersReleasableBoundary pins the release-at predicate:
// a due (past) row is releasable, a future row is not.
func TestSelectGulagUsersReleasableBoundary(t *testing.T) {
	pool := setupGulagTestDB(t)
	g := newGulag(&discordgo.Session{}, pool)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []struct {
		userID int64
		offset time.Duration
	}{{10, -1 * time.Second}, {11, 5 * time.Second}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO gulag_users (user_id, guild_id, gulag_role_id, channel_id, in_gulag, gulag_length, created_at, release_at, message_id)
			 VALUES ($1, 2, 3, 4, true, 300, now(), $2, 0)`, c.userID, now.Add(c.offset)); err != nil {
			t.Fatalf("seed user %d: %v", c.userID, err)
		}
	}
	rows, err := g.selectGulagUsersReleasable(ctx)
	if err != nil {
		t.Fatalf("releasable: %v", err)
	}
	users := map[int64]bool{}
	for _, r := range rows {
		users[r.UserID] = true
	}
	if !users[10] {
		t.Error("due row (release_at in the past) not releasable")
	}
	if users[11] {
		t.Error("future row (release_at in the future) released early")
	}
}

// TestReleaseCheckIteration pins the per-row release flow against a
// non-404 Discord failure: the row flips to in_gulag = false, the
// (always-failing on an empty-token session) role removal leaves the row
// in place, and no delete happens.
func TestReleaseCheckIteration(t *testing.T) {
	pool := setupGulagTestDB(t)
	g := newGulag(&discordgo.Session{}, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO gulag_users (user_id, guild_id, gulag_role_id, channel_id, in_gulag, gulag_length, created_at, release_at, message_id)
		 VALUES (12, 2, 3, 4, true, 300, now(), now() - interval '1 minute', 0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := g.releaseCheckIteration(ctx); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	var inGulag bool
	if err := pool.QueryRow(ctx, `SELECT in_gulag FROM gulag_users WHERE user_id = 12`).Scan(&inGulag); err != nil {
		t.Fatalf("row missing after a non-404 failure (want it kept): %v", err)
	}
	if inGulag {
		t.Error("row still marked in_gulag, want it flipped false (and kept) after a non-404 failure")
	}
}

// ---------------------------------------------------------------------------
// Setup shapes — option strings byte-identical to Rust
// ---------------------------------------------------------------------------

type shapeAssertion struct {
	wantType        discordgo.ApplicationCommandType
	wantName        string
	wantDescription string
	wantOptions     []optionAssertion
}

type optionAssertion struct {
	optType     discordgo.ApplicationCommandOptionType
	name        string
	description string
	required    bool
}

func TestSetupCommandShapes(t *testing.T) {
	shapes := newGulag(nil, nil).commandShapes()
	if len(shapes) != 4 {
		t.Fatalf("got %d command shapes, want exactly 4 (3 slices + the Add Gulag Vote message command)", len(shapes))
	}

	slash := []shapeAssertion{
		{
			wantType: discordgo.ChatApplicationCommand, wantName: "gulag",
			wantDescription: "Send a user to the Gulag",
			wantOptions: []optionAssertion{
				{optType: discordgo.ApplicationCommandOptionUser, name: "user", description: "The user to lookup", required: true},
				{optType: discordgo.ApplicationCommandOptionString, name: "reason", description: "Why Are you sending them", required: true},
				{optType: discordgo.ApplicationCommandOptionInteger, name: "length", description: "How Long minutes", required: true},
			},
		},
		{
			wantType: discordgo.ChatApplicationCommand, wantName: "gulag-release",
			wantDescription: "Release a user from the Gulag",
			wantOptions: []optionAssertion{
				{optType: discordgo.ApplicationCommandOptionUser, name: "user", description: "The user to lookup", required: true},
			},
		},
		{
			wantType: discordgo.ChatApplicationCommand, wantName: "gulag-list",
			wantDescription: "List users in the Gulag",
			wantOptions:     nil,
		},
		{
			wantType: discordgo.MessageApplicationCommand, wantName: "Add Gulag Vote",
			wantDescription: "", // Rust: no description set on the message command
			wantOptions:     nil,
		},
	}
	for i, want := range slash {
		got := shapes[i]
		if got.Type != want.wantType {
			t.Errorf("shape %d: type = %v, want %v", i, got.Type, want.wantType)
		}
		if got.Name != want.wantName {
			t.Errorf("shape %d: name = %q, want %q", i, got.Name, want.wantName)
		}
		if got.Description != want.wantDescription {
			t.Errorf("shape %d: description = %q, want %q", i, got.Description, want.wantDescription)
		}
		if len(got.Options) != len(want.wantOptions) {
			t.Fatalf("shape %d: %d options, want %d", i, len(got.Options), len(want.wantOptions))
		}
		for j, o := range want.wantOptions {
			gotOpt := got.Options[j]
			if gotOpt.Type != o.optType {
				t.Errorf("shape %d opt %d: type = %v, want %v", i, j, gotOpt.Type, o.optType)
			}
			if gotOpt.Name != o.name {
				t.Errorf("shape %d opt %d: name = %q, want %q", i, j, gotOpt.Name, o.name)
			}
			if gotOpt.Description != o.description {
				t.Errorf("shape %d opt %d: description = %q, want %q", i, j, gotOpt.Description, o.description)
			}
			if gotOpt.Required != o.required {
				t.Errorf("shape %d opt %d: required = %v, want %v", i, j, gotOpt.Required, o.required)
			}
		}
	}
	// The dead `gulag-vote` command must NOT be among the shapes.
	for _, s := range shapes {
		if s.Name == "gulag-vote" {
			t.Fatal("dead gulag-vote command is registered — do not port its command")
		}
	}
}

// ---------------------------------------------------------------------------
// remod no-op — the port NEVER touches the remod column
// ---------------------------------------------------------------------------

func TestRemodIsUntouchedByPortedSQL(t *testing.T) {
	for name, sql := range map[string]string{
		"releasable":    selectGulagUsersReleasableSQL,
		"pending":       pendingGulagVotesSQL,
		"delete":        deleteGulagUserSQL,
		"setNotInGulag": setGulagUserNotInGulagSQL,
		"staleReset":    staleRunningVoteResetSQL,
	} {
		if containsCaseInsensitive(sql, "remod") {
			t.Errorf("%s SQL touches remod; remod is write-only-else-never-read in Rust (save default) — no action to port", name)
		}
	}
	// and the model never reads a remod field either (compile-level
	// guarantee: db.GulagUser has no Remod field — exercised everywhere
	// via row scans above).
	_ = db.GulagUser{}
}

func containsCaseInsensitive(s, sub string) bool {
	return len(s) >= len(sub) && (containsFoldHelper(s, sub))
}

func containsFoldHelper(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := lowerRune(s[i+j]), lowerRune(sub[j])
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lowerRune(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// ---------------------------------------------------------------------------
// gulag-list time info — byte parity with Rust's Duration Display
// ---------------------------------------------------------------------------

func TestGulagListTimeInfo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		releaseAt  time.Time
		wantPrefix string
		want       string
	}{
		{"exact 300s in the future (Rust Duration 300s)", now.Add(300 * time.Second), "", "releases in 5m 0s"},
		{"3661s in the future", now.Add(3661 * time.Second), "", "releases in 1h 1m 1s"},
		{"45s in the future", now.Add(45 * time.Second), "", "releases in 45s"},
		{"0.5s in the future (trailing-zero trimmed)", now.Add(500 * time.Millisecond), "", "releases in 0.5s"},
		{"100s in the future", now.Add(100 * time.Second), "", "releases in 1m 40s"},
		{"50s overdue", now.Add(-50 * time.Second), "", "overdue for release (50s ago)"},
		{"Rust test anchor: 41477253s overdue", now.Add(-41477253 * time.Second), "", "overdue for release (41477253s ago)"},
	}
	for _, c := range cases {
		got := listTimeInfo(c.releaseAt, now)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Command dispatch
// ---------------------------------------------------------------------------

// TestHandleGulagMissingGuild pins the FIRST guard (Rust
// gulag_handler.rs:39-49): an interaction with no guild returns the
// Ephemeral "Error: This command can only be used in a guild" — the
// old ported "no member" arm came from Rust's dead inner-match arm
// (unreachable behind that first guard) and is no longer the rationale.
func TestHandleGulagMissingGuild(t *testing.T) {
	pool := gatePool(t)
	g := newGulag(&discordgo.Session{}, pool)
	i := commandInteraction("gulag")
	i.GuildID = ""
	resp := g.handleGulag(context.Background(), i)
	const want = "Error: This command can only be used in a guild"
	if resp.Content != want || !resp.Ephemeral {
		t.Fatalf("missing-guild /gulag = %+v, want ephemeral %q", resp, want)
	}
}

// TestAddToGulagErrorCasing pins Rust's capital-first with_context
// strings (mod.rs:212-278) that surface to the user via /gulag's
// "Failed to send to gulag: {err}". The first arm (the failed member
// fetch on a bad-token session) is exercised here; no DB is reached on
// this error path.
func TestAddToGulagErrorCasing(t *testing.T) {
	g := newGulag(&discordgo.Session{}, nil)
	_, err := g.AddToGulag(context.Background(), GulagParams{
		GuildID: "2", UserID: "3", GulagRoleID: "4",
		GulagLength: 300, ChannelID: "5", MessageID: "0",
	})
	if err == nil || !strings.Contains(err.Error(), "Failed to get guild member") {
		t.Fatalf("err = %v, want the Rust-cased \"Failed to get guild member\" context", err)
	}
}

// ---------------------------------------------------------------------------
// Option-value shape (discordgo decodes Option.Value into interface{} as
// delivered by Discord: USER options = the user's snowflake STRING,
// INTEGER options = JSON numbers → float64, string options = string).
// ---------------------------------------------------------------------------

type fakeGulagSurface struct {
	roles  []*discordgo.Role
	member *discordgo.Member
}

func (f *fakeGulagSurface) GuildChannels(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	return nil, nil
}

func (f *fakeGulagSurface) GuildRoles(_ string, _ ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	return f.roles, nil
}

func (f *fakeGulagSurface) GuildMember(_ string, _ string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	return f.member, nil
}

func (f *fakeGulagSurface) GuildMemberRoleAdd(_ string, _ string, _ string, _ ...discordgo.RequestOption) error {
	return nil
}

// TestHandleGulagOptionValueShapes pins the raw option-value contract:
// a USER option arrives as the user's snowflake string and an INTEGER
// option arrives as a JSON number (float64). The old port asserted
// *discordgo.User / int64 — both always failed, so every /gulag
// invocation died with "Please provide a valid user" and the length
// option silently fell through to the 300s default (proven live at
// cutover). Pinned: the string user value + float64 length reach the
// success arm (non-ephemeral, with-reason text), and a non-string user
// value still yields the non-ephemeral "Please provide a valid user".
func TestHandleGulagOptionValueShapes(t *testing.T) {
	pool := gatePool(t)
	g := newGulag(&discordgo.Session{}, pool)
	g.discordSurface = &fakeGulagSurface{
		roles: []*discordgo.Role{{ID: "900", Name: "admin"}, {ID: "777", Name: "gulag"}},
		// invokerMember hands back this fetched member — it must
		// carry the admin role or the role gate rejects before the
		// options are ever parsed.
		member: &discordgo.Member{User: &discordgo.User{ID: "5"}, Roles: []string{"900"}},
	}
	i := commandInteraction("gulag")
	i.GuildID = "100"
	i.ChannelID = "101"
	i.Member = &discordgo.Member{User: &discordgo.User{ID: "5"}, Roles: []string{"900"}}
	data := i.Data.(discordgo.ApplicationCommandInteractionData)
	data.Options = []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "user", Value: "899"},         // snowflake string
		{Name: "reason", Value: "be kind"},   // string option
		{Name: "length", Value: float64(60)}, // JSON number
	}
	i.Data = data

	resp := g.handleGulag(context.Background(), i)
	const want = "Sending <@899> to the Gulag for 60 minutes, because be kind"
	if resp.Content != want || resp.Ephemeral {
		t.Fatalf("success arm = %+v, want non-ephemeral %q", resp, want)
	}

	// A non-string user value still pins the old arm's text.
	data.Options[0].Value = 899
	i.Data = data
	resp = g.handleGulag(context.Background(), i)
	if resp.Content != "Please provide a valid user" || resp.Ephemeral {
		t.Fatalf("bad user value = %+v, want non-ephemeral %q", resp, "Please provide a valid user")
	}
}

// TestSendToGulagAndMessageMissingRoleCasing pins the role-miss text
// of the shared vote path (Rust mod.rs:293 with_context parity):
// capital-first, log-only surface.
func TestSendToGulagAndMessageMissingRoleCasing(t *testing.T) {
	g := newGulag(&discordgo.Session{}, nil)
	err := g.sendToGulagAndMessage(context.Background(), 2, 3, 4, 5, nil)
	if err == nil || !strings.Contains(err.Error(), "Couldn't find gulag role") {
		t.Fatalf("err = %v, want Rust-cased \"Couldn't find gulag role\"", err)
	}
}

// TestHandleCommandCreateFallthrough pins the dispatch fallthrough and the
// exact no-period gate text for the /gulag slash.
func TestHandleCommandCreateFallthrough(t *testing.T) {
	pool := setupGulagTestDB(t) // no enabled-gate row: the gate is disabled
	g := newGulag(&discordgo.Session{}, pool)

	fallResp := g.HandleCommandCreate(commandInteraction("no-such-command"))
	if fallResp.Content != "Not Implemented" || !fallResp.Ephemeral {
		t.Fatalf("fallthrough = %+v, want ephemeral \"Not Implemented\"", fallResp)
	}

	const want = "Gulag feature is currently disabled" // no trailing period (Rust parity: differs from the message-com revisit text)
	resp := g.HandleCommandCreate(commandInteraction("gulag"))
	if resp.Content != want || !resp.Ephemeral {
		t.Fatalf("disabled-gate response = %+v, want ephemeral %q", resp, want)
	}
}

// TestAddGulagVoteIntegration pins the context-menu command end-to-end
// (DB only — no Discord HTTP on this path): gate, target_id ->
// resolved message, the message's AUTHOR is the gulaged user, the
// invoker is the voter, and the exact response texts (incl. the follow-up
// message and the missing/nil arms).
func TestAddGulagVoteIntegration(t *testing.T) {
	pool := gatePool(t) // feature enabled
	ctx := context.Background()
	g := newGulag(&discordgo.Session{}, pool)

	mkInteraction := func(targetID string, resolved map[string]*discordgo.Message, guildID string) *discordgo.Interaction {
		data := discordgo.ApplicationCommandInteractionData{Name: "Add Gulag Vote", TargetID: targetID}
		if resolved != nil {
			data.Resolved = &discordgo.ApplicationCommandInteractionDataResolved{Messages: resolved}
		}
		return &discordgo.Interaction{
			GuildID:   guildID,
			ChannelID: "2",
			User:      &discordgo.User{ID: "8"},
			Data:      data,
		}
	}
	targetMessage := &discordgo.Message{ID: "9002", ChannelID: "2", Author: &discordgo.User{ID: "7"}}

	resp := g.HandleCommandCreate(mkInteraction("9002", map[string]*discordgo.Message{"9002": targetMessage}, "1"))
	want := "A gulag vote has been added to https://discord.com/channels/1/2/9002\nThere are now 1 unique votes total"
	if resp.Content != want || !resp.Ephemeral {
		t.Fatalf("first vote = %+v, want %q (ephemeral)", resp, want)
	}
	row, err := g.selectMessageVoteByID(ctx, 9002)
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.UserID != 7 {
		t.Fatalf("row user (the gulaged user) = %d, want the message's author (7)", row.UserID)
	}
	if lens := len(row.Voters); lens != 1 || row.Voters[0] != 8 {
		t.Fatalf("voters = %v, want [8] (the invoker)", row.Voters)
	}
	// second (unique) voter
	resp = g.HandleCommandCreate(func() *discordgo.Interaction {
		i := mkInteraction("9002", map[string]*discordgo.Message{"9002": targetMessage}, "1")
		i.User = &discordgo.User{ID: "9"}
		return i
	}())
	want2 := "A gulag vote has been added to https://discord.com/channels/1/2/9002\nThere are now 2 unique votes total"
	if resp.Content != want2 {
		t.Fatalf("second vote = %q, want %q", resp.Content, want2)
	}
	// duplicate voter: the raw error text surfaces
	resp = g.HandleCommandCreate(mkInteraction("9002", map[string]*discordgo.Message{"9002": targetMessage}, "1"))
	if resp.Content != "You have already Voted" || !resp.Ephemeral {
		t.Fatalf("duplicate voter = %+v, want the raw error text (ephemeral)", resp)
	}
	// missing target
	resp = g.HandleCommandCreate(mkInteraction("", map[string]*discordgo.Message{"9002": targetMessage}, "1"))
	if resp.Content != "No target message found." {
		t.Fatalf("no target = %q, want \"No target message found.\"", resp.Content)
	}
	// unresolvable target
	resp = g.HandleCommandCreate(mkInteraction("77", map[string]*discordgo.Message{"9002": targetMessage}, "1"))
	if resp.Content != "Could not resolve target message." {
		t.Fatalf("unresolved target = %q, want \"Could not resolve target message.\"", resp.Content)
	}
	// missing guild
	resp = g.HandleCommandCreate(mkInteraction("9002", map[string]*discordgo.Message{"9002": targetMessage}, ""))
	if resp.Content != "This command can only be used in a server." {
		t.Fatalf("missing guild = %q, want \"This command can only be used in a server.\"", resp.Content)
	}
}

// TestGulagSlashDisabledGateAndSetupCommand pin the gate text and the
// servers-table-driven registration (a servers row registers all 4
// shapes under that guild id; errors are collected, not fatal).
func TestSetupCommandRegistersPerServer(t *testing.T) {
	pool := setupGulagTestDB(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO servers (guild_id, gulag_id) VALUES (501, 601), (502, 602)`); err != nil {
		t.Fatalf("seed servers: %v", err)
	}
	g := newGulag(&discordgo.Session{}, pool)
	sess, err := discordgo.New("123456789012345678") // invalid-token session: every create fails, collected
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	errs := g.SetupCommand(sess, "123456789012345678")
	if len(errs) == 0 {
		t.Fatal("expected per-guild registration errors from the invalid-token session")
	}
	if len(errs) != 8 { // 2 servers × 4 shapes
		t.Fatalf("got %d errors, want 8 (2 servers × 4 shapes)", len(errs))
	}
}

// commandInteraction pins a minimal squad command interaction for the
// delivery tests.
func commandInteraction(name string) *discordgo.Interaction {
	return &discordgo.Interaction{
		GuildID: "1",
		Member:  &discordgo.Member{User: &discordgo.User{ID: "1"}},
		Data:    discordgo.ApplicationCommandInteractionData{Name: name},
	}
}
