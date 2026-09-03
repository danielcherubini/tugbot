-- Rust: src/handlers/gulag/mod.rs gulag_check_handler (the done branch) —
-- `set(job_status=Done, total_vote_tally = current + total, current_vote_tally = 0, voters = [])`
-- The sum (current + total) is computed by the caller (the loaded row carries
-- both values) and passed as @new_total_vote_tally.
-- name: update_message_vote_final_done :exec
UPDATE message_votes
SET job_status = 'done',
    total_vote_tally = @new_total_vote_tally,
    current_vote_tally = 0,
    voters = ARRAY[]::bigint[]
WHERE message_id = @message_id;
