package gulag

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestGulagDurationForOffense pins the exact Rust table for
// get_gulag_duration_for_offense (src/handlers/gulag/mod.rs:37-55):
// 1800 * 2^count with saturating arithmetic; count >= 32 caps the
// multiplier at the 64-bit max (which saturates- (as u64::MAX) into the
// 64-bit max).
func TestGulagDurationForOffense(t *testing.T) {
	tests := []struct {
		count int
		want  int64
	}{
		{0, 1800},
		{1, 3600},
		{10, 1_843_200},
		{20, 1_887_436_800}, // just under int32 max
		{21, 3_774_873_600}, // exceeds int32 max (DB-write path clamps; see below)
		{31, 3_865_470_566_400},
		{32, math.MaxInt64}, // multiplier caps at the 64-bit max
		{100, math.MaxInt64},
	}
	for _, tt := range tests {
		if got := GulagDurationForOffense(tt.count); got != tt.want {
			t.Errorf("GulagDurationForOffense(%d) = %d, want %d", tt.count, got, tt.want)
		}
	}
}

// TestDurationToGulagLength pins the CALLER-side conversion (Rust
// `duration_seconds.try_into().unwrap_or(2_592_000u32)` in goku_poll.rs /
// ai_slop.rs before the DB write): an int64 that exceeds int32 falls back
// to the 30-day cap. (21)'s raw duration exceeds int32, so this — not the
// raw int64 — is what the DB write carries.
func TestDurationToGulagLength(t *testing.T) {
	// (21) raw value from the table above, clamped.
	if got := DurationToGulagLength(GulagDurationForOffense(21)); got != 2_592_000 {
		t.Errorf("DurationToGulagLength(21-offense) = %d, want 2_592_000 (30-day fallback)", got)
	}
	// (20) fits in int32 and passes through untouched.
	if got := DurationToGulagLength(GulagDurationForOffense(20)); got != 1_887_436_800 {
		t.Errorf("DurationToGulagLength(20-offense) = %d, want 1_887_436_800", got)
	}
	if got := DurationToGulagLength(2_592_000); got != 2_592_000 {
		t.Errorf("DurationToGulagLength(2_592_000) = %d, want 2_592_000", got)
	}
	if got := DurationToGulagLength(math.MaxInt64); got != 2_592_000 {
		t.Errorf("DurationToGulagLength(64-bit max, offense >= 32) = %d, want 2_592_000", got)
	}
}

// TestCheckedGulagLengthToSeconds pins the checked u32->int32 conversion in
// add_to_gulag (mod.rs:139-145): it ERRORS on overflow (unlike the
// caller-side clamp above).
func TestCheckedGulagLengthToSeconds(t *testing.T) {
	if got, err := CheckedGulagLengthToSeconds(2_592_000); err != nil || got != 2_592_000 {
		t.Errorf("CheckedGulagLengthToSeconds(2_592_000) = %d, %v; want 2_592_000, nil", got, err)
	}
	if _, err := CheckedGulagLengthToSeconds(2_147_483_648); err == nil {
		t.Error("CheckedGulagLengthToSeconds(2_147_483_648) (above int32 max): want error, got nil")
	} else if !strings.Contains(err.Error(), "Gulag length 2147483648 exceeds i32::MAX") {
		// Rust mod.rs with_context (capital-first, parity)
		t.Errorf("overflow error %q, want Rust-cased \"Gulag length 2147483648 exceeds i32::MAX\"", err.Error())
	}
}

// TestSendToGulagInsertFailureCasing pins the INSERT-failure context of
// add_to_gulag (Rust mod.rs:277 with_context "Failed to send user to
// gulag" — NO "the", matching the Rust casing exactly): the exact
// Error() text when the insert fails. This is user-visible on the
// ai_slop insert-failure path ("Error: Failed to send to gulag: {err}").
func TestSendToGulagInsertFailureCasing(t *testing.T) {
	g := NewWithSeams(plainGoodSurface{}, &failingRowDB{rowErr: errors.New("boom")}, nil)
	_, err := g.AddToGulag(context.Background(), GulagParams{
		GuildID: "10", UserID: "11", GulagRoleID: "12",
		GulagLength: 300, ChannelID: "13", MessageID: "14",
	})
	const want = `Failed to send user to gulag: boom`
	if err == nil || err.Error() != want {
		t.Fatalf("insert-failure err = %v, want exact text %q", err, want)
	}
}

// plainGoodSurface is a Discord surface where the member fetch and role
// add always succeed, so AddToGulag reaches the DB path.
type plainGoodSurface struct{}

