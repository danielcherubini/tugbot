// Package gulag holds the canonical Gulag core of the Go bot.
//
// Task 4 establishes every Gulag helper that Rust keeps in
// src/handlers/gulag/mod.rs (plus the usage/db helpers from src/db/mod.rs)
// HERE, once: Task 5's mention auto-gulag path and Task 6's gulag package
// (vote / reaction / commands / loops) CALL INTO these functions and are
// forbidden from re-declaring them. Files added by Task 6 (vote.go,
// reaction.go, commands.go, loops.go) must reference this core, not
// replace it. New(Gulag) is defined exactly once, in this file.
//
// Two-stage gulag-duration conversion (port exactly — this is the
// tricky part):
//  1. GulagDurationForOffense(count) is the RAW exponential value
//     (1800 * 2^count, saturating; the multiplier saturates at the 64-bit
//     max for count >= 32 where 2^count would overflow). The returned
//     value may exceed int32 — that is fine at this layer.
//  2. Every DB write of a gulag length converts through an explicit int32
//     check WITH the 30-day fallback, 2_592_000 seconds, on overflow
//     (Rust `try_into().unwrap_or(2_592_000u32)` in goku_poll / ai_slop,
//     applied BEFORE the write): that is DurationToGulagLength. Separately,
//     AddToGulag performs a CHECKED u32 -> int32 conversion and ERRORS on
//     overflow (Rust checked try_into), defensively, since the callers
//     already clamp.
package gulag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/db"
)

// GulagChannelName is the auto-gulag channel (the Rust hardcodes
// "the-gulag" at every call site: goku_poll.rs, ai_slop.rs, mod.rs).
const GulagChannelName = "the-gulag"

// MaxGulagLengthSeconds is the 30-day fallback used when a computed
// duration overflows (Rust: `try_into().unwrap_or(2_592_000u32)`).
const MaxGulagLengthSeconds int32 = 2_592_000

// Gulag is the canonical core, built from the shared App (Rust: the
// serenity Http + db-pool pair, injected through the type map).
type Gulag struct {
	d    *discordgo.Session
	pool *pgxpool.Pool

	// reactionUserFetcher is the test seam for the reaction handler's
	// manual get_reaction_users pagination (added by Task 6; when nil the
	// handler uses Session.MessageReactions — see reaction.go).
	reactionUserFetcher reactionUserFetcherFunc

	// Test seams (mirroring the reactionUserFetcher convention): when set
	// they substitute for the concrete surfaces; when nil, the Session /
	// pool paths run unchanged (production).
	// serverLookup substitutes for the pgxpool SelectServerByGuildID
	// (get_server_by_guild_id, source/db/mod.rs).
	serverLookup func(ctx context.Context, guildID int64) (*db.Server, bool, error)
	// discordSurface substitutes for the *discordgo.Session REST calls
	// (FindChannel / FindRole / MemberHasAnyRole / AddToGulag).
	discordSurface DiscordSurface
	// db substitutes for the pgxpool queries inside the DB-backed free
	// functions (usage upserts).
	db QueryExec
}

// New builds the Gulag core from the shared *app.App. Task 6 reuses this
// constructor; it lives in the core and is not re-registered anywhere
// else. Production always runs the concrete Session + pool paths (nil
// seams).
func New(app *app.App) *Gulag {
	return &Gulag{d: app.D, pool: app.Pool}
}

// NewWithSeams builds a *Gulag over injected test surfaces (the dev
// constructor, mirroring the reactionUserFetcher seam convention): each
// argument may be nil, in which case the corresponding concrete surface is
// used. A nil surface for a call the flow actually reaches is a test
// wiring bug, not a production path.
func NewWithSeams(surface DiscordSurface, db QueryExec, serverLookup func(ctx context.Context, guildID int64) (*db.Server, bool, error)) *Gulag {
	return &Gulag{discordSurface: surface, db: db, serverLookup: serverLookup}
}

// DiscordSurface is the Discord REST surface the core talks to (the
// *discordgo.Session implements it; tests inject fakes, e.g. the mention
// auto-gulag tests). The variadic RequestOption positions mirror the
// vendor signatures.
type DiscordSurface interface {
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
	GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error
}

