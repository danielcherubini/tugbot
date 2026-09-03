// Package cull is the Go port of the Rust bot's src/handlers/cull.rs:
// the slash command that kicks inactive members, with a dry-run mode,
// a one-time message-history seed ("scan"), and a non-blocking kick
// loop (the worst-case window MAX_KICKS × KICK_DELAY_MS = 75s exceeds
// Discord's 3s response window — pinned by a unit test, not a runtime
// guard; see the cull section of docs/parity/checklist.md).
//
// All cull responses use the defer response path (Rust
// `defer_response: Some(true)` on every return).
package cull

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danielcherubini/tugbot/internal/app"
	"github.com/danielcherubini/tugbot/internal/features"
	"github.com/danielcherubini/tugbot/internal/handlers/gulag"
)

// FeatureKey is the features-table key this command is gated on (Rust:
// `Features::check_enabled(&pool, "cull")`).
const FeatureKey = "cull"

// Ported constants (cull.rs:21-26). The scan cutoff constant is `180 *
// 86400` seconds — the adjacent "(90 days)" source comment is stale and
// wrong; the constant is what is ported, not the comment.
const (
	// CatHerdChannelID — moderator-only output channel for cull/scan
	// results.
	CatHerdChannelID = "1224402885786472659"
	// MaxKicks — hard cap on kicks per invocation.
	MaxKicks = 50
	// KickDelayMs — sleep between kicks to respect rate limits (1.5s).
	KickDelayMs = 1500
	// DefaultDays — `days` option default.
	DefaultDays = 30
)

// whitelistRoles — users with these roles are never culled
// (cull.rs:24-26, in Rust order).
func whitelistRoles() []string { return []string{"Highly Regarded", "admin"} }

// scanDaysSeconds is the 180-day scan cutoff (the Rust constant
// `180 * 86400` seconds).
const scanDaysSeconds = 180 * 86400

// interPageDelayMs — the 200 ms delay between scan pages.
const interPageDelayMs = 200

// kickLoopTimeout bounds the background kick loop (75s worst case +
// headroom; a stuck loop must not outlive the process).
const kickLoopTimeout = 10 * time.Minute

// scanTimeout bounds the background scan (a full history walk; the
// 200 ms inter-page delay dominates).
const scanTimeout = 15 * time.Minute

// Response is the module's share of Rust's HandlerResponse shape — every
// cull response defers (Rust `defer_response: Some(true)` on every
// return; no cull response sets components).
type Response struct {
	Content       string
	Ephemeral     bool
	DeferResponse bool
}

// store is the module's share of the SQL surface: raw SQL over the
// App pool (the committed-handler pattern — the Task-1 sqlc methods are
// unexported, and no harvested .sql is created for this module). The
// statements are byte-identical to the committed .sql files they mirror.
type store struct {
	pool *pgxpool.Pool
}

// selectInactiveUserIDs mirrors the committed
// `select_inactive_user_ids` .sql (Rust query_inactive_users).
func (s *store) selectInactiveUserIDs(ctx context.Context, guildID int64, cutoff time.Time) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id FROM user_activity WHERE guild_id = $1 AND last_message_at < $2`,
		guildID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// selectTrackedUserIDs mirrors the committed `select_tracked_user_ids`
// .sql (Rust query_all_tracked_user_ids_for_guild).
func (s *store) selectTrackedUserIDs(ctx context.Context, guildID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id FROM user_activity WHERE guild_id = $1`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// selectUserActivityForIDs mirrors the committed
// `select_user_activity_for_ids` .sql (Rust query_user_activity_for_ids):
// the dry-run batched timestamp fetch.
func (s *store) selectUserActivityForIDs(ctx context.Context, guildID int64, userIDs []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, last_message_at FROM user_activity WHERE guild_id = $1 AND user_id = ANY($2)`,
		guildID, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ts time.Time
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// upsertUserActivity mirrors the committed `upsert_user_activity` .sql
// (Rust bulk_upsert_activity, per-pair here — see the scan section
// note): the GREATEST anti-regression is byte-identical.
func (s *store) upsertUserActivity(ctx context.Context, userID, guildID int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_activity (user_id, guild_id, last_message_at, created_at) VALUES ($1, $2, now(), now())
		 ON CONFLICT (user_id, guild_id) DO UPDATE SET last_message_at = GREATEST(user_activity.last_message_at, now())`,
		userID, guildID)
	return err
}

