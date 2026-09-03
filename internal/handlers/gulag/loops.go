package gulag

// loops.go ports the two background loops from src/handlers/gulag/mod.rs:
// run_gulag_check (release loop) and run_gulag_vote_check (vote loop),
// along with the shared send_to_gulag_and_message path and the
// guild_member_addition rejoin flow (ported as JoinRejoin; Task 7's
// OnGuildMemberAdd calls it).
//
// Loop discipline (Rust): 1-second rhythm; connection / query failures
// log and SKIP the iteration (never crash); per-row failures log and
// continue with the next row; on shutdown the ctx error is returned so
// the caller's errgroup can decide.
//
// The 30s stale Running -> Created reset and the FOR UPDATE SKIP LOCKED
// locking belong HERE (the loop queries), not the reaction handler.
//
// The `remod` field is a Rust `GulagUser` model-struct field; not a
// table column (absent from both the Rust schema and the baseline),
// default false, never read — no behavior to port; every ported
// statement leaves it untouched (test-pinned in gulag_test.go).

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/danielcherubini/tugbot/internal/db"
)

// loopRhythm mirrors Rust's `sleep(Duration::from_secs(1))` tick.
const loopRhythm = time.Second

// lastStaleVoteResetAt mirrors Rust's `static LAST_RESET` in
// run_gulag_vote_check: the seconds-epoch of the last stale-reset
// window check (shared across the vote loop's iterations).
var lastStaleVoteResetAt atomic.Int64

// staleResetWindowSeconds is Rust's 30s window.
const staleResetWindowSeconds = 30

// ---------------------------------------------------------------------------
// Loop bodies
// ---------------------------------------------------------------------------

// RunReleaseCheck is the release loop (Rust run_gulag_check,
// mod.rs:252-349).
func (g *Gulag) RunReleaseCheck(ctx context.Context) error {
	ticker := time.NewTicker(loopRhythm)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := g.releaseCheckIteration(ctx); err != nil {
				slog.Error("error in release check, skipping iteration", "module", "gulag", "error", err)
				continue
			}
		}
	}
}

// RunVoteCheck is the vote loop (Rust run_gulag_vote_check,
// mod.rs:352-513).
func (g *Gulag) RunVoteCheck(ctx context.Context) error {
	ticker := time.NewTicker(loopRhythm)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := g.voteCheckIteration(ctx); err != nil {
				slog.Error("error in vote check, skipping iteration", "module", "gulag", "error", err)
				continue
			}
		}
	}
}

// releaseCheckIteration is one release-loop pass (Rust lines 267-347).
func (g *Gulag) releaseCheckIteration(ctx context.Context) error {
	if g.pool == nil {
		return fmt.Errorf("failed to get database connection in run_gulag_check: pool unavailable")
	}
	results, err := g.selectGulagUsersReleasable(ctx)
	if err != nil {
		return fmt.Errorf("error loading gulag users for release: %w", err)
	}
	for i := range results {
		result := results[i]
		slog.Debug(fmt.Sprintf("It's been %d minutes, releasing %d from the gulag", result.GulagLength/60, result.ID),
			"module", "gulag")
		if err := g.setGulagUserNotInGulag(ctx, result.ID); err != nil {
			slog.Error(fmt.Sprintf("Failed to update gulag status for user %d: %v", result.ID, err),
				"module", "gulag")
			continue
		}
		if err := g.removeFromGulag(ctx, strconv.FormatInt(result.GuildID, 10), strconv.FormatInt(result.UserID, 10),
			strconv.FormatInt(result.GulagRoleID, 10)); err == nil {
			if err := g.deleteGulagUser(ctx, result.ID); err != nil {
				slog.Error(fmt.Sprintf("Failed to delete gulag user %d: %v", result.ID, err), "module", "gulag")
				continue
			}
			slog.Debug("Removed from database", "module", "gulag")
			if result.MessageID != 0 {
				// Done the vote from the database (Rust lines 315-345).
				done, err := g.setMessageVoteDone(ctx, result.MessageID)
				if err != nil {
					slog.Error(fmt.Sprintf("Error updating vote status: %v", err), "module", "gulag")
					continue
				}
				if done {
					slog.Debug("Updated Gulag Vote Check Item to Done", "module", "gulag")
				}
			}
		} else {
			if IsDiscordNotFound(err) {
				// Guild or message was deleted on Discord's side —
				// clean up the stale DB row.
				if err := g.deleteGulagUser(ctx, result.ID); err != nil {
					slog.Error(fmt.Sprintf("Failed to delete gulag user %d on error recovery: %v", result.ID, err),
						"module", "gulag")
				} else {
					slog.Debug(fmt.Sprintf("Removed stale gulag user %d from database (%v not found)", result.ID, err),
						"module", "gulag")
				}
			} else {
				slog.Error(fmt.Sprintf("Error run_gulag_check: %v", err), "module", "gulag")
			}
		}
	}
	return nil
}

