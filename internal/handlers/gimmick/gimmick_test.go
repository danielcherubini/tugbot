// Package gimmick test: the /gimmick add|list|delete command. Fake
// store + fake gate (no DB writes — PG is never touched here; the
// poolStore SQL is pinned in-package by code review, the command
// surface is pinned by these arms).
//
// Design constraint under test (ADR docs/decisions/0003-gimmick-
// command-ungated.md, reviewer-verified): the package has NO
// feature-flag check — verified at the grep level by the step
// requiring zero references to the internal feature-flag seam.
package gimmick

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Fakes (in-package; mirrors derpies_test.go's house style)
// ---------------------------------------------------------------------------

// fakeStore implements the store seam, with scripted results and
// call recording (added entries are "word|source").
type fakeStore struct {
	rows      []gimmickRow
	listErr   error
	listCalls int
	addN      int64
	addErr    error
	added     []string
	deleteN   int64
	deleteErr error
	deleted   []string
}

func (s *fakeStore) listGimmicks(_ context.Context) ([]gimmickRow, error) {
	s.listCalls++
	return s.rows, s.listErr
}

func (s *fakeStore) addGimmick(_ context.Context, word, source string) (int64, error) {
	s.added = append(s.added, word+"|"+source)
	return s.addN, s.addErr
}

func (s *fakeStore) deleteGimmick(_ context.Context, word string) (int64, error) {
	s.deleted = append(s.deleted, word)
	return s.deleteN, s.deleteErr
}

// fakeGate implements the gate seam, controlling member-fetch
// success/failure and role allow/deny (with call counters for the
// "gate not consulted" arms).
type fakeGate struct {
	member      *discordgo.Member
	memberErr   error
	allowed     bool
	memberCalls int
	roleCalls   int
}

func (g *fakeGate) guildMember(_, _ string) (*discordgo.Member, error) {
	g.memberCalls++
	return g.member, g.memberErr
}

func (g *fakeGate) memberHasAnyRole(_ context.Context, _ string, _ *discordgo.Member) bool {
	g.roleCalls++
	return g.allowed
}

// newTestGimmick builds the handler with the fakes assigned directly
// (the app field is nil — the command never touches it).
func newTestGimmick(st *fakeStore, gate *fakeGate) *Gimmick {
	return &Gimmick{store: st, gate: gate}
}

// cmdInteraction builds an ApplicationCommandInteractionData
// interaction: the one subcommand option (whose Options holds the
// "word" option when wordOpt != nil).
func cmdInteraction(guildID string, subName string, wordOpt interface{}) *discordgo.Interaction {
	sub := &discordgo.ApplicationCommandInteractionDataOption{
		Name: subName,
		Type: discordgo.ApplicationCommandOptionSubCommand,
	}
	if wordOpt != nil {
		sub.Options = []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "word", Type: discordgo.ApplicationCommandOptionString, Value: wordOpt},
		}
	}
	return &discordgo.Interaction{
		GuildID: guildID,
		Member:  &discordgo.Member{User: &discordgo.User{ID: "user1"}},
		User:    &discordgo.User{ID: "user1"},
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    "gimmick",
			Options: []*discordgo.ApplicationCommandInteractionDataOption{sub},
		},
	}
}

// allowedGate is the gate the "gate passes" arms use.
func allowedGate() *fakeGate {
	return &fakeGate{
		member:  &discordgo.Member{User: &discordgo.User{ID: "user1"}, Roles: []string{"r1"}},
		allowed: true,
	}
}

// requireEphemeral asserts the every-arm contract.
func requireEphemeral(t *testing.T, resp Response) {
	t.Helper()
	if !resp.Ephemeral {
		t.Errorf("Ephemeral = false, want true (every gimmick response is ephemeral)")
	}
}

// requireStoreUntouched asserts a gate/option failure reached no SQL.
func requireStoreUntouched(t *testing.T, st *fakeStore) {
	t.Helper()
	if st.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (store must not be consulted)", st.listCalls)
	}
	if len(st.added) != 0 || len(st.deleted) != 0 {
		t.Errorf("store touched (added=%v deleted=%v), want none", st.added, st.deleted)
	}
}