func gateResponse(content string) Response {
	return Response{Content: content, Ephemeral: true, DeferResponse: true}
}

// Cull handles the "cull" slash command.
type Cull struct {
	app *app.App
	g   *gulag.Gulag
}

// New builds the handler (consumes the canonical gulag core — never
// re-declares any of its helpers).
func New(app *app.App) *Cull {
	return &Cull{app: app, g: gulag.New(app)}
}

// SetupCommand mirrors `CullHandler::setup_command` (cull.rs:27-58)
// field-by-field: name, description, the four OPTIONAL options with
// byte-identical names/descriptions.
func (h *Cull) SetupCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "cull",
		Description: "Cull inactive members from the server",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "days",
				Description: "Inactivity threshold in days (default: 30)",
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "dry-run",
				Description: "Preview candidates without kicking (default: false)",
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "include-never-posted",
				Description: "Include users who have never posted (default: false)",
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "scan",
				Description: "Seed activity data from message history (one-time setup)",
			},
		},
	}
}

// validateDays mirrors Rust's `days <= 0 || days > 365` check.
func validateDays(days int) bool {
	return days >= 1 && days <= 365
}

// inactiveCutoff mirrors Rust's query_inactive_users cutoff = now −
// days·86400s.
func inactiveCutoff(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * 86400 * time.Second)
}

// scanCutoff mirrors Rust: `SystemTime::now() - 180 * 86400` (the
// constant — the adjacent "(90 days)" source comment is stale).
func scanCutoff(now time.Time) time.Time {
	return now.Add(-scanDaysSeconds * time.Second)
}

// shouldStopScan mirrors the per-page stop condition: messages are
// returned newest-first; a page whose oldest message is STRICTLY older
// than the cutoff stops (so a message exactly 180 days old — equal to
// the cutoff — is still processed).
func shouldStopScan(cutoff, oldest time.Time) bool {
	return oldest.Before(cutoff)
}