// QueryExec is the DB surface the core's DB-backed methods talk to (the
// *pgxpool.Pool implements it; tests inject in-memory fakes). The subset
// is exactly the calls the core makes (select / update / insert + the
// usage upserts).
type QueryExec interface {
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// discord returns the Discord surface: the injected seam when set, else
// the concrete session (production, and Task 6's real tests).
func (g *Gulag) discord() DiscordSurface {
	if g.discordSurface != nil {
		return g.discordSurface
	}
	return g.d
}

// query returns the DB executor: the injected seam when set, else the
// concrete pool.
func (g *Gulag) query() QueryExec {
	if g.db != nil {
		return g.db
	}
	return g.pool
}

// GulagParams mirrors Rust's GulagParams (mod.rs:29-36). Discord IDs are
// strings in the pinned discordgo v0.29.0 API and are converted to int64
// with checked conversions at every DB boundary (no silent truncation).
// GulagLength lives in the u32 domain and is checked to int32 before the DB
// write.
type GulagParams struct {
	GuildID     string
	UserID      string
	GulagRoleID string
	GulagLength uint32
	ChannelID   string
	MessageID   string
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

// GulagDurationForOffense mirrors get_gulag_duration_for_offense
// (mod.rs:37-55): base 1800s (30 minutes) x 2^count with saturating
// arithmetic; count >= 32 caps the multiplier at the 64-bit max (2^count
// would overflow the u64 domain). It returns the raw exponential value,
// which may exceed int32 — the DB-write path converts it explicitly.
func GulagDurationForOffense(count int) int64 {
	var multiplier int64
	if count >= 32 {
		multiplier = math.MaxInt64 // Rust: `_ => u64::MAX`
	} else {
		multiplier = 1 << uint(count)
	}
	// Rust: base_seconds.saturating_mul(multiplier).
	if multiplier > math.MaxInt64/1800 {
		return math.MaxInt64
	}
	return 1800 * multiplier
}

// DurationToGulagLength is the CALLER-side conversion applied before every
// DB write (Rust `duration_seconds.try_into().unwrap_or(2_592_000u32)` in
// goku_poll.rs / ai_slop.rs): a value that does not fit a checked u32 ->
// int32 is replaced by the 30-day fallback. Offense (21)'s raw value
// exceeds int32, so this — not the raw 64-bit value — is what the DB
// write carries.
func DurationToGulagLength(seconds int64) uint32 {
	if seconds < 0 || seconds > math.MaxInt32 {
		return uint32(MaxGulagLengthSeconds)
	}
	return uint32(seconds)
}

// CheckedGulagLengthToSeconds is the checked u32 -> int32 conversion Rust
// performs INSIDE add_to_gulag (mod.rs:138-141): it ERRORS on overflow,
// unlike the caller-side clamp above.
func CheckedGulagLengthToSeconds(length uint32) (int32, error) {
	if length > math.MaxInt32 {
		return 0, fmt.Errorf("Gulag length %d exceeds i32::MAX", length)
	}
	return int32(length), nil
}

// OffenseSecondsForCount mirrors the conversion of a fresh DB usage count
// (ai_slop.rs:98-110: `new_count.saturating_sub(1).try_into()` and then
// get_gulag_duration_for_offense). The saturating subtract clamps a zero
// count to offense 0; the try_into i32 -> u32 error arm is unreachable
// after the clamp but is preserved (countTooHigh, defensive).
func OffenseSecondsForCount(newCount int64) (int64, error) {
	offense := newCount - 1
	if offense < 0 {
		offense = 0 // Rust: new_count.saturating_sub(1)
	}
	return GulagDurationForOffense(int(offense)), nil
}

// NextTotalSecondsForCount mirrors Rust's `new_count.try_into()` arm used
// for the "next offense" message (ai_slop.rs:179-185): the conversion
// only fails on counts that overflow u32, falling back to the 30-day cap.
func NextTotalSecondsForCount(newCount int64) int64 {
	if newCount < 0 || newCount > 4_294_967_295 { // u32::MAX
		return 2_592_000
	}
	return GulagDurationForOffense(int(newCount))
}

// IncrementTotalSecondsForCount mirrors goku_poll.rs:114-120
// (`new_count.saturating_add(1).try_into()` with the 30-day fallback).
func IncrementTotalSecondsForCount(newCount int64) int64 {
	next := newCount + 1
	if newCount == math.MaxInt64 {
		next = math.MaxInt64 // Rust: saturating_add(1)
	}
	if next < 0 || next > 4_294_967_295 { // u32::MAX
		return 2_592_000
	}
	return GulagDurationForOffense(int(next))
}

// FormatDuration mirrors format_duration (mod.rs:57-69): "Xh Ym" when
// h > 0 (including zero minutes), else "Xm", else "Xs". NOTE: this is the
// GULAG formatter, distinct from mention's format_remaining (Task 5); both
// keep their Rust names so a reader can map back.
func FormatDuration(seconds int64) string {
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", secs)
}

// ComputeNewGulagTime mirrors the existing-row branch of add_to_gulag
// (mod.rs:148-164) plus the db-layer validations of src/db/mod.rs
// add_time_to_gulag: the checked addition ("Gulag length overflow: {} + {}")
// and the non-negative check ("gulag_duration must be non-negative").
func ComputeNewGulagTime(existingLength int32, releaseAt time.Time, addedLength int32) (int32, time.Time, error) {
	if addedLength < 0 {
		return 0, time.Time{}, errors.New("gulag_duration must be non-negative")
	}
	if existingLength > 0 && addedLength > math.MaxInt32-existingLength {
		return 0, time.Time{}, fmt.Errorf("Gulag length overflow: %d + %d", existingLength, addedLength)
	}
	return existingLength + addedLength, releaseAt.Add(time.Duration(addedLength) * time.Second), nil
}

// NewGulagUserReleaseAt mirrors src/db/mod.rs send_to_gulag:
// release_at = created_at + gulag_length * 1s (computed before insert).
func NewGulagUserReleaseAt(created time.Time, length int32) time.Time {
	return created.Add(time.Duration(length) * time.Second)
}

// IsDiscordNotFound mirrors is_discord_not_found (mod.rs:87-100): true
// when the error chain contains a Discord 404 (Unknown Guild / Unknown
// Message). The pinned discordgo v0.29.0 surfaces REST failures as
// *discordgo.RESTError (no separate ResponseError shape in this release),
// so errors.As unwraps to it and inspects the inner *http.Response status
// code. NEVER string-matches err.Error().
func IsDiscordNotFound(err error) bool {
	var re *discordgo.RESTError
	if !errors.As(err, &re) || re == nil || re.Response == nil {
		return false
	}
	return re.Response.StatusCode == http.StatusNotFound
}

// ---------------------------------------------------------------------------
// Discord guild lookups (channel / role scans — NOT the DB; Rust uses
// serenity's guild channel/role fetches the same way)
// ---------------------------------------------------------------------------

// FindChannel mirrors find_channel (mod.rs:80-98): scan by name; a Discord
// error (404 Unknown Guild, rate limit, network) is PROPAGATED so the
// caller can distinguish "channel gone" from "guild gone" and apply
// IsDiscordNotFound cleanup.
func (g *Gulag) FindChannel(ctx context.Context, guildID, channelName string) (*discordgo.Channel, bool, error) {
	channels, err := g.discord().GuildChannels(guildID)
	if err != nil {
		return nil, false, err
	}
	for _, channel := range channels {
		if channel.Name == channelName {
			return channel, true, nil
		}
	}
	return nil, false, nil
}

// FindRole mirrors find_role (mod.rs:70-78): on a lookup error Rust logs
// and returns None.
func (g *Gulag) FindRole(ctx context.Context, guildID, roleName string) *discordgo.Role {
	roles, err := g.discord().GuildRoles(guildID)
	if err != nil {
		slog.Error("failed to get guild roles", "module", "gulag", "error", err)
		return nil
	}
	for _, role := range roles {
		if role.Name == roleName {
			return role
		}
	}
	return nil
}

// FindGulagRole mirrors find_gulag_role (mod.rs:67-68): FindRole for the
// hardcoded "gulag" role name.
func (g *Gulag) FindGulagRole(ctx context.Context, guildID string) *discordgo.Role {
	return g.FindRole(ctx, guildID, "gulag")
}

// MemberHasAnyRole mirrors member_has_any_role (mod.rs:71-89): one
// get_guild_roles call, match by name, then the member's role-id scan. A
// lookup error returns false (Rust: Err(_) => false).
func (g *Gulag) MemberHasAnyRole(ctx context.Context, guildID string, member *discordgo.Member, roleNames ...string) bool {
	roles, err := g.discord().GuildRoles(guildID)
	if err != nil {
		return false
	}
	for _, roleName := range roleNames {
		for _, role := range roles {
			if role.Name != roleName {
				continue
			}
			for _, memberRoleID := range member.Roles {
				if memberRoleID == role.ID {
					return true
				}
			}
		}
	}
	return false
}

// IsTugbot mirrors is_tugbot (mod.rs:99-108): true if the user is this
// bot; nil (Rust None / the get_current_user error arm) when the current
// user is unavailable. Rust calls get_current_user over HTTP; discordgo
// keeps that user on the ready state, the equivalent source.
func (g *Gulag) IsTugbot(user *discordgo.User) *bool {
	current := g.d.State.User
	if current == nil {
		slog.Error("failed to get the current user", "module", "gulag")
		return nil
	}
	is := current.ID == user.ID
	return &is
}

// ---------------------------------------------------------------------------
// Gulag state (DB)
// ---------------------------------------------------------------------------

// IsUserInGulag mirrors is_user_in_gulag (mod.rs:197-245): filters by
// user_id AND in_gulag = TRUE; on ANY error Rust logs and returns None.
func (g *Gulag) IsUserInGulag(ctx context.Context, userID int64) *db.GulagUser {
	row, err := g.selectGulagUserByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		slog.Error("error loading gulag user in is_user_in_gulag", "module", "gulag", "error", err)
		return nil
	}
	return row
}

// AddToGulag mirrors add_to_gulag (mod.rs:110-183): fetch the member
// (context "Failed to get guild member"), add the gulag role (context
// "Failed to add gulag role"), the checked u32 -> int32 of the length
// (ERRORS on overflow), then the existing-row add-time branch (checked
// addition, context "Failed to add time to gulag") or the fresh
// send_to_gulag branch (checked ID conversions, context "Failed to send
// user to gulag").
func (g *Gulag) AddToGulag(ctx context.Context, p GulagParams) (db.GulagUser, error) {
	if _, err := g.discord().GuildMember(p.GuildID, p.UserID); err != nil {
		return db.GulagUser{}, fmt.Errorf("Failed to get guild member: %w", err)
	}
	if err := g.discord().GuildMemberRoleAdd(p.GuildID, p.UserID, p.GulagRoleID); err != nil {
		return db.GulagUser{}, fmt.Errorf("Failed to add gulag role: %w", err)
	}
	length, err := CheckedGulagLengthToSeconds(p.GulagLength)
	if err != nil {
		return db.GulagUser{}, err
	}
	userID, err := DiscordID("user", p.UserID)
	if err != nil {
		return db.GulagUser{}, err
	}

	if gulagUser := g.IsUserInGulag(ctx, userID); gulagUser != nil {
		newLength, newRelease, err := ComputeNewGulagTime(gulagUser.GulagLength, gulagUser.ReleaseAt.Time, length)
		if err != nil {
			return db.GulagUser{}, err
		}
		if err := g.updateGulagUserTime(ctx, gulagUser.ID, newLength, newRelease); err != nil {
			return db.GulagUser{}, fmt.Errorf("Failed to add time to gulag: %w", err)
		}
		gulagUser.GulagLength = newLength
		gulagUser.ReleaseAt = pgtype.Timestamp{Time: newRelease, Valid: true}
		return *gulagUser, nil
	}
	return g.sendToGulag(ctx, p, length, userID)
}

// sendToGulag mirrors the source/db/mod.rs send_to_gulag body (the None
// branch of add_to_gulag, mod.rs:166-182): the checked ID conversions
// (Rust "User ID {} exceeds i64::MAX" et al.), the non-negative length
// check, release_at = now + length * 1s, then the insert (context
// "Failed to send user to gulag").
func (g *Gulag) sendToGulag(ctx context.Context, p GulagParams, length int32, userID int64) (db.GulagUser, error) {
	if length < 0 {
		return db.GulagUser{}, errors.New("gulag_length must be non-negative")
	}
	guildID, err := DiscordID("guild", p.GuildID)
	if err != nil {
		return db.GulagUser{}, err
	}
	roleID, err := DiscordID("role", p.GulagRoleID)
	if err != nil {
		return db.GulagUser{}, err
	}
	channelID, err := DiscordID("channel", p.ChannelID)
	if err != nil {
		return db.GulagUser{}, err
	}
	messageID, err := DiscordID("message", p.MessageID)
	if err != nil {
		return db.GulagUser{}, err
	}

	now := time.Now().UTC()
	row := db.GulagUser{
		UserID:      userID,
		GuildID:     guildID,
		GulagRoleID: roleID,
		ChannelID:   channelID,
		InGulag:     true,
		GulagLength: length,
		CreatedAt:   pgtype.Timestamp{Time: now, Valid: true},
		ReleaseAt:   pgtype.Timestamp{Time: NewGulagUserReleaseAt(now, length), Valid: true},
		MessageID:   messageID,
	}
	id, err := g.insertGulagUser(ctx, row)
	if err != nil {
		return db.GulagUser{}, fmt.Errorf("Failed to send user to gulag: %w", err)
	}
	row.ID = id
	return row, nil
}

// SelectServerByGuildID mirrors source/db/mod.rs get_server_by_guild_id
// (`servers.filter(guild_id.eq(...)).first().optional()`): (nil, false,
// nil) when the guild has no row. The interface-typed pool parameter
// lets callers pass the concrete *pgxpool.Pool (production) or a
// test-injected executor.
func SelectServerByGuildID(ctx context.Context, pool QueryExec, guildID int64) (*db.Server, bool, error) {
	var row db.Server
	err := pool.QueryRow(ctx, `SELECT id, guild_id, gulag_id FROM servers WHERE guild_id = $1`, guildID).
		Scan(&row.ID, &row.GuildID, &row.GulagID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to query server for guild %d: %w", guildID, err)
	}
	return &row, true, nil
}

// SelectServerByGuildID is the *Gulag-routed form (the one the mention
// auto-gulag calls through its injected core): the serverLookup seam
// when set (tests), delegating to the shared SQL form otherwise.
func (g *Gulag) SelectServerByGuildID(ctx context.Context, guildID int64) (*db.Server, bool, error) {
	if g.serverLookup != nil {
		return g.serverLookup(ctx, guildID)
	}
	return SelectServerByGuildID(ctx, g.query(), guildID)
}

// GetOrCreateGulagPollUsage mirrors source/db/mod.rs
// get_or_create_goku_poll_usage: select by (user_id, guild_id); on the
// missing row insert with usage count 0 and return 0.
func GetOrCreateGulagPollUsage(ctx context.Context, pool QueryExec, userID, guildID int64) (int32, error) {
	var count int32
	err := pool.QueryRow(ctx,
		`SELECT usage_count FROM goku_poll_usage WHERE user_id = $1 AND guild_id = $2`, userID, guildID).Scan(&count)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := pool.Exec(ctx,
				`INSERT INTO goku_poll_usage (user_id, guild_id, usage_count, last_goku_at, created_at) VALUES ($1, $2, 0, now(), now())`,
				userID, guildID); err != nil {
				return 0, fmt.Errorf("failed to insert goku_poll_usage usage: %w", err)
			}
			return 0, nil
		}
		return 0, fmt.Errorf("failed to query goku_poll_usage usage: %w", err)
	}
	return count, nil
}

