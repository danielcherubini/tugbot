-- Rust: src/handlers/gulag/mod.rs (gulag_check_handler sets running; the release
-- loop sets done; the vote loop sets failure on error) —
-- `diesel::update(message_votes.find/....set(job_status.eq(...))`
-- name: update_message_vote_status :exec
UPDATE message_votes SET job_status = @status WHERE message_id = @message_id;
