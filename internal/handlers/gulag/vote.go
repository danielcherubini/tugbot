package gulag

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danielcherubini/tugbot/internal/db"
)

// vote.go ports src/db/message_vote.rs onto the canonical Task-4
// pgxpool-based core. It holds the message_votes workflow: the
// idempotent one-vote-per-user create-or-update (the "Add Gulag Vote"
// context-menu command) and the reaction-handler sync (Discord is the
// source of truth for the tally). The job_status state machine values
// are byte-identical to the Rust enum: created / running / done /
// failure (the sqlc JobStatus type); the 30s stale Running -> Created
// reset and the FOR UPDATE SKIP LOCKED locking belong to the background
// loops (loops.go), NOT this handler — these are plain non-transactional
// SQL.
//
// Dead Rust (verified: no callers anywhere in the Rust tree), and there
//fore NOT ported: `message_vote_remove` and `get_user_id_from_message`
// (pinned in docs/parity/checklist.md). There is also NO vote window of
// any duration anywhere in Rust — nothing here invents one.

// FeatureKey pins the feature flag (Rust literal "gulag").
const FeatureKey = "gulag"

// voteState mirrors Rust's MessageVoteHanderResponseType.
type voteState int

const (
	voteAdded   voteState = iota
	voteRemoved           // exists for parity; unreachable (Rust's remove path has no callers)
)

// voteHandlerResponse mirrors Rust's MessageVoteHandlerResponse.
type voteHandlerResponse struct {
	responseType voteState
	vote         *db.MessageVote
}

// selectMessageVoteByID mirrors message_vote.rs's `message_votes.find
// (message_id as i64).select(MessageVotes::as_select()).first().optional()`
// — (nil, nil) when the row is absent; other errors (including a dead
// pool, which Rust maps to the "Failed to get database connection"
// arm) propagate to the caller's connection-error text.
func (g *Gulag) selectMessageVoteByID(ctx context.Context, messageID int64) (*db.MessageVote, error) {
	var row db.MessageVote
	err := g.pool.QueryRow(ctx,
		`SELECT message_id, channel_id, guild_id, user_id, total_vote_tally,
		        voters, job_status, current_vote_tally
		 FROM message_votes WHERE message_id = $1`, messageID).
		Scan(&row.MessageID, &row.ChannelID, &row.GuildID, &row.UserID, &row.TotalVoteTally,
			&row.Voters, &row.JobStatus, &row.CurrentVoteTally)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// updateMessageVoteVoters ports the `.set(current_vote_tally, voters)`
// update of sync_from_discord / message_vote_create_or_update /
// message_vote_remove (sqlc: update_message_vote_voters).
func (g *Gulag) updateMessageVoteVoters(ctx context.Context, messageID int64, tally int32, voters []int64) error {
	_, err := g.pool.Exec(ctx,
		`UPDATE message_votes
		 SET current_vote_tally = $1, voters = $2
		 WHERE message_id = $3`, tally, voters, messageID)
	if err != nil {
		return fmt.Errorf("failed to update vote voters: %w", err)
	}
	return nil
}

// insertMessageVoteRow ports the NewMessageVotes insert (sqlc:
// insert_message_vote): fresh row, total_vote_tally 0, job_status
// created.
func (g *Gulag) insertMessageVoteRow(ctx context.Context, messageID, channelID, guildID, userID int64, tally int32, voters []int64) (db.MessageVote, error) {
	var row db.MessageVote
	err := g.pool.QueryRow(ctx,
		`INSERT INTO message_votes (message_id, channel_id, guild_id, user_id,
		                          current_vote_tally, total_vote_tally, voters, job_status)
		 VALUES ($1, $2, $3, $4, $5, 0, $6::bigint[], 'created')
		 RETURNING message_id, channel_id, guild_id, user_id, total_vote_tally,
		           voters, job_status, current_vote_tally`,
		messageID, channelID, guildID, userID, tally, voters).
		Scan(&row.MessageID, &row.ChannelID, &row.GuildID, &row.UserID, &row.TotalVoteTally,
			&row.Voters, &row.JobStatus, &row.CurrentVoteTally)
	if err != nil {
		return db.MessageVote{}, fmt.Errorf("failed to insert message vote: %w", err)
	}
	return row, nil
}

// syncFromDiscord ports sync_from_discord (message_vote.rs:38-89): the
// Discord-fetched voter list is authoritative — an existing row's
// current_vote_tally + voters are REPLACED (the status is untouched), a
// missing row is created fresh.
func (g *Gulag) syncFromDiscord(ctx context.Context, messageID, guildID, channelID, userID int64, voters []int64) (*db.MessageVote, error) {
	tally := int32(len(voters))
	existing, err := g.selectMessageVoteByID(ctx, messageID)
	if err != nil {
		return nil, fmt.Errorf("failed to get database connection: %w", err)
	}
	if existing != nil {
		if err := g.updateMessageVoteVoters(ctx, messageID, tally, voters); err != nil {
			return nil, fmt.Errorf("failed to update vote from Discord: %w", err)
		}
		existing.CurrentVoteTally = tally
		existing.Voters = voters
		return existing, nil
	}
	row, err := g.insertMessageVoteRow(ctx, messageID, channelID, guildID, userID, tally, voters)
	if err != nil {
		return nil, fmt.Errorf("failed to create vote from Discord: %w", err)
	}
	return &row, nil
}

// messageVoteCreateOrUpdate ports message_vote_create_or_update
// (message_vote.rs:91-143): the idempotent voter set (one vote per user
// per message — "You have already Voted"), the tally increment, and the
// fresh insert. It preserves Rust's exact asymmetry: message_votes
// .user_id (the VOTED person) is the message AUTHOR, while the INVOKER
// lands in the voters array.
func (g *Gulag) messageVoteCreateOrUpdate(ctx context.Context, messageID, guildID, channelID, userID, voterID int64) (voteHandlerResponse, error) {
	existing, err := g.selectMessageVoteByID(ctx, messageID)
	if err != nil {
		return voteHandlerResponse{}, fmt.Errorf("failed to get database connection: %w", err)
	}
	if existing != nil {
		for _, v := range existing.Voters {
			if v == voterID {
				return voteHandlerResponse{}, errors.New("You have already Voted")
			}
		}
		voters := append(append([]int64{}, existing.Voters...), voterID)
		tally := existing.CurrentVoteTally + 1
		if err := g.updateMessageVoteVoters(ctx, messageID, tally, voters); err != nil {
			return voteHandlerResponse{}, fmt.Errorf("DB Error whilst trying to add vote: %w", err)
		}
		existing.CurrentVoteTally = tally
		existing.Voters = voters
		return voteHandlerResponse{responseType: voteAdded, vote: existing}, nil
	}
	row, err := g.insertMessageVoteRow(ctx, messageID, channelID, guildID, userID, 1, []int64{voterID})
	if err != nil {
		return voteHandlerResponse{}, fmt.Errorf("Database Error Creating Vote: %w", err)
	}
	return voteHandlerResponse{responseType: voteAdded, vote: &row}, nil
}