// IncrementGulagPollUsage mirrors source/db/mod.rs atomic_increment_goku_poll:
// atomic upsert (insert with 1 or increment) returning the NEW count.
func IncrementGulagPollUsage(ctx context.Context, pool QueryExec, userID, guildID int64) (int32, error) {
	var count int32
	err := pool.QueryRow(ctx,
		`INSERT INTO goku_poll_usage (user_id, guild_id, usage_count, last_goku_at, created_at)
		 VALUES ($1, $2, 1, now(), now())
		 ON CONFLICT (user_id, guild_id) DO UPDATE
		 SET usage_count = goku_poll_usage.usage_count + 1, last_goku_at = now()
		 RETURNING usage_count`, userID, guildID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to atomic increment goku_poll_usage usage: %w", err)
	}
	return count, nil
}

// IncrementAiSlopUsage mirrors source/db/mod.rs atomic_increment_ai_slop:
// atomic upsert (insert with 1 or increment) returning the NEW count.
func IncrementAiSlopUsage(ctx context.Context, pool QueryExec, userID, guildID int64) (int32, error) {
	var count int32
	err := pool.QueryRow(ctx,
		`INSERT INTO ai_slop_usage (user_id, guild_id, usage_count, last_slop_at, created_at)
		 VALUES ($1, $2, 1, now(), now())
		 ON CONFLICT (user_id, guild_id) DO UPDATE
		 SET usage_count = ai_slop_usage.usage_count + 1, last_slop_at = now()
		 RETURNING usage_count`, userID, guildID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to atomic increment ai_slop_usage usage: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Checked conversion
// ---------------------------------------------------------------------------

// DiscordID converts a discordgo string ID to the int64 DB boundary with
// the checking discipline of Rust's `i64::try_from` + with_context
// ("User ID {} exceeds i64::MAX" etc.): a Go strconv parse is the
// checked conversion — it refuses empty, non-numeric, or out-of-range
// IDs instead of silently truncating.
func DiscordID(name, raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s ID is empty", name)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s ID %q is not a valid i64 Discord ID", name, raw)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Raw DB calls (the SQL is byte-identical to the Task 1 sqlc files:
// select_gulag_user_by_user, update_gulag_user_time, insert_gulag_user)
// ---------------------------------------------------------------------------

func (g *Gulag) selectGulagUserByUser(ctx context.Context, userID int64) (*db.GulagUser, error) {
	var row db.GulagUser
	err := g.query().QueryRow(ctx,
		`SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
		        gulag_length, created_at, release_at, message_id
		 FROM gulag_users WHERE user_id = $1 AND in_gulag`, userID).
		Scan(&row.ID, &row.UserID, &row.GuildID, &row.GulagRoleID, &row.ChannelID,
			&row.InGulag, &row.GulagLength, &row.CreatedAt, &row.ReleaseAt, &row.MessageID)
	return &row, err
}

func (g *Gulag) updateGulagUserTime(ctx context.Context, id int32, newLength int32, releaseAt time.Time) error {
	_, err := g.query().Exec(ctx, `UPDATE gulag_users SET gulag_length = $1, release_at = $2 WHERE id = $3`,
		newLength, releaseAt, id)
	if err != nil {
		return fmt.Errorf("failed to update gulag user time: %w", err)
	}
	return nil
}

func (g *Gulag) insertGulagUser(ctx context.Context, row db.GulagUser) (int32, error) {
	var id int32
	err := g.query().QueryRow(ctx,
		`INSERT INTO gulag_users (user_id, guild_id, gulag_role_id, channel_id, in_gulag, gulag_length, created_at, release_at, message_id)
		 VALUES ($1, $2, $3, $4, true, $5, $6, $7, $8)
		 RETURNING id`,
		row.UserID, row.GuildID, row.GulagRoleID, row.ChannelID, row.GulagLength,
		row.CreatedAt, row.ReleaseAt, row.MessageID).Scan(&id)
	return id, err
}
