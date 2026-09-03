// Tests for the Go port of src/handlers/cull.rs. The pure logic
// (days validation, the 180-day scan cutoff boundary, the candidate
// pipeline, formatting) is factored into unexported helpers and tested
// here; the command registration shape and the SQL constants pinned by
// the Rust source are ported 1:1 and pinned to the Rust literals. See
// docs/parity/checklist.md (the cull section).
package cull

import (
	"github.com/bwmarrin/discordgo"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielcherubini/tugbot/internal/app"
)

// TestConstants pins the port's constants against the Rust source
// (cull.rs:21-26) — the same test trio Rust has (test_max_kicks_constant /
// test_kick_delay_constant / test_cat_herding_channel_id).
func TestConstants(t *testing.T) {
	if MaxKicks != 50 {
		t.Errorf("MaxKicks = %d, want 50", MaxKicks)
	}
	if KickDelayMs != 1500 {
		t.Errorf("KickDelayMs = %d, want 1500", KickDelayMs)
	}
	if CatHerdChannelID != "1224402885786472659" {
		t.Errorf("CatHerdChannelID = %q, want 1224402885786472659", CatHerdChannelID)
	}
}

// TestWhitelistRoles pins the role allowlist in Rust order
// (cull.rs:24-26).
func TestWhitelistRoles(t *testing.T) {
	roles := whitelistRoles()
	if len(roles) != 2 || roles[0] != "Highly Regarded" || roles[1] != "admin" {
		t.Errorf("whitelistRoles() = %v, want [Highly Regarded admin]", roles)
	}
}

// TestWorstCaseKickWindow mirrors Rust's
// test_execute_mode_max_kicks_would_block: the unit test (NOT a
// runtime guard) proving MAX_KICKS * KICK_DELAY_MS = 75s exceeds
// Discord's 3s response window, which is why the kick loop is spawned
// as a background task. There is NO 24h check in Rust to port.
func TestWorstCaseKickWindow(t *testing.T) {
	const discordResponseWindowMs = 3000
	worstCaseMs := MaxKicks * KickDelayMs
	if worstCaseMs <= discordResponseWindowMs {
		t.Fatalf("worst case %dms must exceed Discord's %dms window — the kick loop must be spawned", worstCaseMs, discordResponseWindowMs)
	}
	if worstCaseMs != 75000 {
		t.Errorf("MaxKicks * KickDelayMs = %d, want 75000", worstCaseMs)
	}
}

// TestDefaultDays pins the option default (cull.rs:210 unwrap_or(30)).
func TestDefaultDays(t *testing.T) {
	if DefaultDays != 30 {
		t.Errorf("DefaultDays = %d, want 30", DefaultDays)
	}
}

// TestValidateDays pins days validation: 1..=365 (Rust `days <= 0 ||
// days > 365`).
func TestValidateDays(t *testing.T) {
	cases := []struct {
		days int
		want bool
	}{
		{1, true},
		{30, true},
		{365, true},
		{0, false},
		{366, false},
		{-5, false},
	}
	for _, c := range cases {
		if got := validateDays(c.days); got != c.want {
			t.Errorf("validateDays(%d) = %v, want %v", c.days, got, c.want)
		}
	}
}

