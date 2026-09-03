-- Rust: src/db/mod.rs query_inactive_users —
-- `user_activity.filter(guild_id.eq(...)).filter(last_message_at.lt(cutoff)).select(user_id)`
-- name: select_inactive_user_ids :many
SELECT user_id FROM user_activity
WHERE guild_id = @guild_id AND last_message_at < @cutoff;
