-- Rust: src/db/message_vote.rs get_user_id_from_message —
-- `message_votes.find(message_id as i64).select(message_votes::user_id).first().optional()`
-- name: select_message_vote_user_id :one
SELECT user_id FROM message_votes WHERE message_id = @message_id;