// TestFormatTimestamp pins the Go equivalence of Rust's format_timestamp
// (the Hinnant days-since-epoch arithmetic is NOT ported —
// time.Time.Format("2006-01-02") is provably equivalent on the same day
// grid Rust's test table uses; cull.rs tests `test_format_timestamp_*`).
func TestFormatTimestamp(t *testing.T) {
	epoch := time.Unix(0, 0)
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"epoch", epoch, "1970-01-01"},
		{"y2k", epoch.AddDate(0, 0, 10957), "2000-01-01"},
		{"2024-01-15", epoch.AddDate(0, 0, 19737), "2024-01-15"},
		{"feb_leap", epoch.AddDate(0, 0, 19782), "2024-02-29"},
		{"2025-06-15", epoch.AddDate(0, 0, 20254), "2025-06-15"},
		{"pre_epoch", epoch.Add(-100 * time.Second), "unknown"},
	}
	for _, c := range cases {
		if got := formatTimestamp(c.ts); got != c.want {
			t.Errorf("%s: formatTimestamp = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestScanCutoff pins the 180-day cutoff computed from the constant
// `180 * 86400` (the "(90 days)" source comment is stale — the constant
// is what is ported).
func TestScanCutoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := scanCutoff(now)
	want := now.Add(-180 * 86400 * time.Second)
	if !cutoff.Equal(want) {
		t.Errorf("scanCutoff = %v, want %v", cutoff, want)
	}
}

// TestScanStopBoundary pins the stop condition on message age relative to
// the cutoff: 179 days (newer than cutoff) is scanned, exactly 180 days
// (== cutoff, the strict-less-than keeps it) is scanned, 181 days
// (older than cutoff) stops the channel.
func TestScanStopBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := scanCutoff(now)
	for _, days := range []int{179, 180, 181} {
		oldest := now.Add(-time.Duration(days) * 24 * time.Hour)
		want := days >= 181
		if got := shouldStopScan(cutoff, oldest); got != want {
			t.Errorf("shouldStopScan at %dd old = %v, want %v", days, got, want)
		}
	}
}

// TestPipeline pins the candidate pipeline order: SORT by user ID
// (determinism), then dedupe, then truncate at MAX_KICKS (Rust
// `candidates.sort(); candidates.dedup(); candidates.truncate`).
func TestPipeline(t *testing.T) {
	// 60 distinct + duplicates of early entries.
	in := make([]int64, 0, 61)
	for i := int64(100); i < 100+int64(60); i++ {
		in = append(in, i)
	}
	in = append(in, 150, 100, 100) // duplicates
	got := pipeline(in)

	if len(got) != MaxKicks {
		t.Fatalf("pipeline len = %d, want %d (truncate at MAX_KICKS)", len(got), MaxKicks)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("pipeline not sorted/deduped at %d: %d >= %d", i, got[i-1], got[i])
		}
	}
	if got[0] != 100 || got[49] != 149 {
		t.Errorf("pipeline[0..49] = %d..%d, want 100..119 (sorted, deduped, truncated)", got[0], got[49])
	}
}

// TestPipelineNoOp pins a smaller set passing through unmodified in
// order.
func TestPipelineSmall(t *testing.T) {
	got := pipeline([]int64{42, 7, 42})
	if len(got) != 2 || got[0] != 7 || got[1] != 42 {
		t.Errorf("pipeline(small) = %v, want [7 42]", got)
	}
}

// TestInactiveCutoff pins the inactive query cutoff = now - days·86400s
// (Rust `query_inactive_users(pool, guild, days)`).
func TestInactiveCutoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := inactiveCutoff(now, 30)
	if !got.Equal(now.Add(-30 * 86400 * time.Second)) {
		t.Errorf("inactiveCutoff = %v, want 30 days before now", got)
	}
}

// TestCullStartedResponse mirrors Rust's
// test_execute_mode_response_starts_with_cull_started: execute mode
// returns the "Cull started" prefix (not "Cull complete" — the
// non-blocking pattern) and the cat-herding result-post suffix.
func TestCullStartedResponse(t *testing.T) {
	const candidateCount = 10
	msg := cullStartedResponse(candidateCount)
	if want := "Cull started: 10 candidates."; !strings.HasPrefix(msg, want) {
		t.Errorf("cullStartedResponse does not start with %q: %q", want, msg)
	}
	if want := "Results will be posted to <#" + CatHerdChannelID + ">"; !strings.Contains(msg, want) {
		t.Errorf("cullStartedResponse does not contain %q: %q", want, msg)
	}
}