// ---------------------------------------------------------------------------
// SetupCommand shape
// ---------------------------------------------------------------------------

func TestSetupCommandShape(t *testing.T) {
	h := newTestGimmick(&fakeStore{}, &fakeGate{})
	cmd := h.SetupCommand()

	if cmd.Name != "gimmick" {
		t.Errorf("Name = %q, want gimmick", cmd.Name)
	}
	if cmd.Type != discordgo.ChatApplicationCommand {
		t.Errorf("Type = %v, want ChatApplicationCommand", cmd.Type)
	}
	if cmd.Description != "Manage the gimmick word list (derpies filter)" {
		t.Errorf("Description = %q, want the pinned text", cmd.Description)
	}

	names := make([]string, 0, len(cmd.Options))
	for _, o := range cmd.Options {
		names = append(names, o.Name)
	}
	if len(cmd.Options) != 3 ||
		names[0] != "add" || names[1] != "list" || names[2] != "delete" {
		t.Fatalf("subcommand arms = %v, want exactly [add list delete]", names)
	}

	checkSub := func(name, desc string, wantWord bool) *discordgo.ApplicationCommandOption {
		var sub *discordgo.ApplicationCommandOption
		for _, o := range cmd.Options {
			if o.Name == name {
				sub = o
			}
		}
		if sub == nil {
			t.Fatalf("subcommand %q missing", name)
		}
		if sub.Type != discordgo.ApplicationCommandOptionSubCommand {
			t.Errorf("%s Type = %v, want SubCommand", name, sub.Type)
		}
		if sub.Description != desc {
			t.Errorf("%s Description = %q, want %q", name, sub.Description, desc)
		}
		if sub.Name == "list" && len(sub.Options) != 0 {
			t.Errorf("list Options = %v, want none", sub.Options)
		}
		if wantWord {
			if len(sub.Options) != 1 || sub.Options[0] == nil {
				t.Fatalf("%s Options = %v, want exactly one (the word option)", name, sub.Options)
			}
			w := sub.Options[0]
			if w.Type != discordgo.ApplicationCommandOptionString || !w.Required {
				t.Errorf("%s word option = (type %v, required %v), want Required string",
					name, w.Type, w.Required)
			}
			return w
		}
		return nil
	}

	addWord := checkSub("add", "Add a gimmick word", true)
	if addWord != nil {
		if addWord.Name != "word" || addWord.Description != "The respelling to add (e.g. sw1ft)" {
			t.Errorf("add word option = (%q, %q), want (word, The respelling to add (e.g. sw1ft))",
				addWord.Name, addWord.Description)
		}
	}
	delWord := checkSub("delete", "Delete a gimmick word", true)
	if delWord != nil {
		if delWord.Name != "word" || delWord.Description != "The word to delete" {
			t.Errorf("delete word option = (%q, %q), want (word, The word to delete)",
				delWord.Name, delWord.Description)
		}
	}
	checkSub("list", "List all gimmick words", false)
}

// ---------------------------------------------------------------------------
// Gates (guild guard, role gate, subcommand dispatch)
// ---------------------------------------------------------------------------

func TestGuildGuard(t *testing.T) {
	st := &fakeStore{}
	gate := &fakeGate{allowed: true}
	resp := newTestGimmick(st, gate).HandleInteraction(cmdInteraction("", "list", nil))

	if resp.Content != "This command can only be used in a guild" {
		t.Errorf("Content = %q, want the guild guard text", resp.Content)
	}
	requireEphemeral(t, resp)
	if gate.memberCalls != 0 || gate.roleCalls != 0 {
		t.Errorf("gate consulted (memberCalls=%d roleCalls=%d), want 0 (the guild guard is before the gate)",
			gate.memberCalls, gate.roleCalls)
	}
	requireStoreUntouched(t, st)
}