// pipeline mirrors Rust's candidate pipeline ordering:
// `candidates.sort(); candidates.dedup(); candidates.truncate(MAX_KICKS)`.
func pipeline(ids []int64) []int64 {
	out := make([]int64, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > 1 {
		last := 1
		for i := 1; i < len(out); i++ {
			if out[i] != out[i-1] {
				out[last] = out[i]
				last++
			}
		}
		out = out[:last]
	}
	if len(out) > MaxKicks {
		out = out[:MaxKicks]
	}
	return out
}

// formatTimestamp mirrors Rust's format_timestamp. A time.Time is a
// civil date by construction, so the Hinnant days-since-epoch
// arithmetic is NOT ported; Go's "2006-01-02" layout is equivalent, and
// pre-epoch input maps to "unknown" (Rust's Err arm — a negative
// duration_since epoch).
func formatTimestamp(ts time.Time) string {
	if ts.IsZero() || ts.Before(time.Unix(0, 0)) {
		return "unknown"
	}
	return ts.Format("2006-01-02")
}

// cullStartedResponse pins the execute-mode reply (Rust test
// test_execute_mode_response_starts_with_cull_started: a "Cull started"
// prefix — the non-blocking pattern — with the cat-herding suffix).
func cullStartedResponse(total int) string {
	return "Cull started: " + strconv.Itoa(total) + " candidates. Results will be posted to <#" + CatHerdChannelID + ">."
}

// commandOption finds an option by name (Rust's `options.iter().find`).
func commandOption(i *discordgo.Interaction, name string) *discordgo.ApplicationCommandInteractionDataOption {
	data, ok := i.Data.(*discordgo.ApplicationCommandInteractionData)
	if !ok {
		return nil
	}
	for _, o := range data.Options {
		if o != nil && o.Name == name {
			return o
		}
	}
	return nil
}

// HandleInteraction mirrors `CullHandler::setup_interaction`
// (cull.rs:60-455) 1:1 in gate order (a→m). Rust's entry point never
// returns an error — every failure path becomes a defer response (or
// the one non-ephemeral empty-candidates arm, which still defers).
func (h *Cull) HandleInteraction(i *discordgo.Interaction) Response {
	ctx := context.Background()
	d := h.app.D
	st := store{pool: h.app.Pool}

	// a. Feature flag check (the PROPAGATING flavor — a missing row is
	// Ok(false) → the disabled arm on a fresh DB).
	enabled, err := features.CheckEnabled(ctx, h.app.Pool, FeatureKey)
	switch {
	case err != nil:
		return gateResponse("Failed to check cull feature: " + err.Error())
	case !enabled:
		return gateResponse("Cull feature is currently disabled")
	}

	// b. Guild check.
	guildID := i.GuildID
	if guildID == "" {
		return gateResponse("This command can only be used in a guild")
	}

	// c. Permission check (Highly Regarded or admin).
	invokerID := ""
	if i.Member != nil && i.Member.User != nil {
		invokerID = i.Member.User.ID
	} else if i.User != nil {
		invokerID = i.User.ID
	}
	member, err := d.GuildMember(guildID, invokerID)
	if err != nil {
		return gateResponse("Error: Could not verify your permissions")
	}
	if !h.g.MemberHasAnyRole(ctx, guildID, member, whitelistRoles()...) {
		return gateResponse("Error: You need Highly Regarded or admin role to use this command")
	}

	// d. Bot KICK_MEMBERS permission check.
	botID, err := h.botUserID()
	if err != nil {
		return gateResponse("Failed to get bot info: " + err.Error())
	}
	botMember, err := d.GuildMember(guildID, botID)
	if err != nil {
		return gateResponse("Failed to get bot member: " + err.Error())
	}
	guild, err := d.Guild(guildID)
	if err != nil {
		return gateResponse("Failed to get guild info: " + err.Error())
	}
	if !guildMemberHasPermission(guild, botMember, discordgo.PermissionKickMembers) {
		return gateResponse("I don't have KICK_MEMBERS permission on this server.")
	}

	// e. Parse options. A missing option or a non-matching value type
	// falls back to the default (Rust `.and_then(match value ...)
	// .unwrap_or(30/false)` — a present-but-wrong-typed value is
	// likewise the default).
	days := DefaultDays
	if o := commandOption(i, "days"); o != nil {
		if v, ok := o.Value.(int); ok {
			days = v
		}
	}
	if !validateDays(days) {
		return gateResponse("Days must be between 1 and 365")
	}
	dryRun := optionBool(i, "dry-run")
	includeNeverPosted := optionBool(i, "include-never-posted")
	doScan := optionBool(i, "scan")

	// e1. Scan mode — seed activity data from message history (a
	// background task; the response returns immediately for the 3s
	// window).
	if doScan {
		return h.runScan(i)
	}

	// f. Fetch member list via REST pagination (1000-page walk, `after`
	// cursor = the last member id, until a short page).
	var allMembers []*discordgo.Member
	var afterID string
	for {
		members, err := d.GuildMembers(guildID, afterID, 1000)
		if err != nil {
			postToCatHerd(d, "Error fetching members: "+err.Error())
			return gateResponse("Failed to fetch members: " + err.Error())
		}
		allMembers = append(allMembers, members...)
		if len(members) < 1000 {
			break
		}
		last := members[len(members)-1]
		if last.User == nil {
			break
		}
		afterID = last.User.ID
	}

	// g. Fetch whitelist roles (fail-closed: abort if they can't
	// resolve).
	roles, err := d.GuildRoles(guildID)
	if err != nil {
		return gateResponse("Failed to resolve whitelist roles: " + err.Error())
	}
	whitelistRoleIDs := map[string]bool{}
	for _, role := range roles {
		for _, name := range whitelistRoles() {
			if role.Name == name {
				whitelistRoleIDs[role.ID] = true
			}
		}
	}
	filtered := make([]*discordgo.Member, 0, len(allMembers))
	for _, m := range allMembers {
		if m.User == nil || m.User.Bot {
			continue
		}
		if memberHasAnyRoleIDs(m, whitelistRoleIDs) {
			continue
		}
		filtered = append(filtered, m)
	}

	// h. Filter out gulaged users.
	nonGulaged := make([]*discordgo.Member, 0, len(filtered))
	for _, m := range filtered {
		uid, err := strconv.ParseInt(m.User.ID, 10, 64)
		if err != nil {
			continue
		}
		if h.g.IsUserInGulag(ctx, uid) == nil {
			nonGulaged = append(nonGulaged, m)
		}
	}

	// i. Query inactive users (last_message_at < now − days).
	inactive, err := st.selectInactiveUserIDs(ctx, parseID(guildID), inactiveCutoff(time.Now(), days))
	if err != nil {
		postToCatHerd(d, "Error querying inactive users: "+err.Error())
		return gateResponse("Failed to query inactive users: " + err.Error())
	}
	inactiveSet := map[int64]bool{}
	for _, id := range inactive {
		inactiveSet[id] = true
	}

	// j. Build the candidate list.
	var candidates []int64
	for _, m := range nonGulaged {
		uid, err := strconv.ParseInt(m.User.ID, 10, 64)
		if err != nil {
			continue
		}
		if inactiveSet[uid] {
			candidates = append(candidates, uid)
		}
	}
	// Include never-posted users if requested (a failed query is only
	// posted to cat-herding — not an abort).
	if includeNeverPosted {
		tracked, err := st.selectTrackedUserIDs(ctx, parseID(guildID))
		if err != nil {
			postToCatHerd(d, "Failed to query tracked users for never-posted check")
		} else {
			trackedSet := map[int64]bool{}
			for _, id := range tracked {
				trackedSet[id] = true
			}
			for _, m := range nonGulaged {
				uid, err := strconv.ParseInt(m.User.ID, 10, 64)
				if err != nil {
					continue
				}
				if !trackedSet[uid] {
					candidates = append(candidates, uid)
				}
			}
		}
	}

	// Deduplicate, sort by user ID for determinism, cap at MAX_KICKS.
	candidates = pipeline(candidates)

	if len(candidates) == 0 {
		neverPosted := "no"
		if includeNeverPosted {
			neverPosted = "yes"
		}
		postToCatHerd(d, "No candidates found (inactive "+strconv.Itoa(days)+"+ days, never posted: "+neverPosted+")")
		// The one NON-ephemeral cull response (ephemeral: false),
		// still deferred.
		return Response{Content: "No candidates found.", Ephemeral: false, DeferResponse: true}
	}

	// l. Dry-run mode.
	if dryRun {
		activityMap, err := st.selectUserActivityForIDs(ctx, parseID(guildID), candidates)
		if err != nil {
			postToCatHerd(d, "Error querying activity: "+err.Error())
			return gateResponse("Failed to query activity: " + err.Error())
		}

		// Build candidate lines (max 25).
		displayCount := len(candidates)
		if displayCount > 25 {
			displayCount = 25
		}
		lines := make([]string, 0, displayCount)
		for _, uid := range candidates[:displayCount] {
			date := "never posted"
			if ts, ok := activityMap[uid]; ok {
				date = formatTimestamp(ts)
			}
			lines = append(lines, "<@"+strconv.FormatInt(uid, 10)+"> (last active: "+date+")")
		}
		candidateBlock := ""
		for idx, line := range lines {
			if idx > 0 {
				candidateBlock += "\n"
			}
			candidateBlock += line
		}
		if extra := len(candidates) - 25; extra > 0 {
			candidateBlock += "\nand " + strconv.Itoa(extra) + " more..."
		}
		neverPosted := "no"
		if includeNeverPosted {
			neverPosted = "yes"
		}
		message := "**Cull Dry-Run** (inactive " + strconv.Itoa(days) + "+ days, never posted: " + neverPosted + ")\n\n" + candidateBlock + "\n\nTotal candidates: " + strconv.Itoa(len(candidates)) + " (capped at " + strconv.Itoa(MaxKicks) + ")\nRun `/cull --days " + strconv.Itoa(days) + "` to execute."
		if postToCatHerd(d, message) {
			return gateResponse("Dry-run posted to <#" + CatHerdChannelID + ">")
		}
		return gateResponse("Failed to post to <#" + CatHerdChannelID + ">. Dry-run results:\n\n" + message)
	}

	// m. Execute mode — spawn the kick loop as a background task so we
	// return immediately and don't exceed the 3s response window
	// (worst case MAX_KICKS * KICK_DELAY_MS = 75s >> 3s).
	userName := ""
	if i.User != nil {
		userName = i.User.Username
	}
	go h.kickLoop(guildID, candidates, days, userName)
	return gateResponse(cullStartedResponse(len(candidates)))
}

// botUserID mirrors Rust's `http.get_current_user()`.
func (h *Cull) botUserID() (string, error) {
	u, err := h.app.D.User("@me")
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// guildMemberHasPermission computes guild-level permissions the way
// serenity's `guild.member_permissions` does: start at the @everyone
// role's base, then apply each of the member's roles in ascending
// position order — `(base & ~deny) | allow` for the role's overwrites
// (plain role permission bits OR in). NOTE: the vendored v0.29.0
// Guild object does not carry role_overwrites (they are a separate
// REST endpoint), so the documented overwrites arm degrades to the
// plain role-permission union here — for a KICK_MEMBERS gate that is
// the standard model without overwrites.
func guildMemberHasPermission(guild *discordgo.Guild, member *discordgo.Member, perm int64) bool {
	var base int64
	memberRoleSet := map[string]bool{}
	for _, id := range member.Roles {
		memberRoleSet[id] = true
	}
	others := make([]*discordgo.Role, 0, len(guild.Roles))
	for _, r := range guild.Roles {
		if r.ID == guild.ID { // @everyone — its ID is the guild ID, never "1"
			base = r.Permissions
			continue
		}
		others = append(others, r)
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Position < others[j].Position })
	for _, r := range others {
		if memberRoleSet[r.ID] {
			base |= r.Permissions
		}
	}
	return base&perm == perm
}

// memberHasAnyRoleIDs mirrors the Rust helper: does the member hold
// any of the resolved role IDs.
func memberHasAnyRoleIDs(member *discordgo.Member, roleIDs map[string]bool) bool {
	for _, id := range member.Roles {
		if roleIDs[id] {
			return true
		}
	}
	return false
}

// optionBool mirrors Rust's boolean-option extraction (no option, or a
// non-boolean value → false).
func optionBool(i *discordgo.Interaction, name string) bool {
	o := commandOption(i, name)
	if o == nil {
		return false
	}
	v, ok := o.Value.(bool)
	return ok && v
}

// postToCatHerd mirrors Rust's post_to_cat_herding: send to the fixed
// channel; log + false on failure (callers treat false as "relay the
// inline results in the response").
func postToCatHerd(d *discordgo.Session, content string) bool {
	if _, err := d.ChannelMessageSend(CatHerdChannelID, content); err != nil {
		slog.Error("[cull] Failed to post to cat-herding: "+err.Error(), "module", "cull")
		return false
	}
	return true
}

// runScan mirrors Rust's `run_scan`: spawn the seed as a background
// task and return immediately (the 3s window).
func (h *Cull) runScan(i *discordgo.Interaction) Response {
	userName := ""
	if i.User != nil {
		userName = i.User.Username
	}
	go h.runScanLoop(i.GuildID, userName)
	return gateResponse("Scan started. Results will be posted to <#" + CatHerdChannelID + ">.")
}

// runScanLoop is the background body of run_scan (the Rust spawned
// task): paginates backwards through every text channel's history,
// dedupes the (user, guild) pairs, then seeds user_activity via the
// per-pair upsert (GREATEST anti-regression).
func (h *Cull) runScanLoop(guildID, username string) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	d := h.app.D

	channels, err := d.GuildChannels(guildID)
	if err != nil {
		slog.Error("[cull] Scan: failed to get channels for guild "+guildID+": "+err.Error(), "module", "cull")
		postToCatHerd(d, "Scan failed: could not fetch channels: "+err.Error())
		return
	}
	var textChannels []*discordgo.Channel
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText {
			textChannels = append(textChannels, ch)
		}
	}

	guildIDInt, _ := strconv.ParseInt(guildID, 10, 64)
	cutoff := scanCutoff(time.Now())
	var allUserIDs []int64
	seen := map[int64]bool{}
	totalMsgCount := 0

	for idx, channel := range textChannels {
		var beforeID string
		channelMsgCount := 0
		for {
			messages, err := d.ChannelMessages(channel.ID, 100, beforeID, "", "")
			// d.ChannelMessages(channelID, limit, beforeID, afterID, aroundID):
			// newest-first with the before cursor.
			if err != nil {
				slog.Error("[cull] Scan: failed to get messages from channel "+channel.Name+": "+err.Error(), "module", "cull")
				break
			}
			if len(messages) == 0 {
				break
			}
			// Process this page FIRST (it may contain valid messages
			// even if the oldest crosses the cutoff).
			for _, msg := range messages {
				if msg.Author != nil && !msg.Author.Bot && msg.WebhookID == "" {
					if uid, err := strconv.ParseInt(msg.Author.ID, 10, 64); err == nil && !seen[uid] {
						seen[uid] = true
						allUserIDs = append(allUserIDs, uid)
					}
				}
				// The counter counts ALL messages in the page (bots /
				// webhooks included in the count).
				channelMsgCount++
			}
			// Stop conditions checked AFTER processing the page.
			if len(messages) < 100 {
				break
			}
			if shouldStopScan(cutoff, messages[len(messages)-1].Timestamp) {
				break
			}
			beforeID = messages[len(messages)-1].ID
			select {
			case <-ctx.Done():
				return
			case <-time.After(interPageDelayMs * time.Millisecond):
			}
		}
		totalMsgCount += channelMsgCount
		if ctx.Err() != nil {
			return
		}
		slog.Info("[cull] Scan: "+strconv.Itoa(idx+1)+"/"+strconv.Itoa(len(textChannels))+" channels ("+strconv.Itoa(channelMsgCount)+" msgs in "+channel.Name+"), "+strconv.Itoa(len(allUserIDs))+" unique users so far", "module", "cull")
	}

	// Seed user_activity — per-pair upsert (the committed single-pair
	// sqlc query; the GREATEST anti-regression is byte-identical to
	// Rust's do_update().set(GREATEST(...))).
	st := store{pool: h.app.Pool}
	upserted := 0
	for _, uid := range allUserIDs {
		if err := st.upsertUserActivity(ctx, uid, guildIDInt); err != nil {
			slog.Error("[cull] Scan: failed to upsert activity for user "+strconv.FormatInt(uid, 10)+": "+err.Error(), "module", "cull")
			continue
		}
		upserted++
	}

	// Scan-complete report (Rust: the Ok(rows) / empty-branch arms —
	// posted to cat-herding; an upsert failure is logged-and-continue
	// and the report go on regardless, mirroring Rust's eprintln+post).
	var msg string
	if len(allUserIDs) > 0 {
		msg = "Scan complete: " + strconv.Itoa(upserted) + " unique users tracked (scanned " + strconv.Itoa(totalMsgCount) + " messages across " + strconv.Itoa(len(textChannels)) + " channels). Initiated by " + username
	} else {
		msg = "Scan complete: no users found (scanned " + strconv.Itoa(totalMsgCount) + " messages across " + strconv.Itoa(len(textChannels)) + " channels). Initiated by " + username
	}
	slog.Info("[cull] Scan: "+msg, "module", "cull")
	postToCatHerd(d, msg)
}

