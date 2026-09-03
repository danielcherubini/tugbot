-- Rust: src/handlers/gulag/mod.rs run_gulag_vote_check (the 30s stale reset) —
-- `diesel::update(message_votes.filter(job_status.eq(Running))).set(job_status.eq(Created))`
-- name: update_message_votes_stale_reset :exec
UPDATE message_votes SET job_status = 'created' WHERE job_status = 'running';
