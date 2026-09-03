-- Rust: src/db/message_vote.rs (the None branches of sync_from_discord /
-- message_vote_create_or_update) — `diesel::insert_into(message_votes::table)...returning(...)`
-- name: insert_message_vote :one
INSERT INTO message_votes (message_id, channel_id, guild_id, user_id,
                          current_vote_tally, total_vote_tally, voters, job_status)
VALUES (@message_id, @channel_id, @guild_id, @user_id,
        @current_vote_tally, 0, @voters::bigint[], 'created')
RETURNING message_id, channel_id, guild_id, user_id, total_vote_tally,
          voters, job_status, current_vote_tally;