// kickLoop is the background body of execute mode (the Rust spawned
// task): kicks each candidate with the KICK_DELAY_MS rhythm.
func (h *Cull) kickLoop(guildID string, candidates []int64, days int, userName string) {
	ctx, cancel := context.WithTimeout(context.Background(), kickLoopTimeout)
	defer cancel()
	d := h.app.D

	postToCatHerd(d, "Starting cull: "+strconv.Itoa(len(candidates))+" candidates (inactive "+strconv.Itoa(days)+"+ days)...")

	successCount := 0
	skipCount := 0
	for _, uid := range candidates {
		reason := "Inactive " + strconv.Itoa(days) + " days — /cull by " + userName
		if err := d.GuildMemberDeleteWithReason(guildID, strconv.FormatInt(uid, 10), reason); err != nil {
			skipCount++
			slog.Error("[cull] Failed to kick "+strconv.FormatInt(uid, 10)+": "+err.Error(), "module", "cull")
		} else {
			successCount++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(KickDelayMs * time.Millisecond):
		}
	}
	postToCatHerd(d, "Cull complete: "+strconv.Itoa(successCount)+" kicked, "+strconv.Itoa(skipCount)+" skipped (errors).")
}

// parseID converts a string ID to an int64 (checked conversion at the
// boundary — never a silent truncation; an invalid ID yields 0, the
// Rust `i64::try_from` overflow arm logs-and-returns for the guild ID
// and the value simply cannot match).
func parseID(id string) int64 {
	v, _ := strconv.ParseInt(id, 10, 64)
	return v
}