// voteCheckIteration is one vote-loop pass (Rust lines 366-512).
func (g *Gulag) voteCheckIteration(ctx context.Context) error {
	if g.pool == nil {
		return fmt.Errorf("failed to get database connection in run_gulag_vote_check: pool unavailable")
	}
	// Periodically reset stale Running entries back to Created so they
	// get retried (Rust lines 390-402).
	g.resetStaleRunningVotes(ctx)

	// The pending predicate: tally >= 5 AND status in (created, done),
	// FOR UPDATE SKIP LOCKED (Rust lines 405-421).
	results, err := g.selectPendingGulagVotes(ctx)
	if err != nil {
		return fmt.Errorf("error loading MessageVotes for vote processing: %w", err)
	}
	for i := range results {
		result := results[i]
		if err := g.gulagCheckHandler(ctx, &result); err != nil {
			slog.Error(fmt.Sprintf("error running gulag vote: %v", err), "module", "gulag")
			if err := g.setJobStatus(ctx, result.MessageID, db.JobStatusFailure); err != nil {
				slog.Error(fmt.Sprintf("Failed to update JobStatus to Failure for message %d: %v", result.MessageID, err),
					"module", "gulag")
			}
		}
	}
	return nil
}

// gulagCheckHandler ports gulag_check_handler (Rust mod.rs:515-690):
// set the vote running and verify it, strip the :gulag reactions from
// the message, run the shared send-to-gulag-and-message path, then
// commit the done transition (total = current + total, current = 0,
// voters cleared).
func (g *Gulag) gulagCheckHandler(ctx context.Context, result *db.MessageVote) error {
	// Set the vote to running in the database.
	s, err := g.setJobStatusReturning(ctx, result.MessageID, db.JobStatusRunning)
	if err != nil {
		return fmt.Errorf("failed to update message_vote_id %d: %w", result.MessageID, err)
	}
	if s != db.JobStatusRunning {
		return nil // non-running: handler ends without side effects
	}
	slog.Debug("Updated Gulag Vote Check Item to Running", "module", "gulag")

	// Remove all gulag emoji's from the message (Rust lines 606-617).
	channelID := strconv.FormatInt(result.ChannelID, 10)
	messageID := strconv.FormatInt(result.MessageID, 10)
	message, err := g.d.ChannelMessage(channelID, messageID)
	if err != nil {
		return fmt.Errorf("failed to get Message: %w", err)
	}
	for i := range message.Reactions {
		r := message.Reactions[i]
		if r.Emoji != nil && strings.Contains(reactionEmojiString(r.Emoji), ":gulag") {
			if err := g.d.MessageReactionsRemoveEmoji(channelID, messageID, r.Emoji.ID); err != nil {
				return fmt.Errorf("failed to delete reaction emoji: %w", err)
			}
		}
	}

	// Send to gulag and message (Rust lines 632-646).
	if err := g.sendToGulagAndMessage(ctx, result.GuildID, result.UserID, result.ChannelID, result.MessageID, nil); err != nil {
		return err
	}

	// Commit the done transition (Rust lines 647-688).
	slog.Debug("OK done with sending to gulag, now setting it to done", "module", "gulag")
	newTotal := result.CurrentVoteTally + result.TotalVoteTally
	if err := g.setMessageVoteFinalDone(ctx, result.MessageID, newTotal); err != nil {
		return fmt.Errorf("failed to update message_vote_id %d: %w", result.MessageID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The 30s stale reset
// ---------------------------------------------------------------------------

func (g *Gulag) setJobStatus(ctx context.Context, messageID int64, status db.JobStatus) error {
	_, err := g.pool.Exec(ctx,
		`UPDATE message_votes SET job_status = $1 WHERE message_id = $2`, status, messageID)
	return err
}

func (g *Gulag) setJobStatusReturning(ctx context.Context, messageID int64, status db.JobStatus) (db.JobStatus, error) {
	var s db.JobStatus
	err := g.pool.QueryRow(ctx,
		`UPDATE message_votes SET job_status = $1 WHERE message_id = $2 RETURNING job_status`, status, messageID).
		Scan(&s)
	if err != nil {
		return "", err
	}
	return s, nil
}

// setMessageVoteDone ports the release loop's "Done the vote from the
// database" (Rust mod.rs:322-344): set the row's status to done and
// return whether it ended done.
func (g *Gulag) setMessageVoteDone(ctx context.Context, messageID int64) (bool, error) {
	var s db.JobStatus
	if err := g.pool.QueryRow(ctx,
		`UPDATE message_votes SET job_status = 'done' WHERE message_id = $1 RETURNING job_status`, messageID).
		Scan(&s); err != nil {
		return false, err
	}
	return s == db.JobStatusDone, nil
}

// setMessageVoteFinalDone ports the done commit of gulag_check_handler
// via the Task-1 sqlc shape (update_message_vote_final_done).
func (g *Gulag) setMessageVoteFinalDone(ctx context.Context, messageID int64, newTotal int32) error {
	_, err := g.pool.Exec(ctx,
		`UPDATE message_votes
		 SET job_status = 'done',
		     total_vote_tally = $1,
		     current_vote_tally = 0,
		     voters = ARRAY[]::bigint[]
		 WHERE message_id = $2`, newTotal, messageID)
	return err
}

// resetStaleRunningVotes is the periodic stale reset (Rust
// mod.rs:390-402): at most once per 30s window, flip every running
// vote back to created. The reset UPDATE's failure is swallowed exactly
// like Rust's `.ok()`.
func (g *Gulag) resetStaleRunningVotes(ctx context.Context) {
	now := time.Now().Unix()
	last := lastStaleVoteResetAt.Load()
	if now-last > staleResetWindowSeconds {
		lastStaleVoteResetAt.Store(now)
		// Rust: .ok() — the reset UPDATE's failure is silently swallowed,
		// no log arm.
		_, _ = g.pool.Exec(ctx, staleRunningVoteResetSQL)
	}
}

// ---------------------------------------------------------------------------
// Release query + gulag_users helpers
// ---------------------------------------------------------------------------

const selectGulagUsersReleasableSQL = `SELECT id, user_id, guild_id, gulag_role_id, channel_id, in_gulag,
       gulag_length, created_at, release_at, message_id
FROM gulag_users
WHERE in_gulag AND release_at <= $1
FOR UPDATE SKIP LOCKED`

const pendingGulagVotesSQL = `SELECT message_id, channel_id, guild_id, user_id, total_vote_tally,
       voters, job_status, current_vote_tally
FROM message_votes
WHERE current_vote_tally >= 5
  AND job_status IN ('created', 'done')
FOR UPDATE SKIP LOCKED`

const deleteGulagUserSQL = `DELETE FROM gulag_users WHERE id = $1`

const setGulagUserNotInGulagSQL = `UPDATE gulag_users SET in_gulag = false WHERE id = $1`

const staleRunningVoteResetSQL = `UPDATE message_votes SET job_status = 'created' WHERE job_status = 'running'`

// selectGulagUsersReleasable mirrors the Task-1 sqlc query (byte-identical
// to select_gulag_users_releasable).
func (g *Gulag) selectGulagUsersReleasable(ctx context.Context) ([]db.GulagUser, error) {
	rows, err := g.pool.Query(ctx, selectGulagUsersReleasableSQL, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.GulagUser
	for rows.Next() {
		var r db.GulagUser
		if err := rows.Scan(&r.ID, &r.UserID, &r.GuildID, &r.GulagRoleID, &r.ChannelID,
			&r.InGulag, &r.GulagLength, &r.CreatedAt, &r.ReleaseAt, &r.MessageID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// selectPendingGulagVotes mirrors the Task-1 sqlc query (byte-identical
// to select_pending_gulag_votes).
func (g *Gulag) selectPendingGulagVotes(ctx context.Context) ([]db.MessageVote, error) {
	rows, err := g.pool.Query(ctx, pendingGulagVotesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.MessageVote
	for rows.Next() {
		var r db.MessageVote
		if err := rows.Scan(&r.MessageID, &r.ChannelID, &r.GuildID, &r.UserID,
			&r.TotalVoteTally, &r.Voters, &r.JobStatus, &r.CurrentVoteTally); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (g *Gulag) deleteGulagUser(ctx context.Context, id int32) error {
	_, err := g.pool.Exec(ctx, deleteGulagUserSQL, id)
	return err
}

func (g *Gulag) setGulagUserNotInGulag(ctx context.Context, id int32) error {
	_, err := g.pool.Exec(ctx, setGulagUserNotInGulagSQL, id)
	return err
}

// cleanupStaleGulagRow is the 404 recovery branch of the release loop,
// isolated for testability: a canonical Discord 404 deletes the stale
// row; anything else is logged (and not deleted by this helper).
func (g *Gulag) cleanupStaleGulagRow(ctx context.Context, id int32, cause error) error {
	if !IsDiscordNotFound(cause) {
		slog.Error(fmt.Sprintf("Error run_gulag_check: %v", cause), "module", "gulag")
		return nil
	}
	if err := g.deleteGulagUser(ctx, id); err != nil {
		slog.Error(fmt.Sprintf("Failed to delete gulag user %d on error recovery: %v", id, err),
			"module", "gulag")
		return err
	}
	slog.Debug(fmt.Sprintf("Removed stale gulag user %d from database (%v not found)", id, cause),
		"module", "gulag")
	return nil
}

// ---------------------------------------------------------------------------
// Discord removal / shared send path
// ---------------------------------------------------------------------------

// removeFromGulag ports remove_from_gulag (Rust mod.rs:221-247): fetch
// the member, remove the role, look up the-gulag by name (the lookup
// error and missing arms are DISTINCT errors), post the release message.
func (g *Gulag) removeFromGulag(ctx context.Context, guildID, userID, roleID string) error {
	mem, err := g.d.GuildMember(guildID, userID)
	if err != nil || mem == nil || mem.User == nil {
		return fmt.Errorf("failed to get guild member: %w", err)
	}
	if err := g.d.GuildMemberRoleRemove(guildID, userID, roleID); err != nil {
		return fmt.Errorf("failed to remove gulag role: %w", err)
	}
	channel, found, err := g.FindChannel(ctx, guildID, GulagChannelName)
	if err != nil {
		return fmt.Errorf("the-gulag channel lookup failed: %w", err)
	}
	if !found {
		return fmt.Errorf("the-gulag channel not found")
	}
	if _, err := g.d.ChannelMessageSend(channel.ID, "Freeing "+mem.User.Mention()+" from the gulag"); err != nil {
		return fmt.Errorf("failed to send release message: %w", err)
	}
	slog.Debug("Removed from gulag", "module", "gulag")
	return nil
}

// sendToGulagAndMessage ports send_to_gulag_and_message (Rust
// mod.rs:185-249): the SHARED vote path (fixed 300s). Note the /gulag
// slash command does NOT use this — it goes straight to the core
// AddToGulag with the user-supplied length. The lookup error arm and
// the missing-channel arm are BOTH errors (Rust's with_context on the
// Option::None).
func (g *Gulag) sendToGulagAndMessage(ctx context.Context, guildID, userID, channelID, messageID int64, voters []*discordgo.User) error {
	gulenRole := g.FindGulagRole(ctx, strconv.FormatInt(guildID, 10))
	if gulenRole == nil {
		return fmt.Errorf("Couldn't find gulag role")
	}
	const gulenLength = 300
	channel, found, err := g.FindChannel(ctx, strconv.FormatInt(guildID, 10), GulagChannelName)
	if err != nil {
		return fmt.Errorf("the-gulag channel lookup failed: %w", err)
	}
	if !found {
		return fmt.Errorf("the-gulag channel not found")
	}
	gulenUser, err := g.AddToGulag(ctx, GulagParams{
		GuildID:     strconv.FormatInt(guildID, 10),
		UserID:      strconv.FormatInt(userID, 10),
		GulagRoleID: gulenRole.ID,
		GulagLength: gulenLength,
		ChannelID:   channel.ID,
		MessageID:   strconv.FormatInt(messageID, 10),
	})
	if err != nil {
		return fmt.Errorf("failed to add user to gulag: %w", err)
	}
	channelIDStr := strconv.FormatInt(channelID, 10)
	messageIDStr := strconv.FormatInt(messageID, 10)
	msg, err := g.d.ChannelMessage(channelIDStr, messageIDStr)
	if err != nil {
		return fmt.Errorf("failed to get message: %w", err)
	}
	mem, err := g.d.GuildMember(strconv.FormatInt(guildID, 10), strconv.FormatInt(userID, 10))
	if err != nil || mem == nil || mem.User == nil {
		return fmt.Errorf("failed to get guild member: %w", err)
	}

	userString := ""
	if voters != nil {
		userString = "\nThese people voted them in"
		for _, user := range voters {
			if user != nil {
				userString += ", " + user.Mention()
			}
		}
	}

	content := fmt.Sprintf("Sending %s to the Gulag for %d minutes because of %s, they have %d minutes remaining%s",
		mem.User.Mention(), gulenLength/60, messageLink(strconv.FormatInt(guildID, 10), msg), gulenUser.GulagLength/60, userString)
	if _, err := g.d.ChannelMessageSend(channel.ID, content); err != nil {
		return fmt.Errorf("failed to send gulag message: %w", err)
	}
	return nil
}

// messageLink mirrors Rust's msg.link()
// (https://discord.com/channels/{guild}/{channel}/{message}).
func messageLink(guildID string, message *discordgo.Message) string {
	return "https://discord.com/channels/" + guildID + "/" + message.ChannelID + "/" + message.ID
}

// ---------------------------------------------------------------------------
// The rejoin flow (Rust guild_member_addition, src/handlers/mod.rs:144-213)
// ---------------------------------------------------------------------------

// JoinRejoin ports guild_member_addition: an already-gulagged member
// re-joins -> re-AddToGulag with the STORED length + role (the existing
// branch doubles the length and extends release_at), then post
// "You can't escape so easily {member}" to the STORED channel. Task 7's
// OnGuildMemberAdd wires this.
func (g *Gulag) JoinRejoin(member *discordgo.Member) {
	ctx := context.Background()
	userID, err := DiscordID("user", member.User.ID)
	if err != nil {
		slog.Error("Failed to convert user ID", "module", "gulag", "error", err)
		return
	}
	stored := g.IsUserInGulag(ctx, userID)
	if stored == nil {
		return
	}
	// The Go model stores the Discord IDs as int64 DB values directly, so
	// the Rust i64 -> u64 try_into arms are satisfied by construction.
	if _, err := g.AddToGulag(ctx, GulagParams{
		GuildID:     strconv.FormatInt(stored.GuildID, 10),
		UserID:      member.User.ID,
		GulagRoleID: strconv.FormatInt(stored.GulagRoleID, 10),
		GulagLength: uint32(stored.GulagLength),
		ChannelID:   strconv.FormatInt(stored.ChannelID, 10),
		MessageID:   "0",
	}); err != nil {
		slog.Error("Failed to re-add user to gulag on rejoin", "module", "gulag", "error", err)
	}
	channelID := strconv.FormatInt(stored.ChannelID, 10)
	if _, err := g.d.Channel(channelID); err == nil {
		msg := "You can't escape so easily " + member.Mention()
		if _, sErr := g.d.ChannelMessageSend(channelID, msg); sErr != nil {
			slog.Error("Failed to send gulag escape message", "module", "gulag", "error", sErr)
		}
	}
}