func TestRoleGateDenied(t *testing.T) {
	st := &fakeStore{}
	gate := &fakeGate{
		member: &discordgo.Member{User: &discordgo.User{ID: "user1"}},
		// allowed = false
	}
	resp := newTestGimmick(st, gate).HandleInteraction(cmdInteraction("guild1", "list", nil))

	if resp.Content != "Error: You need Highly Regarded or admin role to use this command" {
		t.Errorf("Content = %q, want the role gate text", resp.Content)
	}
	requireEphemeral(t, resp)
	if gate.memberCalls != 1 || gate.roleCalls != 1 {
		t.Errorf("gate calls = (%d member, %d role), want (1, 1)", gate.memberCalls, gate.roleCalls)
	}
	requireStoreUntouched(t, st)
}

func TestMemberFetchFailure(t *testing.T) {
	st := &fakeStore{}
	gate := &fakeGate{memberErr: errors.New("fetch failed")}
	resp := newTestGimmick(st, gate).HandleInteraction(cmdInteraction("guild1", "list", nil))

	if resp.Content != "Error: Could not verify your permissions" {
		t.Errorf("Content = %q, want the member-fetch failure text", resp.Content)
	}
	requireEphemeral(t, resp)
	if gate.roleCalls != 0 {
		t.Errorf("roleCalls = %d, want 0 (the role check is after a successful fetch)", gate.roleCalls)
	}
	requireStoreUntouched(t, st)
}

func TestGateAllowedProceeds(t *testing.T) {
	st := &fakeStore{addN: 1}
	gate := allowedGate()
	resp := newTestGimmick(st, gate).HandleInteraction(cmdInteraction("guild1", "add", "sw1ft"))

	if len(st.added) != 1 {
		t.Fatalf("added = %v, want exactly one (the store must be consulted past the gate)", st.added)
	}
	if resp.Content != "Added sw1ft to the gimmick list" {
		t.Errorf("Content = %q, want the add success text", resp.Content)
	}
	requireEphemeral(t, resp)
}

func TestUnknownSubcommand(t *testing.T) {
	st := &fakeStore{rows: []gimmickRow{{Word: "swift", Source: "seed"}}}

	// An unknown subcommand name (the gate passed).
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "frobnicate", nil))
	if resp.Content != "Unknown subcommand" {
		t.Errorf("Content = %q, want Unknown subcommand", resp.Content)
	}
	requireEphemeral(t, resp)

	// And the missing-subcommand arm (no options at all).
	i := &discordgo.Interaction{GuildID: "guild1",
		Member: &discordgo.Member{User: &discordgo.User{ID: "user1"}},
		Data:   discordgo.ApplicationCommandInteractionData{Name: "gimmick"}}
	resp = newTestGimmick(st, allowedGate()).HandleInteraction(i)
	if resp.Content != "Unknown subcommand" {
		t.Errorf("Content = %q, want Unknown subcommand for a missing subcommand option", resp.Content)
	}
	requireEphemeral(t, resp)

	requireStoreUntouched(t, st)
}

func TestAddMissingWordOption(t *testing.T) {
	st := &fakeStore{}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "add", nil))
	if resp.Content != "Missing required option: word" {
		t.Errorf("Content = %q, want Missing required option: word", resp.Content)
	}
	requireEphemeral(t, resp)
	requireStoreUntouched(t, st)
}

func TestDeleteMissingWordOption(t *testing.T) {
	st := &fakeStore{}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "delete", nil))
	if resp.Content != "Missing required option: word" {
		t.Errorf("Content = %q, want Missing required option: word", resp.Content)
	}
	requireEphemeral(t, resp)
	requireStoreUntouched(t, st)
}

// ---------------------------------------------------------------------------
// Normalization (FoldToASCII; NO punctuation trim)
// ---------------------------------------------------------------------------

