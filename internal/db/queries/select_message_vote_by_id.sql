-- Rust: src/db/message_vote.rs (sync_from_discord / message_vote_create_or_update /
-- message_vote_remove / get_user_id_from_message) —
-- `message_votes.find(message_id).select(MessageVotes::as_select()).first().optional()`
-- name: select_message_vote_by_id :one
SELECT message_id, channel_id, guild_id, user_id, total_vote_tally,
       voters, job_status, current_vote_tally
FROM message_votes WHERE message_id = @message_id;
