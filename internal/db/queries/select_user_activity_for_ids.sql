-- Rust: src/db/mod.rs query_user_activity_for_ids —
-- `user_activity.filter(guild_id.eq(...)).filter(user_id.eq_any(ids))`
-- name: select_user_activity_for_ids :many
SELECT user_id, guild_id, last_message_at, created_at
FROM user_activity
WHERE guild_id = @guild_id AND user_id = ANY(@user_ids::int8[]);
