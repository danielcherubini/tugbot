-- Rust: src/handlers/gulag/mod.rs run_gulag_vote_check —
-- `message_votes.filter(current_vote_tally.ge(5)).filter(job_status_created_or_done).for_update().skip_locked().load()`
-- name: select_pending_gulag_votes :many
SELECT message_id, channel_id, guild_id, user_id, total_vote_tally,
       voters, job_status, current_vote_tally
FROM message_votes
WHERE current_vote_tally >= 5
  AND job_status IN ('created', 'done')
FOR UPDATE SKIP LOCKED;