func TestAddNormalizesNoTrim(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		// Punctuation survives the fold — it is NOT trimmed (the
		// stored-word contract is ^[a-z0-9]{2,32}$, so the token is
		// invalid and reported as the folded form).
		{"SwiftDot", "Swift.", `Invalid word "swift.": 2-32 lowercase letters or digits only`},
		// Diacritics fold away; digits/letters survive.
		{"S-W1FT", "ŚW1FT", "Added sw1ft to the gimmick list"},
		// The SPACE survives the fold — it is not punctuation to trim.
		{"SwSpace1ft", "sw 1ft", `Invalid word "sw 1ft": 2-32 lowercase letters or digits only`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{addN: 1}
			resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "add", tc.input))
			if resp.Content != tc.expect {
				t.Errorf("Content = %q, want %q", resp.Content, tc.expect)
			}
			requireEphemeral(t, resp)
		})
	}

	// The accepted arm records the FOLDED word with source manual.
	st := &fakeStore{addN: 1}
	newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "add", "ŚW1FT"))
	if len(st.added) != 1 || st.added[0] != "sw1ft|manual" {
		t.Errorf("added = %v, want exactly [sw1ft|manual]", st.added)
	}
}

func TestAddAlreadyExists(t *testing.T) {
	st := &fakeStore{addN: 0} // ON CONFLICT DO NOTHING -> 0 rows
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "add", "sw1ft"))

	if resp.Content != "sw1ft is already in the list" {
		t.Errorf("Content = %q, want the idempotent-arm text", resp.Content)
	}
	requireEphemeral(t, resp)
	if len(st.added) != 1 {
		t.Errorf("added = %v, want exactly one call (idempotent: no re-insert)", st.added)
	}
}

func TestAddDBError(t *testing.T) {
	st := &fakeStore{addErr: errors.New("pg down")}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "add", "sw1ft"))

	if resp.Content != "Failed to add sw1ft" {
		t.Errorf("Content = %q, want Failed to add sw1ft", resp.Content)
	}
	requireEphemeral(t, resp)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDeleteFound(t *testing.T) {
	st := &fakeStore{deleteN: 1}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "delete", "sw1ft"))

	if resp.Content != "Deleted sw1ft from the gimmick list" {
		t.Errorf("Content = %q, want the delete success text", resp.Content)
	}
	requireEphemeral(t, resp)
	if len(st.deleted) != 1 || st.deleted[0] != "sw1ft" {
		t.Errorf("deleted = %v, want exactly [sw1ft]", st.deleted)
	}
}

func TestDeleteAbsent(t *testing.T) {
	st := &fakeStore{deleteN: 0}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "delete", "sw1ft"))

	if resp.Content != "sw1ft is not in the list" {
		t.Errorf("Content = %q, want the absent-arm text", resp.Content)
	}
	requireEphemeral(t, resp)
}

func TestDeleteDBError(t *testing.T) {
	st := &fakeStore{deleteErr: errors.New("pg down")}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "delete", "SW1FT"))

	if resp.Content != "Failed to delete sw1ft" {
		t.Errorf("Content = %q, want Failed to delete sw1ft (the folded word)", resp.Content)
	}
	requireEphemeral(t, resp)
	if len(st.deleted) != 1 || st.deleted[0] != "sw1ft" {
		t.Errorf("deleted = %v, want [sw1ft] (the FOLDED word is what the store sees)", st.deleted)
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestListThreeWords(t *testing.T) {
	st := &fakeStore{rows: []gimmickRow{
		{Word: "swift", Source: "seed"},
		{Word: "sw1ft", Source: "manual"},
		{Word: "zswift", Source: "llm"},
	}}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "list", nil))

	want := "Gimmick words (3): \nswift (seed)\nsw1ft (manual)\nzswift (llm)"
	if resp.Content != want {
		t.Errorf("Content = %q, want %q", resp.Content, want)
	}
	requireEphemeral(t, resp)
	if len(resp.Chunks) != 1 {
		t.Fatalf("len(Chunks) = %d, want 1 (a small list is one chunk)", len(resp.Chunks))
	}
	if resp.Content != resp.Chunks[0] {
		t.Errorf("Content != Chunks[0] (the delivery contract: content mirrors chunk 0)")
	}
}