func (plainGoodSurface) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	return nil, nil
}
func (plainGoodSurface) GuildRoles(string, ...discordgo.RequestOption) ([]*discordgo.Role, error) {
	return nil, nil
}
func (plainGoodSurface) GuildMember(string, string, ...discordgo.RequestOption) (*discordgo.Member, error) {
	return &discordgo.Member{}, nil
}
func (plainGoodSurface) GuildMemberRoleAdd(string, string, string, ...discordgo.RequestOption) error {
	return nil
}

// failingRowDB is a QueryExec whose QueryRow rows always error (the fail-all
// shape; the select treats the non-NoRows error as "not in gulag" and the
// insert surfaces the error).
type failingRowDB struct{ rowErr error }

func (d *failingRowDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{err: d.rowErr}
}
func (d *failingRowDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

func containsSub(err error, sub string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), strings.ToLower(sub))
}

// TestFormatDuration pins format_duration (mod.rs:57-69): "Xh Ym" when
// hours > 0 (including zero minutes), else "Xm", else "Xs".
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{0, "0s"},
		{30, "30s"},
		{90, "1m"},
		{59, "59s"},
		{900, "15m"},
		{3599, "59m"},
		{1800, "30m"},
		{3600, "1h 0m"},
		{3690, "1h 1m"},
		{9900, "2h 45m"},
		{11820, "3h 17m"},
		{2_592_000, "720h 0m"},
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.seconds); got != tt.want {
			t.Errorf("FormatDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

// TestComputeNewGulagTime ports the add_to_gulag existing-row branch
// (mod.rs:150-170, with the db-layer validations from src/db/mod.rs
// add_time_to_gulag / send_to_gulag): checked add overflow + non-negative
// checks.
func TestComputeNewGulagTime(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		existing   int32
		added      int32
		wantLength int32
		wantErrSub string
	}{
		{"normal adds up", 1_800, 3_600, 5_400, ""},
		{"zero addition", 1_800, 0, 1_800, ""},
		{
			"int32 overflow errors",
			2_147_000_000, 600_000, 0,
			"Gulag length overflow", // Rust (capital-first): "Gulag length overflow: {} + {}"
		},
		{
			"negative addition rejected",
			1_800, -1, 0,
			"gulag_duration must be non-negative", // Rust db-layer validation
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newLen, newRelease, err := ComputeNewGulagTime(tt.existing, base, tt.added)
			if tt.wantErrSub != "" {
				if err == nil || !containsSub(err, tt.wantErrSub) {
					t.Fatalf("want error containing %q, got %v", tt.wantErrSub, err)
				}
				if tt.name == "int32 overflow errors" {
					// Rust mod.rs with_context (capital-first, parity)
					if !strings.Contains(err.Error(), "Gulag length overflow: 2147000000 + 600000") {
						t.Fatalf("overflow error %q, want Rust-cased \"Gulag length overflow: 2147000000 + 600000\"", err.Error())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if newLen != tt.wantLength {
				t.Errorf("new length = %d, want %d", newLen, tt.wantLength)
			}
			wantRelease := base.Add(time.Duration(tt.added) * time.Second)
			if !newRelease.Equal(wantRelease) {
				t.Errorf("new release = %v, want %v", newRelease, wantRelease)
			}
		})
	}
}

// TestIsDiscordNotFound ports is_discord_not_found (mod.rs:87-100) for the
// vendored discordgo v0.29.0: the REST error is *discordgo.RESTError with
// a *http.Response — 404 status (Unknown Guild / Unknown Message) is the
// stale-state cleanup signal. errors.As must see through wrapping.
func TestIsDiscordNotFound(t *testing.T) {
	if IsDiscordNotFound(errors.New("plain error")) {
		t.Error("plain error: want false")
	}
	if IsDiscordNotFound(nil) {
		t.Error("nil: want false")
	}
	notFound := &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
	}
	if !IsDiscordNotFound(notFound) {
		t.Error("raw 404 RESTError: want true")
	}
	if !IsDiscordNotFound(errors.Join(errors.New("failed to get message"), notFound)) {
		t.Error("wrapped 404 RESTError: want true")
	}
	notAllowed := &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusForbidden},
	}
	if IsDiscordNotFound(notAllowed) {
		t.Error("403 RESTError: want false")
	}
	noResponse := &discordgo.RESTError{} // Response nil (defensive)
	if IsDiscordNotFound(noResponse) {
		t.Error("RESTError without response: want false")
	}
}

// TestNewGulagUserRow pins the send_to_gulag (fresh-row) time math:
// release_at = created_at + gulag_length * 1s (src/db/mod.rs send_to_gulag).
func TestNewGulagUserReleaseAt(t *testing.T) {
	created := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	release := NewGulagUserReleaseAt(created, 1_800)
	want := created.Add(1_800 * time.Second)
	if !release.Equal(want) {
		t.Errorf("release = %v, want %v", release, want)
	}
}
