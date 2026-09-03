-- Rust: src/db/message_vote.rs (sync_from_discord, the ADDED branches of
-- message_vote_create_or_update, and message_vote_remove) —
-- `diesel::update(message_votes.find(message_id)).set(current_vote_tally, voters)`
-- name: update_message_vote_voters :exec
UPDATE message_votes
SET current_vote_tally = @current_vote_tally, voters = @voters
WHERE message_id = @message_id;