func TestListEmpty(t *testing.T) {
	st := &fakeStore{}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "list", nil))

	if resp.Content != "No gimmick words yet." {
		t.Errorf("Content = %q, want the empty-list text", resp.Content)
	}
	requireEphemeral(t, resp)
	if resp.Chunks != nil {
		t.Errorf("Chunks = %v, want nil (no chunks on the empty arm)", resp.Chunks)
	}
}

func TestListDBError(t *testing.T) {
	st := &fakeStore{listErr: errors.New("pg down")}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "list", nil))

	if resp.Content != "Failed to query gimmick words. Please try again later." {
		t.Errorf("Content = %q, want the DB-error text (single message, no chunks)", resp.Content)
	}
	requireEphemeral(t, resp)
	if resp.Chunks != nil {
		t.Errorf("Chunks = %v, want nil", resp.Chunks)
	}
}

// TestListChunkingSplits pins the deterministic boundary of the
// greedy ≤2000-char packing: 60 scripted rows, each a 32-char
// "aaaa…NN" word + " (llm)" = a 38-char line. Chunk 1 = 20-char
// first header + 50 lines (20 + 1 + 50×38 + 49 = 1970 ≤ 2000 —
// the 51st line would reach 2009 → flush); chunk 2 = 27-char
// continuation header + 10 lines (27 + 1 + 10×38 + 9 = 417).
func TestListChunkingSplits(t *testing.T) {
	const n = 60
	rows := make([]gimmickRow, 0, n)
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		w := strings.Repeat("a", 30) + (string(rune('0'+i/10)) + string(rune('0'+i%10)))
		rows = append(rows, gimmickRow{Word: w, Source: "llm"})
		lines = append(lines, w+" (llm)")
	}
	st := &fakeStore{rows: rows}
	resp := newTestGimmick(st, allowedGate()).HandleInteraction(cmdInteraction("guild1", "list", nil))

	if len(resp.Chunks) != 2 {
		t.Fatalf("len(Chunks) = %d, want 2 (the pinned 50/10 split at the ≤2000 boundary)", len(resp.Chunks))
	}
	if resp.Content != resp.Chunks[0] {
		t.Errorf("Content != Chunks[0] (the delivery contract)")
	}
	if !strings.HasPrefix(resp.Chunks[0], "Gimmick words (60): ") {
		t.Errorf("Chunks[0] = %q..., want the 20-char first header Gimmick words (60): ", trunc(resp.Chunks[0]))
	}
	if !strings.HasPrefix(resp.Chunks[1], "Gimmick words (continued): ") {
		t.Errorf("Chunks[1] = %q..., want the continuation header Gimmick words (continued): ", trunc(resp.Chunks[1]))
	}

	// Whole-line integrity: each chunk's post-header text splits on
	// '\n' into complete, un-mangled lines, in order.
	headers := []string{"Gimmick words (60): ", "Gimmick words (continued): "}
	var all []string
	for i, c := range resp.Chunks {
		if len(c) > 2000 {
			t.Errorf("len(Chunks[%d]) = %d, want <= 2000", i, len(c))
		}
		body := strings.TrimPrefix(c, headers[i])
		body = strings.TrimPrefix(body, "\n")
		all = append(all, strings.Split(body, "\n")...)
	}
	if len(all) != n {
		t.Fatalf("total line count across chunks = %d, want %d", len(all), n)
	}
	for i := range all {
		if all[i] != lines[i] {
			t.Fatalf("line %d = %q, want %q (a line was mangled)", i, all[i], lines[i])
		}
	}
}