// TestSetupCommandPins pins the slash registration shape field-by-field
// (cull.rs:27-58).
func TestSetupCommandShape(t *testing.T) {
	c := New(&app.App{})
	cmd := c.SetupCommand()
	if cmd.Type != discordgo.ChatApplicationCommand {
		t.Errorf("Type = %v, want ChatApplicationCommand", cmd.Type)
	}
	if cmd.Name != "cull" {
		t.Errorf("Name = %q, want cull", cmd.Name)
	}
	if cmd.Description != "Cull inactive members from the server" {
		t.Errorf("Description = %q, want 'Cull inactive members from the server'", cmd.Description)
	}
	wantOpts := []struct {
		name string
		typ  discordgo.ApplicationCommandOptionType
		desc string
		req  bool
	}{
		{"days", discordgo.ApplicationCommandOptionInteger, "Inactivity threshold in days (default: 30)", false},
		{"dry-run", discordgo.ApplicationCommandOptionBoolean, "Preview candidates without kicking (default: false)", false},
		{"include-never-posted", discordgo.ApplicationCommandOptionBoolean, "Include users who have never posted (default: false)", false},
		{"scan", discordgo.ApplicationCommandOptionBoolean, "Seed activity data from message history (one-time setup)", false},
	}
	if len(cmd.Options) != len(wantOpts) {
		t.Fatalf("len(Options) = %d, want %d", len(cmd.Options), len(wantOpts))
	}
	for i, w := range wantOpts {
		o := cmd.Options[i]
		if o.Name != w.name || o.Type != w.typ || o.Description != w.desc || o.Required != w.req {
			t.Errorf("option %d = {Name:%q Type:%v Desc:%q Req:%v}, want {Name:%q Type:%v Desc:%q Req:%v}",
				i, o.Name, o.Type, o.Description, o.Required, w.name, w.typ, w.desc, w.req)
		}
	}
}

// TestGuildMemberHasPermissionBaseFromEveryone pins Serenity parity for
// the @everyone base: Discord's @everyone role has ID == the guild ID
// (never "1"), so a guild that grants KICK_MEMBERS ONLY through the
// @everyone role (legal, if pathological) grants the permission to every
// member — Serenity's `guild.member_permissions` starts the base from
// the guild's @everyone role regardless of the member's role list.
func TestGuildMemberHasPermissionBaseFromEveryone(t *testing.T) {
	const guildID = "123456789012345678"
	guild := &discordgo.Guild{
		ID: guildID,
		Roles: []*discordgo.Role{
			{ID: guildID, Name: "@everyone", Permissions: discordgo.PermissionKickMembers},
		},
	}
	member := &discordgo.Member{Roles: []string{}}
	if got := guildMemberHasPermission(guild, member, discordgo.PermissionKickMembers); !got {
		t.Errorf("guildMemberHasPermission with KICK via @everyone base = %v, want true", got)
	}
}

// TestGuildMemberHasPermissionHigherRole pins that a non-@everyone role
// (higher position) still ORs its permission bits into the base —
// a guild that grants KICK_MEMBERS through a regular role grants it.
func TestGuildMemberHasPermissionHigherRole(t *testing.T) {
	const guildID = "123456789012345678"
	guild := &discordgo.Guild{
		ID: guildID,
		Roles: []*discordgo.Role{
			{ID: guildID, Name: "@everyone", Position: 0, Permissions: 0},
			{ID: "999", Name: "mod", Position: 5, Permissions: discordgo.PermissionKickMembers},
		},
	}
	member := &discordgo.Member{Roles: []string{guildID, "999"}}
	if got := guildMemberHasPermission(guild, member, discordgo.PermissionKickMembers); !got {
		t.Errorf("guildMemberHasPermission with KICK via higher role = %v, want true", got)
	}
}

// TestGuildMemberHasPermissionAbsent pins the negative arm: no role
// grants KICK, so the gate fails (fail-closed).
func TestGuildMemberHasPermissionAbsent(t *testing.T) {
	guild := &discordgo.Guild{
		ID: "123456789012345678",
		Roles: []*discordgo.Role{
			{ID: "123456789012345678", Name: "@everyone", Permissions: 0},
		},
	}
	member := &discordgo.Member{Roles: []string{}}
	if got := guildMemberHasPermission(guild, member, discordgo.PermissionKickMembers); got {
		t.Errorf("guildMemberHasPermission with no KICK-granting role = %v, want false", got)
	}
}

// TestUpsertSQLShape pins the committed single-pair .sql (sqlc v1 can't
// resolve array params inside UNNEST, so the scan seeds per pair, as
// the committed .sql header documents) — the `GREATEST(existing, new)`
// anti-regression is the ported guarantee from Rust's
// `do_update().set(last_message_at = GREATEST(...))`.
func TestUpsertSQLShape(t *testing.T) {
	path := filepath.Join("..", "..", "db", "queries", "upsert_user_activity.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read upsert_user_activity.sql: %v", err)
	}
	s := string(b)
	for _, needle := range []string{
		"GREATEST(user_activity.last_message_at, now())",
		"ON CONFLICT (user_id, guild_id) DO UPDATE",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("upsert_user_activity.sql missing %q", needle)
		}
	}
}