// trunc is the display helper for long asserts.
func trunc(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------------------------------------------------------------------------
// poolStore PG integration (the only raw-SQL surface of this PR; the
// fake arms above pin the command surface, this pins the SQL's
// ordering / idempotency / source-preservation properties). Mirrors
// the derpies package's integration helper: skip on testing.Short or a
// failed connect (TUGBOT_TEST_DATABASE_URL, compose default).
// ---------------------------------------------------------------------------

func testGimmickDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: PG not guaranteed available (testing.Short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := os.Getenv("TUGBOT_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@127.0.0.1:5432/tugbot_test"
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("cannot create pool: %v (is the compose PG running?)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot reach PG: %v (is the compose PG running?)", err)
	}
	t.Cleanup(pool.Close)
	// Reset the shared table, mirroring migration 000002's shape
	// EXACTLY: serial id PK, varchar(64) NOT NULL word with the UNIQUE
	// constraint the ON CONFLICT relies on, varchar(8) DEFAULT 'seed'
	// NOT NULL source, timestamp-without-time-zone default-now
	// created_at.
	if _, err := pool.Exec(ctx, `
		DROP TABLE IF EXISTS derpies_gimmicks CASCADE;
		CREATE TABLE derpies_gimmicks (
			id serial PRIMARY KEY,
			word character varying(64) NOT NULL,
			source character varying(8) DEFAULT 'seed' NOT NULL,
			created_at timestamp without time zone DEFAULT now() NOT NULL,
			CONSTRAINT derpies_gimmicks_word_key UNIQUE (word)
		);`); err != nil {
		t.Fatalf("reset derpies_gimmicks: %v", err)
	}
	return pool
}

func TestPoolStore(t *testing.T) {
	pool := testGimmickDB(t)
	st := &poolStore{pool: pool}
	ctx := context.Background()

	// 1. list — ORDER BY word. (Inserted directly so the source
	//    values are controlled.)
	if _, err := pool.Exec(ctx, `INSERT INTO derpies_gimmicks (word, source)
		VALUES ('swift', 'seed'), ('sw1ft', 'manual'), ('zswift', 'llm')`); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	rows, err := st.listGimmicks(ctx)
	if err != nil {
		t.Fatalf("listGimmicks: %v", err)
	}
	want := []gimmickRow{{Word: "sw1ft", Source: "manual"},
		{Word: "swift", Source: "seed"}, {Word: "zswift", Source: "llm"}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v (ORDER BY word)", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("rows[%d] = %+v, want %+v (ORDER BY word)", i, rows[i], want[i])
		}
	}

	// 2. add — idempotency + source preservation (the ADR property:
	//    ON CONFLICT DO NOTHING must NEVER rewrite an existing row's
	//    source — a seed row stays seed after a manual add attempt).
	if n, err := st.addGimmick(ctx, "swift", "manual"); err != nil || n != 0 {
		t.Fatalf("addGimmick(swift, manual) = (%d, %v), want (0, nil) (existing word)", n, err)
	}
	rows, err = st.listGimmicks(ctx)
	if err != nil {
		t.Fatalf("listGimmicks after add: %v", err)
	}
	for _, r := range rows {
		if r.Word == "swift" && r.Source != "seed" {
			t.Fatalf("swift source = %q after a manual add attempt, want seed (source preservation)", r.Source)
		}
	}
	if n, err := st.addGimmick(ctx, "newword", "manual"); err != nil || n != 1 {
		t.Fatalf("addGimmick(newword, manual) = (%d, %v), want (1, nil) (new word)", n, err)
	}
	rows, err = st.listGimmicks(ctx)
	if err != nil {
		t.Fatalf("listGimmicks after newword add: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Word == "newword" {
			found = r.Source == "manual"
		}
	}
	if !found {
		t.Fatalf("newword missing or source != manual in %+v", rows)
	}

	// 3. delete — found / re-delete / absent.
	for _, tc := range []struct {
		word string
		want int64
	}{
		{"newword", 1},
		{"newword", 0},
		{"absentword", 0},
	} {
		if n, err := st.deleteGimmick(ctx, tc.word); err != nil || n != tc.want {
			t.Errorf("deleteGimmick(%s) = (%d, %v), want (%d, nil)", tc.word, n, err, tc.want)
		}
	}
	rows, err = st.listGimmicks(ctx)
	if err != nil {
		t.Fatalf("final listGimmicks: %v", err)
	}
	for _, r := range rows {
		if r.Word == "newword" || r.Word == "absentword" {
			t.Errorf("%q still present in %+v after delete", r.Word, rows)
		}
	}
}
